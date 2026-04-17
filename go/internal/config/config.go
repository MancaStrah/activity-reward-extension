// Package config contains configuration values and defaults used by the extension.
package config

import (
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const (
	// Version identifies the extension's observable contract: it is hashed into the
	// stateVersion reported by GET /state and /info, and stamped on every
	// ActionResult. Bump it whenever the State shape or the result format changes, so
	// a client can tell which contract a given result was produced under.
	Version = "0.2.0"

	OPTypeStrava = "STRAVA"
	// OPCommandDistance is the extension's only operation: request a signed proof
	// of the caller's monthly distance, which claimReward then consumes.
	OPCommandDistance = "DISTANCE"

	// RewardThresholdKm is the monthly distance a user must cover to be eligible.
	// Must match DISTANCE_THRESHOLD_X1000 in contracts/InstructionSender.sol (km * 1000).
	RewardThresholdKm = 2.0

	// RewardFreshnessSeconds mirrors FRESHNESS_SECONDS in the contract. The usable
	// claim window is this minus the contract's CLOCK_DRIFT_TOLERANCE (60 s), i.e.
	// 300 s.
	RewardFreshnessSeconds = 360

	TimeoutShutdown = 5 * time.Second

	// ActionBudget bounds the total time spent handling one action. tee-node POSTs
	// the action with a 2s client timeout (settings.ProxyTimeout in
	// PostActionToExtension), and abandons the action when that elapses — so work
	// past this point is discarded by the node while the extension keeps burning
	// Strava rate limit and signs a proof nobody will ever fetch. Failing fast and
	// legibly inside the node's budget is strictly better than that.
	ActionBudget = 1800 * time.Millisecond

	// MaxResponseBytes caps any single outbound response body read into enclave
	// memory (Strava and the TEE node's own endpoints).
	MaxResponseBytes = 4 << 20 // 4 MiB

	// MaxRequestBytes caps the inbound POST /action envelope. processorutils.Parse
	// size-checks the inner instruction, but only after the whole outer body has been
	// decoded, and the envelope has unbounded fields of its own.
	MaxRequestBytes = 1 << 20 // 1 MiB

	// MaxGrantTTL caps how far into the future a token grant's expiry may sit. The
	// client chooses the expiry, so without a ceiling here a caller could mint a
	// decades-long bearer grant that lives in public calldata forever.
	MaxGrantTTL = 24 * time.Hour

	// MaxMonthlyKm is a sanity ceiling on the summed Strava distance. Anything above
	// it means the upstream response is wrong, not that somebody ran very far, and
	// an unchecked value would be converted to int64 (implementation-defined for
	// out-of-range floats) and then signed.
	MaxMonthlyKm = 100_000.0

	// StravaPerPage / StravaMaxPages bound the activity listing. Without paging a
	// prolific athlete is silently under-counted at the page boundary.
	//
	// StravaMaxPages is an upper bound, not the operating limit: the number of pages
	// one action can actually afford is decided by ActionBudget, because the pages
	// are fetched one after another (see fetchMonthlyDistance). How many activities
	// that covers depends on the page size Strava chooses to return, which is not
	// something the API promises — asking for StravaPerPage does not mean receiving
	// it. The supported ceiling is therefore "the largest page Strava actually
	// returns × the pages that fit in the budget", and both failure modes report the
	// figures they observed so the real ceiling is visible rather than guessed at.
	StravaPerPage  = 200
	StravaMaxPages = 10

	// StravaQuerySlack widens the `after`/`before` parameters beyond the window the
	// enclave enforces on each activity's own start_date.
	//
	// The query must be a strict SUPERSET of the attested window. Strava documents
	// those parameters only as filtering "activities that have taken place before /
	// after a certain time" — it names no field and no timezone, so whether they
	// compare the absolute start_date or the naive start_date_local is unspecified.
	// Sending the exact window bounds would therefore let the narrower reading drop
	// activities that fall inside the attested month: for an athlete at a negative
	// UTC offset the month's opening hours, and at a positive offset everything in
	// the closing hours before `now`. Those activities would never be returned, so
	// the in-enclave filter would never see them and the signed total would be short
	// by however much the athlete covered at the edges of the month.
	//
	// One day covers every real UTC offset (max ±14 h) with room to spare. Widening
	// costs a slightly longer listing and nothing in correctness, because the
	// enclave filters the window itself and discards whatever falls outside it.
	StravaQuerySlack = 24 * time.Hour

	// StravaPageTimeReserve is the slice of ActionBudget held back for the work that
	// follows the last Strava page: hashing the athlete id, ABI-encoding the payload,
	// the /sign round trip and marshalling the response. The paging loop refuses to
	// start a page that would eat into it, so running out of time surfaces as a clear
	// statement about the activity listing instead of a signing call that dies on an
	// expired context after the distance was already known.
	StravaPageTimeReserve = 300 * time.Millisecond
)

// Defaults.
var (
	ExtensionPort   = 8080
	SignPort        = 9090
	TypesServerPort = 8100
	// TypesServerHost is the interface the public types-server binds to. It
	// defaults to loopback so a bare `go run ./cmd/types-server` is not exposed on
	// every interface; the container overrides it to 0.0.0.0 (its port is then
	// published only to the host loopback by default — see docker-compose.yaml).
	TypesServerHost = "127.0.0.1"
)

// The deployment this enclave was launched for.
//
// A DISTANCE instruction carries `verifyingContract` and `chainId` in its own
// message, and the enclave signs both into the proof — they are the fields that say
// WHERE that proof is valid. Without something to compare them against, the message
// is its own authority for them: the enclave would attest "this proof is good on
// chain X for contract Y" purely because the request said so.
//
// Both names are in the image's tee.launch_policy.allow_env_override list, so in a
// Confidential Space deployment their values are fixed by the attested launch policy
// rather than by whoever sends the instruction.
//
// A nil ChainID or a zero InstructionSender means "not configured", which
// processDistanceProof refuses. That is deliberate: the alternative reading of an
// unset value is "any chain, any contract", which is exactly the check being absent.
var (
	ChainID           *big.Int
	InstructionSender common.Address
)

// Environment variables override defaults.
func init() {
	ep := os.Getenv("EXTENSION_PORT")
	sp := os.Getenv("SIGN_PORT")
	tp := os.Getenv("TYPES_SERVER_PORT")

	if h := os.Getenv("TYPES_SERVER_HOST"); h != "" {
		TypesServerHost = h
	}

	// Parsed strictly, and left unset on anything unparseable rather than falling
	// back to a default: a wrong value here would be compared against the
	// instruction and silently accept the wrong deployment, so "malformed" has to
	// land in the same place as "missing" — refused.
	if v := os.Getenv("CHAIN_ID"); v != "" {
		if n, ok := new(big.Int).SetString(v, 10); ok && n.Sign() > 0 {
			ChainID = n
		}
	}
	if v := os.Getenv("INSTRUCTION_SENDER"); common.IsHexAddress(v) {
		InstructionSender = common.HexToAddress(v)
	}

	if ep != "" {
		if v, err := strconv.Atoi(ep); err == nil {
			ExtensionPort = v
		}
	}
	if sp != "" {
		if v, err := strconv.Atoi(sp); err == nil {
			SignPort = v
		}
	}
	if tp != "" {
		if v, err := strconv.Atoi(tp); err == nil {
			TypesServerPort = v
		}
	}
}
