// claim-reward runs the whole caller-side flow for the Strava distance reward:
//
//	fetch the TEE  →  seal + encrypt the Strava token  →  getDistanceProof
//	→  poll the proxy for the signed proof  →  claimReward
//
// Unlike run-test this asserts nothing and configures nothing: it never calls
// setExtensionId, never funds the contract, and never sends a tampered proof.
// It is just the path a real user takes. Deployment setup stays in
// post-build.sh; the assertions stay in run-test.
package main

import (
	"context"
	"crypto/ecdsa"
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

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/ecies"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/flare-foundation/tee-node/pkg/types"
	"github.com/joho/godotenv"
	"github.com/pkg/errors"
)

// maxSignatureLen bounds the signature field before it is parsed. A 65-byte
// ECDSA signature is what the contract accepts; the cap only stops the proxy
// from handing back an unbounded string.
const maxSignatureLen = 1024

// DistanceProof mirrors the JSON the TEE returns. Declared here rather than
// imported from the extension so tools/ stays independent of the
// implementation — same reasoning as run-test (see docs/extension-contract.md).
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

func init() {
	// Populate the env-backed flag defaults below. Tolerates being run from the
	// repo root or from tools/ (where `go run ./cmd/...` puts us).
	for _, p := range []string{".env", "../.env"} {
		if godotenv.Load(p) == nil {
			break
		}
	}
}

