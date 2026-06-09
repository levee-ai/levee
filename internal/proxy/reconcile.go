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
	actionReconcile                        // commit actualTokens
	actionTrack                            // observe-mode breach, no reservation
	actionNone                             // passthrough / non-JSON / unknown-agent
)

// reconcileOutcome carries the decision from an exit point to the single
// deferred applier in ServeHTTP.
type reconcileOutcome struct {
	action       reconcileAction
	actualTokens int64
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
		return reconcileOutcome{action: actionReconcile, actualTokens: 0, reason: "provider_refused"}
	}
	if tokens, ok := extractNonStreamingUsage(provider, body); ok {
		return reconcileOutcome{action: actionReconcile, actualTokens: tokens, reason: "reconciled"}
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
	tokens, reason := composeStreamTokens(state, estimator, requestBody)
	return reconcileOutcome{action: actionReconcile, actualTokens: tokens, reason: reason}
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
	tokens, _ := composeStreamTokens(state, estimator, requestBody)
	return reconcileOutcome{action: actionTrack, actualTokens: tokens, reason: "observe_track"}
}

// composeStreamTokens computes the total tokens to commit for a clean-EOF
// stream, backfilling any usage component the provider did not report. A
// provider can deliver a partial usage object (002 lines 369 to 374): an OpenAI
// chunk missing completion_tokens, or an Anthropic message_start missing
// input_tokens. Trusting the captured sum in that case under-counts, the one
// error direction 001 and Tenet 3 forbid. So each missing half is filled from
// the estimator (input) or the content-byte heuristic (output), both of which
// over-count in the safe direction. It returns the total and the reason label:
// "reconciled" when both halves were authoritative, "tiktoken_fallback" when at
// least one half was estimated.
func composeStreamTokens(state *streamState, estimator inputEstimator, requestBody []byte) (int64, string) {
	estimated := false

	inputTokens := state.inputTokens
	if inputTokens == 0 {
		estimated = true
		if estimator != nil && len(requestBody) > 0 {
			inputTokens = estimator.EstimateInput("", requestBody)
		}
	}

	outputTokens := state.outputTokens
	if outputTokens == 0 {
		estimated = true
		outputTokens = heuristicOutputTokens(state.contentBytes)
	}

	reason := "reconciled"
	if estimated {
		reason = "tiktoken_fallback"
	}
	return inputTokens + outputTokens, reason
}

// applyReconcile executes the outcome against the budget store. It is the single
// deferred call in ServeHTTP. Best-effort: errors are logged, never returned to
// the client (the response is already committed, per 001 Implementation Note 2).
func applyReconcile(
	store *budget.Store,
	logger *slog.Logger,
	agentName string,
	reservationID types.ReservationID,
	estimate int64,
	outcome reconcileOutcome,
) {
	switch outcome.action {
	case actionNone:
		return
	case actionTrack:
		if err := store.Track(agentName, outcome.actualTokens); err != nil {
			logger.Warn("Track failed", "agent", agentName, "error", err.Error())
			return
		}
		logger.Info("Usage tracked in observe mode", "agent", agentName, "action", "track",
			"tokens", outcome.actualTokens, "reason", outcome.reason)
	case actionReconcile:
		if err := store.Reconcile(agentName, reservationID, outcome.actualTokens); err != nil {
			logger.Warn("Reconcile failed", "agent", agentName, "error", err.Error())
			return
		}
		logger.Info("Budget reconciled", "agent", agentName, "action", "reconcile",
			"estimate", estimate, "actual", outcome.actualTokens,
			"drift", outcome.actualTokens-estimate, "reason", outcome.reason)
	default: // actionForfeit
		if err := store.Forfeit(agentName, reservationID); err != nil {
			logger.Warn("Forfeit failed", "agent", agentName, "error", err.Error())
			return
		}
		logger.Info("Budget forfeited", "agent", agentName, "action", "forfeit",
			"estimate", estimate, "reason", outcome.reason)
	}
}
