package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levee-ai/levee/internal/metrics"
	"github.com/levee-ai/levee/pkg/types"
	dto "github.com/prometheus/client_model/go"
)

// withRecorder attaches a fresh metrics.Recorder scoped to the given agent and
// provider names to an already-built test proxy. A thin wrapper over the
// enforcement_test.go builders (enforcingProxy, observingProxy,
// dollarAndTokenProxy, newPassthroughTestProxy) so every test in this file
// gets its own isolated registry, one test's counters can never bleed into
// another's.
func withRecorder(proxy *Proxy, agentNames, providerNames []string) *Proxy {
	proxy.recorder = metrics.New(agentNames, providerNames)
	return proxy
}

// metricLabelsMatch reports whether metric carries every label in match with
// the exact given value. A label on metric that is absent from match is
// ignored, so a partial match set (for example just "agent") sums across
// every pre-initialized value of the dimension left unspecified, every
// reason or every provider.
func metricLabelsMatch(metric *dto.Metric, match map[string]string) bool {
	for wantName, wantValue := range match {
		found := false
		for _, label := range metric.GetLabel() {
			if label.GetName() == wantName {
				found = label.GetValue() == wantValue
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// counterSum gathers the named counter family from recorder and sums the
// values of every series whose labels are a superset of match. Every counter
// family here is fully pre-initialized at construction (recorder.go), so a
// metric that never fired sums to exactly 0 rather than being absent, and a
// fully specified label set (agent, provider, reason) isolates one series.
func counterSum(t *testing.T, recorder *metrics.Recorder, metricName string, match map[string]string) float64 {
	t.Helper()
	families, err := recorder.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather %s: %v", metricName, err)
	}
	var sum float64
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metricLabelsMatch(metric, match) {
				sum += metric.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// histogramSampleCount returns the sample count of the single histogram series
// whose labels match every entry in match exactly. Fails the test if no such
// series exists, since every label combination this package uses is
// pre-initialized at construction.
func histogramSampleCount(t *testing.T, recorder *metrics.Recorder, metricName string, match map[string]string) uint64 {
	t.Helper()
	families, err := recorder.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather %s: %v", metricName, err)
	}
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metricLabelsMatch(metric, match) {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("no %s series matching %v", metricName, match)
	return 0
}

// TestMetrics_ReconciledResponseObservesDrift covers the ordinary enforce-mode
// settlement: a non-streaming response with a full usage split reconciles
// with reason "reconciled", which is the only reason that feeds the drift
// histogram. Nothing here forfeits, so every forfeit series for the agent
// must stay at its pre-initialized 0.
func TestMetrics_ReconciledResponseObservesDrift(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	match := map[string]string{"agent": "researcher", "provider": "openai"}
	if count := histogramSampleCount(t, proxy.recorder, "levee_estimation_drift", match); count != 1 {
		t.Errorf("drift sample count = %d, want 1", count)
	}
	if forfeited := counterSum(t, proxy.recorder, "levee_forfeit_total", map[string]string{"agent": "researcher"}); forfeited != 0 {
		t.Errorf("forfeit total for researcher = %v, want 0", forfeited)
	}
}

// TestMetrics_ProviderRefusalDoesNotObserveDrift covers a provider refusal
// (5xx). reconcileForResponse releases the reservation with reason
// "provider_refused", which is neither "reconciled" nor "tiktoken_fallback",
// so the drift histogram must stay untouched.
func TestMetrics_ProviderRefusalDoesNotObserveDrift(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer upstream.Close()

	proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	match := map[string]string{"agent": "researcher", "provider": "openai"}
	if count := histogramSampleCount(t, proxy.recorder, "levee_estimation_drift", match); count != 0 {
		t.Errorf("drift sample count = %d, want 0", count)
	}
}

// TestMetrics_UsageMissingForfeits covers a 2xx response with no usage field,
// which forfeits the full reservation with reason "usage_missing" and must
// also increment the dedicated usage-missing counter.
func TestMetrics_UsageMissingForfeits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
	}))
	defer upstream.Close()

	proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	forfeitMatch := map[string]string{"agent": "researcher", "provider": "openai", "reason": "usage_missing"}
	if got := counterSum(t, proxy.recorder, "levee_forfeit_total", forfeitMatch); got != 1 {
		t.Errorf(`levee_forfeit_total{reason="usage_missing"} = %v, want 1`, got)
	}
	usageMissingMatch := map[string]string{"agent": "researcher", "provider": "openai"}
	if got := counterSum(t, proxy.recorder, "levee_usage_missing_total", usageMissingMatch); got != 1 {
		t.Errorf("levee_usage_missing_total = %v, want 1", got)
	}
}

// TestMetrics_NegativeCrossingFiresOnceAtTransition proves that
// levee_negative_budget_total counts a crossing, a transition from
// at-or-under the limit to over it, not a level, staying over the limit on a
// later settle.
//
// Construction chosen and why: enforce-mode admission blocks any further
// reservation on an agent once its committed usage exceeds the limit,
// because remaining() (Limit - used - reserved) goes negative and Admit
// rejects any non-negative amount against a negative remaining. That means a
// second full ServeHTTP round trip on the same over-budget agent can never
// reach applyReconcile at all: enforce() writes the 429 and returns before
// the settlement defer is even registered. Neither alternative the task
// description raised avoids this cleanly. Routing the second phase through
// an observe-mode agent does not exercise the invariant under test: observe
// breaches settle through Store.TrackMulti, which calls the plain commit(),
// never commitDetectingCrossing, so it could not increment this counter a
// second time regardless of whether the invariant held. Using two separate
// agents proves the counter is per-agent, not that a SECOND commit on the
// SAME already-over-limit window is a no-op, which is the actual claim
// "transition, not level" makes.
//
// So this test drives both reservations through the real admission call
// (store.ReserveMulti, the same call Admit makes) while used() is still 0,
// which both reservations pass, then settles them in sequence through
// applyReconcile, the exact method ServeHTTP's defer calls. The first
// settle's actual usage (30) pushes committed usage from 0 (at-or-under the
// 20 token limit) to 30 (over it): the transition, counter goes to 1. The
// second settle, on the still-held second reservation, commits one more
// token while usedBefore is already 30 (already over 20), so
// commitDetectingCrossing's usedBefore<=Limit guard is false and the counter
// must not move again. This is the real store and the real settlement
// method, only the two reservations are opened directly rather than through
// two overlapping in-flight ServeHTTP calls, which would need actual
// concurrency to keep both alive across the first settle for no additional
// coverage of the code path under test.
func TestMetrics_NegativeCrossingFiresOnceAtTransition(t *testing.T) {
	proxy := withRecorder(enforcingProxy(t, "http://unused.invalid", 20), []string{"researcher"}, []string{"openai"})

	reservationA, admittedA, err := proxy.store.ReserveMulti("researcher", []int64{5})
	if err != nil || !admittedA {
		t.Fatalf("reserve A: admitted=%v err=%v", admittedA, err)
	}
	reservationB, admittedB, err := proxy.store.ReserveMulti("researcher", []int64{5})
	if err != nil || !admittedB {
		t.Fatalf("reserve B: admitted=%v err=%v", admittedB, err)
	}

	// First settle: actual usage 30 (input 20 + output 10) crosses the 20
	// token limit. usedBefore is 0, at-or-under the limit, so this is a
	// transition.
	proxy.applyReconcile("openai", "researcher", reservationA, "gpt-4", []string{"tokens"}, 5,
		reconcileOutcome{action: actionReconcile, inputTokens: 20, outputTokens: 10, reason: "reconciled"})
	match := map[string]string{"agent": "researcher"}
	if got := counterSum(t, proxy.recorder, "levee_negative_budget_total", match); got != 1 {
		t.Fatalf("after first settle: levee_negative_budget_total = %v, want 1", got)
	}

	// Second settle: usedBefore is already 30, over the 20 token limit, so
	// this must not fire again regardless of the outcome after the commit.
	proxy.applyReconcile("openai", "researcher", reservationB, "gpt-4", []string{"tokens"}, 5,
		reconcileOutcome{action: actionReconcile, inputTokens: 1, outputTokens: 0, reason: "reconciled"})
	if got := counterSum(t, proxy.recorder, "levee_negative_budget_total", match); got != 1 {
		t.Fatalf("after second settle: levee_negative_budget_total = %v, want 1 (a level, not a new transition)", got)
	}
}

// TestMetrics_ObserveBreachCounts covers an observe-mode agent whose budget is
// already exhausted: enforce() forwards the request anyway and must count the
// breach at admission time.
func TestMetrics_ObserveBreachCounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	proxy := withRecorder(observingProxy(t, upstream.URL, 100), []string{"researcher"}, []string{"openai"})
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (observe mode forwards on breach)", recorder.Code)
	}
	if got := counterSum(t, proxy.recorder, "levee_observe_breach_total", map[string]string{"agent": "researcher"}); got != 1 {
		t.Errorf("levee_observe_breach_total = %v, want 1", got)
	}
}

