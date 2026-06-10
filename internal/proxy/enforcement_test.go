package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levee-ai/levee/internal/agent"
	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/internal/config"
	"github.com/levee-ai/levee/internal/tokens"
)

func TestBudgetAmounts_TokensAndDollars(t *testing.T) {
	// gpt-4o: input 1000, output 500 -> tokens slot 1500, dollars slot 7500 microdollars.
	amounts, known := budgetAmounts([]string{"tokens", "dollars"}, "gpt-4o", 1000, 500)
	if len(amounts) != 2 {
		t.Fatalf("len = %d, want 2", len(amounts))
	}
	if amounts[0] != 1500 {
		t.Errorf("tokens slot = %d, want 1500 (input+output)", amounts[0])
	}
	if amounts[1] != 7500 {
		t.Errorf("dollars slot = %d, want 7500 microdollars", amounts[1])
	}
	if !known {
		t.Error("known = false, want true for gpt-4o")
	}
}

func TestBudgetAmounts_UnknownModelReportsNotKnown(t *testing.T) {
	_, known := budgetAmounts([]string{"dollars"}, "mystery-model", 1000, 500)
	if known {
		t.Error("known = true, want false for an unrecognized model")
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

func TestWriteBudgetRejection_DollarsRenderedAsDecimal(t *testing.T) {
	recorder := httptest.NewRecorder()
	// 50_000_000 microdollars = $50.00 limit, 49_999_550 remaining = $49.99955.
	binding := &budget.BudgetStatus{
		Type: "dollars", Limit: 50_000_000, Used: 450, Remaining: 49_999_550,
		ResetAt: time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	writeBudgetRejection(recorder, "customer-bot", binding, baseTestTime())

	body := recorder.Body.String()
	// The limit must render as a dollars decimal, not raw microdollars, and unquoted.
	if !strings.Contains(body, `"limit":50.00`) {
		t.Errorf("limit not rendered as dollars decimal: %s", body)
	}
	if !strings.Contains(body, `"used":0.00045`) {
		t.Errorf("used not rendered as dollars decimal: %s", body)
	}
	if strings.Contains(body, "50000000") {
		t.Errorf("body leaked raw microdollars: %s", body)
	}
	// X-Budget-Remaining for dollars is the decimal remaining.
	if got := recorder.Header().Get("X-Budget-Remaining"); got != "49.99955" {
		t.Errorf("X-Budget-Remaining = %q, want 49.99955", got)
	}
}

func TestMicrodollarsToDecimal(t *testing.T) {
	cases := []struct {
		microdollars int64
		want         string
	}{
		{50_000_000, "50.00"},
		{49_999_550, "49.99955"},
		{1_000_000, "1.00"},
		{10_000, "0.01"},
		{0, "0.00"},
		{1, "0.000001"},
		{999_999, "0.999999"},
		{100, "0.0001"},
	}
	for _, testCase := range cases {
		if got := microdollarsToDecimal(testCase.microdollars); got != testCase.want {
			t.Errorf("microdollarsToDecimal(%d) = %q, want %q", testCase.microdollars, got, testCase.want)
		}
	}
}

// enforcingProxy builds a proxy with one enforce-mode agent that identifies via
// X-Levee-Agent: researcher and has a small token budget, pointed at upstreamURL.
func enforcingProxy(tb testing.TB, upstreamURL string, tokenLimit int64) *Proxy {
	tb.Helper()
	agents := []config.AgentConfig{{
		Name: "researcher",
		Mode: "enforce",
		Identifier: config.IdentifierConfig{
			Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "researcher",
		},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: float64(tokenLimit), Window: "1h", WindowType: "rolling"},
		},
	}}
	store, err := budget.NewStore(agents, defaultStreamLimit, nil)
	if err != nil {
		tb.Fatalf("NewStore: %v", err)
	}
	return &Proxy{
		providers:    map[string]*providerTarget{"openai": newProviderTarget(upstreamURL, testTimeouts())},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolver:     agent.NewResolver(agents),
		store:        store,
		estimator:    tokens.NewEstimator("cl100k_base"),
		agents:       map[string]agentRuntime{"researcher": {mode: "enforce", budgetTypes: []string{"tokens"}}},
		unknownAgent: "block",
	}
}

// observingProxy builds a proxy with one observe-mode agent that identifies via
// X-Levee-Agent: researcher, pointed at upstreamURL. Observe mode forwards a
// request even when it breaches the budget, then tracks the actual usage rather
// than holding a reservation. A tiny tokenLimit makes every request breach.
func observingProxy(tb testing.TB, upstreamURL string, tokenLimit int64) *Proxy {
	tb.Helper()
	agents := []config.AgentConfig{{
		Name: "researcher",
		Mode: "observe",
		Identifier: config.IdentifierConfig{
			Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "researcher",
		},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: float64(tokenLimit), Window: "1h", WindowType: "rolling"},
		},
	}}
	store, err := budget.NewStore(agents, defaultStreamLimit, nil)
	if err != nil {
		tb.Fatalf("NewStore: %v", err)
	}
	return &Proxy{
		providers:    map[string]*providerTarget{"openai": newProviderTarget(upstreamURL, testTimeouts())},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolver:     agent.NewResolver(agents),
		store:        store,
		estimator:    tokens.NewEstimator("cl100k_base"),
		agents:       map[string]agentRuntime{"researcher": {mode: "observe", budgetTypes: []string{"tokens"}}},
		unknownAgent: "block",
	}
}

