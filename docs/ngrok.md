# ngrok tunnel

The extension proxy's external port (host `6674`) has to be publicly reachable for FTDC
data providers on a testnet to answer it. You run the ngrok agent; the scripts only read
the URL off it.

## The flow

Start the agent once and leave it up:

```bash
ngrok http 6674
```

Then start services as normal:

```bash
./scripts/start-services.sh --chain coston2 --tunnel
```

That queries the agent's local API (`127.0.0.1:4040/api/tunnels`), takes the public URL and
writes it into `.env` as `EXT_PROXY_URL` before the containers come up — so `post-build.sh`
registers the public URL, and `test.sh` targets it.

The sync runs on every start, with or without `--tunnel`. That is what keeps a rotated URL
from going stale, and what makes switching between extensions work. All `--tunnel` changes
is what happens when no agent answers: **with** it, the run fails immediately and tells you
to start one; **without** it, you get a warning and the run continues on whatever
`EXT_PROXY_URL` is already in `.env`.

Nothing stops the agent. `stop-services.sh --tunnel` accepts the flag and deliberately does
nothing — stopping the agent rotates the URL for anything else pointed at it.

## Doing it by hand

```bash
curl -s http://127.0.0.1:4040/api/tunnels | jq -r '.tunnels[0].public_url'
```

Copy that into `.env` as `EXT_PROXY_URL`, **then** start the containers.
`start-services.sh` blocks on `$EXT_PROXY_URL/info`, so a stale URL there is what makes it
fail. Or write it straight in:

```bash
URL=$(curl -s http://127.0.0.1:4040/api/tunnels | jq -r '.tunnels[0].public_url') \
  && sed -i.bak "s|^EXT_PROXY_URL=.*|EXT_PROXY_URL=$URL|" .env && rm -f .env.bak \
  && grep '^EXT_PROXY_URL=' .env
```

Rewrites an existing `EXT_PROXY_URL=` line only — if the echo comes back empty or stale,
the line isn't in your `.env`; add it once by hand and the command works from then on.

## Keeping the URL stable

A random `*.ngrok-free.app` hostname changes every time the agent restarts. A reserved domain
pins it, and needs no extra configuration in this repo:

1. ngrok dashboard → **Domains** → create the domain you want.
2. Start the agent on it: `ngrok http --url=https://<your-domain>.ngrok-free.app 6674`
   (older agents spell that `--domain=<your-domain>.ngrok-free.app`).
3. Put the same URL in `.env` as `EXT_PROXY_URL`. Discovery is unchanged — `start-services.sh`
   still reads the agent's local API, and now gets the same hostname back every time.

### When the URL did change

Refreshing `.env` is only half the fix. Flare delivers instructions to the URL held in the
on-chain machine registry, and `post-build.sh` cannot rewrite it once the machine is
registered: `register-tee` sees a non-zero `teeId` from `getTeeMachine`, logs "already
registered" and skips pre-registration — the one step that writes the URL. The run therefore
reports success, and the chain leg really does succeed, while the registry still points at the
dead tunnel; every instruction after that times out at `pollAction`. Re-registering can't fix
it, because the container's TEE key — and so its `teeId` — is unchanged.

```bash
./scripts/update-tee-url.sh            # defaults to EXT_PROXY_URL from .env
```

One `updateTeeMachineSettings` transaction against the FlareTeeManager diamond. It prints the
change and asks before sending, and exits without spending gas when the registered URL already
matches. Same note on the other tunnel: [docs/cloudflared.md](cloudflared.md).

## Know this

- **The script never starts, stops or configures ngrok.** Authtoken, plan, reserved domain,
  config file — all outside this repo. It reads one URL and writes one `.env` line.
- **A random `*.ngrok-free.app` hostname rotates every time you restart the agent.** Re-run
  `start-services.sh` (or the one-liner above) afterwards so `.env` catches up. A reserved
  domain avoids this entirely and needs no extra config here — discovery just returns the
  stable hostname.
- **`NGROK_API_PORT` overrides the API port** (default `4040`). A second agent on the same
  machine takes `4041`; set this to whichever one forwards to this proxy.
- **Running the agent in Docker?** Publish its port `4040` to the host, or the script can't
  read the URL. Point the tunnel at `host.docker.internal:6674`, not `localhost`.
- **`--local` expects the tunnel on port 6664**, since the host Go proxy listens there
  instead of 6674. The script warns if the running tunnel's upstream port doesn't match what
  it expects — a tunnel forwarding elsewhere would put a live-looking URL in `.env` that
  never reaches this proxy.
- **The tunnel is fine with nothing behind it.** It just 502s until the containers are up,
  so start order doesn't matter.
- The free-tier browser interstitial only fires on `Accept: text/html` requests, so Go
  clients and `curl` pass straight through. If you hit it in a browser, send
  `ngrok-skip-browser-warning: true`.
