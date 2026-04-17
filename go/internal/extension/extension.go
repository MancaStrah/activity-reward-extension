package extension

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sync"
	"time"

	"activity-reward-extension/internal/config"
	"activity-reward-extension/pkg/types"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/go-flare-common/pkg/tee/instruction"
	teetypes "github.com/flare-foundation/tee-node/pkg/types"
	teeutils "github.com/flare-foundation/tee-node/pkg/utils"

	"github.com/flare-foundation/tee-node/pkg/processorutils"
)

type Extension struct {
	mu     sync.RWMutex
	Server *http.Server

	// Counters only. A per-user value here (e.g. the last athlete's distance) would
	// leak out of the enclave: State is served by GET /state and is carried in the
	// signed /info response used for on-chain availability checks.
	eligibleProofsSigned int
	proofsSigned         int
}

// New wires the two routes the container contract requires — GET /state and
// POST /action — and returns the server the node talks to. The route set and the
// handler signatures are fixed by docs/extension-contract.md, not by this extension.
func New(extensionPort, signPort int) *Extension {
	e := &Extension{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", e.stateHandler)
	mux.HandleFunc("POST /action", e.actionHandler)

	// Bind loopback explicitly, mirroring tee-node's SignHost. This server
	// authenticates nothing — every authenticity check (instruction signatures,
	// TeeID, InstructionID) lives in the node, and the grant check only compares the
	// grant against the context the *caller* supplied. Anyone able to reach this port
	// directly could therefore hand it a victim's ciphertext together with the
	// victim's own (public) caller/contract/chainId and read back that victim's
	// distance, athleteHash and a TEE signature. The node reaches us over loopback,
	// so there is no reason to listen anywhere else.
	e.Server = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", extensionPort),
		Handler: mux,
		// The node abandons an action after config.ActionBudget; keep our own
		// read/write bounds in the same neighbourhood so a stalled peer cannot pin a
		// connection open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return e
}

// stateHandler serves GET /state. The envelope (stateVersion + state) is fixed by
// the container contract; the State fields inside it are this extension's own.
func (e *Extension) stateHandler(w http.ResponseWriter, r *http.Request) {
	e.mu.RLock()
	stateResponse := types.StateResponse{
		StateVersion: teeutils.ToHash(config.Version),
		State: types.State{
			ProofsSigned:         e.proofsSigned,
			EligibleProofsSigned: e.eligibleProofsSigned,
		},
	}
	e.mu.RUnlock()

	err := json.NewEncoder(w).Encode(stateResponse)
	if err != nil {
		http.Error(w, fmt.Sprintf("sending response: %v", err), http.StatusInternalServerError)
		return
	}
}

func (e *Extension) processAction(ctx context.Context, action teetypes.Action) (int, []byte) {
	dataFixed, err := processorutils.Parse[instruction.DataFixed](action.Data.Message)
	if err != nil {
		return http.StatusBadRequest, []byte(fmt.Sprintf("decoding fixed data: %v", err))
	}

	switch {
	case dataFixed.OPType == teeutils.ToHash(config.OPTypeStrava):
		return e.processStrava(ctx, action, dataFixed)

	default:
		return http.StatusNotImplemented, []byte(fmt.Sprintf(
			"unsupported op type: received %s, expected %s (%s)",
			dataFixed.OPType.Hex(), teeutils.ToHash(config.OPTypeStrava).Hex(), config.OPTypeStrava,
		))
	}
}

// processStrava routes STRAVA instructions by OPCommand.
func (e *Extension) processStrava(ctx context.Context, action teetypes.Action, df *instruction.DataFixed) (int, []byte) {
	switch {
	case df.OPCommand == teeutils.ToHash(config.OPCommandDistance):
		ar := e.processDistanceProof(ctx, action, df)
		b, err := json.Marshal(ar)
		if err != nil {
			// Only reachable if a float in the result is non-finite, which the
			// distance validation already rules out — but returning a silent empty
			// body would report success with no payload, so fail loudly instead.
			return http.StatusInternalServerError, []byte(fmt.Sprintf("marshaling action result: %v", err))
		}
		return http.StatusOK, b

	default:
		return http.StatusNotImplemented, []byte(fmt.Sprintf(
			"unsupported op command: received %s, expected %s (%s)",
			df.OPCommand.Hex(),
			teeutils.ToHash(config.OPCommandDistance).Hex(), config.OPCommandDistance,
		))
	}
}

// processDistanceProof handles DISTANCE instructions — the extension's only
// operation. It checks the caller's monthly Strava distance and returns a signed
// proof. The TEE always signs regardless of eligibility so the contract can verify
// the result and emit RewardRefused for an ineligible athlete.
func (e *Extension) processDistanceProof(ctx context.Context, action teetypes.Action, df *instruction.DataFixed) teetypes.ActionResult {
	// 1. ABI-decode message: abi.encode(challenge, caller, verifyingContract, chainId, encryptedToken)
	values, err := types.DistanceMessageArgs.Unpack(df.OriginalMessage)
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("decoding distance message: %w", err))
	}
	challenge := values[0].([32]byte)
	caller := values[1].(common.Address)
	verifyingContract := values[2].(common.Address)
	chainID := values[3].(*big.Int)
	encryptedToken := values[4].([]byte)

	// The two fields that say WHERE the resulting proof is valid come from the
	// message and nowhere else. Anchor them to the deployment this enclave was
	// launched for before doing anything else: refusing here costs nothing, whereas
	// refusing after the fetch has already spent Strava quota that is shared by
	// every user of this extension.
	if err := checkDeploymentIdentity(verifyingContract, chainID); err != nil {
		return buildResult(action, df, nil, 0, err)
	}

	// Sample the clock ONCE. The month boundary is used both to scope the Strava
	// query and to label the signed proof; deriving it twice would let a request
	// straddling 00:00 UTC on the 1st report the previous month's kilometres under the
	// new month's monthStart, which the contract would accept as a fresh month and pay
	// again.
	startedAt := time.Now().UTC()
	monthStart := monthStartOf(startedAt)

	// 2. Decrypt and verify the caller-bound grant. The decrypted plaintext is a
	// structured grant; parseAndVerifyGrant enforces it was sealed for exactly this
	// caller, contract, chain, and operation, and has not expired — so a ciphertext
	// copied from another user's public tx calldata cannot be replayed to mint a
	// proof for their athlete.
	plaintext, err := decryptToken(ctx, encryptedToken)
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("decrypting token: %w", err))
	}
	token, err := parseAndVerifyGrant(plaintext, grantContext{
		caller:            caller,
		verifyingContract: verifyingContract,
		chainID:           chainID,
		purpose:           purposeDistance,
		now:               startedAt.Unix(),
	})
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("grant verification: %w", err))
	}

	// 3. Fetch athlete ID
	athleteID, err := fetchAthleteID(ctx, token)
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("fetching athlete: %w", err))
	}

	// 4. Fetch monthly distance for exactly the window we will attest to
	totalKm, err := fetchMonthlyDistance(ctx, token, monthStart, startedAt)
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("fetching distance: %w", err))
	}

	// 5. The month must not have rolled over while we were talking to Strava —
	// otherwise the figures we just fetched belong to a month we are about to
	// mislabel. Refuse rather than sign a proof for the wrong month.
	if now := time.Now().UTC(); !monthStartOf(now).Equal(monthStart) {
		return buildResult(action, df, nil, 0, fmt.Errorf(
			"month boundary crossed while fetching (started %s, now %s); retry",
			monthStart.Format(time.RFC3339), monthStartOf(now).Format(time.RFC3339)))
	}

	// 6. Determine eligibility from the same integer the proof carries and the
	// contract compares, not from the float. claimReward requires both `eligible`
	// and distanceX1000 >= DISTANCE_THRESHOLD_X1000, and verifyDistanceProofFor
	// answers on distanceX1000 alone, so a float comparison here would disagree with
	// its own rounded value in the sliver just under the bar (1.9995 km rounds to
	// 2000 while 1.9995 < 2.0) and the two answers would contradict each other.
	distanceX1000 := int64(math.Round(totalKm * 1000))
	eligible := distanceX1000 >= int64(config.RewardThresholdKm*1000)

	// 7. Derive the athlete identity (a public pseudonym — see hashAthleteID)
	athleteHash, err := hashAthleteID(athleteID)
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("hashing athlete: %w", err))
	}

	// 8. Build sign payload (eligible flag reflects actual eligibility). The payload
	// is domain-separated and bound to this chain, this contract, and this specific
	// instruction — it must agree byte-for-byte with claimReward in the contract.
	// df.TeeID is this machine's own address, so the contract can require that the
	// recovered signer equals both the proof's teeId and the requested one.
	timestamp := time.Now().UTC().Unix()
	payload, err := abiEncodeDistanceProofPayload(df.InstructionID, chainID, verifyingContract, timestamp, challenge, caller, df.TeeID, eligible, distanceX1000, monthStart.Unix(), athleteHash)
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("encoding sign payload: %w", err))
	}

	// 9. Sign (always sign, regardless of eligibility)
	signature, err := signPayload(ctx, payload)
	if err != nil {
		return buildResult(action, df, nil, 0, fmt.Errorf("signing: %w", err))
	}

	// 9. Build response
	var message string
	if eligible {
		message = fmt.Sprintf("Eligible! You covered %.1f km this month. Claim your 1 C2FLR reward on-chain.", totalKm)
	} else {
		remaining := config.RewardThresholdKm - totalKm
		message = fmt.Sprintf("Not eligible. You covered %.1f km this month. %.1f km more needed.", totalKm, remaining)
	}

	resp := types.DistanceResponse{
		DistanceProof: types.DistanceProof{
			Timestamp:     timestamp,
			Challenge:     "0x" + hex.EncodeToString(challenge[:]),
			Caller:        caller.Hex(),
			TeeID:         df.TeeID.Hex(),
			Eligible:      eligible,
			DistanceKm:    totalKm,
			DistanceX1000: distanceX1000,
			MonthStart:    monthStart.Unix(),
			AthleteHash:   "0x" + hex.EncodeToString(athleteHash[:]),
			Signature:     "0x" + hex.EncodeToString(signature),
		},
		Message: message,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		// DistanceKm is a float; a non-finite value would fail here. The distance
		// validation rules that out, so this is belt-and-braces — but reporting
		// success with an empty payload would be worse than a clear error.
		return buildResult(action, df, nil, 0, fmt.Errorf("marshaling response: %w", err))
	}

	// 11. Update state (counters only — no per-user value, see the Extension struct)
	e.mu.Lock()
	e.proofsSigned++
	if eligible {
		e.eligibleProofsSigned++
	}
	e.mu.Unlock()

	return buildResult(action, df, data, 1, nil)
}

