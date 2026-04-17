# Reproducible Builds

The image's code hash is what gets registered on-chain, so build determinism is
a security property rather than a nicety.

## What is guaranteed

This repo ships a single Go implementation, which carries the strongest
guarantee available: **bit-for-bit reproducibility across machines**. The image
is a static `CGO_ENABLED=0` binary built with `-trimpath -buildid=` on a
digest-pinned builder; nothing host-specific survives, so an auditor on
different hardware can independently reproduce the registered code hash.

## How it works

- `SOURCE_DATE_EPOCH` is set to the commit timestamp and passed as a build arg
  to clamp all timestamps. `start-services.sh` derives it from the last commit
  automatically; a build without it (compose defaults the arg to `0`) still
  works but is **not** reproducible — the apt snapshot pinning and timestamp
  clamping are skipped, so never register the codeHash of such a build. Setting it
  is necessary but not sufficient: see
  [Which build produces the deployable image](#which-build-produces-the-deployable-image)
- Go binary is built with `-trimpath -ldflags="-buildid= -s -w"` and
  `-buildvcs=false` to strip non-deterministic metadata; `CGO_ENABLED=0`
  produces a static binary so link-time libc variance cannot leak in
- Both image stages are pinned by digest in the Dockerfile (builder
  `golang:1.25.13-trixie@sha256:…`, final `gcr.io/distroless/static@sha256:…`),
  as are the compose-pulled `redis` image and the tee-proxy build inputs
  (`proxy/Dockerfile` pins its base images by digest and verifies the source
  checkout against a full commit SHA)
- Debian package versions are pinned via apt's native snapshot support
  (Debian 13+): `Snapshot: true` in the sources file plus `apt-get install
  --snapshot <timestamp>`, where the timestamp is `SOURCE_DATE_EPOCH` formatted
  as `%Y%m%dT%H%M%SZ`. That redirects every fetch to
  [snapshot.debian.org](https://snapshot.debian.org) at the exact instant of
  the commit, so the same `SOURCE_DATE_EPOCH` always yields the same package
  bytes. Adapted from
  [reproducible-containers/repro-sources-list.sh](https://github.com/reproducible-containers/repro-sources-list.sh/blob/master/alternative/Dockerfile.debian-13),
  with one deviation: TLS peer verification stays enabled (the pinned golang
  base already ships a CA store, so the upstream `Verify-Peer=false` bootstrap
  hack is unnecessary)
- Verifying a published image needs BuildKit's
  [`rewrite-timestamp=true`](https://github.com/moby/buildkit/pull/4057)
  exporter option to normalize layer timestamps (see below)

## Build context

The default build is self-contained: the build context is the repo root
(`docker-compose.yaml` sets `context: .`, `dockerfile: ${EXTENSION_DOCKERFILE}`,
resolved from `LANGUAGE`). `go/go.mod` pins
`github.com/flare-foundation/tee-node` to a released version and fetches it from
the network (verified against `go.sum`), so the build needs only this repo's own
sources — no sibling `tee-node/` checkout.

The build uses `go/Dockerfile.dockerignore` (BuildKit prefers it over the root
`.dockerignore`). This is not only a build-speed concern: anything reachable in
the context can perturb layer hashes, so stray local artifacts would otherwise
undermine determinism.

> **Developing `tee-node`/`tee-proxy` locally?** Run
> `USE_LOCAL_SIBLINGS=1 ./scripts/start-services.sh`, which builds from on-disk
> sibling checkouts via `go/Dockerfile.siblings` (build context `tee/`). That
> path is Go-only and is for local iteration — it uses whatever is checked out
> and is **not** reproducible. `start-services.sh` rejects it for other
> languages, which build tee-node from the pinned ref instead.

## Which build produces the deployable image

`docker compose build` (and `start-services.sh`, which runs `docker compose up -d
--build`) is **not** the recipe below. Compose passes `SOURCE_DATE_EPOCH` through
as a build arg — `docker-compose.yaml` sets `args: SOURCE_DATE_EPOCH:
${SOURCE_DATE_EPOCH:-0}` — but the compose build path never sets BuildKit's
`rewrite-timestamp=true` exporter option, and `SOURCE_DATE_EPOCH` on its own does
not normalize every layer timestamp
([moby/buildkit#3180](https://github.com/moby/buildkit/issues/3180)). A
compose-built image is therefore not guaranteed to rebuild to the same image ID,
even from the same commit.

So build the image intended for a real deployment with the same `docker buildx
build … --output "type=docker,rewrite-timestamp=true"` invocation shown below, on
a `docker-container` builder. Compose builds are fine for local and testnet work,
but a compose-built image must never be the artifact whose codeHash gets
allow-listed: an auditor re-running the verification recipe on that commit has no
reason to arrive at the same ID.

## Verifying a remote image

The default Docker builder does not properly support `rewrite-timestamp`
([moby/buildkit#4230](https://github.com/moby/buildkit/issues/4230)). You need
a BuildKit builder using the `docker-container` driver.

Create the builder (one-time setup):

```sh
docker buildx create --driver=docker-container --name=moby-buildkit --driver-opt image=moby/buildkit --bootstrap
```

Clone the extension repository (self-contained — no sibling `tee-node/` needed;
the pinned module is fetched from the network at build time):

```sh
git clone https://github.com/MancaStrah/activity-reward-extension.git
cd activity-reward-extension
```

Check out the exact revision being verified. This repo does not publish
release tags yet, so pin by **full commit SHA**; when a signed release tag
exists, prefer it:

```sh
git checkout <full-commit-sha-being-verified>
```

Build locally and compare the image ID against the image the operator actually
deployed (replace `<published-image>@sha256:<digest>` with the registry
reference the operator published — deploy and mirror **by digest, not by tag**,
see docs/deployment-steps.md):

```sh
docker buildx build --builder moby-buildkit --platform linux/amd64 --no-cache \
  --build-arg SOURCE_DATE_EPOCH=$(git log -1 --format=%ct) \
  --output "type=docker,rewrite-timestamp=true" \
  -t local/activity-reward-extension:verify --load -f go/Dockerfile .

docker pull --platform linux/amd64 <published-image>@sha256:<digest>

docker inspect --format='{{.Id}}' local/activity-reward-extension:verify
docker inspect --format='{{.Id}}' <published-image>@sha256:<digest>
```

Both IDs should be identical. No image of this extension is published to a
public registry at the time of writing — the operator hands the image (or its
registry digest) over out-of-band, and that digest is what the
[production allowlisting runbook](docs/production-allowlisting.md) feeds to
`allow-tee-version` as the expected codeHash.

## Upstream references

- [moby/buildkit#3180](https://github.com/moby/buildkit/issues/3180) -
  `rewrite-timestamp` only clamps timestamps *down* to `SOURCE_DATE_EPOCH`,
  older timestamps are left unchanged. The Dockerfile works around this with
  an explicit `find + touch` to normalize all timestamps before COPY.
- [moby/buildkit#4057](https://github.com/moby/buildkit/pull/4057) - PR that
  added `rewrite-timestamp` support to BuildKit exporters
- [moby/buildkit#4230](https://github.com/moby/buildkit/issues/4230) - open
  issue tracking `rewrite-timestamp` incompatibility with the default Docker
  builder and `--load` (`unpack` conflict)
