package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"activity-reward-extension/internal/config"
	"activity-reward-extension/pkg/types"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	teetypes "github.com/flare-foundation/tee-node/pkg/types"
	teeutils "github.com/flare-foundation/tee-node/pkg/utils"
)

// toHash mirrors teeutils.ToHash for clarity: left-pads a string into a 32-byte hash.
func toHash(s string) common.Hash { return teeutils.ToHash(s) }

// buildTestAction constructs a teetypes.Action whose Data.Message is the
// JSON-encoded DataFixed payload. This is what processAction expects to parse.
func buildTestAction(opType, opCommand common.Hash, originalMessage []byte) teetypes.Action {
	// DataFixed is the structure that processorutils.Parse extracts from Data.Message.
	type dataFixed struct {
		InstructionID      common.Hash    `json:"instructionId"`
		TeeID              common.Address `json:"teeId"`
		Timestamp          uint64         `json:"timestamp"`
		RewardEpochID      uint32         `json:"rewardEpochId"`
		OPType             common.Hash    `json:"opType"`
		OPCommand          common.Hash    `json:"opCommand"`
		Cosigners          []string       `json:"cosigners"`
		CosignersThreshold uint64         `json:"cosignersThreshold"`
		OriginalMessage    hexutil.Bytes  `json:"originalMessage"`
	}

	df := dataFixed{
		OPType:          opType,
		OPCommand:       opCommand,
		OriginalMessage: originalMessage,
	}
	msg, _ := json.Marshal(df)

	return teetypes.Action{
		Data: teetypes.ActionData{
			ID:            common.HexToHash("0x1234"),
			SubmissionTag: "submit",
			Message:       msg,
		},
	}
}

// abiEncodeDistanceMessage mirrors the contract's getDistanceProof()
// abi.encode(bytes32 challenge, address caller, address verifyingContract, uint256 chainId, bytes encryptedToken).
func abiEncodeDistanceMessage(challenge [32]byte, caller, verifyingContract common.Address, chainID *big.Int, encryptedToken []byte) []byte {
	encoded, _ := types.DistanceMessageArgs.Pack(challenge, caller, verifyingContract, chainID, encryptedToken)
	return encoded
}

// withDeploymentIdentity configures the chain and contract this enclave is
// "launched for", so processDistanceProof accepts a message naming that pair.
// Every test driving the full path needs it: the identity defaults to unconfigured
// and unconfigured is refused, which is the point of the check.
func withDeploymentIdentity(t *testing.T, contract common.Address, chainID *big.Int) {
	t.Helper()
	origChain, origSender := config.ChainID, config.InstructionSender
	config.ChainID, config.InstructionSender = chainID, contract
	t.Cleanup(func() { config.ChainID, config.InstructionSender = origChain, origSender })
}

// buildGrant packs a token grant plaintext for tests that exercise the decrypt/verify
// path. It uses grantArgs — the layout under test — so it shows only that this package
// is self-consistent; TestGrantWireVector is what ties the layout to the client's, by
// parsing a fixed literal instead of anything packed here.
func buildGrant(t *testing.T, domain, purpose common.Hash, user, contract common.Address, chainID *big.Int, expiry int64, token string) []byte {
	t.Helper()
	packed, err := grantArgs.Pack([32]byte(domain), [32]byte(purpose), user, contract, chainID, big.NewInt(expiry), token)
	if err != nil {
		t.Fatalf("packing grant: %v", err)
	}
	return packed
}

// --- OPType/OPCommand routing and hash debug info ---

func TestProcessAction_UnknownOPType(t *testing.T) {
	e := &Extension{}
	action := buildTestAction(
		toHash("UNKNOWN_TYPE"),
		toHash(config.OPCommandDistance),
		nil,
	)

	status, body := e.processAction(context.Background(), action)

	if status != http.StatusNotImplemented {
		t.Fatalf("expected status %d, got %d", http.StatusNotImplemented, status)
	}

	bodyStr := string(body)
	t.Logf("501 body: %s", bodyStr)

	if !contains(bodyStr, "unsupported op type") {
		t.Error("expected body to contain 'unsupported op type'")
	}

	receivedHash := toHash("UNKNOWN_TYPE").Hex()
	if !contains(bodyStr, receivedHash) {
		t.Errorf("expected body to contain received hash %s", receivedHash)
	}

	expectedHash := toHash(config.OPTypeStrava).Hex()
	if !contains(bodyStr, expectedHash) {
		t.Errorf("expected body to contain expected hash %s", expectedHash)
	}

	if !contains(bodyStr, config.OPTypeStrava) {
		t.Errorf("expected body to contain %q", config.OPTypeStrava)
	}
}

func TestProcessAction_UnknownOPCommand(t *testing.T) {
	e := &Extension{}
	action := buildTestAction(
		toHash(config.OPTypeStrava),
		toHash("UNKNOWN_COMMAND"),
		nil,
	)

	status, body := e.processAction(context.Background(), action)

	if status != http.StatusNotImplemented {
		t.Fatalf("expected status %d, got %d", http.StatusNotImplemented, status)
	}

	bodyStr := string(body)
	t.Logf("501 body: %s", bodyStr)

	if !contains(bodyStr, "unsupported op command") {
		t.Error("expected body to contain 'unsupported op command'")
	}

	receivedHash := toHash("UNKNOWN_COMMAND").Hex()
	if !contains(bodyStr, receivedHash) {
		t.Errorf("expected body to contain received hash %s", receivedHash)
	}

	for _, cmd := range []string{config.OPCommandDistance} {
		cmdHash := toHash(cmd).Hex()
		if !contains(bodyStr, cmdHash) {
			t.Errorf("expected body to contain hash for %s: %s", cmd, cmdHash)
		}
		if !contains(bodyStr, cmd) {
			t.Errorf("expected body to contain command name %q", cmd)
		}
	}
}

// --- Message decoding ---

func TestProcessDistanceProof_MalformedABIMessage(t *testing.T) {
	e := &Extension{}
	action := buildTestAction(
		toHash(config.OPTypeStrava),
		toHash(config.OPCommandDistance),
		[]byte{0x01, 0x02, 0x03}, // not valid ABI encoding
	)

	status, body := e.processAction(context.Background(), action)

	if status != http.StatusOK {
		t.Fatalf("expected status %d (error is in ActionResult, not HTTP), got %d", http.StatusOK, status)
	}

	var result teetypes.ActionResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if result.Status != 0 {
		t.Fatalf("expected ActionResult.Status=0 (error), got %d", result.Status)
	}

	if !contains(result.Log, "decoding distance message") {
		t.Errorf("expected log to mention 'decoding reward message', got %q", result.Log)
	}
	t.Logf("Error log: %s", result.Log)
}

// TestProcessDistanceProof_ValidMessageFailsAtDecrypt proves the ABI layout matches the
// contract encoding: a well-formed message must get PAST decoding and fail at
// the next step (the TEE node decrypt call, which is unavailable in unit tests).
func TestProcessDistanceProof_ValidMessageFailsAtDecrypt(t *testing.T) {
	origPort := config.SignPort
	config.SignPort = 1 // nothing listens here — decrypt must fail fast
	defer func() { config.SignPort = origPort }()

	e := &Extension{}
	var challenge [32]byte
	challenge[0] = 0xab
	caller := common.HexToAddress("0x00000000000000000000000000000000000000A1")
	verifyingContract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	withDeploymentIdentity(t, verifyingContract, big.NewInt(114))

	action := buildTestAction(
		toHash(config.OPTypeStrava),
		toHash(config.OPCommandDistance),
		abiEncodeDistanceMessage(challenge, caller, verifyingContract, big.NewInt(114), []byte("ciphertext")),
	)

	status, body := e.processAction(context.Background(), action)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, status)
	}

	var result teetypes.ActionResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if result.Status != 0 {
		t.Fatalf("expected ActionResult.Status=0, got %d", result.Status)
	}
	if !contains(result.Log, "decrypting token") {
		t.Errorf("expected failure at decrypt step (proves ABI decode succeeded), got %q", result.Log)
	}
}

