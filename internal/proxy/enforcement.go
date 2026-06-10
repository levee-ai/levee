package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/pkg/types"
)

// agentRuntime is the per-agent enforcement context resolved from config at
// startup. budgetTypes is index-aligned with the agent's configured budgets, so
// the proxy can build the amounts slice Admit expects.
type agentRuntime struct {
	mode        string
	budgetTypes []string
}

// postForwardPolicy tells the reconcile site how to settle a forwarded request.
type postForwardPolicy int

const (
	settleNone     postForwardPolicy = iota // passthrough, non-JSON, or unknown-passthrough: no budget op
	settleReserved                          // a reservation is held: run the 001 decision tree
	settleTrack                             // observe breach, no reservation: track actual usage if known
)

// enforcement is the result of agent identification and budget admission.
type enforcement struct {
	agentName     string
	reservationID types.ReservationID
	proceed       bool // false: a rejection response was already written
	postForward   postForwardPolicy
}

// budgetAmounts maps a model and an input/output token split onto the agent's
// budget slots. The tokens slot gets input+output, the dollars slot gets the
// microdollar cost from the pricing table. It returns the amounts and whether
// every dollar-priced model was known (false means at least one was priced at the
// max known rate, so the caller can warn). The SAME function prices both the
// reserve estimate and the settle actuals, so admission and settlement never
// disagree on the cost of a request.
func budgetAmounts(budgetTypes []string, model string, inputTokens, outputTokens int64) (amounts []int64, pricingKnown bool) {
	amounts = make([]int64, len(budgetTypes))
	pricingKnown = true
	for i, unit := range budgetTypes {
		switch unit {
		case "tokens":
			amounts[i] = saturatingSumTokens(inputTokens, outputTokens)
		case "dollars":
			cost, known := budget.CostMicrodollars(model, inputTokens, outputTokens)
			amounts[i] = cost
			if !known {
				pricingKnown = false
			}
		}
	}
	return amounts, pricingKnown
}

// saturatingSumTokens returns input+output, clamped to MaxInt64 so a near-ceiling
// estimate never wraps negative and bypasses the budget check.
func saturatingSumTokens(input, output int64) int64 {
	if input > math.MaxInt64-output {
		return math.MaxInt64
	}
	return input + output
}

// hasDollarBudget reports whether any budget slot is a dollars budget.
func hasDollarBudget(budgetTypes []string) bool {
	for _, unit := range budgetTypes {
		if unit == "dollars" {
			return true
		}
	}
	return false
}

// budgetErrorBody is the Levee-native 429 body for budget exhaustion.
type budgetErrorBody struct {
	Error struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Agent   string          `json:"agent"`
		Budget  budgetErrorInfo `json:"budget"`
	} `json:"error"`
}

