# Extension Container Contract

**This document is normative.** Any container that satisfies it is a valid FCC extension, regardless of implementation language. **This repo ships the Go implementation only** (`go/`), and no mechanical conformance suite — §8 lists the validation layers that do exist here.

If you are adding a new language to this repo, this is the specification you are implementing — nothing else in the repo needs to change.

---

## 1. Process topology

Two shapes are equally valid. The contract is what is observable from *outside* the container; how you get there is your choice.

**Single-process** — the extension embeds tee-node as a library and runs both in one binary. Only available to Go. This is what `go/Dockerfile` builds: `go/cmd/docker/main.go` calls `teeServer.StartServerExtension(configPort, signPort, extensionPort)` in a goroutine alongside the extension's own HTTP server. Produces the smallest, most reproducible image (distroless, static, single binary).

**Two-process** — a prebuilt tee-node `./cmd/extension` binary runs beside the extension runtime, joined so that the container dies if either child dies. This is what every non-Go language uses:

```dockerfile
CMD ["/bin/bash", "-c", "/app/server & SERVER_PID=$!; <your-runtime> & EXT_PID=$!; wait -n $SERVER_PID $EXT_PID; kill -TERM $SERVER_PID $EXT_PID 2>/dev/null; exit 1"]
```

Use `/bin/bash`, not `/bin/sh`. Debian's `/bin/sh` is dash, which does not implement `wait -n`, and without it the container will not exit when a child dies.

Neither shape is privileged: what must hold is the contract, not the topology.

---

## 2. HTTP surface the extension MUST serve

On `$EXTENSION_PORT`, bound to loopback (`127.0.0.1`) only — never on all interfaces. The node reaches the extension over localhost inside the container, and this endpoint authenticates nothing, so it must not be reachable from off-host.

### `POST /action`

Request body is an `Action` (§4.1). Response is an `ActionResult` (§4.3) as JSON with status 200 whenever the action was routed to a handler — **including when the handler fails**. Handler failure is signalled by `ActionResult.status`, not by the HTTP status.

| Condition | HTTP status | Body |
| --- | --- | --- |
| Routed to a handler (success or handler error) | 200 | `ActionResult` JSON |
| Body is not valid JSON | 400 | error text or `{"error": ...}` |
| `data.message` is not valid hex | 400 | error text |
| `data.message` does not decode to a valid `DataFixed` | 400 | error text |
| `(opType, opCommand)` matches nothing | 501 | plain text naming the level that failed — `unsupported op type` or `unsupported op command` |

The 501 body is a human-readable diagnostic, not a fixed string — the status
code is what is contractual. The Go implementation additionally names the
received and expected identifiers, which is worth imitating.

### `GET /state`

No request body. Returns a `StateResponse` (§4.4) with status 200.

### Routing rules

| Request | HTTP status |
| --- | --- |
| `GET /action` | 405 |
| `POST /state` | 405 |
| Any other path | 404 |

---

## 3. HTTP surface the extension MAY call

tee-node exposes a signing/crypto API on `http://localhost:$SIGN_PORT`. The extension calls it; it is never exposed outside the container.

### `POST /decrypt`

Decrypts a payload that was encrypted to the TEE's public key. Because tee-node is Go and Go marshals `[]byte` as base64 in JSON, **the wire encoding here is base64, not hex** — this is the single most common porting mistake.

Request:

```json
{ "encryptedMessage": "<base64>" }
```

Response:

```json
{ "decryptedMessage": "<base64>" }
```

Here that wrapping lives in `go/internal/extension/helpers.go`, so a handler never hand-rolls it.

---

## 4. Wire format

Every field below is derived from the Go types that tee-node actually serializes. The Go type is given because it determines the JSON encoding, and getting the encoding wrong is silent — the node accepts the request and the result fails verification later.

Encoding rules for the Go types involved:

| Go type | JSON encoding |
| --- | --- |
| `common.Hash` | `"0x"` + 64 lowercase hex chars (32 bytes, always full width) |
| `common.Address` | `"0x"` + 40 hex chars (20 bytes) |
| `hexutil.Bytes` | `"0x"` + hex, variable length; empty/nil encodes as `"0x"` — **not** `null`, **not** `""` |
| `uint8` / `uint32` / `uint64` | JSON number |
| `string` | JSON string |

