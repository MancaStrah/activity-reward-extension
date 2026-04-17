package fccutils

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// signerN builds a distinct, non-zero test address.
func signerN(n byte) common.Address {
	var a common.Address
	a[common.AddressLength-1] = n
	return a
}

// A governance set is only as strong as the number of distinct keys that can
// reach its threshold, so the set must be rejected whenever that count is lower
// than it looks: the zero address (no key, and what ecrecover returns for a
// malformed signature) and repeated signers both break the property.
func TestValidateGovernanceSet(t *testing.T) {
	zero := common.Address{}

	cases := []struct {
		name      string
		signers   []common.Address
		threshold uint64
		wantErr   string // substring; empty means the set must be accepted
	}{
		{
			name:      "zero address as sole signer",
			signers:   []common.Address{zero},
			threshold: 1,
			wantErr:   "zero address",
		},
		{
			name:      "zero address among valid signers",
			signers:   []common.Address{signerN(1), zero, signerN(2)},
			threshold: 2,
			wantErr:   "zero address",
		},
		{
			name:      "duplicate signer",
			signers:   []common.Address{signerN(1), signerN(2), signerN(1)},
			threshold: 3,
			wantErr:   "appears more than once",
		},
		{
			name:      "duplicate signer below threshold",
			signers:   []common.Address{signerN(1), signerN(1)},
			threshold: 1,
			wantErr:   "appears more than once",
		},
		{
			name:      "no signers",
			signers:   nil,
			threshold: 1,
			wantErr:   "at least one governance signer",
		},
		{
			name:      "threshold zero",
			signers:   []common.Address{signerN(1)},
			threshold: 0,
			wantErr:   "GOVERNANCE_THRESHOLD",
		},
		{
			name:      "threshold above signer count",
			signers:   []common.Address{signerN(1), signerN(2)},
			threshold: 3,
			wantErr:   "GOVERNANCE_THRESHOLD",
		},
		{
			name:      "single signer threshold one",
			signers:   []common.Address{signerN(1)},
			threshold: 1,
		},
		{
			name:      "three signers threshold two",
			signers:   []common.Address{signerN(1), signerN(2), signerN(3)},
			threshold: 2,
		},
		{
			name:      "three signers unanimous",
			signers:   []common.Address{signerN(1), signerN(2), signerN(3)},
			threshold: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGovernanceSet(tc.signers, tc.threshold)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("valid set rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("invalid set accepted (%d signer(s), threshold %d)", len(tc.signers), tc.threshold)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// The validation has to run before anything reaches the chain. A nil Support
// would panic the instant it were dereferenced, so an invalid set coming back as
// a plain error is proof that nothing was queried or sent. Cases are looped here
// instead of run as subtests so the deferred recover stays on the same
// goroutine as the calls.
func TestSetTeeGovernanceValidatesBeforeAnyChainCall(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SetTeeGovernance touched the chain before validating the set: %v", r)
		}
	}()

	invalid := []struct {
		name      string
		signers   []common.Address
		threshold uint64
	}{
		{"zero address", []common.Address{{}}, 1},
		{"duplicate signer", []common.Address{signerN(1), signerN(1)}, 2},
		{"no signers", nil, 1},
		{"threshold zero", []common.Address{signerN(1)}, 0},
		{"threshold above signer count", []common.Address{signerN(1)}, 2},
	}

	for _, tc := range invalid {
		if err := SetTeeGovernance(nil, nil, big.NewInt(1), tc.signers, tc.threshold); err == nil {
			t.Errorf("%s: SetTeeGovernance accepted the set", tc.name)
		}
	}
}
