# Extension Development Guide

This guide explains how the extension is structured and where to put your own
logic. The repo is organised around a single operation: a TEE-signed proof of
the caller's monthly Strava distance.

> **This repo ships the Go implementation only** (`go/`, selected by
> `LANGUAGE=go` in `.env`). The language-independent specification of the HTTP
> surface, wire format and container requirements — what you would implement to
> add another language — is [extension-contract.md](extension-contract.md).

## How an Extension Works

An extension is an HTTP server that runs inside a Trusted Execution Environment
(TEE). It receives instructions from the blockchain, processes them, and
returns results. The full lifecycle:

```
1. User calls your Solidity contract (on-chain)
2. Contract emits a TeeInstructionsSent event via TeeExtensionRegistry
3. TEE proxy picks up the instruction from the chain
4. TEE node fetches the instruction from the proxy
5. TEE node forwards it as POST /action to your extension server
6. Your extension processes the action and returns a result
7. TEE node sends the result back to the proxy
8. Caller polls the proxy for the result
```

Your extension controls steps 1 (the contract) and 6 (the action handler).
Everything else is handled by the TEE infrastructure.

## Architecture Overview

```
┌─────────────────────────────────────────────────────┐
│  YOUR CODE (what you customize)                     │
│                                                     │
│  contracts/InstructionSender.sol   On-chain entry   │
│  go/internal/config/config.go      OPType constants │
│  go/internal/extension/            Action handlers  │
│  go/pkg/types/types.go             Wire types       │
│  tools/cmd/run-test/main.go        E2E tests        │
└─────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────┐
│  INFRASTRUCTURE (do not modify)                     │
│                                                     │
│  go/internal/extension/utils.go    actionHandler,   │
│  go/pkg/server/                    buildResult,     │
│                                    server plumbing  │
│  scripts/*                         Build/deploy     │
│  tools/cmd/deploy-contract/        Deployment       │
│  tools/cmd/register-*/             Registration     │
└─────────────────────────────────────────────────────┘
```

## The Files You Modify

### 1. `go/internal/config/config.go` — Operation Constants and Limits

String constants for the operation types, hashed to `bytes32` at runtime with
`teeutils.ToHash()` and compared against the `OPType`/`OPCommand` of incoming
actions. This extension defines exactly one operation:

```go
const (
    OPTypeStrava      = "STRAVA"
    OPCommandDistance = "DISTANCE"
)
```

These strings must exactly match the `bytes32` constants in the Solidity
contract:

```solidity
bytes32 public constant OP_TYPE_STRAVA      = bytes32("STRAVA");
bytes32 public constant OP_COMMAND_DISTANCE = bytes32("DISTANCE");
```

The same file holds the extension's security-relevant limits
(`RewardThresholdKm`, `ActionBudget`, `MaxGrantTTL`, `MaxRequestBytes`,
`StravaMaxPages`, …) and the `Version` string that is hashed into the
`stateVersion` the availability check sees — bump it whenever the observable
contract changes.

### 2. `go/pkg/types/types.go` — Request and Response Types

- `DistanceMessageArgs` — the **flat ABI layout** of the on-chain message. The
  contract sends `abi.encode(challenge, caller, contract, chainId,
  encryptedToken)` (flat arguments, not a struct), so the handler decodes with
  `DistanceMessageArgs.Unpack()`. Change one side, change the other.
- `DistanceProof` / `DistanceResponse` — the JSON the extension returns:
  the signed proof fields (`timestamp`, `challenge`, `caller`, `teeId`,
  `eligible`, `distanceX1000`, `monthStart`, `athleteHash`, `signature`) plus
  informational extras (`distanceKm`, `message`).
- `State` / `StateResponse` — the observable state returned by `GET /state`
  (signed-proof counters only, deliberately minimal).

This file also registers the types-server decoders (`register.go`), so other
apps can decode raw instruction/result hex to JSON.

### 3. `go/internal/extension/` — Action Handlers

The main customization point:

- **`extension.go`** — the `Extension` struct (in-memory state, guarded by
  `e.mu`; the framework does **not** serialize handler calls), the router, and
  the handler. Routing is two-level:

```go
func (e *Extension) processAction(ctx context.Context, action teetypes.Action) (int, []byte) {
    ...
    switch {
    case dataFixed.OPType == teeutils.ToHash(config.OPTypeStrava):
        return e.processStrava(ctx, action, dataFixed)
    default:
        // falls through to HTTP 501 "unsupported op type"
    }
}

// processStrava routes STRAVA instructions by OPCommand.
func (e *Extension) processStrava(...) {
    switch {
    case df.OPCommand == teeutils.ToHash(config.OPCommandDistance):
        ar := e.processDistanceProof(ctx, action, df)
        ...
    }
}
```

- **`processDistanceProof`** — the single handler, following the 4-step
  pattern every handler uses:
  1. **Decode** — `types.DistanceMessageArgs.Unpack(df.OriginalMessage)`;
  2. **Validate** — decrypt the ECIES grant through the TEE sign port and
     re-check *every* binding (domain tag, purpose, caller, contract, chain id,
     expiry) against the on-chain instruction data — never trust the ciphertext
     alone (`grant.go`);
  3. **Execute** — fetch the month's activities from Strava and sum the
     eligible distance (`helpers.go`). Every filter is applied in the enclave,
     not delegated to the query: sport type, manual/flagged exclusion, the
     attested window checked against each activity's own `start_date`,
     deduplication by activity id, and paging that stops only on an empty page;
  4. **Respond** — sign the 12-field proof payload
     (`abiEncodeDistanceProofPayload` — byte-identical to the contract's
     `_recoverProofSigner`, pinned by the cross-language vector test) and return
     it via `buildResult`.

- **`utils.go`** (infrastructure) — `actionHandler` and `buildResult`. Always
  return through `buildResult`: it owns the `ActionResult` envelope
  (`status = 0` → error, the `err` is logged; `status = 1` → success, `data`
  reaches the caller).

- **`stateHandler`** — maps `Extension` fields into `types.State` for
  `GET /state`. Extend the mapping when you add state fields.

### 4. `contracts/InstructionSender.sol` — On-Chain Entry Point

The only address allowed to submit instructions for this extension. After
modifying it, run `./scripts/generate-bindings.sh` to regenerate the Go
bindings. See the **[InstructionSender Contract Guide](instruction-sender.md)**.

### 5. `tools/cmd/run-test/main.go` — E2E Tests

Sends instructions through the full pipeline (contract → TEE → proxy) and
verifies results, including on-chain claiming. See the
**[Testing Guide](testing.md)**.

## How the Pieces Connect

The critical link between the Solidity contract and the Go code is the
**OPType + OPCommand pair**, identical in three places:

| What | Solidity | Go config | Go router |
|------|----------|-----------|-----------|
| Operation type | `OP_TYPE_STRAVA = bytes32("STRAVA")` | `OPTypeStrava = "STRAVA"` | `dataFixed.OPType == teeutils.ToHash(config.OPTypeStrava)` |
| Command | `OP_COMMAND_DISTANCE = bytes32("DISTANCE")` | `OPCommandDistance = "DISTANCE"` | `df.OPCommand == teeutils.ToHash(config.OPCommandDistance)` |

A mismatch is not a compile error — the action falls through to the router's
`default` and returns HTTP 501 "unsupported op type/command" at runtime (the Go
handler helpfully prints both the received and the expected hash).

The second, equally critical coupling is the **signed payload encoding**:
`_recoverProofSigner` (Solidity) and `abiEncodeDistanceProofPayload` (Go TEE)
must agree byte-for-byte. The paired vector tests —
`test/SignPayloadVector.t.sol` and `TestSignPayloadCrossLanguageVector` in
`extension_test.go` — pin this; run `forge test` and `go test ./...` after
touching either side. The **grant** layout is likewise duplicated between
`go/internal/extension/grant.go` and `tools/pkg/fccutils/grant.go` and must be
kept identical by hand.

## Data Flow Through the Extension