type budgetErrorInfo struct {
	Type      string `json:"type"`
	Limit     int64  `json:"limit"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	ResetAt   string `json:"reset_at"`
}

// writeBudgetRejection writes the 429 budget-exhausted response. now is passed in
// so Retry-After is computed against a single consistent clock reading and tests
// are deterministic.
func writeBudgetRejection(writer http.ResponseWriter, agentName string, binding *budget.BudgetStatus, now time.Time) {
	retryAfterSeconds := int64(math.Ceil(binding.ResetAt.Sub(now).Seconds()))
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	remaining := binding.Remaining
	if remaining < 0 {
		remaining = 0
	}

	var body budgetErrorBody
	body.Error.Type = "budget_exhausted"
	body.Error.Message = binding.Type + " budget exhausted for agent " + strconv.Quote(agentName)
	body.Error.Agent = agentName
	body.Error.Budget = budgetErrorInfo{
		Type:      binding.Type,
		Limit:     binding.Limit,
		Used:      binding.Used,
		Remaining: remaining,
		ResetAt:   binding.ResetAt.UTC().Format(time.RFC3339),
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
	writer.Header().Set("X-Budget-Remaining", strconv.FormatInt(remaining, 10))
	writer.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(writer).Encode(body)
}

// writeConcurrencyRejection writes the 429 concurrency-limit response.
func writeConcurrencyRejection(writer http.ResponseWriter) {
	writeSimpleError(writer, http.StatusTooManyRequests, "rate_limit", "concurrent stream limit reached")
}

// writeUnknownAgent writes the 403 unknown-agent response (block policy).
func writeUnknownAgent(writer http.ResponseWriter) {
	writeSimpleError(writer, http.StatusForbidden, "unknown_agent",
		"request could not be identified to a configured agent")
}

// writeSimpleError writes the simple {"error":{"type","message"}} envelope.
// (*Proxy).writeError delegates here so the envelope has one definition.
func writeSimpleError(writer http.ResponseWriter, status int, errorType, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	payload := struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	payload.Error.Type = errorType
	payload.Error.Message = message
	_ = json.NewEncoder(writer).Encode(payload)
}

// enforce runs agent identification and budget admission. It returns an
// enforcement struct. proceed=false means a rejection response was already
// written and the caller must return immediately. postForward tells the
// reconcile site how to settle the forwarded request. info may be nil for
// non-JSON requests (no model, cannot estimate).
func (proxy *Proxy) enforce(writer http.ResponseWriter, request *http.Request, info *RequestInfo, body []byte) enforcement {
	resolved, err := proxy.resolver.Resolve(request)
	if err != nil {
		if proxy.unknownAgent == "block" {
			proxy.logger.Info("Request blocked, unknown agent", "path", request.URL.Path)
			writeUnknownAgent(writer)
			return enforcement{proceed: false}
		}
		return enforcement{proceed: true, postForward: settleNone}
	}

	runtime := proxy.agents[resolved]
	if runtime.mode == "passthrough" {
		return enforcement{agentName: resolved, proceed: true, postForward: settleNone}
	}
	if info == nil {
		// Non-JSON request, no model to estimate. Forward without reserving.
		return enforcement{agentName: resolved, proceed: true, postForward: settleNone}
	}

	inputEstimate, outputEstimate := proxy.estimator.EstimateSplit(info.Model, body)
	// tokenEstimate is the plain sum used only for the log fields below. The
	// amount that actually gates admission is the saturating sum computed inside
	// budgetAmounts, so a near-ceiling estimate cannot wrap negative there.
	tokenEstimate := inputEstimate + outputEstimate
	amounts, pricingKnown := budgetAmounts(runtime.budgetTypes, info.Model, inputEstimate, outputEstimate)
	// pricingKnown only flips false inside the dollars-slot branch, so it already
	// implies a dollar budget. The explicit hasDollarBudget check is a guard
	// against any future path that could set pricingKnown false, and the helper is
	// reused by the settlement path.
	if !pricingKnown && hasDollarBudget(runtime.budgetTypes) {
		proxy.logger.Warn("Pricing unknown for model, charged at max known rate",
			"agent", resolved, "model", info.Model)
	}
	outcome, err := proxy.store.Admit(resolved, amounts)
	if err != nil {
		// Admit only errors on misconfiguration (unknown agent in store, amount
		// length mismatch). Log and forward rather than failing the request.
		proxy.logger.Warn("Budget admission error", "agent", resolved, "error", err.Error())
		return enforcement{agentName: resolved, proceed: true, postForward: settleNone}
	}

	if outcome.Admitted {
		proxy.logger.Info("Budget reserved", "agent", resolved, "action", "reserve", "tokens", tokenEstimate)
		return enforcement{agentName: resolved, reservationID: outcome.ID, proceed: true, postForward: settleReserved}
	}

	// Rejected. enforce vs observe.
	if runtime.mode == "observe" {
		proxy.logger.Warn("Budget breach in observe mode", "agent", resolved, "tokens", tokenEstimate, "reason", rejectReasonString(outcome.Reason))
		return enforcement{agentName: resolved, proceed: true, postForward: settleTrack}
	}

	// enforce mode: write the rejection.
	switch outcome.Reason {
	case budget.RejectConcurrency:
		proxy.logger.Info("Request rejected, concurrency limit", "agent", resolved)
		writeConcurrencyRejection(writer)
	default:
		proxy.logger.Info("Request rejected, budget exhausted", "agent", resolved, "tokens", tokenEstimate)
		writeBudgetRejection(writer, resolved, outcome.Binding, time.Now())
	}
	return enforcement{agentName: resolved, proceed: false}
}

func rejectReasonString(reason budget.RejectReason) string {
	switch reason {
	case budget.RejectBudget:
		return "budget"
	case budget.RejectConcurrency:
		return "concurrency"
	default:
		return "none"
	}
}
