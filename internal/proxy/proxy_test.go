package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levee-ai/levee/internal/config"
)

func newRequest(contentType, body string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "http://localhost/v1/chat/completions", io.NopCloser(bytes.NewBufferString(body)))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestReadRequestBody_ValidWithStreamFalse(t *testing.T) {
	req := newRequest("application/json", `{"model":"gpt-4","stream":false}`)
	info, body, err := readRequestBody(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil info")
	}
	if info.Model != "gpt-4" {
		t.Errorf("expected model gpt-4, got %s", info.Model)
	}
	if info.Stream {
		t.Error("expected stream to be false")
	}
	if len(body) == 0 {
		t.Error("expected non-empty body bytes")
	}
}

func TestReadRequestBody_ValidWithStreamTrue(t *testing.T) {
	req := newRequest("application/json", `{"model":"claude-3-opus","stream":true}`)
	info, body, err := readRequestBody(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil info")
	}
	if info.Model != "claude-3-opus" {
		t.Errorf("expected model claude-3-opus, got %s", info.Model)
	}
	if !info.Stream {
		t.Error("expected stream to be true")
	}
	if len(body) == 0 {
		t.Error("expected non-empty body bytes")
	}
}

func TestReadRequestBody_MissingModel(t *testing.T) {
	req := newRequest("application/json", `{"stream":true}`)
	info, _, err := readRequestBody(req)
	if err != errInvalidBody {
		t.Fatalf("expected errInvalidBody, got %v", err)
	}
	if info != nil {
		t.Error("expected nil info")
	}
}

func TestReadRequestBody_ModelIsNull(t *testing.T) {
	req := newRequest("application/json", `{"model":null,"stream":false}`)
	info, _, err := readRequestBody(req)
	if err != errInvalidBody {
		t.Fatalf("expected errInvalidBody, got %v", err)
	}
	if info != nil {
		t.Error("expected nil info")
	}
}

func TestReadRequestBody_ModelIsNumber(t *testing.T) {
	req := newRequest("application/json", `{"model":42,"stream":false}`)
	info, _, err := readRequestBody(req)
	if err != errInvalidBody {
		t.Fatalf("expected errInvalidBody, got %v", err)
	}
	if info != nil {
		t.Error("expected nil info")
	}
}

func TestReadRequestBody_ModelIsEmptyString(t *testing.T) {
	req := newRequest("application/json", `{"model":"","stream":false}`)
	info, _, err := readRequestBody(req)
	if err != errInvalidBody {
		t.Fatalf("expected errInvalidBody, got %v", err)
	}
	if info != nil {
		t.Error("expected nil info")
	}
}

func TestReadRequestBody_NonJSONContentType(t *testing.T) {
	req := newRequest("multipart/form-data", `{"model":"gpt-4","stream":false}`)
	info, body, err := readRequestBody(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info != nil {
		t.Error("expected nil info for non-JSON content type")
	}
	if body != nil {
		t.Error("expected nil body for non-JSON content type")
	}
}

func TestReadRequestBody_EmptyBody(t *testing.T) {
	req := newRequest("application/json", "")
	info, _, err := readRequestBody(req)
	if err != errInvalidBody {
		t.Fatalf("expected errInvalidBody, got %v", err)
	}
	if info != nil {
		t.Error("expected nil info")
	}
}

func TestReadRequestBody_JSONContentTypeWithCharset(t *testing.T) {
	req := newRequest("application/json; charset=utf-8", `{"model":"gpt-4","stream":true}`)
	info, body, err := readRequestBody(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil info")
	}
	if info.Model != "gpt-4" {
		t.Errorf("expected model gpt-4, got %s", info.Model)
	}
	if !info.Stream {
		t.Error("expected stream to be true")
	}
	if len(body) == 0 {
		t.Error("expected non-empty body bytes")
	}
}

// --- Streaming tests ---

// fakeSSEResponse builds an *http.Response with the given SSE body and headers.
func fakeSSEResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req-123"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func TestStreamResponse_ForwardsAllEvents(t *testing.T) {
	sseBody := strings.Join([]string{
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}",
		"",
		"data: {\"id\":\"2\",\"choices\":[{\"delta\":{\"content\":\" world\"}}]}",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := fakeSSEResponse(http.StatusOK, sseBody)
	rec := httptest.NewRecorder()

	state := streamResponse(rec, resp)

	if !state.completedNormally {
		t.Error("expected completedNormally to be true")
	}

	output := rec.Body.String()
	if !strings.Contains(output, "Hello") {
		t.Error("expected output to contain first chunk")
	}
	if !strings.Contains(output, " world") {
		t.Error("expected output to contain second chunk")
	}
	if !strings.Contains(output, "data: [DONE]") {
		t.Error("expected output to contain terminal marker")
	}
}

func TestStreamResponse_AnthropicMultiLineEvents(t *testing.T) {
	sseBody := strings.Join([]string{
		"event: message_start",
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}",
		"",
		"event: content_block_delta",
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hi\"}}",
		"",
		"event: message_stop",
		"data: {\"type\":\"message_stop\"}",
		"",
	}, "\n")

	resp := fakeSSEResponse(http.StatusOK, sseBody)
	rec := httptest.NewRecorder()

	state := streamResponse(rec, resp)

	if !state.completedNormally {
		t.Error("expected completedNormally to be true for Anthropic stream")
	}
	if state.lastEventType != "message_stop" {
		t.Errorf("expected lastEventType to be message_stop, got %s", state.lastEventType)
	}

	output := rec.Body.String()
	if !strings.Contains(output, "event: message_start") {
		t.Error("expected output to contain message_start event")
	}
	if !strings.Contains(output, "content_block_delta") {
		t.Error("expected output to contain content_block_delta event")
	}
	if !strings.Contains(output, "event: message_stop") {
		t.Error("expected output to contain message_stop event")
	}
}

