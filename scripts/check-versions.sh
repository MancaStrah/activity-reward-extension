#!/usr/bin/env bash
#
# check-versions.sh — fail fast when dependency pins drift apart.
#
# The Go image pins tee-node via go.mod; non-Go images build tee-node from
# source at TEE_NODE_REF. If those diverge, the node and proxy disagree on
# signature formats and the symptom is an opaque "signature check fail" at
# runtime, long after the build. Same for tee-proxy between tools/go.mod and
# proxy/Dockerfile.
#
# Run standalone, or let pre-build.sh invoke it as a pre-flight.
#
# Usage: ./scripts/check-versions.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
log()  { echo -e "${GREEN}[check-versions]${NC} $*"; }
warn() { echo -e "${YELLOW}[check-versions] WARN:${NC} $*" >&2; }
die()  { echo -e "${RED}[check-versions] ERROR:${NC} $*" >&2; exit 1; }

# shellcheck source=lib/versions.sh
source "$SCRIPT_DIR/lib/versions.sh"
load_versions "$PROJECT_DIR" || die "could not load versions"

# Locate the two go.mod files (extension module may be at ./ or ./go/).
if [[ -f "$PROJECT_DIR/go/go.mod" ]]; then
    EXT_GOMOD="$PROJECT_DIR/go/go.mod"
else
    EXT_GOMOD="$PROJECT_DIR/go.mod"
fi
TOOLS_GOMOD="$PROJECT_DIR/tools/go.mod"

pin() { grep -E "^[[:space:]]*${2//\//\\/}[[:space:]]+v" "$1" 2>/dev/null | head -1 | awk '{print $2}'; }

FAILED=0

# --- 1. tee-node must match between the extension module and tools ---
EXT_NODE="$(pin "$EXT_GOMOD" github.com/flare-foundation/tee-node)"
TOOLS_NODE="$(pin "$TOOLS_GOMOD" github.com/flare-foundation/tee-node)"
if [[ -z "$EXT_NODE" ]]; then
    die "no tee-node pin in $EXT_GOMOD"
elif [[ -z "$TOOLS_NODE" ]]; then
    warn "no tee-node pin in $TOOLS_GOMOD — skipping cross-check"
elif [[ "$EXT_NODE" != "$TOOLS_NODE" ]]; then
    echo -e "${RED}  tee-node mismatch:${NC}" >&2
    echo "    $EXT_GOMOD:   $EXT_NODE" >&2
    echo "    $TOOLS_GOMOD: $TOOLS_NODE" >&2
    FAILED=1
else
    log "tee-node       $EXT_NODE (extension == tools)"
fi

# --- 1b. tee-node must not sit below the platform minimum ---
# sort -V puts the lower version first. ponytail: misreads a pseudo-version of
# the floor tag itself; not worth a real semver parser for one comparison.
TEE_NODE_MIN="v0.0.22"
if [[ -n "$EXT_NODE" && "$(printf '%s\n%s\n' "$TEE_NODE_MIN" "$EXT_NODE" | sort -V | head -1)" != "$TEE_NODE_MIN" ]]; then
    echo -e "${RED}  tee-node $EXT_NODE is below the $TEE_NODE_MIN minimum${NC}" >&2
    echo "    bump the pin in $EXT_GOMOD and $TOOLS_GOMOD" >&2
    FAILED=1
fi

# --- 2. go-flare-common must match too (drift here breaks ABI encoding) ---
EXT_COMMON="$(pin "$EXT_GOMOD" github.com/flare-foundation/go-flare-common)"
TOOLS_COMMON="$(pin "$TOOLS_GOMOD" github.com/flare-foundation/go-flare-common)"
if [[ -n "$EXT_COMMON" && -n "$TOOLS_COMMON" && "$EXT_COMMON" != "$TOOLS_COMMON" ]]; then
    echo -e "${RED}  go-flare-common mismatch:${NC}" >&2
    echo "    $EXT_GOMOD:   $EXT_COMMON" >&2
    echo "    $TOOLS_GOMOD: $TOOLS_COMMON" >&2
    FAILED=1
elif [[ -n "$EXT_COMMON" ]]; then
    log "go-flare-common $EXT_COMMON (extension == tools)"
fi

# --- 3. tee-proxy: tools/go.mod must match proxy/Dockerfile's build ARG ---
TOOLS_PROXY="$(pin "$TOOLS_GOMOD" github.com/flare-foundation/tee-proxy)"
if [[ -z "$TEE_PROXY_VERSION" ]]; then
    warn "no ARG TEE_PROXY_VERSION in proxy/Dockerfile — skipping cross-check"
elif [[ -z "$TOOLS_PROXY" ]]; then
    warn "no tee-proxy pin in $TOOLS_GOMOD — skipping cross-check"
