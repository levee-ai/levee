package proxy

import "testing"

func TestReconcileForStream(t *testing.T) {
	cases := []struct {
		name       string
		state      *streamState
		wantAction reconcileAction
		wantTokens int64
	}{
		{
			name:       "normal with full usage reconciles to actual",
			state:      &streamState{provider: providerOpenAI, endReason: endNormal, sawAuthoritativeUsage: true, inputTokens: 29, outputTokens: 11},
			wantAction: actionReconcile,
			wantTokens: 40,
		},
		{
			name:       "upstream drop with full usage still reconciles to actual",
			state:      &streamState{provider: providerOpenAI, endReason: endUpstreamDrop, sawAuthoritativeUsage: true, inputTokens: 29, outputTokens: 11},
			wantAction: actionReconcile,
			wantTokens: 40,
		},
		{
			name:       "client disconnect always forfeits even with usage",
			state:      &streamState{provider: providerOpenAI, endReason: endClientDisconnect, sawAuthoritativeUsage: true, inputTokens: 29, outputTokens: 11},
			wantAction: actionForfeit,
		},
		{
			name:       "idle timeout always forfeits",
			state:      &streamState{provider: providerOpenAI, endReason: endIdleTimeout, contentBytes: 100},
			wantAction: actionForfeit,
		},
		{
			name:       "scan error always forfeits",
			state:      &streamState{provider: providerOpenAI, endReason: endScanError, contentBytes: 100},
			wantAction: actionForfeit,
		},
		{
			name:       "clean EOF no usage but content uses heuristic",
			state:      &streamState{provider: providerAnthropic, endReason: endUpstreamDrop, inputTokens: 25, contentBytes: 40},
			wantAction: actionReconcile,
			wantTokens: 35, // input 25 + ceil(40/4)=10
		},
		{
			name:       "clean EOF no usage no content forfeits",
			state:      &streamState{provider: providerOpenAI, endReason: endUpstreamDrop, contentBytes: 0},
			wantAction: actionForfeit,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			estimator := newReconcileEstimatorStub()
			outcome := reconcileForStream(testCase.state, estimator, nil)
			if outcome.action != testCase.wantAction {
				t.Errorf("action = %v, want %v", outcome.action, testCase.wantAction)
			}
			if testCase.wantAction == actionReconcile && outcome.actualTokens != testCase.wantTokens {
				t.Errorf("actualTokens = %d, want %d", outcome.actualTokens, testCase.wantTokens)
			}
		})
	}
}

func TestReconcileForResponse_NonStreaming(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		provider   string
		wantAction reconcileAction
		wantTokens int64
	}{
		{"2xx with usage reconciles", 200, `{"usage":{"total_tokens":40}}`, providerOpenAI, actionReconcile, 40},
		{"2xx with split usage reconciles to sum", 200, `{"usage":{"prompt_tokens":10,"completion_tokens":30}}`, providerOpenAI, actionReconcile, 40},
		{"2xx without usage forfeits", 200, `{"id":"x"}`, providerOpenAI, actionForfeit, 0},
		{"provider 500 releases nothing", 500, `{"error":"boom"}`, providerOpenAI, actionReconcile, 0},
		{"provider 429 releases nothing", 429, `{"error":"slow down"}`, providerOpenAI, actionReconcile, 0},
		{"provider 400 releases nothing", 400, `{"error":"bad"}`, providerOpenAI, actionReconcile, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			outcome := reconcileForResponse(testCase.provider, testCase.status, []byte(testCase.body))
			if outcome.action != testCase.wantAction {
				t.Errorf("action = %v, want %v", outcome.action, testCase.wantAction)
			}
			if outcome.actualTokens != testCase.wantTokens {
				t.Errorf("actualTokens = %d, want %d", outcome.actualTokens, testCase.wantTokens)
			}
		})
	}
}

// TestReconcileForStream_PartialUsageBackfills guards against the under-count
// where a provider delivers a usage object with one half missing. Trusting the
// captured sum would commit less than was consumed, the one direction 001 and
// Tenet 3 forbid. The missing half must be backfilled from the estimator (input)
// or the content-byte heuristic (output). See 002 lines 369 to 374.
func TestReconcileForStream_PartialUsageBackfills(t *testing.T) {
	const estimatorInput = 50

	cases := []struct {
		name       string
		state      *streamState
		wantTokens int64
		wantReason string
	}{
		{
			// OpenAI chunk with prompt_tokens but no completion_tokens, plus
			// content. Output must be filled from the heuristic, not left at 0.
			name:       "openai missing completion backfills output from content",
			state:      &streamState{provider: providerOpenAI, endReason: endNormal, sawAuthoritativeUsage: true, inputTokens: 29, outputTokens: 0, contentBytes: 400},
			wantTokens: 29 + 100, // input 29 (authoritative) + ceil(400/4)=100 (heuristic)
			wantReason: "tiktoken_fallback",
		},
		{
			// Anthropic with output_tokens but no input_tokens. Input must be
			// filled from the estimator, not dropped.
			name:       "anthropic missing input backfills from estimator",
			state:      &streamState{provider: providerAnthropic, endReason: endNormal, sawAuthoritativeUsage: true, inputTokens: 0, outputTokens: 15, contentBytes: 200},
			wantTokens: estimatorInput + 15, // input 50 (estimated) + output 15 (authoritative)
			wantReason: "tiktoken_fallback",
		},
		{
			// Both halves authoritative: no estimation, exact reconcile.
			name:       "complete usage reconciles exactly",
			state:      &streamState{provider: providerOpenAI, endReason: endNormal, sawAuthoritativeUsage: true, inputTokens: 29, outputTokens: 11, contentBytes: 400},
			wantTokens: 40,
			wantReason: "reconciled",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			outcome := reconcileForStream(testCase.state, stubEstimator{value: estimatorInput}, []byte(`{"model":"x"}`))
			if outcome.action != actionReconcile {
				t.Fatalf("action = %v, want actionReconcile", outcome.action)
			}
			if outcome.actualTokens != testCase.wantTokens {
				t.Errorf("actualTokens = %d, want %d (a smaller value is an under-count)", outcome.actualTokens, testCase.wantTokens)
			}
			if outcome.reason != testCase.wantReason {
				t.Errorf("reason = %q, want %q", outcome.reason, testCase.wantReason)
			}
		})
	}
}

// TestTrackOutcomeForStream_PartialUsageBackfills is the observe-mode mirror of
// the partial-usage backfill: a clean-EOF observe breach with a half-missing
// usage object must still Track the backfilled total, not the truncated sum.
func TestTrackOutcomeForStream_PartialUsageBackfills(t *testing.T) {
	state := &streamState{provider: providerOpenAI, endReason: endNormal, sawAuthoritativeUsage: true, inputTokens: 29, outputTokens: 0, contentBytes: 400}
	outcome := trackOutcomeForStream(state, []byte(`{"model":"x"}`), stubEstimator{value: 50})
	if outcome.action != actionTrack {
		t.Fatalf("action = %v, want actionTrack", outcome.action)
	}
	if outcome.actualTokens != 29+100 {
		t.Errorf("actualTokens = %d, want 129 (29 authoritative input + 100 heuristic output)", outcome.actualTokens)
	}
}

type stubEstimator struct{ value int64 }

func (stub stubEstimator) EstimateInput(_ string, _ []byte) int64 { return stub.value }

func newReconcileEstimatorStub() inputEstimator { return stubEstimator{value: 0} }
