package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"activity-reward-extension/tools/pkg/configs"
	"activity-reward-extension/tools/pkg/contracts/strava"
	"activity-reward-extension/tools/pkg/fccutils"
	"activity-reward-extension/tools/pkg/support"
	instrutils "activity-reward-extension/tools/pkg/utils"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/ecies"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
	"github.com/flare-foundation/tee-node/pkg/types"
	"github.com/pkg/errors"
)

// maxSignatureLen bounds the signature field before it is parsed. A 65-byte
// ECDSA signature is what the contract accepts; the cap only stops the proxy
// from handing back an unbounded string.
const maxSignatureLen = 1024

// Expected response shapes for the STRAVA operations.
//
// These are deliberately declared here rather than imported from the extension:
// this tool asserts on the *wire format*, and tools/ stays independent of the
// extension implementation (see docs/extension-contract.md).

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

func main() {
	af := flag.String("a", configs.AddressesFile, "file with deployed addresses")
	cf := flag.String("c", configs.ChainNodeURL, "chain node url")
	pf := flag.String("p", configs.ExtensionProxyURL, "extension proxy url")
	instructionSenderF := flag.String("instructionSender", "", "instructionSender address")
	extensionIDF := flag.String("extensionID", "", "extension id (decimal or 0x-hex); skips the registry scan, verified with a single registry call")
	flag.Parse()

	// Parsed exactly, like the proof fields in convertProof: the lenient helper
	// would turn a mistyped address into a valid-looking one and drive the whole
	// run against the wrong contract.
	instructionSenderAddress, err := fccutils.StrictAddress("-instructionSender", *instructionSenderF)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	// Same bar as claim-reward/encrypt-token: this tool fetches the TEE
	// public key from the proxy /info and encrypts a real Strava token to it,
	// so neither the proxy URL nor the RPC that drives the on-chain TEE checks
	// may be tamperable in transit. HTTPS or loopback only; ALLOW_INSECURE_RPC
	// is the explicit dev escape hatch for the chain URL.
	if err := fccutils.RequireSecureProxyURL(*pf); err != nil {
		fccutils.FatalWithCause(err)
	}
	if err := fccutils.RequireSecureChainURL(*cf); err != nil {
		fccutils.FatalWithCause(err)
	}

	testSupport, err := support.DefaultSupport(*af, *cf)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	// --- Generic: configure contract -----------------------------------------
	logger.Infof("Setting extension ID on instruction sender...")
	var expectedExtID *big.Int
	if *extensionIDF != "" {
		var ok bool
		expectedExtID, ok = new(big.Int).SetString(*extensionIDF, 0)
		if !ok {
			fccutils.FatalWithCause(errors.Errorf("invalid -extensionID value %q (want decimal or 0x-hex)", *extensionIDF))
		}
		if err := instrutils.CheckExtensionId(testSupport, instructionSenderAddress, expectedExtID); err != nil {
			fccutils.FatalWithCause(err)
		}
		logger.Infof("Extension ID %s verified against registry", expectedExtID.String())
	} else {
		expectedExtID, err = instrutils.ResolveExtensionId(testSupport, instructionSenderAddress)
		if err != nil {
			fccutils.FatalWithCause(errors.Errorf(
				"resolving extension id — is the extension registered? Check that pre-build.sh completed successfully. Error: %s", err))
		}
	}
	err = instrutils.SetExtensionId(testSupport, instructionSenderAddress, expectedExtID)
	if err != nil {
		if strings.Contains(err.Error(), "already set") || strings.Contains(err.Error(), "Extension ID already set") {
			logger.Infof("Extension ID already set on contract, continuing")
		} else {
			logger.Errorf("setExtensionId failed: %s", err)
			fccutils.FatalWithCause(errors.Errorf(
				"setExtensionId failed — is the extension registered? Check that pre-build.sh completed successfully. Error: %s", err))
		}
	}

	// --- Fetch TEE info for ECIES encryption ---
	logger.Infof("Fetching TEE info from proxy...")
	teeInfo, err := fccutils.TeeInfo(*pf)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("fetch TEE info: %s", err))
	}

	ecdsaPub, err := types.ParsePubKey(teeInfo.MachineData.PublicKey)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("parse TEE public key: %s", err))
	}
	teeAddr := crypto.PubkeyToAddress(*ecdsaPub)
	logger.Infof("TEE address: %s", teeAddr.Hex())

	// Verify the /info key belongs to a registered, attested PRODUCTION
	// TEE before encrypting a secret to it. teeAddr commits to the key, so a
	// substituted key would not be PRODUCTION and this fails closed.
	sender, err := strava.NewStravaInstructionSender(instructionSenderAddress, testSupport.ChainClient)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("bind contract: %s", err))
	}

	// Anchor the expected extension on the contract, not on the proxy's /info.
	expectedExt, err := sender.ExtensionId(&bind.CallOpts{Context: context.Background()})
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("reading extensionId() (has setExtensionId run?): %s", err))
	}
	if err := fccutils.VerifyTeeProduction(testSupport.TeeMachineRegistry, teeAddr, expectedExt); err != nil {
		fccutils.FatalWithCause(errors.Errorf("TEE verification failed: %s", err))
	}
	logger.Infof("Verified TEE %s is a PRODUCTION machine of extension %s", teeAddr.Hex(), expectedExt.String())

	// Fund contract with 2 C2FLR (enough for 2 claims)
	logger.Infof("Funding contract with 2 C2FLR...")
	from := crypto.PubkeyToAddress(testSupport.Prv.PublicKey)
	nonce, err := testSupport.ChainClient.PendingNonceAt(context.Background(), from)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("getting nonce: %s", err))
	}
	fundValue := new(big.Int).Mul(big.NewInt(2), big.NewInt(1e18)) // 2 C2FLR
	head, err := testSupport.ChainClient.HeaderByNumber(context.Background(), nil)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("getting latest header: %s", err))
	}
	tip := big.NewInt(2_000_000_000) // 2 gwei priority fee
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	fundTx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID:   testSupport.ChainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       30000,
		To:        &instructionSenderAddress,
		Value:     fundValue,
	})
	signer := ethtypes.LatestSignerForChainID(testSupport.ChainID)
	fundTx, err = ethtypes.SignTx(fundTx, signer, testSupport.Prv)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("signing fund tx: %s", err))
	}
	err = testSupport.ChainClient.SendTransaction(context.Background(), fundTx)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("funding send failed: %s", err))
	}
	fundReceipt, err := bind.WaitMined(context.Background(), testSupport.ChainClient, fundTx)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("funding tx wait failed: %s", err))
	}
	if fundReceipt.Status != 1 {
		fccutils.FatalWithCause(errors.Errorf("funding tx reverted, hash: %s", fundTx.Hash().Hex()))
	}
	logger.Infof("Contract funded with 2 C2FLR (tx: %s)", fundTx.Hash().Hex())

	// Check actual contract balance
	bal, err := testSupport.ChainClient.BalanceAt(context.Background(), instructionSenderAddress, nil)
	if err != nil {
		logger.Warnf("Failed to check contract balance: %s", err)
	} else {
		logger.Infof("Contract balance: %s wei (%s C2FLR)", bal.String(), new(big.Float).Quo(new(big.Float).SetInt(bal), new(big.Float).SetInt(big.NewInt(1e18))).Text('f', 4))
	}

	// --- Verify Go/Solidity month-start implementations agree ---
	logger.Infof("Verifying Go/Solidity month-start implementations agree...")
	monthStartSelector := crypto.Keccak256([]byte("currentMonthStart()"))[:4]
	monthStartResult, err := testSupport.ChainClient.CallContract(context.Background(), ethereum.CallMsg{
		To:   &instructionSenderAddress,
		Data: monthStartSelector,
	}, nil)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("calling currentMonthStart(): %s", err))
	}
	contractMonthStart := new(big.Int).SetBytes(monthStartResult).Int64()
	now := time.Now().UTC()
	goMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	if contractMonthStart != goMonthStart {
		fccutils.FatalWithCause(errors.Errorf("month-start mismatch: contract=%d, go=%d", contractMonthStart, goMonthStart))
	}
	logger.Infof("Month-start check passed: both return %d", goMonthStart)

	// --- Encrypt Strava token with TEE's public key ---
	stravaToken := os.Getenv("STRAVA_TOKEN")
	if stravaToken == "" {
		logger.Fatalf("STRAVA_TOKEN environment variable is not set")
	}

	eciesPub := &ecies.PublicKey{
		X:      ecdsaPub.X,
		Y:      ecdsaPub.Y,
		Curve:  ecies.DefaultCurve,
		Params: ecies.ECIES_AES128_SHA256,
	}

	// Seal the token in caller/contract/chain/purpose-bound grants
	// with an expiry, then encrypt. Instructions are sent from `from` (msg.sender),
	// to instructionSenderAddress, on testSupport.ChainID. Reward and distance get
	// separate grants because each is bound to its own purpose.
	expiry := time.Now().Add(15 * time.Minute).Unix()

	grantPlain, err := fccutils.GrantPlaintext(fccutils.PurposeDistance, from, instructionSenderAddress, testSupport.ChainID, expiry, stravaToken)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("building distance grant: %s", err))
	}
	encryptedToken, err := ecies.Encrypt(rand.Reader, eciesPub, grantPlain, nil, nil)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("ECIES encrypt: %s", err))
	}
	logger.Infof("Sealed caller-bound grant (caller %s, contract %s): %d bytes",
		from.Hex(), instructionSenderAddress.Hex(), len(encryptedToken))

	// --- Test case 1: Request a distance proof ---
	logger.Infof("Sending DISTANCE instruction (getDistanceProof)...")

	instructionId, rewardTxHash, err := instrutils.GetDistanceProof(testSupport, instructionSenderAddress, teeAddr, encryptedToken)
	if err != nil {
		fccutils.FatalWithCause(err)
	}
	logger.Infof("Instruction sent. ID: %s", instructionId.Hex())
	printInstructionEvent(testSupport, rewardTxHash)

	time.Sleep(5 * time.Second)

	rewardResp, err := getDistanceResult(*pf, instructionId)
	if err != nil {
		fccutils.FatalWithCause(err)
	}
	// distanceKm is reported from the SIGNED distanceX1000: the response's own
	// distanceKm field is the one part of the result the TEE never signs, so it is
	// not evidence of anything. convertProof below additionally refuses a response
	// where the two disagree.
	logger.Infof("DISTANCE result: eligible=%v, distanceKm=%.1f, message=%s",
		rewardResp.Eligible, float64(rewardResp.DistanceX1000)/1000,
		fccutils.SanitizeForTerminal(rewardResp.Message))

	// --- Test case 2: Verify the proof on-chain while it is still unconsumed ---
	// verifyDistanceProof is single-use BY DESIGN: _isAuthenticFreshProof requires
	// pendingProofs[caller].instructionId to still equal this instruction, so it has
	// to run BEFORE claimReward deletes that record. Test case 4 pins the other half.
	if rewardResp.Signature == "" {
		fccutils.FatalWithCause(errors.New("expected proof signature in response but got none"))
	}

	proof, err := convertProof(&rewardResp.DistanceProof)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("converting proof: %s", err))
	}

	logger.Infof("Verifying the proof on-chain via verifyDistanceProof...")
	callOpts := &bind.CallOpts{Context: context.Background()}

	genuineOK, err := sender.VerifyDistanceProof(callOpts, instructionId, *proof)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("verifyDistanceProof call failed: %s", err))
	}
	if !genuineOK {
		fccutils.FatalWithCause(errors.New("verifyDistanceProof returned false for a genuine, unconsumed TEE proof"))
	}
	logger.Infof("verifyDistanceProof accepted the genuine proof")

	tampered := *proof
	tampered.DistanceX1000 = new(big.Int).Add(proof.DistanceX1000, big.NewInt(1))
	tamperedOK, err := sender.VerifyDistanceProof(callOpts, instructionId, tampered)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("verifyDistanceProof (tampered) call failed: %s", err))
	}
	if tamperedOK {
		fccutils.FatalWithCause(errors.New("verifyDistanceProof accepted a tampered proof"))
	}
	logger.Infof("verifyDistanceProof rejected the tampered proof")

	// --- Test case 3: Claim reward on-chain (proof is always present) ---
	if rewardResp.Eligible {
		logger.Infof("User is eligible! Claiming reward on-chain...")
	} else {
		logger.Infof("User is NOT eligible. Submitting proof on-chain (expecting RewardRefused)...")
	}

	claimOpts, err := bind.NewKeyedTransactorWithChainID(testSupport.Prv, testSupport.ChainID)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	claimTx, err := sender.ClaimReward(claimOpts, instructionId, *proof)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("claimReward failed: %s", err))
	}

	receipt, err := bind.WaitMined(context.Background(), testSupport.ChainClient, claimTx)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("claimReward tx wait failed: %s", err))
	}

	if receipt.Status == 1 {
		if rewardResp.Eligible {
			logger.Infof("Test passed: claimReward succeeded! 1 C2FLR transferred (RewardClaimed). TX: %s", claimTx.Hash().Hex())
		} else {
			logger.Infof("Test passed: claimReward succeeded with RewardRefused. TX: %s", claimTx.Hash().Hex())
		}
	} else {
		fccutils.FatalWithCause(errors.Errorf("claimReward transaction reverted. TX: %s", claimTx.Hash().Hex()))
	}

	// --- Test case 4: Consuming the proof stops the contract vouching for it ---
	// Proofs are public: they sit in claimReward calldata and on the proxy's
	// unauthenticated result endpoint. So a consumed proof MUST stop verifying, or
	// anyone replaying that calldata could keep satisfying an integrator that grants
	// something per verification. Mirrors test_VerifyStopsVouchingOnceProofIsConsumed.
	consumedOK, err := sender.VerifyDistanceProof(callOpts, instructionId, *proof)
	if err != nil {
		fccutils.FatalWithCause(errors.Errorf("verifyDistanceProof (consumed) call failed: %s", err))
	}
	if consumedOK {
		fccutils.FatalWithCause(errors.New("verifyDistanceProof still vouched for a proof that claimReward consumed"))
	}
	logger.Infof("verifyDistanceProof correctly stopped vouching for the consumed proof")

	logger.Infof("All tests passed.")
}

