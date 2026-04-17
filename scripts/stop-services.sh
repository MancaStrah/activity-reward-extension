#!/usr/bin/env bash
#
# Stop extension services.
#
# By default, stops Docker Compose services, picking the compose overlay from
# --chain (or env CHAIN, or legacy LOCAL_MODE):
#   --chain local    → docker-compose.yaml only
#   --chain coston   → + docker-compose.coston.yaml
#   --chain coston2  → + docker-compose.coston2.yaml
#
# Pass --local to stop background Go processes instead.
#
# What --tunnel does depends on TUNNEL_PROVIDER, the same way it does in
# start-services.sh:
#
#   ngrok (default)  nothing — the agent is started outside these scripts and
#                    stays up, because stopping it rotates the URL for anything
#                    else pointed at it.
#   cloudflared      stops the tunnel container (docker-compose.cloudflared.yaml)
#                    after everything behind it. Without --tunnel it is left
#                    running: other extensions reuse the same container, and
#                    stopping it rotates their URL.
#
# Usage:
#   ./scripts/stop-services.sh                       # local devnet, docker compose
#   ./scripts/stop-services.sh --chain coston        # Coston, docker compose
#   ./scripts/stop-services.sh --local               # background Go processes
#   TUNNEL_PROVIDER=cloudflared ./scripts/stop-services.sh --chain coston2 --tunnel   # also stop the tunnel
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
log()  { echo -e "${GREEN}[stop-services]${NC} $*"; }
die()  { echo -e "${RED}[stop-services] ERROR:${NC} $*" >&2; exit 1; }

# --- Parse flags ---
USE_LOCAL=false
USE_TUNNEL=false
CHAIN="${CHAIN:-}"
# Kept separate from CHAIN: `set -a; source .env` below re-exports whatever CHAIN
# the file sets, so a value assigned here would be silently overwritten and
# `--chain local` would tear down the wrong compose project. Re-applied after.
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

[[ -n "$CHAIN_FROM_FLAG" ]] && CHAIN="$CHAIN_FROM_FLAG"

LOCAL_MODE="${LOCAL_MODE:-true}"

# --- Resolve CHAIN (flag > env > legacy LOCAL_MODE) ---
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

# --- Resolve the tunnel provider (mirrors start-services.sh) ------------------
# Only cloudflared runs as a container these scripts own; ngrok is your agent.
TUNNEL_PROVIDER="${TUNNEL_PROVIDER:-ngrok}"
case "$TUNNEL_PROVIDER" in
    ngrok|cloudflared) ;;
    *) die "Unknown TUNNEL_PROVIDER: $TUNNEL_PROVIDER (valid: ngrok, cloudflared)" ;;
esac

if [[ "$USE_LOCAL" == "true" ]]; then
    # --- Stop background Go processes ---
    E2E="$SCRIPT_DIR/e2e.sh"
    PID_DIR="$PROJECT_DIR/out/pids"

    log "Stopping background Go processes..."
    "$E2E" stop-all "$PID_DIR"
else
    # --- Stop Docker Compose services ---
    COMPOSE_FILES=("-f" "$PROJECT_DIR/docker-compose.yaml")

    # Mirror start-services.sh: include the siblings overlay when active so
    # compose resolves the same project/services that were started.
    case "${USE_LOCAL_SIBLINGS:-}" in
        1|true|yes|on) COMPOSE_FILES+=("-f" "$PROJECT_DIR/docker-compose.siblings.yaml") ;;
    esac

    case "$CHAIN" in
        local) ;;
        coston)  COMPOSE_FILES+=("-f" "$PROJECT_DIR/docker-compose.coston.yaml") ;;
        coston2) COMPOSE_FILES+=("-f" "$PROJECT_DIR/docker-compose.coston2.yaml") ;;
    esac

    # docker-compose.yaml interpolates SOURCE_DATE_EPOCH as a build arg. It's
    # irrelevant on `down`, but compose still warns when it's unset — silence it.
    export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}"

    log "Stopping Docker Compose services (chain: $CHAIN)..."
    docker compose "${COMPOSE_FILES[@]}" down
fi

# Stopped last and only with --tunnel: whatever was behind the tunnel goes down
# first, and other extensions reuse this container, so tearing it down rotates
# their URL too.
if [[ "$TUNNEL_PROVIDER" == "cloudflared" ]]; then
    CF_COMPOSE="$PROJECT_DIR/docker-compose.cloudflared.yaml"
    if [[ -f "$CF_COMPOSE" ]]; then
        # Kept as one array so it is never empty under `set -u`. Mirrors
        # start-services.sh: --local ran its own tunnel project, aimed at the
        # host proxy on 6664 instead of the container's published port.
        CF=(compose -f "$CF_COMPOSE")
        [[ "$USE_LOCAL" == "true" ]] && CF=(compose -p tunnel-local -f "$CF_COMPOSE")
        if [[ "$USE_TUNNEL" == "true" ]]; then
            if docker "${CF[@]}" ps -q cloudflared 2>/dev/null | grep -q .; then
                log "Stopping the Cloudflare tunnel (last)..."
                docker "${CF[@]}" down || log "WARNING: failed to stop cloudflared"
            else
                log "No tunnel running — nothing to stop."
            fi
        elif docker "${CF[@]}" ps -q cloudflared 2>/dev/null | grep -q .; then
            log "Leaving the Cloudflare tunnel running (pass --tunnel to stop it)."
        fi
    elif [[ "$USE_TUNNEL" == "true" ]]; then
        log "--tunnel: $CF_COMPOSE not found — no cloudflared tunnel to stop."
    fi
elif [[ "$USE_TUNNEL" == "true" ]]; then
    # The ngrok agent is started outside these scripts, so it is not ours to
    # kill — and killing it would rotate the URL for everything else using it.
    log "--tunnel: the ngrok agent is not managed here — stop it yourself if you want it down."
    log "          (TUNNEL_PROVIDER=cloudflared is the tunnel these scripts do stop.)"
fi

log "Done."
