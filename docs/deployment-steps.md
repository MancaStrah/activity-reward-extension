# 🚀 TEE Extension Deployment — Step by Step

Linear recipe to deploy a TEE extension to Flare Coston or Coston2. Run the steps top to bottom.

## Prerequisites

- 🐳 Docker Desktop (Linux containers)
- 🐹 Go 1.25+ (both modules pin `toolchain go1.25.13`; any modern `go` downloads it automatically)
- 🔨 Foundry (`forge`, `cast`)
- `jq`
- Bash (Git Bash on Windows works)
- VPN access to Flare's indexer DB — host, database and credentials come from whoever provides indexer access; see the `[db]` section of `config/proxy/extension_proxy.<chain>.docker.toml.example`

## 1. Get the extension repo

The default build is **self-contained**: `go.mod` pins `tee-node` and
`proxy/Dockerfile` pins `tee-proxy`, both fetched from the network at build
time. You only need this repo — no sibling `tee-node/` or `tee-proxy/`
checkouts.

```text
<workspace>/
└── <your-extension>/     # this repo — builds standalone
```

> **Developing `tee-node`/`tee-proxy` locally?** Place them as siblings and use
> the opt-in toggle, which builds the node + proxy from your on-disk checkouts
> instead of the pinned versions:
>
> ```text
> <workspace>/tee/
> ├── tee-node/         # github.com/flare-foundation/tee-node
> ├── tee-proxy/        # github.com/flare-foundation/tee-proxy
> └── extension-examples/
>     └── <your-extension>/
> ```
>
> ```bash
> USE_LOCAL_SIBLINGS=1 ./scripts/start-services.sh --chain coston2
> ```

## 2. Generate a funded deployer key

```bash
cast wallet new
cast wallet address --private-key 0x<private-key>
```

The derived address becomes your `INITIAL_OWNER`. Fund it from the target chain's faucet.

| Chain   | Faucet                                 |
| ------- | -------------------------------------- |
| Coston  | `https://faucet.flare.network/coston`  |
| Coston2 | `https://faucet.flare.network/coston2` |

## 3. Create `.env.<chain>`

Start from `.env.confidential-space.example` — this walkthrough is the real-TEE profile — and save it as `.env.coston` or `.env.coston2`. Fill in:

```bash
CHAIN=coston2                                                         # or coston
CHAIN_URL=https://coston2-api.flare.network/ext/C/rpc                 # chain RPC
ADDRESSES_FILE=./config/coston2/deployed-addresses.json
NORMAL_PROXY_URL=https://tee-proxy-coston2-1.flare.rocks              # FTDC proxy
EXT_PROXY_URL=                                                        # leave empty — set in Step 6

TEE_PROFILE=confidential-space
LOCAL_MODE=false
SIMULATED_TEE=false
MODE=0
DEPLOYMENT_PRIVATE_KEY=<private key, no 0x prefix>
INITIAL_OWNER=0x<derived address from Step 2>
```

