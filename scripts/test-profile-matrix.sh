#!/usr/bin/env bash
#
# test-profile-matrix.sh — unit tests for the fail-fast profile matrix
# in scripts/lib/profile.sh. Pure bash, no Docker or network; run standalone
# or in CI. Exits non-zero if any case fails.
#
# Each case runs in a subshell so env mutations (TEE_PROFILE is set by
# resolve_tee_profile) never leak between cases.
set -uo pipefail   # deliberately not -e: failing calls are the point

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR_EARLY="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=lib/profile.sh
source "$SCRIPT_DIR/lib/profile.sh"

PASS=0; FAIL=0
RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'

# assert <expected: ok|fail> <name> <command...>
assert() {
    local expected="$1" name="$2"; shift 2
    local rc=0
    ( "$@" ) >/dev/null 2>&1 || rc=$?
    if { [[ "$expected" == ok && $rc -eq 0 ]] || [[ "$expected" == fail && $rc -ne 0 ]]; }; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo -e "${RED}FAIL${NC} $name (expected $expected, rc=$rc)" >&2
    fi
}

# run_matrix <chain> — resolve + validate with the current env, in one call
run_matrix() {
    local chain="$1"
    resolve_tee_profile "$chain" && validate_tee_profile "$chain"
}

with_env() {  # with_env VAR=val... -- fn args...
    local kv=()
    while [[ "$1" != "--" ]]; do kv+=("$1"); shift; done
    shift
    ( export "${kv[@]}"; "$@" )
}

# --- Profile resolution + matrix ------------------------------------------

# 1. Local devnet defaults resolve and validate.
assert ok   "local devnet defaults"                 with_env TEE_PROFILE= MODE= -- run_matrix local
# 2. Public chain + MODE=1 without an explicit profile is refused (opt-in).
assert fail "coston2 MODE=1 needs explicit profile" with_env TEE_PROFILE= MODE=1 -- run_matrix coston2
# 3. Explicit testnet-sim with MODE=1 passes (with a loud warning). LOCAL_MODE=false
#    is part of the supported configuration, not incidental: see cases 3b/3c.
assert ok   "testnet-sim MODE=1"                    with_env TEE_PROFILE=testnet-sim MODE=1 SIMULATED_TEE=true LOCAL_MODE=false -- run_matrix coston2
# 3b. testnet-sim with LOCAL_MODE unset is refused. Saying nothing must not select
#     the local-devnet behaviour on a public chain: start-proxy and the allow-listing
#     gate both read an unset LOCAL_MODE as local, so the dev-key fallbacks and the
#     relaxed URL gates would be live while this check reported a coherent profile.
assert fail "testnet-sim LOCAL_MODE unset"          with_env TEE_PROFILE=testnet-sim MODE=1 SIMULATED_TEE=true LOCAL_MODE= -- run_matrix coston2
# 3c. And refused when it is set to true outright.
assert fail "testnet-sim LOCAL_MODE=true"           with_env TEE_PROFILE=testnet-sim MODE=1 SIMULATED_TEE=true LOCAL_MODE=true -- run_matrix coston2
# 4. testnet-sim with MODE=0 is contradictory.
assert fail "testnet-sim MODE=0"                    with_env TEE_PROFILE=testnet-sim MODE=0 -- run_matrix coston2
# 4b. testnet-sim with SIMULATED_TEE=false: MODE=1 emits magic_pass, but
#     register-tee would parse it as a real JWT and abort.
assert fail "testnet-sim SIMULATED_TEE=false"       with_env TEE_PROFILE=testnet-sim MODE=1 SIMULATED_TEE=false LOCAL_MODE=false -- run_matrix coston2
# 5. MODE=0 on a public chain infers confidential-space; full matrix passes.
assert ok   "MODE=0 infers confidential-space"      with_env TEE_PROFILE= MODE=0 SIMULATED_TEE=false LOCAL_MODE=false -- run_matrix coston2
# 6. confidential-space rejects MODE=1.
assert fail "confidential-space MODE=1"             with_env TEE_PROFILE=confidential-space MODE=1 SIMULATED_TEE=false LOCAL_MODE=false -- run_matrix coston2
# 7. confidential-space rejects SIMULATED_TEE=true.
assert fail "confidential-space SIMULATED_TEE=true" with_env TEE_PROFILE=confidential-space MODE=0 SIMULATED_TEE=true LOCAL_MODE=false -- run_matrix coston2
# 8. confidential-space rejects LOCAL_MODE=true.
assert fail "confidential-space LOCAL_MODE=true"    with_env TEE_PROFILE=confidential-space MODE=0 SIMULATED_TEE=false LOCAL_MODE=true -- run_matrix coston2
# 9. Profile local on a public chain is rejected.
assert fail "local profile on coston2"              with_env TEE_PROFILE=local MODE=1 -- run_matrix coston2
# 10. Unknown profile value is rejected.
assert fail "unknown profile"                       with_env TEE_PROFILE=bogus -- run_matrix coston2
# 11. testnet-sim on CHAIN=local is rejected.
assert fail "testnet-sim on local chain"            with_env TEE_PROFILE=testnet-sim MODE=1 -- run_matrix local

# --- Proxy attestation config cross-check ----------------------------------

TMPDIR_FIXTURES="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_FIXTURES"' EXIT

# Every other section a real proxy config needs, so a fixture can be complete
# enough for the proxy's OWN loader (validate_proxy_config_loadable) while still
# isolating the one thing the case is about. Devnet addresses; they are never dialled.
cfg_base_sections() {
    cat <<'EOF'
chain_id = 114
[addresses]
flare_systems_manager = "0xa4bcDF64Cdd5451b6ac3743B414124A6299B65FF"
relay = "0x5A0773Ff307Bf7C71a832dBB5312237fD3437f9F"
voter_registry = "0xB00cC45B4a7d3e1FEE684cFc4417998A1c183e6d"
[ports]
internal = "6663"
external = "6664"
EOF
}

# No [attestation] at all — the upstream default is enable=false, an insecure
# bootstrap. Complete in every OTHER respect, which is also the honest shape of a
# fresh clone's config: the operator copied the .example and filled in [db].
cfg_none="$TMPDIR_FIXTURES/none.toml"
cfg_base_sections > "$cfg_none"

