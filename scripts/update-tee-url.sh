#!/usr/bin/env bash
# update-tee-url.sh — repoint the on-chain TEE machine record at a new proxy URL.
#
# Run this whenever the public tunnel URL changes (ngrok / cloudflared restart).
#
# Why it exists: Flare delivers instructions to the URL stored in the on-chain
# machine registry, and that URL is written by exactly one call —
# MachineManager.register(), reached only through PreRegistration() in
# tools/pkg/fccutils/registration.go. RegisterNode() skips PreRegistration
# entirely once getTeeMachine(teeId) comes back with a non-zero teeId: it logs
# "already registered" and requests a fresh attestation instead. So re-running
# post-build.sh after a URL rotation LOOKS like it worked — start-services.sh
# wrote the new EXT_PROXY_URL into .env, every step reports success, the chain
# leg really did succeed — while the registry still points at the dead tunnel.
# Every instruction after that times out at pollAction with nothing listening
# at the registered URL. Re-registering cannot fix it either: the TEE key of a
# long-running container does not change, so neither does its teeId.
#
# This sends MachineManager.updateTeeMachineSettings(teeId, proxyId, url)
# directly — one transaction, no attestation, machine status and ledger
# untouched.
#
# Usage:
#   ./scripts/update-tee-url.sh                     # use EXT_PROXY_URL from .env
#   ./scripts/update-tee-url.sh --url https://...   # set an explicit URL
#   ./scripts/update-tee-url.sh --yes               # skip the confirmation prompt
#
# Inputs (env vars, or .env in the repo root):
#   EXT_PROXY_URL           — the URL to register (default target; --url overrides)
#   DEPLOYMENT_PRIVATE_KEY  — key that signs the tx; must own the machine (REQUIRED)
#   CHAIN                   — local | coston | coston2 (default: from LOCAL_MODE)
#   CHAIN_URL               — chain RPC (default: http://127.0.0.1:8545)
#   ADDRESSES_FILE          — deployed-addresses.json (auto-detected per chain)
#   LOCAL_INFO_URL          — proxy /info used to derive the teeId
#                             (auto-detected: :6674 Docker, :6664 local)
#
# Requires: cast (ships with Foundry, already a prerequisite for forge), curl,
# jq, od. The local proxy has to be running — its /info supplies the TEE public
# key the teeId is derived from. Nothing here starts or stops a tunnel.
#
# The call is owner-only: a key that is not the machine owner reverts with
# OnlyOwner, and the contract rejects an empty URL with InvalidUrl. This script
# checks the owner locally first so neither costs gas.
set -euo pipefail
# This script handles a signing key. `set -e` does not clear xtrace, and bash
# honours SHELLOPTS=xtrace from the environment at startup, so an inherited
# setting (or a `set -x` in a sourced .env) would print the key to the terminal.
set +x
unset BASH_XTRACEFD 2>/dev/null || true

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
# These print chain- and config-derived text. `echo -e` would re-expand a literal
# backslash-033 inside that text into a real escape sequence, letting a hostile
# value move the cursor and rewrite the confirmation screen it is displayed on.
# _fmt therefore expands the one sequence the messages below rely on — \n — and
# leaves every other backslash escape inert.
_fmt() {
    local s="$*"
    printf '%s' "${s//\\n/$'\n'}"
}
log()  { printf '%b[update-tee-url]%b %s\n' "$GREEN" "$NC" "$(_fmt "$@")"; }
warn() { printf '%b[update-tee-url] WARNING:%b %s\n' "$YELLOW" "$NC" "$(_fmt "$@")" >&2; }
die()  { printf '%b[update-tee-url] ERROR:%b %s\n' "$RED" "$NC" "$(_fmt "$@")" >&2; exit 1; }

usage() {
    cat <<'EOF'
Usage: ./scripts/update-tee-url.sh [--url <url>] [--yes]

Repoints the on-chain TEE machine record (MachineManager.updateTeeMachineSettings)
at a new extension-proxy URL. Run it after the ngrok/cloudflared URL rotates:
post-build.sh cannot rewrite the registered URL of an already-registered machine,
so it reports success while the registry keeps pointing at the dead tunnel.

Options:
  --url <url>   URL to register. Default: EXT_PROXY_URL from .env.
                https://host[:port][/path], or http:// for loopback only.
  --yes, -y     Do not prompt before sending the transaction.
  --help, -h    Show this help.

Reads .env and config/extension.env. Needs DEPLOYMENT_PRIVATE_KEY (the machine
owner), a reachable local proxy /info, and cast on PATH. Exits 0 without sending
anything if the registered URL already matches.
EOF
}

