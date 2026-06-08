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
// timeouts for a provider. The streaming client sets ResponseHeaderTimeout; the
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
// config.Validate, so ParseDuration errors here are not expected; on the off
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

	agentName, reservationID, proceed := p.enforce(w, r, info, body)
	if !proceed {
		return
	}
	if reservationID != 0 {
		defer func() {
			if forfeitErr := p.store.Forfeit(agentName, reservationID); forfeitErr != nil {
				p.logger.Warn("Forfeit failed", "agent", agentName, "error", forfeitErr.Error())
			}
		}()
	}

	upstreamURL := target.upstream + remaining
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	var reqBody io.Reader
	var contentLength int64

	if info != nil {
		// JSON request: use the buffered body bytes.
		reqBody = bytes.NewReader(body)
		contentLength = int64(len(body))
	} else {
		// Non-JSON request: forward the original body stream.
		reqBody = r.Body
		contentLength = r.ContentLength
	}

	// Select the client on the request stream field (ADR-005). Streaming uses the
	// client with ResponseHeaderTimeout set; non-streaming (and non-JSON, where
	// info is nil) uses the client with no header timeout and relies on the
	// request total cap below. Routing later keys on response Content-Type; the
	// rare stream:false-request/SSE-response mismatch is documented in ADR-005.
	isStreamingRequest := info != nil && info.Stream
	client := target.nonStreamingClient
	upstreamContext := r.Context()
	if isStreamingRequest {
		client = target.streamingClient
	} else if target.timeouts.request > 0 {
		// Non-streaming total cap. Guard request>0 so a zero value means "no cap",
		// never context.WithTimeout(ctx, 0) which is instantly expired.
		var cancel context.CancelFunc
		upstreamContext, cancel = context.WithTimeout(upstreamContext, target.timeouts.request)
		defer cancel()
	}

	upstreamReq, err := http.NewRequestWithContext(upstreamContext, r.Method, upstreamURL, reqBody)
	if err != nil {
		p.writeError(w, http.StatusBadGateway, "upstream_error", "failed to build upstream request")
		return
	}

	// Copy headers, skipping hop-by-hop.
	for key, vals := range r.Header {
		if hopByHopHeaders[key] {
			continue
		}
		for _, v := range vals {
			upstreamReq.Header.Add(key, v)
		}
	}

	if contentLength >= 0 {
		upstreamReq.ContentLength = contentLength
	}

	resp, err := client.Do(upstreamReq)
	if err != nil {
		if r.Context().Err() == context.Canceled {
			return
		}
		status, errType := classifyUpstreamError(err)
		p.logger.Warn("upstream request failed",
			"provider", provider,
			"path", remaining,
			"error", err.Error(),
			"status", status,
		)
		p.writeError(w, status, errType, "upstream request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Route based on response Content-Type, not request stream field.
	ct := resp.Header.Get("Content-Type")
	isStreaming := strings.Contains(ct, "text/event-stream")

	p.logger.Info("upstream response",
		"provider", provider,
		"path", remaining,
		"status", resp.StatusCode,
		"streaming", isStreaming,
	)

	if isStreaming {
		_ = streamResponse(w, resp) // TODO(session-6): use streamState for budget reconciliation
		return
	}

	p.forwardResponse(w, resp, provider, remaining)
}

// forwardResponse copies a non-streaming response to the client. Provider and
// path are passed through only for the anomaly log below.
func (p *Proxy) forwardResponse(w http.ResponseWriter, resp *http.Response, provider, path string) {
	for key, vals := range resp.Header {
		if hopByHopHeaders[key] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// The status is already committed, so a copy failure (for example the
	// non-streaming request cap firing mid-body) cannot become an error status.
	// The client receives a truncated body under the committed status. Log it so
	// the truncation is observable rather than silent (ADR-005, non-streaming
	// cap after headers commit).
	written, err := io.Copy(w, resp.Body)
	if err != nil {
		p.logger.Warn("Upstream response body truncated",
			"provider", provider,
			"path", path,
			"bytes_forwarded", written,
			"error", err.Error(),
		)
	}
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
