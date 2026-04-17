package extension

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"activity-reward-extension/internal/config"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
)

// --- Domain tags ---
//
// These constants are part of the cross-language wire contract and MUST match
// their counterparts byte-for-byte:
//   - grantDomain / purposeDistance : the client that seals the token
//     (tools/cmd/encrypt-token, tools/cmd/run-test).
//   - domainDistanceProof           : DOMAIN_DISTANCE_PROOF in contracts/InstructionSender.sol.
var (
	// grantDomain scopes the encrypted plaintext to "a Strava token grant".
	grantDomain = crypto.Keccak256Hash([]byte("STRAVA_TOKEN_GRANT_V2"))
	// purposeDistance binds a grant to this extension's single operation. The field
	// exists so that a second operation, if one is ever added, cannot consume a grant
	// sealed for this one.
	purposeDistance = crypto.Keccak256Hash([]byte("STRAVA_DISTANCE"))

	// domainDistanceProof scopes the TEE-signed distance-proof payload; it must
	// equal the contract's DOMAIN_DISTANCE_PROOF.
	domainDistanceProof = crypto.Keccak256Hash([]byte("STRAVA_DISTANCE_PROOF_V1"))
)

// grantArgs is the ABI layout of the decrypted token grant:
//
//	abi.encode(bytes32 domain, bytes32 purpose, address user,
//	           address verifyingContract, uint256 chainId, uint256 expiry, string token)
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

// grantContext holds the values a grant must be bound to. Every field is taken
// from the on-chain instruction (or the TEE clock) — never from the ciphertext —
// so the ciphertext is checked against an authenticated context, not itself.
type grantContext struct {
	caller            common.Address
	verifyingContract common.Address
	chainID           *big.Int
	purpose           common.Hash
	now               int64
}

// --- Rejection reasons ---
//
// INVARIANT: every error returned by parseAndVerifyGrant is publicly readable.
// buildResult copies it into ActionResult.Log (see utils.go), and the proxy serves
// that log from an unauthenticated endpoint keyed by the instruction ID, which is
// emitted on chain. So the error text is world-readable for any instruction anyone
// can submit.
//
// The plaintext these errors describe is whatever the TEE machine key decrypted:
// the caller supplies the ciphertext but only the enclave holds the key, and the
// same key seals other message types. Formatting a decrypted value into a returned
// error would therefore make the instruction path a decryption oracle — submit a
// ciphertext, read back a field of its plaintext from the public log.
//
// Hence: each reason below is a fixed string that names the binding that failed and
// carries no value. The values behind the decision go to the local log through
// rejectGrant, which never reaches a caller. Keep it that way — adding a %s of a
// decrypted field to any of these strings reopens the oracle.
var (
	// errGrantEncoding replaces the ABI decoder's own message, which can quote
	// offsets and lengths read verbatim out of the plaintext.
	errGrantEncoding   = errors.New("token grant is not a well-formed grant encoding")
	errGrantDomain     = errors.New("token grant domain does not match this extension's grant domain")
	errGrantPurpose    = errors.New("token grant purpose does not match the requested operation")
	errGrantUser       = errors.New("token grant is not bound to caller")
	errGrantContract   = errors.New("token grant is not bound to this verifying contract")
	errGrantChain      = errors.New("token grant is not bound to this chain")
	errGrantExpiry     = errors.New("token grant has expired or carries an out-of-range expiry")
	errGrantTTL        = errors.New("token grant lifetime exceeds the accepted maximum")
	errGrantEmptyToken = errors.New("token grant contains an empty token")
)

// rejectGrant returns reason — the value-free text a caller may read — after
// recording detail in the enclave's own log, where an operator can see which
// values actually failed the check. detail may quote the decrypted grant fields
// (domain, purpose, user, contract, chain, expiry) because that log stays local;
// it must never quote the token, which is the secret the grant carries.
func rejectGrant(reason error, detail string) error {
	logger.Warnf("token grant rejected: %v (%s)", reason, detail)
	return reason
}

// parseAndVerifyGrant validates an already-decrypted grant plaintext against ctx
// and returns the embedded Strava token. It is the single choke point that makes a
// copied ciphertext useless: the ciphertext only yields a token for the exact
// (user, contract, chain, purpose) it was sealed for, and only before its expiry.
// An attacker who copies a victim's public ciphertext cannot forge one carrying
// their own address/contract/chain because they do not hold the plaintext token
// and only the TEE can decrypt.
//
// Every rejection returns one of the fixed reasons above — which binding failed,
// with no decrypted value attached — because the returned error is published; the
// values are logged locally instead.
func parseAndVerifyGrant(plaintext []byte, ctx grantContext) (string, error) {
	values, err := grantArgs.Unpack(plaintext)
	if err != nil {
		return "", rejectGrant(errGrantEncoding, fmt.Sprintf("decoding grant: %v", err))
	}
	domain := common.Hash(values[0].([32]byte))
	purpose := common.Hash(values[1].([32]byte))
	user := values[2].(common.Address)
	contract := values[3].(common.Address)
	chainID := values[4].(*big.Int)
	expiry := values[5].(*big.Int)
	token := values[6].(string)

	// Each check below returns a fixed reason and logs the values locally — see the
	// INVARIANT above the error definitions: these errors are served from a public,
	// unauthenticated endpoint, so they must not restate decrypted plaintext.
	if domain != grantDomain {
		return "", rejectGrant(errGrantDomain,
			fmt.Sprintf("domain %s, want %s", domain.Hex(), grantDomain.Hex()))
	}
	if purpose != ctx.purpose {
		return "", rejectGrant(errGrantPurpose,
			fmt.Sprintf("purpose %s, want %s", purpose.Hex(), ctx.purpose.Hex()))
	}
	if user != ctx.caller {
		return "", rejectGrant(errGrantUser,
			fmt.Sprintf("grant user %s, caller %s", user.Hex(), ctx.caller.Hex()))
	}
	if contract != ctx.verifyingContract {
		return "", rejectGrant(errGrantContract,
			fmt.Sprintf("grant contract %s, verifying contract %s", contract.Hex(), ctx.verifyingContract.Hex()))
	}
	if ctx.chainID == nil || chainID.Cmp(ctx.chainID) != 0 {
		return "", rejectGrant(errGrantChain,
			fmt.Sprintf("grant chain %s, instruction chain %v", chainID.String(), ctx.chainID))
	}
	if !expiry.IsInt64() || expiry.Int64() <= ctx.now {
		return "", rejectGrant(errGrantExpiry,
			fmt.Sprintf("expiry %s, now %d", expiry.String(), ctx.now))
	}
	// Cap the lifetime rather than trusting the client's choice: the expiry is set
	// client-side, so without a ceiling a caller could mint a decades-long bearer
	// grant that then sits in public calldata forever.
	if maxTTL := int64(config.MaxGrantTTL / time.Second); expiry.Int64()-ctx.now > maxTTL {
		return "", rejectGrant(errGrantTTL,
			fmt.Sprintf("lifetime %ds, maximum %ds", expiry.Int64()-ctx.now, maxTTL))
	}
	if token == "" {
		// The detail carries no value here: the failing value is the token itself,
		// and the token is the one field that never goes to any log.
		return "", rejectGrant(errGrantEmptyToken, "token field is empty")
	}
	return token, nil
}
