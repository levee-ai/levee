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
		"Connection":        "keep-alive",
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

	// Upstream non-hop-by-hop headers should be preserved.
	if reqID := result.Header.Get("X-Request-Id"); reqID != "req-123" {
		t.Errorf("expected X-Request-Id req-123, got %q", reqID)
	}
}

// --- Proxy tests ---

func newTestProxy(tb testing.TB, upstreamURL string) *Proxy {
	tb.Helper()
	providers := map[string]*providerTarget{
		"openai":    {upstream: upstreamURL, client: &http.Client{Timeout: 5 * time.Second}},
		"anthropic": {upstream: upstreamURL, client: &http.Client{Timeout: 5 * time.Second}},
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

func TestProxy_UpstreamTimeout_Returns504(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Use a very short timeout.
	providers := map[string]*providerTarget{
		"openai": {upstream: upstream.URL, client: &http.Client{Timeout: 50 * time.Millisecond}},
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
