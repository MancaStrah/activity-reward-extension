#!/usr/bin/env bash
#
# Start extension TEE node and proxy.
#
# By default, starts services via Docker Compose, picking the compose overlay
# from --chain (or env CHAIN, or legacy LOCAL_MODE):
#   --chain local    → docker-compose.yaml only (local devnet)
#   --chain coston   → + docker-compose.coston.yaml
#   --chain coston2  → + docker-compose.coston2.yaml
#
# Pass --local to start services as background Go processes instead of Docker.
#
# On coston/coston2 this first syncs the tunnel's public URL into .env as
# EXT_PROXY_URL — no flag needed. Which tunnel is TUNNEL_PROVIDER:
#
#   ngrok (default)  reads the URL off your already-running ngrok agent (its
#                    local API, 127.0.0.1:4040). Starting and stopping the agent
#                    is yours; --tunnel only makes it mandatory, failing fast
#                    when none is reachable rather than letting the proxy /info
#                    wait time out with an unrelated-looking error.
#   cloudflared      drives docker-compose.cloudflared.yaml: a tunnel that is
#                    already running is reused and resynced, and --tunnel
#                    additionally starts one when none is. See docs/cloudflared.md.
#
# Usage:
#   ./scripts/start-services.sh                       # local devnet, docker compose
#   ./scripts/start-services.sh --chain coston        # Coston, docker compose
#   ./scripts/start-services.sh --local               # local devnet, Go processes
#   ./scripts/start-services.sh --chain coston2 --tunnel      # require a reachable ngrok agent
#   TUNNEL_PROVIDER=cloudflared ./scripts/start-services.sh --chain coston2 --tunnel   # start the cloudflared tunnel
#
# By default the node and proxy are built from PINNED module/image versions
# (tee-node + tee-proxy), fully self-contained — no sibling repos required.
# To build them from on-disk sibling checkouts instead (while developing
# tee-node / tee-proxy), set USE_LOCAL_SIBLINGS=1:
#   USE_LOCAL_SIBLINGS=1 ./scripts/start-services.sh --chain coston2
#
# An existing local/tee-proxy image is reused only when its provenance labels
# show it came from the same recipe this run would use; otherwise the run stops
# rather than silently starting an image of unknown origin. Set
# ALLOW_UNVERIFIED_PROXY_IMAGE=true to reuse it anyway (warns instead).
#
# Prerequisites:
#   - Infrastructure running (Hardhat, indexer, Redis, normal TEE + proxy)
#   - config/extension.env exists (created by pre-build.sh), OR EXTENSION_ID is set
#   - Redis will be started on :6382 automatically (separate from infrastructure Redis)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
log()  { echo -e "${GREEN}[start-services]${NC} $*"; }
die()  { echo -e "${RED}[start-services] ERROR:${NC} $*" >&2; exit 1; }

# --- Parse flags ---
USE_LOCAL=false
USE_TUNNEL=false
CHAIN="${CHAIN:-}"
# Kept separate from CHAIN: `set -a; source .env` below re-exports whatever CHAIN
# the file sets, so a value assigned here would be silently overwritten and
# `--chain local` would run against whatever .env names — the templates ship
# CHAIN=coston2. The flag is re-applied after the file is loaded.
CHAIN_FROM_FLAG=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --local) USE_LOCAL=true; shift ;;
        --tunnel) USE_TUNNEL=true; shift ;;
        --chain) [[ $# -ge 2 ]] || die "--chain requires a value (local|coston|coston2)"
                 CHAIN_FROM_FLAG="$2"; shift 2 ;;
        --chain=*) CHAIN_FROM_FLAG="${1#--chain=}"; shift ;;
        *) die "Unknown argument: $1" ;;
    esac
done

# --- Load .env from project root (if present) ---
if [[ -f "$PROJECT_DIR/.env" ]]; then
    set -a
    source "$PROJECT_DIR/.env"
    set +a
fi

# --- Load extension config ---
CONFIG_FILE="$PROJECT_DIR/config/extension.env"
if [[ -f "$CONFIG_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$CONFIG_FILE"
fi

EXTENSION_ID="${EXTENSION_ID:-}"
PROXY_PRIVATE_KEY="${PROXY_PRIVATE_KEY:-}"
LOCAL_MODE="${LOCAL_MODE:-true}"

# --- Resolve CHAIN (flag > env > legacy LOCAL_MODE) ---
[[ -n "$CHAIN_FROM_FLAG" ]] && CHAIN="$CHAIN_FROM_FLAG"
if [[ -z "$CHAIN" ]]; then
    if [[ "$LOCAL_MODE" == "true" ]]; then
        CHAIN="local"
    else
        CHAIN="coston2"  # legacy
    fi
fi
case "$CHAIN" in
    local|coston|coston2) ;;
    *) die "Unknown --chain value: $CHAIN (valid: local, coston, coston2)" ;;
esac

