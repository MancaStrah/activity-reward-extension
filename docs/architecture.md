# Architecture

A Flare Confidential Compute extension with **one** operation: `STRAVA` /
`DISTANCE`. A caller seals a Strava access token to the TEE, the TEE reads the
caller's distance for the current calendar month and signs a proof of it, and the
contract pays 1 native token when the proof clears the eligibility guards.

The user-facing description of the reward rules (what counts toward the 2 km, how
to get a Strava token, the full env var table) lives in the
[repo README](../README.md) — this document is about the moving parts and how
they fit together.

## Components

| Piece | Where | Role |
|---|---|---|
| `StravaInstructionSender` | `contracts/InstructionSender.sol` | on-chain entry point, reward pool, and proof verifier |
| Extension handler | `go/internal/extension/` | decodes the instruction, fetches Strava, returns a signed proof |
| tee-node | pinned dep (`go/go.mod`) | runs inside the TEE; decrypts grants and signs results |
| tee-proxy | `proxy/Dockerfile` | bridges TEE ↔ chain/indexer; serves `/info` and `/action/result/<id>` |
| redis | compose service | the proxy's queue |
| types-server | `go/cmd/types-server` | decodes raw instruction/result payloads to JSON (port 8100) |
| Deployment tooling | `tools/cmd/*` | deploy, register, allow versions, claim, query state |

On-chain the extension is identified by an **extension id**, assigned by the
`FlareTeeManager` diamond when `register-extension` runs and latched into the
contract by `setExtensionId(expectedId)` — owner-only and one-shot, so only a
redeploy can change it.

The Go image is single-process: `go/cmd/docker/main.go` embeds tee-node as a
library and runs it alongside the extension's own HTTP server. See
[extension-contract.md](extension-contract.md) §1 for the alternative topology.

## Request flow

```
caller → encrypt-token           seals the Strava token into a grant, ECIES-encrypted
                                 to the TEE public key read from the proxy /info
       → StravaInstructionSender.getDistanceProof(teeId, encryptedToken)   (payable)
       → tee-proxy picks up the instruction
       → tee-node (inside the TEE) → processDistanceProof
       → Strava API → signed DistanceProof
       → proxy /action/result/<instructionId>
       → caller → StravaInstructionSender.claimReward(instructionId, proof)
```

`getDistanceProof` requires the requested TEE to be in `PRODUCTION` status **and**
registered to this extension, and records one pending request per `msg.sender`;
`claimReward` must come from that same address.

The proof is signed **whether or not the distance is eligible** — it carries
`eligible` and `distanceX1000`, so reading your distance just means fetching the
result and never claiming (`./scripts/claim-reward.sh --no-claim`). A genuine but
ineligible proof is consumed by `claimReward` and emits `RewardRefused`.

The node signs every response against `CHAIN_ID`; a mismatch with the proxy's
`chain_id` or the on-chain registry fails verification, and an unset `CHAIN_ID`
leaves `chainID=0` and produces empty signatures.

## The one operation

Instructions carry an op type and an op command, each hashed to `bytes32`. The
Solidity side and the handler must agree on the strings.

| Op | Handler | Signed result |
|---|---|---|
| `STRAVA` / `DISTANCE` | `processDistanceProof` | `eligible`, `distanceX1000`, `monthStart`, `athleteHash`, plus the binding fields |

Defined in `go/internal/config/config.go` (`OPTypeStrava`, `OPCommandDistance`)
and in the contract as `OP_TYPE_STRAVA` / `OP_COMMAND_DISTANCE`. An unmatched op
is rejected with 501 and the expected hash logged — the usual cause is editing
one side only.

The JSON result additionally carries `distanceKm` and a human-readable `message`.
Neither is part of the signed payload: only the rounded `distanceX1000` is.

## What the TEE re-checks

The ciphertext is never trusted on its own. The grant is bound to six things —
grant domain (`STRAVA_TOKEN_GRANT_V2`), purpose (`STRAVA_DISTANCE`), caller
wallet, contract address, chain id, expiry — and every binding is re-checked
against the on-chain instruction data before the token is used. Expiries beyond
`MaxGrantTTL` (24 h) are rejected; the tools default to 15 minutes.