// --- Sign payload encoding ---

// TestAbiEncodeDistanceProofPayload_RoundTrip checks the 12-field payload layout
// that claimReward()/verifyDistanceProof() re-encode on-chain. The leading fields
// (domain, chainId, verifyingContract, instructionId) bind the signature to this
// operation/chain/contract/instruction; caller, teeId and athleteHash are the
// identities the contract checks.
func TestAbiEncodeDistanceProofPayload_RoundTrip(t *testing.T) {
	instructionID := common.HexToHash("0x00000000000000000000000000000000000000000000000000000000deadbeef")
	chainID := big.NewInt(114)
	verifyingContract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	var challenge [32]byte
	challenge[31] = 0x01
	caller := common.HexToAddress("0x00000000000000000000000000000000000000C3")
	teeID := common.HexToAddress("0x00000000000000000000000000000000000000EE")
	var athleteHash [32]byte
	athleteHash[0] = 0xee

	payload, err := abiEncodeDistanceProofPayload(instructionID, chainID, verifyingContract, 1700000000, challenge, caller, teeID, true, 5100, 1698796800, athleteHash)
	if err != nil {
		t.Fatalf("encoding payload: %v", err)
	}

	// 12 static fields → exactly 12 words.
	if len(payload) != 12*32 {
		t.Fatalf("expected 12*32=%d bytes, got %d", 12*32, len(payload))
	}

	values, err2 := distanceProofPayloadArgsForTest().Unpack(payload)
	if err = err2; err != nil {
		t.Fatalf("unpack failed: %v", err)
	}
	if len(values) != 12 {
		t.Fatalf("expected 12 decoded values, got %d", len(values))
	}

	if got := common.Hash(values[0].([32]byte)); got != domainDistanceProof {
		t.Errorf("domain: expected %s, got %s", domainDistanceProof.Hex(), got.Hex())
	}
	if got := values[1].(*big.Int); got.Cmp(chainID) != 0 {
		t.Errorf("chainId: expected %s, got %s", chainID, got)
	}
	if got := values[2].(common.Address); got != verifyingContract {
		t.Errorf("verifyingContract: expected %s, got %s", verifyingContract.Hex(), got.Hex())
	}
	if got := common.Hash(values[3].([32]byte)); got != instructionID {
		t.Errorf("instructionId: expected %s, got %s", instructionID.Hex(), got.Hex())
	}
	if got := values[4].(*big.Int).Int64(); got != 1700000000 {
		t.Errorf("timestamp: expected 1700000000, got %d", got)
	}
	if got := values[5].([32]byte); got != challenge {
		t.Errorf("challenge mismatch")
	}
	if got := values[6].(common.Address); got != caller {
		t.Errorf("caller: expected %s, got %s", caller.Hex(), got.Hex())
	}
	if got := values[7].(common.Address); got != teeID {
		t.Errorf("teeId: expected %s, got %s", teeID.Hex(), got.Hex())
	}
	if got := values[8].(bool); got != true {
		t.Errorf("eligible: expected true, got %v", got)
	}
	if got := values[9].(*big.Int).Int64(); got != 5100 {
		t.Errorf("distanceX1000: expected 5100, got %d", got)
	}
	if got := values[10].(*big.Int).Int64(); got != 1698796800 {
		t.Errorf("monthStart: expected 1698796800, got %d", got)
	}
	if got := values[11].([32]byte); got != athleteHash {
		t.Errorf("athleteHash mismatch")
	}
}

// TestSignPayloadCrossLanguageVector pins the keccak256 of the distance-proof sign
// payload for a fixed input vector. The Foundry test SignPayloadVector.t.sol
// reconstructs the same payload in Solidity and asserts the same hash, proving the
// Go TEE and the contract agree on the encoding byte-for-byte (they must, or every
// claim would fail on-chain).
func TestSignPayloadCrossLanguageVector(t *testing.T) {
	instructionID := common.HexToHash("0x00000000000000000000000000000000000000000000000000000000deadbeef")
	chainID := big.NewInt(114)
	verifyingContract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	var challenge [32]byte
	challenge[31] = 0x01
	caller := common.HexToAddress("0x00000000000000000000000000000000000000C3")
	teeID := common.HexToAddress("0x00000000000000000000000000000000000000EE")
	var athleteHash [32]byte
	athleteHash[0] = 0xee

	payload, err := abiEncodeDistanceProofPayload(instructionID, chainID, verifyingContract, 1700000000, challenge, caller, teeID, true, 5100, 1698796800, athleteHash)
	if err != nil {
		t.Fatalf("encoding payload: %v", err)
	}
	h := crypto.Keccak256Hash(payload)

	// Must equal the value asserted by SignPayloadVector.t.sol (Solidity side).
	const expected = "0x92d724a4a2dac9e7c86026e881f6363515b6ad83e74d1590e2e732d8bbeeef13"
	if h.Hex() != expected {
		t.Fatalf("distance-proof payload hash drifted: got %s, want %s\n"+
			"If this changed intentionally, update SignPayloadVector.t.sol to match.", h.Hex(), expected)
	}
}

// distanceProofPayloadArgsForTest rebuilds the sign-payload layout independently,
// so the round-trip does not reuse the code under test.
func distanceProofPayloadArgsForTest() abi.Arguments {
	uint256Ty, _ := abi.NewType("uint256", "", nil)
	bytes32Ty, _ := abi.NewType("bytes32", "", nil)
	addressTy, _ := abi.NewType("address", "", nil)
	boolTy, _ := abi.NewType("bool", "", nil)

	return abi.Arguments{
		{Type: bytes32Ty}, // domain
		{Type: uint256Ty}, // chainId
		{Type: addressTy}, // verifyingContract
		{Type: bytes32Ty}, // instructionId
		{Type: uint256Ty}, // timestamp
		{Type: bytes32Ty}, // challenge
		{Type: addressTy}, // caller
		{Type: addressTy}, // teeId
		{Type: boolTy},    // eligible
		{Type: uint256Ty}, // distanceX1000
		{Type: uint256Ty}, // monthStart
		{Type: bytes32Ty}, // athleteHash
	}
}

// --- Athlete hash ---