// TestMetrics_ReconcileErrorOnPoisonedStore covers all three store-call
// failure branches inside applyReconcile (reconcile, forfeit, track), each
// counted by operation on levee_reconcile_error_total. applyReconcile is
// called directly in every subcase: it is the exact method ServeHTTP's defer
// calls, so this exercises the real error branches without fabricating
// anything about the store's behavior. Each subtest builds its own proxy and
// recorder so the three are fully isolated from each other.
func TestMetrics_ReconcileErrorOnPoisonedStore(t *testing.T) {
	t.Run("reconcile", func(t *testing.T) {
		// The simplest honest way to force ReconcileMulti's "unknown
		// reservation" error, without racing a real reservation's release
		// against a second lookup, is a reservation ID the store has never
		// issued at all.
		proxy := withRecorder(enforcingProxy(t, "http://unused.invalid", 1000000), []string{"researcher"}, []string{"openai"})
		const bogusReservationID = types.ReservationID(999999)
		proxy.applyReconcile("openai", "researcher", bogusReservationID, "gpt-4", []string{"tokens"}, 10,
			reconcileOutcome{action: actionReconcile, inputTokens: 5, outputTokens: 5, reason: "reconciled"})

		match := map[string]string{"agent": "researcher", "operation": "reconcile"}
		if got := counterSum(t, proxy.recorder, "levee_reconcile_error_total", match); got != 1 {
			t.Errorf(`levee_reconcile_error_total{operation="reconcile"} = %v, want 1`, got)
		}
	})

	t.Run("forfeit", func(t *testing.T) {
		// Same bogus-reservation construction as the reconcile subcase, but
		// with an actionForfeit outcome so Store.Forfeit is the one that
		// sees the unknown reservation.
		proxy := withRecorder(enforcingProxy(t, "http://unused.invalid", 1000000), []string{"researcher"}, []string{"openai"})
		const bogusReservationID = types.ReservationID(999999)
		proxy.applyReconcile("openai", "researcher", bogusReservationID, "gpt-4", []string{"tokens"}, 10,
			reconcileOutcome{action: actionForfeit, reason: "idle_timeout"})

		match := map[string]string{"agent": "researcher", "operation": "forfeit"}
		if got := counterSum(t, proxy.recorder, "levee_reconcile_error_total", match); got != 1 {
			t.Errorf(`levee_reconcile_error_total{operation="forfeit"} = %v, want 1`, got)
		}
	})

	t.Run("track", func(t *testing.T) {
		// TrackMulti fails a lookup on the agent name before it ever reaches
		// budget math, so any agent name absent from the store's config
		// triggers it, no reservation involved at all (Track never takes
		// one). The recorder is scoped to that same absent name so its
		// pre-initialized series exists to sum.
		proxy := withRecorder(enforcingProxy(t, "http://unused.invalid", 1000000), []string{"ghost-agent"}, []string{"openai"})
		proxy.applyReconcile("openai", "ghost-agent", 0, "gpt-4", []string{"tokens"}, 10,
			reconcileOutcome{action: actionTrack, inputTokens: 5, outputTokens: 5, reason: "observe_track"})

		match := map[string]string{"agent": "ghost-agent", "operation": "track"}
		if got := counterSum(t, proxy.recorder, "levee_reconcile_error_total", match); got != 1 {
			t.Errorf(`levee_reconcile_error_total{operation="track"} = %v, want 1`, got)
		}
	})
}