# --- Resolve the tunnel provider (ngrok unless asked otherwise) ---------------
# ngrok is the default and stays a read-only integration: you run the agent,
# this only reads its current URL. cloudflared runs as a container out of
# docker-compose.cloudflared.yaml, which is ours to start and stop, so it has to
# be selected explicitly — a silent fallback would start a container nobody
# asked for. An unrecognised value is a typo; falling back to ngrok there would
# look like "--tunnel does nothing".
TUNNEL_PROVIDER="${TUNNEL_PROVIDER:-ngrok}"
case "$TUNNEL_PROVIDER" in
    ngrok|cloudflared) ;;
    *) die "Unknown TUNNEL_PROVIDER: $TUNNEL_PROVIDER (valid: ngrok, cloudflared)" ;;
esac

[[ -n "$EXTENSION_ID" ]] || die "EXTENSION_ID not set. Run pre-build.sh first or set it manually."

# The enclave refuses to sign unless it knows which contract and chain it belongs to
# (see checkDeploymentIdentity), and it learns the contract from INSTRUCTION_SENDER,
# which reaches the container through config/extension.env. Checked here so a missing
# value is one legible failure now rather than every proof being refused later.
INSTRUCTION_SENDER="${INSTRUCTION_SENDER:-}"
[[ -n "$INSTRUCTION_SENDER" ]] || die "INSTRUCTION_SENDER not set. Run pre-build.sh first (it writes config/extension.env) or set it manually."

# --- Resolve + fail-fast validate the TEE profile ------------------------------
# The MODE/SIMULATED_TEE/attestation matrix must be coherent BEFORE anything
# starts: MODE=1 against a magic_pass-rejecting proxy config can never
# bootstrap, and the failure would otherwise surface as an unrelated-looking
# /info timeout. See scripts/lib/profile.sh for the three profiles.
# shellcheck source=lib/profile.sh
source "$SCRIPT_DIR/lib/profile.sh"
resolve_tee_profile "$CHAIN" || die "TEE profile not resolved — set TEE_PROFILE in .env"
validate_tee_profile "$CHAIN" || die "TEE profile matrix is inconsistent — fix .env (templates: .env.example, .env.confidential-space.example)"

# Hand compose the values this pre-flight just validated, rather than letting it
# apply defaults of its own. The two used to default independently — here
# `${MODE:-1}` for the matrix check, and `MODE=${MODE:-1}` again in
# docker-compose.yaml — so the containers could run a posture the check never
# examined, and a change to either default would silently split them. Exporting
# makes the validated value the only one, and lets compose require it (there are
# no fallbacks left there to paper over an unset variable).
MODE="${MODE:-1}"
SIMULATED_TEE="${SIMULATED_TEE:-true}"
export TEE_PROFILE MODE SIMULATED_TEE LOCAL_MODE

# --local cannot host a real Confidential Space workload, and this has to be settled
# before the proxy config is resolved below: under that profile the config check would
# otherwise reject the host config on attestation grounds and report the wrong reason.
[[ "$USE_LOCAL" != "true" || "$TEE_PROFILE" != "confidential-space" ]] \
    || die "--local runs plain Go host processes; TEE_PROFILE=confidential-space requires a real Confidential Space VM"

# --- Resolve + fail-fast validate the proxy config, for whichever mode this is ---
# Docker mode bind-mounts config/proxy/extension_proxy[.<chain>].docker.toml as the
# proxy's config.toml; --local runs tools/cmd/start-proxy, which reads
# config/proxy/extension_proxy[.<chain>].toml.
#
# ONE call site for both, on purpose, and placed before either branch. The check used
# to sit inside the Docker branch, so `--local` started against whatever the host
# config said — and in a fresh clone that file carries no [attestation] section at
# all, which upstream reads as enable=false. A per-branch check is one a new branch
# can be written without, so the branch that decides *how* to start no longer decides
# *whether* the config was checked.
if [[ "$USE_LOCAL" == "true" ]]; then PROXY_MODE=host; else PROXY_MODE=docker; fi
PROXY_CFG_PATH="$(resolve_proxy_config "$PROXY_MODE" "$CHAIN" "$PROJECT_DIR")" \
    || die "the proxy config for --chain $CHAIN is not usable (see above) — fix it before anything starts"
PROXY_CFG="$(basename "$PROXY_CFG_PATH")"
log "Proxy config:   config/proxy/$PROXY_CFG ([attestation] checked against TEE_PROFILE=$TEE_PROFILE)"
# Host mode hands PROXY_CFG_PATH to start-proxy as PROXY_CONFIG, which that binary
# honours ahead of its own lookup — so the file it opens is the file just validated
# rather than one it rediscovers. Passed on the command line where the proxy is
# started, next to PROXY_PRIVATE_KEY, rather than exported here: one place says it,
# and nothing else in this script (compose, go build, the tunnel) inherits it.
# Docker mode needs no equivalent; compose names the path in its own volume entry.

