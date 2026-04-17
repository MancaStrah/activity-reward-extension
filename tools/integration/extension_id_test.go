//go:build integration

package integration

import (
	"math/big"
	"strings"
	"testing"

	"activity-reward-extension/tools/pkg/fccutils"
	instrutils "activity-reward-extension/tools/pkg/utils"
)

// --- 2.2: setExtensionId error handling, including pre-registration capture ---

func TestSetExtensionId_NotRegistered(t *testing.T) {
	// Deploy a fresh InstructionSender but do NOT register it as an extension.
	addr, _ := deployFreshInstructionSender(t)

	// The flow resolves the id first; an unregistered contract has no id,
	// so resolution fails before any transaction is sent.
	_, err := instrutils.ResolveExtensionId(testSupport, addr)
	if err == nil {
		t.Fatal("expected ResolveExtensionId to fail on unregistered contract, but it succeeded")
	}

	t.Logf("Error: %v", err)
	if !strings.Contains(err.Error(), "no extension registers") {
		t.Errorf("expected 'no extension registers' error, got: %s", err.Error())
	}
}

func TestSetExtensionId_AlreadySet(t *testing.T) {
	// Deploy and register a fresh extension
	addr, _ := deployFreshInstructionSender(t)
	registerExtensionForSender(t, addr)

	id, err := instrutils.ResolveExtensionId(testSupport, addr)
	if err != nil {
		t.Fatalf("resolving extension id: %v", err)
	}

	// First call should succeed
	err = instrutils.SetExtensionId(testSupport, addr, id)
	if err != nil {
		t.Fatalf("first setExtensionId call failed: %v", err)
	}
	t.Log("First setExtensionId succeeded")

	// Second call should fail with "Extension ID already set."
	err = instrutils.SetExtensionId(testSupport, addr, id)
	if err == nil {
		t.Fatal("expected second setExtensionId to fail, but it succeeded")
	}

	t.Logf("Error on second call: %v", err)

	errMsg := err.Error()
	if strings.Contains(errMsg, "revert reason:") {
		if !strings.Contains(errMsg, "Extension ID already set") {
			t.Errorf("expected revert reason to mention 'Extension ID already set', got: %s", errMsg)
		}
	} else {
		t.Logf("Note: error does not contain 'revert reason:' — revert data may not be available")
		if !strings.Contains(errMsg, "failed to call setExtensionId") {
			t.Errorf("expected error to mention 'failed to call setExtensionId', got: %s", errMsg)
		}
	}
}

func TestSetExtensionId_RevertReasonDecoded(t *testing.T) {
	// This test specifically verifies the revert decoding chain works.
	// Deploy but do NOT register, then pass a public id the registry does not map to
	// this contract, so the setter reverts on the registry-binding check.
	addr, _ := deployFreshInstructionSender(t)

	bogusID := big.NewInt(0x10000) // FIRST_PUBLIC_EXTENSION_ID, not bound to this contract
	err := instrutils.SetExtensionId(testSupport, addr, bogusID)
	if err == nil {
		t.Fatal("expected setExtensionId to fail")
	}

	errMsg := err.Error()

	// The hardening in SetExtensionId tries:
	// 1. DecodeRevertReason(err) — from the estimation error
	// 2. SimulateAndDecodeRevert() — replays the call via eth_call
	// At least one of these should produce a human-readable reason.
	if !strings.Contains(errMsg, "Registry does not bind this id to this contract") {
		t.Errorf("expected error to contain decoded revert reason 'Registry does not bind this id to this contract', got: %s", errMsg)
		t.Log("This indicates the revert decoding chain is not working correctly.")
		t.Log("Check that DecodeRevertReason or SimulateAndDecodeRevert extracts the reason.")

		// Additional diagnostic: try DecodeRevertReason directly on a fresh error
		directReason := fccutils.DecodeRevertReason(err)
		t.Logf("Direct DecodeRevertReason result: %q", directReason)
	}
}
