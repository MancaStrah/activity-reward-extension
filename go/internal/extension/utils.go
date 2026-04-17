package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"activity-reward-extension/internal/config"

	"github.com/flare-foundation/go-flare-common/pkg/logger"
	"github.com/flare-foundation/go-flare-common/pkg/tee/instruction"
	teetypes "github.com/flare-foundation/tee-node/pkg/types"
)

func (e *Extension) actionHandler(w http.ResponseWriter, r *http.Request) {
	// Bound the request body: the inner instruction is size-checked by
	// processorutils.Parse, but the outer envelope has unbounded fields.
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxRequestBytes)

	var action teetypes.Action
	err := json.NewDecoder(r.Body).Decode(&action)
	if err != nil {
		http.Error(w, fmt.Sprintf("decoding action: %v", err), http.StatusBadRequest)
		return
	}

	logger.Infof("received action, ID: %s", action.Data.ID)

	// Derive the work budget from the request context, so a client disconnect (or the
	// node giving up after its own 2s timeout) cancels the outbound Strava and TEE
	// calls instead of leaving them running to produce a result nobody will read.
	ctx, cancel := context.WithTimeout(r.Context(), config.ActionBudget)
	defer cancel()

	status, body := e.processAction(ctx, action)

	logger.Infof("sending action result, ID: %s, status: %d, log: %s", action.Data.ID, status, getLogFromBody(body))

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func buildResult(a teetypes.Action, df *instruction.DataFixed, data []byte, status uint8, err error) teetypes.ActionResult {
	ar := teetypes.ActionResult{
		ID:            a.Data.ID,
		SubmissionTag: a.Data.SubmissionTag,
		Version:       config.Version,
		OPType:        df.OPType,
		OPCommand:     df.OPCommand,
		Data:          data,
		Status:        status,
	}
	// Log values are part of the wire contract — see docs/extension-contract.md §4.6.
	switch status {
	case 0:
		ar.Log = fmt.Sprintf("error: %v", err)
	case 1:
		ar.Log = "ok"
	default:
		ar.Log = "pending"
	}
	return ar
}

func getLogFromBody(body []byte) string {
	var ar teetypes.ActionResult
	if err := json.Unmarshal(body, &ar); err != nil {
		return string(body)
	}
	return ar.Log
}