# --- Resolve LANGUAGE by directory convention ---------------------------------
# Discovery is driven by <LANGUAGE>/language.env; there is deliberately no
# hardcoded list here (see docs/extension-contract.md §8).
# shellcheck source=lib/language.sh
source "$SCRIPT_DIR/lib/language.sh"
load_language "$PROJECT_DIR" || die "could not resolve LANGUAGE"

# --- Derive dependency versions from the Go module pin ------------------------
# Language images that build tee-node from source consume TEE_NODE_REF; see
# scripts/lib/versions.sh for why a pseudo-version needs a SHA, not a tag.
# shellcheck source=lib/versions.sh
source "$SCRIPT_DIR/lib/versions.sh"
load_versions "$PROJECT_DIR" || die "could not derive dependency versions"

# --- Guard: --local runs Go binaries directly, so it is Go-only ---------------
if [[ "$USE_LOCAL" == "true" && "$LANGUAGE" != "go" ]]; then
    die "--local mode builds and runs Go binaries in-process and only supports LANGUAGE=go (got '$LANGUAGE').\n  Use Docker Compose mode instead: ./scripts/start-services.sh --chain $CHAIN"
fi

# --- Resolve local-siblings toggle ---
case "${USE_LOCAL_SIBLINGS:-}" in
    1|true|yes|on) USE_LOCAL_SIBLINGS=true ;;
    *)             USE_LOCAL_SIBLINGS=false ;;
esac

# The sibling build compiles tee-node into the Go binary; other languages build
# tee-node as a separate binary from a pinned ref and have no sibling path.
if [[ "$USE_LOCAL_SIBLINGS" == "true" && "$LANGUAGE" != "go" ]]; then
    die "USE_LOCAL_SIBLINGS is only supported for LANGUAGE=go (got '$LANGUAGE').\n  Other languages build tee-node from the pinned ref ($TEE_NODE_REF).\n  To test a local tee-node there, push it and update the pin in go/go.mod."
fi

log "Chain:          $CHAIN"
log "TEE profile:    $TEE_PROFILE"
log "Language:       $LANGUAGE ($EXTENSION_DOCKERFILE)"
log "Extension ID:   $EXTENSION_ID"
log "Local mode:     $LOCAL_MODE"
[[ "$CHAIN" != "local" ]] && log "Tunnel:         $TUNNEL_PROVIDER"
log "Local siblings: $USE_LOCAL_SIBLINGS"
log "tee-node ref:   $TEE_NODE_REF"

# --- Manage go.work for host builds (git-ignored, fully managed here) ---
# Toggle on: host `go` builds (e.g. --local mode) resolve the on-disk sibling
# tee-node / tee-proxy. Toggle off: remove any stale go.work so host builds fall
# back to the pinned module versions in go.mod / tools/go.mod.
GOWORK_FILE="$PROJECT_DIR/go.work"
if [[ "$USE_LOCAL_SIBLINGS" == "true" ]]; then
    SIBLINGS_ROOT="$(cd "$PROJECT_DIR/../.." && pwd)"
    for sib in tee-node tee-proxy; do
        [[ -d "$SIBLINGS_ROOT/$sib" ]] || die "USE_LOCAL_SIBLINGS set but $SIBLINGS_ROOT/$sib is missing.\n  Clone it into $SIBLINGS_ROOT/, or unset USE_LOCAL_SIBLINGS to use pinned modules."
    done
    log "Writing $GOWORK_FILE (host builds use sibling tee-node / tee-proxy)"
    GO_VERSION=$(grep '^go ' "$PROJECT_DIR/go/go.mod" | awk '{print $2}')
    cat > "$GOWORK_FILE" <<EOF
// Generated by scripts/start-services.sh for USE_LOCAL_SIBLINGS (git-ignored).
// Points host \`go\` builds at the on-disk sibling tee-node / tee-proxy checkouts.
// Re-run start-services.sh without USE_LOCAL_SIBLINGS (or delete this file) to
// return to the pinned module versions in go/go.mod / tools/go.mod.
go $GO_VERSION

use (
	./go
	./tools
	../../tee-node
	../../tee-proxy
)
EOF
elif [[ -f "$GOWORK_FILE" ]]; then
    log "Removing stale $GOWORK_FILE — host builds use pinned modules"
    rm -f "$GOWORK_FILE" "$PROJECT_DIR/go.work.sum"
fi

# Synced before the other containers so EXT_PROXY_URL is public by the time
# post-build.sh registers it. Whichever provider is selected, a tunnel that is
# already up is read and written into .env, which is what makes switching
# between extensions (and surviving a rotated URL) work.
TUNNEL_ACTIVE=false