// TestHashAthleteID_IsPublicPseudonym pins athleteHash to keccak256 of the
// zero-padded Strava id, and is deliberate about what that means: the value is a
// PUBLIC pseudonym, reproducible by anyone who guesses the id. It exists to give the
// contract a stable, collision-free dedup key, not to anonymise the athlete — see
// hashAthleteID's comment for why a keyed MAC was rejected. This test doubles as the
// documentation of that choice: if someone later makes the hash secret-dependent,
// this fails and forces the privacy claim in the docs to be revisited.
func TestHashAthleteID_IsPublicPseudonym(t *testing.T) {
	athleteID := int64(123456789)
	padded := common.LeftPadBytes(big.NewInt(athleteID).Bytes(), 32)

	got, err := hashAthleteID(athleteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got[:]) != string(crypto.Keccak256(padded)) {
		t.Errorf("athleteHash is not keccak256(paddedID):\n got %x", got)
	}

	// Deterministic — the contract's one-reward-per-athlete-per-month guard is a
	// mapping lookup on this value, so the same athlete must always hash the same.
	again, err := hashAthleteID(athleteID)
	if err != nil || again != got {
		t.Errorf("hashAthleteID is not deterministic: %x vs %x (err=%v)", again, got, err)
	}

	// Distinct athletes must not collide, or one payout would lock out the other.
	other, err := hashAthleteID(athleteID + 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if other == got {
		t.Error("distinct athlete ids collided")
	}
}

// TestHashAthleteID_RejectsNonPositiveID guards the identity-collapse bug: a missing
// Strava "id" field decodes to 0, and big.Int.Bytes() drops the sign, so 0 and any
// negative id would otherwise map several distinct users onto one shared athleteHash
// — and the first payout would lock out everyone else sharing it.
func TestHashAthleteID_RejectsNonPositiveID(t *testing.T) {
	for _, id := range []int64{0, -1, -123456789} {
		if _, err := hashAthleteID(id); err == nil {
			t.Errorf("expected an error for non-positive athlete id %d", id)
		}
	}
}

// --- Token grant binding (grant must match caller / operation / chain / contract) ---

// TestParseAndVerifyGrant covers both halves of the function's contract: which
// grants it accepts, and what a rejection is allowed to say. Rejection errors are
// copied into ActionResult.Log, which is served from an unauthenticated endpoint
// keyed by the on-chain instruction ID, so they must not restate any part of the
// decrypted plaintext. Each case therefore builds its mismatching field out of a
// distinctive sentinel and asserts the sentinel appears nowhere in the returned
// error — while also asserting the error still names the binding that failed, so
// "one opaque error for everything" would not satisfy this test.
func TestParseAndVerifyGrant(t *testing.T) {
	caller := common.HexToAddress("0x00000000000000000000000000000000000000A1")
	contract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	chainID := big.NewInt(114)
	const now = int64(1_700_000_000)
	future := now + 900 // inside config.MaxGrantTTL
	// The token is the secret the grant carries; it must never appear in an error,
	// whichever check fails, so every grant below embeds this sentinel.
	const tokenStr = "sentinel-strava-token-4f81c3"

	ctx := grantContext{caller: caller, verifyingContract: contract, chainID: chainID, purpose: purposeDistance, now: now}

	valid := buildGrant(t, grantDomain, purposeDistance, caller, contract, chainID, future, tokenStr)
	got, err := parseAndVerifyGrant(valid, ctx)
	if err != nil {
		t.Fatalf("valid grant rejected: %v", err)
	}
	if got != tokenStr {
		t.Errorf("token: got %q, want %q", got, tokenStr)
	}

	// Sentinel values, each recognisable in the plaintext it is packed into. Address
	// and hash sentinels are compared case-insensitively because common.Address.Hex
	// emits an EIP-55 checksummed (mixed-case) string.
	var (
		otherDomain   = common.HexToHash("0xdeadbeef")
		otherPurpose  = common.HexToHash("0xfacade00")
		other         = common.HexToAddress("0xcafebabe00000000000000000000000000000000")
		otherContract = common.HexToAddress("0xd00dfeed00000000000000000000000000000000")
		otherChain    = big.NewInt(987654321)
		pastExpiry    = int64(1234567890) // well before now
		maxTTL        = int64(config.MaxGrantTTL / time.Second)
		longExpiry    = now + maxTTL + 424242
	)

	// A grant whose trailing string offset points far outside the payload. The ABI
	// decoder's own error quotes that offset, and the offset is attacker-supplied
	// plaintext, so the decode failure has to be reported as a fixed reason too.
	badOffset, ok := new(big.Int).SetString("18446744073709551616424242", 10)
	if !ok {
		t.Fatal("constructing the bad-offset sentinel")
	}
	malformed := append([]byte(nil), valid...)
	copy(malformed[6*32:7*32], common.LeftPadBytes(badOffset.Bytes(), 32))

	cases := []struct {
		name string
		// grant is the plaintext handed to parseAndVerifyGrant.
		grant []byte
		// want is the reason the rejection must carry, so each binding stays
		// distinguishable to an operator reading the public log.
		want error
		// wantMention keeps the message itself informative: a word that identifies
		// the failed binding independently of the sentinel error identity.
		wantMention string
		// sentinels are the plaintext-derived strings that must not leak into the
		// returned error.
		sentinels []string
	}{
		{
			name:        "malformed encoding",
			grant:       malformed,
			want:        errGrantEncoding,
			wantMention: "encoding",
			sentinels: []string{
				badOffset.String(),
				new(big.Int).Add(badOffset, big.NewInt(32)).String(),
			},
		},
		{
			name:        "wrong domain",
			grant:       buildGrant(t, otherDomain, purposeDistance, caller, contract, chainID, future, tokenStr),
			want:        errGrantDomain,
			wantMention: "domain",
			sentinels:   []string{otherDomain.Hex(), "deadbeef"},
		},
		// A grant sealed for some other (e.g. future) operation must not be accepted
		// here, which is the whole point of keeping the purpose field.
		{
			name:        "wrong purpose",
			grant:       buildGrant(t, grantDomain, otherPurpose, caller, contract, chainID, future, tokenStr),
			want:        errGrantPurpose,
			wantMention: "purpose",
			sentinels:   []string{otherPurpose.Hex(), "facade00"},
		},
		{
			name:        "wrong user",
			grant:       buildGrant(t, grantDomain, purposeDistance, other, contract, chainID, future, tokenStr),
			want:        errGrantUser,
			wantMention: "caller",
			sentinels:   []string{other.Hex(), "cafebabe"},
		},
		{
			name:        "wrong contract",
			grant:       buildGrant(t, grantDomain, purposeDistance, caller, otherContract, chainID, future, tokenStr),
			want:        errGrantContract,
			wantMention: "contract",
			sentinels:   []string{otherContract.Hex(), "d00dfeed"},
		},
		{
			name:        "wrong chain",
			grant:       buildGrant(t, grantDomain, purposeDistance, caller, contract, otherChain, future, tokenStr),
			want:        errGrantChain,
			wantMention: "chain",
			sentinels:   []string{otherChain.String()},
		},
		{
			name:        "expired",
			grant:       buildGrant(t, grantDomain, purposeDistance, caller, contract, chainID, pastExpiry, tokenStr),
			want:        errGrantExpiry,
			wantMention: "expired",
			sentinels:   []string{strconv.FormatInt(pastExpiry, 10)},
		},
		// The client picks the expiry, so an unbounded lifetime would be a
		// decades-long bearer grant sitting in public calldata.
		{
			name:        "lifetime beyond the cap",
			grant:       buildGrant(t, grantDomain, purposeDistance, caller, contract, chainID, longExpiry, tokenStr),
			want:        errGrantTTL,
			wantMention: "lifetime",
			sentinels: []string{
				strconv.FormatInt(longExpiry, 10),
				strconv.FormatInt(longExpiry-now, 10), // the requested lifetime
			},
		},
		{
			name:        "empty token",
			grant:       buildGrant(t, grantDomain, purposeDistance, caller, contract, chainID, future, ""),
			want:        errGrantEmptyToken,
			wantMention: "empty",
		},
	}

	// Message → case name, to prove a single catch-all reason would not pass: the
	// subtests below run sequentially, so writing this map from inside them is safe.
	seen := make(map[string]string, len(cases))

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseAndVerifyGrant(c.grant, ctx)
			if err == nil {
				t.Fatal("expected rejection, got nil")
			}
			if !errors.Is(err, c.want) {
				t.Errorf("reason: got %v, want %v", err, c.want)
			}
			msg := err.Error()
			lower := strings.ToLower(msg)
			if !contains(lower, c.wantMention) {
				t.Errorf("error %q does not identify the failed binding (expected to mention %q)", msg, c.wantMention)
			}
			// The token is never a legitimate part of any rejection.
			for _, s := range append([]string{tokenStr}, c.sentinels...) {
				if contains(lower, strings.ToLower(s)) {
					t.Errorf("error %q leaks the plaintext value %q", msg, s)
				}
			}
			if prev, dup := seen[msg]; dup {
				t.Errorf("%q is also returned for %s; rejections must stay distinguishable", msg, prev)
			}
			seen[msg] = c.name
		})
	}
}

