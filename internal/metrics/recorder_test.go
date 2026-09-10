package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecorder_NilReceiverIsNoOp(t *testing.T) {
	var recorder *Recorder
	recorder.RecordForfeit("agent-a", "openai", "usage_missing")
	recorder.RecordUsageMissing("agent-a", "openai")
	recorder.ObserveDrift("agent-a", "openai", 0.5)
	recorder.RecordNegativeBudget("agent-a")
	recorder.RecordSSEParseError("agent-a", "openai")
	recorder.RecordStreamReadTimeout("agent-a", "openai")
	recorder.RecordStreamUpstreamDrop("agent-a", "openai")
	recorder.RecordObserveBreach("agent-a")
	recorder.RecordTiktokenFallback("agent-a", "openai")
	recorder.RecordReconcileError("agent-a", "reconcile")
}

func TestRecorder_PreInitializesSeriesToZero(t *testing.T) {
	recorder := New([]string{"agent-a"}, []string{"openai"})
	expected := `
# HELP levee_usage_missing_total Successful provider responses that carried no usage field (the reservation was forfeited).
# TYPE levee_usage_missing_total counter
levee_usage_missing_total{agent="agent-a",provider="openai"} 0
levee_usage_missing_total{agent="unknown",provider="openai"} 0
`
	if err := testutil.GatherAndCompare(recorder.Gatherer(), strings.NewReader(expected), "levee_usage_missing_total"); err != nil {
		t.Fatalf("pre-initialized series mismatch: %v", err)
	}
}

func TestRecorder_ForfeitCounterIncrements(t *testing.T) {
	recorder := New([]string{"agent-a"}, []string{"openai"})
	recorder.RecordForfeit("agent-a", "openai", "idle_timeout")
	recorder.RecordForfeit("agent-a", "openai", "idle_timeout")
	value := testutil.ToFloat64(recorder.forfeit.WithLabelValues("agent-a", "openai", "idle_timeout"))
	if value != 2 {
		t.Fatalf("forfeit counter = %v, want 2", value)
	}
}

func TestRecorder_DriftHistogramObserves(t *testing.T) {
	recorder := New([]string{"agent-a"}, []string{"openai"})
	recorder.ObserveDrift("agent-a", "openai", -0.2)
	count := testutil.CollectAndCount(recorder.estimationDrift, "levee_estimation_drift")
	if count == 0 {
		t.Fatal("expected at least one drift series after observing")
	}
}

func TestRecorder_HandlerServesPrometheusText(t *testing.T) {
	recorder := New([]string{"agent-a"}, []string{"openai"})
	server := httptest.NewServer(recorder.Handler())
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("scrape failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", response.StatusCode)
	}
}