# Make a discovered tunnel URL the EXT_PROXY_URL for this run and for every
# script that re-sources .env afterwards.
#   $1 = the URL, $2 = what produced it, $3 = command that shows the raw source
#
# Both providers go through this one function because the URL is untrusted
# either way: the ngrok agent API is unauthenticated loopback, so any local
# process that can bind its port supplies it, and the cloudflared URL is scraped
# out of container logs. It ends up in .env, which every lifecycle script
# `source`s — and `source` performs command substitution, so a URL containing
# $(...), backticks or a shell metacharacter would execute as the operator, in a
# shell holding the deployment and proxy keys. A `|` would also escape a sed
# replacement into sed's own command language, which is why the write below uses
# none. Accept only what a tunnel hostname can actually contain.
publish_tunnel_url() {
    local url="$1" origin="$2" inspect="$3"

    if [[ ! "$url" =~ ^https://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$ ]]; then
        die "$origin returned a URL that is not a plain https URL.\n  Refusing to write it to .env, which the deployment scripts source.\n  Inspect it yourself:  $inspect"
    fi

    export EXT_PROXY_URL="$url"
    # post-build.sh and test.sh re-source .env, so the file is how the fresh URL
    # reaches them — exporting alone would be overwritten.
    if [[ -f "$PROJECT_DIR/.env" ]]; then
        # Drop the old assignment and append the new one with printf, so the URL
        # is only ever handled as data. Interpolating it into a sed replacement
        # would hand it to sed's command language on top of the shell's.
        local tmp
        tmp="$(mktemp "$PROJECT_DIR/.env.XXXXXX")" || die "could not create a temp file next to .env"
        chmod 600 "$tmp"
        grep -v '^EXT_PROXY_URL=' "$PROJECT_DIR/.env" > "$tmp" || true
        printf 'EXT_PROXY_URL=%s\n' "$url" >> "$tmp"
        mv "$tmp" "$PROJECT_DIR/.env"
        log "Tunnel URL: $url  (written to .env)"
    else
        log "Tunnel URL: $url  (no .env to update)"
    fi
}

# TUNNEL_PROVIDER=ngrok (default): the agent is yours to start and keep running,
# so this only reads its current URL.
sync_tunnel_ngrok() {
    # Local API of the running ngrok agent. A second agent on the same machine
    # takes 4041 — point NGROK_API_PORT at whichever one forwards to this proxy.
    local api_port="${NGROK_API_PORT:-4040}"
    local api="http://127.0.0.1:$api_port/api/tunnels"
    # The port the tunnel should be forwarding to, used for the sanity check below.
    local want_port=6674
    [[ "$USE_LOCAL" == "true" ]] && want_port=6664

    log "Reading the ngrok URL from the agent API on :$api_port ..."
    local body
    body=$(curl -sf --max-time 3 "$api" 2>/dev/null || true)

    if [[ -z "$body" ]]; then
        if [[ "$USE_TUNNEL" == "true" ]]; then
            die "No ngrok agent answering on $api.\n  Start one:  ngrok http $want_port\n  Running on another API port (a second agent uses 4041)? Set NGROK_API_PORT.\n  Running it in Docker? Publish its port 4040 to the host."
        fi
        # Otherwise the only symptom is the /info wait timing out, which does not
        # point at the tunnel at all.
        log "NOTE: no ngrok agent on $api and --tunnel not passed — EXT_PROXY_URL must"
        log "      already be reachable by Flare, or the proxy wait below times out."
        return
    fi
    TUNNEL_ACTIVE=true

    local url
    url=$(printf '%s' "$body" | grep -o '"public_url":"https://[^"]*"' | head -1 \
          | sed 's/.*"public_url":"//; s/"$//' || true)
    [[ -n "$url" ]] || die "The ngrok agent on :$api_port reports no https tunnel.\n  Check it with: curl -s $api"

    # A tunnel forwarding somewhere else would put a live-looking URL in .env that
    # never reaches this proxy — the resulting failure points nowhere near ngrok.
    if ! printf '%s' "$body" | grep -q "\"addr\":\"[^\"]*:$want_port\""; then
        log "WARNING: that tunnel does not look like it forwards to port $want_port."
        log "         EXT_PROXY_URL will point at whatever it does forward to."
    fi

    publish_tunnel_url "$url" "The ngrok agent on :$api_port" "curl -s $api"
}

# TUNNEL_PROVIDER=cloudflared: the tunnel is a container out of
# docker-compose.cloudflared.yaml, so this one is ours to start — but only when
# --tunnel says so, and a container that is already up is reused rather than
# recreated, because recreating it mints a new URL for everyone behind it.
sync_tunnel_cloudflared() {
    local cf_compose="$PROJECT_DIR/docker-compose.cloudflared.yaml"
    [[ -f "$cf_compose" ]] || die "$cf_compose not found — see docs/cloudflared.md"

    # Kept as one array so the messages below can echo the exact command. No
    # -p → project "tunnel", from the compose file's own `name:`.
    local -a cf=(compose -f "$cf_compose")

    if [[ "$USE_LOCAL" == "true" ]]; then
        # Host Go proxy is on 6664: a different origin would recreate the shared
        # tunnel and rotate everyone's URL, so local mode gets its own project.
        cf=(compose -p tunnel-local -f "$cf_compose")
        export TUNNEL_TARGET="http://host.docker.internal:6664"
    elif [[ -n "${TUNNEL_TARGET:-}" ]]; then
        log "NOTE: TUNNEL_TARGET is set — this recreates the shared 'tunnel'"
        log "      container and mints a new URL."
    fi

    if docker "${cf[@]}" ps -q cloudflared 2>/dev/null | grep -q .; then
        log "Cloudflare tunnel already running — reusing it."
    elif [[ "$USE_TUNNEL" == "true" ]]; then
        log "Starting the Cloudflare tunnel..."
        docker "${cf[@]}" up -d || die "Failed to start cloudflared.\n  Check: docker ${cf[*]} logs cloudflared"
    else
        # Otherwise the only symptom is the /info wait timing out, which does not
        # point at the tunnel at all.
        log "NOTE: no tunnel running and --tunnel not passed — EXT_PROXY_URL must"
        log "      already be reachable by Flare, or the proxy wait below times out."
        return
    fi
    TUNNEL_ACTIVE=true

    # Named tunnel has a fixed hostname already in .env — nothing to discover.
    if [[ -n "${TUNNEL_ARGS:-}" ]]; then
        log "Named tunnel up — keeping EXT_PROXY_URL=${EXT_PROXY_URL:-<unset>}"
        return
    fi

    # A restarted container keeps its old logs, so scan only from this start —
    # otherwise tail -1 hands back the previous run's dead hostname.
    local cid started
    local -a logs_cmd=(logs)
    cid=$(docker "${cf[@]}" ps -q cloudflared 2>/dev/null | head -1 || true)
    started=$(docker inspect -f '{{.State.StartedAt}}' "$cid" 2>/dev/null | cut -c1-19 || true)
    [[ -n "$started" ]] && logs_cmd=(logs --since "${started}Z")

    log "Reading the quick-tunnel URL..."
    local url="" i
    for ((i = 0; i < 30; i++)); do
        # `|| true` is load-bearing: grep exits 1 until the URL appears, and
        # pipefail would kill the loop on iteration 1 instead of retrying.
        url=$(docker "${cf[@]}" "${logs_cmd[@]}" cloudflared 2>/dev/null \
              | grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' | tail -1 || true)
        [[ -n "$url" ]] && break
        sleep 1
    done
    [[ -n "$url" ]] || die "cloudflared printed no *.trycloudflare.com URL within 30s.\n  Check: docker ${cf[*]} logs cloudflared"

    publish_tunnel_url "$url" "The Cloudflare tunnel" "docker ${cf[*]} logs cloudflared"
}

sync_tunnel() {
    case "$TUNNEL_PROVIDER" in
        ngrok)       sync_tunnel_ngrok ;;
        cloudflared) sync_tunnel_cloudflared ;;
    esac
}

if [[ "$CHAIN" == "local" ]]; then
    [[ "$USE_TUNNEL" == "true" ]] && log "--tunnel ignored on --chain local (the proxy is reachable on localhost)"
else
    sync_tunnel
fi

# ============================================================
# Docker Compose mode (default)
# ============================================================
if [[ "$USE_LOCAL" == "false" ]]; then
    log "Starting services with Docker Compose..."

    # Dockerfile expects SOURCE_DATE_EPOCH for reproducible builds — see REPRODUCIBILITY.md.
    # Without it, `touch -h -d @${SOURCE_DATE_EPOCH}` in the builder stage fails with "invalid date format '@'".
    if [[ -z "${SOURCE_DATE_EPOCH:-}" ]]; then
        if SOURCE_DATE_EPOCH=$(git -C "$PROJECT_DIR" log -1 --format=%ct 2>/dev/null) && [[ -n "$SOURCE_DATE_EPOCH" ]]; then
            export SOURCE_DATE_EPOCH
        else
            export SOURCE_DATE_EPOCH=0
        fi
    fi
    log "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH"

    # --- Build tee-proxy image locally if no remote registry is configured ---
    # (With REGISTRY set the image is pulled instead — pin such a deployment by
    # digest, REGISTRY/tee-proxy@sha256:..., not by a mutable tag.)
    if [[ -z "${REGISTRY:-}" ]]; then
        # `local/tee-proxy` is a mutable local tag, so its presence says nothing
        # about how the image was built: an image left behind by an earlier
        # USE_LOCAL_SIBLINGS run (built from an arbitrary on-disk checkout) would
        # otherwise be reused by every later "pinned" run, and the digest-pinned
        # proxy/Dockerfile would only ever be used when the tag happens to be
        # absent. So every build stamps provenance labels, and reuse requires
        # them to match the recipe this run would have used.
        PROXY_BUILD_LABEL="extension.proxy.build"
        PROXY_REVISION_LABEL="org.opencontainers.image.revision"

        # Escape hatch for an image you know the origin of; fail-closed default.
        case "${ALLOW_UNVERIFIED_PROXY_IMAGE:-}" in
            1|true|yes|on) ALLOW_UNVERIFIED_PROXY_IMAGE=true ;;
            *)             ALLOW_UNVERIFIED_PROXY_IMAGE=false ;;
        esac

        # What this run's recipe stamps, and therefore what reuse requires. The
        # pinned revision is proxy/Dockerfile's own ARG TEE_PROXY_VERSION (read
        # by lib/versions.sh, cross-checked by check-versions.sh) — no version
        # string is hardcoded here, so bumping the pin forces a rebuild.
        if [[ "$USE_LOCAL_SIBLINGS" == "true" ]]; then
            WANT_PROXY_BUILD="siblings"
            # A sibling checkout moves with every commit there, so its revision
            # is recorded for the operator but not required to match on reuse.
            WANT_PROXY_REVISION=""
            STAMP_PROXY_REVISION="$(git -C "$SIBLINGS_ROOT/tee-proxy" rev-parse HEAD 2>/dev/null || echo unknown)"
        else
            WANT_PROXY_BUILD="pinned"
            WANT_PROXY_REVISION="${TEE_PROXY_VERSION:-unknown}"
            STAMP_PROXY_REVISION="$WANT_PROXY_REVISION"
        fi

        # One label off the existing image. Empty when the image is absent, when
        # it carries no labels at all, or when this label is missing — docker
        # renders a missing entry as "<no value>" and a nil label map as "map[]",
        # neither of which may be mistaken for a value.
        proxy_image_label() {
            local value
            value="$(docker image inspect --format "{{ index .Config.Labels \"$1\" }}" local/tee-proxy 2>/dev/null || true)"
            case "$value" in
                "<no value>"|"map[]") value="" ;;
            esac
            printf '%s' "$value"
        }

        if ! docker image inspect local/tee-proxy >/dev/null 2>&1; then
            if [[ "$USE_LOCAL_SIBLINGS" == "true" ]]; then
                # Dev build: use the on-disk sibling tee-proxy checkout.
                TEE_PROXY_DIR="$SIBLINGS_ROOT/tee-proxy"
                [[ -d "$TEE_PROXY_DIR" ]] || die "USE_LOCAL_SIBLINGS set but tee-proxy repo not present at $TEE_PROXY_DIR.\n  Clone it into $SIBLINGS_ROOT/, or unset USE_LOCAL_SIBLINGS to build the pinned proxy."
                log "Building local/tee-proxy from sibling checkout $TEE_PROXY_DIR (USE_LOCAL_SIBLINGS)..."
                docker build -f "$TEE_PROXY_DIR/Dockerfile" \
                    --label "$PROXY_BUILD_LABEL=$WANT_PROXY_BUILD" \
                    --label "$PROXY_REVISION_LABEL=$STAMP_PROXY_REVISION" \
                    -t local/tee-proxy "$TEE_PROXY_DIR" || die "Failed to build tee-proxy image"
            else
                # Default: pinned, self-contained build from the in-repo Dockerfile
                # (clones tee-proxy at the version in proxy/Dockerfile, matching tools/go.mod).
                PROXY_DOCKERFILE="$PROJECT_DIR/proxy/Dockerfile"
                [[ -f "$PROXY_DOCKERFILE" ]] || die "Image local/tee-proxy not found and proxy Dockerfile missing at $PROXY_DOCKERFILE.\n  Set REGISTRY in .env to pull from a remote registry, restore proxy/Dockerfile, or use USE_LOCAL_SIBLINGS=1 to build from a sibling checkout."
                log "Building local/tee-proxy from $PROXY_DOCKERFILE (pinned $STAMP_PROXY_REVISION, self-cloning)..."
                docker build -f "$PROXY_DOCKERFILE" \
                    --label "$PROXY_BUILD_LABEL=$WANT_PROXY_BUILD" \
                    --label "$PROXY_REVISION_LABEL=$STAMP_PROXY_REVISION" \
                    -t local/tee-proxy "$PROJECT_DIR/proxy" || die "Failed to build tee-proxy image"
            fi
            log "local/tee-proxy image built successfully ($PROXY_BUILD_LABEL=$WANT_PROXY_BUILD, $PROXY_REVISION_LABEL=$STAMP_PROXY_REVISION)"
        else
            HAVE_PROXY_BUILD="$(proxy_image_label "$PROXY_BUILD_LABEL")"
            HAVE_PROXY_REVISION="$(proxy_image_label "$PROXY_REVISION_LABEL")"
            if [[ "$HAVE_PROXY_BUILD" == "$WANT_PROXY_BUILD" ]] \
               && { [[ -z "$WANT_PROXY_REVISION" ]] || [[ "$HAVE_PROXY_REVISION" == "$WANT_PROXY_REVISION" ]]; }; then
                log "local/tee-proxy image already exists, built by the $WANT_PROXY_BUILD recipe (${HAVE_PROXY_REVISION:-no revision recorded}) — reusing it (use 'docker rmi local/tee-proxy' to force rebuild)"
            else
                PROXY_LABELS_HAVE="$PROXY_BUILD_LABEL=${HAVE_PROXY_BUILD:-<unset>}, $PROXY_REVISION_LABEL=${HAVE_PROXY_REVISION:-<unset>}"
                PROXY_LABELS_WANT="$PROXY_BUILD_LABEL=$WANT_PROXY_BUILD, $PROXY_REVISION_LABEL=${WANT_PROXY_REVISION:-<any>}"
                if [[ "$ALLOW_UNVERIFIED_PROXY_IMAGE" == "true" ]]; then
                    log "WARNING: reusing local/tee-proxy although its provenance does not match this run (ALLOW_UNVERIFIED_PROXY_IMAGE)."
                    log "         image has:  $PROXY_LABELS_HAVE"
                    log "         run wants:  $PROXY_LABELS_WANT"
                    log "         The proxy that starts is NOT necessarily the build described by proxy/Dockerfile."
                else
                    die "Image local/tee-proxy exists but was not built by the recipe this run uses, so its contents are unknown.\n  image has:  $PROXY_LABELS_HAVE\n  run wants:  $PROXY_LABELS_WANT\n  An unlabelled or sibling-built image under this tag silently replaces the pinned, digest-pinned build in proxy/Dockerfile, so it is neither reused nor overwritten here.\n  Rebuild it:          docker rmi local/tee-proxy && ./scripts/start-services.sh --chain $CHAIN\n  Or pull a published image:  set REGISTRY in .env\n  Or reuse it anyway:  ALLOW_UNVERIFIED_PROXY_IMAGE=true ./scripts/start-services.sh --chain $CHAIN"
                fi
            fi
        fi
    fi

    COMPOSE_FILES=("-f" "$PROJECT_DIR/docker-compose.yaml")

    # Toggle: swap the pinned self-contained node build for the sibling-based one.
    if [[ "$USE_LOCAL_SIBLINGS" == "true" ]]; then
        log "USE_LOCAL_SIBLINGS — node built from on-disk tee-node via Dockerfile.siblings"
        COMPOSE_FILES+=("-f" "$PROJECT_DIR/docker-compose.siblings.yaml")
    fi

    case "$CHAIN" in
        local) ;;
        coston)
            log "Coston mode — attaching docker-compose.coston.yaml"
            COMPOSE_FILES+=("-f" "$PROJECT_DIR/docker-compose.coston.yaml")
            ;;
        coston2)
            log "Coston2 mode — attaching docker-compose.coston2.yaml"
            COMPOSE_FILES+=("-f" "$PROJECT_DIR/docker-compose.coston2.yaml")
            ;;
    esac

    docker compose "${COMPOSE_FILES[@]}" up -d --build || die "docker compose up failed"

    # Wait for proxy to be ready
    E2E="$SCRIPT_DIR/e2e.sh"
    EXT_PROXY_URL="${EXT_PROXY_URL:-http://localhost:6674}"
    log "Waiting for extension proxy at $EXT_PROXY_URL/info ..."
    "$E2E" wait-for-url "$EXT_PROXY_URL/info" 120

    # Validate EXTENSION_ID is recognized by proxy
    log "Validating EXTENSION_ID against proxy..."
    PROXY_INFO=$(curl -sf "$EXT_PROXY_URL/info" 2>/dev/null || true)
    if [[ -n "$PROXY_INFO" ]]; then
        if ! echo "$PROXY_INFO" | grep -q "$EXTENSION_ID" 2>/dev/null; then
            echo -e "${RED}WARNING: EXTENSION_ID $EXTENSION_ID not found in proxy /info response${NC}" >&2
            echo -e "${RED}The proxy may be filtering for a different extension. Check config.${NC}" >&2
        fi
    fi

    echo ""
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN} Services started (Docker Compose)${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""
    echo -e "${CYAN}Mode${NC}"
    case "$CHAIN" in
        local)   echo "  Local devnet" ;;
        coston)  echo "  Coston testnet (chain_id=16)" ;;
        coston2) echo "  Coston2 testnet (chain_id=114)" ;;
    esac
    echo ""
    echo -e "${CYAN}Services${NC}"
    echo "  redis, ext-proxy, extension-tee"
    if [[ "$TUNNEL_ACTIVE" == "true" && "$TUNNEL_PROVIDER" == "cloudflared" ]]; then
        echo "  cloudflared (tunnel)"
    fi
    echo "  Proxy URL: $EXT_PROXY_URL"
    if [[ "$TUNNEL_ACTIVE" == "true" && "$TUNNEL_PROVIDER" == "ngrok" ]]; then
        echo "             (via your ngrok agent — left running)"
    fi
    echo ""
    echo -e "${CYAN}Commands${NC}"
    echo "  Logs:    docker compose ${COMPOSE_FILES[*]} logs -f"
    echo "  Stop:    ./scripts/stop-services.sh --chain $CHAIN"
    # The cloudflared tunnel survives a plain stop on purpose — stopping it
    # rotates the URL for everything else behind it.
    if [[ "$TUNNEL_ACTIVE" == "true" && "$TUNNEL_PROVIDER" == "cloudflared" ]]; then
        echo "  Stop tunnel too: TUNNEL_PROVIDER=cloudflared ./scripts/stop-services.sh --chain $CHAIN --tunnel"
    fi
    exit 0