# Real-network fail-closed with the complete posture: every [attestation] check
# the proxy would otherwise skip on an empty/zero/false value is set. The
# fixtures after it drop exactly one of those keys each.
cfg_failclosed="$TMPDIR_FIXTURES/failclosed.toml"
cat > "$cfg_failclosed" <<'EOF'
chain_id = 114
[attestation]
enable = true
audience = "https://relying-party.example"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
allow_magic_pass = false
EOF

cfg_cs_full="$TMPDIR_FIXTURES/cs-full.toml"   # same posture, inline comments and multi-value lists
cat > "$cfg_cs_full" <<'EOF'
chain_id = 114
[attestation]
enable = true
audience = "https://sts.example/verify"   # expected aud claim
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899", "0xddeeff00112233445566778899aabbccddeeff00112233445566778899aabbcc"]
expected_platforms = ["AMD_SEV_SNP_VM", "INTEL_TDX_VM"]
expected_debug_statuses = ["disabled-since-boot"]
# max_token_age = 0        # decoy: commented-out values must be ignored
max_token_age = "10m"
# require_sec_boot = false # decoy
require_sec_boot = true
allow_magic_pass = false
EOF

cfg_cs_no_platforms="$TMPDIR_FIXTURES/cs-no-platforms.toml"   # posture minus expected_platforms
sed '/^expected_platforms/d' "$cfg_failclosed" > "$cfg_cs_no_platforms"

cfg_cs_no_debug="$TMPDIR_FIXTURES/cs-no-debug-statuses.toml"  # posture minus expected_debug_statuses
sed '/^expected_debug_statuses/d' "$cfg_failclosed" > "$cfg_cs_no_debug"

cfg_cs_empty_audience="$TMPDIR_FIXTURES/cs-empty-audience.toml"   # audience present but empty (upstream: skips the check)
sed 's|^audience = .*|audience = ""|' "$cfg_failclosed" > "$cfg_cs_empty_audience"

cfg_cs_zero_age="$TMPDIR_FIXTURES/cs-zero-token-age.toml"      # max_token_age = 0 (upstream: skips freshness)
sed 's|^max_token_age = .*|max_token_age = 0|' "$cfg_failclosed" > "$cfg_cs_zero_age"

cfg_cs_zero_age_str="$TMPDIR_FIXTURES/cs-zero-token-age-str.toml"   # the quoted "0s" spelling of the same thing
sed 's|^max_token_age = .*|max_token_age = "0s"|' "$cfg_failclosed" > "$cfg_cs_zero_age_str"

cfg_cs_no_secboot="$TMPDIR_FIXTURES/cs-no-sec-boot.toml"       # require_sec_boot = false
sed 's|^require_sec_boot = .*|require_sec_boot = false|' "$cfg_failclosed" > "$cfg_cs_no_secboot"

cfg_failclosed_unpinned="$TMPDIR_FIXTURES/failclosed-unpinned.toml"   # posture minus the code-hash pin (commented out)
sed 's|^expected_code_hashes = |# expected_code_hashes = |' "$cfg_failclosed" > "$cfg_failclosed_unpinned"

cfg_sim="$TMPDIR_FIXTURES/sim.toml"            # explicit simulated profile
cat > "$cfg_sim" <<'EOF'
[attestation]
enable = true
allow_magic_pass = true
EOF

check_cfg() {  # check_cfg <profile> <cfg>
    TEE_PROFILE="$1" validate_proxy_attestation_config "$2"
}

# 12. Deadlock caught: MODE=1 profile against a magic_pass-rejecting proxy.
assert fail "testnet-sim vs fail-closed proxy"          check_cfg testnet-sim "$cfg_failclosed"
# 13. Missing [attestation] section is refused on testnet-sim (fail-open guard).
assert fail "testnet-sim vs missing attestation"       check_cfg testnet-sim "$cfg_none"
# 14. Matching sim config passes.
assert ok   "testnet-sim vs sim proxy"                 check_cfg testnet-sim "$cfg_sim"
# 15. confidential-space accepts only fail-closed + pinned hashes.
assert ok   "confidential-space vs fail-closed pinned" check_cfg confidential-space "$cfg_failclosed"
# 16. confidential-space refuses magic_pass acceptance.
assert fail "confidential-space vs sim proxy"          check_cfg confidential-space "$cfg_sim"
# 17. confidential-space refuses an unpinned allowlist.
assert fail "confidential-space vs unpinned hashes"    check_cfg confidential-space "$cfg_failclosed_unpinned"
# 18. confidential-space refuses a config with no attestation section.
assert fail "confidential-space vs missing attestation" check_cfg confidential-space "$cfg_none"
# 19. local profile skips the proxy config check entirely.
assert ok   "local skips proxy config check"           check_cfg local "$cfg_none"

# --- confidential-space: the whole posture, not just the pin -----------------
# Upstream treats an empty list / empty string / 0 / false as "skip this check",
# so each of these configs would verify less than it appears to. All are refused.

# 20. A fully specified posture is what confidential-space accepts.
assert ok   "confidential-space vs full posture"        check_cfg confidential-space "$cfg_cs_full"
# 21. No expected_platforms: any hardware model that can mint a token passes.
assert fail "confidential-space vs no platforms"       check_cfg confidential-space "$cfg_cs_no_platforms"
# 22. No expected_debug_statuses: a debuggable TEE passes.
assert fail "confidential-space vs no debug statuses"  check_cfg confidential-space "$cfg_cs_no_debug"
# 23. Empty audience: a token minted for another relying party passes.
assert fail "confidential-space vs empty audience"     check_cfg confidential-space "$cfg_cs_empty_audience"
# 24. max_token_age = 0: no freshness check, so an old token can be replayed.
assert fail "confidential-space vs zero max_token_age" check_cfg confidential-space "$cfg_cs_zero_age"
# 24b. Same, spelled "0s".
assert fail "confidential-space vs \"0s\" max_token_age" check_cfg confidential-space "$cfg_cs_zero_age_str"
# 25. require_sec_boot = false: a VM without a verified boot chain passes.
assert fail "confidential-space vs require_sec_boot=false" check_cfg confidential-space "$cfg_cs_no_secboot"