The Strava response is not trusted on its own either. What the proof asserts is a
distance **for one specific UTC month**, so the enclave establishes that itself
rather than delegating it to the API:

| Property | How it is established |
|---|---|
| The activity falls in the attested month | each activity's own `start_date` must lie in `[monthStart, now)`. `after`/`before` are still sent to keep the response small, but Strava documents them only as filtering "activities that have taken place before / after a certain time" — no field, no timezone — so they are an optimisation, not the boundary |
| Nothing in the month is missing from the listing | the query is deliberately **wider** than the attested window, by `StravaQuerySlack` (one day) at each end. The same unspecified semantics that stop `after`/`before` from being the boundary also mean a query sent at the exact bounds could *exclude* activity that belongs to the month — the opening hours for an athlete behind UTC, the closing hours for one ahead of it. The enclave asks for more than it needs and discards the surplus itself, because an activity the listing never returned is one its window check can never put back |
| Every activity in the month is counted | paging stops on an **empty** page. A short page proves nothing: `per_page` is documented only as "Defaults to 30", with no maximum and no guarantee about the returned count, so a server-side cap would make page 1 short while the month continued |
| No activity is counted twice | deduplication by Strava activity id. The listing is paginated and is not a snapshot, so an upload or edit between two page requests can return the same activity on both |
| The total is complete or refused | if the listing has not ended when either budget runs out — `StravaMaxPages` pages, or the time `ActionBudget` leaves for another page — or a qualifying activity has no id or no `start_date`, the handler refuses rather than sign a total it cannot vouch for |

**How many activities that actually supports.** The ceiling is not
`StravaMaxPages × StravaPerPage`, because asking for `StravaPerPage` items is not a
promise of receiving them. It is `StravaMaxPages` × *the largest page Strava actually
returns*, and in practice the clock is the tighter of the two bounds: the pages are
fetched one after another inside `ActionBudget` (1 800 ms), against tee-node's 2 s
action timeout. So a month that needs many pages fails on time long before it runs out
of pages, and if Strava were to cap pages at its documented default of 30 the supported
month would be a few hundred activities rather than the 2 000 the constants suggest.

Both limits are reported when they bite, with the page size that was actually observed,
so the real ceiling is visible in the log rather than inferred from the constants. The
paging loop also refuses to *start* a page it cannot finish in time, keeping back
`StravaPageTimeReserve` for the signing work: without that, the deadline expires on the
`/sign` call after the distance was already computed, and the failure says nothing about
the listing being too long to read.

Cost to keep in mind against `ActionBudget`: the athlete lookup, the first activity
page, and the empty page that ends the listing are three sequential Strava calls in the
ordinary case.

Two encodings are duplicated by hand and must stay byte-for-byte identical:

| Encoding | Copies | Pinned by |
|---|---|---|
| Signed proof payload | Solidity `_recoverProofSigner`, Go `abiEncodeDistanceProofPayload` | paired vector: `test/SignPayloadVector.t.sol` + `TestSignPayloadCrossLanguageVector` |
| Grant layout | `go/internal/extension/grant.go`, `tools/pkg/fccutils/grant.go` | paired vector: `TestGrantWireVector` in both modules — the encoder half in `tools/pkg/fccutils`, the decoder half in `go/internal/extension` |

## State

The enclave keeps signed-proof counters in memory and exposes them on
`GET /state`; nothing is persisted. **The TEE has no durable storage**, so a
relaunch resets the counters and mints a new `teeId` — and the old machine stays
**active** on-chain until it is paused (see
[deployment-steps.md](deployment-steps.md)).

The parts that must survive a relaunch are on-chain, in the contract: the pending
request per caller (`pendingProofs`), and the monthly quotas
(`lastPaidMonth` per address, `lastPaidAthleteHash` per Strava athlete).

## Language-neutral spine

