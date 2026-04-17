//go:build integration

package integration

import (
	"testing"

	"activity-reward-extension/tools/pkg/fccutils"
	instrutils "activity-reward-extension/tools/pkg/utils"

	"github.com/ethereum/go-ethereum/common"
)

// --- 2.5: Extension Registration ---

func TestSetupExtension_FirstTime(t *testing.T) {
	addr, _ := deployFreshInstructionSender(t)

	governanceHash := common.Hash{}
	extID, err := fccutils.SetupExtension(testSupport, governanceHash, addr, common.Address{})
	if err != nil {
		t.Fatalf("SetupExtension failed on first run: %v", err)
	}

	if extID == nil {
		t.Fatal("expected non-nil extension ID")
	}
	t.Logf("Extension registered with ID: %s", extID.String())
}

// The registry ALLOWS registering the same InstructionSender under multiple
// extension ids — that permissiveness is exactly what makes a
// pre-registration attack possible. The safety property therefore lives in the
// tools: ResolveExtensionId must refuse to guess when an address appears under
// more than one id, so an operator can never be steered onto an attacker's
// duplicate registration.
func TestSetupExtension_DuplicateSenderIsAllowedButUnresolvable(t *testing.T) {
	addr, _ := deployFreshInstructionSender(t)

	governanceHash := common.Hash{}

	extID1, err := fccutils.SetupExtension(testSupport, governanceHash, addr, common.Address{})
	if err != nil {
		t.Fatalf("first SetupExtension failed: %v", err)
	}
	t.Logf("First registration: ID=%s", extID1.String())

	extID2, err := fccutils.SetupExtension(testSupport, governanceHash, addr, common.Address{})
	if err != nil {
		t.Fatalf("second SetupExtension failed — the registry is documented to allow duplicates: %v", err)
	}
	if extID1.Cmp(extID2) == 0 {
		t.Fatalf("expected two distinct extension ids, both registrations returned %s", extID1.String())
	}
	t.Logf("Second registration: ID=%s (registry allows duplicates)", extID2.String())

	// With the address ambiguous, the resolver must fail closed instead of
	// picking one of the ids.
	if _, err := instrutils.ResolveExtensionId(testSupport, addr); err == nil {
		t.Fatal("ResolveExtensionId picked an id for an address registered under multiple extensions — it must refuse to guess")
	} else {
		t.Logf("ResolveExtensionId correctly refused: %v", err)
	}
}
