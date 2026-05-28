package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
