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

type stubEstimator struct{ value int64 }

func (stub stubEstimator) EstimateInput(_ string, _ []byte) int64 { return stub.value }

func newReconcileEstimatorStub() inputEstimator { return stubEstimator{value: 0} }