// TestMetrics_ForfeitCrossingFiresOnTransition covers the forfeit arm's
// crossing detection (RecordNegativeBudget on Store.Forfeit's crossed bool),
// the coverage gap TestMetrics_NegativeCrossingFiresOnceAtTransition (the
// reconcile arm) left open.
//
// Forfeit always commits exactly its own reservation's original amount, it
// has no "actual exceeds estimate" drift the way reconcile does, so a SINGLE
// reservation can never cross on forfeit alone: Admit already required that
// reservation's amount to fit within Limit-used-reserved at admission time,
// so committing exactly that amount caps used() at Limit, never over it.
//
// To make the forfeit arm itself the one that crosses, a second concurrent
// reservation (Y) settles first with an ACTUAL that exceeds its own reserved
// estimate, the same drift mechanism TestMetrics_NegativeCrossingFiresOnceAtTransition
// uses, which inflates used() beyond what reservation X's slot assumed when
// X was admitted. X then forfeits its original, still-valid amount, and the
// cumulative used() ends up over the limit at that exact commit, so this is
// the forfeit arm's own crossing, not one inherited from Y's settle.
func TestMetrics_ForfeitCrossingFiresOnTransition(t *testing.T) {
	proxy := withRecorder(enforcingProxy(t, "http://unused.invalid", 20), []string{"researcher"}, []string{"openai"})

	reservationY, admittedY, err := proxy.store.ReserveMulti("researcher", []int64{15})
	if err != nil || !admittedY {
		t.Fatalf("reserve Y: admitted=%v err=%v", admittedY, err)
	}
	reservationX, admittedX, err := proxy.store.ReserveMulti("researcher", []int64{5})
	if err != nil || !admittedX {
		t.Fatalf("reserve X: admitted=%v err=%v", admittedX, err)
	}

	// Y settles for more than its own reserved estimate (actual 18 against a
	// reservation of 15), pushing used to 18, still at-or-under the 20 token
	// limit, so Y's own settle must not cross.
	proxy.applyReconcile("openai", "researcher", reservationY, "gpt-4", []string{"tokens"}, 15,
		reconcileOutcome{action: actionReconcile, inputTokens: 10, outputTokens: 8, reason: "reconciled"})
	match := map[string]string{"agent": "researcher"}
	if got := counterSum(t, proxy.recorder, "levee_negative_budget_total", match); got != 0 {
		t.Fatalf("after Y's settle: levee_negative_budget_total = %v, want 0", got)
	}

	// X forfeits its full, still-valid 5 token reservation. usedBefore (18)
	// is at-or-under the limit, and used after (23) is over it: the forfeit
	// arm's own commit is the transition.
	proxy.applyReconcile("openai", "researcher", reservationX, "gpt-4", []string{"tokens"}, 5,
		reconcileOutcome{action: actionForfeit, reason: "idle_timeout"})
	if got := counterSum(t, proxy.recorder, "levee_negative_budget_total", match); got != 1 {
		t.Fatalf("after X's forfeit: levee_negative_budget_total = %v, want 1", got)
	}
}

