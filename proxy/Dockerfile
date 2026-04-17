# Hardened, self-contained build of tee-proxy.
# Clones tee-proxy from GitHub at build time so no local sibling repos are needed.
#
# TEE_PROXY_VERSION MUST stay aligned with the extension's Go-module pins
# (tools/go.mod): tee-proxy v0.0.18. tee-proxy's own go.mod transitively pins
# the matching tee-node and go-flare-common line, so building it with
# `go mod download` pulls the exact governance-aware node code the extension-tee
# image uses — no separate tee-node clone or replace directive is needed.
# Override via build arg when bumping both sides together.
#
# Supply-chain hardening:
#   - both base images are pinned by digest, not by mutable tag
#   - the source checkout is verified against the full commit SHA — a moved or
#     re-tagged v0.0.18 fails the build instead of silently changing the input
#   - the builder is a security-patched Go (1.25.13 clears the stdlib
#     advisories govulncheck reported against 1.25.1)
#   - known-vulnerable modules in v0.0.18's dependency graph are backported to
#     their fixed versions at build time (exact pins, no floating @latest)
# Evidence: docs/security/govulncheck-tee-proxy.txt — 0 symbol-reachable
# advisories in ./cmd/proxy with these steps in place.

ARG TEE_PROXY_VERSION=v0.0.18
# Full commit SHA the tag pointed at when it was reviewed (git ls-remote).
ARG TEE_PROXY_COMMIT=2e8412990d150a386a2ee5fdc1d2889a04e658aa

FROM golang:1.25.13-alpine@sha256:844b27705f54e73773e0f9bc3c780633b9d7f4b4831bf35cdad02a81a4c80bd0 AS builder

RUN apk add --no-cache git

ARG TEE_PROXY_VERSION
ARG TEE_PROXY_COMMIT

# The installed toolchain is the security floor — never auto-download whatever
# newer toolchain a go.mod might ask for.
ENV GOTOOLCHAIN=local

WORKDIR /app
# Clone by tag for readability, then verify the FULL commit SHA: tags are
# mutable, the SHA is not.
RUN git clone --depth 1 --branch ${TEE_PROXY_VERSION} https://github.com/flare-foundation/tee-proxy.git \
 && cd tee-proxy \
 && actual="$(git rev-parse HEAD)" \
 && if [ "$actual" != "${TEE_PROXY_COMMIT}" ]; then \
        echo "FATAL: tee-proxy tag ${TEE_PROXY_VERSION} points at $actual, expected ${TEE_PROXY_COMMIT}" >&2; \
        exit 1; \
    fi

WORKDIR /app/tee-proxy

# Security backports (exact pins — keep in step with the advisory list in
# docs/security/govulncheck-tee-proxy.txt):
#   grpc     v1.83.0  GO-2026-6061 (xDS RBAC / HTTP2 transport)
#   x/text   v0.41.0  GO-2026-5970 (infinite loop on invalid input)
#   x/net    v0.57.0  GO-2026-5026 (idna punycode acceptance)
#   otel     v1.44.0  GO-2026-5506 (baggage header allocations)
#   edwards25519 v1.1.1  GO-2026-4503 (imported, not called — bumped anyway)
RUN go get \
        google.golang.org/grpc@v1.83.0 \
        golang.org/x/text@v0.41.0 \
        golang.org/x/net@v0.57.0 \
        go.opentelemetry.io/otel@v1.44.0 \
        go.opentelemetry.io/otel/metric@v1.44.0 \
        go.opentelemetry.io/otel/trace@v1.44.0 \
        filippo.io/edwards25519@v1.1.1 \
 && go mod tidy

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-buildid= -s -w" -a -o main ./cmd/proxy

# Final stage
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

WORKDIR /app

COPY --from=builder /app/tee-proxy/main .
COPY --from=builder /app/tee-proxy/config.example.toml ./config/config.toml

RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

RUN chown -R appuser:appgroup /app

USER appuser

EXPOSE 6661
EXPOSE 6662

CMD ["./main"]
