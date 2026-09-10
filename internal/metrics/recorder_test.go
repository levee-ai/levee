package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
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

	metricFamilies, err := recorder.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather failed: %v", err)
	}
	histogram := findDriftHistogram(t, metricFamilies, "agent-a", "openai")

	if histogram.GetSampleCount() != 1 {
		t.Fatalf("sample count = %d, want 1", histogram.GetSampleCount())
	}
	const wantSampleSum = -0.2
	if difference := histogram.GetSampleSum() - wantSampleSum; difference > 1e-9 || difference < -1e-9 {
		t.Fatalf("sample sum = %v, want %v", histogram.GetSampleSum(), wantSampleSum)
	}
}

// findDriftHistogram locates the levee_estimation_drift series for the given
// agent and provider label pair among gathered metric families, or fails the
// test if no matching series exists.
func findDriftHistogram(t *testing.T, metricFamilies []*dto.MetricFamily, agentName, providerName string) *dto.Histogram {
	t.Helper()
	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() != "levee_estimation_drift" {
			continue
		}
		for _, metric := range metricFamily.GetMetric() {
			if labelValue(metric, "agent") == agentName && labelValue(metric, "provider") == providerName {
				return metric.GetHistogram()
			}
		}
	}
	t.Fatalf("no levee_estimation_drift series for agent=%s provider=%s", agentName, providerName)
	return nil
}

// labelValue returns the value of the named label on metric, or the empty
// string if the metric carries no label with that name.
func labelValue(metric *dto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
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
