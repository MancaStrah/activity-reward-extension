package main

import (
	"strings"
	"testing"
)

const (
	okHash = "0x1111111111111111111111111111111111111111111111111111111111111111"
	okAddr = "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471C3"
)

// okSig is a well-formed 65-byte ECDSA signature — the length claimReward accepts.
var okSig = "0x" + strings.Repeat("22", 65)

func validProof() *DistanceProof {
	return &DistanceProof{
		Timestamp:     1755600000,
		Challenge:     okHash,
		Caller:        okAddr,
		TeeID:         okAddr,
		Eligible:      true,
		DistanceKm:    12.4,
		DistanceX1000: 12400,
		MonthStart:    1754006400,
		AthleteHash:   okHash,
		Signature:     okSig,
	}
}

func TestConvertProofAcceptsWellFormed(t *testing.T) {
	got, err := convertProof(validProof())
	if err != nil {
		t.Fatalf("convertProof rejected a well-formed proof: %v", err)
	}
	if len(got.Signature) != 65 {
		t.Errorf("signature length = %d, want 65", len(got.Signature))
	}
	if got.Caller.Hex() != okAddr {
		t.Errorf("caller = %s, want %s", got.Caller.Hex(), okAddr)
	}
}

// Every string in the response comes from the proxy's unauthenticated result
// endpoint. convertProof is the only thing between it and a signed transaction,
// so a field that does not parse exactly must stop the claim here — not be
// zero-padded or truncated into a valid-looking value that travels on.
func TestConvertProofRejectsUnparseableFields(t *testing.T) {
	cases := []struct {
		name   string
		mangle func(*DistanceProof)
	}{
		{"short challenge", func(p *DistanceProof) { p.Challenge = "0x1111" }},
		{"non-hex challenge", func(p *DistanceProof) { p.Challenge = okHash[:64] + "zz" }},
		{"empty challenge", func(p *DistanceProof) { p.Challenge = "" }},
		{"short athleteHash", func(p *DistanceProof) { p.AthleteHash = "0xdeadbeef" }},
		{"short caller", func(p *DistanceProof) { p.Caller = "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471" }},
		{"non-hex caller", func(p *DistanceProof) { p.Caller = "0xZZc4BC718C675bb7aF5F3Aa99F516A5c0Cd471C3" }},
		{"bad EIP-55 checksum on caller", func(p *DistanceProof) { p.Caller = "0x1BC4BC718C675bb7aF5F3Aa99F516A5c0Cd471C3" }},
		{"short teeId", func(p *DistanceProof) { p.TeeID = "0x00" }},
		{"empty signature", func(p *DistanceProof) { p.Signature = "" }},
		{"odd-length signature", func(p *DistanceProof) { p.Signature = "0x222" }},
		{"oversized signature", func(p *DistanceProof) { p.Signature = "0x" + strings.Repeat("22", maxSignatureLen+1) }},
		// Not rejected by the ABI encoder: a negative int64 packs as a huge
		// uint256 by two's complement, so it has to be screened here.
		{"negative timestamp", func(p *DistanceProof) { p.Timestamp = -1 }},
		{"negative monthStart", func(p *DistanceProof) { p.MonthStart = -1 }},
		{"negative distance", func(p *DistanceProof) { p.DistanceX1000 = -1 }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validProof()
			c.mangle(p)
			if _, err := convertProof(p); err == nil {
				t.Error("convertProof accepted a value it cannot have parsed exactly")
			}
		})
	}
}

// The shape that made this worth fixing: a shell/terminal breakout smuggled
// through a structured field. It must not survive into a parsed proof.
func TestConvertProofRejectsInjectionAttempts(t *testing.T) {
	for _, payload := range []string{
		okHash[:10] + "'; rm -rf ~ ;'",
		okHash[:10] + "$(id)",
		"0x\x1b[2K\x1b[1;31mVERIFIED",
		okAddr + "\n0xdeadbeef",
	} {
		p := validProof()
		p.Challenge = payload
		if _, err := convertProof(p); err == nil {
			t.Errorf("convertProof accepted %q as a challenge", payload)
		}
		p = validProof()
		p.Caller = payload
		if _, err := convertProof(p); err == nil {
			t.Errorf("convertProof accepted %q as a caller", payload)
		}
	}
}

// distanceKm is the one field of the result the TEE does not sign, and it used to
// be the number this tool printed as "distance" immediately before asking whether
// to spend gas. It is no longer displayed — the signed distanceX1000 is — and a
// response whose two figures disagree is refused outright, because the extension
// derives both from one value.
func TestConvertProofRejectsUnsignedDistanceContradictingTheSignedOne(t *testing.T) {
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
		if _, err := convertProof(p); err == nil {
			t.Errorf("%s: convertProof accepted distanceKm=%v against distanceX1000=%d",
				c.name, c.km, c.x1000)
		}
	}
}

// What the operator is shown must be what the chain acts on. This pins the value
// of the expression main() prints; it cannot, from here, prove that main() still
// prints THAT expression rather than resp.DistanceKm — that half is held by the
// comment at the call site and by review.
func TestDisplayedDistanceComesFromTheSignedValue(t *testing.T) {
	p := validProof()
	got, err := convertProof(p)
	if err != nil {
		t.Fatalf("convertProof rejected a well-formed proof: %v", err)
	}
	// The expression claim-reward prints.
	if shown := float64(got.DistanceX1000.Int64()) / 1000; shown != 12.4 {
		t.Errorf("displayed distance = %v km, want 12.4 km (from the signed distanceX1000)", shown)
	}
}
