// Package proxy implements the HTTP reverse proxy with budget enforcement.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/levee-ai/levee/internal/agent"
	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/internal/config"
	"github.com/levee-ai/levee/internal/tokens"
)

// providerTimeouts holds the parsed phase-split timeouts for a provider (ADR-005).
// idle is stored but not consumed until the Session 6 streaming watchdog.
type providerTimeouts struct {
	connect        time.Duration
	responseHeader time.Duration
	idle           time.Duration
	request        time.Duration
}

// providerTarget holds the upstream URL, the two HTTP clients, and the parsed
// timeouts for a provider. The streaming client sets ResponseHeaderTimeout. The
// non-streaming client leaves it zero (for non-streaming the header wait IS the
// generation, so a header timeout would re-cap it -- see ADR-005). Both clients
// set Client.Timeout=0 so neither severs a body read.
type providerTarget struct {
	upstream           string
	streamingClient    *http.Client
	nonStreamingClient *http.Client
	timeouts           providerTimeouts
}

// newProviderTarget builds a providerTarget with both clients from parsed
// timeouts. Shared by New() and tests so they exercise the same construction.
func newProviderTarget(upstream string, timeouts providerTimeouts) *providerTarget {
	return &providerTarget{
		upstream:           strings.TrimRight(upstream, "/"),
		streamingClient:    newProviderClient(timeouts.connect, timeouts.responseHeader),
		nonStreamingClient: newProviderClient(timeouts.connect, 0),
		timeouts:           timeouts,
	}
}

// newProviderClient builds one client. responseHeader=0 disables the header
// timeout (used for the non-streaming client). Client.Timeout is always 0.
//
// The transport is cloned from http.DefaultTransport so it keeps the stdlib
// defaults (HTTP/2 via ForceAttemptHTTP2, connection pooling, IdleConnTimeout,
// ProxyFromEnvironment) and only the three phase-split timeout fields are
// overridden. Building a bare http.Transport with a custom DialContext would
// silently disable HTTP/2, because a non-nil DialContext makes the stdlib
// conservatively skip the HTTP/2 upgrade unless ForceAttemptHTTP2 is set.
func newProviderClient(connect, responseHeader time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: connect}).DialContext
	transport.TLSHandshakeTimeout = connect
	transport.ResponseHeaderTimeout = responseHeader
	return &http.Client{Transport: transport, Timeout: 0}
}

// Proxy dispatches inbound requests to the appropriate provider upstream.
type Proxy struct {
	providers map[string]*providerTarget
	logger    *slog.Logger

	resolver     *agent.Resolver
	store        *budget.Store
	estimator    *tokens.Estimator
	agents       map[string]agentRuntime
	unknownAgent string // defaults.unknown_agent: "block" or "passthrough"
}

// defaultStreamLimit is the per-agent concurrent-stream cap (the Session 4
// default of 50). max_concurrent_streams is not yet a config field.
const defaultStreamLimit int64 = 50

// New creates a Proxy from the given config, with two http.Clients per provider
// (streaming and non-streaming) per ADR-005. Timeout strings are pre-validated by
// config.Validate, so ParseDuration errors here are not expected. On the off
// chance one occurs, the zero value is used and the request>0 guard in ServeHTTP
// makes a zero request cap mean "no cap" rather than instant expiry.
func New(cfg *config.Config, logger *slog.Logger) (*Proxy, error) {
	providers := make(map[string]*providerTarget, len(cfg.Providers))
	for _, p := range cfg.Providers {
		connect, _ := time.ParseDuration(p.Timeouts.Connect)
		responseHeader, _ := time.ParseDuration(p.Timeouts.ResponseHeader)
		idle, _ := time.ParseDuration(p.Timeouts.Idle)
		request, _ := time.ParseDuration(p.Timeouts.Request)
		providers[p.Name] = newProviderTarget(p.Upstream, providerTimeouts{
			connect:        connect,
			responseHeader: responseHeader,
			idle:           idle,
			request:        request,
		})
	}

	store, err := budget.NewStore(cfg.Agents, defaultStreamLimit, nil)
	if err != nil {
		return nil, err
	}

	runtimes := make(map[string]agentRuntime, len(cfg.Agents))
	for _, configuredAgent := range cfg.Agents {
		budgetTypes := make([]string, len(configuredAgent.Budgets))
		for i, configuredBudget := range configuredAgent.Budgets {
			budgetTypes[i] = configuredBudget.Type
		}
		runtimes[configuredAgent.Name] = agentRuntime{
			mode:        configuredAgent.Mode,
			budgetTypes: budgetTypes,
		}
	}

	return &Proxy{
		providers:    providers,
		logger:       logger,
		resolver:     agent.NewResolver(cfg.Agents),
		store:        store,
		estimator:    tokens.NewEstimator(cfg.Defaults.UnknownModelTokenizer),
		agents:       runtimes,
		unknownAgent: cfg.Defaults.UnknownAgent,
	}, nil
}

