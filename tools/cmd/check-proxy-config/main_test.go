package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The complete confidential-space posture. Every case below is this config with
// exactly one thing changed, so a case can only fail for the reason it names.
const basePosture = `chain_id = 114
[addresses]
flare_systems_manager = "0xa4bcDF64Cdd5451b6ac3743B414124A6299B65FF"
relay = "0x5A0773Ff307Bf7C71a832dBB5312237fD3437f9F"
voter_registry = "0xB00cC45B4a7d3e1FEE684cFc4417998A1c183e6d"
[ports]
internal = "6663"
external = "6664"
[attestation]
enable = true
allow_magic_pass = false
audience = "https://relying-party.example"
expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
expected_platforms = ["AMD_SEV_SNP_VM"]
expected_debug_statuses = ["disabled-since-boot"]
max_token_age = "5m"
require_sec_boot = true
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return p
}

// replace swaps one whole assignment line, so a case changes exactly one value.
func replace(t *testing.T, key, line string) string {
	t.Helper()
	out := make([]string, 0, 24)
	found := false
	for _, l := range strings.Split(basePosture, "\n") {
		if strings.HasPrefix(l, key+" = ") {
			if line != "" {
				out = append(out, line)
			}
			found = true
			continue
		}
		out = append(out, l)
	}
	if !found {
		t.Fatalf("fixture has no %q line to replace — the base posture changed", key)
	}
	return write(t, strings.Join(out, "\n"))
}

// TestBasePostureIsAccepted is the control. Without it every case below could be
// passing because the fixture is broken rather than because the check works.
func TestBasePostureIsAccepted(t *testing.T) {
	if p := check(write(t, basePosture), "confidential-space", "", true); len(p) != 0 {
		t.Fatalf("the complete posture must be accepted, got: %v", p)
	}
}

// TestValidTomlBypassesAreRefusedAndNamed is the regression for the reported HIGH.
//
// Every config here is VALID TOML that the previous sed/grep/awk validator accepted
// with rc=0 while the proxy decoded it to the value that means "skip this check".
//
// Each case also asserts WHICH field the refusal names. That is the part a shell
// matrix case cannot pin cheaply, and it is the failure mode this repo keeps
// finding: a test that goes red for a reason other than the one in its name still
// passes, and stops testing what it claims the moment the code moves. Three earlier
// rounds found exactly that here.
func TestValidTomlBypassesAreRefusedAndNamed(t *testing.T) {
	cases := []struct {
		name string
		line string // the replacement assignment, or "" to delete the key
		key  string
		want string // substring the refusal must contain
	}{
		{
			// '#' after the opening bracket satisfied the old "first character is
			// not ] or whitespace" regex. In TOML this list is empty, and upstream
			// skips the check on len(list) == 0.
			name: "comment-only list is an empty list",
			key:  "expected_code_hashes",
			line: "expected_code_hashes = [\n  # sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899\n]",
			want: "non-empty expected_code_hashes",
		},
		{
			// A literal string has no escapes at all in TOML, so '' is the empty
			// string. The old regex captured the two apostrophes as a value.
			name: "literal empty audience",
			key:  "audience",
			line: "audience = ''",
			want: "non-empty audience",
		},
		{
			// Single quotes kept the zero out of reach of the zero-detector regex.
			name: "literal zero duration",
			key:  "max_token_age",
			line: "max_token_age = '0s'",
			want: "positive max_token_age",
		},
		{
			// The case that makes this a class rather than three bugs: ORDINARY
			// double quotes, no trick. ^0+(\.0+)?(ns|us|ms|s|m|h)?$ cannot match a
			// duration written in more than one unit, so patching the old regex to
			// understand single quotes would have left this one through.
			name: "composite zero duration 0h0m0s",
			key:  "max_token_age",
			line: `max_token_age = "0h0m0s"`,
			want: "positive max_token_age",
		},
		{
			name: "composite zero duration 0s0ms",
			key:  "max_token_age",
			line: `max_token_age = "0s0ms"`,
			want: "positive max_token_age",
		},
		{
			// Upstream's own Attestation.validate() refuses this, so the proxy would
			// not start. Named here rather than as a container exiting during `up`.
			name: "negative duration",
			key:  "max_token_age",
			line: `max_token_age = "-5m"`,
			want: "negative max_token_age",
		},
		{
			// len() > 0, so upstream runs the check — against a value no token can
			// match. Fail-closed, but as a bootstrap timeout rather than this line.
			name: "list of empty strings is not a pin",
			key:  "expected_platforms",
			line: `expected_platforms = [ "" ]`,
			want: "empty entry at expected_platforms[0]",
		},
		{
			// The old regex captured [A-Za-z]+ after '=', so a quoted bool matched
			// nothing and read as unset: the right verdict by accident. A typed read
			// calls it what it is.
			name: "bool written as a string is a type error",
			key:  "enable",
			line: `enable = "true"`,
			want: "not readable as proxy TOML",
		},
		{
			name: "absent audience",
			key:  "audience",
			line: "",
			want: "non-empty audience",
		},
		{
			name: "absent max_token_age",
			key:  "max_token_age",
			line: "",
			want: "positive max_token_age",
		},
		{
			name: "secure boot off",
			key:  "require_sec_boot",
			line: "require_sec_boot = false",
			want: "require_sec_boot = true",
		},
		{
			name: "magic_pass accepted on a real deployment",
			key:  "allow_magic_pass",
			line: "allow_magic_pass = true",
			want: "allow_magic_pass = false",
		},
		{
			// Not empty, so every non-empty check reads it as configured. The proxy
			// fails closed on it, which is the problem: the operator gets a timeout
			// instead of the name of the field they did not fill in.
			name: "unsubstituted placeholder in a list",
			key:  "expected_platforms",
			line: `expected_platforms = ["<your-machine-hwmodel>"]`,
			want: "unsubstituted <placeholder>",
		},
		{
			name: "unsubstituted placeholder in the audience",
			key:  "audience",
			line: `audience = "<attestation-token-audience>"`,
			want: "unsubstituted <placeholder>",
		},
		{
			// Upstream hex-validates the pins in Attestation.validate(). Catching it
			// here is what keeps preflight and runtime from disagreeing.
			name: "malformed code-hash pin",
			key:  "expected_code_hashes",
			line: `expected_code_hashes = ["sha256:aabbcc"]`,
			want: "unparseable expected_code_hashes",
		},
		{
			// The one pin in this section that does NOT fail closed when it is wrong.
			// A non-empty list satisfied every earlier version of this check, and
			// "enabled" is a perfectly valid dbgstat value — so the check ran, passed,
			// and admitted a TEE whose memory the host can read.
			name: "debuggable TEE is a valid value, and still refused",
			key:  "expected_debug_statuses",
			line: `expected_debug_statuses = ["enabled"]`,
			want: "admits a DEBUGGABLE TEE",
		},
		{
			// Same shape as the <placeholder> cases: not empty, so the presence check
			// reads it as configured, but no token carries it. Fails closed as a
			// bootstrap timeout instead of as a line naming the typo.
			name: "dbgstat typo can never match a token",
			key:  "expected_debug_statuses",
			line: `expected_debug_statuses = ["disabled"]`,
			want: "not a dbgstat value",
		},
		{
			// The zero-duration bypass this file exists for, in the other direction:
			// a value so large that the token's own exp is the only bound left. The
			// operator wrote a freshness window and got none of the tightening.
			name: "max_token_age above the token lifetime tightens nothing",
			key:  "max_token_age",
			line: `max_token_age = "876000h"`,
			want: "tightens nothing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := check(replace(t, tc.key, tc.line), "confidential-space", "", false)
			if len(problems) == 0 {
				t.Fatalf("accepted a config that skips an attestation check")
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("refused, but not for the stated reason.\n  want substring: %q\n  got:\n%s",
					tc.want, joined)
			}
		})
	}
}

// TestFormsThePreviousReaderRefusedWithoutCause covers the other direction. These
// are valid TOML the proxy reads perfectly well; the bracket-counting reader could
// not see them at all and refused the file. Refusing a good config teaches an
// operator that the gate is unreliable, which is how a gate ends up worked around.
func TestFormsThePreviousReaderRefusedWithoutCause(t *testing.T) {
	// The dotted keys must sit at ROOT level: a dotted key is relative to the
	// table it appears in, so the same lines after [ports] would declare
	// ports.attestation.* — a different table with a similar name. Worth stating,
	// because that mistake is exactly the kind of thing the old bracket-counting
	// reader could not have told apart either.
	dotted := `chain_id = 114