// hashAthleteID derives the athlete identity the contract uses for the
// one-reward-per-athlete-per-month guard: keccak256 of the zero-padded Strava id.
//
// This is a PUBLIC PSEUDONYM, not an anonymising hash, and that is a deliberate
// choice. Strava athlete ids are small sequential integers, so anyone can enumerate
// the id space, hash each candidate, and match it against the athleteHash published
// in RewardClaimed/RewardRefused — recovering the athlete's public Strava profile
// and linking it to their wallet. The hash therefore serves only to give the
// contract a stable, collision-free key for deduplication; it does not hide who the
// athlete is.
//
// A keyed MAC cannot fix that here. It needs a secret shared by every TEE of the
// extension, and the enclave cannot generate one: Confidential Space has no
// persistent storage, so each restart would mint a new key and re-map every athlete,
// letting already-paid athletes claim again. Provisioning it as a workload env var
// instead hands the salt to the operator, who could then de-anonymise anyway — which
// moves the problem rather than solving it. The production-grade answer is
// attestation-gated secret release; short of that, the honest position is to treat
// the pseudonym as public.
//
// The positive-id check is not cosmetic: a missing Strava "id" field decodes to 0,
// and big.Int.Bytes() drops the sign, so 0 or a negative id would collapse distinct
// users onto one shared athleteHash and the first payout would lock out the rest.
func hashAthleteID(athleteID int64) ([32]byte, error) {
	var athleteHash [32]byte
	if athleteID <= 0 {
		return athleteHash, fmt.Errorf("athlete id must be positive, got %d", athleteID)
	}
	copy(athleteHash[:], crypto.Keccak256(common.LeftPadBytes(big.NewInt(athleteID).Bytes(), 32)))
	return athleteHash, nil
}

