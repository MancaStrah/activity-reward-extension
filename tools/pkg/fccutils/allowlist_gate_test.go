package fccutils

import (
	"strings"
	"testing"
)

// Negative matrix for the allow-tee-version gate: it must never authorize a
// codeHash it only knows from the proxy's own /info on a real deployment.
func TestAllowlistGate(t *testing.T) {
	cases := []struct {
		name             string
		profile          string
		localMode        string
		expectedCodeHash string
		wantErr          bool
	}{
		{"local profile", "local", "true", "", false},
		{"testnet-sim profile", "testnet-sim", "false", "", false},
		{"confidential-space without expectation is refused", "confidential-space", "false", "", true},
		{"confidential-space with operator expectation", "confidential-space", "false", "0xabc", false},
		{"legacy local default (no profile, no LOCAL_MODE)", "", "", "", false},
		{"legacy local mode", "", "true", "", false},
		{"legacy non-local without expectation is refused", "", "false", "", true},
		{"legacy non-local with operator expectation", "", "false", "0xabc", false},
		{"unknown profile is refused", "prod", "false", "0xabc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AllowlistGate(tc.profile, tc.localMode, tc.expectedCodeHash)
			if (err != nil) != tc.wantErr {
				t.Fatalf("AllowlistGate(%q, %q, %q) = %v, wantErr=%v",
					tc.profile, tc.localMode, tc.expectedCodeHash, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "refusing to allow-list") && !strings.Contains(err.Error(), "TEE_PROFILE") {
				t.Errorf("gate error should point at the cause and the fix, got: %v", err)
			}
		})
	}
}