func main() {
	keyF := flag.String("key", "", "private key hex, with or without 0x (default: $DEPLOYMENT_PRIVATE_KEY)")
	tokenF := flag.String("token", "", "Strava access token (default: $STRAVA_TOKEN)")
	contractF := flag.String("contract", os.Getenv("INSTRUCTION_SENDER"), "StravaInstructionSender address (default: $INSTRUCTION_SENDER)")
	pf := flag.String("p", envOr("EXT_PROXY_URL", configs.ExtensionProxyURL), "extension proxy url")
	cf := flag.String("c", envOr("CHAIN_URL", configs.ChainNodeURL), "chain rpc url")
	af := flag.String("a", envOr("ADDRESSES_FILE", configs.AddressesFile), "deployed addresses file")
	ttlF := flag.Int64("ttl", 900, "grant time-to-live in seconds")
	claimF := flag.Bool("claim", true, "submit claimReward; -claim=false stops after printing the proof")
	jsonF := flag.Bool("json", false, "also print the raw proof JSON")
	setExtF := flag.Bool("set-extension-id", false, "bootstrap a freshly deployed contract by calling setExtensionId() — ONE-SHOT and permanent")
	extIDF := flag.String("extension-id", "", "with --set-extension-id: the extension id to bind (decimal or 0x); default resolves it from the registry")
	flag.Parse()

	// A secret on the command line is visible in `ps` and lands in shell history;
	// the env-var path is the safe default. Warn rather than reject so existing
	// scripts keep working.
	warnSecretFlag("key", *keyF, "DEPLOYMENT_PRIVATE_KEY")
	warnSecretFlag("token", *tokenF, "STRAVA_TOKEN")

	// --- Inputs -------------------------------------------------------------
	prv, err := parseKey(*keyF)
	if err != nil {
		die("%s", err)
	}
	from := crypto.PubkeyToAddress(prv.PublicKey)

	token := *tokenF
	if token == "" {
		token = os.Getenv("STRAVA_TOKEN")
	}
	if token == "" {
		die("no Strava token: pass -token or set STRAVA_TOKEN")
	}
	if *contractF == "" {
		die("no contract: pass -contract or set INSTRUCTION_SENDER (config/extension.env has it)")
	}
	// Parsed exactly, like the proof fields below: IsHexAddress checks the shape
	// but not the EIP-55 checksum, and inside 40 valid hex digits a transposed or
	// mistyped character is still 40 valid hex digits — a different contract that
	// the grant would then be sealed to, costing the user an instruction fee for a
	// proof the TEE will refuse.
	contractAddr, err := fccutils.StrictAddress("-contract", *contractF)
	if err != nil {
		die("%s", err)
	}

	// The TEE caps the grant lifetime (config.MaxGrantTTL); reject an over-long
	// one here so it fails before the token is sealed, not after it is sent.
	const maxTTLSeconds = int64(24 * 60 * 60)
	if *ttlF <= 0 || *ttlF > maxTTLSeconds {
		die("invalid -ttl %d: must be between 1 and %d seconds", *ttlF, maxTTLSeconds)
	}

	// Same bar as encrypt-token: the /info key is about to receive a secret, so
	// refuse to fetch it over a transport a MITM could rewrite. VerifyTeeProduction
	// below would catch a swapped key too — this is the cheaper, earlier stop.
	if err := fccutils.RequireSecureProxyURL(*pf); err != nil {
		die("%s", err)
	}
	// The RPC drives the on-chain TEE checks that gate token encryption, so it must
	// not be tamperable in transit either.
	if err := fccutils.RequireSecureChainURL(*cf); err != nil {
		die("%s", err)
	}

	// --- Chain + contract ---------------------------------------------------
	cc, err := support.DialBounded(*cf)
	if err != nil {
		die("dialling %s: %s", *cf, err)
	}
	addrs := &support.Addresses{}
	if configs.ReadAddresses(*af, addrs) != nil {
		if addrs, err = support.ParseAddresses(*af); err != nil {
			die("reading addresses from %s: %s", *af, err)
		}
	}
	s, err := support.NewSupport(prv, cc, addrs)
	if err != nil {
		die("building chain support: %s", err)
	}
	sender, err := strava.NewStravaInstructionSender(contractAddr, cc)
	if err != nil {
		die("binding contract %s: %s", contractAddr.Hex(), err)
	}
	callOpts := &bind.CallOpts{Context: context.Background()}

	fmt.Printf("Caller:    %s\n", from.Hex())
	fmt.Printf("Contract:  %s\n", contractAddr.Hex())
	fmt.Printf("Proxy:     %s\n", *pf)
	fmt.Printf("Chain:     %s (id %s)\n\n", *cf, s.ChainID)

	// --- Resolve and verify the TEE ----------------------------------------
	// Anchored on the contract's extensionId, never on the proxy's own /info:
	// a substituted key would not be PRODUCTION for this extension, so a
	// tampering proxy fails closed here rather than receiving the token.
	// A freshly deployed contract has no extension id until setExtensionId() runs.
	// Nothing in scripts/ does it — only run-test — so offer it here, but behind a
	// flag: the setter is one-shot, and a wrong binding means redeploying.
	extensionID, err := sender.ExtensionId(callOpts)
	if err != nil {
		if !isUnsetExtensionID(err) {
			die("reading extensionId(): %s", err)
		}
		if !*setExtF {
			die("this contract has no extension id yet — setExtensionId() has never run.\n"+
				"  It binds %s to the extension registered by pre-build.sh, and is ONE-SHOT:\n"+
				"  the only way to change it afterwards is to redeploy the contract.\n"+
				"  If that address is the one you want, re-run with --set-extension-id.",
				contractAddr.Hex())
		}
		fmt.Println("No extension id set — calling setExtensionId() (one-shot) ...")
		// Bind to the id we registered. -extension-id pins it explicitly; otherwise
		// resolve it from the registry, which refuses to guess if this address is
		// registered under more than one extension (the signature of a duplicate registration).
		expectedID := new(big.Int)
		if *extIDF != "" {
			if _, ok := expectedID.SetString(*extIDF, 0); !ok {
				die("invalid -extension-id %q: expected a decimal or 0x id", *extIDF)
			}
		} else {
			resolved, rerr := instrutils.ResolveExtensionId(s, contractAddr)
			if rerr != nil {
				die("resolving extension id (pass -extension-id to pin it): %s", rerr)
			}
			expectedID = resolved
		}
		if err := instrutils.SetExtensionId(s, contractAddr, expectedID); err != nil {
			die("setExtensionId failed — is the extension registered (pre-build.sh)? %s", err)
		}
		if extensionID, err = sender.ExtensionId(callOpts); err != nil {
			die("reading extensionId() after setting it: %s", err)
		}
		fmt.Printf("Bound to extension %s\n", extensionID)
	}
	teeInfo, err := fccutils.TeeInfo(*pf)
	if err != nil {
		die("fetching %s/info — is the tunnel up? %s", *pf, err)
	}
	ecdsaPub, err := types.ParsePubKey(teeInfo.MachineData.PublicKey)
	if err != nil {
		die("parsing TEE public key: %s", err)
	}
	teeAddr := crypto.PubkeyToAddress(*ecdsaPub)
	if err := fccutils.VerifyTeeProduction(s.TeeMachineRegistry, teeAddr, extensionID); err != nil {
		die("TEE verification failed: %s", err)
	}
	fmt.Printf("TEE %s verified as PRODUCTION for extension %s\n", teeAddr.Hex(), extensionID)

	// --- Preflight ----------------------------------------------------------
	// Both are warnings, not errors: the proof is still worth fetching if you
	// only want the distance, and claimReward is what authoritatively decides.
	warnIfAlreadyClaimed(sender, callOpts, from)
	warnIfPoolEmpty(sender, callOpts, cc, contractAddr)

	// --- Seal the token -----------------------------------------------------
	// The grant binds the token to this caller, this contract, this chain and
	// the DISTANCE purpose, with an expiry. The TEE re-checks every binding, so
	// a grant lifted off the wire is useless to anyone else.
	expiry := time.Now().Add(time.Duration(*ttlF) * time.Second).Unix()
	grant, err := fccutils.GrantPlaintext(fccutils.PurposeDistance, from, contractAddr, s.ChainID, expiry, token)
	if err != nil {
		die("building the grant: %s", err)
	}
	encrypted, err := ecies.Encrypt(rand.Reader, &ecies.PublicKey{
		X: ecdsaPub.X, Y: ecdsaPub.Y,
		Curve:  ecies.DefaultCurve,
		Params: ecies.ECIES_AES128_SHA256,
	}, grant, nil, nil)
	if err != nil {
		die("ECIES encrypt: %s", err)
	}
	fmt.Printf("Sealed a %d-byte grant, expires %s\n\n",
		len(encrypted), time.Unix(expiry, 0).UTC().Format(time.RFC3339))

	// --- getDistanceProof ---------------------------------------------------
	fmt.Println("Sending getDistanceProof ...")
	instructionID, txHash, err := instrutils.GetDistanceProof(s, contractAddr, teeAddr, encrypted)
	if err != nil {
		die("%s", err)
	}
	fmt.Printf("  tx:            %s\n", txHash.Hex())
	fmt.Printf("  instructionId: %s\n\n", instructionID.Hex())

	// --- Collect the signed proof ------------------------------------------
	fmt.Println("Waiting for the TEE ...")
	resp, err := fetchProof(*pf, instructionID)
	if err != nil {
		die("%s", err)
	}

	// Parse every proxy-supplied field before anything is printed or sent. Doing
	// it here rather than at claim time holds -claim=false to the same bar — the
	// signature used to be printed and then never parsed at all on that path —
	// and lets the structured lines below be re-encoded from what parsed instead
	// of echoed from what arrived.
	proof, err := convertProof(&resp.DistanceProof)
	if err != nil {
		die("the proof does not validate, so it is not one this extension produced: %s", err)
	}

	fmt.Printf("\n  eligible:   %v\n", resp.Eligible)
	// Printed from the SIGNED distanceX1000, not from the response's distanceKm.
	// distanceKm is the one field of the result the TEE does not sign, so a
	// compromised proxy picks it freely — and this line sits next to a threshold
	// read from the contract, immediately before the operator decides whether to
	// spend gas. The number on screen has to be the number the chain acts on.
	fmt.Printf("  distance:   %.2f km (threshold %.2f km)\n",
		float64(proof.DistanceX1000.Int64())/1000, thresholdKm(sender, callOpts))
	fmt.Printf("  month:      %s\n", time.Unix(resp.MonthStart, 0).UTC().Format("2006-01"))
	// Free text from the proxy, which is attacker-influenced if it is compromised
	// or impersonated: escape it rather than writing raw control bytes to a
	// terminal that is about to ask whether to send a transaction.
	fmt.Printf("  message:    %s\n", fccutils.SanitizeForTerminal(resp.Message))
	fmt.Printf("  signature:  0x%s\n\n", hex.EncodeToString(proof.Signature))
	if *jsonF {
		raw, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Printf("%s\n\n", raw)
	}

	if !*claimF {
		fmt.Println("-claim=false — stopping before claimReward.")
		fmt.Println("The pending request stays open; cancelPendingProof() clears it.")
		return
	}

	// --- claimReward --------------------------------------------------------
	// Catches a bad proof as a static call, where the revert reason is readable,
	// instead of burning gas on a transaction that reverts on-chain.
	ok, err := sender.VerifyDistanceProof(callOpts, instructionID, *proof)
	if err != nil {
		die("verifyDistanceProof call failed: %s", err)
	}
	if !ok {
		die("the contract rejected this proof as inauthentic — refusing to submit it")
	}

	opts, err := bind.NewKeyedTransactorWithChainID(prv, s.ChainID)
	if err != nil {
		die("%s", err)
	}
	fmt.Println("Submitting claimReward ...")
	claimTx, err := sender.ClaimReward(opts, instructionID, *proof)
	if err != nil {
		reason := fccutils.DecodeRevertReason(err)
		if reason != "" {
			die("claimReward reverted: %s", reason)
		}
		die("claimReward failed: %s", err)
	}
	receipt, err := bind.WaitMined(context.Background(), cc, claimTx)
	if err != nil {
		die("waiting for claimReward: %s", err)
	}
	if receipt.Status != 1 {
		die("claimReward reverted on-chain. tx: %s", claimTx.Hash().Hex())
	}

	fmt.Printf("  tx: %s\n\n", claimTx.Hash().Hex())
	reportOutcome(sender, receipt.Logs)
}