attestation.enable = true
attestation.allow_magic_pass = false
attestation.audience = "https://relying-party.example"
attestation.expected_code_hashes = ["sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"]
attestation.expected_platforms = ["AMD_SEV_SNP_VM"]
attestation.expected_debug_statuses = ["disabled-since-boot"]
attestation.max_token_age = "5m"
attestation.require_sec_boot = true
[addresses]
flare_systems_manager = "0xa4bcDF64Cdd5451b6ac3743B414124A6299B65FF"
relay = "0x5A0773Ff307Bf7C71a832dBB5312237fD3437f9F"
voter_registry = "0xB00cC45B4a7d3e1FEE684cFc4417998A1c183e6d"
[ports]
internal = "6663"
external = "6664"
`
	multiline := basePosture + `
[notes]
text = """
harmless prose that merely mentions [attestation]
"""
`
	awkwardStrings := strings.Replace(basePosture,
		`audience = "https://relying-party.example"`,
		`audience = 'https://relying-party.example/[tenant#frag'`, 1)

	for name, body := range map[string]string{
		"dotted keys name the same table":     dotted,
		"multi-line string elsewhere in file": multiline,
		"literal string holding '[' and '#'":  awkwardStrings,
		// A composite duration that is not zero. Kept under maxTokenAgeCeiling on
		// purpose: this case is about the PARSE (multi-unit durations read correctly
		// and non-zero ones pass), not about the ceiling, which has its own cases.
		"composite duration that is not zero": strings.Replace(basePosture, `max_token_age = "5m"`, `max_token_age = "1m30s"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			// full=false: these cases are about the attestation READ. The -full
			// load also rejects unknown fields, which legitimately refuses the
			// [notes] fixture below — a separate question, covered by
			// TestFullLoadIsASeparateQuestion.
			if p := check(write(t, body), "confidential-space", "", false); len(p) != 0 {
				t.Errorf("valid config refused: %v", p)
			}
		})
	}
}

