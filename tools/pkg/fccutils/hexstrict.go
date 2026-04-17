package fccutils

import (
	"encoding/hex"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
)

// The go-ethereum HexToHash/HexToAddress helpers are lenient by design: they
// silently zero-pad, left-truncate and ignore malformed input, so a hostile
// value round-trips into a well-formed-looking hash or address instead of an
// error. That is acceptable where the result is only compared on-chain, and not
// acceptable where it is displayed to an operator or used to build a command —
// there the value must either parse exactly or be rejected.

// StrictHash parses exactly 32 bytes of hex, with an optional 0x prefix.
func StrictHash(field, s string) (common.Hash, error) {
	b, err := strictHexBytes(field, s)
	if err != nil {
		return common.Hash{}, err
	}
	if len(b) != common.HashLength {
		return common.Hash{}, errors.Errorf("%s: expected %d bytes, got %d", field, common.HashLength, len(b))
	}
	return common.BytesToHash(b), nil
}

// StrictAddress parses exactly 20 bytes of hex, with an optional 0x prefix, and
// verifies the EIP-55 checksum when the value carries one.
//
// Length and hex-ness alone catch a mangled address, not a wrong one: inside 40
// valid hex digits a transposition or a single mistyped character is still 40
// valid hex digits, and the result is an address that exists, belongs to nobody,
// and is about to be written on-chain or displayed as if it were the intended
// one. EIP-55 encodes a checksum in the letter case, so a mixed-case value can
// be checked against itself.
func StrictAddress(field, s string) (common.Address, error) {
	b, err := strictHexBytes(field, s)
	if err != nil {
		return common.Address{}, err
	}
	if len(b) != common.AddressLength {
		return common.Address{}, errors.Errorf("%s: expected %d bytes, got %d", field, common.AddressLength, len(b))
	}
	if err := checkAddressChecksum(field, s); err != nil {
		return common.Address{}, err
	}
	return common.BytesToAddress(b), nil
}

// checkAddressChecksum verifies the EIP-55 checksum of a mixed-case address.
//
// Only mixed case is checked, and that is not a loophole: the checksum IS the
// case pattern, so an all-lowercase or all-uppercase address carries no checksum
// to verify. Both are the ordinary shape of an address in a .env file, in JSON,
// or pasted from a block explorer's raw view, and rejecting them would reject
// correct input. What this does catch is the case that matters — a value that
// claims a checksum by being mixed-case and then fails it.
//
// As everywhere in this file, the error names the field and the problem: the
// value is untrusted input on its way to a terminal.
func checkAddressChecksum(field, s string) error {
	digits := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if digits == strings.ToLower(digits) || digits == strings.ToUpper(digits) {
		return nil
	}
	// The 0x form is required: ValidChecksum compares the original string against
	// the canonical rendering, which is prefixed. The value has already been
	// validated as exactly 20 bytes of clean hex, so a parse failure here would
	// mean the two disagree about what an address is — treat it as a failure too.
	mixed, err := common.NewMixedcaseAddressFromString("0x" + digits)
	if err != nil || !mixed.ValidChecksum() {
		return errors.Errorf("%s: EIP-55 checksum does not match; either fix the letter case or pass the address in lower case", field)
	}
	return nil
}

// StrictBytes parses a variable-length hex string, with an optional 0x prefix.
// maxLen bounds the result; pass 0 for no bound.
func StrictBytes(field, s string, maxLen int) ([]byte, error) {
	b, err := strictHexBytes(field, s)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.Errorf("%s: empty", field)
	}
	if maxLen > 0 && len(b) > maxLen {
		return nil, errors.Errorf("%s: %d bytes exceeds the %d-byte maximum", field, len(b), maxLen)
	}
	return b, nil
}

// strictHexBytes rejects anything that is not clean, even-length hex. The field
// name is echoed rather than the value: the value is untrusted and must not
// reach the terminal unsanitized through an error string.
func strictHexBytes(field, s string) ([]byte, error) {
	if s == "" {
		return nil, errors.Errorf("%s: empty", field)
	}
	// Only a leading 0x/0X is tolerated; anything else must be pure hex.
	trimmed := s
	if len(trimmed) >= 2 && trimmed[0] == '0' && (trimmed[1] == 'x' || trimmed[1] == 'X') {
		trimmed = trimmed[2:]
	}
	if trimmed == "" {
		return nil, errors.Errorf("%s: no digits after the 0x prefix", field)
	}
	if strings.ContainsAny(trimmed, "xX") {
		return nil, errors.Errorf("%s: malformed hex", field)
	}
	b, err := hex.DecodeString(trimmed)
	if err != nil {
		// hex.DecodeString embeds the offending rune; keep only the shape.
		if len(trimmed)%2 != 0 {
			return nil, errors.Errorf("%s: odd-length hex string", field)
		}
		return nil, errors.Errorf("%s: not valid hex", field)
	}
	return b, nil
}
