// Package metrics defines the Prometheus collectors Levee emits and the
// registry that serves them on the admin port.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// UnknownAgent is the agent label value for requests that carried no
// resolvable agent identity. It keeps the label set bounded (configured
// agent names plus this one value).
const UnknownAgent = "unknown"

// forfeitReasons is the compile-time set of levee_forfeit_total reason label
// values. "unsettled" is the panic-path default and should never appear, which
// makes it alertable. The two release reasons (request_build_failed,
// not_connected) are intentionally absent, they deduct nothing.
var forfeitReasons = []string{
	"usage_missing",
	"client_disconnect",
	"idle_timeout",
	"sse_error",
	"empty_stream",
	"pre_response_timeout",
	"response_read_error",
	"response_too_large",
	"unsettled",
}

// reconcileOperations is the operation label set for levee_reconcile_error_total.
var reconcileOperations = []string{"reconcile", "track", "forfeit"}

// driftBuckets span the estimation drift ratio. The floor is -1.0 (actual
// usage of zero), the ceiling is open (+Inf catches pathological
// under-estimates). Denser near zero, where a healthy estimator lives.
var driftBuckets = []float64{-1, -0.5, -0.25, -0.1, -0.05, 0, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Recorder owns every Levee collector on a dedicated registry. A nil
// *Recorder is a valid no-op receiver so tests can construct a Proxy without
// metrics. All label values come from bounded sets and every known
// combination is pre-initialized at construction so rate() sees a series
// before the first event.
type Recorder struct {
	registry *prometheus.Registry

	usageMissing       *prometheus.CounterVec
	sseParseError      *prometheus.CounterVec
	estimationDrift    *prometheus.HistogramVec
	forfeit            *prometheus.CounterVec
	negativeBudget     *prometheus.CounterVec
	streamReadTimeout  *prometheus.CounterVec
	streamUpstreamDrop *prometheus.CounterVec
	observeBreach      *prometheus.CounterVec
	tiktokenFallback   *prometheus.CounterVec
	reconcileError     *prometheus.CounterVec
}

// New builds the Recorder, registers every collector plus the standard Go and
// process collectors, and pre-initializes all known label combinations.
func New(agentNames, providerNames []string) *Recorder {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	recorder := &Recorder{
		registry: registry,
		usageMissing: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_usage_missing_total",
			Help: "Successful provider responses that carried no usage field (the reservation was forfeited).",
		}, []string{"agent", "provider"}),
		sseParseError: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_sse_parse_error_total",
			Help: "Streams ended by a scanner error (transport read failure or line over the scan cap). SSE payload syntax is not validated, so malformed JSON inside events does not increment this.",
		}, []string{"agent", "provider"}),
		estimationDrift: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "levee_estimation_drift",
			Help:    "(actual - estimated) / estimated for settlements with authoritative usage only. Observations can be negative, so the _sum series is non-monotone, use bucket quantiles, not rate(_sum)/rate(_count).",
			Buckets: driftBuckets,
		}, []string{"agent", "provider"}),
		forfeit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_forfeit_total",
			Help: "Reservations forfeited (full reserved estimate committed), by reason. Increments only after the store operation succeeded.",
		}, []string{"agent", "provider", "reason"}),
		negativeBudget: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_negative_budget_total",
			Help: "Settlements that pushed committed usage past the limit (transition, not level). Enforce-path settlements only.",
		}, []string{"agent"}),
		streamReadTimeout: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_stream_read_timeout_total",
			Help: "Streams ended by the idle watchdog.",
		}, []string{"agent", "provider"}),
		streamUpstreamDrop: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_stream_upstream_drop_total",
			Help: "Streams that reached EOF without a terminal marker. Not a forfeit duplicate, a dropped stream with content still reconciles via fallback.",
		}, []string{"agent", "provider"}),
		observeBreach: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_observe_breach_total",
			Help: "Budget breaches in observe mode (request forwarded anyway).",
		}, []string{"agent"}),
		tiktokenFallback: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_tiktoken_fallback_total",
			Help: "Settlements where at least one usage half was estimated rather than provider-reported.",
		}, []string{"agent", "provider"}),
		reconcileError: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "levee_reconcile_error_total",
			Help: "Budget store operations that failed during settlement, by operation.",
		}, []string{"agent", "operation"}),
	}

	registry.MustRegister(
		recorder.usageMissing,
		recorder.sseParseError,
		recorder.estimationDrift,
		recorder.forfeit,
		recorder.negativeBudget,
		recorder.streamReadTimeout,
		recorder.streamUpstreamDrop,
		recorder.observeBreach,
		recorder.tiktokenFallback,
		recorder.reconcileError,
	)

	labelAgents := append(append([]string{}, agentNames...), UnknownAgent)
	for _, agentName := range labelAgents {
		recorder.negativeBudget.WithLabelValues(agentName)
		recorder.observeBreach.WithLabelValues(agentName)
		for _, operation := range reconcileOperations {
			recorder.reconcileError.WithLabelValues(agentName, operation)
		}
		for _, providerName := range providerNames {
			recorder.usageMissing.WithLabelValues(agentName, providerName)
			recorder.sseParseError.WithLabelValues(agentName, providerName)
			recorder.estimationDrift.WithLabelValues(agentName, providerName)
			recorder.streamReadTimeout.WithLabelValues(agentName, providerName)
			recorder.streamUpstreamDrop.WithLabelValues(agentName, providerName)
			recorder.tiktokenFallback.WithLabelValues(agentName, providerName)
			for _, reason := range forfeitReasons {
				recorder.forfeit.WithLabelValues(agentName, providerName, reason)
			}
		}
	}
	return recorder
}