```
StravaInstructionSender.getDistanceProof(teeId, encryptedToken)
    │
    │  message = abi.encode(challenge, msg.sender, address(this), block.chainid, encryptedToken)
    ▼
TeeExtensionRegistry.sendInstructions()
    │
    │  wraps into DataFixed{OPType, OPCommand, OriginalMessage}
    ▼
TEE node → POST /action → actionHandler()
    │
    ▼
processAction()  ──OPType==STRAVA──▶ processStrava()
    │                                     │
    │                                     └─OPCommand==DISTANCE─▶ processDistanceProof()
    │                                            decode flat ABI (DistanceMessageArgs)
    │                                            decrypt + verify grant (sign port)
    │                                            fetch Strava month, sum eligible km
    │                                            sign the 12-field proof payload
    └─ default ──▶ 501 "unsupported"              return DistanceResponse
    ▼
buildResult() → JSON → TEE node → proxy → caller → claimReward() on-chain
```

Key types in the flow:
- `teetypes.Action` — the envelope from the TEE node (contains `Data.Message`, `Data.ID`, …)
- `instruction.DataFixed` — parsed from `Action.Data.Message` (contains `OPType`, `OPCommand`, `OriginalMessage`)
- `df.OriginalMessage` — the raw message bytes from the Solidity contract (flat-ABI here)
- `teetypes.ActionResult` — what you return (contains `Status`, `Data`, `Log`)

## Using the TEE Signing Port

The extension calls tee-node's crypto API on `localhost:{SIGN_PORT}` (default
9090 standalone, 7701 in Docker) to decrypt the ECIES token grant and to sign
the distance proof. It is never exposed outside the container.

**The sign port speaks base64, not hex.** tee-node is Go, and Go marshals
`[]byte` as base64 in JSON — so `/decrypt` takes
`{"encryptedMessage": "<base64>"}` and returns
`{"decryptedMessage": "<base64>"}`, unlike the hex used everywhere else on the
wire. `helpers.go` wraps this; prefer those helpers over hand-rolling the call.
See [extension-contract.md §3](extension-contract.md).

## Step-by-Step: Adding a New Operation

1. **Add constants** in `go/internal/config/config.go` — one `OPType` for the
   operation group, one `OPCommand` per command
2. **Define request/response shapes** in `go/pkg/types/types.go` — including
   the ABI layout if the contract sends ABI rather than JSON — plus any new
   state fields
3. **Route it** — add a `case` in `processAction()` (new op type) or in the
   op-type's sub-router (new command)
4. **Write the handler** following the 4-step pattern: decode → validate →
   execute → respond. For flat-ABI messages use `abi.Arguments.Unpack`; for
   JSON use `json.Decoder` with `DisallowUnknownFields`
5. **Expose new state** — add `Extension` fields (locked by `e.mu`) and map
   them in `stateHandler()`
6. **Add the Solidity constants and send function** in
   `contracts/InstructionSender.sol`
7. **Regenerate bindings**: `./scripts/generate-bindings.sh`
8. **Register a types-server decoder** for the new `(opType, opCommand)` pair
   in `go/pkg/types/register.go` so `POST /decode` can decode it
9. **Add a test case** in `tools/cmd/run-test/main.go` and, for contract
   changes, a Foundry test in `test/`

## Common Patterns

### Returning errors to the caller

Use `status = 0` in `buildResult`. The error message goes into
`ActionResult.Log`:

```go
if err := grant.Verify(...); err != nil {
    return buildResult(action, df, nil, 0, fmt.Errorf("verifying grant: %w", err))
}
```

Note the extension **signs a proof whether or not the user is eligible** — an
ineligible-but-genuine result is a success (`status = 1`) whose proof carries
`eligible = false`; the contract then emits `RewardRefused`. Reserve
`status = 0` for actual failures (bad grant, Strava error, over-budget).

### Maintaining state across actions

Add fields to the `Extension` struct and protect them with the mutex — the
framework may run handlers concurrently:

```go
e.mu.Lock()
e.proofsSigned++
e.mu.Unlock()
```

Return state in `stateHandler()` via the `types.State` struct.

### Fail closed on anything unbounded

Every external interaction here has a ceiling: request size
(`MaxRequestBytes`), response size (`MaxResponseBytes`), activity paging
(`StravaMaxPages` — *refuse to sign* rather than under-count past it), grant
lifetime (`MaxGrantTTL`), total handler time (`ActionBudget`). Follow that
pattern for new operations: an unbounded input is a signing oracle waiting to
be fed garbage.
