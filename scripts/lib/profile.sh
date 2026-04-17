#!/usr/bin/env bash
#
# profile.sh — TEE deployment profile resolution and fail-fast matrix validation.
#
# A profile names one coherent MODE/attestation matrix:
#
#   local                local devnet, simulated attestation (MODE=1).
#                        Default when CHAIN=local.
#   testnet-sim          public testnet chain (coston/coston2) with the TEE as a
#                        plain local Docker container: MODE=1,
#                        SIMULATED_TEE=true, the proxy accepts the tee-node
#                        magic_pass sentinel. DEVELOPMENT ONLY —
#                        there is no hardware attestation, so nothing this
#                        deployment signs proves anything about the code.
#                        Must be selected EXPLICITLY (TEE_PROFILE=testnet-sim).
#   confidential-space   real GCP Confidential Space deployment: MODE=0,
#                        SIMULATED_TEE=false, LOCAL_MODE=false, proxy
#                        attestation fail-closed (allow_magic_pass=false) with
#                        the full posture pinned: expected_code_hashes,
#                        expected_platforms, expected_debug_statuses, audience,
#                        a non-zero max_token_age and require_sec_boot=true.
#
# Why this exists: MODE=1 against a proxy config with allow_magic_pass=false is
# a combination that can never bootstrap, and it pressures an operator into
# disabling attestation to get a successful start. These checks fail fast
# instead, before any container is started, and make the insecure combination
# an explicit opt-in.
#
# Sourced by start-services.sh / post-build.sh.
# Unit-tested by scripts/test-profile-matrix.sh (run it after changing this).

# resolve_tee_profile <chain>
# Normalizes the global TEE_PROFILE from the environment, inferring a value
# only where inference is unambiguous. Returns 1 (message on stderr) when the
# operator must choose explicitly.
resolve_tee_profile() {
    local chain="${1:?resolve_tee_profile requires the chain}"
    local mode="${MODE:-1}"   # docker-compose defaults MODE to 1

    if [[ -n "${TEE_PROFILE:-}" ]]; then
        case "$TEE_PROFILE" in
            local|testnet-sim|confidential-space) return 0 ;;
            *) echo "[profile] ERROR: unknown TEE_PROFILE '$TEE_PROFILE' (valid: local, testnet-sim, confidential-space)" >&2
               return 1 ;;
        esac
    fi

    if [[ "$chain" == "local" ]]; then
        TEE_PROFILE=local
        return 0
    fi
    if [[ "$mode" == "0" ]]; then
        TEE_PROFILE=confidential-space
        return 0
    fi

    cat >&2 <<'EOF'
[profile] ERROR: refusing to guess a TEE profile for a public chain with MODE=1.

  MODE=1 is SIMULATED attestation: the TEE emits the tee-node magic_pass
  sentinel instead of a Confidential Space attestation. On a public chain that
  is a development configuration and must be selected explicitly in .env:

    TEE_PROFILE=testnet-sim           # simulated TEE on a public testnet (dev)

  or configure the real thing:

    TEE_PROFILE=confidential-space    # MODE=0, fail-closed proxy attestation

  Templates: .env.example (testnet-sim), .env.confidential-space.example.
EOF
    return 1
}

