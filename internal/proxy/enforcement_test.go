package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/levee-ai/levee/internal/budget"
)

func TestBuildAmounts_TokensAndDollars(t *testing.T) {
	got := buildAmounts([]string{"tokens", "dollars"}, 1234)
	if len(got) != 2 || got[0] != 1234 || got[1] != 0 {
		t.Fatalf("buildAmounts: got %v, want [1234 0]", got)
	}
}

func TestWriteBudgetRejection_BodyAndHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	resetAt := time.Date(2026, 6, 7, 19, 30, 0, 0, time.UTC)
	binding := &budget.BudgetStatus{
		Type: "tokens", Limit: 1000000, Used: 1000000, Remaining: 0, ResetAt: resetAt,
	}
	writeBudgetRejection(recorder, "researcher", binding, baseTestTime())

	if recorder.Code != 429 {
		t.Fatalf("status: got %d, want 429", recorder.Code)
	}
	if recorder.Header().Get("X-Budget-Remaining") != "0" {
		t.Errorf("X-Budget-Remaining: got %q", recorder.Header().Get("X-Budget-Remaining"))
	}
	// The 1800s gap between baseTestTime (19:00:00Z) and ResetAt (19:30:00Z)
	// locks the ceil math. A refactor dropping math.Ceil or using time.Now
	// instead of the injected now would change this value.
	if got := recorder.Header().Get("Retry-After"); got != "1800" {
		t.Errorf("Retry-After: got %q, want 1800", got)
	}
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Agent   string `json:"agent"`
			Budget  struct {
				Type      string `json:"type"`
				Limit     int64  `json:"limit"`
				Used      int64  `json:"used"`
				Remaining int64  `json:"remaining"`
				ResetAt   string `json:"reset_at"`
			} `json:"budget"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if parsed.Error.Type != "budget_exhausted" {
		t.Errorf("type: got %q, want budget_exhausted", parsed.Error.Type)
	}
	if parsed.Error.Agent != "researcher" {
		t.Errorf("agent: got %q", parsed.Error.Agent)
	}
	wantMessage := `tokens budget exhausted for agent "researcher"`
	if parsed.Error.Message != wantMessage {
		t.Errorf("message: got %q, want %q", parsed.Error.Message, wantMessage)
	}
	if parsed.Error.Budget.Type != "tokens" {
		t.Errorf("budget.type: got %q, want tokens", parsed.Error.Budget.Type)
	}
	if parsed.Error.Budget.Limit != 1000000 {
		t.Errorf("budget.limit: got %d, want 1000000", parsed.Error.Budget.Limit)
	}
	if parsed.Error.Budget.Used != 1000000 {
		t.Errorf("budget.used: got %d, want 1000000", parsed.Error.Budget.Used)
	}
	if parsed.Error.Budget.Remaining != 0 {
		t.Errorf("budget.remaining: got %d, want 0", parsed.Error.Budget.Remaining)
	}
	if parsed.Error.Budget.ResetAt != "2026-06-07T19:30:00Z" {
		t.Errorf("reset_at: got %q", parsed.Error.Budget.ResetAt)
	}
}

func TestWriteBudgetRejection_PastResetFloorsRetryAfter(t *testing.T) {
	recorder := httptest.NewRecorder()
	now := baseTestTime()
	// ResetAt is 5 minutes in the past relative to now, so the raw retry
	// computation is negative and must floor to 1. A zero or negative
	// Retry-After breaks SDK backoff, so the floor is a client contract.
	binding := &budget.BudgetStatus{
		Type: "tokens", Limit: 1000000, Used: 1000000, Remaining: 0,
		ResetAt: now.Add(-5 * time.Minute),
	}
	writeBudgetRejection(recorder, "researcher", binding, now)

	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After: got %q, want 1", got)
	}
}

func baseTestTime() time.Time {
	return time.Date(2026, 6, 7, 19, 0, 0, 0, time.UTC) // 30 min before the reset above
}
