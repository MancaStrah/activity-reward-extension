package fccutils

import (
	"context"
	"crypto/ecdsa"
	"math/big"

	"activity-reward-extension/tools/pkg/support"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
	"github.com/flare-foundation/tee-node/pkg/types"
	"github.com/pkg/errors"
)

// SetTeeGovernance registers the TEE governance signer set + threshold for the
// extension via ExtensionGovernance.SetNewTeeGovernance.
//
// The node derives its governanceHash from the SAME (signers, threshold) pair
// (see governanceFromEnv in tee-node, fed by GOVERNANCE_SIGNERS /
// GOVERNANCE_THRESHOLD). MachineManager.register only accepts a TEE machine
// whose signed governanceHash matches the one registered here — so this MUST be
// kept consistent with the node's env. Both sides read the same two variables from
// .env, which is what guarantees they agree.
//
// Idempotent: if the latest on-chain governance hash already equals the desired
// one, the transaction is skipped.
func SetTeeGovernance(s *support.Support, privKey *ecdsa.PrivateKey, extensionId *big.Int, signers []common.Address, threshold uint64) error {
	// Validate before touching the chain: the registered set is what the contract
	// enforces from then on.
	if err := validateGovernanceSet(signers, threshold); err != nil {
		return err
	}

	desiredHash, err := types.GovernanceHash(signers, threshold)
	if err != nil {
		return errors.Errorf("computing governance hash: %s", err)
	}

	callOpts := &bind.CallOpts{Context: context.Background()}
	if current, err := s.TeeExtensionGovernance.GetLatestTeeGovernanceHash(callOpts, extensionId); err == nil {
		if common.Hash(current) == desiredHash {
			logger.Infof("TEE governance already set for extension %s (hash %s), skipping", extensionId.String(), desiredHash.Hex())
			return nil
		}
	}

	opts, err := bind.NewKeyedTransactorWithChainID(privKey, s.ChainID)
	if err != nil {
		return errors.Errorf("%s", err)
	}

	tx, err := s.TeeExtensionGovernance.SetNewTeeGovernance(opts, extensionId, signers, threshold)
	if err != nil {
		return errors.Errorf("SetNewTeeGovernance failed: %s", err)
	}

	if _, err := support.CheckTx(tx, s.ChainClient); err != nil {
		return errors.Errorf("%s", err)
	}

	logger.Infof("TEE governance set for extension %s: %d signer(s), threshold %d (hash %s)",
		extensionId.String(), len(signers), threshold, desiredHash.Hex())
	return nil
}

// validateGovernanceSet rejects signer sets that cannot enforce the quorum they
// describe. A caller may have built the set from configuration, so the checks
// live here rather than only at the parse site:
//
//   - the zero address has no key behind it, and ecrecover yields it for a
//     malformed signature — a set containing it turns an unverifiable signature
//     into an accepted one;
//   - a repeated signer raises the signer count without adding a party, so a
//     threshold of N can be met by fewer than N distinct key holders.
func validateGovernanceSet(signers []common.Address, threshold uint64) error {
	if len(signers) == 0 {
		return errors.New("at least one governance signer is required (set GOVERNANCE_SIGNERS)")
	}
	if threshold == 0 || threshold > uint64(len(signers)) {
		return errors.Errorf("invalid GOVERNANCE_THRESHOLD %d for %d signer(s)", threshold, len(signers))
	}

	seen := make(map[common.Address]struct{}, len(signers))
	for i, signer := range signers {
		if signer == (common.Address{}) {
			return errors.Errorf("governance signer %d is the zero address: no key can sign for it, and ecrecover returns it for a malformed signature", i)
		}
		if _, dup := seen[signer]; dup {
			return errors.Errorf("governance signer %s appears more than once: fewer than %d distinct parties could then reach the threshold", signer.Hex(), threshold)
		}
		seen[signer] = struct{}{}
	}
	return nil
}