# --- Parse flags ---
# Parsed into CLI_* names that the sourced files below are then not allowed to
# overwrite; the working variables are re-assigned from these afterwards.
CLI_URL=""
CLI_URL_GIVEN=false
CLI_ASSUME_YES=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url)      [[ $# -ge 2 ]] || die "--url requires a value"; CLI_URL="$2"; CLI_URL_GIVEN=true; shift 2 ;;
        --url=*)    CLI_URL="${1#--url=}"; CLI_URL_GIVEN=true; shift ;;
        --yes|-y)   CLI_ASSUME_YES=true; shift ;;
        --help|-h)  usage; exit 0 ;;
        *)          usage >&2; echo >&2; die "Unknown argument: $1" ;;
    esac
done

# --- Load .env, then config/extension.env (same order as claim-reward.sh) ---
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

# Both files are sourced, so both can assign any name this script uses — and
# config/extension.env is machine-generated by pre-build.sh and tracked in git.
# Re-assert the command line over them: without this, a value in either file
# silently replaces the URL the operator typed, or sets ASSUME_YES and skips the
# confirmation entirely.
URL_OVERRIDE="$CLI_URL"
URL_GIVEN="$CLI_URL_GIVEN"
ASSUME_YES="$CLI_ASSUME_YES"

# --- Strict, anchored patterns for every value that comes from outside ---
# The proxy /info body, the RPC response, --url and .env are all untrusted:
# nothing derived from them is interpolated into a command, a sed script or a
# file before it has matched one of these. Path characters are deliberately
# narrower than RFC 3986 — no shell metacharacters survive the filter at all.
HOST_RE='[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*'
PORT_RE='(:[0-9]{1,5})?'
PATH_RE='(/[A-Za-z0-9._~/-]*)?'
HTTPS_URL_RE="^https://${HOST_RE}${PORT_RE}${PATH_RE}\$"
LOOPBACK_URL_RE="^http://(localhost|127\.0\.0\.1|\[::1\])${PORT_RE}${PATH_RE}\$"
ADDRESS_RE='^0x[0-9a-fA-F]{40}$'
HEX32_RE='^0x[0-9a-fA-F]{64}$'
HEXDATA_RE='^0x[0-9a-fA-F]*$'

