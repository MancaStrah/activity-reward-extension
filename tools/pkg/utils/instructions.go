package utils

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"activity-reward-extension/tools/pkg/contracts/strava"
	"activity-reward-extension/tools/pkg/fccutils"
	"activity-reward-extension/tools/pkg/support"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pkg/errors"
)

func DeployInstructionSender(s *support.Support) (common.Address, *strava.StravaInstructionSender, error) {
	opts, err := bind.NewKeyedTransactorWithChainID(s.Prv, s.ChainID)
	if err != nil {
		return common.Address{}, nil, errors.Errorf("failed to create transactor: %s", err)
	}

	// Both registry args are the FlareTeeManager diamond proxy: the diamond
	// routes ExtensionManager and MachineManager calls to the right facets.
	address, tx, contract, err := strava.DeployStravaInstructionSender(
		opts, s.ChainClient, s.Addresses.FlareTeeManager, s.Addresses.FlareTeeManager,
	)
	if err != nil {
		return common.Address{}, nil, errors.Errorf("failed to deploy contract: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	receipt, err := bind.WaitMined(ctx, s.ChainClient, tx)
	if err != nil {
		return common.Address{}, nil, errors.Errorf("deployment tx not mined within 2 minutes (tx: %s): %s", tx.Hash().Hex(), err)
	}

	if receipt.Status != types.ReceiptStatusSuccessful {
		return common.Address{}, nil, errors.New("contract deployment failed")
	}

	return address, contract, nil
}

// firstPublicExtensionID mirrors FIRST_PUBLIC_EXTENSION_ID in
// contracts/InstructionSender.sol: the registry reserves ids below this for
// system extensions, so public extensions start here.
const firstPublicExtensionID = 0x10000 // 65536

// ExtensionRegistry is the slice of the generated ExtensionManager binding that the
// id scan needs.
//
// It exists for testability, and that is not incidental bookkeeping: the scan's error
// handling IS the security property. "Refuses to guess when the answer is ambiguous"
// is only true if an id that could not be READ is distinguished from an id that did
// not MATCH, and with the concrete generated binding there was no seam to inject a
// failing call through — so that distinction was unverifiable, and it was wrong.
type ExtensionRegistry interface {
	NextPublicExtensionId(opts *bind.CallOpts) (*big.Int, error)
	GetTeeExtensionInstructionsSender(opts *bind.CallOpts, extensionID *big.Int) (common.Address, error)
}

const (
	// resolveScanBudget bounds the whole scan and resolveCallTimeout bounds one
	// call. Both are needed: without a per-call bound a single hung request stalls
	// the scan, and without an overall bound a large registry on a slow RPC never
	// finishes. Running out of either is reported, never absorbed — see below.
	resolveScanBudget  = 2 * time.Minute
	resolveCallTimeout = 15 * time.Second
)

// ResolveExtensionId scans the registry for the public extension id whose
// instruction sender is instructionSenderAddress. It refuses to guess when the
// answer is ambiguous: zero matches means the extension was never registered, and
// more than one match is the signature of a duplicate pre-registration — in
// that case the operator must pass the id they registered explicitly rather
// than let a tool pick the wrong (attacker's) one.
func ResolveExtensionId(s *support.Support, instructionSenderAddress common.Address) (*big.Int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resolveScanBudget)
	defer cancel()
	return ResolveExtensionIDWithRegistry(ctx, s.TeeExtensionRegistry, instructionSenderAddress)
}

// ResolveExtensionIDWithRegistry is ResolveExtensionId against an explicit registry
// and context, so the scan — including every way it can fail to complete — is
// testable without a chain.
func ResolveExtensionIDWithRegistry(ctx context.Context, reg ExtensionRegistry, instructionSenderAddress common.Address) (*big.Int, error) {
	call := func(fn func(*bind.CallOpts) error) error {
		callCtx, cancel := context.WithTimeout(ctx, resolveCallTimeout)
		defer cancel()
		return fn(&bind.CallOpts{Context: callCtx})
	}

	var count *big.Int
	if err := call(func(opts *bind.CallOpts) error {
		var err error
		count, err = reg.NextPublicExtensionId(opts)
		return err
	}); err != nil {
		return nil, errors.Errorf("reading nextPublicExtensionId: %s", err)
	}
	if count == nil {
		return nil, errors.New("the registry reported no nextPublicExtensionId")
	}
	last := count.Int64()

	var matches []*big.Int
	for i := int64(firstPublicExtensionID); i < last; i++ {
		// Checked explicitly so that running out of budget is reported as "the scan
		// did not finish" rather than silently becoming "nothing else matched".
		if err := ctx.Err(); err != nil {
			// "ids 65536..65535" would be a correct rendering of an empty range and a
			// confusing one, and this message exists to be read.
			read := fmt.Sprintf("ids %d..%d of %d were read", int64(firstPublicExtensionID), i-1, last-1)
			if i == int64(firstPublicExtensionID) {
				read = fmt.Sprintf("no ids were read of the %d..%d range", int64(firstPublicExtensionID), last-1)
			}
			return nil, errors.Errorf(
				"the extension scan did not finish (%s): %s, so a second registration of %s may exist "+
					"among the ids that were not — retry, or pass the id you registered explicitly "+
					"instead of letting the tool choose",
				err, read, instructionSenderAddress.Hex())
		}

		id := big.NewInt(i)
		var sender common.Address
		if err := call(func(opts *bind.CallOpts) error {
			var err error
			sender, err = reg.GetTeeExtensionInstructionsSender(opts, id)
			return err
		}); err != nil {
			// An id that could not be read is NOT an id that did not match. Skipping it
			// would let one transient RPC error turn "ambiguous, refuse" into "unique,
			// proceed" — and the caller feeds the result straight into setExtensionId,
			// which is owner-only and one-shot, so the only remedy is redeploying the
			// contract. The registry permits a duplicate registration, and a duplicate
			// satisfies the contract's own expectedId check for BOTH ids, which is
			// exactly why this refusal has to live here.
			return nil, errors.Errorf(
				"reading the instruction sender of extension %d: %s — refusing to call an id "+
					"unambiguous when part of the registry could not be read; retry, or pass the id "+
					"you registered explicitly", i, err)
		}
		if sender == instructionSenderAddress {
			matches = append(matches, id)
		}
	}

	switch len(matches) {
	case 0:
		return nil, errors.Errorf(
			"no extension registers %s as its instruction sender — run pre-build.sh (register step) first",
			instructionSenderAddress.Hex())
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.String()
		}
		return nil, errors.Errorf(
			"%s is registered under multiple extensions (%s) — possible pre-registration; "+
				"pass the id you registered explicitly instead of letting the tool choose",
			instructionSenderAddress.Hex(), strings.Join(ids, ", "))
	}
}

