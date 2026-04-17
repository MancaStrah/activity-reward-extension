package fccutils

import (
	"math"

	"github.com/pkg/errors"
)

// maxDistanceDisagreementX1000 is how far the informational distanceKm may sit
// from the signed distanceX1000 before the two are treated as contradicting each
// other, in km*1000 — i.e. one metre.
//
// The enclave derives both from ONE float: distanceX1000 is
// int64(math.Round(totalKm * 1000)) of the same totalKm it reports as distanceKm,
// so a faithful result reconstructs exactly and the tolerance is spare change. It
// is not zero only because the container contract is language-agnostic
// (docs/extension-contract.md): a re-implementation that rounds half-away-from-zero
// where Go rounds half-to-even may legitimately differ by one unit in the last
// place, and refusing a correct proof over that would be worse than the leniency.
const maxDistanceDisagreementX1000 = 1.0

// CheckDistanceAgreement rejects a result whose informational distanceKm
// contradicts the signed distanceX1000.
//
// distanceKm is the ONLY field of the distance result that the TEE does not sign:
// the payload covers distanceX1000 and eleven other fields, and the contract acts
// on the integer alone. So distanceKm arrives from the proxy's unauthenticated
// result endpoint with nothing vouching for it, and a compromised or impersonated
// proxy chooses it freely.
//
// The tools therefore display the signed integer rather than the float, which is
// what makes what an operator reads the same number the chain will act on. This
// check closes the other half: with the float no longer displayed it would
// otherwise be entirely unchecked, still reaching a terminal through -json and
// through get-result's raw data dump. A well-behaved extension computes the two
// from one value, so a disagreement does not mean "the float is wrong" — it means
// the result did not come from a well-behaved extension, and the right response is
// the one get-result already takes for an unparseable field: stop, rather than
// display it as if it were genuine.
//
// Callers screen distanceX1000 for negativity separately, with their own message;
// this reports only the disagreement.
func CheckDistanceAgreement(distanceKm float64, distanceX1000 int64) error {
	// Checked before the arithmetic: NaN fails every comparison silently, so a
	// bare subtraction would report agreement for it.
	if math.IsNaN(distanceKm) || math.IsInf(distanceKm, 0) {
		return errors.Errorf("distanceKm is not a finite number (%v)", distanceKm)
	}
	// A finite distanceKm large enough to overflow to +Inf here still yields an
	// infinite difference, so it is rejected rather than wrapping to something
	// that looks close.
	if diff := math.Abs(distanceKm*1000 - float64(distanceX1000)); diff > maxDistanceDisagreementX1000 {
		return errors.Errorf(
			"distanceKm (%v) contradicts the signed distanceX1000 (%d): the TEE derives both from one value, "+
				"so a result where they disagree did not come from this extension",
			distanceKm, distanceX1000)
	}
	return nil
}
