# Testing

Tests split into three groups: the **extension** (Go + Solidity), the
**deployment tooling**, and the **configuration/scripts**. Roughly cheapest
first — the types-server row needs that service running, and the last two rows
need a live chain.

| Layer | What it tests | How to run | Needs |
|-------|--------------|------------|-------|
| Go unit (32 tests) | Handlers, grant parsing, ABI round-trips, month math, the attested-window, query-superset and pagination guarantees, both fetch budgets, the cross-language sign-payload vector, the cross-module grant wire vector | `./scripts/test-unit.sh` (or `cd go && go test ./...`) | nothing |
| Solidity (33 tests) | 26-test claim-flow behavioral matrix, 6 `setExtensionId` tests, sign-payload vector | `forge test` | Foundry |
| Profile matrix | The fail-fast MODE/attestation validation, including the full `confidential-space` posture, the `[attestation]`-scoping, <placeholder> and valid-TOML-bypass regressions, whether the whole file loads with the proxy's own loader, which proxy config each deployment mode resolves and checks, that the compose bind-mount is the file the scripts validated, and the chain_id cross-check (98 cases) | `./scripts/test-profile-matrix.sh` | a Go toolchain — the posture pre-flight it drives is `tools/cmd/check-proxy-config`, built on first use |
| Types-server contract | 8 decode cases + every error status | `./scripts/test-types-server.sh` | a running types-server |
| Tooling unit | Revert decoding, state I/O, validation, the allowlist gate, strict hex/address parsing, terminal-output sanitization, `get-result` proof validation, the signed-vs-displayed distance agreement, governance parsing, the launch-policy label cross-check, the `[attestation]` posture pre-flight, proxy-config resolution, the extension-id scans | `cd tools && go test ./...` | nothing |
| Tooling integration | On-chain constructor/registration/setExtensionId/CheckTx behavior | `cd tools && go test -tags integration ./integration/ -v -count=1` | live chain + funded key |
| End-to-end | Full lifecycle: deploy → encrypt → instruction → proof → on-chain claim | `STRAVA_TOKEN=… ./scripts/test.sh` | full deployed stack |

Every chain-free layer except the types-server contract runs in CI
(`.github/workflows/ci.yml`), alongside
`go vet`, `govulncheck`, `forge fmt --check`, the version-pin/launch-label
consistency check and gitleaks secret scanning (the CLI pinned by digest, over the
whole history — `--log-opts=--all`, the command `.gitleaks.toml` documents for
local runs).

## Solidity tests

Three suites under `test/`, run with `forge test` (solc pinned to 0.8.27 in
`foundry.toml`):

- **`SignPayloadVector.t.sol`** — the Solidity half of the cross-language
  vector: the payload hash in `_recoverProofSigner` must equal the one Go
  computes in `abiEncodeDistanceProofPayload`. The Go half is
  `TestSignPayloadCrossLanguageVector`. If these diverge, every on-chain claim
  fails — run both after touching the payload encoding.
- **`SetExtensionId.t.sol`** — 6 tests covering the binding rules (owner-only,
  single-use, public-id floor, registry cross-check) and the pre-registration
  capture scenario they exist to defeat.
- **`ClaimReward.t.sol`** — the behavioral matrix for the claim flow:
  successful payout, next-month reset, replay, wrong signer, proof from a
  different production TEE, demoted signer, signer from another extension,
  foreign/unregistered TEE at request time, stale proof, future proof, wrong
  month, below-threshold, ineligible-but-genuine proof (refused and consumed),
  address and athlete monthly quotas, empty pool, owner withdrawal with a
  pending claim (documents a deliberate design choice), non-owner withdrawal,
  reentrancy (exactly one payout), `verifyDistanceProof*` tamper/expiry/caller/
  forged-challenge/wrong-TEE
  checks, and cancel. It uses the raw Foundry cheatcode interface (`vm.sign`,
  `vm.warp`) — the repo deliberately ships no forge-std.

## The deployment tooling

`tools/` is a standalone Go module with no dependency on the extension
implementation. It has its own tests.

### Tooling unit tests

No external services required:

- **Revert reason decoding** (`fccutils/revert_test.go`) — ABI-encoded
  `Error(string)` reverts, all revert messages from `InstructionSender.sol`,
  edge cases.
- **Support revert decoding** (`support/support_test.go`) — extracting revert
  reasons from go-ethereum JSON-RPC error types.
- **Registration state I/O** (`fccutils/registration_test.go`) — the
  register-tee resume flow.
- **Allowlist gate** (`fccutils/allowlist_gate_test.go`) — the allowlist
  gate's negative matrix: `allow-tee-version` must refuse to authorize a
  proxy-reported codeHash outside the simulated profiles unless the operator
  supplies `-expected-codehash`.
- **Strict hex parsing** (`fccutils/hexstrict_test.go`) — `StrictHash` /
  `StrictAddress` / `StrictBytes` must reject what the lenient go-ethereum
  helpers silently zero-pad or truncate into a plausible-looking value
  (wrong length, non-hex, whitespace, escape sequences), and must not echo the
  rejected value back.
- **Terminal-output sanitization** (`fccutils/sanitize_test.go`) — proxy-supplied
  free text is escaped before printing: no live control bytes, no CR/LF-forged
  output lines, no bidi reordering, and a bounded length.
- **`get-result` proof validation** (`cmd/get-result/main_test.go`) — every
  field of a proxy-returned proof must parse exactly or the proof is refused;
  displayed values are re-encodes of the parsed types, and a non-proof payload
  is not treated as one.
- **Proxy config resolution** (`cmd/start-proxy/main_test.go`) — the host-mode
  proxy config must be the one `start-services.sh` validated: `PROXY_CONFIG` wins
  outright, and the standalone fallback is chain-aware rather than always loading
  the local-devnet file. The filename mapping is duplicated in
  `scripts/lib/profile.sh`, so a paired cross-language case asserts the two agree —
  if they drift, the file the script checks is not the file the binary opens.
