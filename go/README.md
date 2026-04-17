# Go implementation

The default implementation. Selected with `LANGUAGE=go` in `.env` (also the fallback when `LANGUAGE` is unset).

Go embeds tee-node as a **library**, so this image runs a single static binary on a distroless base: ~22 MB, and bit-for-bit reproducible across machines.

## Layout

```
cmd/
├── main.go                Standalone extension server (local dev)
├── docker/main.go         Combined tee-node + extension — the image entry point
├── start-tee/main.go      Host-process runner backing `start-services.sh --local`
└── types-server/main.go   Standalone types-server (ships in the same image)
internal/
├── config/config.go       ★ Version, OPType/OPCommand constants, thresholds, budgets
├── extension/
│   ├── extension.go       ★ MAIN CUSTOMIZATION POINT — routing and handlers
│   ├── grant.go           Token-grant layout and verification (domain/purpose tags)
│   ├── helpers.go         Strava API calls, sport-type filter, tee-node decrypt/sign
│   ├── utils.go           Infrastructure: actionHandler, buildResult
│   ├── extension_test.go  Unit tests incl. the cross-language sign-payload vector
│   └── grant_test.go      Grant layout tests incl. the cross-module wire vector
└── typesserver/           types-server HTTP handlers (/decode, /registry, /health)
pkg/
├── decoder/               Decoder registry: JSON, tuple-ABI and flat-ABI decoders
├── server/                Infrastructure: StartExtension
└── types/                 ★ Request/response/state types + decoder registration
```

★ = yours to change. Everything else is infrastructure — see the `DO NOT MODIFY` comments.

## Develop

```bash
cd go && go build ./... && go test ./...
```

Or through the language-neutral entry point, from the repo root:

```bash
./scripts/test-unit.sh go
```

Run the extension alone (no tee-node, no proxy) on a port of your choosing:

```bash
EXTENSION_PORT=8080 go run ./cmd
```

## Add an operation

1. Add the constants to `internal/config/config.go`
2. Add request/response structs to `pkg/types/types.go`
3. Add a `case` to `processAction` and write the handler in `internal/extension/extension.go`
4. Mirror the `bytes32` constants and a send function in `../contracts/InstructionSender.sol`

Full walkthrough: [../docs/extension-guide.md](../docs/extension-guide.md).

## Things specific to this implementation

**You must lock.** `Extension` holds mutable state guarded by `e.mu`. The framework does not serialize handler calls for you — `actionHandler` may run concurrently.

**`buildResult` owns the envelope.** Return through it rather than constructing an `ActionResult` by hand; it sets the `log` values the wire contract requires.

**ABI payloads are flat.** The contract encodes messages with `abi.encode(val1, val2, val3)` (flat arguments), so handlers decode with `abi.Arguments.Unpack()` — see `DistanceMessageArgs` in `pkg/types/types.go`. `structs.DecodeTo` with a single tuple `abi.Argument` is only for `abi.encode(SomeStruct(...))`-style payloads; using it on flat encodings mis-reads the first word as a tuple offset.

**The types-server is a second binary.** `cmd/types-server` serves `POST /decode` (port 8100), turning raw message/result hex into JSON via the decoder registry in `pkg/decoder` + `pkg/types/register.go`. It ships in the same Docker image; compose runs it as its own service.

## Verify

```bash
cd go && go build ./... && go test ./...
./scripts/test-types-server.sh   # needs the types-server running (docker compose up types-server)
```

This repo carries only the Go implementation, so there is no cross-language conformance suite here — the two encodings that *are* shared with something outside this directory are pinned by paired vector tests instead: the signed proof payload against `../test/SignPayloadVector.t.sol`, and the grant layout against `../tools/pkg/fccutils/grant_test.go`. Run both halves after touching either.
