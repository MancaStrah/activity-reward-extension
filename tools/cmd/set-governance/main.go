// set-governance: register the TEE governance signer set + threshold for this
// extension on-chain.
//
// Reads the signer set from GOVERNANCE_SIGNERS (comma-separated 0x addresses)
// and the threshold from GOVERNANCE_THRESHOLD. Both default to "the deployer
// alone, threshold 1" when unset, so a developer who configures nothing still
// gets a working setup. These MUST match the same env vars passed to the TEE
// node (it derives its governanceHash from them), or register-tee fails with
// InvalidGovernanceHash.
package main

import (
	"crypto/ecdsa"
	"flag"
	"os"
	"strconv"
	"strings"

	"activity-reward-extension/tools/pkg/configs"
	"activity-reward-extension/tools/pkg/fccutils"
	"activity-reward-extension/tools/pkg/support"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
	"github.com/pkg/errors"
)

func main() {
	af := flag.String("a", configs.AddressesFile, "file with deployed addresses")
	cf := flag.String("c", configs.ChainNodeURL, "chain node url")
	pf := flag.String("p", configs.ExtensionProxyURL, "extension proxy url (used to query the extension id)")
	flag.Parse()

	// Resolve the env-supplied governance inputs first: these are the values that
	// end up on-chain, and once a set is registered it is what the contract
	// enforces. Failing before anything is dialled keeps a bad value cheap.
	ownerSigner, ownerSet, err := parseInitialOwner(os.Getenv("INITIAL_OWNER"))
	if err != nil {
		fccutils.FatalWithCause(err)
	}
	signers, err := parseSigners(os.Getenv("GOVERNANCE_SIGNERS"))
	if err != nil {
		fccutils.FatalWithCause(err)
	}
	threshold, err := parseThreshold(os.Getenv("GOVERNANCE_THRESHOLD"))
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	testSupport, err := support.DefaultSupport(*af, *cf)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	// Extension owner key (falls back to the deployment key, like the other tools).
	var privKey *ecdsa.PrivateKey
	if privKeyString := os.Getenv("EXTENSION_OWNER_KEY"); privKeyString != "" {
		privKeyString = strings.TrimPrefix(privKeyString, "0x")
		privKeyString = strings.TrimPrefix(privKeyString, "0X")
		privKey, err = crypto.HexToECDSA(privKeyString)
		if err != nil {
			fccutils.FatalWithCause(err)
		}
	} else {
		privKey = testSupport.Prv
	}
	deployer := crypto.PubkeyToAddress(privKey.PublicKey)

	// Default governance signer: INITIAL_OWNER if set (this is what the node's
	// compose env defaults to), else the deployer. Keeping both sides on the
	// same default is what makes the node's governanceHash match the chain.
	// Rejecting a malformed INITIAL_OWNER above does not change that default —
	// it only refuses the case where both sides would agree on an address
	// nobody holds a key for.
	if len(signers) == 0 {
		defaultSigner := deployer
		if ownerSet {
			defaultSigner = ownerSigner
		}
		signers = []common.Address{defaultSigner}
	}

	// Resolve the extension id from the proxy /info.
	teeInfo, err := fccutils.TeeInfo(*pf)
	if err != nil {
		fccutils.FatalWithCause(err)
	}
	extensionID := teeInfo.MachineData.ExtensionID.Big()

	logger.Infof("Extension ID:        %s", extensionID.String())
	logger.Infof("Governance signers:  %v", signers)
	logger.Infof("Governance threshold: %d", threshold)

	if err := fccutils.SetTeeGovernance(testSupport, privKey, extensionID, signers, threshold); err != nil {
		fccutils.FatalWithCause(err)
	}
}

// parseSigners parses a comma-separated list of 0x addresses. An empty value
// yields no signers, leaving the caller to apply the default signer.
//
// Every entry must parse as exactly 20 bytes of hex. Parsing leniently is not an
// option here: a truncated paste like "0xdeadbeef" would left-pad into a
// plausible-looking address and non-hex would become the zero address, so a
// mistyped or shell-mangled value would register a governance set nobody can
// sign for. The env var is named in the error so the operator knows which value
// to fix; the value itself is not echoed, since it is untrusted input on its way
// to a terminal.
func parseSigners(raw string) ([]common.Address, error) {
	var signers []common.Address
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		signer, err := fccutils.StrictAddress("GOVERNANCE_SIGNERS", part)
		if err != nil {
			return nil, err
		}
		if signer == (common.Address{}) {
			return nil, errors.New("GOVERNANCE_SIGNERS: the zero address is not a usable signer")
		}
		for _, seen := range signers {
			if seen == signer {
				// A parsed address is safe to print: it is 20 bytes of hex.
				return nil, errors.Errorf("GOVERNANCE_SIGNERS: %s is listed twice, which inflates the signer count without adding a party", signer.Hex())
			}
		}
		signers = append(signers, signer)
	}
	return signers, nil
}

// parseInitialOwner parses the optional INITIAL_OWNER override for the default
// signer. Unset means "no override"; set-but-unparsable is fatal rather than a
// silent fall-through to the deployer, because a wrong value here is written
// on-chain as the sole governance signer.
func parseInitialOwner(raw string) (common.Address, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return common.Address{}, false, nil
	}
	owner, err := fccutils.StrictAddress("INITIAL_OWNER", raw)
	if err != nil {
		return common.Address{}, false, err
	}
	if owner == (common.Address{}) {
		return common.Address{}, false, errors.New("INITIAL_OWNER: the zero address is not a usable signer")
	}
	return owner, true, nil
}

// parseThreshold parses GOVERNANCE_THRESHOLD, the number of signers that must
// sign. Unset or empty keeps the default of 1; anything else must be a clean
// positive decimal integer.
//
// Treating an unparsable value as 1 is not an option: this is the single input
// that decides the quorum, and every way of getting it wrong — a typo, a "2of3"
// shorthand, an inline comment a .env loader did not strip — would then register
// a 1-of-N set that the signer-count check downstream happily accepts, because
// 1 is a legal threshold for any set. The env var is named in the error and the
// value is not echoed, as with the two parses above.
func parseThreshold(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 1, nil
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		// strconv's error embeds the offending value; keep only the shape.
		return 0, errors.New("GOVERNANCE_THRESHOLD: expected a positive decimal integer, the number of GOVERNANCE_SIGNERS that must sign")
	}
	if v == 0 {
		return 0, errors.New("GOVERNANCE_THRESHOLD: a threshold of 0 is meaningless, it would require no signatures at all")
	}
	return v, nil
}