**bytes32 identifiers** (`opType`, `opCommand`) are UTF-8 strings right-padded with zero bytes to 32 bytes, then hex-encoded. `"STRAVA"` becomes `0x5354524156410000000000000000000000000000000000000000000000000000` — 6 content bytes followed by 26 zero bytes. The empty string is 32 zero bytes, which carries no special dispatch meaning: see §5.

### 4.1 `Action` — request body of `POST /action`

Source: `tee-node/pkg/types/actions.go`.

| Field | Go type | JSON | Notes |
| --- | --- | --- | --- |
| `data` | `ActionData` | object | §4.2 |
| `additionalVariableMessages` | `[]hexutil.Bytes` | array of hex strings | |
| `timestamps` | `[]uint64` | array of numbers | |
| `additionalActionData` | `hexutil.Bytes` | hex string | |
| `signatures` | `[]hexutil.Bytes` | array of hex strings | |

### 4.2 `ActionData` — the `data` field

| Field | Go type | JSON | Notes |
| --- | --- | --- | --- |
| `id` | `common.Hash` | 32-byte hex | echoed back in the result |
| `type` | `ActionType` (string) | `"instruction"` or `"direct"` | |
| `submissionTag` | `SubmissionTag` (string) | `"threshold"`, `"end"` or `"submit"` | echoed back in the result |
| `message` | `hexutil.Bytes` | hex string | **hex-encoded UTF-8 JSON** that decodes to a `DataFixed` (§4.3) |

Note the double encoding on `message`: hex-decode it, then parse the resulting bytes as JSON.

### 4.3 `DataFixed` — decoded from `ActionData.message`

Source: `go-flare-common/pkg/tee/instruction/instruction.go`.

| Field | Go type | JSON | Notes |
| --- | --- | --- | --- |
| `instructionId` | `common.Hash` | 32-byte hex | |
| `teeId` | `common.Address` | 20-byte hex | |
| `timestamp` | `uint64` | number | |
| `rewardEpochId` | `uint32` | number | |
| `opType` | `common.Hash` | 32-byte hex | bytes32 of the op-type string |
| `opCommand` | `common.Hash` | 32-byte hex | bytes32 of the op-command string |
| `cosigners` | `[]common.Address` | array of 20-byte hex | |
| `cosignersThreshold` | `uint64` | number | |
| `originalMessage` | `hexutil.Bytes` | hex string | **your payload** — the bytes your contract passed to `sendInstructions` |
| `additionalFixedMessage` | `hexutil.Bytes` | hex string | |

`originalMessage` is what a handler receives. Its interpretation is entirely up to the extension — this extension's `DISTANCE` operation treats it as flat-ABI `abi.encode(challenge, caller, contract, chainId, encryptedToken)`, decoded by `DistanceMessageArgs.Unpack()` in `go/pkg/types/types.go`; UTF-8 JSON payloads are equally valid for other operations.

### 4.4 `ActionResult` — response body of `POST /action`

Source: `tee-node/pkg/types/actions.go`.

| Field | Go type | JSON | Notes |
| --- | --- | --- | --- |
| `id` | `common.Hash` | 32-byte hex | echo `action.data.id` |
| `submissionTag` | `SubmissionTag` (string) | string | echo `action.data.submissionTag` |
| `status` | `uint8` | number | §4.6 |
| `log` | `string` | string | §4.6 |
| `opType` | `common.Hash` | 32-byte hex | echo from `DataFixed` |
| `opCommand` | `common.Hash` | 32-byte hex | echo from `DataFixed` |
| `additionalResultStatus` | `hexutil.Bytes` | hex string | `"0x"` when unused |
| `version` | **`string`** | **plain string** | see the warning below |
| `data` | `hexutil.Bytes` | hex string | your response payload; `"0x"` when there is none |

> **Every field is always present.** The Go struct carries no `omitempty` tags,
> so `data` and `additionalResultStatus` marshal as `"0x"` rather than being
> omitted, and `log` is always a string. An implementation that drops empty
> fields produces a different JSON shape from the reference Go image.

