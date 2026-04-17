// check-proxy-config validates a tee-proxy config.toml against a TEE_PROFILE
// before anything is started, using the proxy's OWN parser and its OWN config
// types.
//
// Why this is a Go program and not the shell function it replaced
// ---------------------------------------------------------------
// The previous implementation read the [attestation] table with sed/grep/awk. That
// is not a weaker TOML reader, it is a DIFFERENT language: shell regexes do not
// implement TOML's lexis or its types, so a config that is perfectly valid TOML
// could satisfy every regex while the proxy decoded the same bytes to the
// "skip this check" value. Five such configs were demonstrated, and only three of
// them involved anything unusual:
//
//	expected_code_hashes = [      # a comment-only list is an EMPTY list; the '#'
//	  # sha256:...                # satisfied a "first char is not ] or space" regex
//	]
//	audience = ''                 # a literal string; two apostrophes are not a value
//	max_token_age = '0s'          # single quotes hid the zero from the regex
//	max_token_age = "0h0m0s"      # ORDINARY double quotes — no trick at all; the
//	max_token_age = "0s0ms"       # zero-detector regex cannot match a composite
//
// The last two are the reason this is a rewrite and not another regex. Teaching the
// old zero-detector about single quotes — the obvious minimal fix — leaves
// "0h0m0s" bypassing the freshness check, because ^0+(\.0+)?(ns|us|ms|s|m|h)?$
// structurally cannot match a duration written in more than one unit. There is no
// finite set of regexes that implements duration parsing; there is a parser that
// does, and the proxy already links it.
//
// So this reads the file the way the proxy will:
//
//	go-flare-common/pkg/toml  →  BurntSushi/toml   (the proxy's parser, verbatim)
//	tee-proxy/pkg/config      →  Attestation type  (the proxy's schema, verbatim)
//
// Both are already direct dependencies of this module (tools/go.mod pins
// tee-proxy v0.0.18, the version proxy/Dockerfile builds), so preflight and
// runtime cannot drift into two interpretations of the same file — which is the
// property the regex version could not have at any level of effort.
//
// What the parser now handles for free, which the shell had to refuse by name:
// duplicate tables, duplicate keys, the [[attestation]] array-of-tables form, the
// dotted attestation.key spelling, strings containing brackets or '#', and strings
// spanning lines. Three of those were previously refused as unreadable; the dotted
// and multi-line forms are valid TOML that the proxy reads, so refusing them was a
// fail-closed false alarm this fixes in the other direction.
//
// Why the posture is demanded at all
// ----------------------------------
// In the upstream [attestation] schema an empty string, an empty list, a zero
// duration or a false flag means "skip this check" — not "misconfigured". From
// tee-proxy@v0.0.18 pkg/attestation/verifier.go:
//
//	Audience:    cfg.Audience != ""            // :97
//	CodeHash:    len(cfg.ExpectedCodeHash) > 0 // :98
//	Platform:    len(cfg.ExpectedPlatform) > 0 // :99
//	DebugStatus: len(cfg.ExpectedDebugStatus) > 0
//	MaxTokenAge: cfg.MaxTokenAge > 0           // :101, again at :163
//	SecBoot:     cfg.RequireSecBoot            // :102
//
// and the struct comment says it outright: "Empty allowlists / zero values skip the
// corresponding check." Upstream's own Attestation.validate() enforces only
// MaxTokenAge >= 0 and code-hash hex validity — it requires no posture whatsoever.
// So this check is not a second opinion on a runtime check. On a confidential-space
// deployment it is the ONLY thing between an operator and an attestation that
// verifies nothing, which is why a half-filled section has to be refused here.
//
// Usage:
//
//	check-proxy-config -profile <local|testnet-sim|confidential-space> -config <path> [-full]
//
// Exit status is 0 when the config matches the profile and non-zero otherwise,
// with every reason printed to stderr — all of them, not just the first, so an
// operator filling in a template gets one list instead of one round trip per field.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	proxyconfig "github.com/flare-foundation/tee-proxy/pkg/config"

	gftoml "github.com/flare-foundation/go-flare-common/pkg/toml"
)