// TestMetrics_StreamSignals covers the three stream-lifecycle signal metrics
// emitted in the ServeHTTP streaming branch: an idle-watchdog fire, a clean
// EOF that never reached a terminal marker, and a scanner error from an
// oversized line. Each subcase reuses upstream shapes already exercised for
// the underlying streamResponse behavior in enforcement_test.go and
// proxy_test.go, driven here through the full ServeHTTP path so the metric
// emission site itself is under test, not just streamResponse's classification.
func TestMetrics_StreamSignals(t *testing.T) {
	t.Run("idle timeout", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
			flusher.Flush()
			<-r.Context().Done()
		}))
		defer upstream.Close()

		proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
		tightIdle := providerTimeouts{connect: 5 * time.Second, responseHeader: 5 * time.Second, idle: 150 * time.Millisecond, request: 5 * time.Second}
		proxy.providers["openai"] = newProviderTarget(upstream.URL, tightIdle)

		request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Levee-Agent", "researcher")
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)

		match := map[string]string{"agent": "researcher", "provider": "openai"}
		if got := counterSum(t, proxy.recorder, "levee_stream_read_timeout_total", match); got != 1 {
			t.Errorf("levee_stream_read_timeout_total = %v, want 1", got)
		}
	})

	t.Run("upstream drop", func(t *testing.T) {
		ssePayload := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"some words here"}}],"usage":null}`, "",
		}, "\n")
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(ssePayload))
		}))
		defer upstream.Close()

		proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
		request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Levee-Agent", "researcher")
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)

		match := map[string]string{"agent": "researcher", "provider": "openai"}
		if got := counterSum(t, proxy.recorder, "levee_stream_upstream_drop_total", match); got != 1 {
			t.Errorf("levee_stream_upstream_drop_total = %v, want 1", got)
		}
	})

	t.Run("scan error", func(t *testing.T) {
		hugeLine := "data: " + strings.Repeat("x", 5*1024*1024) + "\n\n"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte(hugeLine))
		}))
		defer upstream.Close()

		proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
		request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Levee-Agent", "researcher")
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)

		match := map[string]string{"agent": "researcher", "provider": "openai"}
		if got := counterSum(t, proxy.recorder, "levee_sse_parse_error_total", match); got != 1 {
			t.Errorf("levee_sse_parse_error_total = %v, want 1", got)
		}
	})
}

// TestMetrics_UnresolvedAgentStreamSignalUsesUnknownLabel covers the bounded
// label contract for a request that never resolved to a configured agent.
// enforced.agentName stays "" all the way to the streaming branch, and
// metricAgentLabel must fold that into metrics.UnknownAgent rather than
// letting an empty string become a new, unbounded label value. newTestProxy
// (proxy_test.go) configures zero agents and unknownAgent "passthrough", so
// every request is unresolved and still forwarded, exactly this shape.
func TestMetrics_UnresolvedAgentStreamSignalUsesUnknownLabel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	proxy := withRecorder(newTestProxy(t, upstream.URL), nil, []string{"openai"})
	tightIdle := providerTimeouts{connect: 5 * time.Second, responseHeader: 5 * time.Second, idle: 150 * time.Millisecond, request: 5 * time.Second}
	proxy.providers["openai"] = newProviderTarget(upstream.URL, tightIdle)

	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	// No X-Levee-Agent header: newTestProxy configures no agents at all, so
	// resolution fails and unknownAgent "passthrough" forwards anyway.
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	match := map[string]string{"agent": metrics.UnknownAgent, "provider": "openai"}
	if got := counterSum(t, proxy.recorder, "levee_stream_read_timeout_total", match); got != 1 {
		t.Errorf("levee_stream_read_timeout_total{agent=%s} = %v, want 1", metrics.UnknownAgent, got)
	}
}

// TestMetrics_TiktokenFallbackCounts covers a stream that reaches a terminal
// marker with forwarded content but no authoritative usage from the
// provider. composeStreamTokens must backfill both halves from the estimator
// and the content-byte heuristic, which tags the settlement
// "tiktoken_fallback".
func TestMetrics_TiktokenFallbackCounts(t *testing.T) {
	ssePayload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello world"}}],"usage":null}`, "",
		"data: [DONE]", "",
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssePayload))
	}))
	defer upstream.Close()

	proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	match := map[string]string{"agent": "researcher", "provider": "openai"}
	if got := counterSum(t, proxy.recorder, "levee_tiktoken_fallback_total", match); got != 1 {
		t.Errorf("levee_tiktoken_fallback_total = %v, want 1", got)
	}
}