func TestStreamResponse_SetsCorrectHeaders(t *testing.T) {
	sseBody := "data: {\"id\":\"1\"}\n\ndata: [DONE]\n\n"
	resp := fakeSSEResponse(http.StatusOK, sseBody)
	rec := httptest.NewRecorder()

	streamResponse(rec, resp)

	result := rec.Result()
	defer func() { _ = result.Body.Close() }()

	checks := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	}

	for header, expected := range checks {
		got := result.Header.Get(header)
		if got != expected {
			t.Errorf("header %s: expected %q, got %q", header, expected, got)
		}
	}

	// Content-Length must not be present.
	if cl := result.Header.Get("Content-Length"); cl != "" {
		t.Errorf("expected no Content-Length header, got %q", cl)
	}

	// Connection header must not be set (hop-by-hop, forbidden in HTTP/2).
	if conn := result.Header.Get("Connection"); conn != "" {
		t.Errorf("expected no Connection header, got %q", conn)
	}

	// Upstream non-hop-by-hop headers should be preserved.
	if reqID := result.Header.Get("X-Request-Id"); reqID != "req-123" {
		t.Errorf("expected X-Request-Id req-123, got %q", reqID)
	}
}

// --- Proxy tests ---

// testTimeouts returns generous parsed timeouts for tests. Individual tests that
// need a tight bound build their own providerTarget via newProviderTarget.
func testTimeouts() providerTimeouts {
	return providerTimeouts{
		connect:        5 * time.Second,
		responseHeader: 5 * time.Second,
		idle:           5 * time.Second,
		request:        5 * time.Second,
	}
}

