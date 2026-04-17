package fccutils

import (
	"math"
	"testing"
)

// A faithful result reconstructs exactly: the extension publishes distanceX1000 as
// int64(math.Round(totalKm * 1000)) of the very float it publishes as distanceKm.
func TestCheckDistanceAgreementAcceptsFaithfulPairs(t *testing.T) {
	cases := []struct {
		km     float64
		x1000  int64
		reason string
	}{
		{12.4, 12400, "an ordinary result"},
		{0, 0, "a month with no qualifying activity"},
		{2.0, 2000, "exactly the reward threshold"},
		{1.9995, 2000, "a float just under the bar whose rounded value clears it"},
		{100000, 100000000, "the enclave's MaxMonthlyKm ceiling"},
		{12.4, 12401, "one metre out — inside the cross-language rounding tolerance"},
	}
	for _, c := range cases {
		if err := CheckDistanceAgreement(c.km, c.x1000); err != nil {
			t.Errorf("rejected %s (km=%v, x1000=%d): %v", c.reason, c.km, c.x1000, err)
		}
	}
}

// distanceKm is the only unsigned field in the result, so it is the only one a
// compromised proxy can choose freely. These are the pairs it would choose.
func TestCheckDistanceAgreementRejectsContradictions(t *testing.T) {
	cases := []struct {
		name  string
		km    float64
		x1000 int64
	}{
		{"km inflated while the signed value is zero", 42.0, 0},
		{"km understates a signed value that clears the bar", 0.5, 5000},
		{"km negative against a signed value that pays out", -1000.0, 2500},
		{"km off by two metres", 12.4, 12402},
		{"km is NaN", math.NaN(), 12400},
		{"km is +Inf", math.Inf(1), 12400},
		{"km is -Inf", math.Inf(-1), 12400},
		{"km large enough to overflow the scaling", math.MaxFloat64, 12400},
	}
	for _, c := range cases {
		if err := CheckDistanceAgreement(c.km, c.x1000); err == nil {
			t.Errorf("%s: accepted km=%v against distanceX1000=%d", c.name, c.km, c.x1000)
		}
	}
}

// NaN fails every comparison, so a bare subtraction would report agreement for it.
// This pins the ordering that makes it fail instead.
func TestCheckDistanceAgreementRejectsNaNRatherThanComparingIt(t *testing.T) {
	err := CheckDistanceAgreement(math.NaN(), 0)
	if err == nil {
		t.Fatal("NaN distanceKm was accepted against distanceX1000=0")
	}
	if got := err.Error(); got == "" {
		t.Fatal("rejection carried no reason")
	}
}