// TestProcessDistanceProof_GrantMismatch is the negative test for the ciphertext-reuse
// attack: it drives the full processDistanceProof path with a mocked TEE /decrypt that
// returns a grant sealed for the VICTIM, while the instruction caller is the
// ATTACKER — exactly what happens when Eve replays Alice's public ciphertext. It
// must be rejected at grant verification, before any Strava call or signature.
func TestProcessDistanceProof_GrantMismatch(t *testing.T) {
	victim := common.HexToAddress("0xbeefbeef00000000000000000000000000000000")
	contract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	chainID := big.NewInt(114)
	const victimToken = "victim-strava-token"
	grant := buildGrant(t, grantDomain, purposeDistance, victim, contract, chainID, time.Now().Add(10*time.Minute).Unix(), victimToken)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DecryptResponse{DecryptedMessage: grant})
	}))
	defer srv.Close()

	origPort := config.SignPort
	config.SignPort = portFromURL(t, srv.URL)
	defer func() { config.SignPort = origPort }()

	e := &Extension{}
	var challenge [32]byte
	attacker := common.HexToAddress("0x00000000000000000000000000000000000000A1") // != victim
	withDeploymentIdentity(t, contract, chainID)
	action := buildTestAction(
		toHash(config.OPTypeStrava),
		toHash(config.OPCommandDistance),
		abiEncodeDistanceMessage(challenge, attacker, contract, chainID, []byte("stolen-ciphertext")),
	)

	status, body := e.processAction(context.Background(), action)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, status)
	}

	var result teetypes.ActionResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if result.Status != 0 {
		t.Fatalf("expected ActionResult.Status=0 (grant rejected), got %d", result.Status)
	}
	if !contains(result.Log, "grant verification") || !contains(result.Log, "not bound to caller") {
		t.Errorf("expected grant/caller-binding rejection, got %q", result.Log)
	}
	// ActionResult.Log is fetched from an unauthenticated endpoint, so this is the
	// surface the rejection actually reaches: it may say which binding failed, but
	// nothing the machine key decrypted — here the victim's address and token.
	for _, sentinel := range []string{victimToken, victim.Hex(), "beefbeef"} {
		if contains(strings.ToLower(result.Log), strings.ToLower(sentinel)) {
			t.Errorf("public log %q leaks the decrypted value %q", result.Log, sentinel)
		}
	}
}

// portFromURL extracts the numeric TCP port from an httptest server URL.
func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing test server url %q: %v", raw, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parsing port from %q: %v", raw, err)
	}
	return p
}

// --- Pagination completeness ---

// TestFetchMonthlyDistance_CountsEveryPageWhenPagesComeBackShort is the guard on
// the one thing the signed distance has to be: complete.
//
// Strava documents per_page as "Number of items per page. Defaults to 30" — no
// maximum, and no promise that asking for config.StravaPerPage returns that many
// while more activities remain. A server that caps the page size therefore answers
// every page short. A loop that treated a short page as the last page would stop on
// page 1 and sign a fraction of the month, so the pages below are all far shorter
// than the requested size and the total must still include every one of them.
func TestFetchMonthlyDistance_CountsEveryPageWhenPagesComeBackShort(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	inWindow := monthStart.AddDate(0, 0, 3)
	pages := [][]types.StravaActivity{
		{
			{ID: 1, Distance: 1000, SportType: "Run", StartDate: inWindow},
			{ID: 2, Distance: 500, SportType: "Ride", StartDate: inWindow},
		},
		{{ID: 3, Distance: 2000, SportType: "TrailRun", StartDate: inWindow}},
		{
			{ID: 4, Distance: 1500, SportType: "VirtualRide", StartDate: inWindow},
			{ID: 5, Distance: 1000, SportType: "EBikeRide", StartDate: inWindow},
		},
	}

	stub := &stubUpstreams{activityPages: pages}
	orig := httpClient
	httpClient = &http.Client{Transport: stub}
	defer func() { httpClient = orig }()

	got, err := fetchMonthlyDistance(context.Background(), "token", monthStart, monthStart.AddDate(0, 0, 15))
	if err != nil {
		t.Fatalf("fetchMonthlyDistance: %v", err)
	}

	const want = 6.0 // (1000+500+2000+1500+1000) metres
	if got != want {
		t.Errorf("total distance: got %v km, want %v km — a page was dropped", got, want)
	}
	// Three pages of data plus the empty page that ends the listing.
	if stub.activityRequests != len(pages)+1 {
		t.Errorf("activity requests: got %d, want %d (each page once, then the empty page)",
			stub.activityRequests, len(pages)+1)
	}
}

// TestFetchMonthlyDistance_RefusesWhenPageBudgetRunsOut pins the fail-closed end of
// the same rule: if the page budget is exhausted while Strava is still returning
// activities, the month is not fully counted and no total may be signed. Under-counting
// silently would deny a reward the athlete earned, and the proof would state a distance
// that is simply wrong.
func TestFetchMonthlyDistance_RefusesWhenPageBudgetRunsOut(t *testing.T) {
	// Every page in the budget comes back non-empty, so the end is never reached.
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	pages := make([][]types.StravaActivity, config.StravaMaxPages)
	for i := range pages {
		pages[i] = []types.StravaActivity{{
			ID: int64(i + 1), Distance: 1000, SportType: "Run",
			StartDate: monthStart.AddDate(0, 0, 3),
		}}
	}

	stub := &stubUpstreams{activityPages: pages}
	orig := httpClient
	httpClient = &http.Client{Transport: stub}
	defer func() { httpClient = orig }()

	_, err := fetchMonthlyDistance(context.Background(), "token", monthStart, monthStart.AddDate(0, 0, 15))
	if err == nil {
		t.Fatal("expected a refusal when the page budget runs out, got a signed-able total")
	}
	if !errors.Is(err, errDistancePageBudget) {
		t.Errorf("reason: got %v, want %v", err, errDistancePageBudget)
	}
	assertNoActivityDataLeak(t, err)
	if stub.activityRequests != config.StravaMaxPages {
		t.Errorf("activity requests: got %d, want %d (the whole budget, then stop)",
			stub.activityRequests, config.StravaMaxPages)
	}
}

// TestFetchMonthlyDistance_EnforcesTheWindowItself is the guard on the other half of
// what the signature claims: not just "this many metres" but "in THIS month".
//
// The proof carries a UTC monthStart that claimReward checks against its own calendar,
// but the `after`/`before` query parameters cannot be relied on to produce that window:
// Strava documents them only as filtering "activities that have taken place before /
// after a certain time", naming no field and no timezone, so whether they compare the
// absolute start_date or the naive start_date_local is unspecified. The listing below
// therefore returns activities the query should have excluded — one from the previous
// month, one dated in the future — and the total must contain neither.
func TestFetchMonthlyDistance_EnforcesTheWindowItself(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	now := monthStart.AddDate(0, 0, 15)

	stub := &stubUpstreams{activityPages: [][]types.StravaActivity{{
		// Late on 31 July: inside the window only if the boundary is read in some
		// local timezone rather than UTC. It must not be counted.
		{ID: 1, Distance: 9000, SportType: "Run", StartDate: monthStart.Add(-90 * time.Minute)},
		// Squarely inside the attested month.
		{ID: 2, Distance: 3000, SportType: "Run", StartDate: monthStart.AddDate(0, 0, 2)},
		// Exactly on the boundary: monthStart is inclusive.
		{ID: 3, Distance: 1000, SportType: "Ride", StartDate: monthStart},
		// Future-dated. Strava will store these, and `now` is exclusive.
		{ID: 4, Distance: 7000, SportType: "Run", StartDate: now.AddDate(0, 0, 1)},
		// Exactly at `now`: excluded, so the window is half-open on both proofs.
		{ID: 5, Distance: 5000, SportType: "Run", StartDate: now},
	}}}

	orig := httpClient
	httpClient = &http.Client{Transport: stub}
	defer func() { httpClient = orig }()

	got, err := fetchMonthlyDistance(context.Background(), "token", monthStart, now)
	if err != nil {
		t.Fatalf("fetchMonthlyDistance: %v", err)
	}
	const want = 4.0 // ids 2 and 3 only
	if got != want {
		t.Errorf("total distance: got %v km, want %v km — an out-of-window activity was counted", got, want)
	}
}