// ServeHTTP handles inbound proxy requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	provider, remaining := splitProviderPath(r.URL.Path)

	target, ok := p.providers[provider]
	if !ok {
		p.writeError(w, http.StatusBadGateway, "unknown_provider",
			"provider not configured: "+provider)
		return
	}

	info, body, err := readRequestBody(r)
	if err != nil {
		p.writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	enforced := p.enforce(w, r, info, body)
	if !enforced.proceed {
		return
	}

	// The deferred outcome defaults to Forfeit (the safe default). Each exit
	// point below sets it. A single deferred applyReconcile settles the budget
	// once, on any return path including a panic, replacing the Session 5 blanket
	// defer Forfeit. For settleNone (passthrough / non-JSON / unknown-passthrough)
	// the action is actionNone, so no budget operation runs.
	outcome := reconcileOutcome{action: actionForfeit, reason: "unsettled"}
	if enforced.postForward == settleNone {
		outcome.action = actionNone
	}
	defer func() {
		applyReconcile(p.store, p.logger, enforced.agentName, enforced.reservationID, p.estimateFor(enforced, info, body), outcome)
	}()

	// Inject stream_options on OpenAI streaming requests so the provider emits a
	// final usage chunk. Only for JSON bodies we parsed (info != nil).
	if info != nil && info.Stream && provider == providerOpenAI {
		if injected, ok := injectStreamOptions(body); ok && streamOptionsIncludeUsage(injected) {
			body = injected
		} else {
			p.logger.Warn("stream_options injection failed, falling back to heuristic", "provider", provider)
		}
	}

	upstreamURL := target.upstream + remaining
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	var requestBody io.Reader
	var contentLength int64
	if info != nil {
		requestBody = bytes.NewReader(body)
		contentLength = int64(len(body))
	} else {
		requestBody = r.Body
		contentLength = r.ContentLength
	}

	isStreamingRequest := info != nil && info.Stream
	client := target.nonStreamingClient
	upstreamContext := r.Context()
	if isStreamingRequest {
		client = target.streamingClient
	} else if target.timeouts.request > 0 {
		var cancel context.CancelFunc
		upstreamContext, cancel = context.WithTimeout(upstreamContext, target.timeouts.request)
		defer cancel()
	}

	upstreamRequest, err := http.NewRequestWithContext(upstreamContext, r.Method, upstreamURL, requestBody)
	if err != nil {
		// Build failure: nothing was sent upstream, so release the reservation.
		outcome = reconcileOutcome{action: actionReconcile, actualTokens: 0, reason: "request_build_failed"}
		p.writeError(w, http.StatusBadGateway, "upstream_error", "failed to build upstream request")
		return
	}

	for key, values := range r.Header {
		if hopByHopHeaders[key] {
			continue
		}
		for _, value := range values {
			upstreamRequest.Header.Add(key, value)
		}
	}
	if contentLength >= 0 {
		upstreamRequest.ContentLength = contentLength
	}

	response, err := client.Do(upstreamRequest)
	if err != nil {
		if r.Context().Err() == context.Canceled {
			// Client disconnected before/while connecting. Forfeit (default).
			outcome = reconcileOutcome{action: actionForfeit, reason: "client_disconnect"}
			return
		}
		status, errType := classifyUpstreamError(err)
		// A dial-phase failure (connection refused, DNS) consumed no tokens:
		// release. A timeout MAY have consumed tokens: forfeit (the default).
		if status == http.StatusBadGateway {
			outcome = reconcileOutcome{action: actionReconcile, actualTokens: 0, reason: "not_connected"}
		} else {
			outcome = reconcileOutcome{action: actionForfeit, reason: "pre_response_timeout"}
		}
		p.logger.Warn("upstream request failed", "provider", provider, "path", remaining, "error", err.Error(), "status", status)
		p.writeError(w, status, errType, "upstream request failed: "+err.Error())
		return
	}
	defer func() { _ = response.Body.Close() }()

	contentType := response.Header.Get("Content-Type")
	isStreaming := strings.Contains(contentType, "text/event-stream")
	p.logger.Info("upstream response", "provider", provider, "path", remaining, "status", response.StatusCode, "streaming", isStreaming)

	if isStreaming {
		state := streamResponse(w, r, response, provider, target.timeouts.idle)
		if enforced.postForward == settleTrack {
			outcome = trackOutcomeForStream(state, body, p.estimator)
		} else {
			outcome = reconcileForStream(state, p.estimateFor(enforced, info, body), p.estimator, body)
		}
		return
	}

	outcome = p.forwardResponse(w, response, provider, remaining, enforced, body)
}