func newTestProxy(tb testing.TB, upstreamURL string) *Proxy {
	tb.Helper()
	providers := map[string]*providerTarget{
		"openai":    newProviderTarget(upstreamURL, testTimeouts()),
		"anthropic": newProviderTarget(upstreamURL, testTimeouts()),
	}
	return &Proxy{
		providers: providers,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestProxy_NonStreamingForward(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify path stripping.
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("expected path /v1/chat/completions, got %s", r.URL.Path)
		}
		// Verify a request header is forwarded.
		if r.Header.Get("X-Custom-Header") != "test-value" {
			t.Errorf("expected X-Custom-Header test-value, got %s", r.Header.Get("X-Custom-Header"))
		}
		// Echo a response.
		w.Header().Set("X-Upstream-Response", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp-1","choices":[]}`))
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()

	body := `{"model":"gpt-4","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Header", "test-value")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-Upstream-Response") != "yes" {
		t.Error("expected upstream response header to be forwarded")
	}
	if !strings.Contains(rec.Body.String(), "resp-1") {
		t.Error("expected response body to contain resp-1")
	}
}

func TestProxy_UnknownProvider(t *testing.T) {
	proxy := newTestProxy(t, "http://localhost:1")
	rec := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodPost, "/nonexistent/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}

	var errResp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Error.Type != "unknown_provider" {
		t.Errorf("expected type unknown_provider, got %s", errResp.Error.Type)
	}
}

func TestProxy_InvalidBody_Returns400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called for invalid body")
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()

	// Missing model field.
	body := `{"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	var errResp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Error.Type != "invalid_request" {
		t.Errorf("expected type invalid_request, got %s", errResp.Error.Type)
	}
}

func TestProxy_Upstream5xx_PassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service down"}`))
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()

	body := `{"model":"gpt-4","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "service down") {
		t.Error("expected upstream error body to be forwarded")
	}
}

func TestProxy_ResponseHeaderTimeout_Returns504(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Withhold headers longer than the response_header timeout.
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Tight response_header timeout on a streaming request so the header timeout
	// fires before headers arrive. Client.Timeout stays 0.
	timeouts := providerTimeouts{
		connect:        5 * time.Second,
		responseHeader: 50 * time.Millisecond,
		idle:           5 * time.Second,
		request:        5 * time.Second,
	}
	proxy := &Proxy{
		providers: map[string]*providerTarget{
			"openai": newProviderTarget(upstream.URL, timeouts),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rec := httptest.NewRecorder()
	body := `{"model":"gpt-4","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}
	var errResp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Error.Type != "upstream_timeout" {
		t.Errorf("expected type upstream_timeout, got %s", errResp.Error.Type)
	}
}

func TestProxy_NonStreamingRequestCap_BoundsBodyAfterHeaders(t *testing.T) {
	// Absent the request cap, the upstream would send the full body
	// {"id":"resp-1","choices":[],"trailer":"END"} across two writes. The cap
	// truncates the read after the first write, so the "END" trailer never lands.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // commit headers immediately
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("expected http.Flusher")
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-1",`))
		flusher.Flush()
		time.Sleep(300 * time.Millisecond) // stall mid-body, past the request cap
		_, _ = w.Write([]byte(`"choices":[],"trailer":"END"}`))
	}))
	defer upstream.Close()

	// request cap shorter than the upstream mid-body stall; response_header
	// generous so headers arrive and a 200 commits before the cap fires.
	timeouts := providerTimeouts{
		connect:        5 * time.Second,
		responseHeader: 5 * time.Second,
		idle:           5 * time.Second,
		request:        100 * time.Millisecond,
	}
	proxy := &Proxy{
		providers: map[string]*providerTarget{
			"openai": newProviderTarget(upstream.URL, timeouts),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rec := httptest.NewRecorder()
	body := `{"model":"gpt-4","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	// Status committed as 200 before the cap fired; it cannot become a 504.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected committed 200, got %d", rec.Code)
	}
	// The cap bounded the body read, so the trailer never arrived.
	if strings.Contains(rec.Body.String(), "END") {
		t.Errorf("expected truncated body (no trailer), got full body: %q", rec.Body.String())
	}
}

func TestProxy_StreamingRequest_NotCappedByRequestTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("expected http.Flusher")
			return
		}
		// Total stream duration (160ms) exceeds the request cap (50ms) below.
		// Each chunk arrives within the idle bound, so a future watchdog would
		// also keep it alive; here we only assert the request cap does not apply.
		for i := 0; i < 4; i++ {
			_, _ = w.Write([]byte("data: chunk\n\n"))
			flusher.Flush()
			time.Sleep(40 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	timeouts := providerTimeouts{
		connect:        5 * time.Second,
		responseHeader: 5 * time.Second,
		idle:           5 * time.Second,
		request:        50 * time.Millisecond, // would kill the stream if (wrongly) applied
	}
	proxy := &Proxy{
		providers: map[string]*providerTarget{
			"openai": newProviderTarget(upstream.URL, timeouts),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rec := httptest.NewRecorder()
	body := `{"model":"gpt-4","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("expected full stream including [DONE], got: %q", rec.Body.String())
	}
}

func TestProxy_UpstreamConnectionRefused_Returns502(t *testing.T) {
	// Point at a port that is not listening.
	providers := map[string]*providerTarget{
		"openai": newProviderTarget("http://127.0.0.1:1", testTimeouts()),
	}
	proxy := &Proxy{
		providers: providers,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rec := httptest.NewRecorder()
	body := `{"model":"gpt-4","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}

	var errResp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Error.Type != "upstream_unreachable" {
		t.Errorf("expected type upstream_unreachable, got %s", errResp.Error.Type)
	}
}

func TestProxy_NonJSONContentType_ForwardedWithoutParsing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the body is forwarded as-is.
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "multipart-data-here") {
			t.Error("expected original body to be forwarded")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/audio/transcriptions",
		strings.NewReader("multipart-data-here"))
	req.Header.Set("Content-Type", "multipart/form-data")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rec.Body.String())
	}
}

func TestProxy_StreamingForward(t *testing.T) {
	ssePayload := strings.Join([]string{
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}",
		"",
		"data: {\"id\":\"2\",\"choices\":[{\"delta\":{\"content\":\" world\"}}]}",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "stream-123")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssePayload))
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()

	body := `{"model":"gpt-4","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	result := rec.Result()
	defer func() { _ = result.Body.Close() }()

	if ct := result.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %s", ct)
	}
	if !strings.Contains(rec.Body.String(), "Hello") {
		t.Error("expected SSE body to contain Hello")
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Error("expected SSE body to contain terminal marker")
	}
}

func TestProxy_BareProviderPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bare /openai should forward to upstream + "/".
		if r.URL.Path != "/" {
			t.Errorf("expected path /, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("root"))
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()

	// Non-JSON so body parsing is skipped.
	req := httptest.NewRequest(http.MethodPost, "/openai", strings.NewReader("payload"))
	req.Header.Set("Content-Type", "text/plain")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "root" {
		t.Errorf("expected body 'root', got %q", rec.Body.String())
	}
}

func TestProxy_StreamRequestButNonStreamResponse(t *testing.T) {
	// Upstream returns a JSON 429 despite stream:true in the request.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"too many requests"}}`))
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()

	body := `{"model":"gpt-4","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	result := rec.Result()
	defer func() { _ = result.Body.Close() }()

	// Must NOT have SSE headers, since response is JSON.
	ct := result.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		t.Error("expected non-SSE Content-Type for JSON error response")
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Error("expected error body to be forwarded")
	}
}

func TestStreamResponse_ScannerOverflow(t *testing.T) {
	// Create a line longer than 4MB to trigger bufio.ErrTooLong.
	hugeLine := "data: " + strings.Repeat("x", 5*1024*1024) + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = fmt.Fprint(w, hugeLine)
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	rec := httptest.NewRecorder()
	state := streamResponse(rec, resp)

	if state.completedNormally {
		t.Error("expected completedNormally = false on scanner overflow")
	}
	if state.scanErr == nil {
		t.Error("expected scanErr to be non-nil on overflow")
	}
}

// --- splitProviderPath unit tests ---

func TestSplitProviderPath(t *testing.T) {
	cases := []struct {
		input     string
		provider  string
		remaining string
	}{
		{"/openai/v1/chat/completions", "openai", "/v1/chat/completions"},
		{"/anthropic/v1/messages", "anthropic", "/v1/messages"},
		{"/openai", "openai", "/"},
		{"/", "", "/"},
		{"", "", "/"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("path=%q", tc.input), func(t *testing.T) {
			provider, remaining := splitProviderPath(tc.input)
			if provider != tc.provider {
				t.Errorf("provider: expected %q, got %q", tc.provider, provider)
			}
			if remaining != tc.remaining {
				t.Errorf("remaining: expected %q, got %q", tc.remaining, remaining)
			}
		})
	}
}

// --- Benchmarks ---

func BenchmarkProxy_NonStreaming(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":10}}`)
	}))
	defer upstream.Close()

	p := newTestProxy(b, upstream.URL)
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"test"}]}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}
}