# validate_tee_profile <chain>
# Fail-fast matrix check: returns 1 on any combination that either
# cannot bootstrap or silently downgrades attestation.
validate_tee_profile() {
    local chain="${1:?validate_tee_profile requires the chain}"
    local mode="${MODE:-1}"
    local sim="${SIMULATED_TEE:-true}"
    local localmode="${LOCAL_MODE:-true}"
    local bad=0

    case "${TEE_PROFILE:-}" in
        local)
            if [[ "$chain" != "local" ]]; then
                echo "[profile] ERROR: TEE_PROFILE=local but CHAIN=$chain — use testnet-sim or confidential-space on a public chain" >&2
                return 1
            fi
            if [[ "$mode" == "0" ]]; then
                echo "[profile] ERROR: TEE_PROFILE=local with MODE=0 — a local devnet has no Confidential Space attestation; use MODE=1" >&2
                return 1
            fi
            ;;
        testnet-sim)
            if [[ "$chain" == "local" ]]; then
                echo "[profile] ERROR: TEE_PROFILE=testnet-sim but CHAIN=local — use TEE_PROFILE=local" >&2
                return 1
            fi
            if [[ "$mode" != "1" ]]; then
                echo "[profile] ERROR: TEE_PROFILE=testnet-sim requires MODE=1 (got MODE=$mode) — MODE=0 means confidential-space" >&2
                return 1
            fi
            # MODE and SIMULATED_TEE must agree. MODE=1 makes the node emit the
            # magic_pass sentinel instead of a Confidential Space JWT, and only
            # the SIMULATED_TEE=true branch of fccutils.GetCodeHashAndPlatform
            # accepts that. With SIMULATED_TEE=false, register-tee feeds
            # "magic_pass" to the JWT parser and aborts, so the mismatch is
            # rejected here rather than discovered as an opaque post-build
            # failure.
            if [[ "$sim" != "true" ]]; then
                echo "[profile] ERROR: TEE_PROFILE=testnet-sim requires SIMULATED_TEE=true (got '$sim')." >&2
                echo "  MODE=1 emits the magic_pass sentinel, not a Confidential Space JWT, so" >&2
                echo "  register-tee would try to parse it as one and abort. Set" >&2
                echo "  SIMULATED_TEE=true for this simulated dev run, or switch to" >&2
                echo "  TEE_PROFILE=confidential-space with MODE=0 for real attestation." >&2
                return 1
            fi
            # LOCAL_MODE decides whether the dev-key fallbacks and the relaxed
            # URL gates are live, and an UNSET value is not neutral: several tools
            # read it as "local". start-proxy tests
            # `localMode == "" || localMode == "true"`, fccutils.AllowlistGate
            # documents "LOCAL_MODE=true (or unset)", and this file's own
            # `${LOCAL_MODE:-true}` default says the same. So on a public chain the
            # profile could validate cleanly while the tooling signed with a
            # well-known Hardhat key and skipped the secure-URL checks — the insecure
            # reading being the one you get by saying nothing.
            #
            # Demanded explicitly, and unset is reported as unset rather than folded
            # into 'true', because "I never set this" and "I chose this" need
            # different fixes. .env.example already ships LOCAL_MODE=false with this
            # exact reasoning; this makes the template's promise enforced.
            if [[ "${LOCAL_MODE:-}" != "false" ]]; then
                if [[ -z "${LOCAL_MODE:-}" ]]; then
                    echo "[profile] ERROR: TEE_PROFILE=testnet-sim requires LOCAL_MODE=false, but LOCAL_MODE is unset." >&2
                else
                    echo "[profile] ERROR: TEE_PROFILE=testnet-sim requires LOCAL_MODE=false (got '$LOCAL_MODE')." >&2
                fi
                echo "  This is a public chain. LOCAL_MODE=true — and unset, which start-proxy and" >&2
                echo "  the allow-listing gate both read as local — enables the local-devnet dev-key" >&2
                echo "  fallbacks and relaxes the secure-URL checks. Set LOCAL_MODE=false (as" >&2
                echo "  .env.example does), or use TEE_PROFILE=local on CHAIN=local." >&2
                return 1
            fi
            cat >&2 <<'EOF'
[profile] ================= SIMULATED TEE (testnet-sim) ==================
[profile]  MODE=1: the TEE runs as a plain Docker container and presents the
[profile]  magic_pass sentinel instead of real attestation. Signatures from
[profile]  this deployment prove NOTHING about the code that produced them.
[profile]  Development and testing only — never fund it with real value.
[profile] ================================================================
EOF
            ;;
        confidential-space)
            [[ "$mode" == "0" ]]        || { echo "[profile] ERROR: confidential-space requires MODE=0 (got MODE=$mode)" >&2; bad=1; }
            [[ "$sim" == "false" ]]     || { echo "[profile] ERROR: confidential-space requires SIMULATED_TEE=false (got '$sim')" >&2; bad=1; }
            [[ "$localmode" == "false" ]] || { echo "[profile] ERROR: confidential-space requires LOCAL_MODE=false (got '$localmode')" >&2; bad=1; }
            [[ "$chain" != "local" ]]   || { echo "[profile] ERROR: confidential-space on CHAIN=local makes no sense" >&2; bad=1; }
            return "$bad"
            ;;
        *)
            echo "[profile] ERROR: TEE_PROFILE not resolved — call resolve_tee_profile first" >&2
            return 1
            ;;
    esac
}

