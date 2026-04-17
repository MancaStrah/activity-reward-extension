package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"activity-reward-extension/internal/config"
	"activity-reward-extension/pkg/types"

	"github.com/flare-foundation/go-flare-common/pkg/logger"
)

// httpClient is the shared client for all outbound calls (TEE node decrypt/sign and
// the Strava API). Its timeout is a backstop only: every request also carries the
// per-action context deadline (config.ActionBudget), which is the real bound, so a
// slow upstream cannot make the extension outlive the node's 2s action timeout.
var httpClient = &http.Client{Timeout: config.ActionBudget}

// readLimited drains at most config.MaxResponseBytes from an upstream response, so a
// hostile or malfunctioning endpoint cannot balloon enclave memory.
func readLimited(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, config.MaxResponseBytes))
}

// --- TEE node communication types ---
//
// The sign port speaks base64, not hex: tee-node is Go, and Go marshals []byte
// as base64 in JSON. See docs/extension-contract.md §3.

type DecryptRequest struct {
	EncryptedMessage []byte `json:"encryptedMessage"`
}

type DecryptResponse struct {
	DecryptedMessage []byte `json:"decryptedMessage"`
}

type SignRequest struct {
	Message []byte `json:"message"`
}

type SignResponse struct {
	Message   []byte `json:"message"`
	Signature []byte `json:"signature"`
}

// --- TEE node calls ---

// postJSON sends a JSON body to url under ctx and returns the bounded response body.
func postJSON(ctx context.Context, url string, payload any, what string) ([]byte, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling %s request: %w", what, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("creating %s request: %w", what, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", what, err)
	}
	defer resp.Body.Close()

	body, err := readLimited(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		logger.Errorf("%s returned %d: %s", what, resp.StatusCode, string(body))
		return nil, fmt.Errorf("%s failed with status %d", what, resp.StatusCode)
	}
	return body, nil
}

// --- Decrypt failure reasons ---
//
// INVARIANT: every error returned by decryptToken is publicly readable, on exactly
// the terms set out above the grant rejections in grant.go. buildResult copies it
// into ActionResult.Log, and the proxy serves that log from an unauthenticated
// endpoint keyed by the on-chain instruction ID, so whatever these say is
// world-readable for any instruction anyone can submit.
//
// Nothing here handles a decrypted value directly — the failures are a transport
// problem or a malformed response envelope — but the response body being decoded
// is the machine key's own output, and a decoder's message can quote the bytes it
// choked on. Fixed reasons keep that structurally impossible rather than true by
// inspection; the operator detail goes to the local log through rejectDecrypt.
var (
	errDecryptCall     = errors.New("the TEE node decrypt call did not succeed")
	errDecryptResponse = errors.New("the TEE node returned a malformed decrypt response")
)

// rejectDecrypt mirrors rejectGrant in grant.go: it records detail in the
// enclave's own log, where an operator can see what actually failed, and returns
// only the fixed, value-free reason a caller may read.
func rejectDecrypt(reason error, detail string) error {
	logger.Warnf("decrypt failed: %v (%s)", reason, detail)
	return reason
}

// decryptToken calls the TEE node's /decrypt endpoint and returns the raw plaintext.
// Callers must not treat the plaintext as a bare Strava token: it is an ABI-encoded
// grant — see parseAndVerifyGrant, which validates it against the instruction and
// extracts the token.
func decryptToken(ctx context.Context, ciphertext []byte) ([]byte, error) {
	url := fmt.Sprintf("http://localhost:%d/decrypt", config.SignPort)
	body, err := postJSON(ctx, url, DecryptRequest{EncryptedMessage: ciphertext}, "decrypt")
	if err != nil {
		// postJSON's message names the endpoint and the transport failure, which
		// an operator wants and a caller has no business reading.
		return nil, rejectDecrypt(errDecryptCall, err.Error())
	}

	var dr DecryptResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, rejectDecrypt(errDecryptResponse, fmt.Sprintf("decoding decrypt response: %v", err))
	}
	return dr.DecryptedMessage, nil
}

