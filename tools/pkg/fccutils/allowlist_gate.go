package fccutils

import (
	"fmt"
)

// AllowlistGate decides whether allow-tee-version may authorize a codeHash
// under the current deployment profile, and with what independent expectation.
//
// The tool reads the codeHash it authorizes from the SAME proxy /info it is
// supposed to be vetting — circular trust. On the simulated profiles (local,
// testnet-sim) that is acceptable: attestation is
// simulated there and the "codeHash" is a development constant, not a trust
// anchor. On a real Confidential Space deployment the operator MUST supply the
// expected value from their own build (the measured image digest), and the
// tool then only confirms the proxy agrees — it never promotes a
// proxy-supplied value on its own. See docs/production-allowlisting.md.
//
// teeProfile/localMode come from the TEE_PROFILE/LOCAL_MODE env vars;
// expectedCodeHash from the -expected-codehash flag.
func AllowlistGate(teeProfile, localMode, expectedCodeHash string) error {
	switch teeProfile {
	case "local", "testnet-sim":
		// Simulated attestation: /info carries the dev constant. Allowed.
		return nil
	case "confidential-space":
		if expectedCodeHash == "" {
			return fmt.Errorf(
				"TEE_PROFILE=confidential-space: refusing to allow-list a codeHash read from the proxy's own /info — " +
					"pass -expected-codehash with the measured image digest from YOUR build; see docs/production-allowlisting.md")
		}
		return nil
	case "":
		// Configs that do not set TEE_PROFILE at all: LOCAL_MODE=true (or unset)
		// selects the local dev flow.
		if localMode == "" || localMode == "true" {
			return nil
		}
		if expectedCodeHash != "" {
			return nil
		}
		return fmt.Errorf(
			"LOCAL_MODE=false with TEE_PROFILE unset: refusing to allow-list an unverified codeHash — " +
				"set TEE_PROFILE=testnet-sim for the simulated dev flow, or pass -expected-codehash from your own build; " +
				"see docs/production-allowlisting.md")
	default:
		return fmt.Errorf("unknown TEE_PROFILE %q (valid: local, testnet-sim, confidential-space)", teeProfile)
	}
}