func TestEnforce_AdmittedRequestForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000) // generous budget
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")

	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", recorder.Code)
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"id":"ok"`) {
		t.Errorf("body: got %q, want upstream payload (forwarding did not happen)", got)
	}
}

func TestEnforce_ExhaustedBudgetReturns429(t *testing.T) {
	var upstreamHits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	// Tiny budget: 100 tokens. A request reserving max_tokens 4096 cannot fit.
	proxy := enforcingProxy(t, upstream.URL, 100)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")

	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", recorder.Code)
	}
	var parsed struct {
		Error struct {
			Type  string `json:"type"`
			Agent string `json:"agent"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Error.Type != "budget_exhausted" {
		t.Errorf("type: got %q, want budget_exhausted", parsed.Error.Type)
	}
	if parsed.Error.Agent != "researcher" {
		t.Errorf("agent: got %q, want researcher", parsed.Error.Agent)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on budget 429")
	}
	if got := atomic.LoadInt64(&upstreamHits); got != 0 {
		t.Errorf("upstream reached %d times on a rejected request, want 0", got)
	}
}

// TestEnforce_HugeMaxTokensStillRejected is the inverse of the overflow bypass.
// A max_tokens at the int64 ceiling once wrapped the estimate negative, which
// slipped past the budget check and reached the upstream unbudgeted. With the
// reserve clamped and the store rejecting negative amounts, a huge max_tokens
// against a tiny budget must return 429 and never reach the upstream.
func TestEnforce_HugeMaxTokensStillRejected(t *testing.T) {
	var upstreamHits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 100) // tiny budget
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":9223372036854775807,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")

	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", recorder.Code)
	}
	if got := atomic.LoadInt64(&upstreamHits); got != 0 {
		t.Errorf("upstream reached %d times on an overflowing max_tokens, want 0", got)
	}
}

func TestEnforce_UnknownAgentBlocked403(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be reached for a blocked unknown agent")
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	// No X-Levee-Agent header.

	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", recorder.Code)
	}
}

func TestEnforce_ReturnsPostForwardPolicy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	info, body, _ := readRequestBody(request)
	recorder := httptest.NewRecorder()

	result := proxy.enforce(recorder, request, info, body)
	if !result.proceed {
		t.Fatal("expected proceed = true for admitted request")
	}
	if result.postForward != settleReserved {
		t.Errorf("postForward = %v, want settleReserved", result.postForward)
	}
	if result.reservationID == 0 {
		t.Error("expected a non-zero reservation id")
	}
}

func TestServeHTTP_NonStreamingReconcilesToActual(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	// After reconcile, used must be the actual 12, not the ~4096+ estimate.
	used := proxyAgentUsed(t, proxy, "researcher")
	if used != 12 {
		t.Errorf("budget used = %d, want 12 (reconciled to actual, not estimate)", used)
	}
}

func TestServeHTTP_ProviderErrorReleasesReservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	// Provider refused: reservation released, nothing deducted.
	used := proxyAgentUsed(t, proxy, "researcher")
	if used != 0 {
		t.Errorf("budget used = %d, want 0 (provider refusal deducts nothing)", used)
	}
}

func TestEnforce_ConcurrencyReleasedAfterRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	// Fire more sequential requests than the stream limit. Each must release its
	// slot via defer Forfeit, so none should hit the concurrency cap.
	for i := int64(0); i < defaultStreamLimit+5; i++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Levee-Agent", "researcher")
		proxy.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200 (slot should have been released)", i, recorder.Code)
		}
	}
}

// proxyAgentUsed reads the committed usage of an agent's first budget. It uses
// the store's exported StatusOf accessor (added in Task 7).
func proxyAgentUsed(t *testing.T, proxy *Proxy, agentName string) int64 {
	t.Helper()
	status, err := proxy.store.StatusOf(agentName)
	if err != nil {
		t.Fatalf("StatusOf(%q): %v", agentName, err)
	}
	return status.Used
}