The repo root is language-agnostic. An implementation is a directory marked by a
`language.env` manifest, and `scripts/lib/language.sh` discovers them by globbing
`*/language.env` — there is no hardcoded list. This repo ships **one**
implementation, `go/`:

| Path | Contains |
|---|---|
| `go/cmd/docker` | image entry point (tee-node embedded) |
| `go/cmd/start-tee` | host-process runner for `--local` |
| `go/cmd/types-server` | standalone types-server |
| `go/internal/extension` | routing, the handler, grant parsing, Strava calls |
| `go/internal/config` | op codes, thresholds, budgets, TTL caps |
| `go/pkg/types`, `go/pkg/decoder` | wire types, ABI layouts, decoder registry |

The container contract an implementation must satisfy is normative in
[extension-contract.md](extension-contract.md); [extension-guide.md](extension-guide.md)
walks through writing a handler.

## Generated configuration

`scripts/pre-build.sh` writes `config/extension.env` on every deploy with
`EXTENSION_ID` and `INSTRUCTION_SENDER` — both public on-chain values, no
secrets. It is **not** tracked: `.gitignore` ignores `*.env` and this file is no
exception, so a fresh clone has none until `pre-build.sh` writes one. Everything
that reads it tolerates its absence — compose declares it `required: false`, and
`start-services.sh` also accepts `EXTENSION_ID` from the environment instead.

Chain selection is by `.env.<chain>` files activated with
`./scripts/use-chain.sh <chain>`; `--chain` on the lifecycle scripts does the
same copy automatically.

## Entry points

| Script | Does |
|---|---|
| `pre-build.sh` | version check, bindings, preflight, deploy `InstructionSender`, register extension, bind the id |
| `start-services.sh` | build and start node/proxy/redis (or Go host processes with `--local`); reads the ngrok URL on testnets |
| `post-build.sh` | allow TEE version, register governance, register the TEE machine |
| `test.sh` | end-to-end round-trip through a running deployment |
| `claim-reward.sh` | the caller-side flow only: proof → claim |
| `full-setup.sh` | pre-build → start-services → post-build → optional `test.sh` |
| `update-tee-url.sh` | repoint the on-chain machine URL after a tunnel URL rotates |
| `check-versions.sh` | fails the build when dependency pins drift or fall below the floor |
| `check-docs.sh` | validates this docs set against the shared standard |

## Deployment profiles

Every deployment runs under an explicit `TEE_PROFILE` — `local`, `testnet-sim` or
`confidential-space` — a coherent `MODE`/attestation matrix that
`scripts/lib/profile.sh` validates fail-fast before anything starts, because an
inconsistent matrix otherwise surfaces only as an opaque `/info` bootstrap
timeout. The proxy config it cross-checks is not read by the shell: `profile.sh`
delegates that to `tools/cmd/check-proxy-config`, which decodes the file with the
**proxy's own parser and schema** at the pinned tee-proxy version, so the
pre-flight and the running proxy cannot reach two readings of the same bytes.
`scripts/test-profile-matrix.sh` unit-tests that validator (and therefore needs a
Go toolchain, not just bash). The full
matrix is in the README's
[Deployment profiles](../README.md#deployment-profiles) section.

## Version pinning

`go/go.mod` is the single source of truth for the tee-node version;
`scripts/lib/versions.sh` derives `TEE_NODE_REF` from it, and `tools/go.mod` must
match. `check-versions.sh` enforces that agreement, the tee-node minimum
(v0.0.22), the tee-proxy pin against `proxy/Dockerfile`, and that `TEE_VERSION`
agrees across the env templates and `post-build.sh`. It runs as a hard gate
inside `pre-build.sh`.

What the image build guarantees for the registered `codeHash` is in
[../REPRODUCIBILITY.md](../REPRODUCIBILITY.md).

## Where to look next

[getting-started.md](getting-started.md) to run it locally ·
[extension-guide.md](extension-guide.md) to change the handler ·
[deployment-steps.md](deployment-steps.md) for Coston2 and Confidential Space