func BenchmarkProxy_StreamingSmall(b *testing.B) {
	ssePayload := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, ssePayload)
	}))
	defer upstream.Close()

	p := newTestProxy(b, upstream.URL)
	body := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"test"}]}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}
}

func BenchmarkReadRequestBody(b *testing.B) {
	body := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"benchmark test message with some content to make it realistic"}],"max_tokens":1024}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		_, _, _ = readRequestBody(req)
	}
}

// TestNew_ParsesTimeoutsFromConfig locks the config-string to providerTimeouts
// mapping in New(). Every other proxy test builds providerTarget directly, so
// without this a transposed field (for example responseHeader into idle) would
// ship green. It also confirms idle is populated even though nothing reads it
// until the Session 6 watchdog.
func TestNew_ParsesTimeoutsFromConfig(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:     "openai",
				Upstream: "https://api.openai.com/",
				Timeouts: config.TimeoutsConfig{
					Connect:        "3s",
					ResponseHeader: "7s",
					Idle:           "11s",
					Request:        "13s",
				},
			},
		},
	}

	proxy := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	target, ok := proxy.providers["openai"]
	if !ok {
		t.Fatal("expected openai provider target")
	}
	if target.upstream != "https://api.openai.com" {
		t.Errorf("expected trailing slash trimmed, got %q", target.upstream)
	}
	want := providerTimeouts{
		connect:        3 * time.Second,
		responseHeader: 7 * time.Second,
		idle:           11 * time.Second,
		request:        13 * time.Second,
	}
	if target.timeouts != want {
		t.Errorf("timeouts mapping: got %+v, want %+v", target.timeouts, want)
	}
}

