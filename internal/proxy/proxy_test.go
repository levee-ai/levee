package proxy

import (
	"bytes"
	"io"
	"net/http"
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
