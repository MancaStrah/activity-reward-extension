# Activity Reward Extension

A Flare Confidential Compute (FCC) extension that pays **1 native token** (C2FLR on Coston2) to users who cover **at least 2 km of qualifying Strava activity in the current calendar month (UTC)**. The extension runs inside a TEE (Trusted Execution Environment), verifies Strava fitness data, and signs cryptographic proofs that users submit on-chain to claim rewards.

The layout is a language-neutral spine — contracts, deployment tooling, scripts — plus a single implementation in `go/`. All on-chain work goes through one address, the **FlareTeeManager diamond**: extension registration, TEE governance and machine registration alike. Three properties are worth knowing before you read further: `setExtensionId` is owner-only and takes the expected id explicitly, every deployment runs under an explicit [deployment profile](#deployment-profiles) whose MODE/attestation matrix and proxy config are validated before anything starts, on both the container and the host-process path, and the tee-proxy image is built from a digest-pinned Dockerfile whose source checkout is verified against a full commit SHA.

## How It Works

1. **User** seals a Strava access token into a *grant* bound to six things: the grant domain (`STRAVA_TOKEN_GRANT_V2`), a purpose tag (`STRAVA_DISTANCE`), their wallet, the contract address, the chain id, and an expiry (tools default to 15 minutes; the TEE caps it at 24 h). The grant is ECIES-encrypted (AES-128/SHA-256) to the TEE's public key.
2. **User** calls `getDistanceProof(teeId, encryptedToken)` on the on-chain contract (payable — `msg.value` forwards the instruction fee). The contract checks the requested TEE is in `PRODUCTION` status **and** belongs to this extension, records one pending request per caller, and routes the encrypted grant to the TEE.
3. **TEE** decrypts the grant, re-checks every binding against the on-chain instruction data (never trusting the ciphertext alone), fetches the user's monthly distance from the Strava API, and signs a proof — **always**, whether the distance is eligible or not.
4. **User** fetches the proof from the proxy and calls `claimReward(instructionId, proof)` — **from the same address** that requested it (pending state is keyed by `msg.sender`).
5. **Contract** verifies the TEE signature against the machine registry, checks the eligibility guards, and transfers 1 token — emitting `RewardClaimed`, or `RewardRefused` if the proof is genuine but ineligible.

There is a single operation — `STRAVA` / `DISTANCE` — and it is the same one whether you intend to claim or not: the result carries `eligible` and your distance, so reading it just means fetching the result and never calling `claimReward` (`--no-claim` below). `verifyDistanceProof()` checks a proof's authenticity on-chain without consuming it (and only until it is claimed or cancelled); integrators gating their own logic should use `verifyDistanceProofFor()` instead (see [Contract Reference](#contract-reference)). `cancelPendingProof()` clears an unclaimed request. A **types-server** (port 8100) decodes raw instruction messages/results to JSON for other apps.

> **Production vs Testing:** In a production deployment, the extension Docker image runs inside an actual TEE enclave with hardware attestation, and the proxy is deployed infrastructure managed by the TEE operator. For testing and development, you can run the same Docker containers locally on your machine and use a public tunnel so the proxy is reachable from the Coston2 testnet. The instructions below describe this local testing setup. For a Confidential Space VM deployment, follow [docs/deployment-steps.md](docs/deployment-steps.md).

## What Counts Toward the 2 km

The TEE does not simply sum your Strava feed. Eligible distance is:

- **Only these sport types:** `Run`, `TrailRun`, `Ride`, `VirtualRide`, `MountainBikeRide`, `EBikeRide`. Walks, hikes, swims and everything else are ignored — a user whose 2 km came from a Walk gets `RewardRefused`.
- **No manual or flagged activities** — manually-entered and Strava-flagged activities are excluded.
- **Current calendar month, UTC** — enforced by the TEE on each activity's own `start_date`, which must fall in `[monthStart, now)`. The `after`/`before` query parameters are still sent, but they are not the boundary and they are deliberately **widened** by `StravaQuerySlack` (24 h at each end) so the listing is a strict superset of the attested window: Strava documents them only as filtering "activities that have taken place before / after a certain time", naming no field and no timezone, so a query at the exact bounds could drop activities that do belong to the month — and the enclave would never see them. Asking for more and filtering in the enclave is what makes the month correct for an athlete at any UTC offset. Activity from the previous month is excluded, and so is anything future-dated (which Strava will store).
- **Each activity counted once** — the activity listing is paginated and is not a snapshot, so an upload or edit between two page requests can return the same activity twice. Activities are deduplicated by Strava id.
- **Fail-closed ceilings:** the TEE pages until Strava returns an *empty* page. If it runs out of pages (`StravaMaxPages`) or out of action budget (`ActionBudget` minus `StravaPageTimeReserve`) before that, it refuses to sign rather than report a total that may be incomplete — and the local log records the activities seen and the largest page actually returned, because that, not the requested page size, is what the real ceiling depends on. A qualifying activity with no `start_date` or no id is refused for the same reason: a missing field cannot be silently skipped out of a total that is meant to be complete. Monthly totals above 100 000 km are rejected as nonsensical.

Constants live in `go/internal/config/config.go` (`RewardThresholdKm`, `StravaPerPage`, `StravaMaxPages`, `StravaQuerySlack`, `StravaPageTimeReserve`, `MaxMonthlyKm`) and the allowed sport types in `go/internal/extension/helpers.go`.

## Prerequisites

- **Go 1.25+** — both modules pin `toolchain go1.25.13`, the patch level this build treats as its stdlib security floor; any modern `go` downloads that toolchain automatically
- **Foundry** (`forge`) — compiles the Solidity contract and runs the 33 Solidity tests (claim-flow matrix, `setExtensionId`, cross-language payload vector)
- **jq** — extracts ABI/bytecode from Foundry output
- **Docker** and **Docker Compose**
- A **public tunnel** so the proxy is reachable from the testnet. Two providers, selected with `TUNNEL_PROVIDER`:
  - `ngrok` (default) — run `ngrok http 6674` yourself; the scripts read the URL off the running agent and never start or stop it (see [docs/ngrok.md](docs/ngrok.md)).
  - `cloudflared` — the scripts drive `docker-compose.cloudflared.yaml`: `--tunnel` starts one if none is running, and `stop-services.sh --tunnel` brings it down (see [docs/cloudflared.md](docs/cloudflared.md)).
- A **funded Coston2 account** — get testnet C2FLR from the [Coston2 faucet](https://faucet.flare.network/coston2)
- **Indexer DB credentials** for the proxy — `config/proxy/extension_proxy.coston2.docker.toml` is gitignored, so a clone has only the `.example`; copy it and fill in the `[db]` section (see step 2 below)
- A **Strava account** with recorded activities
- A **Strava API application** (see next section)

## Getting a Strava Access Token

The extension needs a Strava OAuth access token with `activity:read_all` scope to read your activities:

### Step 1: Create a Strava API Application

1. Go to [https://www.strava.com/settings/api](https://www.strava.com/settings/api)
2. Create a new application (or use an existing one)
3. Set the **Authorization Callback Domain** to `localhost`
4. Note your **Client ID** and **Client Secret**

### Step 2: Authorize and Get an Authorization Code

Open the following URL in your browser (replace `YOUR_CLIENT_ID`):

```
https://www.strava.com/oauth/authorize?client_id=YOUR_CLIENT_ID&response_type=code&redirect_uri=http://localhost/exchange_token&approval_prompt=force&scope=activity:read_all
```

Click **Authorize**. You will be redirected to a URL like:

```
http://localhost/exchange_token?state=&code=AUTHORIZATION_CODE&scope=read,activity:read_all
```

The page won't load (localhost isn't running a server), but **copy the `code` parameter** from the URL bar.

### Step 3: Exchange the Code for an Access Token

```bash
curl -X POST https://www.strava.com/api/v3/oauth/token \
  -d client_id=YOUR_CLIENT_ID \
  -d client_secret=YOUR_CLIENT_SECRET \
  -d code=AUTHORIZATION_CODE \
  -d grant_type=authorization_code
```

The response contains your `access_token`.

### Step 4: Set the Token

```bash
export STRAVA_TOKEN=YOUR_ACCESS_TOKEN
```

> **Note:** Access tokens expire after 6 hours. Refresh with:
> ```bash
> curl -X POST https://www.strava.com/api/v3/oauth/token \
>   -d client_id=YOUR_CLIENT_ID \
>   -d client_secret=YOUR_CLIENT_SECRET \
>   -d refresh_token=YOUR_REFRESH_TOKEN \
>   -d grant_type=refresh_token
> ```

## Deploying & Registering on Coston2

### Deployment profiles

Every deployment runs under an explicit **TEE profile** — a coherent MODE/attestation matrix that `start-services.sh` and `post-build.sh` validate fail-fast *before* anything starts. Without that check an inconsistent matrix surfaces only as an opaque `/info` bootstrap timeout:

| `TEE_PROFILE` | Chain | `MODE` | `SIMULATED_TEE` | Proxy `[attestation]` | Meaning |
|---|---|---|---|---|---|
| `local` | local devnet | `1` | `true` | not checked | Everything simulated on your machine (default for `--chain local`) |
| `testnet-sim` | coston/coston2 | `1` | `true` | `enable=true`, `allow_magic_pass=true` | **Simulated TEE** in local Docker against a public testnet. No hardware attestation — signatures prove nothing about the code. Dev/test only; never fund it with real value. Must be selected explicitly. |
| `confidential-space` | coston/coston2 | `0` | `false` | `enable=true`, `allow_magic_pass=false`, and the **full posture** pinned (see below) | Real GCP Confidential Space deployment ([docs/deployment-steps.md](docs/deployment-steps.md)); also requires `LOCAL_MODE=false` |

`MODE` and `SIMULATED_TEE` are two switches that must agree, and the validator enforces that on the two public-chain profiles (on `local` both simply default correctly). `MODE` decides what the *node emits*: `MODE=1` makes it return the `magic_pass` sentinel instead of a Confidential Space attestation token. `SIMULATED_TEE` decides what the *registration tool accepts*: only `SIMULATED_TEE=true` tolerates that sentinel, while `false` sends it to the JWT parser. Setting `MODE=1` with `SIMULATED_TEE=false` therefore aborts `post-build.sh` at "Register TEE machine", so `scripts/lib/profile.sh` rejects that pair up front rather than letting it surface as a parse error deep in step 3.

The walkthrough below is the **`testnet-sim`** flow (local Docker + public tunnel). It is a development setup: convenient for exercising the full on-chain round-trip, but the "TEE" is an ordinary container.

### 1. Create `.env.coston2`

Chain-specific env files are the current workflow: you keep one `.env.<chain>` per network and activate it with `use-chain.sh`. Create `.env.coston2` in the repo root (start from `.env.example`; for a real deployment start from `.env.confidential-space.example`):

```bash
LANGUAGE=go

CHAIN=coston2
CHAIN_ID=114                  # REQUIRED — unset means chainID 0 and every TEE signature comes back empty
CHAIN_URL=https://coston2-api.flare.network/ext/C/rpc
ADDRESSES_FILE=./config/coston2/deployed-addresses.json
NORMAL_PROXY_URL=https://tee-proxy-coston2-1.flare.rocks
EXT_PROXY_URL=                # leave empty — read from the running ngrok agent

TEE_PROFILE=testnet-sim       # explicit opt-in: simulated TEE on a public testnet
LOCAL_MODE=false
SIMULATED_TEE=true            # must agree with MODE=1 (see the note above the walkthrough)
MODE=1                        # simulated attestation — testnet-sim only; MODE=0 means confidential-space
TEE_VERSION=v0.1.0

DEPLOYMENT_PRIVATE_KEY=<funded Coston2 private key, no 0x prefix>
INITIAL_OWNER=0x<address derived from DEPLOYMENT_PRIVATE_KEY>
PROXY_PRIVATE_KEY=<proxy /info signing key — prefer a separate, unfunded key>

# Optional TEE governance (defaults: deployer as sole signer, threshold 1)
# GOVERNANCE_SIGNERS=0xAbc...,0xDef...
# GOVERNANCE_THRESHOLD=1
```

Activate it:

```bash
./scripts/use-chain.sh coston2      # use-chain.sh --list shows available chains
```

### 2. Check the proxy config

`config/proxy/extension_proxy.coston2.docker.toml` must exist with valid `[db]` indexer credentials. The chain-specific config files are gitignored (credentials), so on a fresh clone:

```bash
cp config/proxy/extension_proxy.coston2.docker.toml.example config/proxy/extension_proxy.coston2.docker.toml
# then fill in the [db] section
```

Docker mode mounts `extension_proxy[.<chain>].docker.toml`; `--local` mode runs the proxy as a host process and reads `extension_proxy[.<chain>].toml` from the same directory. `start-services.sh` resolves whichever applies and checks it before anything starts, so both deployment modes go through the same check.

How the checked file ends up being the file the proxy *opens* differs by mode, and it is worth knowing which: in `--local` mode the resolved path is handed to the proxy as `PROXY_CONFIG`, so it is the same string. Under Docker `PROXY_CONFIG` is not read at all — the container opens `/app/config/config.toml`, which compose bind-mounts from a path written literally in each compose file. That literal is a third copy of the filename mapping, so `test-profile-matrix.sh` reads the mount out of every compose file and compares it with the name the scripts resolve; pointing an overlay at another chain's config fails that case rather than silently starting the proxy on a config nothing validated.

The examples ship with a fail-closed `[attestation]` section (`enable = true`, `allow_magic_pass = false`) — never delete it, since omitting the section entirely falls back to the insecure upstream default (`enable = false`). Then match it to your profile:

- **`testnet-sim`** (this walkthrough): set `allow_magic_pass = true` — the simulated TEE presents the `magic_pass` sentinel, and a proxy that rejects it can never finish bootstrapping. `start-services.sh` refuses to start on this mismatch instead of timing out.
- **`confidential-space`**: keep `allow_magic_pass = false` and set the whole posture — `expected_code_hashes`, `expected_platforms`, `expected_debug_statuses`, `audience`, a `max_token_age` that is positive and no longer than an hour, and `require_sec_boot = true`. `start-services.sh` refuses the profile until every one of them is meaningfully set.

  The reason it insists on all of them: the proxy treats an empty list, an empty string, `max_token_age = 0` or `require_sec_boot = false` as *skip that check*, not as a misconfiguration. Leaving one blank therefore removes a control silently — a debuggable TEE, a token minted for someone else, or an arbitrarily old one would all be accepted. The shipped examples carry every key with a placeholder and a per-key note; only `expected_code_hashes` has to come from your own build (see the [allowlisting runbook](docs/production-allowlisting.md)).

  Being *present* is not enough for the two fields where a valid value can still remove the control, so those are checked for meaning as well:

  - `expected_debug_statuses` must be `["disabled-since-boot"]`. It is the one pin here that does **not** fail closed when it is wrong: `["enabled"]` is a perfectly valid `dbgstat` value, so the check runs, passes, and admits a TEE whose memory the host can read — including the Strava tokens it decrypts. A typo like `["disabled"]` is refused for the opposite reason: no token carries it, so the check can never match and the deployment dies as a bootstrap timeout instead of naming the field.
  - `max_token_age` must be positive **and** no longer than an hour. The attestation token carries its own `exp`, always enforced, so this setting can only ever *narrow* the freshness window below the token's own lifetime. A larger value — `876000h`, or `24h` typed for `24m` — is a number that reads as a control and tightens nothing.

  The pre-flight also compares the config's `chain_id` with `CHAIN_ID`. The node signs against `CHAIN_ID` and the proxy verifies against its own `chain_id`, so a disagreement fails closed as signatures that do not verify, with nothing pointing at the file.

The pre-flight that enforces this is `tools/cmd/check-proxy-config`, called by `scripts/lib/profile.sh` before anything starts. It reads the config with the **proxy's own parser and schema** (`go-flare-common/pkg/toml` into `tee-proxy/pkg/config.Attestation`, at the `v0.0.18` pin `proxy/Dockerfile` builds), so the pre-flight and the running proxy cannot interpret the same file two different ways. It replaced a `sed`/`grep`/`awk` reader that could be satisfied by valid TOML the proxy decoded to "skip this check" — `audience = ''`, a comment-only list, or `max_token_age = "0h0m0s"`. On a real deployment it also runs the proxy's whole loader (`config.Read`), which rejects unknown fields, so a mistyped key is named here instead of aborting the container during `up`.

### 3. Pre-build (deploy contract & register extension)

```bash
./scripts/pre-build.sh
```

This checks version pins (`check-versions.sh`), compiles the contract, generates Go bindings, runs a pre-flight check, deploys `StravaInstructionSender` to Coston2, registers the extension on the FlareTeeManager diamond, writes `EXTENSION_ID` + `INSTRUCTION_SENDER` to `config/extension.env`, and binds the contract to that id (step 5 — owner-only `setExtensionId(expectedId)`, so deploy → register → bind is one uninterrupted operator sequence with no public gap). Deploy/register output (both streams) is captured to `config/deploy.log`.

### 4. Start services (with a public tunnel)

Start the ngrok agent first and leave it running:

```bash
ngrok http 6674
```

Then, in another shell:

```bash
./scripts/start-services.sh --chain coston2 --tunnel
```

Prefer this over a bare `docker compose up`: it resolves `LANGUAGE`, builds the proxy image if needed, attaches the Coston2 compose overlay (chain id 114, coston2 proxy config, its own network — every profile now gets a private network of its own), waits for `/info` and validates the `EXTENSION_ID` appears in it, and reads the public URL off the running ngrok agent into `.env` as `EXT_PROXY_URL`. The scripts never start or stop the agent — `--tunnel` just makes the run fail fast when none is reachable instead of timing out later on `/info`.

This starts four containers: **redis**, **ext-proxy**, **extension-tee** (tee-node + extension), and **types-server**.

> **If the tunnel URL rotated after the TEE was already registered, `.env` is only half the fix.** Flare delivers instructions to the URL held in the on-chain machine registry, and `post-build.sh` cannot rewrite it for an already-registered machine — `register-tee` sees a non-zero `teeId` and skips pre-registration, the only step that writes it. The run reports success, the chain leg does succeed, and the registry still points at the dead tunnel, so every instruction times out at `pollAction`. Run `./scripts/update-tee-url.sh` (one `updateTeeMachineSettings` transaction, confirms before sending, no-op when the URL already matches) — see [docs/ngrok.md](docs/ngrok.md).

> **The proxy image is only reused when its provenance matches.** Both build paths stamp `extension.proxy.build` (`pinned` or `siblings`) and the tee-proxy revision onto `local/tee-proxy`. On the next run the labels are re-read, and an image that the current recipe did not produce — unlabelled, left over from a `USE_LOCAL_SIBLINGS` build, or built from an older pin — is neither reused nor silently overwritten: the run stops and tells you to `docker rmi local/tee-proxy` and try again. Without this, a stale image under that tag quietly replaces the hardened, digest-pinned build in `proxy/Dockerfile`. Set `ALLOW_UNVERIFIED_PROXY_IMAGE=true` to downgrade the refusal to a warning.

> **Dev variant:** `USE_LOCAL_SIBLINGS=1 ./scripts/start-services.sh …` builds the image from sibling `../../tee-node` / `../../tee-proxy` checkouts instead of the pinned modules (generates a `go.work`, attaches `docker-compose.siblings.yaml`, builds via `go/Dockerfile.siblings`). Go-only, and it expects `tee-node` and `tee-proxy` checkouts two levels above this repo (`../../`).

### 5. Post-build (register TEE on-chain)

```bash
./scripts/post-build.sh
```

Three steps, all against the diamond:
1. **allow-tee-version** — whitelists the image's codeHash for this extension (uses `EXTENSION_OWNER_KEY` if set, else `DEPLOYMENT_PRIVATE_KEY`). On the simulated profiles it proceeds with a warning (the hash is a dev constant); on `confidential-space` it fails closed unless you supply both `EXPECTED_CODE_HASH` and `EXPECTED_PLATFORM` measured from your own build, so the proxy can never promote its own values — see [docs/production-allowlisting.md](docs/production-allowlisting.md)
2. **set-governance** — registers the governance signer set + threshold (must match what the node container got via `GOVERNANCE_SIGNERS`/`GOVERNANCE_THRESHOLD`; both default to the deployer)
3. **register-tee `-command rRap`** — pre-registers the TEE, issues a fresh challenge, runs the FTDC availability check, and promotes the machine to PRODUCTION. The capital `R` makes re-runs safe (no `Verification.ChallengeExpired`). Override the step letters via `REGISTER_TEE_COMMAND`.

### 6. Test

```bash
STRAVA_TOKEN=YOUR_ACCESS_TOKEN ./scripts/test.sh
```

The test runner (`tools/cmd/run-test`): verifies the extension id against the registry (`test.sh` passes `EXTENSION_ID` when set; otherwise the tool resolves it by scanning, refusing to guess if the address appears under more than one extension), calls `setExtensionId(id)` (tolerates "already set" — pre-build normally binds it already), verifies Go/Solidity month-start agreement, funds the contract with 2 C2FLR, ECIES-encrypts your token with the TEE's public key, sends the DISTANCE instruction, polls the proxy, asserts `verifyDistanceProof()` accepts the genuine proof and rejects a tampered one *while the proof is still unconsumed*, submits `claimReward()` on-chain (expecting `RewardClaimed` or `RewardRefused`), and finally asserts `verifyDistanceProof()` now returns false — the claim consumed the proof, so the contract stops vouching for it. `test.sh` itself takes no flags — it reads everything from the environment.

Or run everything in one shot:

```bash
STRAVA_TOKEN=YOUR_ACCESS_TOKEN ./scripts/full-setup.sh --chain coston2 --tunnel --test
```

`full-setup.sh` runs pre-build → start-services → post-build → test.

### Claiming a reward (the caller-side flow)

`test.sh` is a test harness — it also funds the contract and submits a tampered proof to check it gets rejected. To just *use* the extension:

```bash
export STRAVA_TOKEN=YOUR_ACCESS_TOKEN
./scripts/claim-reward.sh              # request a proof, then claim
./scripts/claim-reward.sh --no-claim   # only read the distance
./scripts/claim-reward.sh --json       # also dump the raw signed proof
```

It fetches the TEE from the proxy and verifies it's a PRODUCTION machine of this extension, seals your token into a caller-bound grant, ECIES-encrypts it, calls `getDistanceProof`, polls for the TEE-signed result, static-calls `verifyDistanceProof` as a pre-flight, and submits `claimReward` — reporting whether `RewardClaimed` or `RewardRefused` fired. The reward goes to whatever `DEPLOYMENT_PRIVATE_KEY` signs with; everything after `--` is passed through to the underlying Go tool, e.g. `-- -ttl 3600` for a longer grant (TEE cap: 24 h), or `-- -key <hex>` / `-- -token <t>` to override a wallet or token.

> Prefer the `DEPLOYMENT_PRIVATE_KEY` and `STRAVA_TOKEN` env vars over the `-key`/`-token` flags: a secret passed as a command-line argument is visible in the process list (`ps`) and is written to your shell history. The tools warn when you use them. Similarly, `CHAIN_URL` must be HTTPS or loopback in `claim-reward`/`encrypt-token` (they gate token encryption on RPC reads) — `ALLOW_INSECURE_RPC=true` bypasses this for dev.

One thing a freshly deployed contract still needs is a funded reward pool:

```bash
cast send <INSTRUCTION_SENDER> --value 2ether \
  --rpc-url "$CHAIN_URL" --private-key $DEPLOYMENT_PRIVATE_KEY   # plain transfer — the contract has receive()
```

The other one-time prerequisite — binding the contract to its extension id — is done by `pre-build.sh` itself (step 5). For a contract deployed some other way, bind manually with `./scripts/claim-reward.sh --set-extension-id` (append `-- -extension-id <id>` to pin the id; without it the tool resolves it from the registry and refuses to guess if the address appears under more than one extension) or `cd tools && go run ./cmd/set-extension-id …`; the first `test.sh` run also sets it as a side effect. The setter is owner-only (`Only owner.`) and one-shot (`Extension ID already set.`). Until it is set, `getDistanceProof` and `claimReward` revert with `Extension ID is not set.`. The pool needs `REWARD_AMOUNT` (1 token) per claim or an eligible claim reverts with `Insufficient reward pool balance.`; `claim-reward.sh` warns before spending gas on either.

### Verifying the deployment

```bash
cd tools
go run ./cmd/verify-deploy -a ../config/coston2/deployed-addresses.json -c "$CHAIN_URL"
go run ./cmd/query-tee -ext <extensionId-decimal> -reg <FlareTeeManager-address> -rpc "$CHAIN_URL"
```

**Always pass `-reg` to `query-tee`** — its compiled-in default is not the diamond, so an omitted `-reg` queries the wrong contract. The diamond address is the `FlareTeeManager` entry in `config/<chain>/deployed-addresses.json` (Coston2: `0x1a9C4A0f9D76c0b1D91d22E24E573a9b377618aE`, Coston: `0xc4885998f5D792ed88C5Af7a3AaCBe333f017658`).

`verify-deploy` exits non-zero on any `FAIL`, which includes finding this contract's address registered under more than one extension — the pre-registration signature. If that happens, do not let a tool resolve the id: pass the one you registered.

`query-tee` lists the TEE machines registered for the extension. **More than one active machine is a problem** — instructions are load-balanced across all of them, so a stale machine swallows requests. See the platform traps in [docs/deployment-steps.md](docs/deployment-steps.md) for how to pause a stale one.

### Status, logs, stopping

```bash
docker compose -f docker-compose.yaml -f docker-compose.coston2.yaml ps
docker compose -f docker-compose.yaml -f docker-compose.coston2.yaml logs -f extension-tee
./scripts/stop-services.sh --chain coston2          # the ngrok agent is yours; it stays up
```

### Redeploying after code changes

```bash
./scripts/start-services.sh --chain coston2   # rebuild + restart containers
./scripts/post-build.sh                        # re-register (new code hash)
```

If the **FlareTeeManager diamond is redeployed**, all extension registrations are wiped: re-run `pre-build.sh` (fresh `EXTENSION_ID`), restart services, `post-build.sh`, `test.sh`.

## Repository Structure

```
├── go/                                 # ── Go implementation (LANGUAGE=go)
│   ├── cmd/main.go                     # Extension server entry point (standalone, for dev)
│   ├── cmd/docker/main.go              # Combined TEE node + extension (single-process image)
│   ├── cmd/start-tee/main.go           # Host-process runner for --local mode
│   ├── cmd/types-server/main.go        # Standalone types-server entry point
│   ├── internal/config/config.go       # Op constants (STRAVA / DISTANCE), thresholds, budgets, TTL caps
│   ├── internal/extension/
│   │   ├── extension.go                # Routing + the single handler: processDistanceProof
│   │   ├── grant.go                    # Domain tags, grant layout, parseAndVerifyGrant
│   │   ├── helpers.go                  # Strava API calls, sport-type filter, TEE node decrypt/sign
│   │   ├── utils.go                    # Boilerplate: actionHandler, buildResult
│   │   ├── extension_test.go           # Unit tests incl. TestSignPayloadCrossLanguageVector
│   │   └── grant_test.go               # Grant layout tests incl. the cross-module TestGrantWireVector
│   ├── internal/typesserver/           # types-server HTTP handlers (/decode, /registry, /health)
│   ├── pkg/decoder/                    # Decoder registry: JSON, tuple-ABI and flat-ABI decoders
│   ├── pkg/server/                     # StartExtension()
│   ├── pkg/types/                      # Request/response types, ABI layouts, decoder registration
│   ├── Dockerfile                      # Reproducible distroless image (extension-tee + types-server)
│   ├── Dockerfile.siblings             # Dev image built from sibling tee-node checkout
│   ├── README.md                       # Go implementation guide (locking, flat-ABI, add-an-op recipe)
│   └── language.env                    # Marks go/ as an implementation (LANGUAGE=go)
├── contracts/InstructionSender.sol     # StravaInstructionSender (see Contract Reference below)
├── contracts/interfaces/               # Minimal diamond interfaces (nextPublicExtensionId, TeeStatus…)
├── test/SignPayloadVector.t.sol        # Solidity half of the cross-language sign-payload vector (forge test)
├── test/SetExtensionId.t.sol           # 6 tests: owner-only binding, id checks, pre-registration capture (forge test)
├── test/ClaimReward.t.sol              # 26-test behavioral matrix for the claim flow (payout, replay, quotas, reentrancy…)
├── foundry.toml                        # via-ir build, solc pinned to 0.8.27; src = contracts/, out = out/
├── config/
│   ├── extension.env                   # Written by pre-build (EXTENSION_ID, INSTRUCTION_SENDER); gitignored along with
│   │                                   #   every other *.env, so a fresh clone has none until pre-build writes it —
│   │                                   #   compose reads it with `required: false`, the scripts source it when present
│   ├── coston/ · coston2/              # deployed-addresses.json per chain (FlareTeeManager diamond + facets)
│   ├── proxy/                          # Proxy configs (chain-specific .toml gitignored for credentials; the
│   │                                   #   credential-free extension_proxy[.docker].toml and all .example variants shipped)
│   └── deploy.log                      # output of the last deploy/register run; gitignored
├── scripts/                            # Language-neutral lifecycle scripts
│   ├── full-setup.sh                   # pre-build → services → post-build → test
│   ├── pre-build.sh                    # version check + bindings + preflight + deploy + register + bind id
│   ├── post-build.sh                   # allow version + set governance + register TEE (rRap)
│   ├── start-services.sh / stop-services.sh
│   ├── use-chain.sh                    # activate .env.<chain> (--list to enumerate)
│   ├── test-profile-matrix.sh          # unit tests for the fail-fast profile matrix
│   ├── test.sh · test-unit.sh · test-types-server.sh
│   ├── claim-reward.sh                 # the caller-side flow (proof → claim)
│   ├── update-tee-url.sh               # repoint the on-chain machine URL after the tunnel URL rotates
│   │                                   #   (post-build.sh cannot: it skips pre-registration, the only writer)
│   ├── generate-bindings.sh            # forge build → abigen (StravaInstructionSender)
│   ├── check-versions.sh               # pin consistency: tee-node (≥ v0.0.22), go-flare-common, tee-proxy, launch label vs docs,
│   │                                   #   TEE_VERSION agreement across the templates + post-build
│   ├── check-docs.sh                   # docs standard: presence, platform traps, the Solidity test count
│   ├── e2e.sh                          # background-process supervisor (wait-for-url, start/stop) used by the lifecycle scripts
│   └── lib/                            # language.sh (LANGUAGE discovery), versions.sh (pins), profile.sh
│                                       #   (fail-fast MODE matrix; resolves the proxy config and checks its
│                                       #   posture, chain_id and loadability via tools/cmd/check-proxy-config)
├── tools/                              # Language-neutral deployment tooling (Go)
│   ├── cmd/{deploy-contract,register-extension,set-extension-id,set-governance,allow-tee-version,register-tee}
│   ├── cmd/{run-test,claim-reward,encrypt-token,get-result,test-types-server}
│   ├── cmd/{query-tee,verify-deploy,audit-deploy,start-proxy}
│   ├── cmd/check-proxy-config            # proxy-config pre-flight: reads the file with the
│   │                                     # PROXY's own parser and schema, not shell regexes
│   ├── pkg/{contracts/strava,fccutils,support,utils,validate,configs}
│   └── integration/                    # On-chain integration tests (go test -tags integration)
├── proxy/Dockerfile                    # Self-contained tee-proxy build (v0.0.18 @ full SHA, digest-pinned bases, security backports)
├── .github/workflows/ci.yml            # CI gates: forge (fmt/build/test), go vet + race tests + govulncheck in
│                                       #   both modules, profile matrix, version pins, docs standard, gitleaks
│                                       #   (CLI pinned by digest, whole history), double image build with a
│                                       #   digest comparison
├── docker-compose.yaml                 # redis + ext-proxy + extension-tee + types-server
├── docker-compose.coston2.yaml         # Coston2 overlay (chain id 114, coston2 proxy config)
├── docker-compose.coston.yaml          # Coston overlay (chain id 16; first create extension_proxy.coston.docker.toml from its .example)
├── docker-compose.siblings.yaml        # USE_LOCAL_SIBLINGS=1 dev overlay (build from sibling tee-node)
├── docker-compose.cloudflared.yaml     # Manual cloudflared tunnel alternative to ngrok
├── docs/                               # Guides: deployment, testing, wire contract, tunnels… + production-allowlisting.md
│                                       #   (allowlisting runbook), security/ (govulncheck evidence + exceptions)
├── REPRODUCIBILITY.md                  # What the image build guarantees for the codeHash
├── .env.example                        # testnet-sim profile template (simulated TEE, explicit opt-in)
└── .env.confidential-space.example     # real Confidential Space profile template (MODE=0, fail-closed attestation)
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LANGUAGE` | `go` | Implementation directory (`<dir>/language.env` must exist) |
| `STRAVA_TOKEN` | (none, required for `test.sh` and `claim-reward.sh`) | Strava OAuth access token with `activity:read_all` scope |
| `DEPLOYMENT_PRIVATE_KEY` | dev key, only honored when `LOCAL_MODE=true` (fail-closed otherwise) | Funded key for deployments and on-chain calls |
| `PROXY_PRIVATE_KEY` | none — compose refuses to start without it | Proxy `/info` signing key. There is no fallback: leave it unset and `compose up` aborts with `set PROXY_PRIVATE_KEY in .env` |
| `EXTENSION_OWNER_KEY` | falls back to `DEPLOYMENT_PRIVATE_KEY` | Key used by `allow-tee-version` when the extension owner differs from the deployer |
| `ETH_KEYSTORE_ACCOUNT` / `ETH_KEYSTORE` | unset | **Preferred over `DEPLOYMENT_PRIVATE_KEY` for `update-tee-url.sh`.** Names an encrypted `cast` keystore (`cast wallet import <name> --interactive`) instead of a raw key: `cast` reads the key itself and prompts for the passphrase, so it never appears in this process's argv. A raw `DEPLOYMENT_PRIVATE_KEY` still works but must be passed as `--private-key`, which is visible in the process list for the lifetime of the call |
| `INITIAL_OWNER` | hardcoded compose fallback address | Contract/TEE owner — set it to the address of `DEPLOYMENT_PRIVATE_KEY` |
| `CHAIN` | none in the scripts — they read it from `.env`, and both shipped templates set `coston2` | `local` \| `coston` \| `coston2`. `--chain <name>` on the lifecycle scripts overwrites `.env` with `.env.<name>`, so the flag wins. Where a standalone binary has to guess (`go run ./cmd/start-proxy` with `CHAIN` unset) it derives it from `LOCAL_MODE`: unset or `true` → `local`, anything else → `coston2` |
| `CHAIN_ID` | `31337` (base compose); overlays set `114` / `16` | **Required for signing** — unset means chainID 0 and empty TEE signatures. When set, the tools also fail closed if the RPC reports a different chain id. Launch-policy overridable on the image (Confidential Space deploys set it at launch) |
| `CHAIN_URL` | `http://127.0.0.1:8545` | Chain RPC endpoint |
| `ALLOW_INSECURE_RPC` | unset | `true` skips the HTTPS/loopback requirement on `CHAIN_URL` in `claim-reward`/`encrypt-token` (dev only) |
| `ADDRESSES_FILE` | auto-detected per chain | Path to `deployed-addresses.json` |
| `TEE_PROFILE` | inferred for `local`/`MODE=0`; **explicit `testnet-sim` required otherwise** | Deployment profile (`local` \| `testnet-sim` \| `confidential-space`) — see [Deployment profiles](#deployment-profiles). `start-services.sh`/`post-build.sh` fail fast on an inconsistent matrix |
| `LOCAL_MODE` | `true` (shell default) | Must be `false` on live networks (disables dev-key fallbacks, enables secure-URL gates). `testnet-sim` and `confidential-space` both **refuse to start** unless it is `false`, and unset is refused as well: `start-proxy` and the allow-listing gate read an unset value as local, so silence would select the dev-key behaviour |
| `SIMULATED_TEE` | none — **required** | Must agree with `MODE`: `true` for `local`/`testnet-sim` (`MODE=1`), `false` for `confidential-space` (`MODE=0`). `MODE=1` with `false` makes `register-tee` parse the `magic_pass` sentinel as a real attestation JWT and abort |
| `MODE` | none — **required**; image bakes `0` | Attestation mode: `1` = simulated (`local`/`testnet-sim`), `0` = real Confidential Space. Neither this nor `SIMULATED_TEE` has a compose default: `docker compose up` run directly skips every profile check, so it has to state its own attestation posture rather than inherit a simulated one. Deliberately **not** overridable via the image's launch policy |
| `EXT_PROXY_URL` | `http://localhost:6674` | Extension proxy URL (tunnel URL on Coston2; `6664` in `--local` mode) |
| `EXT_PROXY_HOST_URL` | falls back to `EXT_PROXY_URL` | The URL `post-build.sh` registers on-chain for the machine (`register-tee -h`), when it must differ from the one the scripts poll — e.g. a public hostname in front of the proxy. Get it wrong and instructions are delivered somewhere unreachable; `update-tee-url.sh` is the repair |
| `NGROK_API_PORT` | `4040` | Local API port of your running ngrok agent (a second agent uses 4041) |
| `PROXY_CONFIG` | resolved per chain and mode | Proxy config the host-process proxy reads. `start-services.sh` sets it to the file it validated; a standalone `go run ./cmd/start-proxy` derives it from `CHAIN` instead (falling back to `LOCAL_MODE` when `CHAIN` is unset), and refuses a `CHAIN` that is not a plain chain name rather than falling back to another chain's config. **Not read on the Docker path** — there the compose bind-mount decides, see [step 2](#2-check-the-proxy-config) |
| `NORMAL_PROXY_URL` | `http://localhost:6662` | FTDC proxy (`https://tee-proxy-coston2-1.flare.rocks` on Coston2) |
| `EXTENSION_ID` | from `config/extension.env` | Set by pre-build |
| `INSTRUCTION_SENDER` | from `config/extension.env` | Set by pre-build. Read by the enclave as well as the tools: with `CHAIN_ID` it is the deployment the enclave will sign for, and an instruction naming any other contract or chain is refused (see [Proof format](#proof-format-and-cross-language-coupling)). Launch-policy overridable, so a Confidential Space deploy fixes it at launch |
| `GOVERNANCE_SIGNERS` | deployer | Comma-separated governance signer addresses (launch-policy overridable on the image) |
| `GOVERNANCE_THRESHOLD` | `1` | Minimum governance signatures (launch-policy overridable on the image) |
| `TEE_VERSION` | `v0.1.0` | Version string for TEE registration — the label `allow-tee-version` attaches to the allow-listed TEE **image** on-chain, independent of the extension `Version` in `go/internal/config/config.go` (the `stateVersion`/`ActionResult` contract version); `check-versions.sh` warns when the pair moves |
| `REGISTER_TEE_COMMAND` | `rRap` | Step letters passed to `register-tee` by post-build |
| `TYPES_SERVER_PORT` | `8100` | Types server HTTP port |
| `TYPES_SERVER_URL` | `http://localhost:8100` | Which types-server `./scripts/test-types-server.sh` runs its 8 decode cases against |
| `LOG_LEVEL` | `INFO` (compose) | Container log level (launch-policy overridable) |
| `WAIT_TIMEOUT` | `120` | Service wait timeout (seconds) |
| `USE_LOCAL_SIBLINGS` | unset | `1` = build tee-node/tee-proxy from sibling checkouts (dev only) |
| `ALLOW_UNVERIFIED_PROXY_IMAGE` | unset | `true` downgrades to a warning the refusal to reuse a `local/tee-proxy` image that the current build recipe did not produce (see [Start services](#4-start-services-with-a-public-tunnel)) |
| `REDIS_BIND` / `EXT_PROXY_INTERNAL_BIND` / `EXT_PROXY_EXTERNAL_BIND` / `TYPES_SERVER_BIND` | see Ports | Host port-binding overrides for compose |
| `TUNNEL_PROVIDER` | `ngrok` | `ngrok` (read the URL off your own agent) \| `cloudflared` (the scripts start and stop the tunnel container) |
| `TUNNEL_ARGS` / `TUNNEL_TARGET` | quick tunnel → `host.docker.internal:6674` | cloudflared overlay: named-tunnel args / target URL |

Build-time knobs (set by the scripts, rarely by hand): `SOURCE_DATE_EPOCH`, `TEE_NODE_REF`, `EXTENSION_DOCKERFILE`, `REGISTRY`. `COMPOSE_NETWORK` is a **runtime** one and the exception worth reading twice: it renames the per-profile compose network, and it is the only switch that can undo the isolation the [Ports](#ports) table relies on — point it at a network shared with unrelated containers and the node's unauthenticated config server on 5501 is reachable from every one of them, whatever `ports:` publishes. Container-internal: `PROXY_URL`, `CONFIG_PORT`, `SIGN_PORT`, `EXTENSION_PORT`, `TYPES_SERVER_HOST` (types-server bind interface — compiled default `127.0.0.1`; compose sets `0.0.0.0` inside the container and controls exposure via the host-side bind instead).

## Ports

Host-published under Docker (`docker-compose.yaml`):

| Host port | Container port | Service | Purpose |
|-----------|----------------|---------|---------|
| 127.0.0.1:6382 | 6379 | redis | Queue storage for proxy |
| 127.0.0.1:6673 | 6663 | ext-proxy (internal) | TEE polls actions from proxy queue — no application auth, so loopback-only, and **only on the local devnet**: the coston/coston2 overlays drop this publish entirely (the TEE reaches it over the compose-internal network) |
| 6674 | 6664 | ext-proxy (external) | Clients query results, `/info` endpoint |
| 127.0.0.1:8100 | 8100 | types-server | Decodes raw instruction data to JSON — unauthenticated, loopback-only by default |

The `*_BIND` env vars override the host side (e.g. `TYPES_SERVER_BIND=0.0.0.0:8100` deliberately exposes the types-server; do the same for the internal proxy port only on a firewalled/mTLS'd host).

Container-internal only — **not** published to the host (set via compose env, baked into the image as `ENV`):

| Port | Purpose |
|------|---------|
| 5501 | `CONFIG_PORT` — sets the proxy URL, initial owner, extension ID, chain id and governance signer set. **Unauthenticated, and it binds every interface inside the container**, so anything that can route to the container can reconfigure the node. Docker networks have no port-level isolation, which is why each profile gets a private network rather than a shared one |
| 7701 | `SIGN_PORT` — extension calls TEE for signing/decrypting |
| 7702 | `EXTENSION_PORT` — TEE forwards `POST /action` to extension. Binds loopback deliberately: the endpoint authenticates nothing, so it must never be reachable from outside |

In `--local` (non-Docker) mode the proxy listens on **6663/6664 directly** (no host remap), and the Go binaries' compiled-in defaults apply when the env vars are unset: `EXTENSION_PORT` 8080, `SIGN_PORT` 9090.

## Contract Reference

`contracts/InstructionSender.sol` — `StravaInstructionSender`. One operation: OPType `STRAVA`, OPCommand `DISTANCE`.

### Write functions

| Function | Notes |
|----------|-------|
| `getDistanceProof(address teeId, bytes encryptedToken)` | `payable` — `msg.value` forwards the instruction fee. Requires the TEE to be `PRODUCTION` **and** registered to this extension. One pending request per `msg.sender` |
| `claimReward(bytes32 instructionId, DistanceProof proof)` | Verifies the proof and pays `REWARD_AMOUNT` (1 token). Must be called from the address that requested the proof. Ineligible-but-genuine proofs emit `RewardRefused` **and are consumed** (no retry with the same instruction) |
| `cancelPendingProof()` | Clears the caller's unclaimed request and refunds its storage |
| `setExtensionId(uint256 expectedId)` | Owner-only, one-shot; requires the registry to map `expectedId` (≥ `0x10000`) to this contract. The registry scan lives in the tools (`ResolveExtensionId`), which refuses to guess when the address appears under more than one id |
| `withdraw(uint256)` | Owner-only pool withdrawal |
| `receive()` | Plain transfers fund the reward pool |

### Read functions

- `verifyDistanceProof(instructionId, proof)` — checks authenticity + freshness without consuming the proof, and **only while the proof is still unconsumed**: `claimReward` and `cancelPendingProof` both clear the pending record, after which this returns false. Authenticity is the same question `claimReward` asks — all three fields of the pending record (`instructionId`, `challenge`, `teeId`) must match, so a signature over a never-issued challenge, or from a machine the caller did not route to, returns false even when that machine is a production TEE of this extension. **Not a fitness check and not caller-bound**, though — anyone could satisfy it with a stranger's proof.
- `verifyDistanceProofFor(instructionId, proof, expectedCaller, minDistanceX1000)` — what integrators should gate on: additionally pins the proof to a caller and a minimum distance.
- `canClaimAddress()` / `canClaimAddress(address)` / `canClaimAthlete(bytes32)` / `canClaim(bytes32)` / `canClaim(address,bytes32)` — monthly-quota pre-checks.
- `currentMonthStart()`, `extensionId()`, `getTee()`, plus public mappings `pendingProofs`, `lastPaidMonth`, `lastPaidAthleteHash`.

### Events

`RewardRequested`, `RewardClaimed`, `RewardRefused`, `PendingProofCancelled`, `Withdrawal`. `RewardClaimed`/`RewardRefused` carry `(user, instructionId, distanceX1000, monthStart, athleteHash)`.

### Claim guards

- **One reward per wallet address per month** (`Address already paid this month.`)
- **One reward per Strava athlete per month** via athlete ID hash (`Strava account already paid this month.`)
- **Distance threshold: 2 km** — `DISTANCE_THRESHOLD_X1000 = 2000` in the contract and `RewardThresholdKm = 2.0` in the TEE; keep them in step
- **Proof is for the current month** — `proof.monthStart` must equal `currentMonthStart()` (`Proof not for current month.`)
- **Freshness:** `FRESHNESS_SECONDS` is 360 s, but since the same constant bounds future-dated proofs via `CLOCK_DRIFT_TOLERANCE` (60 s), the usable claim window is **~300 s**
- **TEE signature** verified against the machine registry: the recovered signer must be in `PRODUCTION` status **and** belong to this extension, and must match both the proof's `teeId` and the pending request's. High-`s` (malleable) signatures are rejected outright.

### Month boundaries

Go and Solidity compute the month start with different algorithms over different clocks (enclave wall-clock vs `block.timestamp`) but agree on the result: first day of the current calendar month, 00:00 UTC (`run-test` cross-checks this at runtime). Consequences worth knowing:

- Near the end of a month, the ~300 s claim window collapses to zero the moment the chain crosses 00:00 UTC on the 1st — a proof signed at 23:58 is unclaimable at 00:01.
- The TEE samples its clock exactly once per request and refuses (with a "retry" error) if the month rolls over mid-fetch, so a request can never report last month's kilometres under the new month's label.

### Proof format and cross-language coupling

The TEE signs `keccak256(abi.encode(...))` of 12 flat fields — `DOMAIN_DISTANCE_PROOF` (= `keccak256("STRAVA_DISTANCE_PROOF_V1")`), `chainId`, `verifyingContract`, `instructionId`, `timestamp`, `challenge`, `caller`, `teeId`, `eligible`, `distanceX1000`, `monthStart`, `athleteHash` — wrapped in the EIP-191 `personal_sign` prefix. The JSON result returned via the proxy additionally carries `distanceKm` (informational — it is **not** part of the signed payload; only the rounded `distanceX1000` is) and a human-readable `message`.

`chainId` and `verifyingContract` are the two fields that say *where* a proof is valid, and they arrive in the instruction message rather than being known to the enclave a priori. The enclave therefore anchors them before it signs: `CHAIN_ID` and `INSTRUCTION_SENDER` name the deployment it belongs to, and an instruction quoting any other pair is refused outright, before the token is decrypted and before any Strava call is made. Both names are launch-policy overridable, so a Confidential Space deployment fixes them through the attested launch policy. The grant binding is not a substitute for this: a grant is sealed by the same requester who wrote the message, so the two agree trivially when that requester chose both. The contract's own check is not a substitute either — `claimReward` recomputes the hash with its own address and `block.chainid`, which stops a foreign proof being *used* here, not this enclave from *producing* one.

This encoding is duplicated in two places — Solidity (`_recoverProofSigner`) and the Go TEE (`abiEncodeDistanceProofPayload`); the client tools never build it, they submit the proof and let the contract verify. The sign-payload half is pinned by a **paired cross-language vector test**: `test/SignPayloadVector.t.sol` (run with `forge test` — no script runs it automatically) and `TestSignPayloadCrossLanguageVector` in `go/internal/extension/extension_test.go` assert the same hash for the same inputs. The **grant** layout is likewise duplicated between `go/internal/extension/grant.go` and `tools/pkg/fccutils/grant.go` and must be kept byte-for-byte identical by hand — change one, change all.

> **Privacy note:** `athleteHash = keccak256(athleteId)` is a *pseudonym, not anonymisation* — Strava athlete IDs are small sequential integers, so anyone can enumerate them and link the hash in `RewardClaimed`/`RewardRefused` events back to a Strava profile and a wallet.
>
> Requesting a proof is itself a disclosure, claimed or not. `getDistanceProof` emits `RewardRequested(user, instructionId, challenge)` with **both** the wallet and the instruction id indexed, and the proxy serves `GET /action/result/<instructionId>` **unauthenticated**. That body is the entire result — including `caller` (the wallet address itself), `distanceKm`, `athleteHash` and the human-readable message — so a wallet can be tied to a monthly distance and an athlete pseudonym from public data alone, without reading the chain. `distanceX1000` is necessarily public, because `claimReward` verifies the signature over it; `distanceKm` and `message` are not signed and are exposed only because the result carries them. Anyone who requests a proof just to read their own number, and never claims, is disclosed on the same terms.

## Types Server

`POST /decode` on port 8100 decodes hex payloads:

```bash
curl -s localhost:8100/decode -d '{"opType":"STRAVA","opCommand":"DISTANCE","kind":"result","data":"0x…"}'
```

`kind` is `message` (flat-ABI-encoded contract → TEE payload) or `result` (JSON TEE → caller payload); `data` must be `0x`-prefixed hex. The only registered decoder pair is `STRAVA`/`DISTANCE`. Responses: 400 for a bad body/kind/hex, 404 for an unregistered `(opType, opCommand, kind)`, 422 when the payload doesn't decode. `GET /registry` lists registered decoders; `GET /health` reports status + extension version. The types-server has no auth, but it is bounded (server timeouts, 64 KiB header cap, 1 MiB `/decode` body cap) and loopback-only by default: standalone it binds `TYPES_SERVER_HOST` (default `127.0.0.1`), and the container publishes to `127.0.0.1:8100` — expose it deliberately with `TYPES_SERVER_BIND=0.0.0.0:8100`.

Exercise it with `./scripts/test-types-server.sh` — 8 decode cases covering both kinds, the removed-`REWARD` 404, and every error status. It expects a types-server already listening on 8100 (the Docker stack, or `cd go && go run ./cmd/types-server`).

## Testing

| Layer | Command | Needs |
|-------|---------|-------|
| Go unit tests (32): grant parsing, ABI round-trips, month math, the attested-window, query-superset and pagination guarantees, both fetch budgets, the cross-language sign-payload vector and the cross-module grant wire vector | `./scripts/test-unit.sh` (or `cd go && go test ./...`) | nothing |
| Solidity tests (33): 26-test claim-flow behavioral matrix, 6 `setExtensionId` tests, sign-payload vector | `forge test` | Foundry. Run in CI; run locally whenever the payload encoding or the contract changes |
| Deployment profile fail-fast matrix (98 cases) | `./scripts/test-profile-matrix.sh` | a Go toolchain: the `[attestation]` posture pre-flight it exercises is `tools/cmd/check-proxy-config`, which `profile.sh` builds on first use. Also run in CI |
| Tooling unit tests (revert decoding, validation, the allowlist gate, strict hex/address parsing, terminal-output sanitization, `get-result` proof validation, the signed-vs-displayed distance agreement, governance signer/threshold parsing, the launch-policy label cross-check, the `[attestation]` posture pre-flight itself, proxy-config resolution, and the extension-id scans — which must refuse rather than report an id as unambiguous when part of the registry could not be read) | `cd tools && go test ./...` | nothing |
| Types-server decode contract (8 cases) | `./scripts/test-types-server.sh` | a running types-server (`cd go && go run ./cmd/types-server`) |
| On-chain integration tests (constructor, setExtensionId, CheckTx, registration, preflight) | `cd tools && go test -tags integration ./integration/ -v -count=1` | live chain + `CHAIN_URL`, `ADDRESSES_FILE`, `DEPLOYMENT_PRIVATE_KEY` |
| End-to-end (deploy → proof → claim → tamper check) | `STRAVA_TOKEN=… ./scripts/test.sh` | full deployed stack |

## Operational Limits (TEE side)

From `go/internal/config/config.go` — extension version **0.2.0** (hashed into the `/info` state version, so bumping it changes what the availability check sees):

| Limit | Value | Effect |
|-------|-------|--------|
| `ActionBudget` | 1 800 ms | The whole decrypt → Strava fetch → sign round-trip must fit inside tee-node's 2 s POST timeout. Budget for at least three Strava calls: the athlete lookup, the first activity page, and the empty page that proves the month is complete |
| `StravaPageTimeReserve` | 300 ms | Held back from `ActionBudget` for the work after the last page (athlete hash, payload encoding, `/sign`, marshalling). The paging loop refuses to start a page that would eat into it, so running out of time is reported as "the listing was too long" instead of dying on the signing call |
| `StravaQuerySlack` | 24 h | How far the `after`/`before` query is widened past the attested window at each end, so the listing is a strict superset of it whichever field Strava filters on. Covers every real UTC offset (max ±14 h) |
| `MaxGrantTTL` | 24 h | Grants with a longer expiry are rejected (tools default to 15 min / `-ttl 900`) |
| `StravaPerPage` / `StravaMaxPages` | 200 / 10 | Paging stops on an empty page, never on a short one — Strava promises no maximum for `per_page`, so a short page does not mean the last page. `StravaMaxPages` is an upper bound, not the operating limit: how many pages one action can afford is decided by `ActionBudget`, and how many activities that covers depends on the page size Strava actually returns. Both exhaustion paths refuse to sign |
| `MaxMonthlyKm` | 100 000 | Sanity ceiling on the summed distance |
| `MaxRequestBytes` / `MaxResponseBytes` | 1 MiB / 4 MiB | HTTP body caps |

The extension also serves `GET /state` (signed-proof counters only — deliberately minimal, since real state rides in the signed `/info` response used for on-chain availability checks).

> **The Strava quota is shared and nothing throttles it.** Strava rate-limits per *application*, not per user or per token, and the tier governing every call made here is the non-upload one: **100 requests every 15 minutes, 1 000 per day**, shared by everyone using this extension's client id. Nothing bounds consumption — `getDistanceProof` has no per-caller cooldown (it simply overwrites `pendingProofs[msg.sender]`), and there is no limiter, cache or backoff anywhere in `go/`. One proof costs at least three calls, and up to eleven for an athlete with a very full month, so on the order of thirty instructions inside a single window exhausts it for everybody; requests already rejected by the short-term limit still count against the daily budget. Legitimate users then see `Strava activities API failed with status 429` until the window rolls. The failure is clean — no incorrect proof is produced and nothing is overpaid — but the extension's only operation is unavailable for as long as it lasts, and the same ceiling caps honest throughput. This is an accepted limitation of this version rather than an oversight.

## Platform Integration Notes

Facts about the on-chain platform this extension binds to. Each one is a place where a
wrong assumption fails in a way that is hard to read from the error alone.

- **One diamond address.** `FlareTeeManager` serves both registries, so both constructor arguments of the contract receive the same address. There are no separate `TeeExtensionRegistry` / `TeeMachineRegistry` deployments to point at.
- **`setExtensionId(uint256 expectedId)`** is owner-only, single-use, and takes the id explicitly rather than discovering it; `test/SetExtensionId.t.sol` covers the binding rules and the pre-registration capture scenario. Discovery lives in the tools' `ResolveExtensionId`, which scans `nextPublicExtensionId()` from `0x10000` up and refuses to guess when an address is registered under more than one extension. `pre-build.sh` binds the id immediately after registering (step 5), so deploy → register → bind is one uninterrupted sequence. Note there is no `extensionsCounter()` on-chain.
- **`TeeStatus` ordinals matter.** `getTeeMachineStatus` returns the raw enum value and `PRODUCTION` is **2** (`NONE`, `INITIALIZED`, `PRODUCTION`, `SUSPENDED`, `PAUSED`, `BANNED`); an interface declaring them in another order compares against the wrong ordinal silently. Both `getExtensionId` and `getTeeMachineStatus` also *revert* with `TeeNotFound` for a never-registered address, which is why the contract wraps them in `try/catch` wherever a predicate must answer `false` instead of throwing.
- **TEE governance is mandatory.** Post-build registers a signer set + threshold (`set-governance`) before `register-tee`, and the same values reach the node container through the environment. A mismatch between the two is rejected with `InvalidGovernanceHash`.
- **`register-tee` runs `rRap`** — the capital `R` issues the attestation challenge as its own step, so re-registering after an image change does not revert with `ChallengeExpired`.
- **Pins**: tee-node `v0.0.24` (platform minimum v0.0.22), tee-proxy `v0.0.18` (clone verified against its full commit SHA), go-ethereum `v1.17.4`, Go toolchain `go1.25.13` (pinned in both `go.mod`s and the `go/Dockerfile` base-image digest), solc `0.8.27` (foundry.toml). `check-versions.sh` (a hard gate in pre-build) enforces that tee-node and go-flare-common match across `go/go.mod` and `tools/go.mod`, that tee-node ≥ v0.0.22, and that the tee-proxy pin matches `proxy/Dockerfile` — go-ethereum itself is not checked.
- **Proxy config**: the key is named by `private_key_variable = "PROXY_PRIVATE_KEY"`, `initial_signing_policy_offset` is `2` on Coston/Coston2 and `0` on local configs, and the live-network configs carry a fail-closed `[attestation]` section.
- **Reproducible distroless image** with Confidential Space launch-policy labels: digest-pinned builder, `snapshot.debian.org` apt, `SOURCE_DATE_EPOCH` normalization. `MODE` is deliberately excluded from `allow_env_override` — it selects the attestation backend, and the measured image is what should pin it — while the deployment-specific `CHAIN_ID` / `GOVERNANCE_SIGNERS` / `GOVERNANCE_THRESHOLD` are included so a real Confidential Space deploy sets them through the attested launch policy (cross-checked on-chain by post-build's `set-governance`).