**`data` must be byte-exact across implementations.** The signed hash is
`ActionResult.Hash()`, which is
`keccak256(keccak256(data) || id || keccak256(submissionTag) || status)` — so
`keccak256(data)` is not itself the signed value, it is the leading 32-byte
component of it. Either way `data` feeds the signature, so serialization details
matter: emit compact JSON with no whitespace, preserving field declaration
order. Go's `encoding/json`, Python dicts and TypeScript object literals all do
this naturally; the cross-language vector test compares the resulting hex exactly.

> **`version` is a plain string, not bytes32.**
>
> The Go declaration is `Version string` (`tee-node/pkg/types/actions.go:57`). Send `"0.2.0"`, not `"0x302e322e30000..."`.
>
> This is easy to get wrong because `StateResponse.stateVersion` *is* bytes32 (§4.5) — the two are genuinely asymmetric, and older ports of other extensions have hex-encoded `ActionResult.version` incorrectly. `buildResult` in `go/internal/extension/utils.go` shows the correct encoding.

### 4.5 `StateResponse` — response body of `GET /state`

| Field | Type | JSON | Notes |
| --- | --- | --- | --- |
| `stateVersion` | `common.Hash` | **32-byte hex** | bytes32 of the version string — asymmetric with `ActionResult.version` by design |
| `state` | extension-defined | object | any JSON-serializable snapshot |

### 4.6 `status` and `log`

| `status` | Meaning | Required `log` |
| --- | --- | --- |
| `0` | Handler failed | `"error: <message>"` |
| `1` | Handler succeeded | `"ok"` |
| anything else | In progress | `"pending"` |

`data` is only meaningful for `status == 1`.

---

## 5. Handler dispatch

An extension dispatches on the `(opType, opCommand)` pair, both compared as bytes32 values.

**Dispatch:** an exact match on `opType`, then an exact match on `opCommand` within it. There is no wildcard and no default handler — an empty `opCommand` is just another unmatched value, not a fallback. Anything unmatched at either level is a 501 whose body names the value received and the value expected.

**Concurrency:** the framework does **not** serialize handler invocations — handlers may run concurrently with each other and with `GET /state`. Guarding the extension's own mutable state is therefore the implementation's responsibility (a mutex in Go and Python, the equivalent elsewhere), and it is required: a `GET /state` read must never observe a half-applied mutation.

---

## 6. Container requirements

### Environment variables consumed

| Variable | Meaning |
| --- | --- |
| `MODE` | `1` = simulated attestation (local dev), `0` = production attestation |
| `CONFIG_PORT` | tee-node configuration endpoint (default `5501`) |
| `SIGN_PORT` | tee-node signing/crypto endpoint the extension calls (default `7701`) |
| `EXTENSION_PORT` | port the extension serves `/action` and `/state` on (default `7702`) |
| `PROXY_URL` | extension proxy the node polls for actions |
| `CHAIN_ID` | chain the node binds signatures to; must match the proxy and the chain |
| `LOG_LEVEL` | node log level |
| `INITIAL_OWNER` | extension owner address |
| `GOVERNANCE_SIGNERS` | comma-separated governance signer addresses |
| `GOVERNANCE_THRESHOLD` | governance threshold |

An extension implementation itself only needs to read `EXTENSION_PORT` and `SIGN_PORT`; the rest are consumed by tee-node. All of them must still be *settable* on the container.

### Ports

```dockerfile
EXPOSE 5501 7701 7702
```

### Launch policy label — required

```dockerfile
LABEL "tee.launch_policy.allow_env_override"="LOG_LEVEL,PROXY_URL,INITIAL_OWNER,EXTENSION_ID,INSTRUCTION_SENDER,CHAIN_URL,CHAIN_ID,GOVERNANCE_SIGNERS,GOVERNANCE_THRESHOLD,CONFIG_PORT,SIGN_PORT,EXTENSION_PORT,TYPES_SERVER_PORT"
```

Without this label, a GCP Confidential Space VM **rejects operator env overrides at attestation time** and whatever was baked into the image at build time is final. Every language image must carry an identical list — a mismatch produces a deployment that cannot be reconfigured, and the failure appears at attestation rather than at build.

