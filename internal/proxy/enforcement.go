package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/levee-ai/levee/internal/budget"
)

// agentRuntime is the per-agent enforcement context resolved from config at
// startup. budgetTypes is index-aligned with the agent's configured budgets, so
// the proxy can build the amounts slice Admit expects.
type agentRuntime struct {
	mode        string
	budgetTypes []string
}

// buildAmounts maps a single token estimate onto the agent's budget slots.
// Token budgets get the estimate. Dollar budgets get 0 (priced in Session 7),
// and 0 always fits, so a dollar budget never binds this session.
func buildAmounts(budgetTypes []string, tokenEstimate int64) []int64 {
	amounts := make([]int64, len(budgetTypes))
	for i, unit := range budgetTypes {
		if unit == "tokens" {
			amounts[i] = tokenEstimate
		}
	}
	return amounts
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
// Task 9 points (*Proxy).writeError at this same function to remove duplication.
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