func main() {
	profile := flag.String("profile", "",
		"TEE profile to validate against: local, testnet-sim or confidential-space")
	cfgPath := flag.String("config", "", "path to the proxy config.toml")
	wantChainID := flag.String("chain-id", "",
		"when set, require the config's chain_id to equal this — the deployment's CHAIN_ID. "+
			"Empty skips the comparison: an unset CHAIN_ID has its own failure (empty TEE signatures)")
	full := flag.Bool("full", false,
		"additionally load the whole file with the proxy's own loader (tee-proxy config.Read), "+
			"which also rejects unknown fields — use on a real deployment config, not on a partial fixture")
	flag.Parse()

	if *cfgPath == "" || *profile == "" {
		fmt.Fprintln(os.Stderr, "[profile] ERROR: check-proxy-config requires -config and -profile")
		os.Exit(2)
	}

	problems := check(*cfgPath, *profile, *wantChainID, *full)
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "[profile] ERROR: %s\n", p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}

// check returns every reason the config does not match the profile, or nil.
func check(path, profile, wantChainID string, full bool) []string {
	// Decode with the proxy's parser into the proxy's struct. Unknown fields are
	// tolerated here on purpose: this reads ONE table out of a file that may be a
	// partial fixture, and rejecting the whole file over a key in some other
	// section would make the attestation verdict depend on unrelated content.
	// -full is where the strict, whole-file load happens.
	var proxy proxyconfig.Proxy
	if err := gftoml.ReadTo(path, &proxy, true); err != nil {
		// Duplicate tables/keys, type mismatches ([[attestation]], enable = "true"),
		// an unparseable duration and malformed strings all land here, named by the
		// parser and with a line number, rather than being silently read as "unset".
		return []string{fmt.Sprintf("%s is not readable as proxy TOML: %v", path, err)}
	}

	att := proxy.Attestation

	var problems []string

	// The proxy's chain_id must be the chain this deployment is for. The node signs
	// every response against CHAIN_ID and the proxy verifies against its own
	// chain_id, so a disagreement fails closed — as an opaque verification failure
	// with nothing pointing at the config. Naming it here is the same job every other
	// check in this file does. Profile-independent: a local devnet gets it wrong the
	// same way a testnet does.
	if wantChainID != "" {
		want, err := strconv.ParseUint(wantChainID, 10, 64)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf(
				"CHAIN_ID=%q is not a chain id, so it cannot be compared with %s", wantChainID, path))
		case proxy.ChainID != want:
			problems = append(problems, fmt.Sprintf(
				"%s sets chain_id = %d but this deployment is CHAIN_ID=%d.\n"+
					"  The node signs against CHAIN_ID and the proxy verifies against its own chain_id,\n"+
					"  so every signature would be rejected — and the symptom is a failed verification,\n"+
					"  not a message about this file. Either fix chain_id, or you are pointing at the\n"+
					"  wrong config for this chain.", path, proxy.ChainID, want))
		}
	}

	// Upstream refuses a negative max_token_age in Attestation.validate(), so the
	// proxy would not start. Say so here rather than at bootstrap-timeout time.
	if att.MaxTokenAge < 0 {
		return append(problems, fmt.Sprintf(
			"%s sets a negative max_token_age (%s); the proxy refuses to start on it", path, att.MaxTokenAge))
	}
	if full {
		// The proxy's own loader: applies its defaults, validates every section and
		// — unlike the read above — rejects unknown fields. A config that fails here
		// cannot start the proxy at all, whatever its attestation posture says.
		if _, err := proxyconfig.Read(path); err != nil {
			problems = append(problems, fmt.Sprintf(
				"%s is not loadable by the proxy: %v", path, err))
		}
	}

	switch profile {
	case "local":
		// Simulated attestation on a local devnet: the posture is not meaningful and
		// nothing signed here proves anything about the code. The file still had to
		// PARSE, which is why this returns after the read above rather than before it.
		return problems

	case "testnet-sim":
		if !att.Enable {
			problems = append(problems, fmt.Sprintf(
				"%s has no fail-closed [attestation] section (enable = true).\n"+
					"  Omitting the section falls back to the insecure upstream default (enable=false).\n"+
					"  For testnet-sim use:  enable = true, allow_magic_pass = true", path))
		}
		if !att.AllowMagicPass {
			problems = append(problems, fmt.Sprintf(
				"MODE=1 (testnet-sim) but %s sets allow_magic_pass = false.\n"+
					"  The simulated TEE presents the magic_pass sentinel, so this proxy would reject\n"+
					"  every bootstrap. Either set allow_magic_pass = true for this simulated dev run,\n"+
					"  or switch to TEE_PROFILE=confidential-space with MODE=0.", path))
		}
		return problems

	case "confidential-space":
		return append(problems, confidentialSpaceProblems(path, att)...)

	default:
		return append(problems, "TEE_PROFILE not resolved — call resolve_tee_profile first")
	}
}

