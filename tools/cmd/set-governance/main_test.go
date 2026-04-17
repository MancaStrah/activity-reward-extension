package main

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

const (
	addrA    = "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471C3"
	addrB    = "0x2222222222222222222222222222222222222222"
	addrC    = "0x3333333333333333333333333333333333333333"
	zeroAddr = "0x0000000000000000000000000000000000000000"
)

// Every one of these used to parse as "1", which is a legal threshold for any
// signer set and so passes every downstream check: a mistyped or unstripped
// value would quietly register a 1-of-N quorum.
func TestParseThresholdRejectsAnythingButAPositiveInteger(t *testing.T) {
	bad := []struct {
		name, in string
	}{
		{"zero", "0"},
		{"padded zero", "00"},
		{"word", "two"},
		{"decimal point", "3.0"},
		{"explicit sign", "+3"},
		{"hex", "0x2"},
		{"negative", "-1"},
		{"shorthand", "2of3"},
		{"inline comment", "2 # two of three"},
		{"trailing comment no space", "2#two"},
		{"two numbers", "2 3"},
		{"digit with unit", "2/3"},
		{"underscore separator", "1_0"},
		{"quoted", "\"2\""},
		{"shell metacharacters", "2; rm -rf ~"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseThreshold(tc.in)
			if err == nil {
				t.Fatalf("parseThreshold(%q) accepted it, returning %d", tc.in, got)
			}
			if got != 0 {
				t.Errorf("parseThreshold(%q) returned %d alongside an error; want 0", tc.in, got)
			}
			if !strings.Contains(err.Error(), "GOVERNANCE_THRESHOLD") {
				t.Errorf("error should name the env var, got: %q", err.Error())
			}
		})
	}
}

// Zero is rejected on its own terms, not lumped in with the unparsable values:
// it parses fine and is simply not a quorum.
func TestParseThresholdRejectsZeroAsMeaningless(t *testing.T) {
	_, err := parseThreshold("0")
	if err == nil {
		t.Fatal("parseThreshold accepted 0")
	}
	if !strings.Contains(err.Error(), "meaningless") {
		t.Errorf("error should say a zero threshold is meaningless, got: %q", err.Error())
	}
}

// Unset stays the documented default — the node's compose env leaves it unset in
// the single-signer case and both sides must agree on 1.
func TestParseThresholdUnsetDefaultsToOne(t *testing.T) {
	for _, in := range []string{"", " ", "\t", "\n", "   \t\n "} {
		got, err := parseThreshold(in)
		if err != nil {
			t.Errorf("parseThreshold(%q) failed: %v", in, err)
			continue
		}
		if got != 1 {
			t.Errorf("parseThreshold(%q) = %d, want the default 1", in, got)
		}
	}
}