fi

# ============================================================
# Local Go process mode (--local)
# ============================================================
# The confidential-space guard and the proxy-config check both ran before the mode
# branch above, so by here PROXY_CONFIG already names a validated config.
log "Starting services as local Go processes (--local)..."

E2E="$SCRIPT_DIR/e2e.sh"
PID_DIR="$PROJECT_DIR/out/pids"
LOG_DIR="$PROJECT_DIR/out/logs"

# --- Build Go binaries (once) so we run the actual binary, not `go run` ---
BIN_DIR="$PROJECT_DIR/out/bin"
mkdir -p "$BIN_DIR"
log "Building Go binaries..."
# start-tee links the Go extension in-process, so it lives in the extension
# module; start-proxy is deployment tooling and lives in tools/.
cd "$EXTENSION_DIR"
go build -o "$BIN_DIR/start-tee" ./cmd/start-tee
cd "$PROJECT_DIR/tools"
go build -o "$BIN_DIR/start-proxy" ./cmd/start-proxy

# --- Start extension TEE ---
log "Starting extension TEE node..."
EXTENSION_ID="$EXTENSION_ID" "$E2E" start ext-tee "$PID_DIR/ext-tee.pid" "$LOG_DIR/ext-tee.log" \
    "$BIN_DIR/start-tee" -extensionID "$EXTENSION_ID"