// fetchProof polls the proxy and validates the response is complete and signed.
func fetchProof(proxyURL string, instructionID common.Hash) (*DistanceResponse, error) {
	// ActionResult already retries internally, so a miss here is a real failure
	// rather than a race with a TEE that has not finished yet.
	actionResponse, err := fccutils.ActionResult(proxyURL, instructionID)
	if err != nil {
		return nil, errors.Errorf("polling %s for the result: %s", proxyURL, err)
	}
	result := actionResponse.Result

	switch result.Status {
	case 0:
		return nil, errors.Errorf("the TEE failed to process the instruction: %s",
			fccutils.SanitizeForTerminal(result.Log))
	case 2:
		return nil, errors.New("still pending after polling — more than one active TEE machine will do this (see docs/deployment-steps.md)")
	}
	if len(result.Data) == 0 {
		return nil, errors.New("the TEE returned no data")
	}

	var resp DistanceResponse
	if err := json.Unmarshal(result.Data, &resp); err != nil {
		return nil, errors.Errorf("decoding the response: %s", err)
	}
	if resp.Signature == "" {
		return nil, errors.New("the response carries no signature, so there is nothing to claim with")
	}
	return &resp, nil
}

// convertProof maps the JSON proof onto the Solidity struct claimReward takes,
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
	signature, err := fccutils.StrictBytes("signature", p.Signature, maxSignatureLen)
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
		Signature:     signature,
	}, nil
}