// checkDeploymentIdentity refuses an instruction whose `verifyingContract` or
// `chainId` is not the pair this enclave was launched for.
//
// Those two values arrive inside the instruction message and are signed into the
// proof, so on their own they are self-asserting: nothing downstream in the enclave
// disagrees with a message that names a different chain or a different contract.
// parseAndVerifyGrant binds the grant to the same pair, but that proves the message
// and the grant AGREE — both are supplied by the same requester, so they agree
// trivially when the requester chose both.
//
// The contract is the other half of the check and it is already sound: claimReward
// recomputes the payload hash with its own address and block.chainid, so a proof
// naming anything else simply fails to verify there. What that does not do is stop
// this enclave from producing such a proof, or from spending the shared Strava quota
// to produce it. Refusing to sign at all is the stronger position: the signing key
// is the extension's identity, and it should only ever assert things about the
// deployment it belongs to.
//
// Unset configuration is refused rather than waved through — see config.ChainID.
func checkDeploymentIdentity(verifyingContract common.Address, chainID *big.Int) error {
	if config.ChainID == nil || config.InstructionSender == (common.Address{}) {
		return fmt.Errorf(
			"refusing to sign: this enclave has no deployment identity configured (CHAIN_ID and INSTRUCTION_SENDER must both be set)")
	}
	if chainID == nil || chainID.Cmp(config.ChainID) != 0 {
		return fmt.Errorf(
			"refusing to sign: instruction names chain %s, this enclave is deployed for chain %s",
			chainIDString(chainID), config.ChainID)
	}
	if verifyingContract != config.InstructionSender {
		return fmt.Errorf(
			"refusing to sign: instruction names contract %s, this enclave is deployed for %s",
			verifyingContract.Hex(), config.InstructionSender.Hex())
	}
	return nil
}