# proxy_config_name <docker|host> <chain>
# Prints the basename of the proxy config that mode uses on that chain.
#
# It lives here, beside the gate that reads it, because "which file is in force" and
# "is that file's [attestation] section acceptable" are one question. Splitting them
# is what let the two deployment modes drift apart: the gate was called on the Docker
# path only, so `--local` started whatever config the proxy discovered for itself —
# and what it discovered was chain-blind, so a testnet run loaded the local-devnet
# file along with whatever posture that file happened to carry.
#
# PAIRED DEFINITION: findProxyConfig() in tools/cmd/start-proxy/main.go must agree
# with this mapping for the host mode, and is unit-tested against it. start-services.sh
# exports PROXY_CONFIG so the binary normally uses the path this script validated
# rather than resolving one itself; the Go side matters for a standalone
# `go run ./cmd/start-proxy` with no script driving it.
proxy_config_name() {
    local mode="${1:?proxy_config_name requires docker|host}"
    local chain="${2:?proxy_config_name requires the chain}"
    local suffix=""

    case "$mode" in
        docker) suffix=".docker" ;;
        host)   suffix="" ;;
        *)      echo "[profile] ERROR: proxy_config_name: unknown mode '$mode' (valid: docker, host)" >&2
                return 1 ;;
    esac

    if [[ "$chain" == "local" ]]; then
        printf 'extension_proxy%s.toml' "$suffix"
    else
        printf 'extension_proxy.%s%s.toml' "$chain" "$suffix"
    fi
}

# resolve_proxy_config <docker|host> <chain> <project_dir>
# Prints the absolute path of the proxy config that mode uses on that chain, after
# confirming the file is there and its [attestation] section matches TEE_PROFILE.
#
# BOTH deployment modes go through this one function, and that is the whole point.
# validate_proxy_attestation_config used to be called from the Docker branch of
# start-services.sh alone, so `--local` started against whatever
# config/proxy/extension_proxy.toml said — and in a fresh clone that file carries no
# [attestation] section at all, which upstream reads as enable=false. A check with one
# call site is a check for one code path; the other path was not safer, only unchecked.
#
# It returns non-zero with a message on stderr rather than exiting, so the same
# function a deployment script dies on is callable from test-profile-matrix.sh. The
# path goes to stdout and nothing else does, so the caller can capture it.
resolve_proxy_config() {
    local mode="${1:?resolve_proxy_config requires docker|host}"
    local chain="${2:?resolve_proxy_config requires the chain}"
    local root="${3:?resolve_proxy_config requires the project dir}"
    local name path

    name="$(proxy_config_name "$mode" "$chain")" || return 1
    path="$root/config/proxy/$name"

    if [[ -d "$path" ]]; then
        # Compose bind-mounts the docker config as the proxy's config.toml; when it is
        # missing docker creates a directory in its place and `up` dies with an opaque
        # rootfs error, so name that cause where it is the likely one.
        if [[ "$mode" == "docker" ]]; then
            echo "[profile] ERROR: config/proxy/$name is a directory — docker created it on an earlier run when the config was missing." >&2
        else
            echo "[profile] ERROR: config/proxy/$name is a directory, not a proxy config file." >&2
        fi
        echo "  rm -rf config/proxy/$name && cp config/proxy/$name.example config/proxy/$name" >&2
        return 1
    fi
    if [[ ! -f "$path" ]]; then
        echo "[profile] ERROR: config/proxy/$name not found (it is gitignored — a fresh clone only has the .example)." >&2
        echo "  cp config/proxy/$name.example config/proxy/$name   # then fill in the [db] credentials" >&2
        return 1
    fi

    # The proxy's [attestation] section must match the profile, or bootstrap deadlocks
    # / silently downgrades. Checked before anything starts, not at /info-timeout time.
    if ! validate_proxy_attestation_config "$path"; then
        echo "[profile] ERROR: config/proxy/$name does not match TEE_PROFILE=${TEE_PROFILE:-unset}" >&2
        return 1
    fi

    # The config must also be for the chain this deployment targets. Cheap, and the
    # failure it replaces is the least legible one in the stack: the node signs
    # against CHAIN_ID, the proxy verifies against its own chain_id, and a
    # disagreement shows up as signatures that do not verify with nothing naming a
    # file. Also the second line of defence for the mount below — a config for
    # another chain is exactly what a wrong bind-mount produces.
    if ! validate_proxy_chain_id "$path"; then
        echo "[profile] ERROR: config/proxy/$name is not a config for CHAIN_ID=${CHAIN_ID:-unset}" >&2
        return 1
    fi

    # This is a real deployment config, not a fixture, so also prove the proxy can
    # actually load the whole file — its own loader, unknown-field strict. A mistyped
    # key or a missing [ports] entry otherwise surfaces as a container that exits
    # during `up`, which reads as an infrastructure problem rather than a typo.
    if ! validate_proxy_config_loadable "$path"; then
        echo "[profile] ERROR: config/proxy/$name is not a config the proxy can load" >&2
        return 1
    fi

    printf '%s' "$path"
}