// signPayload calls the TEE node's /sign endpoint to sign a payload.
func signPayload(ctx context.Context, payload []byte) ([]byte, error) {
	// The node rejects an empty message, but relying on that to catch a
	// mis-encoded payload would be relying on an accident — refuse here.
	if len(payload) == 0 {
		return nil, fmt.Errorf("refusing to sign an empty payload")
	}

	url := fmt.Sprintf("http://localhost:%d/sign", config.SignPort)
	body, err := postJSON(ctx, url, SignRequest{Message: payload}, "sign")
	if err != nil {
		return nil, err
	}

	var sr SignResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("decoding sign response: %w", err)
	}
	if len(sr.Signature) == 0 {
		return nil, fmt.Errorf("sign returned an empty signature (is CHAIN_ID set?)")
	}
	return sr.Signature, nil
}

// --- Strava API calls ---

var allowedSportTypes = map[string]bool{
	"Run":              true,
	"TrailRun":         true,
	"Ride":             true,
	"VirtualRide":      true,
	"MountainBikeRide": true,
	"EBikeRide":        true,
}

// stravaGet issues an authenticated GET under ctx and returns the bounded body.
func stravaGet(ctx context.Context, url, token, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating %s request: %w", what, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Strava %s API: %w", what, err)
	}
	defer resp.Body.Close()

	body, err := readLimited(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading Strava %s response: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		logger.Errorf("Strava %s API returned %d: %s", what, resp.StatusCode, string(body))
		return nil, fmt.Errorf("Strava %s API failed with status %d", what, resp.StatusCode)
	}
	return body, nil
}

// fetchAthleteID calls the Strava API to get the authenticated athlete's permanent ID.
func fetchAthleteID(ctx context.Context, token string) (int64, error) {
	body, err := stravaGet(ctx, "https://www.strava.com/api/v3/athlete", token, "athlete")
	if err != nil {
		return 0, err
	}

	var athlete types.StravaAthlete
	if err := json.Unmarshal(body, &athlete); err != nil {
		return 0, fmt.Errorf("decoding athlete response: %w", err)
	}
	// A missing "id" field decodes to 0 without error, and a negative id would hash
	// to the same value as its positive counterpart (see hashAthleteID). Either would
	// collapse distinct users onto one shared athleteHash, so the first payout would
	// lock out everyone else mapped to it. Reject both rather than mint a bad identity.
	if athlete.ID <= 0 {
		return 0, fmt.Errorf("Strava athlete API returned a non-positive athlete id (%d)", athlete.ID)
	}
	return athlete.ID, nil
}

// --- Distance failure reasons ---
//
// INVARIANT: as with the grant rejections in grant.go and the decrypt failures above,
// every error returned by fetchMonthlyDistance is publicly readable. buildResult puts
// it in ActionResult.Log and the proxy serves that from an unauthenticated endpoint
// keyed by the on-chain instruction ID.
//
// What these describe is the athlete's own activity data, so the values are private
// even though the failures are mundane: an activity id identifies one specific Strava
// activity, and a count of activities in the month is a fact about how much that person
// trains. Both belong in the operator's log, not in a world-readable field, so the
// reasons below are fixed and value-free and rejectDistance carries the figures locally.
var (
	errDistancePageBudget   = errors.New("the activity listing did not end within the supported number of pages")
	errDistanceTimeBudget   = errors.New("the activity listing did not end within the time allowed for one action")
	errDistanceActivityID   = errors.New("Strava returned a qualifying activity with an unusable id")
	errDistanceActivityDate = errors.New("Strava returned a qualifying activity with no start date")
	errDistanceActivityKm   = errors.New("Strava returned a qualifying activity with an unusable distance")
)

// rejectDistance mirrors rejectGrant and rejectDecrypt: detail goes to the enclave's
// own log for the operator, and only the fixed reason reaches the caller.
func rejectDistance(reason error, detail string) error {
	logger.Warnf("monthly distance rejected: %v (%s)", reason, detail)
	return reason
}