// TestFetchMonthlyDistance_AsksForMoreThanTheAttestedWindow pins the DIRECTION of the
// query bounds, which is the half of the window guarantee the in-enclave filter cannot
// provide on its own.
//
// Enforcing [monthStart, now) locally stops an out-of-window activity from being
// counted. It does nothing about the opposite error: an activity inside the window that
// the query never returned. Since Strava names neither the field nor the timezone its
// `after`/`before` compare, the only way to be sure the listing contains everything the
// enclave means to judge is to ask for strictly more than that and throw away the
// surplus. So the query must reach past both boundaries by more than any real timezone
// could shift a timestamp.
func TestFetchMonthlyDistance_AsksForMoreThanTheAttestedWindow(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	now := monthStart.AddDate(0, 0, 15)

	stub := &stubUpstreams{activityPages: [][]types.StravaActivity{{
		{ID: 1, Distance: 3000, SportType: "Run", StartDate: monthStart.AddDate(0, 0, 2)},
	}}}

	orig := httpClient
	httpClient = &http.Client{Transport: stub}
	defer func() { httpClient = orig }()

	if _, err := fetchMonthlyDistance(context.Background(), "token", monthStart, now); err != nil {
		t.Fatalf("fetchMonthlyDistance: %v", err)
	}

	// The largest real UTC offset is +14:00, so a query reaching at least that far
	// past each boundary cannot exclude anything the enclave would have counted,
	// whichever field the server compares.
	const maxRealOffsetSeconds = int64(14 * 60 * 60)
	if slack := monthStart.Unix() - stub.lastAfter; slack < maxRealOffsetSeconds {
		t.Errorf("query `after` sits %ds before monthStart, want at least %ds — "+
			"a narrower query can drop the month's opening hours for an athlete behind UTC",
			slack, maxRealOffsetSeconds)
	}
	if slack := stub.lastBefore - now.Unix(); slack < maxRealOffsetSeconds {
		t.Errorf("query `before` sits %ds after now, want at least %ds — "+
			"a narrower query can drop the closing hours for an athlete ahead of UTC",
			slack, maxRealOffsetSeconds)
	}
}

// TestFetchMonthlyDistance_CountsEdgeActivityWhenTheQueryFiltersOnLocalTime is the
// behavioural half of the test above: it puts a qualifying activity at each edge of the
// attested month and has the stub filter the listing the way a server comparing the
// naive local timestamp would. The activity must still be counted, which it can only be
// if the query asked wide enough to return it in the first place.
//
// Both cases fail with the query sent at the exact window bounds: the athlete is denied
// a reward they earned, and the proof attests a distance short by the missing activity.
func TestFetchMonthlyDistance_CountsEdgeActivityWhenTheQueryFiltersOnLocalTime(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	now := monthStart.AddDate(0, 0, 15)

	cases := []struct {
		name   string
		offset time.Duration
		at     time.Time
	}{
		{
			// Kiritimati is UTC+14; +13 is a common summer offset. The local
			// timestamp of a recent activity reads as later than `now`.
			name:   "athlete ahead of UTC, activity an hour ago",
			offset: 13 * time.Hour,
			at:     now.Add(-1 * time.Hour),
		},
		{
			// Anywhere in the Americas: the local timestamp of an activity in the
			// month's first hour reads as belonging to the previous month.
			name:   "athlete behind UTC, activity in the month's first half hour",
			offset: -11 * time.Hour,
			at:     monthStart.Add(30 * time.Minute),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubUpstreams{
				localOffset: c.offset,
				activityPages: [][]types.StravaActivity{{
					{ID: 1, Distance: 5000, SportType: "Run", StartDate: c.at},
				}},
			}

			orig := httpClient
			httpClient = &http.Client{Transport: stub}
			defer func() { httpClient = orig }()

			got, err := fetchMonthlyDistance(context.Background(), "token", monthStart, now)
			if err != nil {
				t.Fatalf("fetchMonthlyDistance: %v", err)
			}
			if stub.queryDropped != 0 {
				t.Errorf("the query dropped %d in-window activities before the enclave could see them; "+
					"the listing has to be a superset of the attested window", stub.queryDropped)
			}
			const want = 5.0
			if got != want {
				t.Errorf("total distance: got %v km, want %v km — an activity at the edge of the month was lost", got, want)
			}
		})
	}
}

// TestFetchMonthlyDistance_RefusesWhenTheActionBudgetRunsOut covers the other bound on
// the paging loop. The page cap is not the operating limit: the pages are fetched one
// after another inside config.ActionBudget, so on a listing that keeps coming back full
// it is the clock that runs out first.
//
// Without the check this surfaces much later and much less usefully — the context
// expires on the /sign call, after the distance was already computed, and the operator
// gets a deadline error that says nothing about the listing being too long to read.
func TestFetchMonthlyDistance_RefusesWhenTheActionBudgetRunsOut(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)

	// Every page in the budget is non-empty, so only the clock can stop the loop.
	pages := make([][]types.StravaActivity, config.StravaMaxPages)
	for i := range pages {
		pages[i] = []types.StravaActivity{{
			ID: int64(i + 1), Distance: 1000, SportType: "Run",
			StartDate: monthStart.AddDate(0, 0, 3),
		}}
	}

	stub := &stubUpstreams{activityPages: pages, pageDelay: 60 * time.Millisecond}
	orig := httpClient
	httpClient = &http.Client{Transport: stub}
	defer func() { httpClient = orig }()

	// Room for a few pages, not for ten. The guard reserves
	// config.StravaPageTimeReserve for the signing work that has to follow.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := fetchMonthlyDistance(ctx, "token", monthStart, monthStart.AddDate(0, 0, 15))
	if err == nil {
		t.Fatal("expected a refusal when the action budget runs out, got a signed-able total")
	}
	if !errors.Is(err, errDistanceTimeBudget) {
		t.Errorf("reason: got %v, want %v", err, errDistanceTimeBudget)
	}
	assertNoActivityDataLeak(t, err)
	if stub.activityRequests >= config.StravaMaxPages {
		t.Errorf("asked for %d pages; the budget guard should have stopped the loop before the page cap",
			stub.activityRequests)
	}
	// The guard must stop BEFORE a request that cannot finish, so no page request is
	// ever the thing that trips the deadline.
	if ctxErr := ctx.Err(); ctxErr != nil {
		t.Errorf("context expired during the fetch (%v); the guard should have refused the next page while there was still time", ctxErr)
	}
}

// TestFetchMonthlyDistance_CountsARepeatedActivityOnce covers the consequence of a
// paginated listing that is not a snapshot: an upload or an edit between two page
// requests reorders it, and the same activity comes back on both pages. Counting it
// twice would attest a distance the athlete did not cover.
func TestFetchMonthlyDistance_CountsARepeatedActivityOnce(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	inWindow := monthStart.AddDate(0, 0, 4)
	shared := types.StravaActivity{ID: 77, Distance: 5000, SportType: "Run", StartDate: inWindow}

	stub := &stubUpstreams{activityPages: [][]types.StravaActivity{
		{shared, {ID: 78, Distance: 1000, SportType: "Ride", StartDate: inWindow}},
		{shared}, // the boundary shifted; id 77 is served again
	}}

	orig := httpClient
	httpClient = &http.Client{Transport: stub}
	defer func() { httpClient = orig }()

	got, err := fetchMonthlyDistance(context.Background(), "token", monthStart, monthStart.AddDate(0, 0, 15))
	if err != nil {
		t.Fatalf("fetchMonthlyDistance: %v", err)
	}
	const want = 6.0 // 5000 + 1000, not 5000 + 1000 + 5000
	if got != want {
		t.Errorf("total distance: got %v km, want %v km — a duplicate was counted twice", got, want)
	}
}