# validate_url <value> <label> — die unless <value> is an acceptable absolute URL.
validate_url() {
    local value="$1" label="$2"
    [[ -n "$value" ]] || die "$label is empty."
    (( ${#value} <= 512 )) || die "$label is longer than 512 characters — refusing."
    if [[ "$value" =~ [[:space:]] || "$value" =~ [[:cntrl:]] ]]; then
        die "$label contains whitespace or control characters — refusing: $(printf '%q' "$value")"
    fi
    if [[ ! "$value" =~ $HTTPS_URL_RE && ! "$value" =~ $LOOPBACK_URL_RE ]]; then
        die "$label is not an acceptable URL: $(printf '%q' "$value")\n  Expected https://host[:port][/path]; http:// is accepted only for localhost / 127.0.0.1 / [::1].\n  Letters, digits, '.', '_', '~', '-' and '/' only — no shell metacharacters, whitespace or credentials."
    fi
    # A relative segment has no business in a base URL, and the char class above
    # would otherwise let one through.
    [[ "$value" != *..* ]] || die "$label contains a '..' path segment: $(printf '%q' "$value")"
}

# validate_address <value> <label> — die unless <value> is 0x + 40 hex digits.
validate_address() {
    local value="$1" label="$2"
    [[ "$value" =~ $ADDRESS_RE ]] \
        || die "$label is not a 20-byte hex address: $(printf '%q' "$value")"
}

# str_to_hex <string> — lowercase hex of the string's bytes (no 0x prefix).
str_to_hex() {
    printf '%s' "$1" | od -An -v -tx1 | tr -d ' \n'
}

# lc <string> — lowercase, for comparing checksummed addresses and hex blobs.
# (tr, not ${x,,}: this has to run under the bash 3.2 that ships with macOS.)
lc() {
    printf '%s' "$1" | LC_ALL=C tr '[:upper:]' '[:lower:]'
}

# hex_to_printable <hex> — render on-chain bytes for display only. Everything
# outside printable ASCII is dropped so a hostile URL in the registry cannot
# emit terminal escapes. Backslash goes too: it survives the printable filter,
# and the display helpers expand \n, so a registry value could otherwise forge
# an extra output line.
hex_to_printable() {
    local hex="$1" esc
    [[ -n "$hex" ]] || { printf '(empty)'; return 0; }
    esc="$(printf '%s' "$hex" | sed 's/../\\x&/g')"
    printf '%b' "$esc" | LC_ALL=C tr -dc '\040-\176' | LC_ALL=C tr -d '\\'
}

# --- Resolve the target URL and the chain, honouring the sibling scripts' vars ---
if [[ "$URL_GIVEN" == true ]]; then
    TARGET_URL="$URL_OVERRIDE"
    [[ -n "$TARGET_URL" ]] || die "--url was given an empty value."
else
    TARGET_URL="${EXT_PROXY_URL:-}"
fi
[[ -n "$TARGET_URL" ]] || die "No URL to register.\n  Pass --url <url>, or set EXT_PROXY_URL in .env (start-services.sh writes it from the running tunnel)."
validate_url "$TARGET_URL" "Target URL"

CHAIN="${CHAIN:-}"
if [[ -z "$CHAIN" ]]; then
    [[ "${LOCAL_MODE:-true}" == "true" ]] && CHAIN="local" || CHAIN="coston2"
fi
CHAIN_URL="${CHAIN_URL:-http://127.0.0.1:8545}"
validate_url "$CHAIN_URL" "CHAIN_URL"

# Signing credential. A keystore is preferred and is used when either of cast's
# own env vars names one: cast then reads the key itself and prompts for the
# passphrase, so it never appears in this process's argv. A raw
# DEPLOYMENT_PRIVATE_KEY still works, but it has to be passed as --private-key,
# which is visible in the process list for the lifetime of the call.
SIGNER_ARGS=()
if [[ -n "${ETH_KEYSTORE_ACCOUNT:-}" || -n "${ETH_KEYSTORE:-}" ]]; then
    USING_KEYSTORE=true
else
    USING_KEYSTORE=false
    [[ -n "${DEPLOYMENT_PRIVATE_KEY:-}" ]] || die "No signing credential.\n  Set ETH_KEYSTORE_ACCOUNT (see: cast wallet import --help), or DEPLOYMENT_PRIVATE_KEY."
    PRIVATE_KEY="0x${DEPLOYMENT_PRIVATE_KEY#0x}"
    [[ "$PRIVATE_KEY" =~ $HEX32_RE ]] || die "DEPLOYMENT_PRIVATE_KEY is not a 32-byte hex key."
    SIGNER_ARGS=(--private-key "$PRIVATE_KEY")
fi

if [[ "$CHAIN" != "local" && "$TARGET_URL" =~ $LOOPBACK_URL_RE ]]; then
    warn "Target URL is a loopback address but CHAIN=$CHAIN. Flare's infrastructure cannot reach it — you probably meant the tunnel URL."
fi

# --- Auto-detect the addresses file, same rule as post-build.sh ---
ADDRESSES_FILE="${ADDRESSES_FILE:-}"
if [[ -n "$ADDRESSES_FILE" && "$ADDRESSES_FILE" != /* ]]; then
    ADDRESSES_FILE="$PROJECT_DIR/$ADDRESSES_FILE"
fi
if [[ -z "$ADDRESSES_FILE" ]]; then
    case "$CHAIN" in
        coston)  candidate="$PROJECT_DIR/config/coston/deployed-addresses.json" ;;
        coston2) candidate="$PROJECT_DIR/config/coston2/deployed-addresses.json" ;;
        *)       candidate="" ;;
    esac
    if [[ -n "$candidate" && -f "$candidate" ]]; then
        ADDRESSES_FILE="$candidate"
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
ADDRESSES_FILE="$(cd "$(dirname "$ADDRESSES_FILE")" && pwd)/$(basename "$ADDRESSES_FILE")"

# --- Tools ---
for tool in cast curl jq od sed tr; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool not found on PATH.\n  cast ships with Foundry (see Prerequisites in README.md); the rest are standard."
done

# --- The diamond address comes from the addresses file, never hardcoded ---
MANAGER="$(jq -r '[.[] | select(.name == "FlareTeeManager") | .address] | first // empty' "$ADDRESSES_FILE")" \
    || die "Failed to read $ADDRESSES_FILE"
[[ -n "$MANAGER" ]] || die "No FlareTeeManager entry in $ADDRESSES_FILE"
validate_address "$MANAGER" "FlareTeeManager address from $ADDRESSES_FILE"

# --- Derive the teeId from the local proxy's /info ---
# Same derivation as fccutils.TeeProxyId: the teeId is the EVM address of the
# TEE's secp256k1 public key, i.e. keccak256(X || Y) truncated to its low 20
# bytes. Read from the LOCAL proxy, not the tunnel — the tunnel is the thing
# that just died.
if [[ -z "${LOCAL_INFO_URL:-}" ]]; then
    if docker compose -f "$PROJECT_DIR/docker-compose.yaml" ps ext-proxy --status running 2>/dev/null | grep -q ext-proxy; then
        LOCAL_INFO_URL="http://localhost:6674/info"
    else
        LOCAL_INFO_URL="http://localhost:6664/info"
    fi
fi
validate_url "$LOCAL_INFO_URL" "LOCAL_INFO_URL"

INFO_JSON="$(curl -sf -m 10 "$LOCAL_INFO_URL")" \
    || die "Cannot read the TEE public key from $LOCAL_INFO_URL.\n  Start the proxy first (./scripts/start-services.sh --chain $CHAIN), or set LOCAL_INFO_URL."

PUBKEY_X="$(printf '%s' "$INFO_JSON" | jq -r '.teeInfo.publicKey.x // empty')"
PUBKEY_Y="$(printf '%s' "$INFO_JSON" | jq -r '.teeInfo.publicKey.y // empty')"
[[ "$PUBKEY_X" =~ $HEX32_RE ]] || die "teeInfo.publicKey.x from $LOCAL_INFO_URL is not a 32-byte hex value: $(printf '%q' "$PUBKEY_X")"
[[ "$PUBKEY_Y" =~ $HEX32_RE ]] || die "teeInfo.publicKey.y from $LOCAL_INFO_URL is not a 32-byte hex value: $(printf '%q' "$PUBKEY_Y")"

PUBKEY_HASH="$(cast keccak "0x${PUBKEY_X#0x}${PUBKEY_Y#0x}")" || die "cast keccak failed"
[[ "$PUBKEY_HASH" =~ $HEX32_RE ]] || die "cast keccak returned an unexpected value: $(printf '%q' "$PUBKEY_HASH")"
TEE_ID="0x${PUBKEY_HASH: -40}"
validate_address "$TEE_ID" "Derived teeId"

# --- Read the current machine record ---
# getTeeMachine returns a (address,address,string) tuple. The signature is
# passed without return types so cast hands back the raw ABI bytes, which we
# decode here: cast's human-readable tuple rendering has changed between
# releases, and comparing the URL as bytes makes the idempotence check exact.
RAW="$(cast call "$MANAGER" "getTeeMachine(address)" "$TEE_ID" --rpc-url "$CHAIN_URL" 2>&1)" \
    || die "getTeeMachine($TEE_ID) failed against $CHAIN_URL:\n  $RAW\n  A TeeNotFound revert means the machine was never registered — run ./scripts/post-build.sh first."
[[ "$RAW" =~ $HEXDATA_RE ]] || die "Unexpected getTeeMachine response: $(printf '%q' "$RAW")"

HEXDATA="${RAW#0x}"
(( ${#HEXDATA} % 64 == 0 )) || die "getTeeMachine response is not a whole number of ABI words (${#HEXDATA} hex digits)"
(( ${#HEXDATA} >= 320 )) || die "getTeeMachine response is too short to be a (address,address,string) tuple — machine may not be registered. Run ./scripts/post-build.sh first."

# abi_word <index> — hex digits of the n-th 32-byte word.
abi_word() { local i="$1"; printf '%s' "${HEXDATA:$((i * 64)):64}"; }
# abi_uint <word> — a small unsigned integer, refusing anything above 2^32-1.
abi_uint() {
    [[ "$1" =~ ^0{56}[0-9a-fA-F]{8}$ ]] || die "ABI word out of the expected range in the getTeeMachine response"
    echo "$((16#${1:56:8}))"
}
# abi_address <word> — a left-padded address word.
abi_address() {
    [[ "$1" =~ ^0{24}[0-9a-fA-F]{40}$ ]] || die "ABI word is not a left-padded address in the getTeeMachine response"
    echo "0x${1:24:40}"
}

# decode_machine_url <hexdata> — the url field of a getTeeMachine tuple, as
# lowercase hex. Used for the before-comparison and again after the send, so both
# compare the same decoded value rather than one of them scanning raw bytes.
decode_machine_url() {
    local hexdata="$1" url_offset url_len_pos url_len
    url_offset="$(abi_uint "${hexdata:$((3 * 64)):64}")"
    # The tuple is (address, address, string), so the string data always begins
    # after those three words. Accepting any in-bounds offset would let a hostile
    # RPC point the decode at a shifted region and steer the comparisons below.
    (( url_offset == 96 )) || die "Unexpected URL offset in the getTeeMachine response"
    url_len_pos=$(( 64 + url_offset * 2 ))
    (( url_len_pos + 64 <= ${#hexdata} )) || die "URL offset points past the end of the getTeeMachine response"
    url_len="$(abi_uint "${hexdata:$url_len_pos:64}")"
    (( url_len <= 512 )) || die "On-chain URL claims to be $url_len bytes — refusing to decode."
    (( url_len_pos + 64 + url_len * 2 <= ${#hexdata} )) || die "URL length runs past the end of the getTeeMachine response"
    lc "${hexdata:$((url_len_pos + 64)):$((url_len * 2))}"
}

# Word 0 is the offset of the returned tuple; the tuple is dynamic (it holds a
# string), so it is 0x20 for any well-formed response.
[[ "$(abi_uint "$(abi_word 0)")" == "32" ]] || die "Unexpected tuple offset in the getTeeMachine response"
CURRENT_TEE_ID="$(abi_address "$(abi_word 1)")"
CURRENT_PROXY_ID="$(abi_address "$(abi_word 2)")"
URL_OFFSET="$(abi_uint "$(abi_word 3)")"        # relative to the start of the tuple (word 1)

if [[ "$CURRENT_TEE_ID" == "0x0000000000000000000000000000000000000000" ]]; then
    die "Machine $TEE_ID is not registered on $CHAIN (getTeeMachine returned an empty record).\n  Run ./scripts/post-build.sh to register it — this script only repoints an existing record."
fi
[[ "$(lc "$CURRENT_TEE_ID")" == "$(lc "$TEE_ID")" ]] \
    || die "getTeeMachine returned a different teeId ($CURRENT_TEE_ID) than requested ($TEE_ID) — refusing to write."
validate_address "$CURRENT_PROXY_ID" "On-chain teeProxyId"

CURRENT_URL_HEX="$(decode_machine_url "$HEXDATA")"
TARGET_URL_HEX="$(lc "$(str_to_hex "$TARGET_URL")")"

# --- Idempotent: identical bytes means there is nothing to pay for ---
if [[ "$CURRENT_URL_HEX" == "$TARGET_URL_HEX" ]]; then
    log "On-chain URL already matches — nothing to do."
    log "  machine: $TEE_ID"
    log "  url:     $TARGET_URL"
    exit 0
fi

# --- The signer has to be the machine owner, or updateTeeMachineSettings reverts ---
if [[ "$USING_KEYSTORE" == true ]]; then
    warn "Using the keystore cast was pointed at; it will prompt for the passphrase (twice: owner check, then send)."
else
    warn "DEPLOYMENT_PRIVATE_KEY is passed to cast as --private-key, so it is visible in this host's process list while each call runs.
  To avoid that:  cast wallet import <name> --interactive   then   export ETH_KEYSTORE_ACCOUNT=<name>"
fi
SENDER="$(cast wallet address ${SIGNER_ARGS[@]+"${SIGNER_ARGS[@]}"} 2>/dev/null)" \
    || die "cast could not derive an address from the signing credential."
validate_address "$SENDER" "Sender address"

OWNER_RAW="$(cast call "$MANAGER" "getTeeMachineOwner(address)" "$TEE_ID" --rpc-url "$CHAIN_URL" 2>&1)" \
    || die "getTeeMachineOwner($TEE_ID) failed against $CHAIN_URL:\n  $OWNER_RAW"
[[ "$OWNER_RAW" =~ $HEXDATA_RE && ${#OWNER_RAW} -eq 66 ]] \
    || die "Unexpected getTeeMachineOwner response: $(printf '%q' "$OWNER_RAW")"
OWNER="$(abi_address "${OWNER_RAW#0x}")"
[[ "$(lc "$OWNER")" == "$(lc "$SENDER")" ]] \
    || die "DEPLOYMENT_PRIVATE_KEY ($SENDER) does not own machine $TEE_ID (owner: $OWNER).\n  updateTeeMachineSettings is owner-only and would revert with OnlyOwner. Use the owner's key."

# --- Show the change and confirm ---
echo ""
echo -e "${CYAN}=== updateTeeMachineSettings ===${NC}"
log "FlareTeeManager: $MANAGER"
# Name the file the address came from. ADDRESSES_FILE is auto-detected when it is
# not set, and the search reaches directories outside this repo, so "which
# contract am I about to write to" is only answerable if the source is shown.
log "  from:          $ADDRESSES_FILE"
log "Chain / RPC:     $CHAIN / $CHAIN_URL"
log "Machine (teeId): $TEE_ID"
log "Proxy (proxyId): $CURRENT_PROXY_ID  (unchanged)"
log "Sender / owner:  $SENDER"
log "Registered URL:  $(hex_to_printable "$CURRENT_URL_HEX")"
log "New URL:         $TARGET_URL"
echo ""

if [[ "$ASSUME_YES" != true ]]; then
    if [[ ! -t 0 ]]; then
        die "Refusing to send a transaction unconfirmed on a non-interactive stdin. Re-run in a terminal, or pass --yes."
    fi
    echo -en "${CYAN}[update-tee-url]${NC} Send this transaction? [y/N] "
    read -r reply
    case "$reply" in
        y|Y|yes|Yes) ;;
        *) die "Aborted — nothing sent." ;;
    esac
fi

# --- Send ---
SEND_OUT="$(cast send "$MANAGER" "updateTeeMachineSettings(address,address,string)" \
    "$TEE_ID" "$CURRENT_PROXY_ID" "$TARGET_URL" \
    ${SIGNER_ARGS[@]+"${SIGNER_ARGS[@]}"} --rpc-url "$CHAIN_URL" 2>&1)" \
    || die "updateTeeMachineSettings reverted:\n  $SEND_OUT\n  OnlyOwner means the wrong key; InvalidUrl means the contract rejected the URL itself."
log "Transaction sent."

# --- Verify the registry really changed ---
VERIFY_RAW="$(cast call "$MANAGER" "getTeeMachine(address)" "$TEE_ID" --rpc-url "$CHAIN_URL" 2>&1)" \
    || die "Update sent, but re-reading getTeeMachine failed:\n  $VERIFY_RAW"
[[ "$VERIFY_RAW" =~ $HEXDATA_RE ]] || die "Unexpected getTeeMachine response after the update: $(printf '%q' "$VERIFY_RAW")"
# Decode and compare exactly. A substring test over the raw response would pass
# whenever the registered URL merely CONTAINS the target — registering
# https://x.ngrok.io while the record still reads https://x.ngrok.io/api would
# report success on an unchanged registry, which is the silent failure this
# script exists to catch.
VERIFY_HEXDATA="${VERIFY_RAW#0x}"
(( ${#VERIFY_HEXDATA} % 64 == 0 && ${#VERIFY_HEXDATA} >= 256 )) \
    || die "Unexpected getTeeMachine response after the update: not a whole number of ABI words."
VERIFIED_URL_HEX="$(decode_machine_url "$VERIFY_HEXDATA")"
if [[ "$VERIFIED_URL_HEX" != "$TARGET_URL_HEX" ]]; then
    die "The transaction was accepted but the registry still reads a different URL:\n  on-chain now: $(hex_to_printable "$VERIFIED_URL_HEX")\n  expected:     $TARGET_URL"
fi

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN} On-chain machine URL updated${NC}"
echo -e "${GREEN}========================================${NC}"
log "$TEE_ID -> $TARGET_URL"
if [[ "${EXT_PROXY_URL:-}" != "$TARGET_URL" ]]; then
    warn "EXT_PROXY_URL in .env is '${EXT_PROXY_URL:-<unset>}', not the URL just registered. Update it so the local scripts agree with the registry."
fi