- **`[attestation]` posture pre-flight** (`cmd/check-proxy-config/main_test.go`) — the
  binary `profile.sh` delegates the config reading to. The base `confidential-space`
  posture is accepted; every valid-TOML bypass that satisfied the old shell reader
  (`audience = ''`, a comment-only list, `max_token_age = "0h0m0s"`) is refused *and
  named*; the forms that reader refused without cause (dotted `attestation.enable`, a
  multi-line string elsewhere in the file) are accepted; each profile asks its own
  question; duplicate tables/keys come back as parser errors with a line number; `-full`
  (the proxy's whole loader) is kept a separate question from the posture; one run
  reports every problem in the file rather than the first; a `dbgstat` of `enabled`
  (valid, and the one pin that does not fail closed when wrong) and an unknown
  `dbgstat` are both refused and named; a `max_token_age` above the token's own
  lifetime is refused as a control that tightens nothing; and a `chain_id` that is not
  the deployment's `CHAIN_ID` is named rather than left to surface as signatures that
  do not verify.
- **`claim-reward` proof conversion** (`cmd/claim-reward/main_test.go`) — every field of a
  proxy-returned proof must parse exactly, injection attempts are refused, and what is
  displayed is the **signed** `distanceX1000`: an unsigned `distanceKm` that contradicts
  it does not reach the terminal.
- **Distance agreement** (`fccutils/distance_test.go`) — `CheckDistanceAgreement` accepts
  faithful pairs, rejects contradictions, and rejects NaN rather than comparing it.
- **Transport and status predicates** (`fccutils/verify_test.go`, `fccutils/common_test.go`)
  — `RequireSecureProxyURL`, `IsProductionStatus`, and reading codeHash/platform from a
  node that reports the `magic_pass` sentinel instead of a JWT.
- **Governance parsing** (`cmd/set-governance/main_test.go`, `fccutils/governance_test.go`)
  — thresholds and signer sets: unset defaults to 1, zero is refused as meaningless, a
  malformed `INITIAL_OWNER` is fatal rather than ignored, duplicate/unusable signer sets
  are rejected, the errors never echo the rejected value, and the whole set is validated
  before any chain call.
- **Launch-policy label** (`fccutils/policy_consistency_test.go`) — the
  `tee.launch_policy.allow_env_override` list in `go/Dockerfile` and the one documented in
  `extension-contract.md` must stay in sync; `check-versions.sh` gates the same pair.
- **Extension-id scans** (`utils/instructions_test.go`, `validate/checks_test.go`) — both
  places that look for a duplicate instruction-sender registration must distinguish an id
  they could not READ from an id that did not MATCH. A per-id RPC error, or a scan that
  runs out of budget, has to be reported rather than absorbed: absorbing it turns
  "ambiguous, refuse" into "unique, proceed", and the caller feeds that id into a
  one-shot `setExtensionId`. Both take a small registry interface purely so these failure
  paths are testable without a chain.
- **Validation checks / report formatting / primitives** (`validate/*_test.go`) — including
  that the report writer escapes check text it did not write. Messages are built from
  `extension.env` fields, on-chain strings and remote error text, and the report is read to
  decide whether a deployment is sound, so text that can forge a line can forge a verdict.

```bash
cd tools && go test ./... -v
```

### Tooling integration tests

Run against a live node (Hardhat, Anvil, or Coston2); excluded from
`go test ./...` via the `integration` build tag.

- **Constructor validation** — zero/EOA/valid registry addresses, decoded
  revert messages.
- **setExtensionId errors** — before registration and double-set; exercises
  the full revert-decoding chain.
- **CheckTx revert reasons** — on-chain reverts replayed into human-readable
  reasons.
- **Duplicate registration semantics** — the registry *allows* registering the
  same InstructionSender under multiple extension ids (that permissiveness is
  what enables a pre-registration attack), so the test asserts the
  guard lives in the tools: `ResolveExtensionId` must refuse to guess when the
  address is ambiguous.
- **Pre-flight validation** — `AddressHasCode` / `KeyHasFunds` against real
  contracts and EOAs.

```bash
cd tools && go test -tags integration ./integration/ -v -count=1
# against Coston2:
cd tools && CHAIN_URL=https://coston2-api.flare.network/ext/C/rpc \
  DEPLOYMENT_PRIVATE_KEY=<your-funded-key> \
  go test -tags integration ./integration/ -v -count=1
```

| Variable | Default | Description |
|----------|---------|-------------|
| `CHAIN_URL` | `http://127.0.0.1:8545` | RPC endpoint |
| `ADDRESSES_FILE` | `../../config/coston2/deployed-addresses.json` | Deployed registry addresses |
| `DEPLOYMENT_PRIVATE_KEY` | Hardhat dev key (only honored with `LOCAL_MODE=true`) | Funded key |

Integration tests deploy fresh contracts on each run — free locally, costs gas
on Coston2.

## End-to-end test

After post-build completes:

```bash
STRAVA_TOKEN=<token with activity:read_all> ./scripts/test.sh
```

or everything in one shot:

```bash
STRAVA_TOKEN=… ./scripts/full-setup.sh --chain coston2 --tunnel --test
```

`test.sh` reads everything from the environment and drives
`tools/cmd/run-test`, which executes the real caller lifecycle:

```
1. Verify the extension id against the registry (explicit -extensionID, or a
   scan that refuses to guess on ambiguity) and setExtensionId if unbound
2. Enforce secure transports (HTTPS/loopback proxy + RPC) before the token is used
3. Cross-check Go vs Solidity month-start agreement
4. Verify the /info key belongs to a registered PRODUCTION TEE of THIS
   extension, then ECIES-encrypt the Strava token grant to it
5. Fund the contract and send the DISTANCE instruction (getDistanceProof)
6. Poll the proxy for the TEE-signed result
7. Assert verifyDistanceProof() accepts the genuine proof and rejects a
   tampered one — while the proof is still unconsumed
8. Submit claimReward() on-chain — expect RewardClaimed or RewardRefused
9. Assert verifyDistanceProof() now returns false: claimReward consumed the
   proof, so the contract stops vouching for it
```

Steps 1–3 and 6 are generic; 4, 5, 7, 8 and 9 are this extension's specifics — if
you add an operation, extend `run-test` with a send + verify pair for it. The
tool asserts on the wire format (the proxy's JSON envelope: `status` 0/1/2,
`log`, `data`), so it needs no import of the extension implementation.