# --- The value must come from [attestation], not from anywhere in the file ---
# Regression for a HIGH: every key was read with a whole-file scan that took the LAST
# assignment, so a later section supplying a fail-closed-looking value satisfied the
# gate while [attestation] itself had the check switched off. The pre-flight then
# approved a deployment whose attestation was disabled. Case 28 is the other half of
# the fix: reading the right table, not merely refusing more configs.

cfg_late_enable="$TMPDIR_FIXTURES/late-enable.toml"   # the reported case, minimally
cat > "$cfg_late_enable" <<'EOF'
chain_id = 114
[attestation]
enable = false
audience = "https://relying-party.example"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
allow_magic_pass = false

[direct]
enable = true
EOF

cfg_late_posture="$TMPDIR_FIXTURES/late-posture.toml"   # every key satisfied from elsewhere
cat > "$cfg_late_posture" <<'EOF'
chain_id = 114
[attestation]
enable = false
allow_magic_pass = true
audience = ""
expected_code_hashes = []
expected_platforms = []
expected_debug_statuses = []
max_token_age = "0s"
require_sec_boot = false

[direct]
enable = true
allow_magic_pass = false
audience = "https://relying-party.example"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
EOF

cfg_late_decoy="$TMPDIR_FIXTURES/late-decoy.toml"   # correct posture, later section contradicts every key
cat > "$cfg_late_decoy" <<'EOF'
chain_id = 114
[attestation]
enable = true
audience = "https://relying-party.example"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
allow_magic_pass = false

[direct]
enable = false
allow_magic_pass = true
audience = ""
max_token_age = 0
require_sec_boot = false
EOF

cfg_dup_table="$TMPDIR_FIXTURES/dup-table.toml"     # [attestation] declared twice
printf '%s\n\n[attestation]\nenable = false\n' "$(cat "$cfg_failclosed")" > "$cfg_dup_table"

cfg_dup_key="$TMPDIR_FIXTURES/dup-key.toml"         # enable assigned twice inside the table
printf '%s\nenable = false\n' "$(cat "$cfg_failclosed")" > "$cfg_dup_key"

cfg_dotted="$TMPDIR_FIXTURES/dotted.toml"           # top-level dotted keys, no [attestation] table
cat > "$cfg_dotted" <<'EOF'
chain_id = 114
attestation.enable = true
attestation.allow_magic_pass = false
attestation.audience = "https://relying-party.example"
attestation.expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
attestation.expected_platforms = ["AMD_SEV_SNP_VM"]
attestation.expected_debug_statuses = ["disabled-since-boot"]
attestation.max_token_age = "5m"
attestation.require_sec_boot = true
EOF

cfg_multiline="$TMPDIR_FIXTURES/multiline.toml"     # a pin written across lines is still a pin
cat > "$cfg_multiline" <<'EOF'
chain_id = 114
[attestation]
enable = true
audience = "https://relying-party.example"
expected_code_hashes = [
  "sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899",
]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
allow_magic_pass = false

[direct]
enable = false
EOF

# 26. The reported case: attestation off, a later section says enable = true.
assert fail "cs vs enable=false + later [direct] enable=true" check_cfg confidential-space "$cfg_late_enable"
# 27. The whole posture satisfied from a later section while [attestation] is disabled.
assert fail "cs vs full posture supplied by another section"  check_cfg confidential-space "$cfg_late_posture"
# 28. The converse, and the real proof of section-awareness: a correct [attestation]
#     stays valid however loudly a later section contradicts it.
assert ok   "cs vs correct posture + contradicting later section" check_cfg confidential-space "$cfg_late_decoy"
# 29. [attestation] declared twice — TOML forbids it and which one wins decides the gate.
assert fail "cs vs duplicate [attestation] table"             check_cfg confidential-space "$cfg_dup_table"
# 30. A key assigned twice inside the table, for the same reason.
assert fail "cs vs duplicate enable key in [attestation]"     check_cfg confidential-space "$cfg_dup_key"
# 31. The dotted-key spelling names the SAME table in TOML, and the proxy reads it
#     that way, so the pre-flight validates it instead of refusing it. This case
#     used to assert a refusal — correct for a bracket-counting reader that could
#     not see the form at all, but a fail-closed false alarm against the real
#     parser. The posture here is complete, so the answer is "valid".
assert ok   "cs vs dotted attestation.* keys"                 check_cfg confidential-space "$cfg_dotted"
# 31b. And the refusal still lands when a dotted posture is INCOMPLETE, so case 31
#      passes because the config is good, not because dotted keys stopped being read.
cfg_dotted_bad="$TMPDIR_FIXTURES/dotted-bad.toml"
sed '/^attestation.audience/d' "$cfg_dotted" > "$cfg_dotted_bad"
assert fail "cs vs dotted keys missing the audience"          check_cfg confidential-space "$cfg_dotted_bad"
# 32. A multi-line array is a real pin: refusing it would be a fail-closed false alarm.
assert ok   "cs vs multi-line expected_code_hashes"           check_cfg confidential-space "$cfg_multiline"

# --- A <placeholder> is not a value --------------------------------------------
# The posture checks test for non-EMPTY, and "<attestation-token-audience>" is not
# empty. So a config straight from the template read as fully configured. The proxy
# fails closed on it — no token's aud claim matches that string — which is the point:
# the operator got a bootstrap timeout to debug instead of the name of the field they
# had not filled in, and converting that is this gate's entire job.

cfg_ph_audience="$TMPDIR_FIXTURES/placeholder-audience.toml"
sed 's|^audience = .*|audience = "<attestation-token-audience>"|' "$cfg_failclosed" > "$cfg_ph_audience"

cfg_ph_list="$TMPDIR_FIXTURES/placeholder-in-list.toml"   # placeholder inside a list value
sed 's|^expected_code_hashes = .*|expected_code_hashes = ["sha256:<measured-image-code-hash>"]|' \
    "$cfg_failclosed" > "$cfg_ph_list"