# --- The proxy config pre-flight -------------------------------------------
#
# These two functions used to be ~130 lines of sed/grep/awk that read the
# [attestation] table by counting brackets and matching values with regexes. That
# was not a weaker TOML reader, it was a DIFFERENT language: a config can be
# perfectly valid TOML, satisfy every regex, and still decode — in the proxy — to
# the value that means "skip this check". Five such configs were demonstrated, and
# only three of them involved anything unusual:
#
#     expected_code_hashes = [        # a comment-only list is an EMPTY list
#       # sha256:...
#     ]
#     audience = ''                  # a literal string: two apostrophes, no value
#     max_token_age = '0s'           # single quotes hid the zero from the regex
#     max_token_age = "0h0m0s"       # ORDINARY double quotes — no trick at all
#     max_token_age = "0s0ms"
#
# The last two are why this is a rewrite rather than another regex. Teaching the
# old zero-detector about single quotes leaves "0h0m0s" through, because
# ^0+(\.0+)?(ns|us|ms|s|m|h)?$ structurally cannot match a duration written in more
# than one unit. Duration parsing is not expressible as a finite set of regexes;
# the proxy already links a parser that does it.
#
# So the reading is delegated to tools/cmd/check-proxy-config, which decodes the
# file with the proxy's own parser (go-flare-common/pkg/toml → BurntSushi) into the
# proxy's own schema (tee-proxy/pkg/config.Attestation, pinned at the version
# proxy/Dockerfile builds). Preflight and runtime therefore cannot drift into two
# interpretations of the same bytes — the property no amount of regex work could
# have given this file.
#
# Consequences worth knowing when reading the matrix tests:
#   - duplicate tables/keys, [[attestation]], type mismatches and unparseable
#     durations are now PARSER errors, named with a line number, instead of
#     hand-written refusals;
#   - the dotted `attestation.enable = ...` spelling and a multi-line string
#     elsewhere in the file are valid TOML that the proxy reads, so they are now
#     validated instead of refused. Those two refusals were fail-closed false
#     alarms, and removing them is part of the fix, not a relaxation of it.

