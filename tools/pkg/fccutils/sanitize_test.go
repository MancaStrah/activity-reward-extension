package fccutils

import (
	"strings"
	"testing"
)

// Every string here is something a compromised or impersonated proxy could put
// in an ActionResult's log/message/data. None of it may reach the terminal as
// live control bytes, and none of it may forge extra output lines.
func TestSanitizeForTerminalNeutralizesControlSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"OSC window title", "ok\x1b]0;pwned\x07"},
		{"OSC 52 clipboard write", "ok\x1b]52;c;cHduZWQ=\x07"},
		{"CSI clear screen", "\x1b[2Jcleared"},
		{"CSI cursor move", "a\x1b[10;10Hb"},
		{"bare ESC", "a\x1bb"},
		{"carriage return overwrite", "real total: 5 km\rfake total: 500 km"},
		{"forged log line via LF", "status: 0\nstatus: 1 all good"},
		{"NUL byte", "a\x00b"},
		{"DEL", "a\x7fb"},
		{"C1 control", "a\x85b"},
		{"backspace erase", "500\x08\x08\x081"},
		{"bell", "a\ab"},
		{"tab realignment", "a\tb"},
		{"bidi override", "a‮b"},
		{"RLI isolate", "a⁦b"},
		{"zero width space", "a​b"},
		{"invalid utf8", "a\xffb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeForTerminal(tc.in)

			for _, r := range got {
				if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) {
					t.Fatalf("control rune %#U survived sanitization: %q -> %q", r, tc.in, got)
				}
				if isBidiControl(r) {
					t.Fatalf("bidi control %#U survived sanitization: %q -> %q", r, tc.in, got)
				}
			}
			if strings.ContainsAny(got, "\x1b\r\n\x00\a\b") {
				t.Fatalf("raw control byte survived: %q -> %q", tc.in, got)
			}
			if got == "" && tc.in != "" {
				t.Fatalf("everything was dropped for %q — escaped output must stay visible", tc.in)
			}
		})
	}
}

func TestSanitizeForTerminalKeepsOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"",
		"eligible: monthly distance 12.4 km",
		"0x1bc4BC718C675bb7aF5F3Aa99F516A5c0Cd471C3",
		"Insufficient reward pool balance.",
		"mesečna razdalja — 2 km",
	} {
		if got := SanitizeForTerminal(s); got != s {
			t.Errorf("SanitizeForTerminal(%q) = %q, want it unchanged", s, got)
		}
	}
}

// TestSanitizeForTerminalBoundsLength exercises the bound on every branch of the
// sanitizer, not just on printable ASCII — the one class where input runes and
// output bytes happen to coincide. A field of invalid UTF-8 expands 4x, an
// unprintable astral rune 8x, and the invalid-UTF-8 branch is a path of its own,
// so a bound that holds for "AAAA…" says nothing about what a hostile proxy can
// actually put on the terminal.
func TestSanitizeForTerminalBoundsLength(t *testing.T) {
	// The output may exceed the budget only by the marker itself.
	limit := MaxDisplayLen + len(truncationMarker)

	// Each input is one repeated rune, well past the budget in every encoding.
	const reps = 2000

	cases := []struct {
		name string
		in   string
		// piece is the output width of one escaped input rune. Whatever survives
		// must be a whole number of these: half an escape describes a byte that
		// was never there.
		piece int
	}{
		{"printable ascii", strings.Repeat("A", reps), 1},
		{"invalid utf8", strings.Repeat("\xff", reps), 4},              // \xff
		{"astral unprintable", strings.Repeat("\U0010FFFF", reps), 8},  // backslash-u plus six digits
		{"escape byte run", strings.Repeat("\x1b", reps), 4},           // \x1b
		{"mixed control run", strings.Repeat("\x00\r\n\a", reps/4), 4}, // \x00 …
		{"multibyte printable", strings.Repeat("ž", reps), 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeForTerminal(tc.in)

			if len(got) > limit {
				t.Errorf("output not bounded: %d bytes out of %d bytes of input, limit %d",
					len(got), len(tc.in), limit)
			}
			if !strings.HasSuffix(got, truncationMarker) {
				t.Fatalf("truncation must be visible: %d-byte output does not end in the marker", len(got))
			}

			kept := strings.TrimSuffix(got, truncationMarker)
			if len(kept)%tc.piece != 0 {
				t.Errorf("kept %d bytes, not a whole number of %d-byte escapes: the last one was cut in half",
					len(kept), tc.piece)
			}
			// Truncating must not become a way to smuggle a control byte past the
			// escaping — the reason the bound is checked before the write.
			for _, r := range kept {
				if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) || isBidiControl(r) {
					t.Fatalf("control rune %#U survived truncation", r)
				}
			}
		})
	}

	// Anything inside the budget passes through whole, marker included only when
	// something was actually dropped.
	if got := SanitizeForTerminal(strings.Repeat("A", MaxDisplayLen)); got != strings.Repeat("A", MaxDisplayLen) {
		t.Errorf("a field exactly at the budget was altered: %d bytes out", len(got))
	}
}