# 33. An unsubstituted audience is refused, and named.
assert fail "cs vs <placeholder> audience"             check_cfg confidential-space "$cfg_ph_audience"
# 34. The template's own shape: a placeholder inside the code-hash list. Refused
#     twice over — it is a placeholder AND it is not 32 bytes of hex — which is why
#     34b exists rather than this case standing alone.
assert fail "cs vs <placeholder> inside a list value"  check_cfg confidential-space "$cfg_ph_list"
# 34b. The placeholder check's list branch, isolated. expected_platforms is a plain
#      string list with no format validation behind it, so a placeholder there is
#      caught by NOTHING ELSE: the list is non-empty and the entry is not blank.
#      Verified by mutation — disabling the placeholder check fails this case and
#      leaves case 34 green, which is what made this case necessary.
cfg_ph_platform="$TMPDIR_FIXTURES/placeholder-platform.toml"
sed 's|^expected_platforms = .*|expected_platforms = ["<your-machine-hwmodel>"]|' \
    "$cfg_failclosed" > "$cfg_ph_platform"
assert fail "cs vs <placeholder> in expected_platforms" check_cfg confidential-space "$cfg_ph_platform"
# 35. The converse: a fully substituted posture must still pass. Angle brackets do not
#     occur in a real audience, hash or hwmodel, but a check that refused real configs
#     would be worse than the gap it closed.
assert ok   "cs vs fully substituted posture"          check_cfg confidential-space "$cfg_failclosed"

# --- Strings are not structure: brackets and headers inside a value ---------
#
# The section-awareness above is only as good as the reader that finds the table
# boundaries. It counts brackets to skip over multi-line arrays, so anything that
# makes that count wrong silently reattributes one table to another — which is
# case 26 again, through a different door. Two ordinary TOML string forms did
# exactly that: a literal (single-quoted) string was not skipped when counting,
# so an unbalanced [ inside one swallowed the next table header; and a string
# spanning lines had its contents read as configuration.

cfg_lit_bypass="$TMPDIR_FIXTURES/literal-bypass.toml"   # the bypass shape: no enable in the real table
cat > "$cfg_lit_bypass" <<'EOF'
chain_id = 114
[attestation]
allow_magic_pass = false
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
audience = 'https://relying-party.example/[tenant'

[direct]
enable = true
EOF

cfg_lit_ok="$TMPDIR_FIXTURES/literal-ok.toml"           # same awkward value, correct posture, hostile neighbour
cat > "$cfg_lit_ok" <<'EOF'
chain_id = 114
[attestation]
enable = true
allow_magic_pass = false
audience = 'https://relying-party.example/[tenant'
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true

[direct]
enable = false
allow_magic_pass = true
require_sec_boot = false
max_token_age = "0s"
EOF

cfg_quote_mix="$TMPDIR_FIXTURES/quote-mix.toml"         # both quote styles on one line, stray ]
cat > "$cfg_quote_mix" <<'EOF'
chain_id = 114
[attestation]
enable = true
allow_magic_pass = false
audience = "it's a [basic] string"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
note = { basic = "it's", literal = '[x' }

[direct]
enable = false
EOF

cfg_hash_in_lit="$TMPDIR_FIXTURES/hash-in-literal.toml" # a # inside a value is not a comment
sed "s|^audience = .*|audience = 'https://relying-party.example/#[frag'|" \
    "$cfg_late_decoy" > "$cfg_hash_in_lit"

cfg_ml_phantom="$TMPDIR_FIXTURES/multiline-phantom.toml" # a table that exists only inside a string
cat > "$cfg_ml_phantom" <<'EOF'
chain_id = 114
[notes]
text = """
[attestation]
enable = true
allow_magic_pass = false
audience = "https://relying-party.example"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
"""
EOF

cfg_ml_harmless="$TMPDIR_FIXTURES/multiline-harmless.toml" # correct posture, unrelated multi-line prose
cat > "$cfg_ml_harmless" <<'EOF'
chain_id = 114
[notes]
text = """
harmless prose
"""
[attestation]
enable = true
allow_magic_pass = false
audience = "https://relying-party.example"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
EOF

# 46. The bypass itself: an unbalanced [ in a literal string used to hide the next
#     table header, so [direct]'s enable = true was read as [attestation]'s while
#     the real table had no enable key at all — attestation off, gate satisfied.
assert fail "cs vs literal-string [ hiding a later enable=true" check_cfg confidential-space "$cfg_lit_bypass"
# 47. The converse, and the proof this reads the right table rather than merely
#     refusing more: the same awkward value with a correct posture stays valid
#     however loudly the next section contradicts every key. This is case 28 for
#     strings, and it is what a blanket "refuse anything with a quote" would fail.
assert ok   "cs vs literal-string [ + correct posture"         check_cfg confidential-space "$cfg_lit_ok"
# 48. Both quote styles on ONE line — an apostrophe inside a basic string, then a
#     literal string holding an unbalanced [. This is what a masking implementation
#     cannot do: it must pick an order, and masking the literal style first consumes
#     from the apostrophe to the next single quote, exposing the [ as if it were
#     syntax. Spreading the two styles over separate lines does NOT test this, which
#     is how the first version of this case passed against a masking implementation.
assert ok   "cs vs mixed quote styles and a stray ]"           check_cfg confidential-space "$cfg_quote_mix"
# 49. Acceptance guard, not a control test: a value holding both # and [ must be
#     read, not refused. Stated precisely because it cannot be more — moving the
#     comment check ahead of the string check is provably equivalent here, since a #
#     inside a string can only be followed by brackets inside that same string, and
#     both orders ignore those. It guards the fail-closed direction: a future
#     tightening that refused any awkward value outright would fail this case.
assert ok   "cs vs # and [ inside a literal string"            check_cfg confidential-space "$cfg_hash_in_lit"
# 50. The phantom table: [attestation] appears only INSIDE a multi-line string, and
#     there is no real section anywhere. The old reader held no string state between
#     lines and read that block as configuration, so the file passed on a table made
#     of prose. The parser sees one string and no [attestation], so the posture is
#     unset and the config is refused — same verdict, now for the true reason.
assert fail "cs vs phantom [attestation] inside a multi-line string" check_cfg confidential-space "$cfg_ml_phantom"
# 51. The other direction, and the reason case 50 is no longer about form: harmless
#     multi-line prose beside a good posture is now ACCEPTED. The old reader refused
#     it outright — a documented fail-closed false alarm, because it could not track
#     a string across lines and would rather guess wrong loudly. Removing that
#     refusal is part of the fix, not a relaxation of it: the parser reads the string
#     as a string, so the posture beside it is simply read.
assert ok   "cs vs harmless multi-line string beside a good posture" check_cfg confidential-space "$cfg_ml_harmless"