elif [[ "$TOOLS_PROXY" != "$TEE_PROXY_VERSION" ]]; then
    echo -e "${RED}  tee-proxy mismatch:${NC}" >&2
    echo "    $TOOLS_GOMOD:        $TOOLS_PROXY" >&2
    echo "    proxy/Dockerfile:    $TEE_PROXY_VERSION" >&2
    FAILED=1
else
    log "tee-proxy      $TOOLS_PROXY (tools == proxy/Dockerfile)"
fi

# --- 4. Report the ref that language images will clone ---
log "TEE_NODE_REF   $TEE_NODE_REF $([[ "$TEE_NODE_REF" != "$TEE_NODE_VERSION" ]] && echo '(commit SHA from pseudo-version)' || echo '(tag)')"

# --- 5. Launch-policy label: docs must match the image ---
# The documented example and go/Dockerfile must agree: an operator who deploys by
# the document rather than the image would otherwise silently change the security
# assumptions of the measured image — for instance by allowing MODE, or by omitting
# CHAIN_ID/GOVERNANCE_*.
# Every allow_env_override value in the file, one per line. Docker applies the
# LAST assignment of a repeated label key, so reading only the first would let a
# second LABEL line further down grant MODE while this check inspected a clean
# decoy above it. Callers below therefore examine all of them, not just one.
_labels_of() {
    sed -n 's/.*"tee.launch_policy.allow_env_override"="\([^"]*\)".*/\1/p' "$1" 2>/dev/null
}
# The effective value: the one Docker ends up applying.
_label_of() {
    _labels_of "$1" | tail -1
}
IMG_LABEL="$(_label_of "$PROJECT_DIR/go/Dockerfile")"
DOC_LABEL="$(_label_of "$PROJECT_DIR/docs/extension-contract.md")"
if [[ -z "$IMG_LABEL" ]]; then
    warn "no launch-policy label in go/Dockerfile — skipping doc cross-check"
elif [[ -z "$DOC_LABEL" ]]; then
    warn "no launch-policy label example in docs/extension-contract.md — skipping doc cross-check"
elif [[ "$IMG_LABEL" != "$DOC_LABEL" ]]; then
    echo -e "${RED}  launch-policy label drift:${NC}" >&2
    echo "    go/Dockerfile:              $IMG_LABEL" >&2
    echo "    docs/extension-contract.md: $DOC_LABEL" >&2
    FAILED=1
else
    log "launch label   docs/extension-contract.md == go/Dockerfile"
fi
# MODE selects the attestation backend, so it must never be launch-overridable:
# whichever image you built fixes the value, and whoever launches it cannot
# change it. Checked on every Dockerfile that ships a label, including the
# dev-only siblings build — that file is what people copy from, so a MODE entry
# there would propagate into a production image whose attestation an operator
# could switch off at launch.
for _df in "$PROJECT_DIR/go/Dockerfile" "$PROJECT_DIR"/go/Dockerfile.*; do
    [[ -f "$_df" ]] || continue
    case "$_df" in *.dockerignore) continue ;; esac
    _rel="${_df#"$PROJECT_DIR/"}"
    # Declaring the label more than once is refused outright: Docker would apply
    # the last one, but anyone reading the file sees the first, so the file
    # would say one thing and the image mean another.
    _label_lines="$(_labels_of "$_df" | grep -c . || true)"
    if [[ "$_label_lines" -gt 1 ]]; then
        echo -e "${RED}  $_rel declares the launch-policy label $_label_lines times — declare it once, or the effective value is not the one being read${NC}" >&2
        FAILED=1
    fi
    # Scan every occurrence, not just the effective one: any line granting MODE
    # is a problem regardless of which one Docker would win with.
    while IFS= read -r _one; do
        [[ -n "$_one" ]] || continue
        if [[ ",$_one," == *",MODE,"* ]]; then
            echo -e "${RED}  MODE is listed in $_rel's launch-policy label — an operator could disable attestation at launch${NC}" >&2
            FAILED=1
        fi
    done < <(_labels_of "$_df")
    # No label at all is safe — nothing is overridable. A label this parser
    # cannot read is NOT: the launcher may still honour it, so treating an
    # extraction failure as "MODE absent" would turn this gate into a no-op
    # exactly when someone reformats the line it inspects.
    if [[ "$_label_lines" -eq 0 ]] && grep -q 'allow_env_override' "$_df"; then
        echo -e "${RED}  $_rel has an allow_env_override label this check cannot parse — reformat it to LABEL \"tee.launch_policy.allow_env_override\"=\"...\" so MODE cannot slip in undetected${NC}" >&2
        FAILED=1
    fi
done