// CheckExtensionId verifies the registry maps id to instructionSenderAddress with a
// single call — the operator-supplied alternative to ResolveExtensionId's full scan.
// An explicit id preserves the same guarantee: a duplicate registration under a
// different id cannot be picked by mistake, because the id never comes from a guess.
func CheckExtensionId(s *support.Support, instructionSenderAddress common.Address, id *big.Int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts := &bind.CallOpts{Context: ctx}
	sender, err := s.TeeExtensionRegistry.GetTeeExtensionInstructionsSender(opts, id)
	if err != nil {
		return errors.Errorf("reading instruction sender for extension %s: %s", id.String(), err)
	}
	if sender != instructionSenderAddress {
		return errors.Errorf(
			"extension %s registers %s as its instruction sender, not %s — check EXTENSION_ID and INSTRUCTION_SENDER in config/extension.env",
			id.String(), sender.Hex(), instructionSenderAddress.Hex())
	}
	return nil
}

// SetExtensionId binds the contract to expectedId. The contract (owner-only) rejects
// the call unless the registry maps expectedId to this contract, so expectedId must
// be the id the caller actually registered — obtain it from the register step's
// output or ResolveExtensionId, never a blind on-chain guess.
func SetExtensionId(s *support.Support, instructionSenderAddress common.Address, expectedId *big.Int) error {
	sender, err := strava.NewStravaInstructionSender(instructionSenderAddress, s.ChainClient)
	if err != nil {
		return errors.Errorf("failed to bind contract: %s", err)
	}

	opts, err := bind.NewKeyedTransactorWithChainID(s.Prv, s.ChainID)
	if err != nil {
		return errors.Errorf("failed to create transactor: %s", err)
	}

	tx, err := sender.SetExtensionId(opts, expectedId)
	if err != nil {
		reason := fccutils.DecodeRevertReason(err)
		if reason == "" {
			parsed, _ := strava.StravaInstructionSenderMetaData.GetAbi()
			if parsed != nil {
				callData, packErr := parsed.Pack("setExtensionId", expectedId)
				if packErr == nil {
					from := crypto.PubkeyToAddress(s.Prv.PublicKey)
					reason = fccutils.SimulateAndDecodeRevert(
						s.ChainClient, from, instructionSenderAddress, nil, callData,
					)
				}
			}
		}
		if reason != "" {
			return errors.Errorf("failed to call setExtensionId: %s (revert reason: %s)", err, reason)
		}
		return errors.Errorf("failed to call setExtensionId: %s", err)
	}

	mineCtx, cancelMine := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelMine()
	receipt, err := bind.WaitMined(mineCtx, s.ChainClient, tx)
	if err != nil {
		return errors.Errorf("failed waiting for transaction: %s", err)
	}

	if receipt.Status != types.ReceiptStatusSuccessful {
		parsed, _ := strava.StravaInstructionSenderMetaData.GetAbi()
		if parsed != nil {
			callData, packErr := parsed.Pack("setExtensionId", expectedId)
			if packErr == nil {
				from := crypto.PubkeyToAddress(s.Prv.PublicKey)
				reason := fccutils.SimulateAndDecodeRevert(
					s.ChainClient, from, instructionSenderAddress, nil, callData,
				)
				if reason != "" {
					return errors.Errorf("setExtensionId transaction failed (revert reason: %s)", reason)
				}
			}
		}
		return errors.New("setExtensionId transaction failed")
	}

	return nil
}

