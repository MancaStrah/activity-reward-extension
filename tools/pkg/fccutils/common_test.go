package fccutils

import (
	"strings"
	"testing"

	"github.com/flare-foundation/tee-node/pkg/attestation"
	"github.com/flare-foundation/tee-node/pkg/types"
)

// magicPassInfo is what a MODE=1 node reports: the magic_pass sentinel in place
// of a Confidential Space JWT, with the simulated codeHash/platform constants in
// MachineData.
func magicPassInfo() *types.SignedTeeInfoResponse {
	info := &types.SignedTeeInfoResponse{}
	info.Attestation = attestation.MagicPass
	info.MachineData.CodeHash = TeeCodeHash
	info.MachineData.Platform = TestPlatform
	return info
}

// Pins the SIMULATED_TEE branch of GetCodeHashAndPlatform: register-tee calls
// this over the proxy's /info, so a MODE=1 deployment that leaves
// SIMULATED_TEE=false feeds "magic_pass" to the JWT parser and aborts
// registration. scripts/lib/profile.sh rejects that matrix before any
// container starts; this test is the reason it has to.
func TestGetCodeHashAndPlatformOverMagicPass(t *testing.T) {
	t.Run("SIMULATED_TEE=true accepts the sentinel", func(t *testing.T) {
		t.Setenv("SIMULATED_TEE", "true")

		codeHash, platform, err := GetCodeHashAndPlatform(magicPassInfo())
		if err != nil {
			t.Fatalf("simulated mode must accept the magic_pass sentinel, got: %v", err)
		}
		if codeHash != TeeCodeHash {
			t.Errorf("codeHash = %s, want the simulated constant %s", codeHash, TeeCodeHash)
		}
		if platform != TestPlatform {
			t.Errorf("platform = %s, want the test constant %s", platform, TestPlatform)
		}
	})

	t.Run("SIMULATED_TEE=false rejects the sentinel", func(t *testing.T) {
		t.Setenv("SIMULATED_TEE", "false")

		if _, _, err := GetCodeHashAndPlatform(magicPassInfo()); err == nil {
			t.Fatal("real-attestation mode must not accept magic_pass as a JWT")
		}
	})

	// Unset must behave like the simulated path, since post-build.sh and
	// profile.sh both default SIMULATED_TEE to true.
	t.Run("unset defaults to real attestation", func(t *testing.T) {
		t.Setenv("SIMULATED_TEE", "")

		if _, _, err := GetCodeHashAndPlatform(magicPassInfo()); err == nil {
			t.Fatal("only the literal \"true\" selects the simulated path")
		}
	})

	// A simulated node whose MachineData disagrees with the constants is still
	// refused — the branch skips JWT parsing, not the consistency check.
	t.Run("simulated mode still cross-checks MachineData", func(t *testing.T) {
		t.Setenv("SIMULATED_TEE", "true")

		info := magicPassInfo()
		info.MachineData.CodeHash = TeeCodeHash
		info.MachineData.Platform = PlatformIntel

		_, _, err := GetCodeHashAndPlatform(info)
		if err == nil {
			t.Fatal("mismatched platform must be rejected even in simulated mode")
		}
		if !strings.Contains(err.Error(), "platform") {
			t.Errorf("error should name the mismatched field, got: %v", err)
		}
	})
}