# --- Valid TOML that the regex reader accepted and the proxy skipped -----------
#
# The reported HIGH, and the reason the reader is now the proxy's own parser rather
# than sed/grep/awk. Each config below is VALID TOML, was accepted by the old
# validator with rc=0, and decodes — in the proxy — to the value that means "skip
# this check". Upstream states the semantics outright in pkg/attestation/verifier.go:
# `Audience != ""`, `len(list) > 0`, `MaxTokenAge > 0`, and the struct comment
# "Empty allowlists / zero values skip the corresponding check."
#
# Cases 52-53 are the ones that matter most for how this is fixed. They use ORDINARY
# double quotes and no trick at all: the old zero-detector regex
# ^0+(\.0+)?(ns|us|ms|s|m|h)?$ cannot match a duration written in more than one unit,
# so teaching it about single quotes — the obvious minimal fix — would have left the
# freshness check bypassable. That is what makes this a class rather than three bugs.

# A list whose only element is a comment is an EMPTY list. The old check tested
# "the first character after [ is not ] or whitespace", and '#' satisfied it.
cfg_list_comment_only="$TMPDIR_FIXTURES/list-comment-only.toml"
awk '{ if ($0 ~ /^expected_code_hashes/) { print "expected_code_hashes = ["; print "  # " $0; print "]" } else print }' \
    "$cfg_failclosed" > "$cfg_list_comment_only"

# Literal (single-quoted) strings. TOML gives them no escapes at all, so '' is the
# empty string and '0s' is the two characters 0 and s — a zero duration.
cfg_lit_empty_audience="$TMPDIR_FIXTURES/literal-empty-audience.toml"
sed "s|^audience = .*|audience = ''|" "$cfg_failclosed" > "$cfg_lit_empty_audience"

cfg_lit_zero_age="$TMPDIR_FIXTURES/literal-zero-age.toml"
sed "s|^max_token_age = .*|max_token_age = '0s'|" "$cfg_failclosed" > "$cfg_lit_zero_age"

# The two that need no quoting trick.
cfg_zero_composite="$TMPDIR_FIXTURES/zero-composite-age.toml"
sed 's|^max_token_age = .*|max_token_age = "0h0m0s"|' "$cfg_failclosed" > "$cfg_zero_composite"

cfg_zero_composite2="$TMPDIR_FIXTURES/zero-composite-age-2.toml"
sed 's|^max_token_age = .*|max_token_age = "0s0ms"|' "$cfg_failclosed" > "$cfg_zero_composite2"

# Negative duration: upstream's own Attestation.validate() refuses it, so the proxy
# would not start. Named here rather than as a container that exits during `up`.
cfg_negative_age="$TMPDIR_FIXTURES/negative-age.toml"
sed 's|^max_token_age = .*|max_token_age = "-5m"|' "$cfg_failclosed" > "$cfg_negative_age"

# A non-empty list of empty strings. len() > 0 so upstream runs the check, against a
# value no token can match — fail-closed, but as a bootstrap timeout rather than a
# message about this field. Only a typed read can see the difference.
cfg_empty_elem="$TMPDIR_FIXTURES/empty-list-element.toml"
sed 's|^expected_platforms = .*|expected_platforms = [ "" ]|' "$cfg_failclosed" > "$cfg_empty_elem"

# A bool written as a string. The old regex captured [A-Za-z]+ after the '=', so
# `enable = "true"` matched nothing and read as unset — the right verdict by
# accident. A typed read calls it what it is: the wrong type, with a line number.
cfg_bool_as_string="$TMPDIR_FIXTURES/bool-as-string.toml"
sed 's|^enable = .*|enable = "true"|' "$cfg_failclosed" > "$cfg_bool_as_string"

# The array-of-tables spelling. Previously refused by name in a hand-written list of
# forms the reader could not handle; now a type error from the parser.
cfg_array_of_tables="$TMPDIR_FIXTURES/array-of-tables.toml"
sed 's|^\[attestation\]$|[[attestation]]|' "$cfg_failclosed" > "$cfg_array_of_tables"

# Acceptance control: a composite duration is a perfectly ordinary way to write a
# real value, and must be READ, not refused. Cases 52-53 must fail because the
# duration is zero, not because it has two units in it.
cfg_composite_ok="$TMPDIR_FIXTURES/composite-age-ok.toml"
# Under the ceiling on purpose: this fixture is about a multi-unit duration PARSING
# and passing, not about how large it may be — the ceiling has its own cases.
sed 's|^max_token_age = .*|max_token_age = "1m30s"|' "$cfg_failclosed" > "$cfg_composite_ok"

# 49b. An empty list spelled as a comment-only list.
assert fail "cs vs comment-only expected_code_hashes list"    check_cfg confidential-space "$cfg_list_comment_only"
# 49c. audience = '' — a literal empty string, not a two-character value.
assert fail "cs vs literal-string empty audience"             check_cfg confidential-space "$cfg_lit_empty_audience"
# 49d. max_token_age = '0s' — single quotes hid the zero from the regex.
assert fail "cs vs literal-string zero max_token_age"         check_cfg confidential-space "$cfg_lit_zero_age"
# 52. max_token_age = "0h0m0s" — ORDINARY double quotes. No regex can do this.
assert fail "cs vs composite zero max_token_age (0h0m0s)"     check_cfg confidential-space "$cfg_zero_composite"
# 53. The same in another spelling, so case 52 is not a single hard-coded string.
assert fail "cs vs composite zero max_token_age (0s0ms)"      check_cfg confidential-space "$cfg_zero_composite2"
# 54. A negative duration the proxy itself refuses to start on.
assert fail "cs vs negative max_token_age"                    check_cfg confidential-space "$cfg_negative_age"
# 55. A list of empty strings is not a pin.
assert fail "cs vs expected_platforms = [\"\"]"               check_cfg confidential-space "$cfg_empty_elem"
# 56. A bool written as a string is a type error, not an absent value.
assert fail "cs vs enable = \"true\" (wrong type)"            check_cfg confidential-space "$cfg_bool_as_string"
# 57. [[attestation]] is an array of tables, not the table this validates.
assert fail "cs vs [[attestation]] array-of-tables"           check_cfg confidential-space "$cfg_array_of_tables"
# 58. The acceptance guard for 52-53: a real composite duration must pass.
assert ok   "cs vs composite max_token_age = \"1m30s\""       check_cfg confidential-space "$cfg_composite_ok"