// TestFetchMonthlyDistance_RefusesUnusableActivityMetadata pins the fail-closed
// direction for a qualifying activity the enclave cannot place or identify. Skipping
// either silently would under-count, and the alternative — signing anyway — would
// attest a month that was never confirmed.
func TestFetchMonthlyDistance_RefusesUnusableActivityMetadata(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	inWindow := monthStart.AddDate(0, 0, 4)

	cases := []struct {
		name     string
		activity types.StravaActivity
		want     error
	}{
		{
			name:     "no id",
			activity: types.StravaActivity{Distance: 3000, SportType: "Run", StartDate: inWindow},
			want:     errDistanceActivityID,
		},
		{
			name:     "no start_date",
			activity: types.StravaActivity{ID: 9, Distance: 3000, SportType: "Run"},
			want:     errDistanceActivityDate,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubUpstreams{activityPages: [][]types.StravaActivity{{c.activity}}}
			orig := httpClient
			httpClient = &http.Client{Transport: stub}
			defer func() { httpClient = orig }()

			_, err := fetchMonthlyDistance(context.Background(), "token", monthStart, monthStart.AddDate(0, 0, 15))
			if err == nil {
				t.Fatal("expected a refusal, got a signed-able total")
			}
			if !errors.Is(err, c.want) {
				t.Errorf("reason: got %v, want %v", err, c.want)
			}
			assertNoActivityDataLeak(t, err)
		})
	}
}

// assertNoActivityDataLeak holds the invariant above the distance rejections: the
// returned error is published on the proxy's unauthenticated result endpoint, so it
// must not restate the athlete's data. Every private value these paths could carry —
// an activity id, a count of activities, a distance — is a number, so "carries no
// digit" is a property a leak cannot satisfy.
func assertNoActivityDataLeak(t *testing.T, err error) {
	t.Helper()
	if msg := err.Error(); strings.ContainsAny(msg, "0123456789") {
		t.Errorf("rejection %q carries a number; activity ids and counts belong in the local log, not in the published error", msg)
	}
}

// TestFetchMonthlyDistance_IgnoresMetadataOnNonQualifyingActivities keeps the two
// refusals above from becoming a denial-of-service on the whole proof: an activity
// that was never going to be counted — a Walk, a manual entry, a flagged one — is
// discarded before its id or start_date is inspected. Otherwise one unrelated record
// in the athlete's month would block every claim they make.
func TestFetchMonthlyDistance_IgnoresMetadataOnNonQualifyingActivities(t *testing.T) {
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	inWindow := monthStart.AddDate(0, 0, 4)

	stub := &stubUpstreams{activityPages: [][]types.StravaActivity{{
		{Distance: 9000, SportType: "Walk"},                            // wrong sport, no id/date
		{Distance: 9000, SportType: "Run", Manual: true},               // self-reported
		{Distance: 9000, SportType: "Run", Flagged: true},              // Strava distrusts it
		{ID: 5, Distance: 2000, SportType: "Run", StartDate: inWindow}, // the only one that counts
	}}}

	orig := httpClient
	httpClient = &http.Client{Transport: stub}
	defer func() { httpClient = orig }()

	got, err := fetchMonthlyDistance(context.Background(), "token", monthStart, monthStart.AddDate(0, 0, 15))
	if err != nil {
		t.Fatalf("a non-qualifying activity must not fail the proof: %v", err)
	}
	const want = 2.0
	if got != want {
		t.Errorf("total distance: got %v km, want %v km", got, want)
	}
}

// --- Pagination completeness end ---

// --- Eligibility boundary ---

// stubUpstreams answers every outbound request processDistanceProof makes — the
// TEE node's /decrypt and /sign, and the two Strava endpoints — from canned data.
// It is installed as the shared httpClient's transport rather than as a listening
// server, so the test needs neither a network nor a Strava account, and any URL
// the code did not expect fails the request instead of leaving the machine.
// failingTransport fails the test if the enclave makes any outbound request. Used
// where the point is that a refusal happened BEFORE the network was touched.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.t.Errorf("refused instruction still reached the network: %s", req.URL)
	return nil, fmt.Errorf("must not be called")
}

type stubUpstreams struct {
	// grant is the plaintext /decrypt hands back: an already-packed token grant.
	grant []byte
	// activityMeters is the single qualifying activity Strava reports, in metres —
	// the input to the km total whose rounding the eligibility rule turns on.
	activityMeters float64
	// activityPages, when set, is served page by page and takes precedence over
	// activityMeters. Index 0 answers page=1. Requests past the end get an empty
	// page, which is how a real listing signals that the window is exhausted.
	activityPages [][]types.StravaActivity
	// activityRequests counts the /athlete/activities calls, so a test can assert
	// how many pages the loop actually asked for.
	activityRequests int
	// localOffset, when non-zero, makes the stub filter the listing the way Strava
	// would if its `after`/`before` parameters compared the naive start_date_local
	// rather than the absolute start_date: the activity's instant is shifted by the
	// athlete's UTC offset and the shifted value is compared against the bounds. The
	// API's wording permits that reading, so the extension has to survive it.
	localOffset time.Duration
	// queryDropped counts the activities the simulated query filter removed. An
	// in-window activity landing here is the defect: the enclave never sees it, so
	// its own window check cannot put it back.
	queryDropped int
	// lastAfter / lastBefore record the window the code actually asked for, so a test
	// can assert the query is wider than the window the enclave enforces.
	lastAfter  int64
	lastBefore int64
	// pageDelay makes every page request take this long, so a test can exercise the
	// action-budget guard without a network.
	pageDelay time.Duration
}

// activitiesPage answers one /athlete/activities request, honouring the `page`
// query parameter the way a paginated listing does.
func (s *stubUpstreams) activitiesPage(req *http.Request) []types.StravaActivity {
	s.activityRequests++

	if s.pageDelay > 0 {
		time.Sleep(s.pageDelay)
	}

	q := req.URL.Query()
	s.lastAfter, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	s.lastBefore, _ = strconv.ParseInt(q.Get("before"), 10, 64)

	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}

	if s.activityPages != nil {
		if page > len(s.activityPages) {
			return []types.StravaActivity{}
		}
		return s.filterAsQueryWould(s.activityPages[page-1])
	}

	// Default: one qualifying activity on page 1, nothing after it. Returning the
	// same page for every request would be a stub that never terminates, and
	// fetchMonthlyDistance is right to keep asking until a page comes back empty.
	if page > 1 {
		return []types.StravaActivity{}
	}
	// A real id and a start_date inside the attested window: fetchMonthlyDistance
	// enforces the month itself rather than trusting the query, so an activity
	// missing either is refused.
	return s.filterAsQueryWould([]types.StravaActivity{{
		ID:        1,
		Distance:  s.activityMeters,
		SportType: "Run",
		StartDate: midMonth(),
	}})
}

// filterAsQueryWould applies the server-side `after`/`before` filter under the reading
// where the bounds are compared against the naive local timestamp. With localOffset
// unset it is the identity, so tests that do not care are unaffected.
func (s *stubUpstreams) filterAsQueryWould(in []types.StravaActivity) []types.StravaActivity {
	if s.localOffset == 0 {
		return in
	}
	out := make([]types.StravaActivity, 0, len(in))
	for _, a := range in {
		asLocal := a.StartDate.Add(s.localOffset).Unix()
		if asLocal < s.lastAfter || asLocal > s.lastBefore {
			s.queryDropped++
			continue
		}
		out = append(out, a)
	}
	return out
}