// forwardResponse copies a non-streaming response to the client and returns the
// reconciliation outcome. It buffers the body (bounded by maxBodySize) so usage
// can be extracted. This does not regress latency because a non-streaming client
// waits for the full body regardless (the response carries Content-Length).
func (p *Proxy) forwardResponse(w http.ResponseWriter, response *http.Response, provider, path string, enforced enforcement, requestBody []byte) reconcileOutcome {
	for key, values := range response.Header {
		if hopByHopHeaders[key] {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxBodySize))
	w.WriteHeader(response.StatusCode)
	written, writeErr := w.Write(responseBody)
	if writeErr != nil {
		p.logger.Warn("Upstream response body truncated", "provider", provider, "path", path, "bytes_forwarded", written, "error", writeErr.Error())
	}

	// Observe-mode breach: Track actual usage if known, else accept the under-count.
	if enforced.postForward == settleTrack {
		if tokens, ok := extractNonStreamingUsage(provider, responseBody); ok {
			return reconcileOutcome{action: actionTrack, actualTokens: tokens, reason: "observe_track"}
		}
		return reconcileOutcome{action: actionNone, reason: "observe_skip"}
	}

	// A read error mid-body means we cannot trust the usage field: forfeit.
	if readErr != nil {
		return reconcileOutcome{action: actionForfeit, reason: "response_read_error"}
	}
	return reconcileForResponse(provider, response.StatusCode, responseBody)
}

// estimateFor returns the token estimate used for the reservation, for the
// drift log. It recomputes from the body for a reserved request and returns 0
// otherwise (no reservation, so drift is not meaningful).
func (p *Proxy) estimateFor(enforced enforcement, info *RequestInfo, body []byte) int64 {
	if enforced.postForward != settleReserved || info == nil {
		return 0
	}
	return p.estimator.Estimate(info.Model, body)
}

// writeError writes a JSON error response. It delegates to the package-level
// writeSimpleError so the error envelope has a single source of truth.
func (p *Proxy) writeError(w http.ResponseWriter, status int, errType, message string) {
	writeSimpleError(w, status, errType, message)
}

// classifyUpstreamError distinguishes connection refused (502) from timeout
// (504). Per 001-error-handling.md: connection refused means the provider never
// received the request (no tokens consumed), while timeout means tokens MAY
// have been consumed.
func classifyUpstreamError(err error) (status int, errType string) {
	// Check timeout first. A dial timeout is BOTH a net.Error with Timeout()=true
	// AND a *net.OpError, so ordering matters. Dial timeouts return 504 because
	// we cannot know if the server received the request before the timeout fired.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout, "upstream_timeout"
	}

	// Connection refused, DNS failure, or other dial-phase errors. The provider
	// never received the request, so no tokens were consumed.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return http.StatusBadGateway, "upstream_unreachable"
	}

	// Default to 502 for unknown errors. This is the opposite of the arch doc's
	// general "when in doubt, forfeit" principle, but justified because all
	// client.Do() errors mean the HTTP layer never received a response, so the
	// provider almost certainly did not process tokens.
	return http.StatusBadGateway, "upstream_error"
}

// splitProviderPath splits a path like "/openai/v1/chat/completions" into
// ("openai", "/v1/chat/completions"). An empty path or "/" returns ("", "/").
func splitProviderPath(path string) (provider, remaining string) {
	// Remove leading slash.
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "/"
	}

	idx := strings.IndexByte(trimmed, '/')
	if idx < 0 {
		// Path like "/openai" with no trailing path.
		return trimmed, "/"
	}

	return trimmed[:idx], trimmed[idx:]
}