// getDistanceResult polls the proxy and returns the parsed DistanceResponse.
func getDistanceResult(proxyURL string, instructionId common.Hash) (*DistanceResponse, error) {
	actionResponse, err := fccutils.ActionResult(proxyURL, instructionId)
	if err != nil {
		return nil, err
	}
	actionResult := actionResponse.Result

	if actionResult.Status == 0 {
		return nil, errors.Errorf("instruction processing failed: %s",
			fccutils.SanitizeForTerminal(actionResult.Log))
	}
	if actionResult.Status == 2 {
		return nil, errors.New("instruction still pending after polling, expected completed")
	}
	if len(actionResult.Data) == 0 {
		return nil, errors.New("expected response data but got none")
	}

	var resp DistanceResponse
	if err := json.Unmarshal(actionResult.Data, &resp); err != nil {
		return nil, errors.Errorf("failed to unmarshal response: %s", err)
	}

	if resp.Message == "" {
		return nil, errors.New("expected non-empty Message")
	}
	if resp.Signature == "" {
		return nil, errors.New("proof must include a signature")
	}

	return &resp, nil
}

// convertProof converts the JSON proof into the Solidity struct for claimReward,
// parsing every field exactly first.
//
// All of it arrives from the proxy's unauthenticated result endpoint. The lenient
// go-ethereum helpers turn malformed input into a well-formed-looking address or
// hash by zero-padding and truncating, so a mangled value would travel on into a
// transaction instead of stopping here; fccutils.StrictAddress additionally
// verifies the EIP-55 checksum when the value carries one. The numeric fields need
// screening for the same reason: a negative int64 is not rejected by the ABI
// encoder but packed as a huge uint256 by two's complement.
//
// distanceKm gets a check of a different kind. It is the one field of the result
// the TEE does not sign, so there is nothing to parse it against — it is screened
// for agreement with the signed distanceX1000 instead, because the extension
// derives both from a single value and a result where they disagree did not come
// from it.
//
// The tampered-proof assertion in main() mutates the struct this returns, so it is
// unaffected by the parsing here — what it tampers with is already valid.
func convertProof(p *DistanceProof) (*strava.StravaInstructionSenderDistanceProof, error) {
	challenge, err := fccutils.StrictHash("challenge", p.Challenge)
	if err != nil {
		return nil, err
	}

	athleteHash, err := fccutils.StrictHash("athleteHash", p.AthleteHash)
	if err != nil {
		return nil, err
	}

	caller, err := fccutils.StrictAddress("caller", p.Caller)
	if err != nil {
		return nil, err
	}

	teeID, err := fccutils.StrictAddress("teeId", p.TeeID)
	if err != nil {
		return nil, err
	}

	sigBytes, err := fccutils.StrictBytes("signature", p.Signature, maxSignatureLen)
	if err != nil {
		return nil, err
	}

	if p.Timestamp < 0 || p.MonthStart < 0 || p.DistanceX1000 < 0 {
		return nil, errors.Errorf("negative timestamp (%d), monthStart (%d) or distanceX1000 (%d)",
			p.Timestamp, p.MonthStart, p.DistanceX1000)
	}
	if err := fccutils.CheckDistanceAgreement(p.DistanceKm, p.DistanceX1000); err != nil {
		return nil, err
	}

	return &strava.StravaInstructionSenderDistanceProof{
		Timestamp:     big.NewInt(p.Timestamp),
		Challenge:     challenge,
		Caller:        caller,
		TeeId:         teeID,
		Eligible:      p.Eligible,
		DistanceX1000: big.NewInt(p.DistanceX1000),
		MonthStart:    big.NewInt(p.MonthStart),
		AthleteHash:   athleteHash,
		Signature:     sigBytes,
	}, nil
}

