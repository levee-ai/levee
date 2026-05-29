// Package proxy implements the HTTP reverse proxy with budget enforcement.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/levee-ai/levee/internal/config"
)

// providerTarget holds the upstream URL and HTTP client for a provider.
type providerTarget struct {
	upstream string
	client   *http.Client
}

// Proxy dispatches inbound requests to the appropriate provider upstream.
type Proxy struct {
	providers map[string]*providerTarget
	logger    *slog.Logger
}

// New creates a Proxy from the given config, with one http.Client per provider.
func New(cfg *config.Config, logger *slog.Logger) *Proxy {
	providers := make(map[string]*providerTarget, len(cfg.Providers))
	for _, p := range cfg.Providers {
		timeout, _ := time.ParseDuration(p.Timeout)
		providers[p.Name] = &providerTarget{
			upstream: strings.TrimRight(p.Upstream, "/"),
			client:   &http.Client{Timeout: timeout},
		}
	}
	return &Proxy{
		providers: providers,
		logger:    logger,
	}
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

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, reqBody)
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

	resp, err := target.client.Do(upstreamReq)
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

	p.forwardResponse(w, resp)
}

// forwardResponse copies a non-streaming response to the client.
func (p *Proxy) forwardResponse(w http.ResponseWriter, resp *http.Response) {
	for key, vals := range resp.Header {
		if hopByHopHeaders[key] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// writeError writes a JSON error response.
func (p *Proxy) writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	payload := struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	payload.Error.Type = errType
	payload.Error.Message = message

	_ = json.NewEncoder(w).Encode(payload)
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