# --- The whole file must load, not just [attestation] -----------------------
#
# resolve_proxy_config runs this second check on a real deployment config, because a
# config whose attestation posture is perfect still cannot start a proxy if some
# other section is wrong. It is the proxy's own loader (tee-proxy config.Read), which
# applies the proxy's defaults, validates every section and — unlike the attestation
# read — rejects unknown fields. Kept as a separate function so the verdict on
# [attestation] never depends on unrelated sections, and so a partial fixture above
# stays testable.

cfg_loadable="$TMPDIR_FIXTURES/loadable.toml"
{ cfg_base_sections; cat "$cfg_failclosed" | sed '/^chain_id/d'; } > "$cfg_loadable"

cfg_typo_key="$TMPDIR_FIXTURES/typo-key.toml"      # require_sec_bootT: a real typo shape
sed 's|^require_sec_boot = |require_sec_boott = |' "$cfg_loadable" > "$cfg_typo_key"

cfg_no_ports="$TMPDIR_FIXTURES/no-ports.toml"      # [ports] external missing
sed '/^external = /d' "$cfg_loadable" > "$cfg_no_ports"

# 59. A complete config loads.
assert ok   "loadable: complete config"                validate_proxy_config_loadable "$cfg_loadable"
# 60. A mistyped key is refused. Upstream reads with allowUnknownFields=false, so
#     this would abort the proxy at start; naming it here is the whole point.
assert fail "loadable: mistyped key rejected"          validate_proxy_config_loadable "$cfg_typo_key"
# 61. A missing [ports] entry is refused by the proxy's own section validation.
assert fail "loadable: missing ports.external"         validate_proxy_config_loadable "$cfg_no_ports"
# 62. And the attestation posture is NOT what this check is about. The SAME file,
#     complete but with max_token_age = 0, loads fine and fails the posture. Two
#     checks, two questions — this is what stops either from masking the other.
cfg_loadable_bad_posture="$TMPDIR_FIXTURES/loadable-bad-posture.toml"
sed 's|^max_token_age = .*|max_token_age = 0|' "$cfg_loadable" > "$cfg_loadable_bad_posture"
assert ok   "loadable: config with a bad posture still loads" validate_proxy_config_loadable "$cfg_loadable_bad_posture"
assert fail "posture: the same file fails the posture check"  check_cfg confidential-space "$cfg_loadable_bad_posture"

# --- Which config is in force, per deployment mode -------------------------
#
# The gate above is only worth as much as the set of paths that call it. It used to
# be called from the Docker branch of start-services.sh alone, so `--local` started
# against whatever config/proxy/extension_proxy.toml said — a file that in a fresh
# clone has no [attestation] section at all. resolve_proxy_config is now the single
# entry point both modes use, so these cases cover the wiring as well as the check.

# name_is <mode> <chain> <expected> — the mapping. Non-zero on a different name.
name_is() {
    local got
    got="$(proxy_config_name "$1" "$2")" || return 1
    [[ "$got" == "$3" ]]
}

# resolves_to <mode> <chain> <root> <expected-basename> — non-zero when
# resolve_proxy_config refuses the config OR picks a different file than expected.
resolves_to() {
    local got
    got="$(resolve_proxy_config "$1" "$2" "$3")" || return 1
    [[ "$(basename "$got")" == "$4" ]]
}

# refuses_with <regex> <mode> <chain> <root> — non-zero unless resolve_proxy_config
# refuses AND says why in matching terms.
#
# The two shape checks (directory, absent) are pinned on their MESSAGE rather than on
# the refusal, because the refusal alone does not pin them: delete either branch and
# the run still fails closed one step later — a directory falls through to the
# not-found branch, and a missing file makes the [attestation] reader refuse to parse
# it. Those branches exist to name the cause, so the cause is what a test of them has
# to assert. Verified by mutation: asserting only "it refused" passed with either
# branch deleted.
refuses_with() {
    local want="$1"; shift
    local out rc=0
    out="$(resolve_proxy_config "$@" 2>&1 >/dev/null)" || rc=$?
    [[ $rc -ne 0 ]] || return 1
    grep -qE "$want" <<<"$out"
}

# 52-57. Docker mode keeps the exact names compose bind-mounts; host mode is the
#        same names without the .docker infix. Spelled out rather than derived, so a
#        change to either mapping has to be made here too.
assert ok   "name docker/local"    name_is docker local   extension_proxy.docker.toml
assert ok   "name docker/coston"   name_is docker coston  extension_proxy.coston.docker.toml
assert ok   "name docker/coston2"  name_is docker coston2 extension_proxy.coston2.docker.toml
assert ok   "name host/local"      name_is host   local   extension_proxy.toml
assert ok   "name host/coston"     name_is host   coston  extension_proxy.coston.toml
assert ok   "name host/coston2"    name_is host   coston2 extension_proxy.coston2.toml
# 58. An unknown mode is refused rather than defaulting to one of them.
assert fail "name rejects unknown mode" name_is sideways local extension_proxy.toml

# Fixture tree A: the host config for CHAIN=local is GOOD for testnet-sim while the
# one for coston2 is missing its [attestation] section entirely. A chain-blind
# resolution reads the good file on a coston2 run and passes; a chain-aware one reads
# the bad file and refuses. That asymmetry is the point of the fixture.
treeA="$TMPDIR_FIXTURES/treeA/config/proxy"; mkdir -p "$treeA"
{ cfg_base_sections; printf '[attestation]\nenable = true\nallow_magic_pass = true\n'; } \
    > "$treeA/extension_proxy.toml"