// fetchMonthlyDistance sums the athlete's qualifying distance for the month beginning
// at monthStart, in km.
//
// monthStart is a PARAMETER rather than being re-derived here: the caller samples the
// month boundary once and reuses it for both the query window and the signed proof. If
// the two were sampled independently, a request straddling 00:00 UTC on the 1st could
// return the previous month's kilometres labelled with the new month, which the
// contract would accept as a fresh month's activity and pay out a second time.
//
// The window is enforced HERE, on each activity's own start_date, and not delegated to
// the query. `after`/`before` are still sent, because narrowing the listing server-side
// keeps the response small — but they are an optimisation, not the boundary, and they
// are deliberately widened by config.StravaQuerySlack so the listing is a strict
// SUPERSET of the attested window. Strava documents them only as filtering "activities
// that have taken place before / after a certain time": no field is named and no
// timezone is given, so whether they compare against the absolute start_date or the
// naive start_date_local is unspecified. A query sent at the exact window bounds could
// therefore exclude activities that belong to the attested month, and the enclave would
// never see them to include them. Since the signed proof labels its total with a UTC
// monthStart that the contract checks against its own calendar, the enclave asks for
// more than it needs and confirms the boundary itself: every activity is kept only if
// its start_date lies in [monthStart, now).
func fetchMonthlyDistance(ctx context.Context, token string, monthStart time.Time, now time.Time) (float64, error) {
	var totalMeters float64
	var seen int
	// largestPage is the biggest page Strava has actually returned so far. Asking for
	// config.StravaPerPage does not mean receiving it, so this — not the requested
	// size — is what the exhaustion paths report, because it is what decides how many
	// activities the page budget really covers.
	var largestPage int
	// slowestFetch is the longest single page request so far, used to judge whether
	// another page fits in what is left of the action budget.
	var slowestFetch time.Duration
	// counted holds the activity ids already summed. The listing is paginated and can
	// shift between requests (an upload or an edit reorders it), so the same activity
	// can appear on two pages; without this it would be counted twice and the athlete
	// would be attested a distance they did not cover.
	counted := make(map[int64]struct{})

	// The query window, widened at both ends — see config.StravaQuerySlack.
	queryAfter := monthStart.Add(-config.StravaQuerySlack).Unix()
	queryBefore := now.Add(config.StravaQuerySlack).Unix()

	// Page explicitly: a single page silently truncates a prolific athlete's month
	// at the page size, under-counting them with no error.
	for page := 1; page <= config.StravaMaxPages; page++ {
		// Only ask for a page there is time to use. Without this the loop runs until
		// the context expires somewhere further down — most likely on the /sign call,
		// after the distance was already computed — and the operator is left with a
		// deadline error that says nothing about which listing was too long to read.
		if page > 1 && !timeForAnotherPage(ctx, slowestFetch) {
			return 0, rejectDistance(errDistanceTimeBudget, fmt.Sprintf(
				"ran out of action budget after %d pages (%d activities seen, largest page %d, "+
					"slowest page %s, budget %s)",
				page-1, seen, largestPage, slowestFetch, config.ActionBudget))
		}

		url := fmt.Sprintf(
			"https://www.strava.com/api/v3/athlete/activities?after=%d&before=%d&per_page=%d&page=%d",
			queryAfter, queryBefore, config.StravaPerPage, page,
		)

		fetchStarted := time.Now()
		body, err := stravaGet(ctx, url, token, "activities")
		if elapsed := time.Since(fetchStarted); elapsed > slowestFetch {
			slowestFetch = elapsed
		}
		if err != nil {
			return 0, err
		}

		var activities []types.StravaActivity
		if err := json.Unmarshal(body, &activities); err != nil {
			return 0, fmt.Errorf("decoding activities response: %w", err)
		}

		// An EMPTY page is the end of the window, and it is the only terminator that
		// can be trusted. Stopping on a merely SHORT page would mean believing that a
		// request for config.StravaPerPage items returns exactly that many whenever
		// more exist — which the Strava API does not promise: it documents per_page
		// only as "Number of items per page. Defaults to 30", with no maximum and no
		// guarantee about the returned count. If the server caps the page size below
		// what was asked for, every page comes back short, and a short-page terminator
		// would return on page 1 having counted only part of the month. That is the
		// same silent truncation the paging above exists to prevent, so the loop keeps
		// asking until Strava says there is nothing left. The cost is one extra request
		// per proof, which is the honest price of a total that is actually complete.
		if len(activities) == 0 {
			return validatedKm(totalMeters / 1000.0)
		}
		seen += len(activities)
		if len(activities) > largestPage {
			largestPage = len(activities)
		}

		for _, a := range activities {
			// Manual entries are self-reported with no recorded track; flagged ones
			// Strava itself distrusts. Neither should earn a reward. (A fabricated
			// GPX/FIT file uploaded as a normal activity is still neither, which is
			// the limit of what this can detect.)
			//
			// This filter runs FIRST so the stricter checks below only ever apply to
			// activities that would actually be summed: an unusable id or start_date
			// on a Walk is no reason to refuse the whole proof.
			if !allowedSportTypes[a.SportType] || a.Manual || a.Flagged {
				continue
			}
			// A missing "id" decodes to 0, and every such activity would then collide
			// on the same key in `counted` — so all but the first would be silently
			// dropped from a total that is supposed to be complete. Refuse instead.
			if a.ID <= 0 {
				return 0, rejectDistance(errDistanceActivityID,
					fmt.Sprintf("non-positive activity id %d on page %d", a.ID, page))
			}
			if _, dup := counted[a.ID]; dup {
				continue
			}
			// A missing "start_date" decodes to the zero time. Without it the month
			// cannot be confirmed, and this figure is about to be signed as belonging
			// to one specific month — so refuse rather than guess. Silently skipping
			// would under-count, which is the failure the paging above exists to avoid.
			if a.StartDate.IsZero() {
				return 0, rejectDistance(errDistanceActivityDate, fmt.Sprintf(
					"activity id %d has no start_date; cannot confirm it falls in the attested month", a.ID))
			}
			// The attested window, enforced on the activity's own timestamp: inclusive
			// of monthStart, exclusive of now. Anything outside it belongs to another
			// month (or has not happened yet) and must not enter this proof, whatever
			// the query parameters returned.
			if a.StartDate.Before(monthStart) || !a.StartDate.Before(now) {
				continue
			}
			// Guard each summand: a NaN or Inf anywhere poisons the total, and the
			// total is what ends up converted to int64 and signed.
			if math.IsNaN(a.Distance) || math.IsInf(a.Distance, 0) || a.Distance < 0 {
				return 0, rejectDistance(errDistanceActivityKm, fmt.Sprintf(
					"activity id %d has a non-finite or negative distance (%v)", a.ID, a.Distance))
			}
			counted[a.ID] = struct{}{}
			totalMeters += a.Distance
		}
	}

	// The page budget ran out before Strava returned an empty page, so there may be
	// activities in the window that were never fetched. Refuse rather than sign a
	// total known to be possibly incomplete.
	//
	// The detail records the largest page actually returned, because that is what
	// turns the page cap into a number of activities: StravaMaxPages pages of
	// `largestPage` items each is the real ceiling, and it is only equal to the
	// requested StravaPerPage if Strava honoured the request.
	return 0, rejectDistance(errDistancePageBudget, fmt.Sprintf(
		"%d pages were not enough to reach the end of the window (%d activities seen, "+
			"largest page %d of the %d requested)",
		config.StravaMaxPages, seen, largestPage, config.StravaPerPage))
}

