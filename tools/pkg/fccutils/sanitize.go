package fccutils

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxDisplayLen bounds a single sanitized field, counted in BYTES OF OUTPUT
// rather than in input runes. Escaping expands what it describes — one byte of
// invalid UTF-8 becomes four characters (\xNN) and an unprintable astral rune
// becomes eight (\uNNNNNN) — so a rune count would bound the input and leave the
// terminal to receive several times as much. The proxy is not trusted to bound
// what it returns, and an unbounded string can push the useful output off screen
// on its own.
const MaxDisplayLen = 512

// truncationMarker stands in for the discarded remainder. A field that was cut
// short must say so: silently returning the prefix reads as if that were all the
// proxy sent.
const truncationMarker = "…(truncated)"

// SanitizeForTerminal makes a proxy-supplied string safe to write to a
// terminal, and safe to read once it is there.
//
// Everything the extension prints from a /result response is attacker-influenced
// if the proxy is compromised or impersonated. Written raw, such a string can do
// more than look wrong:
//
//   - ESC (0x1B) introduces CSI/OSC sequences that reposition the cursor, clear
//     the screen, recolour later output, or set the window title; OSC 52 can
//     even load the paste buffer.
//   - CR alone rewrites the current line, so earlier output can be overwritten
//     after the fact.
//   - LF forges additional lines, which is enough to fake a plausible-looking
//     instruction or success message that the tool never emitted.
//   - Bidi overrides reorder glyphs, so what is displayed need not match the
//     bytes.
//
// Anything outside the printable set is therefore rendered visibly as \xNN or
// \uNNNN rather than dropped: a field that contained control bytes should look
// suspicious, not merely shorter. Invalid UTF-8 is escaped bytewise for the same
// reason.
//
// The result is at most MaxDisplayLen bytes plus the truncation marker, on every
// path. Because the budget is spent as the escapes are written, the loop also
// stops after at most that many input runes, so a multi-megabyte field costs no
// more to sanitize than a short one.
func SanitizeForTerminal(s string) string {
	var b strings.Builder
	b.Grow(min(len(s), MaxDisplayLen) + len(truncationMarker))

	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])

		// piece is what this one input rune contributes: an escape sequence, or
		// the rune itself. It is built before anything is committed so the budget
		// below can be charged the width the terminal will actually receive —
		// counting input runes instead would let 512 of them expand into 4 KiB.
		var piece string
		switch {
		case r == utf8.RuneError && size == 1:
			// Not valid UTF-8 — escape the raw byte.
			piece = fmt.Sprintf("\\x%02x", s[i])
		case r < 0x20 || r == 0x7F:
			// C0 controls and DEL, newlines and tabs included: a single field
			// must never be able to span lines or realign the output.
			piece = fmt.Sprintf("\\x%02x", r)
		case r >= 0x80 && r <= 0x9F:
			// C1 controls; some terminals still act on these.
			piece = fmt.Sprintf("\\x%02x", r)
		case isBidiControl(r):
			piece = fmt.Sprintf("\\u%04x", r)
		case !unicode.IsPrint(r):
			piece = fmt.Sprintf("\\u%04x", r)
		default:
			piece = s[i : i+size]
		}

		// Truncate here, on a piece boundary and before the piece is written: the
		// check sits on the single path every branch above funnels through — the
		// invalid-UTF-8 branch included — and an escape written half-way would
		// misdescribe the very byte it was there to expose.
		if b.Len()+len(piece) > MaxDisplayLen {
			b.WriteString(truncationMarker)
			break
		}
		b.WriteString(piece)
		i += size
	}

	return b.String()
}

// isBidiControl reports whether r reorders surrounding text, which lets the
// rendered form disagree with the underlying bytes.
func isBidiControl(r rune) bool {
	switch r {
	case '؜', // ARABIC LETTER MARK
		'‎', '‏', // LRM, RLM
		'‪', '‫', '‬', '‭', '‮', // LRE, RLE, PDF, LRO, RLO
		'⁦', '⁧', '⁨', '⁩': // LRI, RLI, FSI, PDI
		return true
	}
	return false
}