cp "$cfg_none" "$treeA/extension_proxy.coston2.toml"
cp "$treeA/extension_proxy.toml" "$treeA/extension_proxy.coston2.docker.toml"
mkdir -p "$treeA/extension_proxy.docker.toml"   # the directory docker leaves behind

# Fixture tree B: only the host local config, with no [attestation] section — the
# state of a fresh clone, and exactly what `--local` used to start against unchecked.
treeB="$TMPDIR_FIXTURES/treeB/config/proxy"; mkdir -p "$treeB"
cp "$cfg_none" "$treeB/extension_proxy.toml"

# 59. THE REGRESSION. Host mode on testnet-sim now reads the file --local actually
#     loads and refuses it for having no [attestation] section. Before the fix this
#     path never reached the gate at all.
assert fail "host/local refuses a config with no [attestation] on testnet-sim" \
    with_env TEE_PROFILE=testnet-sim -- resolves_to host local "$TMPDIR_FIXTURES/treeB" extension_proxy.toml
# 60. Fail-closed only where it should be: the same file is fine on the local
#     devnet profile, which has no attestation to downgrade.
assert ok   "host/local accepts the same config on the local profile" \
    with_env TEE_PROFILE=local -- resolves_to host local "$TMPDIR_FIXTURES/treeB" extension_proxy.toml
# 61. Chain-awareness, proven by the asymmetry in tree A: a coston2 host run must
#     read extension_proxy.coston2.toml (bad) and refuse — not extension_proxy.toml
#     (good), which is what a chain-blind lookup returned.
assert fail "host/coston2 reads the coston2 config, not the local one" \
    with_env TEE_PROFILE=testnet-sim -- resolves_to host coston2 "$TMPDIR_FIXTURES/treeA" extension_proxy.coston2.toml
# 62. And the good file is still reachable on the chain it belongs to, so case 61
#     fails for the right reason rather than because host mode refuses everything.
assert ok   "host/local reads the local config" \
    with_env TEE_PROFILE=testnet-sim -- resolves_to host local "$TMPDIR_FIXTURES/treeA" extension_proxy.toml
# 63. Docker mode is unchanged by the refactor: same name, same gate.
assert ok   "docker/coston2 resolves and passes the gate" \
    with_env TEE_PROFILE=testnet-sim -- resolves_to docker coston2 "$TMPDIR_FIXTURES/treeA" extension_proxy.coston2.docker.toml
# 64. A directory where the config should be — what docker leaves behind when the
#     bind-mount source is missing — is named as such, not reported as absent.
assert ok   "docker/local names a directory in place of the config" \
    with_env TEE_PROFILE=testnet-sim -- refuses_with 'is a directory' docker local "$TMPDIR_FIXTURES/treeA"
# 65. A config that is simply absent is refused with the .example hint, rather than
#     silently falling back to another chain's file or dying inside the TOML reader.
assert ok   "host/coston names an absent config and points at the .example" \
    with_env TEE_PROFILE=testnet-sim -- refuses_with 'not found.*fresh clone' host coston "$TMPDIR_FIXTURES/treeB"
# 66. With no profile resolved there is nothing to check the config against, so a
#     perfectly good config is still refused. resolve_proxy_config runs after
#     resolve_tee_profile in every caller, and this is what makes that ordering
#     enforced rather than merely observed.
assert ok   "host/local refuses when no profile is resolved" \
    with_env TEE_PROFILE= -- refuses_with 'TEE_PROFILE not resolved|does not match TEE_PROFILE' host local "$TMPDIR_FIXTURES/treeA"

# --- The other two calls resolve_proxy_config makes ------------------------
#
# Cases 59-66 pin that resolve_proxy_config calls the ATTESTATION check: delete that
# call and three of them go red. Its two siblings had no such case, and F-9 is the
# reminder of what that costs — "a check with one call site is a check for one code
# path" applies just as well to a check with one call site and no test on it. Both
# mutations below were confirmed to survive the suite before these cases existed.

# Tree C: a config whose [attestation] section is right for testnet-sim and whose
# chain_id matches, but which carries a key the proxy's own loader does not know.
# Only the whole-file load can refuse this one, so it is that call or nothing.
treeC="$TMPDIR_FIXTURES/treeC/config/proxy"; mkdir -p "$treeC"
{ cfg_base_sections
  printf '[attestation]\nenable = true\nallow_magic_pass = true\n'
  printf 'redis_prot = "redis:6379"\n'   # a typo for redis_port: valid TOML, unknown key
} > "$treeC/extension_proxy.toml"

assert ok "resolve names a config the proxy could not load" \
    with_env TEE_PROFILE=testnet-sim CHAIN_ID=114 -- \
    refuses_with 'not a config the proxy can load' host local "$TMPDIR_FIXTURES/treeC"

# Tree D: a perfectly good config for chain 114, used to pin the chain_id call in
# both directions. Without the call the first of these passes and the mismatch ships.
treeD="$TMPDIR_FIXTURES/treeD/config/proxy"; mkdir -p "$treeD"
{ cfg_base_sections; printf '[attestation]\nenable = true\nallow_magic_pass = true\n'; } \
    > "$treeD/extension_proxy.toml"

assert ok "resolve refuses a config for another chain" \
    with_env TEE_PROFILE=testnet-sim CHAIN_ID=16 -- \
    refuses_with 'CHAIN_ID=16|not a config for CHAIN_ID' host local "$TMPDIR_FIXTURES/treeD"
assert ok "resolve accepts the config for this chain" \
    with_env TEE_PROFILE=testnet-sim CHAIN_ID=114 -- \
    resolves_to host local "$TMPDIR_FIXTURES/treeD" extension_proxy.toml
# An unset CHAIN_ID is not this check's business: it has its own, louder failure
# (chainID 0 → empty signatures), so the comparison is skipped rather than guessed.
assert ok "resolve does not guess a chain when CHAIN_ID is unset" \
    with_env TEE_PROFILE=testnet-sim CHAIN_ID= -- \
    resolves_to host local "$TMPDIR_FIXTURES/treeD" extension_proxy.toml