// confidentialSpaceProblems demands the whole posture. Every check below names the
// consequence of the value being absent, because "empty" reads as configured and
// the failure it produces is a bootstrap timeout, not a message about this field.
func confidentialSpaceProblems(path string, att proxyconfig.Attestation) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if !att.Enable {
		add("confidential-space requires [attestation] enable = true in %s", path)
	}
	if att.AllowMagicPass {
		add("confidential-space requires allow_magic_pass = false in %s "+
			"(the magic_pass sentinel is a development bypass of attestation entirely)", path)
	}

	// The three allowlists. An empty list skips the check; an element that is the
	// empty string keeps the check running against a value nothing can match, which
	// fails closed but presents as a bootstrap timeout instead of this message.
	checkList(add, path, att.ExpectedCodeHashes, "expected_code_hashes",
		"an unpinned allowlist authorizes whatever image answers the bootstrap first")
	checkList(add, path, att.ExpectedPlatforms, "expected_platforms",
		"a missing or empty list makes the proxy skip the platform check rather than fail,\n"+
			"   so any hardware model that can produce a token is accepted")
	checkList(add, path, att.ExpectedDebugStatuses, "expected_debug_statuses",
		"a missing or empty list makes the proxy skip the debug-status check rather than fail,\n"+
			"   so a debuggable TEE — whose memory the host can inspect — is accepted")
	checkDebugStatuses(add, path, att.ExpectedDebugStatuses)

	// Hex-validate the pins with upstream's own parser, so a malformed codeHash is
	// named here rather than at proxy start.
	if len(att.ExpectedCodeHashes) > 0 {
		if _, err := att.ParsedCodeHashes(); err != nil {
			add("%s has an unparseable expected_code_hashes pin: %v", path, err)
		}
	}

	if strings.TrimSpace(att.Audience) == "" {
		add("confidential-space requires a non-empty audience in %s\n"+
			"  (a missing or empty audience makes the proxy skip the audience check rather than fail,\n"+
			"   so a token minted for some other relying party is accepted)", path)
	} else {
		checkPlaceholder(add, path, "audience", att.Audience)
	}

	// The whole point of the rewrite: `> 0` is the same comparison the proxy makes
	// (verifier.go:101 and :163), so every spelling of zero — 0, "0s", '0s',
	// "0h0m0s", "0s0ms" — lands here, and no future spelling can escape it.
	if att.MaxTokenAge <= 0 {
		add("confidential-space requires a positive max_token_age in %s (got %s)\n"+
			"  (a missing or zero duration makes the proxy skip the token freshness check rather\n"+
			"   than fail, so an attestation token captured arbitrarily long ago can be replayed)",
			path, durationForMessage(att.MaxTokenAge))
	} else if att.MaxTokenAge > maxTokenAgeCeiling {
		add("%s sets max_token_age = %s, which is longer than the attestation token's own\n"+
			"  lifetime, so it tightens nothing: the token's exp claim — always enforced, see\n"+
			"  jwt.WithExpirationRequired in go-flare-common's Confidential Space verifier — is\n"+
			"  then the only freshness bound in force. This control can only narrow that window,\n"+
			"  never widen it, so a value above %s is a number that does not do what it reads as.\n"+
			"  The usual cause is a unit typo (5000h for 5m, 24h for 24m).",
			path, att.MaxTokenAge, maxTokenAgeCeiling)
	}

	if !att.RequireSecBoot {
		add("confidential-space requires require_sec_boot = true in %s\n"+
			"  (false or unset makes the proxy skip the secure-boot check rather than fail,\n"+
			"   so a VM booted without a verified boot chain is accepted)", path)
	}

	return problems
}