// printInstructionEvent fetches the tx receipt, parses the TeeInstructionsSent event, and prints it.
func printInstructionEvent(s *support.Support, txHash common.Hash) {
	receipt, err := s.ChainClient.TransactionReceipt(context.Background(), txHash)
	if err != nil {
		logger.Warnf("Failed to get receipt for event printing: %s", err)
		return
	}
	if len(receipt.Logs) == 0 {
		logger.Warnf("No logs in receipt for tx %s", txHash.Hex())
		return
	}

	event, err := s.TeeVerification.ParseTeeInstructionsSent(*receipt.Logs[0])
	if err != nil {
		logger.Warnf("Failed to parse TeeInstructionsSent event: %s", err)
		return
	}

	fmt.Println("┌─── TeeInstructionsSent Event ───")
	fmt.Printf("│ ExtensionID:        %s\n", event.ExtensionId.String())
	fmt.Printf("│ InstructionID:      0x%s\n", hex.EncodeToString(event.InstructionId[:]))
	fmt.Printf("│ RewardEpochID:      %d\n", event.RewardEpochId)
	fmt.Printf("│ OpType:             %s\n", bytes32ToString(event.OpType))
	fmt.Printf("│ OpCommand:          %s\n", bytes32ToString(event.OpCommand))
	fmt.Printf("│ Fee:                %s\n", event.Fee.String())
	fmt.Printf("│ CosignersThreshold: %d\n", event.CosignersThreshold)
	fmt.Printf("│ ClaimBackAddress:   %s\n", event.ClaimBackAddress.Hex())
	fmt.Printf("│ Message:            0x%s\n", hex.EncodeToString(event.Message))
	if len(event.Cosigners) > 0 {
		fmt.Printf("│ Cosigners:          ")
		for i, c := range event.Cosigners {
			if i > 0 {
				fmt.Printf(", ")
			}
			fmt.Printf("%s", c.Hex())
		}
		fmt.Println()
	}
	for _, m := range event.TeeMachines {
		fmt.Printf("│ TEE Machine:\n")
		fmt.Printf("│   TeeID:    %s\n", m.TeeId.Hex())
		fmt.Printf("│   ProxyID:  %s\n", m.TeeProxyId.Hex())
		fmt.Printf("│   ProxyURL: %s\n", m.Url)
	}
	fmt.Println("└─────────────────────────────────")
}

func bytes32ToString(b [32]byte) string {
	// Trim trailing null bytes to get the readable string
	n := 0
	for n < 32 && b[n] != 0 {
		n++
	}
	return string(b[:n])
}
