package fccutils

import "testing"

func TestRequireSecureProxyURL(t *testing.T) {
	ok := []string{
		"https://proxy.example.com",
		"https://proxy.example.com:6664/info",
		"http://localhost:6664",
		"http://127.0.0.1:6664",
		"http://[::1]:6664",
	}
	for _, u := range ok {
		if err := RequireSecureProxyURL(u); err != nil {
			t.Errorf("RequireSecureProxyURL(%q) = %v, want nil", u, err)
		}
	}

	bad := []string{
		"http://proxy.example.com",        // plain http to a remote host
		"http://1.2.3.4:6664",             // plain http to a remote IP
		"http://evil.ngrok-free.dev/info", // the shape of the stale default
	}
	for _, u := range bad {
		if err := RequireSecureProxyURL(u); err == nil {
			t.Errorf("RequireSecureProxyURL(%q) = nil, want error", u)
		}
	}
}

func TestIsProductionStatus(t *testing.T) {
	if !IsProductionStatus(TeeStatusProduction) {
		t.Errorf("IsProductionStatus(%d) = false, want true", TeeStatusProduction)
	}
	// NONE, INITIALIZED, SUSPENDED, PAUSED, BANNED must all be rejected.
	for _, s := range []uint8{0, 1, 3, 4, 5} {
		if IsProductionStatus(s) {
			t.Errorf("IsProductionStatus(%d) = true, want false", s)
		}
	}
}