// midMonth returns an instant guaranteed to fall inside the current attested window
// — at or after the 1st at 00:00 UTC and before now — whenever the test runs. Using
// "now minus a fixed offset" would fall outside the window in the first minutes of a
// month; the midpoint cannot.
func midMonth() time.Time {
	now := time.Now().UTC()
	start := monthStartOf(now)
	return start.Add(now.Sub(start) / 2)
}

func (s *stubUpstreams) RoundTrip(req *http.Request) (*http.Response, error) {
	var (
		body []byte
		err  error
	)
	switch req.URL.Path {
	case "/decrypt":
		body, err = json.Marshal(DecryptResponse{DecryptedMessage: s.grant})
	case "/sign":
		body, err = json.Marshal(SignResponse{Signature: bytes.Repeat([]byte{0x7f}, 65)})
	case "/api/v3/athlete":
		body, err = json.Marshal(types.StravaAthlete{ID: 424242})
	case "/api/v3/athlete/activities":
		body, err = json.Marshal(s.activitiesPage(req))
	default:
		return nil, fmt.Errorf("unexpected request to %s", req.URL)
	}
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// runDistanceProof drives one complete DISTANCE action against stubbed upstreams
// and returns the decoded response — the same JSON the caller and the contract
// tooling read.
func runDistanceProof(t *testing.T, activityMeters float64) types.DistanceResponse {
	t.Helper()

	caller := common.HexToAddress("0x00000000000000000000000000000000000000A1")
	contract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	chainID := big.NewInt(114)
	grant := buildGrant(t, grantDomain, purposeDistance, caller, contract, chainID,
		time.Now().Add(10*time.Minute).Unix(), "stub-strava-token")

	orig := httpClient
	httpClient = &http.Client{Transport: &stubUpstreams{grant: grant, activityMeters: activityMeters}}
	t.Cleanup(func() { httpClient = orig })

	withDeploymentIdentity(t, contract, chainID)

	var challenge [32]byte
	challenge[0] = 0x5a
	action := buildTestAction(
		toHash(config.OPTypeStrava),
		toHash(config.OPCommandDistance),
		abiEncodeDistanceMessage(challenge, caller, contract, chainID, []byte("ciphertext")),
	)

	e := &Extension{}
	status, body := e.processAction(context.Background(), action)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, body)
	}

	var result teetypes.ActionResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal action result: %v", err)
	}
	if result.Status != 1 {
		t.Fatalf("expected a signed proof (status 1), got %d: %s", result.Status, result.Log)
	}

	var resp types.DistanceResponse
	if err := json.Unmarshal(result.Data, &resp); err != nil {
		t.Fatalf("failed to unmarshal distance response: %v", err)
	}
	return resp
}

// TestProcessDistanceProof_EligibilityFollowsTheRoundedInteger pins WHICH value
// decides eligibility: distanceX1000 — the integer the signed payload carries and
// the contract compares against DISTANCE_THRESHOLD_X1000 — and not the float it
// was rounded from.
//
// The two rules disagree in the sliver just under the bar. A month totalling
// 1.9995 km is below config.RewardThresholdKm as a float, yet rounds to
// distanceX1000 = 2000, which is exactly the threshold: comparing the float would
// sign eligible=false next to a distance the contract reads as eligible, so
// verifyDistanceProofFor would answer "eligible" for a proof whose own flag says
// otherwise and claimReward would refuse it. That interval is therefore the case
// this test is built around; the others fix the bar itself and a plainly
// qualifying month.
func TestProcessDistanceProof_EligibilityFollowsTheRoundedInteger(t *testing.T) {
	// The threshold as the contract holds it, derived the same way the extension
	// derives it, so a change to RewardThresholdKm moves both together.
	thresholdX1000 := int64(config.RewardThresholdKm * 1000)

	cases := []struct {
		name string
		// meters is what Strava reports for the month; totalKm is meters/1000.
		meters       float64
		wantX1000    int64
		wantEligible bool
	}{
		{"just below the bar", 1998.9, 1999, false},
		{"rounds up onto the bar", 1999.5, 2000, true},
		{"exactly on the bar", 2000, 2000, true},
		{"clearly above the bar", 5100, 5100, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := runDistanceProof(t, c.meters)

			if resp.DistanceX1000 != c.wantX1000 {
				t.Fatalf("distanceX1000: got %d, want %d (%.4f m)", resp.DistanceX1000, c.wantX1000, c.meters)
			}
			if resp.Eligible != c.wantEligible {
				t.Errorf("eligible: got %v, want %v for distanceX1000 %d (threshold %d)",
					resp.Eligible, c.wantEligible, resp.DistanceX1000, thresholdX1000)
			}
			// The flag and the number must agree, since the contract checks both:
			// claimReward requires eligible AND distanceX1000 >= the threshold,
			// while verifyDistanceProofFor answers on distanceX1000 alone.
			if want := resp.DistanceX1000 >= thresholdX1000; resp.Eligible != want {
				t.Errorf("eligible=%v contradicts distanceX1000=%d against threshold %d",
					resp.Eligible, resp.DistanceX1000, thresholdX1000)
			}
		})
	}
}

// --- State ---

func TestStateHandler_ReportsFields(t *testing.T) {
	e := &Extension{
		eligibleProofsSigned: 3,
		proofsSigned:         7,
	}

	req := httptest.NewRequest(http.MethodGet, "/state", nil)
	rec := httptest.NewRecorder()
	e.stateHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var sr types.StateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sr); err != nil {
		t.Fatalf("failed to unmarshal state response: %v", err)
	}

	if sr.StateVersion != teeutils.ToHash(config.Version) {
		t.Errorf("stateVersion: expected %s, got %s", teeutils.ToHash(config.Version).Hex(), sr.StateVersion.Hex())
	}
	if sr.State.EligibleProofsSigned != 3 {
		t.Errorf("eligibleProofsSigned: expected 3, got %d", sr.State.EligibleProofsSigned)
	}
	if sr.State.ProofsSigned != 7 {
		t.Errorf("proofsSigned: expected 7, got %d", sr.State.ProofsSigned)
	}
}

// --- Envelope parsing ---

func TestProcessAction_InvalidDataMessage(t *testing.T) {
	e := &Extension{}

	// Data.Message is not valid JSON — processorutils.Parse should fail
	action := teetypes.Action{
		Data: teetypes.ActionData{
			ID:      common.HexToHash("0xabcd"),
			Message: []byte(`not json at all`),
		},
	}

	status, body := e.processAction(context.Background(), action)

	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d for invalid Data.Message, got %d: %s",
			http.StatusBadRequest, status, body)
	}

	bodyStr := string(body)
	if !contains(bodyStr, "decoding fixed data") {
		t.Errorf("expected body to mention 'decoding fixed data', got %q", bodyStr)
	}
	t.Logf("400 body: %s", bodyStr)
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// --- Distance validation (an unchecked float becomes a signed attestation) ---