// TestMetrics_EnforceModePairsMoveTogether covers the idle-timeout streaming
// case end to end: the stream-signal counter (fired at the ServeHTTP
// streaming branch) and the forfeit-by-reason counter (fired inside
// applyReconcile) both describe the same idle-timeout event and must move
// together.
func TestMetrics_EnforceModePairsMoveTogether(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	proxy := withRecorder(enforcingProxy(t, upstream.URL, 1000000), []string{"researcher"}, []string{"openai"})
	tightIdle := providerTimeouts{connect: 5 * time.Second, responseHeader: 5 * time.Second, idle: 150 * time.Millisecond, request: 5 * time.Second}
	proxy.providers["openai"] = newProviderTarget(upstream.URL, tightIdle)

	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "researcher")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	streamMatch := map[string]string{"agent": "researcher", "provider": "openai"}
	if got := counterSum(t, proxy.recorder, "levee_stream_read_timeout_total", streamMatch); got != 1 {
		t.Errorf("levee_stream_read_timeout_total = %v, want 1", got)
	}
	forfeitMatch := map[string]string{"agent": "researcher", "provider": "openai", "reason": "idle_timeout"}
	if got := counterSum(t, proxy.recorder, "levee_forfeit_total", forfeitMatch); got != 1 {
		t.Errorf(`levee_forfeit_total{reason="idle_timeout"} = %v, want 1`, got)
	}
}

// TestMetrics_SettleNoneEmitsNoSettlementMetrics is the metrics-parity
// counterpart to TestSettleNone_NeverTouchesStoreOrLogsWarnings (Task 3): a
// configured passthrough agent forces the deferred outcome to actionNone
// inside the closure, which returns from applyReconcile before any store call
// or metric emission. The store-error counter is the correct one to check
// here because it is the only one applyReconcile's actionNone branch could
// possibly have touched.
func TestMetrics_SettleNoneEmitsNoSettlementMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := withRecorder(newPassthroughTestProxy(t, logger), []string{"passthrough-agent"}, []string{"openai"})

	request := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "passthrough-agent")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	for _, operation := range []string{"reconcile", "track", "forfeit"} {
		match := map[string]string{"operation": operation}
		if got := counterSum(t, proxy.recorder, "levee_reconcile_error_total", match); got != 0 {
			t.Errorf("levee_reconcile_error_total{operation=%s} = %v, want 0", operation, got)
		}
	}
}
