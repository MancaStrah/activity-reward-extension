// Command encrypt-token seals a Strava access token as a structured "grant" and
// ECIES-encrypts it with a TEE's public key, producing the _encryptedToken bytes
// for getDistanceProof().
//
// The grant binds the ciphertext to a single (user, contract, chain, purpose)
// tuple with an expiry, so a ciphertext copied from another user's public
// transaction cannot be reused (the TEE rejects it). The caller must send the
// instruction from the SAME wallet passed as -caller, to the SAME contract passed
// as -contract, on the SAME chain.
//
// Before using the key, the tool (unless -insecure) (a) requires the proxy URL to
// be HTTPS or loopback so the key cannot be swapped in transit, and (b) verifies
// on-chain that the key belongs to a registered, attested PRODUCTION TEE.
//
// It prints the ciphertext and the values it is bound to, but deliberately not a
// ready-to-paste `cast send`, same as cmd/get-result: a pasted command skips the
// on-chain pre-verification the real path performs, and a shell command built out
// of values that arrived over the wire is one quote away from executing on the
// operator's machine. Use ./scripts/claim-reward.sh (or cmd/claim-reward), which
// sends the instruction itself.
//
// Usage:
//
//	STRAVA_TOKEN=... go run ./cmd/encrypt-token \
//	  -p https://<proxy-url> -caller 0x<wallet> -contract 0x<instructionSender> \
//	  -c <rpc-url> -reg <teeMachineRegistry> [-ttl 900]
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"activity-reward-extension/tools/pkg/configs"
	"activity-reward-extension/tools/pkg/contracts/strava"
	"activity-reward-extension/tools/pkg/fccutils"
	"activity-reward-extension/tools/pkg/support"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/ecies"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/flare-foundation/go-flare-common/pkg/contracts/tee/machinemanager"
	"github.com/flare-foundation/tee-node/pkg/types"
)