# _proxy_config_checker <project-root>
# Prints the path to the check-proxy-config binary, building it if the binary is
# missing or older than its source. Built rather than `go run` because the matrix
# calls this ~30 times in one process and `go run` relinks every time.
_proxy_config_checker() {
    local root="${1:?_proxy_config_checker requires the project dir}"
    local bin="$root/out/bin/check-proxy-config"
    local src="$root/tools/cmd/check-proxy-config"

    # go.mod and go.sum are part of the staleness set, not just the sources. The
    # whole property this check buys is that the pre-flight parses with the SAME
    # pinned tee-proxy the deployment runs (check-versions.sh gates that pin against
    # proxy/Dockerfile). Watching only cmd/*.go would let a `tee-proxy` bump leave a
    # cached binary in place that still validates against the OLD schema — the
    # pre-flight and the proxy back to two interpretations, which is the entire bug
    # this replaced.
    if [[ ! -x "$bin" ]] || [[ -n "$(find "$src" "$root/tools/go.mod" "$root/tools/go.sum" \
            -newer "$bin" -print -quit 2>/dev/null)" ]]; then
        mkdir -p "$root/out/bin" || return 1
        # Output captured so a successful build stays silent; on failure the
        # compiler's own message is what the operator needs to see.
        local out
        if ! out="$(cd "$root/tools" && go build -o "$bin" ./cmd/check-proxy-config 2>&1)"; then
            echo "[profile] ERROR: could not build the proxy config checker (tools/cmd/check-proxy-config):" >&2
            echo "$out" >&2
            return 1
        fi
    fi
    printf '%s' "$bin"
}

# _project_root
# The repo root, derived from this file's own location (scripts/lib/profile.sh) so
# it does not depend on which script sourced it or from where.
_project_root() {
    cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}

# validate_proxy_attestation_config <config.toml>
# Cross-checks the mounted proxy config's [attestation] table against TEE_PROFILE,
# so the bootstrap deadlock (MODE=1 + allow_magic_pass=false) and the silent
# insecure default (no [attestation] section at all → enable=false upstream) are
# caught before `compose up` rather than surfacing as an opaque bootstrap timeout.
#
# On confidential-space it demands the whole posture, because in the upstream
# schema an empty list, an empty string, a zero duration or a false flag means
# "skip this check" rather than "misconfigured" — and upstream's own
# Attestation.validate() requires no posture at all, so on a real deployment this
# is the only thing standing there, not a second opinion on a runtime check.
#
# Returns non-zero with the reasons on stderr rather than exiting, so the same
# function a deployment script dies on is callable from test-profile-matrix.sh.
validate_proxy_attestation_config() {
    local cfg="${1:?validate_proxy_attestation_config requires the config path}"
    local root bin
    root="$(_project_root)" || return 1
    bin="$(_proxy_config_checker "$root")" || return 1
    "$bin" -profile "${TEE_PROFILE:-}" -config "$cfg"
}

# validate_proxy_chain_id <config.toml>
# Cross-checks the config's chain_id against CHAIN_ID. Kept a separate call for the
# same reason the two below are separate: one question per call, so the verdict on
# one property never depends on another.
#
# An unset CHAIN_ID skips the comparison rather than guessing. That is not a hole
# being left open — an unset CHAIN_ID has its own, louder failure (chainID 0, so
# every TEE signature comes back empty), and refusing here would only relabel it.
validate_proxy_chain_id() {
    local cfg="${1:?validate_proxy_chain_id requires the config path}"
    local root bin
    [[ -n "${CHAIN_ID:-}" ]] || return 0
    root="$(_project_root)" || return 1
    bin="$(_proxy_config_checker "$root")" || return 1
    # -profile local: this call is about chain_id only. The posture is the other
    # function's job, and on `local` the checker returns after the parse.
    "$bin" -profile local -config "$cfg" -chain-id "$CHAIN_ID"
}

# validate_proxy_config_loadable <config.toml>
# The other half of "preflight and runtime agree": runs the proxy's OWN loader
# (tee-proxy config.Read) over the whole file, which applies its defaults,
# validates every section and rejects unknown fields. A config that fails this
# cannot start the proxy at all, whatever its attestation posture says — so a
# mistyped key or a missing [ports] entry is named here instead of as a container
# that exits during `up`.
#
# Kept separate from the attestation check on purpose: this one is only meaningful
# against a complete deployment config, while the attestation posture is checked on
# its own so the verdict on one table never depends on unrelated sections.
validate_proxy_config_loadable() {
    local cfg="${1:?validate_proxy_config_loadable requires the config path}"
    local root bin
    root="$(_project_root)" || return 1
    bin="$(_proxy_config_checker "$root")" || return 1
    # -profile local: this call is about loadability only. The posture is the other
    # function's job, and both run on the deployment path.
    "$bin" -profile local -config "$cfg" -full
}