The scripts validate this matrix fail-fast (`scripts/lib/profile.sh`): `confidential-space` requires exactly `MODE=0`, `SIMULATED_TEE=false`, `LOCAL_MODE=false`, and a proxy config with the complete fail-closed attestation posture — `enable = true`, `allow_magic_pass = false`, plus a meaningful `audience`, `expected_code_hashes`, `expected_platforms`, `expected_debug_statuses`, `max_token_age` and `require_sec_boot`. An empty, zero or false value there disables that one check inside the proxy instead of erroring, so none may be left unset; `config/proxy/extension_proxy.<chain>.docker.toml.example` documents each key and is the source of truth. Two of them are checked for meaning rather than presence, because a *valid* value can still remove the control: `expected_debug_statuses` must be `["disabled-since-boot"]` (`["enabled"]` admits a TEE whose memory the host can inspect, and the check would run and pass), and `max_token_age` must be positive and no longer than an hour (the token's own `exp` is always enforced, so a larger value narrows nothing). The pre-flight also refuses a config whose `chain_id` is not the deployment's `CHAIN_ID`. A simulated local-Docker run against a testnet is a different, explicitly-selected profile (`TEE_PROFILE=testnet-sim`) — see the README.

Activate it:

```bash
bash ./scripts/use-chain.sh <chain>
```

Copies `.env.<chain>` → `.env`, which all scripts auto-load.

## 4. Register the extension on-chain

```bash
bash ./scripts/pre-build.sh
```

Compiles Solidity, deploys `InstructionSender`, registers the extension on-chain. Writes `EXTENSION_ID` and `INSTRUCTION_SENDER` to `config/extension.env`.

Read the new values — `EXTENSION_ID` is part of the hand-off in Step 6:

```bash
cat config/extension.env
```

## 5. Build the Docker image

The image built is selected by `LANGUAGE` in `.env` — this repo ships `go/Dockerfile` only (`LANGUAGE=go`; additional languages resolve by directory convention if you add them, see [extension-contract.md §8](extension-contract.md)).

### Attestation mode

`MODE` selects the attestation backend:

| Value | Meaning | Used for |
|---|---|---|
| `1` | Simulated attestation (test code hash) | local devnet and the `testnet-sim` profile |
| `0` | Production attestation | a real Confidential Space VM |

**FTDC rejects simulated attestation**, so a production deploy must run with `MODE=0` — and `go/Dockerfile` bakes exactly that: `ENV MODE=0`, with `MODE` **deliberately absent** from the `tee.launch_policy.allow_env_override` label, so a Confidential Space VM can never be flipped to simulated attestation at workload launch. Local development still works because `docker-compose.yaml` supplies `MODE=${MODE:-1}` — compose env is not governed by the launch-policy label. A bare `docker run` against a devnet therefore needs an explicit `-e MODE=1`. The dev-only `go/Dockerfile.siblings` bakes `MODE=1` instead, but it too omits `MODE` from the override label — the value is fixed by whichever image you built, never by whoever launches it.

Verify what actually ended up in the image before registering its hash on-chain — see the check at the end of this section.

Then build:

```powershell
$env:SOURCE_DATE_EPOCH = (git log -1 --format=%ct)
docker compose -f docker-compose.yaml build --no-cache extension-tee
docker tag <your-extension>-extension-tee:latest <your-extension>:v0.1.0
docker save <your-extension>:v0.1.0 -o <your-extension>-v0.1.0.tar
```

Compose resolves the Dockerfile from `EXTENSION_DOCKERFILE`, which `start-services.sh` derives from `LANGUAGE`; building compose directly (as above) uses the `go/Dockerfile` default.

Setting `SOURCE_DATE_EPOCH` pins the timestamps the build clamps to, but compose is **not** the reproducible recipe: it does not use BuildKit's `rewrite-timestamp=true` exporter, so the image above is not guaranteed to rebuild to the same `codeHash`. That is fine for local and testnet runs. The image you hand off in Step 6 and allow-list in Step 8 must instead be built with the `docker buildx build` recipe in [REPRODUCIBILITY.md](../REPRODUCIBILITY.md#verifying-a-remote-image), which anyone verifying your deployment will re-run.

Check which mode is baked into the image:

```powershell
docker inspect <your-extension>:v0.1.0 --format '{{range .Config.Env}}{{println .}}{{end}}' | Select-String MODE
```

Expect `MODE=0`. Also confirm the launch-policy label does **not** list `MODE` (so it cannot be overridden at attestation time):

```powershell
docker inspect <your-extension>:v0.1.0 --format '{{index .Config.Labels "tee.launch_policy.allow_env_override"}}'
# MODE must NOT appear in this list
```

## 6. Deploy the image on a Confidential Space VM

Hand off (or deploy yourself) to a GCP Confidential Space VM with:

- The image (tar or registry URL+tag)
- Workload-launch env: `INITIAL_OWNER`, `CHAIN_URL`, `EXTENSION_ID` (from Step 4), `PROXY_URL` (proxy URL reachable from the TEE)
- Public HTTPS routed to port `6664` of the proxy container

You receive back the **public proxy URL**. Add it to `.env.<chain>` and re-activate:

```bash
# in .env.<chain>
EXT_PROXY_URL=<public proxy URL>
```

```bash
bash ./scripts/use-chain.sh <chain>
```

## 7. Verify the proxy `/info`

```powershell
curl -s $env:EXT_PROXY_URL/info | jq '.machineData'
```

Required values:

| Field          | Expected                                                          |
| -------------- | ----------------------------------------------------------------- |
| `platform`     | starts with `0x4743505f414d445f534556…` (GCP_AMD_SEV)             |
| `codeHash`     | real measured hash (**not** `0x194844cf…` — that's simulated)     |
| `extensionId`  | matches your `config/extension.env` `EXTENSION_ID`                |
| `initialOwner` | matches your `INITIAL_OWNER`                                      |

If `extensionId` is wrong, ask the VM operator to restart the container with the correct `EXTENSION_ID` env override (no image rebuild needed — it's a launch-policy override).

## 8. Register the TEE machine

> [!NOTE]
> `scripts/post-build.sh` already passes `-command rRap` (the tool's own default is `rap`). Override with `REGISTER_TEE_COMMAND` if you need to run individual steps.
>
> Step `a` (availability check) needs a one-time **challenge** — a random number from the contract that the TEE signs to prove it's alive. Lowercase `r` only issues one while pre-registering, and it skips itself once the TEE is registered on-chain, so re-runs (image changes, diamond cuts, retries) revert with `Verification.ChallengeExpired`. Capital `R` issues the challenge directly — decoupled from `r` — so re-runs work.

Run:

```bash
EXPECTED_CODE_HASH=0x<measured-image-code-hash> \
EXPECTED_PLATFORM=0x<expected-platform> bash ./scripts/post-build.sh
```

- `allow-tee-version` whitelists the codeHash + platform pair for your
  extension. On `TEE_PROFILE=confidential-space` it **fails closed without
  either `EXPECTED_CODE_HASH` or `EXPECTED_PLATFORM`**: supply the measured
  image digest of *your* build from Step 5 and the platform value of the machine
  type you provisioned (the hex-encoded `GCP_AMD_SEV` from the Step 7 table) —
  the tool only confirms the proxy `/info` agrees, it never promotes a
  proxy-reported value on its own. Full procedure:
  [production-allowlisting.md](production-allowlisting.md).
- `register-tee -command rRap` pre-registers the TEE, requests fresh attestation, runs the FTDC availability check, promotes to production.

## 9. End-to-end test

```bash
bash ./scripts/test.sh
```

Sends test instructions through the deployed TEE and verifies the round-trip.

---

## Platform traps

Properties of FCC, not of this extension. Each is silent, presents as something
else, and has cost real redeploys.

### The TEE key is in memory only

Confidential Space has no persistent storage, so **every relaunch mints a new
`teeId`**. The previous machine stays *active* on-chain with a key nobody holds, and
`getRandomTeeIds` load-balances across active machines — so instructions are routed
to a dead node roughly half the time and silently never complete (`/action/result`
404s, callers report a poll timeout).

```bash
cd tools && go run ./cmd/query-tee -ext <extensionId> -rpc "$CHAIN_URL" \
  -reg 0x1a9C4A0f9D76c0b1D91d22E24E573a9b377618aE   # FlareTeeManager from deployed-addresses.json (the built-in default is stale); via getActiveTeeMachines
cast send <FlareTeeManager> 'pause(address)' <staleTeeId> --rpc-url "$CHAIN_URL" --private-key "$KEY"
```

The live `teeId` is `keccak256(pubkey.x ‖ pubkey.y)[12:]` from the proxy's `/info`.
There is no `unpause` — only `toProduction` with a fresh availability proof — so
never pause the live one.

### One-shot bindings must be written last

`setExtensionId()` requires the current value to be zero and has no reset. Bound to
a stale value, the contract must be redeployed. Reads keep working, so it hides
until someone sends an instruction. Corollary: never run `full-setup.sh` against a
remote TEE — it chains the post-setup script and binds early.

### The launch policy aborts the workload

Confidential Space rejects any env var outside the image's
`tee.launch_policy.allow_env_override` label and exits `exit_code=4` before the
workload starts. Diff the launcher's `Image Labels` against its `Envs:`. The label
is baked in, so a fix means a new image → new code hash → re-register.

### Deploy by digest, not tag

Attestation pins the code hash registered on-chain, so a rebuild invalidates it.
Mirror between registries instead of rebuilding:

```bash
crane copy <src>@sha256:<digest> <dst>@sha256:<digest>
```

### `SIMULATED_TEE=false` on real hardware

And `CHAIN_ID` must be set — unset leaves `chainID=0` and every signature comes back
empty (`signature must be 65 bytes, got 0`).

---

## When the extension image changes

1. Rebuild and hand off the new image.
2. The VM is re-deployed → `codeHash` changes.
3. `bash ./scripts/post-build.sh` whitelists the new codeHash.
4. `bash ./scripts/test.sh`.

## When the `FlareTeeManager` diamond is re-deployed

All extension registrations on that chain are wiped:

1. `bash ./scripts/pre-build.sh` — mints a fresh `EXTENSION_ID`.
2. Send the new `EXTENSION_ID` to the VM operator. They restart the container with `EXTENSION_ID=<new value>` as a launch-policy env override — no image rebuild needed.
3. Re-curl `/info` and confirm `extensionId` matches.
4. `bash ./scripts/post-build.sh`.
5. `bash ./scripts/test.sh`.
