package main

import (
	"encoding/json"
	"strings"
	"testing"

	"activity-reward-extension/tools/pkg/fccutils"
)

const (
	okHash = "0x1111111111111111111111111111111111111111111111111111111111111111"
	okAddr = "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471C3"
)

func validProof() *proof {
	return &proof{
		Timestamp:     1755600000,
		Challenge:     okHash,
		Caller:        okAddr,
		TeeID:         okAddr,
		Eligible:      true,
		DistanceKm:    12.4,
		DistanceX1000: 12400,
		MonthStart:    1754006400,
		AthleteHash:   okHash,
		Signature:     "0x" + strings.Repeat("22", 65),
		Message:       "eligible",
	}
}

func TestValidateProofAcceptsWellFormed(t *testing.T) {
	v, err := validateProof(validProof())
	if err != nil {
		t.Fatalf("validateProof rejected a well-formed proof: %v", err)
	}
	if len(v.Signature) != 65 {
		t.Errorf("signature length = %d, want 65", len(v.Signature))
	}
	// Every displayed value must be a re-encode of the parsed type, so it cannot
	// carry anything but 0x-hex regardless of what the proxy sent.
	for _, s := range []string{v.Challenge.Hex(), v.AthleteHash.Hex(), v.Caller.Hex(), v.TeeID.Hex()} {
		if !strings.HasPrefix(s, "0x") {
			t.Errorf("re-encoded value %q lost its 0x prefix", s)
		}
		if strings.ContainsAny(s, "'\";`$\\ \n\r\x1b") {
			t.Errorf("re-encoded value %q contains a shell or terminal metacharacter", s)
		}
	}
}

// A hostile proxy controls every string in this response. The command must refuse
// the proof rather than display attacker-chosen text as if the TEE had signed it.
func TestValidateProofRejectsHostileFields(t *testing.T) {
	shellBreakout := okHash[:10] + "'; rm -rf ~ ;'"
	cases := []struct {
		name   string
		mutate func(*proof)
	}{
		{"quote breakout in challenge", func(p *proof) { p.Challenge = shellBreakout }},
		{"quote breakout in athleteHash", func(p *proof) { p.AthleteHash = shellBreakout }},
		{"quote breakout in signature", func(p *proof) { p.Signature = shellBreakout }},
		{"command substitution in caller", func(p *proof) { p.Caller = "0x$(id)" }},
		{"backticks in teeId", func(p *proof) { p.TeeID = "0x`id`" }},
		{"escape sequence in challenge", func(p *proof) { p.Challenge = "\x1b[2J" + okHash }},
		{"newline forging output in caller", func(p *proof) { p.Caller = okAddr + "\ncaller: 0xdead" }},
		{"empty challenge", func(p *proof) { p.Challenge = "" }},
		{"empty signature", func(p *proof) { p.Signature = "" }},
		{"short address", func(p *proof) { p.Caller = "0x1bc4" }},
		{"hash where address expected", func(p *proof) { p.Caller = okHash }},
		{"non-hex signature", func(p *proof) { p.Signature = "0xzzzz" }},
		{"odd-length hash", func(p *proof) { p.Challenge = okHash + "1" }},
		{"negative timestamp", func(p *proof) { p.Timestamp = -1 }},
		{"negative distance", func(p *proof) { p.DistanceX1000 = -1 }},
		{"negative monthStart", func(p *proof) { p.MonthStart = -1 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProof()
			tc.mutate(p)
			v, err := validateProof(p)
			if err == nil {
				t.Fatalf("validateProof accepted a hostile proof (%s): %+v", tc.name, v)
			}
			// The rejection message is printed, so it must not smuggle the payload.
			if strings.ContainsAny(err.Error(), "\x1b\r\n") || strings.Contains(err.Error(), "rm -rf") {
				t.Errorf("error echoed the untrusted value: %q", err.Error())
			}
		})
	}
}

// The free-text fields are legitimately free text, so they are sanitized rather
// than rejected — but nothing live may survive into the terminal.
func TestFreeTextFieldsAreSanitizedNotTrusted(t *testing.T) {
	hostile := "all good\x1b]0;pwned\x07\rstatus: 0 OK\nsignature verified"
	got := fccutils.SanitizeForTerminal(hostile)
	if strings.ContainsAny(got, "\x1b\r\n\a") {
		t.Fatalf("live control bytes survived: %q", got)
	}
	if !strings.Contains(got, "pwned") {
		t.Error("visible text should be preserved, only the control bytes escaped")
	}
}

// The decode step must not panic or half-populate on arbitrary bytes: anything
// that is not a distance proof has to fall through to the sanitized data dump.
func TestNonProofPayloadDoesNotParseAsProof(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"signature":""}`,
		`[]`,
		`"just a string"`,
		`{"signature":123}`,
		`not json at all`,
	} {
		var p proof
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			continue // main() returns early on a decode error
		}
		if p.Signature == "" {
			continue // main() returns early on an absent signature
		}
		if _, err := validateProof(&p); err == nil {
			t.Errorf("payload %q was accepted as a valid proof", body)
		}
	}
}

// get-result already displayed the signed distanceX1000 rather than the unsigned
// distanceKm, but it parsed nothing against that float at all — leaving it free to
// reach a terminal through the raw data dump. A response whose two figures
// disagree did not come from this extension, so it is refused.
func TestValidateProofRejectsUnsignedDistanceContradictingTheSignedOne(t *testing.T) {
	cases := []struct {
		name  string
		km    float64
		x1000 int64
	}{
		{"km inflated while the signed value is zero", 42.0, 0},
		{"km understates a signed value that clears the bar", 0.5, 5000},
		{"km off by two metres", 12.4, 12402},
	}
	for _, c := range cases {
		p := validProof()
		p.DistanceKm, p.DistanceX1000 = c.km, c.x1000
		if _, err := validateProof(p); err == nil {
			t.Errorf("%s: validateProof accepted distanceKm=%v against distanceX1000=%d",
				c.name, c.km, c.x1000)
		}
	}
}
