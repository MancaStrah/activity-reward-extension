// Package types contains types that could be useful to other apps when interacting with this extension.
package types

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// --- ABI argument descriptors for decoding messages from the contract ---
//
// The contract encodes messages with FLAT arguments — abi.encode(val1, val2, val3),
// NOT abi.encode(struct) — so these are abi.Arguments decoded with Unpack(), not
// single tuple arguments for structs.DecodeTo().

// DistanceMessageArgs describes the ABI layout of the DISTANCE message — the
// extension's only instruction, sent by getDistanceProof():
// abi.encode(bytes32 challenge, address caller, address verifyingContract, uint256 chainId, bytes encryptedToken).
var DistanceMessageArgs abi.Arguments

func init() {
	bytes32Ty, _ := abi.NewType("bytes32", "", nil)
	addressTy, _ := abi.NewType("address", "", nil)
	uint256Ty, _ := abi.NewType("uint256", "", nil)
	bytesTy, _ := abi.NewType("bytes", "", nil)

	DistanceMessageArgs = abi.Arguments{
		{Name: "challenge", Type: bytes32Ty},
		{Name: "caller", Type: addressTy},
		{Name: "verifyingContract", Type: addressTy},
		{Name: "chainId", Type: uint256Ty},
		{Name: "encryptedToken", Type: bytesTy},
	}
}

// --- Decoded message structs ---

type DistanceMessage struct {
	Challenge         [32]byte       `json:"challenge"`
	Caller            common.Address `json:"caller"`
	VerifyingContract common.Address `json:"verifyingContract"`
	// The abi tag is required: go-ethereum maps the "chainId" argument to a
	// field named "ChainId", not the idiomatic "ChainID".
	ChainID        *big.Int `json:"chainId" abi:"chainId"`
	EncryptedToken []byte   `json:"encryptedToken"`
}

// --- Strava API types ---

type StravaActivity struct {
	// ID is Strava's permanent identifier for the activity. It is what makes the
	// monthly total idempotent across pages: the listing is paginated and can shift
	// between requests, so without an identity to deduplicate on, one activity
	// returned twice is counted twice.
	ID        int64   `json:"id"`
	Distance  float64 `json:"distance"`
	SportType string  `json:"sport_type"`
	Manual    bool    `json:"manual"`
	// Flagged is set by Strava when an activity is disputed or looks fabricated.
	// Counting it toward a reward would pay out on data Strava itself distrusts.
	Flagged bool `json:"flagged"`
	// StartDate is the activity's start instant in UTC. It is decoded and checked
	// rather than taken on trust, because it is the only thing that lets the enclave
	// confirm the month it is about to attest to. Note the field: `start_date` is
	// absolute, whereas Strava's `start_date_local` is a naive local timestamp that
	// carries no offset and therefore cannot be compared against a UTC boundary.
	StartDate time.Time `json:"start_date"`
}

type StravaAthlete struct {
	ID int64 `json:"id"`
}

// --- Extension response types ---

// DistanceProof is the TEE-signed statement claimReward consumes. Field order and
// types mirror the DistanceProof struct in contracts/InstructionSender.sol.
type DistanceProof struct {
	Timestamp     int64   `json:"timestamp"`
	Challenge     string  `json:"challenge"`
	Caller        string  `json:"caller"`
	TeeID         string  `json:"teeId"`
	Eligible      bool    `json:"eligible"`
	DistanceKm    float64 `json:"distanceKm"`
	DistanceX1000 int64   `json:"distanceX1000"`
	MonthStart    int64   `json:"monthStart"`
	AthleteHash   string  `json:"athleteHash"`
	Signature     string  `json:"signature"`
}

type DistanceResponse struct {
	DistanceProof
	Message string `json:"message"`
}

// State holds the extension's observable state, returned by GET /state.
//
// Counters only, deliberately. State is served publicly and is also carried in the
// signed /info response used for on-chain availability checks, so a per-user value
// here (such as the last athlete's monthly distance) would publish one specific
// person's activity data out of the enclave.
type State struct {
	ProofsSigned         int `json:"proofsSigned"`
	EligibleProofsSigned int `json:"eligibleProofsSigned"`
}

// StateResponse is the envelope returned by GET /state. Its shape is fixed by the
// container contract (docs/extension-contract.md), so only the State type it wraps
// is this extension's to choose.
type StateResponse struct {
	StateVersion common.Hash `json:"stateVersion"`
	State        State       `json:"state"`
}