# --- The bind-mount is the third copy of the config-filename mapping -------
#
# resolve_proxy_config validates the name proxy_config_name derives, and exports it as
# PROXY_CONFIG. On the HOST path that is the file the binary opens, and a paired Go
# case pins the two mappings against each other. On the DOCKER path PROXY_CONFIG is
# never read: the container opens /app/config/config.toml, and what appears there is
# whatever the compose file bind-mounts. That is a third copy of the mapping,
# maintained by hand, and until these cases nothing compared it with the other two.
#
# Proven reachable: pointing the coston2 overlay at extension_proxy.docker.toml — a
# committed config with no [attestation] section at all, which upstream reads as
# enable = false — left all 90 cases, check-docs and check-versions green, on a
# public-testnet deployment. "The file the proxy opens is the file that was checked"
# was true for Docker only because two lists happened to agree.
#
# Reads the mount rather than restating it, so the case cannot drift with the file.
compose_mount_is() {
    local file="$1" chain="$2" want mounted count
    want="$(proxy_config_name docker "$chain")" || return 1
    mounted="$(sed -nE 's|^[[:space:]]*-[[:space:]]*\./config/proxy/([^:]+):/app/config/config\.toml.*|\1|p' "$file")"
    count="$(grep -c . <<<"$mounted")"
    # Exactly one. Two mounts on the same target is a file docker would refuse, and
    # taking the last would quietly pick a winner instead of naming the problem.
    [[ "$count" -eq 1 ]] || return 1
    [[ "$mounted" == "$want" ]]
}

assert ok "base compose mounts the config the scripts validate" \
    compose_mount_is "$PROJECT_DIR_EARLY/docker-compose.yaml" local

# Data-driven over the overlays actually present, so a new chain cannot be added with
# a mismatched mount and no case to catch it. The counter below is the "no silent
# caps" half: a rename that made this loop find nothing would otherwise read as pass.
overlays_checked=0
for _ov in "$PROJECT_DIR_EARLY"/docker-compose.*.yaml; do
    _chain="$(basename "$_ov" .yaml)"; _chain="${_chain#docker-compose.}"
    # Only the chain overlays declare the proxy config mount; siblings/cloudflared do not.
    grep -q '/app/config/config\.toml' "$_ov" || continue
    assert ok "$_chain overlay mounts the config the scripts validate" \
        compose_mount_is "$_ov" "$_chain"
    overlays_checked=$((overlays_checked + 1))
done
assert ok "every shipped chain overlay was checked, not zero of them" \
    test "$overlays_checked" -ge 2

# --- Compose cannot be the way around the profile guards -------------------
#
# Everything above validates the profile, and none of it runs when someone types
# `docker compose up` directly. Two properties therefore have to hold in the compose
# file itself, and nothing else in this repo checks them.

compose_yaml="$PROJECT_DIR_EARLY/docker-compose.yaml"

# The attestation posture must have no default. A default meant the path that skips
# every check above silently chose simulated attestation.
assert ok "compose requires MODE explicitly" \
    grep -qE '^\s*-\s*MODE=\$\{MODE:\?' "$compose_yaml"
assert ok "compose requires SIMULATED_TEE explicitly" \
    grep -qE '^\s*-\s*SIMULATED_TEE=\$\{SIMULATED_TEE:\?' "$compose_yaml"
assert fail "compose has no MODE fallback" \
    grep -qE '^\s*-\s*MODE=\$\{MODE:-' "$compose_yaml"
assert fail "compose has no SIMULATED_TEE fallback" \
    grep -qE '^\s*-\s*SIMULATED_TEE=\$\{SIMULATED_TEE:-' "$compose_yaml"

# And the default network must be this project's own. Docker networks have no
# port-level isolation, so on a shared network every co-tenant container can reach
# tee-node's unauthenticated config server — which accepts POSTs setting the chain
# id, extension id, owner, governance set and proxy URL — plus the proxy's internal
# queue API, whatever `ports:` publishes.
assert fail "base compose does not join a shared external network" \
    grep -qE '^\s*external:\s*true' "$compose_yaml"

# --- Repo config sanity: shipped examples must satisfy their profile --------

PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
for ex in extension_proxy.coston.docker.toml.example extension_proxy.coston.toml.example \
          extension_proxy.coston2.docker.toml.example extension_proxy.coston2.toml.example; do
    f="$PROJECT_DIR/config/proxy/$ex"
    [[ -f "$f" ]] || continue
    # The examples ship fail-closed (allow_magic_pass=false) — they must at
    # minimum refuse the simulated profile rather than silently accept it.
    assert fail "example $ex refuses testnet-sim as shipped" check_cfg testnet-sim "$f"
    # And they are templates, not deployable configs: each still carries at least one
    # <placeholder> the operator must substitute, so confidential-space must refuse
    # them as shipped rather than approve a posture nothing can satisfy.
    assert fail "example $ex refuses confidential-space as shipped" check_cfg confidential-space "$f"
done

echo ""
if [[ $FAIL -gt 0 ]]; then
    echo -e "${RED}[test-profile-matrix] $FAIL failed, $PASS passed${NC}"
    exit 1
fi

# docs/testing.md states this count, and it had drifted twice (66 documented
# against 73 actual) with nothing to catch it: check-docs.sh matches on keywords,
# so a stale number is invisible to every check there. It is checked HERE because
# this is the only place the true number exists — two of the assertions above run
# once per shipped .example file, so counting `assert` lines statically is not
# exact, and a gate that has to guess is the kind that gets switched off.
DOC_TESTING="$PROJECT_DIR_EARLY/docs/testing.md"
if [[ -f "$DOC_TESTING" ]]; then
    documented="$(sed -nE 's/.*\(([0-9]+) cases\).*/\1/p' "$DOC_TESTING" | head -1)"
    if [[ -z "$documented" ]]; then
        echo -e "${RED}[test-profile-matrix] docs/testing.md no longer states a case count; it should say ($PASS cases)${NC}" >&2
        exit 1
    fi
    if [[ "$documented" != "$PASS" ]]; then
        echo -e "${RED}[test-profile-matrix] docs/testing.md says $documented cases, this suite ran $PASS — update the doc${NC}" >&2
        exit 1
    fi
fi

echo -e "${GREEN}[test-profile-matrix] all $PASS cases passed${NC}"
