#!/usr/bin/env bash
# check-docs.sh — validate the shared docs standard (see docs/README.md).
#
# Checks presence, that the platform-wide traps are actually covered, and that
# nothing has grown too long to be read by a tester. Content checks match on
# keywords rather than headings so rewording does not break them.
#
# Keep this file identical across extensions; the per-repo differences are
# ONE_SHOT_SETTER, REQUIRED and OPTIONAL below. Here:
#   - REQUIRED adds ngrok.md and cloudflared.md: both are supported tunnel
#     providers (TUNNEL_PROVIDER), and ngrok is the default, so an operator
#     following the walkthrough needs the ngrok page to exist.
#   - languages.md and manual-setup.md are NOT listed: this repo ships the Go
#     implementation only and has no manual-setup path, so requiring them would
#     ask for docs that describe nothing.
#   - types-server.md is OPTIONAL, not required. This repo DOES have a
#     types-server (go/cmd/types-server, port 8100) — it is described in the
#     repo README's "Types Server" section and exercised by
#     scripts/test-types-server.sh — so the standalone doc is a real gap and the
#     check warns about it rather than pretending it does not apply.
#
# Exit 1 on a missing/incomplete required doc; long docs only warn.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DOCS="$(cd "$SCRIPT_DIR/.." && pwd)/docs"

RED='\033[0;31m'; YELLOW='\033[0;33m'; GREEN='\033[0;32m'; NC='\033[0m'
fail=0
err()  { echo -e "${RED}FAIL${NC}  $*"; fail=1; }
warn() { echo -e "${YELLOW}WARN${NC}  $*"; }
ok()   { echo -e "${GREEN}ok${NC}    $*"; }

# The one-shot binding differs per extension: here it is the contract's
# owner-only setExtensionId(expectedId), which only a redeploy can undo.
ONE_SHOT_SETTER="setExtensionId"

REQUIRED="getting-started.md deployment-steps.md testing.md testing-against-coston2.md architecture.md ngrok.md cloudflared.md"
OPTIONAL="extension-guide.md instruction-sender.md types-server.md"
MAX_LINES=400   # past this a tester stops reading; split or cut

echo "docs: $DOCS"
echo

for f in $REQUIRED; do
    p="$DOCS/$f"
    if [[ ! -f "$p" ]]; then err "$f missing (required)"; continue; fi
    n=$(wc -l <"$p")
    # 20 lines is about the floor for a real doc; below that it is a stub.
    if (( n < 20 )); then err "$f is only $n lines — stub?"; continue; fi
    (( n > MAX_LINES )) && warn "$f is $n lines (>$MAX_LINES) — trim or split"
    ok "$f ($n lines)"
done

echo
for f in $OPTIONAL; do
    [[ -f "$DOCS/$f" ]] && ok "$f (optional)" || warn "$f missing — optional, expected"
done

# Platform traps every extension hits. Absent = the doc will not save anyone.
echo
D="$DOCS/deployment-steps.md"
if [[ -f "$D" ]]; then
    check() { grep -qiF "$2" "$D" && ok "deployment-steps: $1" || err "deployment-steps: missing $1 (looked for '$2')"; }
    check "stale-machine pausing"        "pause("
    check "active-machine listing"       "getActiveTeeMachines"
    check "one-shot binding warning"     "$ONE_SHOT_SETTER"
    check "launch-policy env override"   "allow_env_override"
    check "digest-pinned deploy"         "digest"
    check "attestation mode"             "SIMULATED_TEE"
fi

# NOTE: the "(N cases)" profile-matrix count in testing.md is checked by
# scripts/test-profile-matrix.sh itself, not here. That suite is the only thing
# that knows the true number — two of its assertions run once per shipped
# .example file, so no static count of `assert` lines is exact — and a gate that
# has to guess is the kind that gets switched off.
#
# The Solidity count IS checkable here, and exactly: Solidity has no way to
# generate test cases at run time, so one `function test*()` is one test and
# `grep -c` is not an approximation. Checked without Foundry on purpose — this
# script must stay runnable in a bare checkout. Two numbers had already drifted
# in testing.md with nothing to catch them; every other check here matches on
# keywords, so a stale figure is invisible to all of them.
echo
TESTDIR="$(cd "$SCRIPT_DIR/.." && pwd)/test"
if [[ -d "$TESTDIR" && -f "$DOCS/testing.md" ]]; then
    sol_actual=$(grep -rhcE '^[[:space:]]*function test[A-Za-z0-9_]*\(' "$TESTDIR"/*.sol 2>/dev/null | paste -sd+ - | bc)
    sol_doc=$(sed -nE 's/.*Solidity \(([0-9]+) tests\).*/\1/p' "$DOCS/testing.md" | head -1)
    if [[ -z "$sol_doc" ]]; then
        err "testing.md no longer states a Solidity test count (expected \"Solidity ($sol_actual tests)\")"
    elif [[ "$sol_doc" != "$sol_actual" ]]; then
        err "testing.md says $sol_doc Solidity tests; test/*.sol declares $sol_actual"
    else
        ok "testing.md Solidity test count matches test/*.sol ($sol_actual tests)"
    fi
fi

echo
if [[ -f "$DOCS/README.md" ]]; then ok "docs/README.md index present"; else err "docs/README.md index missing"; fi

echo
if (( fail )); then echo -e "${RED}docs standard: FAILED${NC}"; else echo -e "${GREEN}docs standard: OK${NC}"; fi
exit $fail