# --- 6. TEE_VERSION vs. the extension version: related, deliberately unequal ---
# Two values that both read like "the version of this extension", but are not
# the same thing:
#
#   TEE_VERSION (.env*.example, scripts/post-build.sh default) is packed into
#   bytes32 and attached to the (extensionId, codeHash, platform) triple by
#   AddTeeVersion. It labels an allow-listed TEE *image* on-chain, and no
#   extension code ever reads it.
#
#   Version in go/internal/config/config.go is the extension's observable
#   contract: hashed into stateVersion on GET /state and /info, and stamped
#   verbatim on every ActionResult. It labels the *wire format*, not an image.
#
# Forcing the numbers equal would therefore be wrong. What must not happen is
# one of them moving because somebody assumed it tracked the other, so the
# acknowledged pairing is declared here: bump either value and this declaration
# has to be updated too, and until it is the warning below names both files.
ACK_TEE_VERSION="v0.1.0"   # as in .env*.example and post-build.sh's default
ACK_EXT_VERSION="0.2.0"    # as in config.Version

# TEE_VERSION out of an env template (inline comments stripped).
_tee_version_in() {
    [[ -r "$1" ]] || return 0
    sed -n 's/^[[:space:]]*TEE_VERSION=\([^[:space:]#]*\).*/\1/p' "$1" | head -1 || true
}
# One "<label>|<value>" row per place TEE_VERSION is spelled out, newline-separated.
TEE_VERSION_ROWS=""
_tee_version_row() { [[ -n "$2" ]] && TEE_VERSION_ROWS+="$1|$2"$'\n'; return 0; }
_tee_version_row ".env.example" \
    "$(_tee_version_in "$PROJECT_DIR/.env.example")"
_tee_version_row ".env.confidential-space.example" \
    "$(_tee_version_in "$PROJECT_DIR/.env.confidential-space.example")"
_tee_version_row "scripts/post-build.sh (default)" \
    "$(sed -n 's/^[[:space:]]*TEE_VERSION="\${TEE_VERSION:-\([^}"]*\)}".*/\1/p' \
        "$PROJECT_DIR/scripts/post-build.sh" 2>/dev/null | head -1 || true)"

CONFIG_GO="$PROJECT_DIR/go/internal/config/config.go"
EXT_VERSION=""
if [[ -r "$CONFIG_GO" ]]; then
    EXT_VERSION="$(sed -n 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
        "$CONFIG_GO" | head -1 || true)"
fi

_print_tee_version_rows() {
    printf '%s' "$TEE_VERSION_ROWS" | while IFS='|' read -r src val; do
        [[ -n "$src" ]] || continue
        printf '    %-34s %s\n' "$src:" "$val"
    done
}

if [[ -z "$TEE_VERSION_ROWS" ]]; then
    warn "no TEE_VERSION found in .env*.example or scripts/post-build.sh — skipping extension-version cross-check"
elif [[ -z "$EXT_VERSION" ]]; then
    warn "no Version constant in go/internal/config/config.go — skipping TEE_VERSION cross-check"
else
    TEE_VERSION_VALUE="$(printf '%s' "$TEE_VERSION_ROWS" | head -1 | cut -d'|' -f2)"
    TEE_VERSION_DISTINCT="$(printf '%s' "$TEE_VERSION_ROWS" | cut -d'|' -f2 | sort -u | wc -l | tr -d '[:space:]')"
    if [[ "$TEE_VERSION_DISTINCT" -ne 1 ]]; then
        # One concept spelled out in several files: these really must be equal,
        # or post-build allow-lists a version the templates never mention.
        echo -e "${RED}  TEE_VERSION disagrees between the templates and post-build's default:${NC}" >&2
        _print_tee_version_rows >&2
        FAILED=1
    elif [[ "$TEE_VERSION_VALUE" != "$ACK_TEE_VERSION" || "$EXT_VERSION" != "$ACK_EXT_VERSION" ]]; then
        warn "the TEE_VERSION / extension-version pair moved. They are different things (on-chain image label vs. wire contract), so neither has to follow the other — confirm the change was deliberate and update ACK_TEE_VERSION / ACK_EXT_VERSION in scripts/check-versions.sh."
        echo "    TEE_VERSION — on-chain image label consumed by allow-tee-version:" >&2
        _print_tee_version_rows >&2
        echo "    extension Version — hashed into stateVersion, stamped on ActionResult:" >&2
        printf '    %-34s %s\n' "go/internal/config/config.go:" "$EXT_VERSION" >&2
        printf '    %-34s %s\n' "acknowledged pair:" "TEE_VERSION $ACK_TEE_VERSION / extension $ACK_EXT_VERSION" >&2
    else
        log "TEE_VERSION    $TEE_VERSION_VALUE (on-chain image label) vs extension $EXT_VERSION (state+result contract) — distinct by design, pair acknowledged"
    fi
fi

if [[ "$FAILED" -ne 0 ]]; then
    echo "" >&2
    die "version pins are inconsistent. Align them before building, or the Go and non-Go images will run different tee-node builds."
fi

log "all version pins consistent"