// timeForAnotherPage reports whether the remaining action budget covers one more page
// request plus the signing work that has to follow it. A context without a deadline
// (unit tests, a caller that opted out) imposes no limit.
func timeForAnotherPage(ctx context.Context, slowestFetch time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	// Budget the slowest page seen so far, not the average: the pages are the same
	// request against the same endpoint, so the slowest one is the honest estimate of
	// what the next will cost, and being wrong in the optimistic direction means
	// dying on the /sign call instead of reporting the listing was too long.
	return time.Until(deadline) > slowestFetch+config.StravaPageTimeReserve
}

// validatedKm screens the summed distance before it is rounded into an int64 and
// signed. Converting an out-of-range float to int64 is implementation-defined in Go
// and differs by architecture — on arm64 a NaN/Inf total would pack cleanly and get
// signed as a bogus distance, while on amd64 it becomes negative and fails to pack.
// Neither belongs in a signed attestation, so reject it here instead.
func validatedKm(km float64) (float64, error) {
	if math.IsNaN(km) || math.IsInf(km, 0) {
		return 0, fmt.Errorf("computed a non-finite monthly distance")
	}
	if km < 0 {
		return 0, fmt.Errorf("computed a negative monthly distance (%v km)", km)
	}
	if km > config.MaxMonthlyKm {
		return 0, fmt.Errorf("computed an implausible monthly distance (%v km, ceiling %v)", km, config.MaxMonthlyKm)
	}
	return km, nil
}

// --- Time helpers ---

// monthStartOf returns the 1st of t's month at 00:00 UTC.
// Must agree with _currentMonthStart() in the Solidity contract; run-test
// cross-checks the two before sending instructions.
func monthStartOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