func main() {
	pf := flag.String("p", configs.ExtensionProxyURL, "extension proxy url (source of the TEE public key)")
	tokenF := flag.String("token", "", "token to encrypt (default: STRAVA_TOKEN env var)")
	callerF := flag.String("caller", "", "wallet address that will send the instruction (must equal msg.sender)")
	contractF := flag.String("contract", "", "StravaInstructionSender address the instruction will be sent to")
	ttlF := flag.Int64("ttl", 900, "grant time-to-live in seconds (expiry = now + ttl)")
	chainF := flag.String("c", configs.ChainNodeURL, "chain rpc url (for chainId and on-chain TEE verification)")
	chainIdF := flag.Int64("chainid", 0, "chain id override (0 = read from the rpc url)")
	regF := flag.String("reg", "", "TeeMachineRegistry (FlareTeeManager diamond) address, for on-chain TEE verification")
	insecure := flag.Bool("insecure", false, "skip HTTPS and on-chain TEE verification (LOCAL DEV ONLY)")
	flag.Parse()

	// A secret on the command line is visible in `ps` and lands in shell history;
	// $STRAVA_TOKEN is the safe default.
	if *tokenF != "" {
		fmt.Fprintln(os.Stderr, "WARNING: -token passes a secret on the command line (visible in `ps` and shell history); prefer $STRAVA_TOKEN")
	}

	token := *tokenF
	if token == "" {
		token = os.Getenv("STRAVA_TOKEN")
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "no token: set STRAVA_TOKEN or pass -token")
		os.Exit(1)
	}

	// Parsed exactly. IsHexAddress checks the shape but not the EIP-55 checksum,
	// and inside 40 valid hex digits a transposition or one mistyped character is
	// still 40 valid hex digits — a real address belonging to nobody. The grant
	// below is sealed to whatever these say, so a wrong value produces a
	// ciphertext bound to an address the user does not control: the TEE refuses
	// it, and the instruction fee is spent for nothing.
	caller, err := fccutils.StrictAddress("-caller", *callerF)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n  Pass the wallet address that will send the instruction (msg.sender).\n", err)
		os.Exit(1)
	}

	contractAddr, err := fccutils.StrictAddress("-contract", *contractF)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n  Pass the StravaInstructionSender address.\n", err)
		os.Exit(1)
	}

	// --- Do not encrypt to an unverified key ---
	var registryAddr common.Address
	if !*insecure {
		if err := fccutils.RequireSecureProxyURL(*pf); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if err := fccutils.RequireSecureChainURL(*chainF); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		registryAddr, err = fccutils.StrictAddress("-reg", *regF)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n  The TeeMachineRegistry address is required to verify the TEE on-chain (or pass -insecure for local dev).\n", err)
			os.Exit(1)
		}
	}

	// Dial the RPC once, if we need it for the chainId or the on-chain verification.
	var chainID *big.Int
	if *chainIdF > 0 {
		chainID = big.NewInt(*chainIdF)
	}
	var cc *ethclient.Client
	if !*insecure || chainID == nil {
		var err error
		cc, err = support.DialBounded(*chainF)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dial chain rpc %s: %v\n", *chainF, err)
			os.Exit(1)
		}
		defer cc.Close()
		if chainID == nil {
			chainID, err = cc.ChainID(context.Background())
			if err != nil {
				fmt.Fprintf(os.Stderr, "reading chain id from %s (pass -chainid to override): %v\n", *chainF, err)
				os.Exit(1)
			}
		}
	}

	teeInfo, err := fccutils.TeeInfo(*pf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch TEE info from %s: %v\n", *pf, err)
		os.Exit(1)
	}

	ecdsaPub, err := types.ParsePubKey(teeInfo.MachineData.PublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse TEE public key: %v\n", err)
		os.Exit(1)
	}
	teeAddr := crypto.PubkeyToAddress(*ecdsaPub)

	if !*insecure {
		mm, err := machinemanager.NewMachineManager(registryAddr, cc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bind TeeMachineRegistry %s: %v\n", registryAddr.Hex(), err)
			os.Exit(1)
		}
		// Anchor the expected extension on the CONTRACT we are about to instruct,
		// not on the proxy's self-reported /info value — otherwise a hostile proxy
		// could point us at a machine registered under its own extension.
		sender, err := strava.NewStravaInstructionSender(contractAddr, cc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bind InstructionSender %s: %v\n", contractAddr.Hex(), err)
			os.Exit(1)
		}
		expectedExt, err := sender.ExtensionId(&bind.CallOpts{Context: context.Background()})
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading extensionId() from %s (has setExtensionId() been called?): %v\n",
				contractAddr.Hex(), err)
			os.Exit(1)
		}
		if err := fccutils.VerifyTeeProduction(mm, teeAddr, expectedExt); err != nil {
			fmt.Fprintf(os.Stderr, "TEE verification failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Verified TEE %s is a PRODUCTION machine of extension %s.\n", teeAddr.Hex(), expectedExt.String())
	} else {
		fmt.Fprintln(os.Stderr, "WARNING: -insecure set — skipping HTTPS and on-chain TEE verification (do NOT use with a real token)")
	}

	// Same ECIES parameters the TEE node uses on /decrypt.
	eciesPub := &ecies.PublicKey{
		X:      ecdsaPub.X,
		Y:      ecdsaPub.Y,
		Curve:  ecies.DefaultCurve,
		Params: ecies.ECIES_AES128_SHA256,
	}

	// --- Seal the token in a caller/contract/chain/purpose-bound, expiring grant,
	// then encrypt it to the TEE. ---
	// The TEE caps the grant lifetime (config.MaxGrantTTL). Reject an over-long ttl
	// here so the user gets a clear message instead of an opaque rejection later.
	const maxTTLSeconds = int64(24 * 60 * 60)
	if *ttlF <= 0 || *ttlF > maxTTLSeconds {
		fmt.Fprintf(os.Stderr, "invalid -ttl %d: must be between 1 and %d seconds\n", *ttlF, maxTTLSeconds)
		os.Exit(1)
	}
	expiry := time.Now().Add(time.Duration(*ttlF) * time.Second).Unix()
	plaintext, err := fccutils.GrantPlaintext(fccutils.PurposeDistance, caller, contractAddr, chainID, expiry, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "building grant: %v\n", err)
		os.Exit(1)
	}

	encrypted, err := ecies.Encrypt(rand.Reader, eciesPub, plaintext, nil, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ECIES encrypt: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("TEE (proxy /info):  %s\n", *pf)
	fmt.Printf("teeId:              %s\n", teeAddr.Hex())
	fmt.Printf("caller (bound):     %s\n", caller.Hex())
	fmt.Printf("contract (bound):   %s\n", contractAddr.Hex())
	fmt.Printf("chainId (bound):    %s\n", chainID.String())
	fmt.Printf("expires:            %s\n", time.Unix(expiry, 0).UTC().Format(time.RFC3339))
	fmt.Printf("encryptedToken:     %s\n", hexutil.Encode(encrypted))
	fmt.Println()
	fmt.Printf("Send it from the %s wallet before expiry, attaching the instruction fee.\n", caller.Hex())
	fmt.Println("Use ./scripts/claim-reward.sh (or cmd/claim-reward) to submit it: that path runs")
	fmt.Println("the same checks, sends getDistanceProof itself, then polls for the proof and claims.")
}
