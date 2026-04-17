#!/usr/bin/env bash
# claim-reward.sh — Run the caller-side reward flow end to end.
#
#   fetch the TEE → seal + encrypt the Strava token → getDistanceProof
#   → poll the proxy for the signed proof → claimReward
#
# Unlike test.sh this asserts nothing and configures nothing: no setExtensionId,
# no contract funding, no tampered-proof check. Run it AFTER post-build.sh.
#
# Inputs (env vars, or .env in the repo root):
#   STRAVA_TOKEN            — Strava access token with activity:read_all (REQUIRED)
#   DEPLOYMENT_PRIVATE_KEY  — key that sends the tx and receives the reward (REQUIRED)
#   EXT_PROXY_URL           — extension proxy (auto-detected: :6674 Docker, :6664 local)
#   CHAIN_URL               — chain RPC (default: http://127.0.0.1:8545)
#   ADDRESSES_FILE          — deployed-addresses.json (auto-detected)
#   INSTRUCTION_SENDER      — from config/extension.env
#
# Usage:
#   ./scripts/claim-reward.sh                  # request a proof and claim
#   ./scripts/claim-reward.sh --no-claim       # just read the distance, don't claim
#   ./scripts/claim-reward.sh --json           # also dump the raw proof
#   ./scripts/claim-reward.sh -- -ttl 300      # pass anything else through to the tool
#
# A freshly deployed contract also needs setExtensionId() once — nothing else in
# scripts/ does it. Pass --set-extension-id on the first run only; it is one-shot
# and only a redeploy can change it afterwards.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
log() { echo -e "${GREEN}[claim-reward]${NC} $*"; }
die() { echo -e "${RED}[claim-reward] ERROR:${NC} $*" >&2; exit 1; }

# --- Parse flags (everything after `--` goes straight to the Go tool) ---
EXTRA_ARGS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --no-claim) EXTRA_ARGS+=(-claim=false); shift ;;
        --json)     EXTRA_ARGS+=(-json); shift ;;
        --set-extension-id) EXTRA_ARGS+=(-set-extension-id); shift ;;
        --)         shift; EXTRA_ARGS+=("$@"); break ;;
        *)          die "Unknown argument: $1 (use -- to pass flags through)" ;;
    esac
done

# --- Load .env, then config/extension.env (which supplies INSTRUCTION_SENDER) ---
if [[ -f "$PROJECT_DIR/.env" ]]; then
    set -a
    # shellcheck disable=SC1091
    source "$PROJECT_DIR/.env"
    set +a
fi
CONFIG_FILE="$PROJECT_DIR/config/extension.env"
if [[ -f "$CONFIG_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$CONFIG_FILE"
fi

[[ -n "${STRAVA_TOKEN:-}" ]] || die "STRAVA_TOKEN not set.\n  export STRAVA_TOKEN=... (needs activity:read_all; tokens expire after 6 hours)"
[[ -n "${DEPLOYMENT_PRIVATE_KEY:-}" ]] || die "DEPLOYMENT_PRIVATE_KEY not set — it signs the tx and receives the reward."
[[ -n "${INSTRUCTION_SENDER:-}" ]] || die "INSTRUCTION_SENDER not set. Run pre-build.sh first, or set it manually."

# --- Auto-detect the proxy port, same rule as test.sh ---
# Docker publishes 6674; the --local Go process listens on 6664.
if [[ -z "${EXT_PROXY_URL:-}" ]]; then
    if docker compose -f "$PROJECT_DIR/docker-compose.yaml" ps ext-proxy --status running 2>/dev/null | grep -q ext-proxy; then
        EXT_PROXY_URL="http://localhost:6674"
    else
        EXT_PROXY_URL="http://localhost:6664"
    fi
fi
CHAIN_URL="${CHAIN_URL:-http://127.0.0.1:8545}"

# --- Auto-detect the addresses file, same rule as test.sh ---
ADDRESSES_FILE="${ADDRESSES_FILE:-}"
if [[ -n "$ADDRESSES_FILE" && "$ADDRESSES_FILE" != /* ]]; then
    ADDRESSES_FILE="$PROJECT_DIR/$ADDRESSES_FILE"
fi
if [[ -z "$ADDRESSES_FILE" ]]; then
    if [[ "${LOCAL_MODE:-true}" != "true" ]]; then
        candidate="$PROJECT_DIR/config/coston2/deployed-addresses.json"
        [[ -f "$candidate" ]] && ADDRESSES_FILE="$candidate"
    fi
    if [[ -z "$ADDRESSES_FILE" ]]; then
        for candidate in \
            "$PROJECT_DIR/../../e2e/docker/sim_dump/deployed-addresses.json" \
            "$PROJECT_DIR/../docker/sim_dump/deployed-addresses.json" \
            "$PROJECT_DIR/../../docker/sim_dump/deployed-addresses.json" \
            "$PROJECT_DIR/../../../docker/sim_dump/deployed-addresses.json"; do
            [[ -f "$candidate" ]] && { ADDRESSES_FILE="$candidate"; break; }
        done
    fi
    [[ -n "$ADDRESSES_FILE" ]] || die "Cannot find deployed-addresses.json. Set ADDRESSES_FILE."
fi
[[ -f "$ADDRESSES_FILE" ]] || die "Addresses file not found: $ADDRESSES_FILE"
# Absolute, so it still resolves after cd into tools/
ADDRESSES_FILE="$(cd "$(dirname "$ADDRESSES_FILE")" && pwd)/$(basename "$ADDRESSES_FILE")"

# --- Pre-flight: the proxy has to answer before we encrypt anything to it ---
if ! curl -sf -o /dev/null "$EXT_PROXY_URL/info" 2>/dev/null; then
    die "Extension proxy not reachable at $EXT_PROXY_URL.\n  Start services (./scripts/start-services.sh --chain \${CHAIN:-coston2} --tunnel),\n  or refresh EXT_PROXY_URL if the ngrok URL rotated."
fi
log "Proxy reachable at $EXT_PROXY_URL"

cd "$PROJECT_DIR/tools"
exec go run ./cmd/claim-reward \
    -a "$ADDRESSES_FILE" \
    -c "$CHAIN_URL" \
    -p "$EXT_PROXY_URL" \
    -contract "$INSTRUCTION_SENDER" \
    ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