func TestServeHTTP_OpenAIStreamReconciles(t *testing.T) {
	ssePayload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}`, "",
		`data: {"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}}`, "",
		"data: [DONE]", "",
	}, "\n")
	var injectedSawStreamOptions bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		injectedSawStreamOptions = strings.Contains(string(bodyBytes), `"include_usage":true`)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssePayload))
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if !injectedSawStreamOptions {
		t.Error("expected stream_options.include_usage=true to be injected into the upstream request")
	}
	used := proxyAgentUsed(t, proxy, "researcher")
	if used != 10 {
		t.Errorf("budget used = %d, want 10 (reconciled to streaming usage)", used)
	}
}

func TestServeHTTP_AnthropicStreamReconciles(t *testing.T) {
	ssePayload := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1}}}`, "",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"text":"hello"}}`, "",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":15}}`, "",
		"event: message_stop",
		`data: {"type":"message_stop"}`, "",
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssePayload))
	}))
	defer upstream.Close()

	// Anthropic agent: the enforcingProxy points "openai" at the upstream, so add
	// an anthropic provider target to the same upstream for this test.
	proxy := enforcingProxy(t, upstream.URL, 1000000)
	proxy.providers["anthropic"] = newProviderTarget(upstream.URL, testTimeouts())

	request := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-3-opus","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	used := proxyAgentUsed(t, proxy, "researcher")
	if used != 40 {
		t.Errorf("budget used = %d, want 40 (input 25 + output 15)", used)
	}
}

func TestServeHTTP_StreamUpstreamDropUsesFallback(t *testing.T) {
	// Content delivered, but the stream drops with no usage and no terminal marker.
	ssePayload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"some words here"}}],"usage":null}`, "",
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssePayload))
		// Handler returns: clean EOF, no [DONE], no usage chunk.
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	used := proxyAgentUsed(t, proxy, "researcher")
	// Fallback fired: some non-zero estimate, well below the 4096+ full reserve.
	if used <= 0 || used >= 4096 {
		t.Errorf("budget used = %d, want a fallback estimate in (0, 4096)", used)
	}
}

func TestServeHTTP_StreamIdleTimeoutForfeits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
		flusher.Flush()
		<-r.Context().Done() // go silent until the watchdog cancels us
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	// Tight idle so the watchdog fires fast.
	tightIdle := providerTimeouts{connect: 5 * time.Second, responseHeader: 5 * time.Second, idle: 150 * time.Millisecond, request: 5 * time.Second}
	proxy.providers["openai"] = newProviderTarget(upstream.URL, tightIdle)

	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	// Idle timeout forfeits the full reservation, so used is the full estimate
	// (well above the tiny content). Assert it is at least the output reserve.
	used := proxyAgentUsed(t, proxy, "researcher")
	if used < 4096 {
		t.Errorf("budget used = %d, want >= 4096 (idle timeout forfeits full reservation)", used)
	}
}

func TestServeHTTP_SlowButAliveStreamReconciles(t *testing.T) {
	// One event per 90ms under a 400ms idle. The wide margin keeps the test from
	// false-firing the watchdog under load or the race scheduler. The assertion
	// is about ordering (a live stream resets the watchdog faster than it fires),
	// not absolute timing, so a generous ratio costs only that the test reaches
	// the terminal marker.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}],\"usage\":null}\n\n")
			flusher.Flush()
			time.Sleep(90 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	proxy := enforcingProxy(t, upstream.URL, 1000000)
	tightIdle := providerTimeouts{connect: 5 * time.Second, responseHeader: 5 * time.Second, idle: 400 * time.Millisecond, request: 5 * time.Second}
	proxy.providers["openai"] = newProviderTarget(upstream.URL, tightIdle)

	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	used := proxyAgentUsed(t, proxy, "researcher")
	if used != 7 {
		t.Errorf("budget used = %d, want 7 (slow-but-alive reconciled, not forfeited)", used)
	}
}

func TestServeHTTP_ObserveModeNonStreamingTracksActual(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	// Tiny budget so the request breaches. Observe mode forwards anyway and
	// tracks the actual usage instead of holding a reservation.
	proxy := observingProxy(t, upstream.URL, 100)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (observe mode forwards on breach)", recorder.Code)
	}
	used := proxyAgentUsed(t, proxy, "researcher")
	if used != 12 {
		t.Errorf("budget used = %d, want 12 (observe mode tracks actual usage)", used)
	}
}

func TestServeHTTP_ObserveModeStreamingTracksActual(t *testing.T) {
	ssePayload := strings.Join([]string{
		`data: {"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}}`, "",
		"data: [DONE]", "",
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssePayload))
	}))
	defer upstream.Close()

	proxy := observingProxy(t, upstream.URL, 100)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	used := proxyAgentUsed(t, proxy, "researcher")
	if used != 10 {
		t.Errorf("budget used = %d, want 10 (observe mode tracks streaming usage)", used)
	}
}