// TestProfilesAskDifferentQuestions pins the per-profile behaviour, including that
// `local` still requires the file to PARSE. Returning before the read would make a
// malformed config invisible on the profile most runs use.
func TestProfilesAskDifferentQuestions(t *testing.T) {
	sim := `chain_id = 114
[attestation]
enable = true
allow_magic_pass = true
`
	failClosed := write(t, basePosture)

	if p := check(write(t, sim), "testnet-sim", "", false); len(p) != 0 {
		t.Errorf("testnet-sim must accept a magic_pass-accepting proxy: %v", p)
	}
	// The bootstrap deadlock: MODE=1 emits the sentinel, this proxy rejects it.
	if p := check(failClosed, "testnet-sim", "", false); len(p) == 0 {
		t.Error("testnet-sim must refuse a proxy that rejects magic_pass — nothing could ever bootstrap")
	}
	if p := check(failClosed, "confidential-space", "", false); len(p) != 0 {
		t.Errorf("confidential-space must accept the full posture: %v", p)
	}
	if p := check(write(t, sim), "confidential-space", "", false); len(p) == 0 {
		t.Error("confidential-space must refuse allow_magic_pass = true")
	}
	if p := check(write(t, sim), "local", "", false); len(p) != 0 {
		t.Errorf("local has no posture to check: %v", p)
	}
	// local skips the POSTURE, not the parse.
	if p := check(write(t, "[attestation\nenable = true\n"), "local", "", false); len(p) == 0 {
		t.Error("local must still refuse a file that is not TOML")
	}
	if p := check(failClosed, "bogus-profile", "", false); len(p) == 0 {
		t.Error("an unresolved profile must refuse rather than default to a permissive answer")
	}
}