// TestNew_PreservesHTTP2 guards against a transport regression: building a bare
// http.Transport with a custom DialContext silently disables HTTP/2 unless
// ForceAttemptHTTP2 is set. Cloning http.DefaultTransport keeps it. OpenAI and
// Anthropic both serve HTTP/2, so a downgrade to HTTP/1.1 is a latency
// regression. This asserts the negotiated protocol against an HTTP/2 upstream.
func TestNew_PreservesHTTP2(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.Proto)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	timeouts := testTimeouts()
	target := newProviderTarget(upstream.URL, timeouts)
	// Trust the test server's self-signed CA on both clients.
	upstreamTLS := upstream.Client().Transport.(*http.Transport).TLSClientConfig
	target.streamingClient.Transport.(*http.Transport).TLSClientConfig = upstreamTLS
	target.nonStreamingClient.Transport.(*http.Transport).TLSClientConfig = upstreamTLS

	for name, client := range map[string]*http.Client{
		"streaming":    target.streamingClient,
		"nonStreaming": target.nonStreamingClient,
	} {
		response, err := client.Get(upstream.URL)
		if err != nil {
			t.Fatalf("%s client request failed: %v", name, err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.Proto != "HTTP/2.0" {
			t.Errorf("%s client: expected HTTP/2.0, negotiated %s (body proto %q)", name, response.Proto, string(body))
		}
	}
}

// TestProxy_NonStreamingZeroRequestCap_NotCapped covers the request>0 guard.
// A zero request duration must mean "no total cap", not context.WithTimeout(0)
// which is instantly expired. Without the guard, a non-streaming request to a
// provider with request=0 would return an immediate spurious 504.
func TestProxy_NonStreamingZeroRequestCap_NotCapped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Take a little time, but succeed. With request=0 there is no cap, so
		// this must complete rather than expire instantly.
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp-ok"}`))
	}))
	defer upstream.Close()

	timeouts := providerTimeouts{
		connect:        5 * time.Second,
		responseHeader: 5 * time.Second,
		idle:           5 * time.Second,
		request:        0, // no total cap
	}
	proxy := &Proxy{
		providers: map[string]*providerTarget{
			"openai": newProviderTarget(upstream.URL, timeouts),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rec := httptest.NewRecorder()
	body := `{"model":"gpt-4","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (zero request means no cap), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "resp-ok") {
		t.Errorf("expected full body, got %q", rec.Body.String())
	}
}

// TestProxy_NonStreamingRequestCap_PreHeader504 covers the non-streaming cap
// firing BEFORE headers arrive (slow generation, the realistic non-streaming
// shape). The context deadline aborts client.Do before any status commits, so
// it surfaces as a net.Error timeout and is classified 504. This complements
// the post-commit body-truncation test.
func TestProxy_NonStreamingRequestCap_PreHeader504(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Withhold headers entirely past the request cap.
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"too-late"}`))
	}))
	defer upstream.Close()

	timeouts := providerTimeouts{
		connect:        5 * time.Second,
		responseHeader: 5 * time.Second,
		idle:           5 * time.Second,
		request:        50 * time.Millisecond,
	}
	proxy := &Proxy{
		providers: map[string]*providerTarget{
			"openai": newProviderTarget(upstream.URL, timeouts),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rec := httptest.NewRecorder()
	body := `{"model":"gpt-4","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}
	var errResp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Error.Type != "upstream_timeout" {
		t.Errorf("expected type upstream_timeout, got %s", errResp.Error.Type)
	}
}