// checkList refuses an empty allowlist, and an allowlist whose entries are empty or
// still carry a template <placeholder>.
func checkList(add func(string, ...any), path string, values []string, key, why string) {
	if len(values) == 0 {
		add("confidential-space requires a non-empty %s in %s\n  (%s)", key, path, why)
		return
	}
	for i, v := range values {
		if strings.TrimSpace(v) == "" {
			add("%s has an empty entry at %s[%d]; the check then runs against a value no\n"+
				"  attestation token can match, which surfaces as a bootstrap timeout rather than this message",
				path, key, i)
			continue
		}
		checkPlaceholder(add, path, fmt.Sprintf("%s[%d]", key, i), v)
	}
}

// maxTokenAgeCeiling is the largest max_token_age that can still mean something.
// A Confidential Space token carries its own exp and the parser enforces it
// (jwt.WithExpirationRequired), so this setting can only ever TIGHTEN the freshness
// window below the token's own lifetime — about an hour. Above that it is a no-op
// dressed as a control, which is the same reading problem as `max_token_age = 0`:
// the operator wrote a number and got no tightening. Refused rather than warned,
// because no deployment needs it and the check has no warning channel by design.
const maxTokenAgeCeiling = time.Hour

// debugStatusSecure is the only dbgstat value a production deployment should accept.
// The claim has exactly two documented values and upstream compares the configured
// list with slices.Contains against free-form strings — there is no enum — so both
// the insecure member and a typo read as a configured pin.
const debugStatusSecure = "disabled-since-boot"

// checkDebugStatuses is the reason a non-empty list is not enough here. Every other
// pin in this section fails CLOSED when its value is wrong: a bad audience, platform
// or code hash matches no token, so the deployment does not start. dbgstat is the one
// list where a perfectly valid entry — "enabled" — leaves the check running and
// passing for a TEE whose memory the host can read. That is not a misconfiguration
// the proxy can catch: it is exactly what the config asked for.
func checkDebugStatuses(add func(string, ...any), path string, values []string) {
	for i, v := range values {
		v = strings.TrimSpace(v)
		// Empty entries and unsubstituted placeholders are already named by checkList;
		// reporting them twice would make one mistake look like two.
		if v == "" || isPlaceholder(v) {
			continue
		}
		switch v {
		case debugStatusSecure:
			// The secure value.
		case "enabled":
			add("%s accepts expected_debug_statuses[%d] = %q, which admits a DEBUGGABLE TEE.\n"+
				"  The debug status is the one pin here that does not fail closed when it is wrong:\n"+
				"  the check runs, passes, and the host can inspect the enclave's memory — including\n"+
				"  the Strava tokens it decrypts. Use %q.", path, i, v, debugStatusSecure)
		default:
			add("%s sets expected_debug_statuses[%d] = %q, which is not a dbgstat value any\n"+
				"  Confidential Space token carries (the claim is %q or \"enabled\").\n"+
				"  Upstream compares this list as free-form strings, so a typo here is not an error:\n"+
				"  it is a check that can never match, and the deployment fails as a bootstrap\n"+
				"  timeout rather than as this line.", path, i, v, debugStatusSecure)
		}
	}
}

// isPlaceholder reports whether a value still carries an <angle-bracketed> template
// placeholder. Shared so checkDebugStatuses can defer to checkPlaceholder's verdict
// rather than re-deciding it differently.
func isPlaceholder(value string) bool {
	open := strings.Index(value, "<")
	if open < 0 {
		return false
	}
	return strings.Contains(value[open+1:], ">")
}

// checkPlaceholder catches a value still carrying an <angle-bracketed> placeholder
// from the shipped template. Such a value is unset in every sense that matters, but
// it is not EMPTY, so the checks above read it as filled in. The proxy does fail
// closed on it — an audience of "<attestation-token-audience>" matches no token's
// aud claim — and that is precisely the problem: the operator would get a bootstrap
// timeout to debug instead of a line naming the field they did not fill in.
func checkPlaceholder(add func(string, ...any), path, key, value string) {
	if !isPlaceholder(value) {
		return
	}
	add("%s leaves %s as an unsubstituted <placeholder> from the template (%q)\n"+
		"  (it is not empty, so the checks above read it as set — but no attestation token can\n"+
		"   ever match it, and the bootstrap would fail with a timeout rather than this message)",
		path, key, value)
}

// durationForMessage distinguishes "the key is absent" from "the key says zero".
// Both skip the check, but they are different mistakes to fix.
func durationForMessage(d time.Duration) string {
	if d == 0 {
		return "0 — unset, or explicitly zero"
	}
	return d.String()
}