log "Waiting for extension TEE to initialize..."
sleep 5

# --- Start extension Redis on port 6382 via Docker Compose ---
log "Starting Redis via Docker Compose..."
docker compose -f "$PROJECT_DIR/docker-compose.yaml" up -d redis
log "Waiting for Redis on :6382..."
retries=0
while ! docker compose -f "$PROJECT_DIR/docker-compose.yaml" exec -T redis redis-cli ping > /dev/null 2>&1; do
    retries=$((retries + 1))
    if [ $retries -ge 15 ]; then
        die "Redis container failed to become healthy"
    fi
    sleep 1
done
log "Redis on :6382 ready"

# --- Start extension proxy ---
log "Starting extension proxy..."
PROXY_PRIVATE_KEY="$PROXY_PRIVATE_KEY" PROXY_CONFIG="$PROXY_CFG_PATH" \
    "$E2E" start ext-proxy "$PID_DIR/ext-proxy.pid" "$LOG_DIR/ext-proxy.log" \
    "$BIN_DIR/start-proxy"

cd "$PROJECT_DIR"

# --- Wait for proxy to be ready ---
if [[ "$EXT_PROXY_URL" != *"localhost"* && "$EXT_PROXY_URL" != *"127.0.0.1"* ]]; then
    log "NOTE: EXT_PROXY_URL=$EXT_PROXY_URL (not localhost) — health check targets this URL"
