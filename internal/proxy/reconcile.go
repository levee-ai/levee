package proxy

import (
	"log/slog"

	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/pkg/types"
)

// reconcileAction is the post-forward budget operation. The zero value is
// actionForfeit, the safe default per 001 ("when in doubt, forfeit"). A missed
// exit point or a panic therefore forfeits rather than leaks the reservation.
type reconcileAction int

const (
	actionForfeit   reconcileAction = iota // zero value: safe default
	actionReconcile                        // commit the input/output token split
	actionTrack                            // observe-mode breach, no reservation
	actionNone                             // passthrough / non-JSON / unknown-agent
)

// reconcileOutcome carries the decision from an exit point to the single deferred
// applier in ServeHTTP. Tokens are carried as an input/output split so the dollar
// budget can be priced at settlement, the token budget commits their sum.
type reconcileOutcome struct {
	action       reconcileAction
	inputTokens  int64
	outputTokens int64
	reason       string
}

// inputEstimator is the subset of tokens.Estimator the fallback needs. An
// interface keeps reconcile.go testable with a stub.
type inputEstimator interface {
	EstimateInput(model string, body []byte) int64
}

// reconcileForResponse decides the budget operation for a non-streaming
// response, given the upstream status and the (already-read) body.
func reconcileForResponse(provider string, statusCode int, body []byte) reconcileOutcome {
	// Provider returned a complete HTTP response that is not a success: it told
	// us it did not process tokens. Release the reservation, deduct nothing.
	if statusCode < 200 || statusCode >= 300 {
		return reconcileOutcome{action: actionReconcile, reason: "provider_refused"}
	}
	if input, output, ok := extractNonStreamingUsage(provider, body); ok {
		return reconcileOutcome{action: actionReconcile, inputTokens: input, outputTokens: output, reason: "reconciled"}
	}
	// 2xx but no usage field: cannot reconcile, forfeit the full reservation.
	return reconcileOutcome{action: actionForfeit, reason: "usage_missing"}
}

// reconcileForStream decides the budget operation for a finished stream.
// requestBody feeds the input half of the fallback estimate when the provider
// did not report input tokens. It may be empty when the caller has no body, in
// which case the input estimate is zero and only the output heuristic contributes.
func reconcileForStream(state *streamState, estimator inputEstimator, requestBody []byte) reconcileOutcome {
	switch state.endReason {
	case endClientDisconnect:
		// Provider may keep generating after we stop reading. Direction of error
		// unknown. Forfeit (002 Client Disconnects).
		return reconcileOutcome{action: actionForfeit, reason: "client_disconnect"}
	case endIdleTimeout:
		return reconcileOutcome{action: actionForfeit, reason: "idle_timeout"}
	case endScanError:
		return reconcileOutcome{action: actionForfeit, reason: "sse_error"}
	}

	// Clean EOF (endNormal or endUpstreamDrop). If neither a usage component nor
	// content arrived, there is nothing to account for. Forfeit.
	if !state.sawAuthoritativeUsage && state.contentBytes <= 0 {
		return reconcileOutcome{action: actionForfeit, reason: "empty_stream"}
	}
	input, output, reason := composeStreamTokens(state, estimator, requestBody)
	return reconcileOutcome{action: actionReconcile, inputTokens: input, outputTokens: output, reason: reason}
}

// trackOutcomeForStream builds the observe-mode Track outcome for a finished
// stream. Track applies only on a clean EOF. An aborted stream (disconnect,
// idle, scan error) has no trustworthy count, so the observe breach accepts the
// under-count (001). A clean EOF with neither usage nor content also skips.
func trackOutcomeForStream(state *streamState, requestBody []byte, estimator inputEstimator) reconcileOutcome {
	if state.endReason != endNormal && state.endReason != endUpstreamDrop {
		return reconcileOutcome{action: actionNone, reason: "observe_skip"}
	}
	if !state.sawAuthoritativeUsage && state.contentBytes <= 0 {
		return reconcileOutcome{action: actionNone, reason: "observe_skip"}
	}
	input, output, _ := composeStreamTokens(state, estimator, requestBody)
	return reconcileOutcome{action: actionTrack, inputTokens: input, outputTokens: output, reason: "observe_track"}
}

// composeStreamTokens returns the input and output token counts to commit for a
// clean-EOF stream, backfilling any half the provider did not report (input from
// the estimator, output from the content-byte heuristic, both over-counting in the
// safe direction). The reason is "reconciled" when both halves were authoritative,
// "tiktoken_fallback" when at least one was estimated.
func composeStreamTokens(state *streamState, estimator inputEstimator, requestBody []byte) (input, output int64, reason string) {
	estimated := false

	input = state.inputTokens
	if input == 0 {
		estimated = true
		if estimator != nil && len(requestBody) > 0 {
			input = estimator.EstimateInput("", requestBody)
		}
	}

	output = state.outputTokens
	if output == 0 {
		estimated = true
		output = heuristicOutputTokens(state.contentBytes)
	}

	reason = "reconciled"
	if estimated {
		reason = "tiktoken_fallback"
	}
	return input, output, reason
}

// applyReconcile executes the outcome against the budget store, settling every
// budget to its actual cost. It is the single deferred call in ServeHTTP.
// Best-effort: errors are logged, never returned to the client. The model and
// budgetTypes let it price the dollar slot identically to admission.
func applyReconcile(
	store *budget.Store,
	logger *slog.Logger,
	agentName string,
	reservationID types.ReservationID,
	model string,
	budgetTypes []string,
	estimate int64,
	outcome reconcileOutcome,
) {
	switch outcome.action {
	case actionNone:
		return
	case actionTrack:
		actuals, _ := budgetAmounts(budgetTypes, model, outcome.inputTokens, outcome.outputTokens)
		if err := store.TrackMulti(agentName, actuals); err != nil {
			logger.Warn("Track failed", "agent", agentName, "error", err.Error())
			return
		}
		logger.Info("Usage tracked in observe mode", "agent", agentName, "action", "track",
			"tokens", outcome.inputTokens+outcome.outputTokens, "reason", outcome.reason)
	case actionReconcile:
		actuals, _ := budgetAmounts(budgetTypes, model, outcome.inputTokens, outcome.outputTokens)
		if err := store.ReconcileMulti(agentName, reservationID, actuals); err != nil {
			logger.Warn("Reconcile failed", "agent", agentName, "error", err.Error())
			return
		}
		actualTokens := outcome.inputTokens + outcome.outputTokens
		logger.Info("Budget reconciled", "agent", agentName, "action", "reconcile",
			"estimate", estimate, "actual", actualTokens, "drift", actualTokens-estimate, "reason", outcome.reason)
	default: // actionForfeit
		if err := store.Forfeit(agentName, reservationID); err != nil {
			logger.Warn("Forfeit failed", "agent", agentName, "error", err.Error())
			return
		}
		logger.Info("Budget forfeited", "agent", agentName, "action", "forfeit",
			"estimate", estimate, "reason", outcome.reason)
	}
}