// TestValidatedKm_RejectsNonFiniteAndOutOfRange guards the float→int64 conversion that
// feeds distanceX1000 into the signed payload. Converting an out-of-range float to
// int64 is implementation-defined in Go and differs by architecture — on arm64 a NaN
// or Inf total packs cleanly and would be SIGNED as a bogus distance, while on amd64
// it becomes negative and fails to pack. Neither belongs in an attestation, so the
// value is screened before it ever reaches the encoder.
func TestValidatedKm_RejectsNonFiniteAndOutOfRange(t *testing.T) {
	if got, err := validatedKm(12.5); err != nil || got != 12.5 {
		t.Fatalf("valid distance rejected: got %v, err %v", got, err)
	}
	if _, err := validatedKm(0); err != nil {
		t.Errorf("zero distance should be valid: %v", err)
	}

	for name, km := range map[string]float64{
		"NaN":           math.NaN(),
		"+Inf":          math.Inf(1),
		"-Inf":          math.Inf(-1),
		"negative":      -1,
		"above ceiling": config.MaxMonthlyKm + 1,
	} {
		if _, err := validatedKm(km); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

// TestAbiEncodeDistanceProofPayload_RejectsNegative proves the encoder now surfaces
// what it used to swallow. args.Pack cannot represent a negative value as uint256, and
// discarding that error produced a nil payload that was then handed to the signer.
func TestAbiEncodeDistanceProofPayload_RejectsNegative(t *testing.T) {
	var challenge, athleteHash [32]byte
	id := common.HexToHash("0x01")
	addr := common.HexToAddress("0x00000000000000000000000000000000000000CC")

	if _, err := abiEncodeDistanceProofPayload(id, big.NewInt(114), addr, 1700000000, challenge, addr, addr, true, -1, 1698796800, athleteHash); err == nil {
		t.Error("expected an error for a negative distanceX1000")
	}
	if _, err := abiEncodeDistanceProofPayload(id, nil, addr, 1700000000, challenge, addr, addr, true, 5100, 1698796800, athleteHash); err == nil {
		t.Error("expected an error for a nil chainID")
	}
}

// TestSignPayload_RefusesEmpty pins that a mis-encoded payload is caught here rather
// than relying on the TEE node happening to reject a zero-length message.
func TestSignPayload_RefusesEmpty(t *testing.T) {
	if _, err := signPayload(context.Background(), nil); err == nil {
		t.Error("expected signPayload to refuse an empty payload")
	}
}

// TestMonthStartOf_IsPureFunctionOfItsArgument guards the month-boundary TOCTOU: the
// month used for the Strava query and the month written into the signed proof must
// come from ONE sampled instant. Deriving it twice would let a request straddling
// 00:00 UTC on the 1st report the previous month's kilometres under the new month's
// monthStart — which the contract would accept as a fresh month and pay out a second
// time. monthStartOf takes the instant as an argument precisely so the caller can
// sample once and reuse it.
func TestMonthStartOf_IsPureFunctionOfItsArgument(t *testing.T) {
	justBefore := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	justAfter := time.Date(2026, 2, 1, 0, 0, 1, 0, time.UTC)

	if got := monthStartOf(justBefore); !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("before rollover: got %s", got)
	}
	if got := monthStartOf(justAfter); !got.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("after rollover: got %s", got)
	}
	// Straddling instants must yield DIFFERENT months — which is exactly why the two
	// former independent samples could disagree within one request.
	if monthStartOf(justBefore).Equal(monthStartOf(justAfter)) {
		t.Fatal("expected the rollover to change the month")
	}
	// Non-UTC input must still normalise to the UTC month.
	loc := time.FixedZone("UTC+13", 13*3600)
	if got := monthStartOf(justBefore.In(loc)); !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("non-UTC input: got %s", got)
	}
}

// TestProcessDistanceProof_RefusesForeignDeployment pins the anchor on the two
// message fields that say where the resulting proof is valid.
//
// Both arrive in the instruction and are signed into the proof, so without this the
// enclave would attest "valid on chain X for contract Y" on the requester's word
// alone. The grant check does not cover it: the grant is sealed by the same
// requester, so a requester who picks both makes them agree trivially — which is
// exactly what the "consistent but foreign" case below constructs.
//
// Each case must be refused BEFORE any Strava call: httpClient is left pointing at a
// transport that fails the test if it is used at all, so a refusal that happened too
// late fails here rather than passing quietly.
func TestProcessDistanceProof_RefusesForeignDeployment(t *testing.T) {
	const (
		homeChain    = 114
		foreignChain = 16
	)
	homeContract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	foreignContract := common.HexToAddress("0x00000000000000000000000000000000000000DD")
	caller := common.HexToAddress("0x00000000000000000000000000000000000000A1")

	cases := []struct {
		name             string
		configuredChain  *big.Int
		configuredSender common.Address
		msgContract      common.Address
		msgChain         *big.Int
		wantSubstring    string
	}{
		{
			name:            "another chain, same contract",
			configuredChain: big.NewInt(homeChain), configuredSender: homeContract,
			msgContract: homeContract, msgChain: big.NewInt(foreignChain),
			wantSubstring: "names chain 16",
		},
		{
			name:            "another contract, same chain",
			configuredChain: big.NewInt(homeChain), configuredSender: homeContract,
			msgContract: foreignContract, msgChain: big.NewInt(homeChain),
			wantSubstring: "names contract",
		},
		{
			// The case the grant binding cannot catch: a requester who seals the
			// grant for the same foreign pair they put in the message.
			name:            "consistent but entirely foreign",
			configuredChain: big.NewInt(homeChain), configuredSender: homeContract,
			msgContract: foreignContract, msgChain: big.NewInt(foreignChain),
			wantSubstring: "refusing to sign",
		},
		{
			name:            "chain id absent from the message",
			configuredChain: big.NewInt(homeChain), configuredSender: homeContract,
			msgContract: homeContract, msgChain: big.NewInt(0),
			wantSubstring: "names chain 0",
		},
		{
			// Unconfigured must land where malformed lands. The other reading of an
			// unset value is "any chain, any contract" — the check being absent.
			name:            "enclave identity not configured",
			configuredChain: nil, configuredSender: common.Address{},
			msgContract: homeContract, msgChain: big.NewInt(homeChain),
			wantSubstring: "no deployment identity configured",
		},
		{
			name:            "only the chain configured",
			configuredChain: big.NewInt(homeChain), configuredSender: common.Address{},
			msgContract: homeContract, msgChain: big.NewInt(homeChain),
			wantSubstring: "no deployment identity configured",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withDeploymentIdentity(t, tc.configuredSender, tc.configuredChain)

			orig := httpClient
			httpClient = &http.Client{Transport: failingTransport{t}}
			t.Cleanup(func() { httpClient = orig })

			var challenge [32]byte
			action := buildTestAction(
				toHash(config.OPTypeStrava),
				toHash(config.OPCommandDistance),
				abiEncodeDistanceMessage(challenge, caller, tc.msgContract, tc.msgChain, []byte("ciphertext")),
			)

			status, body := (&Extension{}).processAction(context.Background(), action)
			if status != http.StatusOK {
				t.Fatalf("expected status %d, got %d", http.StatusOK, status)
			}
			var result teetypes.ActionResult
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatalf("unmarshalling result: %v", err)
			}
			if result.Status != 0 {
				t.Fatalf("signed a proof for a deployment this enclave does not belong to (Status=%d)", result.Status)
			}
			if !contains(result.Log, tc.wantSubstring) {
				t.Errorf("log %q does not mention %q", result.Log, tc.wantSubstring)
			}
		})
	}
}

// TestProcessDistanceProof_AcceptsItsOwnDeployment is the other direction: the
// refusals above must come from the mismatch, not from the check refusing broadly.
func TestProcessDistanceProof_AcceptsItsOwnDeployment(t *testing.T) {
	contract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	chainID := big.NewInt(114)
	withDeploymentIdentity(t, contract, chainID)

	if err := checkDeploymentIdentity(contract, big.NewInt(114)); err != nil {
		t.Fatalf("refused its own deployment: %v", err)
	}
	// A distinct *big.Int with the same value must compare equal.
	if err := checkDeploymentIdentity(contract, new(big.Int).SetInt64(114)); err != nil {
		t.Fatalf("compared chain ids by pointer rather than value: %v", err)
	}
}