// reportOutcome says which of the two terminal events the claim produced.
// A refusal is a successful transaction, so the receipt status cannot say it.
func reportOutcome(sender *strava.StravaInstructionSender, logs []*ethtypes.Log) {
	for _, l := range logs {
		if claimed, err := sender.ParseRewardClaimed(*l); err == nil {
			fmt.Printf("RewardClaimed — %s received 1 C2FLR for %.2f km\n",
				claimed.User.Hex(), float64(claimed.DistanceX1000.Int64())/1000)
			return
		}
		if refused, err := sender.ParseRewardRefused(*l); err == nil {
			fmt.Printf("RewardRefused — %.2f km was not enough, or this month was already claimed\n",
				float64(refused.DistanceX1000.Int64())/1000)
			return
		}
	}
	fmt.Println("Claim mined, but neither RewardClaimed nor RewardRefused was emitted.")
}

func warnIfAlreadyClaimed(sender *strava.StravaInstructionSender, opts *bind.CallOpts, from common.Address) {
	lastPaid, err := sender.LastPaidMonth(opts, from)
	if err != nil {
		return
	}
	monthStart, err := sender.CurrentMonthStart(opts)
	if err != nil {
		return
	}
	if lastPaid.Cmp(monthStart) == 0 {
		fmt.Printf("NOTE: %s already claimed this month — expect RewardRefused.\n", from.Hex())
	}
}

func warnIfPoolEmpty(sender *strava.StravaInstructionSender, opts *bind.CallOpts, cc *ethclient.Client, contractAddr common.Address) {
	reward, err := sender.REWARDAMOUNT(opts)
	if err != nil {
		return
	}
	balance, err := cc.BalanceAt(context.Background(), contractAddr, nil)
	if err != nil {
		return
	}
	if balance.Cmp(reward) < 0 {
		fmt.Printf("NOTE: the reward pool holds %s wei, below the %s wei reward —\n",
			balance, reward)
		fmt.Printf("      an eligible claim will revert with \"Insufficient reward pool balance.\"\n")
		fmt.Printf("      Top it up by sending C2FLR to %s.\n", contractAddr.Hex())
	}
}

func thresholdKm(sender *strava.StravaInstructionSender, opts *bind.CallOpts) float64 {
	t, err := sender.DISTANCETHRESHOLDX1000(opts)
	if err != nil {
		return 0
	}
	return float64(t.Int64()) / 1000
}

// isUnsetExtensionID distinguishes "the contract was never bootstrapped" from a
// genuine RPC or binding failure. Matched on the revert string because that is
// all a require() gives us — the contract declares no custom error for it.
func isUnsetExtensionID(err error) bool {
	return strings.Contains(err.Error(), "Extension ID is not set")
}

func parseKey(flagValue string) (*ecdsa.PrivateKey, error) {
	raw := flagValue
	if raw == "" {
		raw = os.Getenv("DEPLOYMENT_PRIVATE_KEY")
	}
	if raw == "" {
		return nil, errors.New("no private key: pass -key or set DEPLOYMENT_PRIVATE_KEY")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimPrefix(raw, "0x"), "0X"))
	if err != nil {
		return nil, errors.Errorf("parsing the private key: %s", err)
	}
	return key, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// warnSecretFlag flags a secret passed literally on the command line, where it is
// visible in the process list and shell history. envVar names the safer source.
func warnSecretFlag(flagName, value, envVar string) {
	if value != "" {
		fmt.Fprintf(os.Stderr,
			"WARNING: -%s passes a secret on the command line (visible in `ps` and shell history); prefer $%s\n",
			flagName, envVar)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\nERROR: "+format+"\n", args...)
	os.Exit(1)
}