// Handler returns the /metrics scrape handler for the admin mux.
func (recorder *Recorder) Handler() http.Handler {
	return promhttp.HandlerFor(recorder.registry, promhttp.HandlerOpts{})
}

// Gatherer exposes the registry for test assertions.
func (recorder *Recorder) Gatherer() prometheus.Gatherer {
	return recorder.registry
}

// RecordForfeit counts a successful forfeit by reason.
func (recorder *Recorder) RecordForfeit(agentName, providerName, reason string) {
	if recorder == nil {
		return
	}
	recorder.forfeit.WithLabelValues(agentName, providerName, reason).Inc()
}

// RecordUsageMissing counts a 2xx response with no usage field.
func (recorder *Recorder) RecordUsageMissing(agentName, providerName string) {
	if recorder == nil {
		return
	}
	recorder.usageMissing.WithLabelValues(agentName, providerName).Inc()
}

// ObserveDrift records estimation drift for an authoritative settlement.
func (recorder *Recorder) ObserveDrift(agentName, providerName string, drift float64) {
	if recorder == nil {
		return
	}
	recorder.estimationDrift.WithLabelValues(agentName, providerName).Observe(drift)
}

// RecordNegativeBudget counts a committed-usage limit crossing.
func (recorder *Recorder) RecordNegativeBudget(agentName string) {
	if recorder == nil {
		return
	}
	recorder.negativeBudget.WithLabelValues(agentName).Inc()
}

// RecordSSEParseError counts a stream ended by a scanner error.
func (recorder *Recorder) RecordSSEParseError(agentName, providerName string) {
	if recorder == nil {
		return
	}
	recorder.sseParseError.WithLabelValues(agentName, providerName).Inc()
}

// RecordStreamReadTimeout counts an idle-watchdog fire.
func (recorder *Recorder) RecordStreamReadTimeout(agentName, providerName string) {
	if recorder == nil {
		return
	}
	recorder.streamReadTimeout.WithLabelValues(agentName, providerName).Inc()
}

// RecordStreamUpstreamDrop counts a clean EOF with no terminal marker.
func (recorder *Recorder) RecordStreamUpstreamDrop(agentName, providerName string) {
	if recorder == nil {
		return
	}
	recorder.streamUpstreamDrop.WithLabelValues(agentName, providerName).Inc()
}

// RecordObserveBreach counts an observe-mode budget breach at admission.
func (recorder *Recorder) RecordObserveBreach(agentName string) {
	if recorder == nil {
		return
	}
	recorder.observeBreach.WithLabelValues(agentName).Inc()
}

// RecordTiktokenFallback counts a settlement that used estimated usage.
func (recorder *Recorder) RecordTiktokenFallback(agentName, providerName string) {
	if recorder == nil {
		return
	}
	recorder.tiktokenFallback.WithLabelValues(agentName, providerName).Inc()
}

// RecordReconcileError counts a failed store operation during settlement.
func (recorder *Recorder) RecordReconcileError(agentName, operation string) {
	if recorder == nil {
		return
	}
	recorder.reconcileError.WithLabelValues(agentName, operation).Inc()
}
