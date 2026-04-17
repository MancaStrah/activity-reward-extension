package fccutils

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// --- The grant wire vector ---
//
// The grant layout lives in TWO modules that cannot import each other: this file's
// GrantPlaintext seals it, and parseAndVerifyGrant in go/internal/extension/grant.go
// opens it. Until these tests existed, nothing but a comment held the two together —
// while the analogous PROOF payload was pinned on both sides by
// SignPayloadVector.t.sol and TestSignPayloadCrossLanguageVector.
//
// A one-sided edit fails closed (the enclave answers errGrantEncoding), so nothing is
// forged by the drift — but every claim breaks, with no test firing and an error that
// names the encoding rather than the change that caused it.
//
// These constants are the fixed vector. TestGrantWireVector in go/internal/extension
// reconstructs the same grant from the same inputs and asserts the same hash, then
// feeds it through parseAndVerifyGrant. Changing either side alone fails one of them.
const (
	// vecTokenGrantKeccak is keccak256 of the ABI-encoded grant for the vector below.
	vecTokenGrantKeccak = "0x256cd8c88ece698e6d32e2a83ce4c75aa7177ab40fd7287a7e3b289e8ee56f40"
	// vecGrantDomain and vecPurposeDistance pin the tags themselves. They are half of
	// the duplicated wire format: a grant encoded with the right layout and the wrong
	// domain is rejected just as hard as a malformed one.
	vecGrantDomain     = "0x856f55bd199bdbfcead2abd5bafb42df5871fd5058ca122f838a78432f2d22eb"
	vecPurposeDistance = "0x44a9514a902189b255def42268d4a704ad2951efbb892391d027999e9cbbdc06"

	vecUser     = "0x00000000000000000000000000000000000000C3"
	vecContract = "0x00000000000000000000000000000000000000CC"
	vecChainID  = 114
	vecExpiry   = 1700000000
	vecToken    = "strava-test-token"
)

// The tags are derived from string literals that must not drift: the enclave compares
// the decoded domain against its own keccak of the same literal, so a typo on either
// side rejects every grant.
func TestGrantTagsMatchTheirLiterals(t *testing.T) {
	if got := GrantDomain.Hex(); got != vecGrantDomain {
		t.Errorf("GrantDomain = %s, want %s (keccak256(\"STRAVA_TOKEN_GRANT_V2\"))", got, vecGrantDomain)
	}
	if got := PurposeDistance.Hex(); got != vecPurposeDistance {
		t.Errorf("PurposeDistance = %s, want %s (keccak256(\"STRAVA_DISTANCE\"))", got, vecPurposeDistance)
	}
	if GrantDomain == PurposeDistance {
		t.Error("domain and purpose are the same value; they must be distinct tags")
	}
}

// TestGrantWireVector pins the encoder half of the cross-module wire format.
func TestGrantWireVector(t *testing.T) {
	plaintext, err := GrantPlaintext(
		PurposeDistance,
		common.HexToAddress(vecUser),
		common.HexToAddress(vecContract),
		big.NewInt(vecChainID),
		vecExpiry,
		vecToken,
	)
	if err != nil {
		t.Fatalf("GrantPlaintext: %v", err)
	}
	if got := crypto.Keccak256Hash(plaintext).Hex(); got != vecTokenGrantKeccak {
		t.Fatalf("grant wire format drifted: got %s, want %s\n"+
			"The enclave decodes this exact layout. If the change was intended, update the same\n"+
			"vector in go/internal/extension (TestGrantWireVector) and re-check that a grant\n"+
			"sealed by the old client is no longer accepted.", got, vecTokenGrantKeccak)
	}
}

// GrantPlaintext refuses the two inputs that would produce a grant the enclave can
// only reject: a nil chain id cannot bind the grant to a chain, and an empty token
// yields errGrantEmptyToken after a round trip through ECIES and the TEE.
func TestGrantPlaintextRejectsUnusableInput(t *testing.T) {
	user := common.HexToAddress(vecUser)
	contract := common.HexToAddress(vecContract)

	if _, err := GrantPlaintext(PurposeDistance, user, contract, nil, vecExpiry, vecToken); err == nil {
		t.Error("accepted a nil chainID")
	}
	if _, err := GrantPlaintext(PurposeDistance, user, contract, big.NewInt(vecChainID), vecExpiry, ""); err == nil {
		t.Error("accepted an empty token")
	}
}
