# Getting started — local

Runs the extension end to end on a local devnet in **`--local` mode**: the TEE
node and the extension proxy run as background Go processes on the host (redis
still runs as a container). The lifecycle is the same as any other chain —
deploy `StravaInstructionSender`, register the extension, start node + proxy,
register the TEE machine — then request a signed distance proof and claim
against it.

`--local` is Go-only: `start-services.sh` refuses it for any other `LANGUAGE`.
For Coston2 see [deployment-steps.md](deployment-steps.md); for the architecture
behind these steps see [architecture.md](architecture.md).

## Prerequisites

| Need | Why |
|---|---|
| Go 1.25+ | the `go/` implementation and the `tools/` CLIs (both `go.mod`s pin `toolchain go1.25.13`) |
| Foundry (`forge`) | compiles `contracts/`, runs the Solidity tests |
| `jq` | `pre-build.sh` extracts ABI/bytecode from the Foundry output |
| Docker + Compose | redis on `:6382`, even in `--local` mode |
| FCC infrastructure on a local devnet | Hardhat node, indexer DB, and the "normal" TEE proxy. **Not part of this repo** — bring up your own |
| That devnet's `deployed-addresses.json` | the `FlareTeeManager` diamond. The repo ships only `config/coston/` and `config/coston2/`, so point `ADDRESSES_FILE` at yours |
| A funded devnet key | deploys the contract, registers, and funds the reward pool |
| A Strava token with `activity:read_all` | `test.sh` and `claim-reward.sh` need it; see the README's [Getting a Strava Access Token](../README.md#getting-a-strava-access-token). Tokens expire after 6 h |

## Configure

`.env.example` is the **Coston2** `testnet-sim` template, not a local one — for a
devnet run write `.env` directly:

```bash
cat > .env <<'EOF'
LANGUAGE=go

CHAIN=local
CHAIN_ID=31337                 # REQUIRED — unset leaves chainID 0 and every TEE signature comes back empty
CHAIN_URL=http://127.0.0.1:8545
ADDRESSES_FILE=/abs/path/to/deployed-addresses.json
NORMAL_PROXY_URL=http://localhost:6662

LOCAL_MODE=true
SIMULATED_TEE=true
MODE=1                         # simulated attestation; TEE_PROFILE=local is inferred from CHAIN=local

DEPLOYMENT_PRIVATE_KEY=        # funded devnet key, no 0x prefix
INITIAL_OWNER=                 # 0x address derived from DEPLOYMENT_PRIVATE_KEY
PROXY_PRIVATE_KEY=             # proxy /info signing key — prefer a separate, unfunded key
EOF
```

`.env` is gitignored. Per-chain copies live in `.env.<chain>` and
`./scripts/use-chain.sh <chain>` activates one by copying it over `.env`
(`--list` enumerates them). Passing `--chain <name>` to the lifecycle scripts
does the same copy automatically, so it **overwrites `.env`**.

## Check the host-process proxy config

In `--local` mode the proxy reads `config/proxy/extension_proxy.toml` on the local
devnet and `config/proxy/extension_proxy.<chain>.toml` on a public chain — the same
per-chain split the container path uses, one directory over. `start-services.sh`
resolves that path, checks its `[attestation]` section against `TEE_PROFILE`, and
passes it down as `PROXY_CONFIG`, so the file the proxy opens is the file that was
checked. A bare `go run ./cmd/start-proxy` with no script picks the same file from
`CHAIN` (falling back to `LOCAL_MODE`), and logs which one it chose.

`extension_proxy.toml` is committed because it holds no credentials, which also means
its values are generic — confirm two of them before starting:

- `chain_id` must match the devnet. The committed copy carries `114`; the
  container path's `config/proxy/extension_proxy.docker.toml` carries `31337`.
- `[db]` must point at your indexer database.

Both `--local` and Docker mode now go through the same check, so a config whose
`[attestation]` section does not match the profile stops the run before anything is
built or started — including the case that used to slip through: no `[attestation]`
section at all, which the proxy reads as `enable = false`.

## Run it

```bash
./scripts/full-setup.sh --local                            # setup only
STRAVA_TOKEN=… ./scripts/full-setup.sh --local --test      # setup + end-to-end test
```

That chains pre-build (deploy + register + bind the extension id) →
start-services `--local` (TEE node, redis, proxy) → post-build (allow the TEE
version, register governance, register the TEE machine) → `test.sh`.

The same thing one phase at a time — none of these take flags, they read `.env`:

```bash
./scripts/pre-build.sh
./scripts/start-services.sh --local
./scripts/post-build.sh
STRAVA_TOKEN=… ./scripts/test.sh
```

## Verify it works

```bash
curl -s http://localhost:6664/info | jq '.machineData | {extensionId, codeHash, platform}'
```

`extensionId` must match `EXTENSION_ID` in `config/extension.env` (written by
`pre-build.sh`). Then exercise the caller-side flow:

```bash
STRAVA_TOKEN=… ./scripts/claim-reward.sh --no-claim   # read your monthly distance, don't claim
STRAVA_TOKEN=… ./scripts/claim-reward.sh              # request a proof and claim the reward
```

`claim-reward.sh` configures nothing — the contract must already hold at least
`REWARD_AMOUNT` (1 native token; a plain transfer funds the pool via
`receive()`), and a freshly deployed contract needs `--set-extension-id` once if
`pre-build.sh` did not bind it. `test.sh` handles both itself.

The chain-free test layers need no running stack:

```bash
./scripts/test-unit.sh              # Go unit tests
forge test                          # Solidity tests
./scripts/test-profile-matrix.sh    # the fail-fast profile matrix
./scripts/check-versions.sh         # dependency pin consistency
```

See [testing.md](testing.md) for the full matrix.

## Ports

| Port | What |
|---|---|
| 6664 | extension proxy, external — `/info` and `/action/result/<id>` |
| 6663 | extension proxy, internal — the TEE polls actions from the queue |
| 6382 | this extension's redis (a container, even in `--local`) |
| 6662 | the "normal" FTDC proxy (infrastructure, not this repo) |
| 8100 | types-server — **not** started by `--local`; run `cd go && go run ./cmd/types-server` |
| 8080 / 9090 | compiled-in `EXTENSION_PORT` / `SIGN_PORT` defaults of the host-process node |

Under Docker the external proxy port is republished on **6674** instead; the
README's [Ports](../README.md#ports) table has the full container mapping.

## Logs and stopping

Host processes log to `out/logs/` and write PIDs to `out/pids/`:

```bash
tail -f out/logs/ext-tee.log
tail -f out/logs/ext-proxy.log
```

```bash
./scripts/stop-services.sh --local   # stops the Go processes only
docker compose down                  # redis stays up otherwise — this removes it
```

## Docker instead of host processes

Drop `--local` and the identical lifecycle runs in containers
(`./scripts/full-setup.sh`), building the extension image from `go/Dockerfile`
and the proxy from `proxy/Dockerfile`. The proxy is then published on `6674` and
the types-server on `127.0.0.1:8100`.

## Common failures

| Symptom | Cause |
|---|---|
| `Cannot find deployed-addresses.json. Set ADDRESSES_FILE.` | pre-build only auto-detects `config/coston{,2}/` and a sibling e2e `sim_dump` — set `ADDRESSES_FILE` |
| `refusing to guess a TEE profile for a public chain with MODE=1` | `CHAIN` is not `local` and `TEE_PROFILE` is unset — choose `testnet-sim` or `confidential-space` explicitly |
| `--local runs plain Go host processes; TEE_PROFILE=confidential-space requires a real Confidential Space VM` | `.env` carries the Confidential Space profile |
| `--local mode builds and runs Go binaries in-process and only supports LANGUAGE=go` | this repo ships only `go/`; drop `--local` to run under Compose |
| TEE signatures come back empty | `CHAIN_ID` unset → `chainID=0` |
| `Redis container failed to become healthy` | something else already holds `:6382` |
| `tee-node v… is below the v0.0.22 minimum` | bump the pin in `go/go.mod` **and** `tools/go.mod` |
| `InvalidGovernanceHash` | `GOVERNANCE_SIGNERS` / `GOVERNANCE_THRESHOLD` differ between `post-build.sh` and the node process |
| `Extension ID already set.` | `setExtensionId` is one-shot; only a redeploy of the `InstructionSender` clears it |
| `STRAVA_TOKEN environment variable is not set` | `run-test` requires it; tokens expire after 6 h |
| `Insufficient reward pool balance.` | the contract holds less than 1 native token — send it a plain transfer |
| `Result too old.` | the usable claim window is ~300 s (`FRESHNESS_SECONDS` 360 minus `CLOCK_DRIFT_TOLERANCE` 60) |
| `Proof not for current month.` | the chain crossed 00:00 UTC on the 1st between signing and claiming — request a new proof |
| `pollAction` timeout / `/action/result` 404 | more than one active TEE machine; see [deployment-steps.md](deployment-steps.md) |
