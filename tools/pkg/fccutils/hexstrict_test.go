package fccutils

import (
	"strings"
	"testing"
)

const (
	goodHash = "0x1111111111111111111111111111111111111111111111111111111111111111"
	goodAddr = "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471C3"
)

// The lenient go-ethereum helpers turn every one of these into a plausible-looking
// hash or address. The strict parsers must reject them instead, because these
// values get displayed to an operator.
func TestStrictHashRejectsMalformed(t *testing.T) {
	bad := []struct {
		name, in string
	}{
		{"empty", ""},
		{"prefix only", "0x"},
		{"too short", "0x1111"},
		{"too long", goodHash + "11"},
		{"odd length", "0x111"},
		{"non-hex letter", "0x111111111111111111111111111111111111111111111111111111111111111z"},
		{"embedded quote", "0x1111'11111111111111111111111111111111111111111111111111111111111"},
		{"shell metacharacters", "0x11'; rm -rf ~ ;'"},
		{"double prefix", "0x0x" + strings.Repeat("11", 32)},
		{"whitespace", " " + goodHash},
		{"trailing newline", goodHash + "\n"},
		{"escape sequence", "\x1b[2J" + goodHash},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := StrictHash("challenge", tc.in); err == nil {
				t.Fatalf("StrictHash accepted %q", tc.in)
			}
		})
	}

	if h, err := StrictHash("challenge", goodHash); err != nil {
		t.Fatalf("StrictHash rejected a valid hash: %v", err)
	} else if h.Hex() != strings.ToLower(goodHash) {
		t.Errorf("round-trip mismatch: got %s", h.Hex())
	}

	// The unprefixed form is the same value.
	if _, err := StrictHash("challenge", strings.TrimPrefix(goodHash, "0x")); err != nil {
		t.Errorf("StrictHash rejected the unprefixed form: %v", err)
	}
}

func TestStrictAddressRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"", "0x", "0x1bc4", goodAddr + "11", "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471Cz",
		"0x1bc4'; echo hi ;'", goodHash /* 32 bytes, not 20 */, " " + goodAddr,
	} {
		if _, err := StrictAddress("caller", in); err == nil {
			t.Errorf("StrictAddress accepted %q", in)
		}
	}
	if a, err := StrictAddress("caller", goodAddr); err != nil {
		t.Fatalf("StrictAddress rejected a valid address: %v", err)
	} else if !strings.EqualFold(a.Hex(), goodAddr) {
		t.Errorf("round-trip mismatch: got %s", a.Hex())
	}
}

// A wrong address is not a malformed one: 40 hex digits stay 40 hex digits after
// a typo, so length and hex-ness cannot tell the intended address from a
// neighbour of it. The EIP-55 case pattern can, for any value that carries it.
func TestStrictAddressVerifiesEIP55Checksum(t *testing.T) {
	// Same 20 bytes as goodAddr, case scrambled: what a hand-retyped or
	// case-normalised-then-recapitalised paste looks like.
	const scrambled = "0x1BC4bc718c675BB7Af5f3aA99f516a5C0cD471c3"

	if _, err := StrictAddress("caller", goodAddr); err != nil {
		t.Errorf("a correctly checksummed address was rejected: %v", err)
	}
	if _, err := StrictAddress("caller", scrambled); err == nil {
		t.Error("StrictAddress accepted a mixed-case address whose EIP-55 checksum does not match")
	} else if strings.Contains(err.Error(), scrambled) {
		t.Errorf("error echoed the untrusted value: %q", err.Error())
	} else if !strings.Contains(err.Error(), "caller") {
		t.Errorf("error should name the field, got: %q", err.Error())
	}

	// Single-case forms carry no checksum — they are the usual shape in .env
	// files and JSON — and must keep parsing, prefixed or not.
	for _, in := range []string{
		strings.ToLower(goodAddr),
		"0x" + strings.ToUpper(strings.TrimPrefix(goodAddr, "0x")),
		strings.ToLower(strings.TrimPrefix(goodAddr, "0x")),
		"0X" + strings.TrimPrefix(goodAddr, "0x"), // 0X prefix, checksummed digits
		strings.TrimPrefix(goodAddr, "0x"),        // checksummed, unprefixed
		"0x" + strings.Repeat("11", 20),           // no letters at all: nothing to check
	} {
		a, err := StrictAddress("caller", in)
		if err != nil {
			t.Errorf("StrictAddress rejected %q: %v", in, err)
			continue
		}
		if !strings.EqualFold(a.Hex(), "0x"+strings.TrimPrefix(strings.TrimPrefix(in, "0x"), "0X")) {
			t.Errorf("round-trip mismatch for %q: got %s", in, a.Hex())
		}
	}
}

func TestStrictBytesBoundsAndValidates(t *testing.T) {
	if _, err := StrictBytes("signature", "", 100); err == nil {
		t.Error("StrictBytes accepted an empty string")
	}
	if _, err := StrictBytes("signature", "0xzz", 100); err == nil {
		t.Error("StrictBytes accepted non-hex")
	}
	if _, err := StrictBytes("signature", "0x"+strings.Repeat("11", 50), 10); err == nil {
		t.Error("StrictBytes ignored maxLen")
	}
	b, err := StrictBytes("signature", "0x"+strings.Repeat("11", 65), 1024)
	if err != nil {
		t.Fatalf("StrictBytes rejected a 65-byte signature: %v", err)
	}
	if len(b) != 65 {
		t.Errorf("got %d bytes, want 65", len(b))
	}
}

// An error message must not become the injection vector it was reporting.
func TestStrictParserErrorsDoNotEchoTheValue(t *testing.T) {
	hostile := "0x\x1b]0;pwned\x07'; rm -rf ~ ;'"
	_, err := StrictHash("challenge", hostile)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.ContainsAny(err.Error(), "\x1b\a") || strings.Contains(err.Error(), "rm -rf") {
		t.Errorf("error echoed the untrusted value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "challenge") {
		t.Errorf("error should name the field, got: %q", err.Error())
	}
}
