// Package proxy implements the HTTP reverse proxy with budget enforcement.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
		p.writeError(w, http.StatusGatewayTimeout, "upstream_timeout",
			"upstream request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Route based on response Content-Type, not request stream field.
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		streamResponse(w, resp)
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