// chainIDString renders a possibly-nil chain id for a refusal message.
func chainIDString(chainID *big.Int) string {
	if chainID == nil {
		return "none"
	}
	return chainID.String()
}

// abiEncodeDistanceProofPayload ABI-encodes the payload that the TEE signs and
// claimReward() / verifyDistanceProof() reconstruct:
//
//	abi.encode(DOMAIN_DISTANCE_PROOF, chainId, verifyingContract, instructionId,
//	           timestamp, challenge, caller, teeId, eligible, distanceX1000,
//	           monthStart, athleteHash)
//
// The leading domain/chainId/contract/instructionId fields bind the signature to
// this operation, chain, contract, and specific instruction, so it cannot be
// replayed across extensions, contracts, chains, or instructions. caller, teeId and
// athleteHash are all covered, so the contract can check every identity it cares
// about. Must match the abi.encode(...) in _recoverProofSigner() in
// contracts/InstructionSender.sol EXACTLY — the paired vector tests enforce it.
// The error is returned rather than swallowed: Pack fails on a negative
// distanceX1000 (which an out-of-range float→int64 conversion can produce), and
// ignoring it would hand a nil payload to the signer.
func abiEncodeDistanceProofPayload(instructionID common.Hash, chainID *big.Int, verifyingContract common.Address, timestamp int64, challenge [32]byte, caller common.Address, teeID common.Address, eligible bool, distanceX1000 int64, monthStart int64, athleteHash [32]byte) ([]byte, error) {
	if distanceX1000 < 0 || monthStart < 0 || timestamp < 0 {
		return nil, fmt.Errorf("negative value in payload (distanceX1000=%d, monthStart=%d, timestamp=%d)",
			distanceX1000, monthStart, timestamp)
	}
	if chainID == nil {
		return nil, fmt.Errorf("chainID is nil")
	}

	uint256Ty, _ := abi.NewType("uint256", "", nil)
	bytes32Ty, _ := abi.NewType("bytes32", "", nil)
	addressTy, _ := abi.NewType("address", "", nil)
	boolTy, _ := abi.NewType("bool", "", nil)

	args := abi.Arguments{
		{Type: bytes32Ty}, // DOMAIN_DISTANCE_PROOF
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

	packed, err := args.Pack(
		[32]byte(domainDistanceProof),
		chainID,
		verifyingContract,
		[32]byte(instructionID),
		big.NewInt(timestamp),
		challenge,
		caller,
		teeID,
		eligible,
		big.NewInt(distanceX1000),
		big.NewInt(monthStart),
		athleteHash,
	)
	if err != nil {
		return nil, fmt.Errorf("packing sign payload: %w", err)
	}
	return packed, nil
}
