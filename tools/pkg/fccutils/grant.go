package fccutils

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The domain/purpose tags and the ABI layout below MUST match
// go/internal/extension/grant.go byte-for-byte. The tools module is intentionally
// independent of the extension implementation, so the layout is duplicated here
// (like the proof structs in run-test) rather than imported.
var (
	// GrantDomain scopes the encrypted plaintext to "a Strava token grant".
	GrantDomain = crypto.Keccak256Hash([]byte("STRAVA_TOKEN_GRANT_V2"))
	// PurposeDistance binds a grant to the extension's single operation.
	PurposeDistance = crypto.Keccak256Hash([]byte("STRAVA_DISTANCE"))
)

var grantArgs abi.Arguments

func init() {
	bytes32Ty, _ := abi.NewType("bytes32", "", nil)
	addressTy, _ := abi.NewType("address", "", nil)
	uint256Ty, _ := abi.NewType("uint256", "", nil)
	stringTy, _ := abi.NewType("string", "", nil)
	grantArgs = abi.Arguments{
		{Type: bytes32Ty}, // domain
		{Type: bytes32Ty}, // purpose
		{Type: addressTy}, // user
		{Type: addressTy}, // verifyingContract
		{Type: uint256Ty}, // chainId
		{Type: uint256Ty}, // expiry (unix seconds)
		{Type: stringTy},  // token
	}
}

// GrantPlaintext ABI-encodes a token grant to be ECIES-encrypted to the TEE. The
// TEE (parseAndVerifyGrant) accepts the decrypted grant only for this exact
// (user, verifyingContract, chainId, purpose) tuple and only before expiry, so a
// ciphertext copied from another user's public transaction cannot be reused.
func GrantPlaintext(purpose common.Hash, user, verifyingContract common.Address, chainID *big.Int, expiry int64, token string) ([]byte, error) {
	if chainID == nil {
		return nil, fmt.Errorf("chainID is required")
	}
	if token == "" {
		return nil, fmt.Errorf("token is required")
	}
	return grantArgs.Pack(
		[32]byte(GrantDomain),
		[32]byte(purpose),
		user,
		verifyingContract,
		chainID,
		big.NewInt(expiry),
		token,
	)
}
