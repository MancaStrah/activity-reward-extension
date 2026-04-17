# Testing against a Coston2 deployment

For exercising an extension that is already deployed and registered on Coston2 —
either on a Confidential Space VM, or locally with a public tunnel. To deploy one
first, see [deployment-steps.md](deployment-steps.md).

## What you need

| Need | Note |
|---|---|
| `.env.coston2` | filled in (incl. `TEE_PROFILE=testnet-sim` — this flow is the simulated-TEE profile; start from `.env.example`); `use-chain.sh coston2` activates it |
| A funded Coston2 key | sends the test instructions; get C2FLR from the [faucet](https://faucet.flare.network/coston2) |
| `STRAVA_TOKEN` | an OAuth access token with `activity:read_all` (see the README's "Getting a Strava Access Token"); tokens expire after 6 h |
| `config/coston2/deployed-addresses.json` | the `FlareTeeManager` diamond and friends |
| `config/proxy/extension_proxy.coston2.docker.toml` | gitignored — copy the `.example` and fill in `[db]` |
| A reachable `EXT_PROXY_URL` | the VM's URL, or a tunnel if the proxy runs locally |

## Confirm the deployment is live

```bash
curl -s "$EXT_PROXY_URL/info" | jq '.machineData | {extensionId, codeHash, platform}'
```

| Field | Expect |
|---|---|
| `extensionId` | matches `EXTENSION_ID` in `config/extension.env` |
| `codeHash` | the hash registered on-chain — `0x194844cf…` means simulated, not real hardware |
| `platform` | the platform name hex-encoded: `0x4743505f414d445f534556000000000000000000000000000000000000000000` (`GCP_AMD_SEV`) on real hardware, `0x544553545f504c4154464f524d00000000000000000000000000000000000000` (`TEST_PLATFORM`) when simulated |

Then check the on-chain side:

```bash
cd tools
go run ./cmd/query-tee -ext <extensionId> -rpc "$CHAIN_URL" \
  -reg 0x1a9C4A0f9D76c0b1D91d22E24E573a9b377618aE   # FlareTeeManager from deployed-addresses.json; the built-in default is stale
go run ./cmd/verify-deploy -a ../config/coston2/deployed-addresses.json -c "$CHAIN_URL"
```

`query-tee` lists the TEE machines registered for the extension. **More than one
active machine is a problem** — instructions are load-balanced across them, so a
stale one swallows roughly half your requests. See
[deployment-steps.md](deployment-steps.md).

## Run the test

```bash
./scripts/use-chain.sh coston2
STRAVA_TOKEN=<token> ./scripts/test.sh
```

This drives the full `DISTANCE` lifecycle: verifies the extension id against
the registry, cross-checks Go/Solidity month-start agreement, verifies the
`/info` key belongs to a PRODUCTION TEE of this extension, ECIES-encrypts your
Strava token to it, sends the instruction, polls the proxy for the TEE-signed
proof, asserts `verifyDistanceProof()` accepts the genuine proof and rejects a
tampered one while the proof is still unconsumed, submits `claimReward()`
on-chain (a pass ends in `RewardClaimed`, or `RewardRefused` if your month's
eligible distance is under 2 km — both mean the pipeline works), and finally
asserts `verifyDistanceProof()` has stopped vouching for the consumed proof.

Two operational notes: the claim window is ~300 s after the proof is signed, so
a stalled poll can push a valid proof past its freshness; and each address and
Strava athlete can be *paid* only once per calendar month — a second eligible
run in the same month correctly fails the claim.

The extension's `/state` counters are in memory and restart at zero after any
TEE relaunch.

## Testing a local proxy against Coston2

The proxy must be publicly reachable for FTDC data providers to answer it. Start the
ngrok agent yourself, then let the scripts wire its URL in:

```bash
ngrok http 6674                                          # leave running
STRAVA_TOKEN=<token> ./scripts/full-setup.sh --chain coston2 --tunnel --test
```

That reads the agent's URL into `.env` as `EXT_PROXY_URL`, so `post-build.sh` and
`test.sh` pick it up. Details in [ngrok.md](ngrok.md).

## If something's blocked

| Symptom | Cause |
|---|---|
| `pollAction` timeout, `/action/result` 404 | multiple active TEE machines; pause the stale ones |
| `Verification.ChallengeExpired` | re-registration without `-command rRap` |
| `no round` / 404 from the FTDC proxy | proxy signing policy out of sync with the on-chain reward epoch; `register-tee` pre-flights this |
| `signature must be 65 bytes, got 0` | `CHAIN_ID` unset on the node |
| `InvalidTeePublicKeyOrSignature` | node `CHAIN_ID`, proxy `chain_id` and the registry disagree — all three must say 114 |
| Instructions never arrive | `EXT_PROXY_URL` not reachable from outside, or a rotated tunnel URL left stale in `.env` |
| `Extension ID already set.` | `setExtensionId()` is one-shot; a redeploy is the only reset |