fi
log "Waiting for extension proxy..."
"$E2E" wait-for-url "http://localhost:6664/info" 60

# --- Summary ---
EXT_TEE_PID=$(cat "$PID_DIR/ext-tee.pid" 2>/dev/null || echo "?")
EXT_PROXY_PID=$(cat "$PID_DIR/ext-proxy.pid" 2>/dev/null || echo "?")

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN} Services started (local Go processes)${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo -e "${CYAN}Processes${NC}"
echo "  Extension Redis  Docker container (port 6382)"
echo "  Extension TEE    PID $EXT_TEE_PID"
echo "  Extension Proxy  PID $EXT_PROXY_PID"
echo "  Proxy URL        http://localhost:6664"
echo ""
echo -e "${CYAN}Logs${NC}"
echo "  Redis log        docker compose logs redis"
echo "  TEE log          $LOG_DIR/ext-tee.log"
echo "  Proxy log        $LOG_DIR/ext-proxy.log"
echo ""
echo -e "${CYAN}Commands${NC}"
echo "  Status:  $SCRIPT_DIR/e2e.sh status $PID_DIR"
echo "  Stop:    $SCRIPT_DIR/stop-services.sh --local"
if [[ "$TUNNEL_ACTIVE" == "true" && "$TUNNEL_PROVIDER" == "cloudflared" ]]; then
    echo "  Stop tunnel too: TUNNEL_PROVIDER=cloudflared $SCRIPT_DIR/stop-services.sh --local --tunnel"
fi