// GetDistanceProof sends the DISTANCE instruction via the contract and returns the
// instruction id and tx hash.
func GetDistanceProof(s *support.Support, instructionSenderAddress common.Address, teeId common.Address, encryptedToken []byte) (common.Hash, common.Hash, error) {
	sender, err := strava.NewStravaInstructionSender(instructionSenderAddress, s.ChainClient)
	if err != nil {
		return common.Hash{}, common.Hash{}, errors.Errorf("failed to bind contract: %s", err)
	}

	opts, err := bind.NewKeyedTransactorWithChainID(s.Prv, s.ChainID)
	if err != nil {
		return common.Hash{}, common.Hash{}, errors.Errorf("failed to create transactor: %s", err)
	}
	opts.Value = big.NewInt(1000000) // Instruction fee in wei — must match registry's required fee

	tx, err := sender.GetDistanceProof(opts, teeId, encryptedToken)
	if err != nil {
		reason := fccutils.DecodeRevertReason(err)
		if reason == "" {
			parsed, _ := strava.StravaInstructionSenderMetaData.GetAbi()
			if parsed != nil {
				callData, packErr := parsed.Pack("getDistanceProof", teeId, encryptedToken)
				if packErr == nil {
					from := crypto.PubkeyToAddress(s.Prv.PublicKey)
					reason = fccutils.SimulateAndDecodeRevert(
						s.ChainClient, from, instructionSenderAddress,
						big.NewInt(1000000), callData,
					)
				}
			}
		}
		if reason != "" {
			return common.Hash{}, common.Hash{}, errors.Errorf("failed to send instruction: %s (revert reason: %s)", err, reason)
		}
		return common.Hash{}, common.Hash{}, errors.Errorf("failed to send instruction: %s", err)
	}

	mineCtx, cancelMine := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelMine()
	receipt, err := bind.WaitMined(mineCtx, s.ChainClient, tx)
	if err != nil {
		return common.Hash{}, common.Hash{}, errors.Errorf("failed waiting for transaction: %s", err)
	}

	if receipt.Status != 1 {
		parsed, _ := strava.StravaInstructionSenderMetaData.GetAbi()
		if parsed != nil {
			callData, packErr := parsed.Pack("getDistanceProof", teeId, encryptedToken)
			if packErr == nil {
				from := crypto.PubkeyToAddress(s.Prv.PublicKey)
				reason := fccutils.SimulateAndDecodeRevert(
					s.ChainClient, from, instructionSenderAddress,
					big.NewInt(1000000), callData,
				)
				if reason != "" {
					return common.Hash{}, common.Hash{}, errors.Errorf("transaction failed with status %d (revert reason: %s)", receipt.Status, reason)
				}
			}
		}
		return common.Hash{}, common.Hash{}, errors.Errorf("transaction failed with status: %d", receipt.Status)
	}

	if len(receipt.Logs) == 0 {
		return common.Hash{}, common.Hash{}, errors.New("no logs found in receipt")
	}

	instructionSent, err := s.TeeVerification.ParseTeeInstructionsSent(*receipt.Logs[0])
	if err != nil {
		return common.Hash{}, common.Hash{}, errors.Errorf("failed to parse TeeInstructionsSent event: %s", err)
	}

	return instructionSent.InstructionId, receipt.TxHash, nil
}
