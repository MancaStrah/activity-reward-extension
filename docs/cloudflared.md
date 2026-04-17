# Cloudflare Tunnel (ngrok replacement)

Exposes the extension proxy's external port (host `6674`) over a public HTTPS URL.
Use when ngrok is unavailable. Compose file: `docker-compose.cloudflared.yaml`.

ngrok is the default tunnel, and it stays that way until you say otherwise: set
`TUNNEL_PROVIDER=cloudflared` and the lifecycle scripts drive this one instead — including
starting and stopping the container, which the ngrok path deliberately never does. Driving it
entirely by hand still works; that path is further down.

## Driven by the scripts

```bash
TUNNEL_PROVIDER=cloudflared ./scripts/start-services.sh --chain coston2 --tunnel
```

With `--tunnel`, `start-services.sh` (before it starts anything else, so the URL is public by
the time `post-build.sh` registers it):

1. Reuses the tunnel if one is already up — recreating it would mint a new URL for every
   extension behind it — and starts it otherwise, pulling `cloudflare/cloudflared` on first
   run.
2. Waits up to 30 seconds for the generated `*.trycloudflare.com` hostname to appear in the
   container logs, reading only logs from this start so a restarted container cannot hand back
   its previous, dead hostname. If nothing shows up it stops with the exact
   `docker compose … logs cloudflared` command to run.
3. Writes the hostname into `.env` as `EXT_PROXY_URL`. The value is accepted only if it is a
   plain https URL; every lifecycle script `source`s `.env`, so anything else is refused rather
   than written.

Without `--tunnel` nothing is ever started: a running tunnel is still read and resynced into
`.env` (which is what makes switching between extensions work), and if none is running you get
a note and the run continues — the proxy `/info` wait later on is what will fail if
`EXT_PROXY_URL` isn't reachable.

Set the provider once in `.env` instead of on every command:

```
TUNNEL_PROVIDER=cloudflared
```

**Stop it:**

```bash
TUNNEL_PROVIDER=cloudflared ./scripts/stop-services.sh --chain coston2 --tunnel
```

The tunnel goes down last, after everything behind it. Without `--tunnel` it is left running on
purpose (stopping it rotates the URL for anything else pointed at it); with `--tunnel` and no
tunnel running it says so and exits 0.

**Proxy as a local Go binary (`--local`).** The host proxy listens on `6664`, so the script
points the tunnel at `http://host.docker.internal:6664` and runs it as its own compose project
`tunnel-local` — pointing the shared `tunnel` container at a different origin would recreate it
and rotate everyone's URL. Stop it with the same pair: `--local --tunnel`.

## Driving it by hand

From the repo root, in Git Bash:

**1. Start the tunnel.** Pulls `cloudflare/cloudflared` on first run (cached after that) and
starts the container detached.

```bash
docker compose -f docker-compose.cloudflared.yaml up -d
```

**2. Read the URL it was assigned.** cloudflared prints the generated `*.trycloudflare.com`
hostname once at startup; this greps it back out of the container logs (`tail -1` keeps the
most recent one, in case the container has been restarted).

```bash
docker compose -f docker-compose.cloudflared.yaml logs cloudflared \
  | grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' | tail -1
```

Copy the printed URL into `.env` as `EXT_PROXY_URL=<url>`, **then** start the containers
(`./scripts/start-services.sh --chain coston2`). `start-services.sh` blocks on
`$EXT_PROXY_URL/info`, so a stale URL there is what makes it fail.

Stop: `docker compose -f docker-compose.cloudflared.yaml down`

A tunnel started this way is an ordinary running tunnel: `TUNNEL_PROVIDER=cloudflared` picks it
up and resyncs its URL, with or without `--tunnel`, which saves the copy-paste after every
restart.

## Rotated the URL? The on-chain record needs repointing too

`EXT_PROXY_URL` in `.env` only feeds the local scripts. Flare delivers instructions to the URL
stored in the on-chain machine registry, and in this repo's tooling that URL is written by
exactly one step: machine pre-registration. For a machine that is already registered,
`post-build.sh` skips it — `register-tee` sees a non-zero `teeId` from `getTeeMachine`, logs
"already registered" and only requests a fresh attestation. So a re-run reports success end to
end and the chain leg really does succeed, while the registry keeps pointing at the dead
tunnel. The symptom is every instruction timing out at `pollAction` with nothing listening at
the registered URL. Re-registering does not help either: the container's TEE key does not
change, so neither does its `teeId`.

```bash
./scripts/update-tee-url.sh                                  # defaults to EXT_PROXY_URL from .env
./scripts/update-tee-url.sh --url https://tee.example.com --yes
```

One `updateTeeMachineSettings(teeId, proxyId, url)` transaction against the FlareTeeManager
diamond — no attestation, machine status and ledger untouched. It prints the change and asks
before sending, exits 0 without spending gas when the registered URL already matches, and
needs `DEPLOYMENT_PRIVATE_KEY` to be the machine owner. It reads the TEE public key off the
*local* proxy `/info`, so it works with the tunnel already dead. This is the strongest reason
to want a stable hostname.

## Making the URL stable

A quick tunnel's URL always rotates and no flag pins it. Cheapest fix first:

**Don't restart the tunnel.** It has no network dependency on the main stack, so `down && up`
on the app containers does not touch it — and neither does `stop-services.sh` unless you pass
`--tunnel`. Only restarting `cloudflared` itself rotates the URL.

**Named tunnel — a permanently fixed hostname.** Needs a free Cloudflare account with a domain
on it:

1. Zero Trust → Networks → Tunnels → *Create a tunnel* → Cloudflared. Copy the `eyJ...` token.
2. Give it a public hostname pointing at `http://host.docker.internal:6674` — the compose file
   already maps `host.docker.internal` to the host gateway.
3. In `.env`:
   ```
   TUNNEL_PROVIDER=cloudflared
   TUNNEL_ARGS=run --token eyJhIjoi...
   EXT_PROXY_URL=https://tee.yourdomain.com
   ```
4. `./scripts/start-services.sh --chain coston2 --tunnel` — same command, named mode now.
   `TUNNEL_ARGS` replaces the whole `--url <target>` argument, so `TUNNEL_TARGET` is ignored
   once it is set. The hostname is fixed and already in `.env`, so the script starts the tunnel
   and leaves `EXT_PROXY_URL` exactly as you wrote it instead of scraping the logs for a
   generated one. `docker compose -f docker-compose.cloudflared.yaml up -d` works the same way.

With a fixed hostname `EXT_PROXY_URL` and the on-chain machine record survive every restart,
and adding `restart: unless-stopped` to the service becomes safe.

## Know this

- **The URL changes on every start.** Quick tunnel, no Cloudflare account. The scripts re-read
  it on every run, so `.env` keeps up on its own; the on-chain record does not (see above).
  There's deliberately no `restart:` policy, since a silent restart would mint a new URL and
  strand `EXT_PROXY_URL`.
- **The tunnel starts fine with nothing behind it.** It just 502s until the containers are up.
- **No network dependency on the main stack**, so start order doesn't matter.
- **`--chain local` never touches a tunnel** — the proxy is reachable on localhost, and
  `--tunnel` is ignored there.
- `TUNNEL_TARGET` overrides what the tunnel points at. `--local` sets it for you; setting it by
  hand against the shared `tunnel` project recreates that container and mints a new URL, so
  isolate it with `-p <name>` when driving compose directly.