// TestDuplicatesAreParserErrors: TOML forbids both, and which one wins used to
// decide whether a check ran at all. The parser rejects them, so the hand-written
// refusals these replaced are gone rather than merely unused.
func TestDuplicatesAreParserErrors(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate [attestation] table": basePosture + "\n[attestation]\nenable = false\n",
		"duplicate key in the table":    basePosture + "enable = false\n",
		"[[attestation]] array of tables": strings.Replace(
			basePosture, "[attestation]", "[[attestation]]", 1),
	} {
		t.Run(name, func(t *testing.T) {
			problems := check(write(t, body), "confidential-space", "", false)
			if len(problems) == 0 {
				t.Fatal("accepted a config TOML forbids")
			}
			if !strings.Contains(strings.Join(problems, "\n"), "not readable as proxy TOML") {
				t.Errorf("refused, but not as a parse error: %v", problems)
			}
		})
	}
}

// TestFullLoadIsASeparateQuestion: a config can have a perfect posture and still be
// unable to start the proxy, and the reverse. Neither check may mask the other.
func TestFullLoadIsASeparateQuestion(t *testing.T) {
	// Unknown field: upstream reads with allowUnknownFields=false, so this aborts
	// the proxy at start. Only the -full load sees it.
	typo := strings.Replace(basePosture, "require_sec_boot = true", "require_sec_boott = true", 1)
	if p := check(write(t, typo), "local", "", false); len(p) != 0 {
		t.Errorf("the attestation read tolerates unknown fields by design: %v", p)
	}
	if p := check(write(t, typo), "local", "", true); len(p) == 0 {
		t.Error("-full must reject a mistyped key: the proxy's own loader does")
	}

	// Missing section: the posture is untouched, the proxy still cannot start.
	noPorts := strings.Replace(basePosture, "external = \"6664\"\n", "", 1)
	if p := check(write(t, noPorts), "confidential-space", "", false); len(p) != 0 {
		t.Errorf("posture check must not depend on [ports]: %v", p)
	}
	if p := check(write(t, noPorts), "confidential-space", "", true); len(p) == 0 {
		t.Error("-full must reject a config missing ports.external")
	}
}

// TestEveryProblemIsReported: an operator filling in a template should get one list,
// not one round trip per field. Asserted because the natural implementation returns
// on the first problem.
func TestEveryProblemIsReported(t *testing.T) {
	bare := `chain_id = 114
[attestation]
enable = true
`
	problems := check(write(t, bare), "confidential-space", "", false)
	// code hashes, platforms, debug statuses, audience, max_token_age, sec_boot.
	if len(problems) < 6 {
		t.Errorf("expected every missing posture key to be listed, got %d:\n%s",
			len(problems), strings.Join(problems, "\n"))
	}
}

// TestChainIDIsComparedWhenGiven pins the cross-check that names a config belonging
// to another chain. The failure it replaces is the worst kind to debug: the node
// signs against CHAIN_ID, the proxy verifies against its own chain_id, and a
// disagreement surfaces as signatures that do not verify — with nothing anywhere
// pointing at the file. Profile-independent, so it is asserted on `local`, where no
// posture check can be the thing that fails instead.
func TestChainIDIsComparedWhenGiven(t *testing.T) {
	// basePosture carries chain_id = 114.
	cfg := write(t, basePosture)

	if p := check(cfg, "local", "114", false); len(p) != 0 {
		t.Fatalf("matching chain id must be accepted, got: %v", p)
	}

	p := check(cfg, "local", "16", false)
	if len(p) == 0 {
		t.Fatal("a config for another chain must be refused")
	}
	if !strings.Contains(p[0], "chain_id = 114") || !strings.Contains(p[0], "CHAIN_ID=16") {
		t.Errorf("the refusal must name both values, got: %v", p)
	}

	// Empty means "not given" — an unset CHAIN_ID has its own, different failure
	// (empty TEE signatures), and guessing here would refuse every fixture.
	if p := check(cfg, "local", "", false); len(p) != 0 {
		t.Errorf("no chain id given must skip the comparison, got: %v", p)
	}

	// A CHAIN_ID that is not a number is refused rather than silently not compared:
	// skipping on unparseable input is how a check stops running without saying so.
	if p := check(cfg, "local", "0x72", false); len(p) == 0 {
		t.Error("an unparseable CHAIN_ID must be refused, not ignored")
	}
}