func TestServeHTTP_ObserveModeCleanDropTracksFallback(t *testing.T) {
	// Stream delivers content then drops with no usage and no terminal marker.
	// This is a clean EOF (endUpstreamDrop), so trackOutcomeForStream tracks the
	// fallback heuristic estimate rather than skipping (skip is reserved for an
	// aborted stream: disconnect, idle, or scan error).
	ssePayload := `data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssePayload))
	}))
	defer upstream.Close()

	proxy := observingProxy(t, upstream.URL, 100)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	used := proxyAgentUsed(t, proxy, "researcher")
	if used <= 0 {
		t.Errorf("budget used = %d, want > 0 (observe clean-EOF drop tracks fallback estimate)", used)
	}
}

// dollarAndTokenProxy builds an enforce-mode agent with BOTH a token budget and a
// dollar budget (in microdollars via config), pointed at upstreamURL.
func dollarAndTokenProxy(tb testing.TB, upstreamURL string, tokenLimit int64, dollarLimit float64) *Proxy {
	tb.Helper()
	agents := []config.AgentConfig{{
		Name: "researcher", Mode: "enforce",
		Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "researcher"},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: float64(tokenLimit), Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: dollarLimit, Window: "1h", WindowType: "rolling"},
		},
	}}
	store, err := budget.NewStore(agents, defaultStreamLimit, nil)
	if err != nil {
		tb.Fatalf("NewStore: %v", err)
	}
	return &Proxy{
		providers:    map[string]*providerTarget{"openai": newProviderTarget(upstreamURL, testTimeouts())},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolver:     agent.NewResolver(agents),
		store:        store,
		estimator:    tokens.NewEstimator("cl100k_base"),
		agents:       map[string]agentRuntime{"researcher": {mode: "enforce", budgetTypes: []string{"tokens", "dollars"}}},
		unknownAgent: "block",
	}
}

func TestServeHTTP_DollarBudgetExhaustionReturns429(t *testing.T) {
	var upstreamHits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	// Generous token budget, tiny dollar budget: $0.0001 = 100 microdollars. A
	// gpt-4o request reserving max_tokens 4096 costs far more than 100 microdollars,
	// so the DOLLAR budget is the binding constraint.
	proxy := dollarAndTokenProxy(t, upstream.URL, 100_000_000, 0.0001)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	var parsed struct {
		Error struct {
			Budget struct {
				Type string `json:"type"`
			} `json:"budget"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Error.Budget.Type != "dollars" {
		t.Errorf("binding budget type = %q, want dollars", parsed.Error.Budget.Type)
	}
	if got := atomic.LoadInt64(&upstreamHits); got != 0 {
		t.Errorf("upstream reached %d times on a dollar-rejected request, want 0", got)
	}
}

func TestServeHTTP_BothBudgetsReconcileToActual(t *testing.T) {
	// gpt-4o usage: 5 input + 7 output. Dollar cost = 5*2_500_000/1e6 +
	// 7*10_000_000/1e6 = ceil(12.5) + 70 = 13 + 70 = 83 microdollars.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	proxy := dollarAndTokenProxy(t, upstream.URL, 1_000_000, 50.00)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	statuses, err := proxy.store.StatusAll("researcher")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if statuses[0].Used != 12 {
		t.Errorf("tokens used = %d, want 12 (reconciled to actual)", statuses[0].Used)
	}
	// The dollar budget MUST reconcile to its actual 83 microdollars, NOT remain
	// stuck at the much larger reserved estimate (the Session 6 positional bug).
	if statuses[1].Used != 83 {
		t.Errorf("dollars used = %d microdollars, want 83 (reconciled, not stuck at reserve)", statuses[1].Used)
	}
}

func TestServeHTTP_UnknownModelPricedAndForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	// Unknown model with a generous dollar budget: priced at the max known rate and
	// forwarded (not rejected). The dollar budget still reconciles to a non-zero cost.
	proxy := dollarAndTokenProxy(t, upstream.URL, 1_000_000, 50.00)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"some-unreleased-model","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown model is priced, not rejected)", recorder.Code)
	}
	statuses, err := proxy.store.StatusAll("researcher")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	// Unknown model is priced at the max known rate. maxKnownPrice maxes each half
	// independently across the pricing table, so input is gpt-4 (30_000_000 per
	// million) and output is claude-3-opus (75_000_000 per million). Actual usage
	// 5 input + 7 output: ceil(5*30_000_000/1e6) + ceil(7*75_000_000/1e6) =
	// 150 + 525 = 675 microdollars.
	if statuses[1].Used != 675 {
		t.Errorf("dollars used = %d microdollars, want 675 (unknown model priced at max known rate)", statuses[1].Used)
	}
}
