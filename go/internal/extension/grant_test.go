package extension

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// --- The grant wire vector, decoder side ---
//
// The grant layout lives in TWO modules that cannot import each other:
// tools/pkg/fccutils/grant.go seals it (GrantPlaintext, used by encrypt-token,
// claim-reward and run-test) and this package opens it (parseAndVerifyGrant). Only a
// comment held them together, while the analogous PROOF payload was pinned on both
// sides by SignPayloadVector.t.sol and TestSignPayloadCrossLanguageVector.
//
// The rest of this package's grant tests build their input with buildGrant, which packs
// using grantArgs — the layout under test. That proves the decoder is self-consistent,
// not that it agrees with the client, so a layout change made here alone would leave
// them all green while every real claim failed with errGrantEncoding.
//
// TestGrantWireVector in tools/pkg/fccutils asserts the encoder produces exactly the
// bytes below. Changing either module alone fails one of the two.
const (
	vecTokenGrantKeccak = "0x256cd8c88ece698e6d32e2a83ce4c75aa7177ab40fd7287a7e3b289e8ee56f40"
	vecGrantDomain      = "0x856f55bd199bdbfcead2abd5bafb42df5871fd5058ca122f838a78432f2d22eb"
	vecPurposeDistance  = "0x44a9514a902189b255def42268d4a704ad2951efbb892391d027999e9cbbdc06"

	vecUser     = "0x00000000000000000000000000000000000000C3"
	vecContract = "0x00000000000000000000000000000000000000CC"
	vecChainID  = 114
	vecExpiry   = 1700000000
	vecToken    = "strava-test-token"
)

// vecGrantPlaintext is the sealed grant for the vector above, as a literal rather than
// as something this package computes.
//
// That distinction is the point. A hash of a locally packed grant catches a changed
// field count, a changed type, or a changed tag — but NOT two adjacent fields of the
// same type being read in the wrong order, because the encoding is identical either
// way. This layout has two such pairs: (user, verifyingContract) and
// (chainId, expiry). Swapping either in parseAndVerifyGrant's destructuring would make
// the enclave compare a grant's chain id against its expiry, or its user against the
// contract, and reject every genuine grant — and a test that packed its own input
// would swap in exactly the same way and stay green. Parsing a fixed literal is what
// closes that.
//
// One 32-byte word per line, in layout order:
//
//	domain, purpose, user, verifyingContract, chainId, expiry,
//	token offset, token length, token bytes
const vecGrantPlaintext = "" +
	"856f55bd199bdbfcead2abd5bafb42df5871fd5058ca122f838a78432f2d22eb" + // domain
	"44a9514a902189b255def42268d4a704ad2951efbb892391d027999e9cbbdc06" + // purpose
	"00000000000000000000000000000000000000000000000000000000000000c3" + // user
	"00000000000000000000000000000000000000000000000000000000000000cc" + // verifyingContract
	"0000000000000000000000000000000000000000000000000000000000000072" + // chainId 114
	"000000000000000000000000000000000000000000000000000000006553f100" + // expiry 1700000000
	"00000000000000000000000000000000000000000000000000000000000000e0" + // token offset
	"0000000000000000000000000000000000000000000000000000000000000011" + // token length 17
	"7374726176612d746573742d746f6b656e000000000000000000000000000000" //   "strava-test-token"

// The enclave rejects a grant whose domain or purpose does not equal its own keccak of
// a string literal. The client computes the same tags from the same literals in a
// different module, so a typo on either side rejects every grant.
func TestGrantTagsMatchTheirLiterals(t *testing.T) {
	if got := grantDomain.Hex(); got != vecGrantDomain {
		t.Errorf("grantDomain = %s, want %s (keccak256(\"STRAVA_TOKEN_GRANT_V2\"))", got, vecGrantDomain)
	}
	if got := purposeDistance.Hex(); got != vecPurposeDistance {
		t.Errorf("purposeDistance = %s, want %s (keccak256(\"STRAVA_DISTANCE\"))", got, vecPurposeDistance)
	}
	if grantDomain == purposeDistance {
		t.Error("domain and purpose are the same value; they must be distinct tags")
	}
}

// TestGrantWireVector pins the decoder half of the cross-module wire format: the
// enclave must open a grant the client sealed, byte-for-byte, and read every field back
// into the slot it belongs in.
func TestGrantWireVector(t *testing.T) {
	plaintext, err := hex.DecodeString(vecGrantPlaintext)
	if err != nil {
		t.Fatalf("decoding the vector: %v", err)
	}
	if got := crypto.Keccak256Hash(plaintext).Hex(); got != vecTokenGrantKeccak {
		t.Fatalf("the vector literal and its hash disagree: got %s, want %s", got, vecTokenGrantKeccak)
	}

	user := common.HexToAddress(vecUser)
	contract := common.HexToAddress(vecContract)

	token, err := parseAndVerifyGrant(plaintext, grantContext{
		caller:            user,
		verifyingContract: contract,
		chainID:           big.NewInt(vecChainID),
		purpose:           purposeDistance,
		now:               vecExpiry - 3600,
	})
	if err != nil {
		t.Fatalf("parseAndVerifyGrant rejected the client's own wire format: %v\n"+
			"tools/pkg/fccutils.GrantPlaintext produces exactly these bytes. If the layout\n"+
			"changed intentionally, update the vector in BOTH modules — and note that a grant\n"+
			"sealed by the old client will no longer be accepted.", err)
	}
	if token != vecToken {
		t.Errorf("token = %q, want %q", token, vecToken)
	}
}

// Each binding is checked against the instruction, never against the ciphertext, so a
// grant that is genuine but sealed for someone else must still be refused. Driving that
// from the fixed literal — rather than from a locally packed grant — is what proves the
// enclave reads each field out of the slot the client wrote it into.
func TestGrantWireVectorBindingsAreEnforced(t *testing.T) {
	plaintext, err := hex.DecodeString(vecGrantPlaintext)
	if err != nil {
		t.Fatalf("decoding the vector: %v", err)
	}
	base := grantContext{
		caller:            common.HexToAddress(vecUser),
		verifyingContract: common.HexToAddress(vecContract),
		chainID:           big.NewInt(vecChainID),
		purpose:           purposeDistance,
		now:               vecExpiry - 3600,
	}
	other := common.HexToAddress("0x00000000000000000000000000000000000000AA")

	cases := []struct {
		name string
		ctx  grantContext
		want error
	}{
		{"another caller", func() grantContext { c := base; c.caller = other; return c }(), errGrantUser},
		{"another contract", func() grantContext { c := base; c.verifyingContract = other; return c }(), errGrantContract},
		{"another chain", func() grantContext { c := base; c.chainID = big.NewInt(16); return c }(), errGrantChain},
		{"another purpose", func() grantContext { c := base; c.purpose = grantDomain; return c }(), errGrantPurpose},
		{"after expiry", func() grantContext { c := base; c.now = vecExpiry; return c }(), errGrantExpiry},
	}
	for _, c := range cases {
		if _, err := parseAndVerifyGrant(plaintext, c.ctx); err != c.want {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}