func TestParseThresholdAcceptsPositiveIntegers(t *testing.T) {
	good := []struct {
		in   string
		want uint64
	}{
		{"1", 1},
		{"3", 3},
		{" 3", 3},
		{"3 ", 3},
		{"  3  ", 3},
		{"\t3\n", 3},
		{"12", 12},
	}
	for _, tc := range good {
		got, err := parseThreshold(tc.in)
		if err != nil {
			t.Errorf("parseThreshold(%q) rejected a valid value: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseThreshold(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The rejected value is untrusted input on its way to a terminal.
func TestParseThresholdErrorDoesNotEchoTheValue(t *testing.T) {
	hostile := "\x1b]0;pwned\x07'; rm -rf ~ ;'"
	_, err := parseThreshold(hostile)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.ContainsAny(err.Error(), "\x1b\a") || strings.Contains(err.Error(), "rm -rf") {
		t.Errorf("error echoed the untrusted value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "GOVERNANCE_THRESHOLD") {
		t.Errorf("error should name the env var, got: %q", err.Error())
	}
}

func TestParseSignersRejectsUnusableSets(t *testing.T) {
	bad := []struct {
		name, in string
	}{
		{"zero address", zeroAddr},
		{"zero address among valid ones", addrA + "," + zeroAddr},
		{"duplicate", addrA + "," + addrA},
		{"duplicate in different case", addrA + "," + strings.ToLower(addrA)},
		{"duplicate unprefixed and upper-cased", addrA + "," + strings.ToUpper(addrA[2:])},
		{"duplicate not adjacent", addrA + "," + addrB + "," + strings.ToLower(addrA)},
		{"truncated address", "0xdeadbeef"},
		{"non-hex", "notanaddress"},
		{"too long", addrA + "11"},
		{"non-hex digit", "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471Cz"},
		{"odd length", "0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471C"},
		{"double prefix", "0x0x" + strings.Repeat("11", 20)},
		{"shell metacharacters", "0x11'; rm -rf ~ ;'"},
		{"one bad entry among good ones", addrA + ",0xnope," + addrB},
		{"internal whitespace", "0x1bc4BC718C675bb7aF5F3Aa 99F516A5c0Cd471C3"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSigners(tc.in)
			if err == nil {
				t.Fatalf("parseSigners(%q) accepted it, returning %v", tc.in, got)
			}
			if got != nil {
				t.Errorf("parseSigners(%q) returned %v alongside an error; want nil", tc.in, got)
			}
			if !strings.Contains(err.Error(), "GOVERNANCE_SIGNERS") {
				t.Errorf("error should name the env var, got: %q", err.Error())
			}
		})
	}
}

// An empty list is not an error: main applies the deployer/INITIAL_OWNER default
// when nothing comes back, so the separators-only forms must land there too.
func TestParseSignersEmptyYieldsNoSigners(t *testing.T) {
	for _, in := range []string{"", " ", ",", ",,,", " , , ", "\t,\n"} {
		got, err := parseSigners(in)
		if err != nil {
			t.Errorf("parseSigners(%q) failed: %v", in, err)
			continue
		}
		if len(got) != 0 {
			t.Errorf("parseSigners(%q) = %v, want an empty slice", in, got)
		}
	}
}

func TestParseSignersAcceptsDistinctAddresses(t *testing.T) {
	got, err := parseSigners(" " + addrA + " , " + addrB + ",\t" + addrC + " ")
	if err != nil {
		t.Fatalf("parseSigners rejected a valid list: %v", err)
	}
	want := []common.Address{
		common.HexToAddress(addrA),
		common.HexToAddress(addrB),
		common.HexToAddress(addrC),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d signers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("signer %d = %s, want %s", i, got[i].Hex(), want[i].Hex())
		}
	}

	// The unprefixed form is the same address.
	single, err := parseSigners(strings.TrimPrefix(addrA, "0x"))
	if err != nil {
		t.Fatalf("parseSigners rejected the unprefixed form: %v", err)
	}
	if len(single) != 1 || single[0] != common.HexToAddress(addrA) {
		t.Errorf("got %v, want [%s]", single, addrA)
	}
}

func TestParseSignersErrorDoesNotEchoTheValue(t *testing.T) {
	hostile := "0x\x1b]0;pwned\x07'; rm -rf ~ ;'"
	_, err := parseSigners(hostile)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.ContainsAny(err.Error(), "\x1b\a") || strings.Contains(err.Error(), "rm -rf") {
		t.Errorf("error echoed the untrusted value: %q", err.Error())
	}
}

func TestParseInitialOwnerUnsetIsNoOverride(t *testing.T) {
	for _, in := range []string{"", " ", "\t\n"} {
		owner, set, err := parseInitialOwner(in)
		if err != nil {
			t.Errorf("parseInitialOwner(%q) failed: %v", in, err)
			continue
		}
		if set {
			t.Errorf("parseInitialOwner(%q) reported an override", in)
		}
		if owner != (common.Address{}) {
			t.Errorf("parseInitialOwner(%q) = %s, want the zero address", in, owner.Hex())
		}
	}
}

func TestParseInitialOwnerSetButMalformedIsFatal(t *testing.T) {
	bad := []struct {
		name, in string
	}{
		{"zero address", zeroAddr},
		{"truncated", "0xdeadbeef"},
		{"non-hex", "notanaddress"},
		{"too long", addrA + "11"},
		{"prefix only", "0x"},
		{"hash-length value", "0x" + strings.Repeat("11", 32)},
		{"shell metacharacters", "0x11'; rm -rf ~ ;'"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			owner, set, err := parseInitialOwner(tc.in)
			if err == nil {
				t.Fatalf("parseInitialOwner(%q) accepted it, returning %s (set=%v)", tc.in, owner.Hex(), set)
			}
			if set {
				t.Errorf("parseInitialOwner(%q) reported an override alongside an error", tc.in)
			}
			if !strings.Contains(err.Error(), "INITIAL_OWNER") {
				t.Errorf("error should name the env var, got: %q", err.Error())
			}
			if strings.Contains(err.Error(), "rm -rf") {
				t.Errorf("error echoed the untrusted value: %q", err.Error())
			}
		})
	}
}

func TestParseInitialOwnerAcceptsAnAddress(t *testing.T) {
	owner, set, err := parseInitialOwner("  " + addrA + "  ")
	if err != nil {
		t.Fatalf("parseInitialOwner rejected a valid address: %v", err)
	}
	if !set {
		t.Error("parseInitialOwner did not report an override")
	}
	if owner != common.HexToAddress(addrA) {
		t.Errorf("got %s, want %s", owner.Hex(), addrA)
	}
}