Two rules about the list's content:

- `CHAIN_ID`, `GOVERNANCE_SIGNERS` and `GOVERNANCE_THRESHOLD` **must be in the list** — they are deployment-specific (chain id differs per network, the governance set is chosen per deployment), so they cannot be baked to one correct value in the measured image.
- `MODE` **must never be in the list** — it selects the attestation backend, and listing it would let the workload operator switch a real Confidential Space VM to simulated attestation at launch. It is baked to `0` in the image; local development overrides it through Docker Compose, which the label does not govern. The dev-only `go/Dockerfile.siblings` bakes `1` rather than `0`, but it omits `MODE` from the label on the same principle.

This code block is cross-checked against `go/Dockerfile` by `scripts/check-versions.sh` (run as a pre-build gate and in CI), so the documentation cannot drift from the image again.

### User

```dockerfile
USER 0:0
```

Matches tee-node. The TEE itself is the isolation boundary, not in-container user separation.

---

## 7. Reproducibility

The container's code hash is what gets registered on-chain, so build determinism is a security property, not a nicety.

Every language image must:

- accept a `SOURCE_DATE_EPOCH` build arg and propagate it,
- pin apt to `snapshot.debian.org` keyed on `SOURCE_DATE_EPOCH`,
- install dependencies from a committed lockfile (`go.sum`, `package-lock.json`, pinned `requirements.txt`),
- normalize mtimes as the final build step: `RUN find /app -exec touch -h -d @${SOURCE_DATE_EPOCH} {} +`

Determinism is not equal across languages. Go on distroless is bit-for-bit reproducible across machines — this repo's `go/Dockerfile` achieves that; see `REPRODUCIBILITY.md` for what exactly is guaranteed and how to verify it. Python wheels and `node_modules` trees embed build-host paths and package-manager-dependent layout, so images in those languages typically target *same-machine* determinism only.

**tee-node version pinning.** Non-Go images build the tee-node `./cmd/extension` binary from source. The version must match the Go module pin in `go/go.mod`, or the node and the proxy will disagree on signature formats and surface confusing verification failures. That pin is frequently a Go *pseudo-version* (`v0.0.21-0.20260619120252-31fc839ae6d2`), whose last segment is an **abbreviated** commit SHA. Two obvious approaches both fail on it: `git clone --branch <sha>` resolves tags and branches only, and `git fetch --depth 1 origin <sha>` requires a full 40-char SHA plus server-side `uploadpack.allowAnySHA1InWant`, which GitHub rejects with `couldn't find remote ref`. Use a blobless partial clone, which fetches all refs cheaply and lets an abbreviated SHA resolve locally:

```dockerfile
RUN git clone --filter=blob:none https://github.com/flare-foundation/tee-node.git tee-node && cd tee-node && git checkout "${TEE_NODE_REF}"
```

`scripts/lib/versions.sh` derives `TEE_NODE_REF` from `go/go.mod`, and `scripts/check-versions.sh` fails the build if the pins drift apart.

---

## 8. Adding a language

1. Create `<language>/` with a `language.env` (that file is what makes the directory a discoverable implementation) and a `Dockerfile` producing an image that satisfies §2, §4, §6.
2. Implement the framework layer: HTTP server, wire types, bytes32 helpers, dispatch registry, serialization. The reference implementation to read is `go/pkg/server/` plus `go/internal/extension/utils.go`.
3. Implement this extension's operation — `STRAVA`/`DISTANCE` — behaviourally identical to the Go version: same grant verification, same eligibility rules, and a byte-identical signed payload (`abiEncodeDistanceProofPayload`; the expected hash is pinned by `test/SignPayloadVector.t.sol` and the Go vector test, so port that test first).
4. Validate against the layers that exist: the cross-language vector test, `./scripts/test-types-server.sh` against your implementation's wire output, and the end-to-end `./scripts/test.sh`. There is no fixture-replay suite; a set of golden request/response fixtures is the more rigorous option if you need one.
5. Set `LANGUAGE=<language>` in `.env` and run the normal flow. Nothing in `scripts/`, `tools/`, `contracts/` or `docker-compose.yaml` needs to change — language selection resolves by directory convention.
