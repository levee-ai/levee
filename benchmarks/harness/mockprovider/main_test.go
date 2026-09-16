package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fixtureDir resolves the repo's committed fixtures from this package.
// benchmarks/harness/mockprovider sits three directories below the repo root,
// and Go runs a test with its own package directory as the working directory.
func fixtureDir(testBench testing.TB) string {
	testBench.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "fixtures"))
	if err != nil {
		testBench.Fatalf("resolve fixture dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		testBench.Fatalf("fixtures not found at %s: %v", dir, err)
	}
	return dir
}

// loadTestFixtures is the arrange step every serving test repeats.
func loadTestFixtures(testBench testing.TB) *fixtureSet {
	testBench.Helper()
	fixtures, err := loadFixtures(fixtureDir(testBench))
	if err != nil {
		testBench.Fatalf("loadFixtures: %v", err)
	}
	return fixtures
}

func TestLoadFixtures_MissingFileFailsFast(t *testing.T) {
	empty := t.TempDir()
	if _, err := loadFixtures(empty); err == nil {
		t.Fatal("loadFixtures accepted a directory with no fixtures, it must fail fast")
	} else if !strings.Contains(err.Error(), "chat-completion.json") {
		t.Fatalf("error must name the missing file, got: %v", err)
	}
}

func TestLoadFixtures_RealFixturesCarryUsage(t *testing.T) {
	fixtures := loadTestFixtures(t)
	// A body without usage would make the enforce cell forfeit every
	// request instead of reconciling, which is the trap this guards.
	for name, body := range map[string][]byte{
		"openai json":    fixtures.openAIJSON,
		"anthropic json": fixtures.anthropicJSON,
		"openai sse":     fixtures.openAISSE,
		"anthropic sse":  fixtures.anthropicSSE,
	} {
		if !bytes.Contains(body, []byte("tokens")) {
			t.Errorf("%s fixture carries no token usage field", name)
		}
	}
}

// TestLoadFixtures_SSEFixturesUseBlankLineFraming mirrors the guard in
// internal/proxy/fixture_test.go serveSSEFixture. A recaptured fixture with
// CRLF endings would split into ONE event, so the mock would emit the whole
// stream in a single write and the benchmark would silently measure a code
// path the proxy never takes against a real provider.
//
// loadFixtures rejects the same condition first, so this is a deliberate
// regression guard on the committed fixture data rather than on the loader.
func TestLoadFixtures_SSEFixturesUseBlankLineFraming(t *testing.T) {
	fixtures := loadTestFixtures(t)
	for name, body := range map[string][]byte{
		"openai sse":    fixtures.openAISSE,
		"anthropic sse": fixtures.anthropicSSE,
	} {
		if count := sseEventCount(body); count < 2 {
			t.Errorf("%s split into %d events, blank-line framing is broken (CRLF fixture?)", name, count)
		}
	}
}

func TestLoadFixtures_RejectsSingleEventSSEFixture(t *testing.T) {
	dir := t.TempDir()
	for _, relativePath := range []string{
		filepath.Join("openai", "chat-completion.json"),
		filepath.Join("openai", "chat-completion-stream.sse"),
		filepath.Join("anthropic", "messages.json"),
		filepath.Join("anthropic", "messages-stream.sse"),
	} {
		target := filepath.Join(dir, relativePath)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// CRLF framing, which no blank-line split can see.
		body := "data: {\"usage\":{\"input_tokens\":1}}\r\n\r\ndata: [DONE]\r\n\r\n"
		if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_, err := loadFixtures(dir)
	if err == nil {
		t.Fatal("loadFixtures accepted an SSE fixture with no blank-line framing")
	}
	if !strings.Contains(err.Error(), "chat-completion-stream.sse") {
		t.Errorf("error must name the offending file, got: %v", err)
	}
}

// TestContentTypeConstantsMatchRecordedCapture pins the served headers to the
// values the capture actually observed, recorded in the fixtures README
// metadata block. Without this the per-route header assertions would compare
// the constants against themselves, so editing a constant would change both
// sides at once and prove nothing.
func TestContentTypeConstantsMatchRecordedCapture(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(fixtureDir(t), "README.md"))
	if err != nil {
		t.Fatalf("read fixtures README: %v", err)
	}
	for _, provider := range []string{"openai", "anthropic"} {
		if got := recordedContentType(t, string(readme), provider, false); got != contentTypeJSON {
			t.Errorf("%s recorded non-streaming Content-Type = %q, but the mock serves %q", provider, got, contentTypeJSON)
		}
		if got := recordedContentType(t, string(readme), provider, true); got != contentTypeSSE {
			t.Errorf("%s recorded streaming Content-Type = %q, but the mock serves %q", provider, got, contentTypeSSE)
		}
	}
}

// recordedContentType reads one Content-Type out of the fixtures README
// metadata block, the same way internal/proxy/fixture_test.go does. Only the
// per-provider model line is consulted, never prose, and the streaming marker
// keeps its leading comma so it can never match inside
// "non-streaming Content-Type: ".
func recordedContentType(testBench testing.TB, readme string, provider string, streaming bool) string {
	testBench.Helper()
	marker := "non-streaming Content-Type: "
	if streaming {
		marker = ", streaming: "
	}
	for _, line := range strings.Split(readme, "\n") {
		if !strings.Contains(strings.ToLower(line), provider+" model:") {
			continue
		}
		index := strings.Index(line, marker)
		if index < 0 {
			continue
		}
		rest := line[index+len(marker):]
		if end := strings.IndexAny(rest, ",)"); end >= 0 {
			rest = rest[:end]
		}
		return strings.TrimSpace(rest)
	}
	testBench.Fatalf("fixtures README carries no %q entry for %s, re-run capture.sh", marker, provider)
	return ""
}

func TestServe_OpenAINonStreaming(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	response, err := http.Post(server.URL+"/v1/chat/completions", contentTypeJSON,
		strings.NewReader(`{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"ok"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	body := readAll(t, response)
	if !bytes.Equal(body, fixtures.openAIJSON) {
		t.Errorf("body is not the fixture bytes: got %d bytes, want %d", len(body), len(fixtures.openAIJSON))
	}
}

func TestServe_OpenAIStreamingReplaysEveryEventByteExact(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	response, err := http.Post(server.URL+"/v1/chat/completions", contentTypeJSON,
		strings.NewReader(`{"model":"gpt-4o-mini","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"ok"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if got := response.Header.Get("Content-Type"); got != contentTypeSSE {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeSSE)
	}
	body := readAll(t, response)
	// The concatenation invariant is what lets the proxy replay tests and the
	// benchmark assert byte-exact forwarding.
	if !bytes.Equal(body, fixtures.openAISSE) {
		t.Errorf("stream is not the fixture bytes: got %d bytes, want %d", len(body), len(fixtures.openAISSE))
	}
	if !bytes.Contains(body, []byte("data: [DONE]")) {
		t.Error("stream is missing the OpenAI terminal marker")
	}
	// The marker must be the LAST event, not merely present. The benchmark's
	// forwarding assertions treat it as the end of stream.
	if !bytes.HasSuffix(body, []byte("data: [DONE]\n\n")) {
		t.Errorf("stream does not END with the OpenAI terminal marker, last 32 bytes: %q", tail(body, 32))
	}
}

func TestServe_AnthropicRoutes(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	tests := []struct {
		name     string
		body     string
		wantType string
		wantBody []byte
	}{
		{"non streaming", `{"model":"claude-haiku-4-5","max_tokens":16,"messages":[]}`, contentTypeJSON, fixtures.anthropicJSON},
		{"streaming", `{"model":"claude-haiku-4-5","max_tokens":16,"stream":true,"messages":[]}`, contentTypeSSE, fixtures.anthropicSSE},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			response, err := http.Post(server.URL+"/v1/messages", contentTypeJSON, strings.NewReader(testCase.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if got := response.Header.Get("Content-Type"); got != testCase.wantType {
				t.Errorf("Content-Type = %q, want %q", got, testCase.wantType)
			}
			if body := readAll(t, response); !bytes.Equal(body, testCase.wantBody) {
				t.Errorf("body is not the fixture bytes: got %d bytes, want %d", len(body), len(testCase.wantBody))
			}
		})
	}
}

// TestServe_StreamingUsesChunkedFraming defends the per-event flush, which no
// byte-level assertion can see because framing does not survive into the
// concatenated body.
//
// A flushed stream forces chunked transfer encoding: the headers leave before
// the body is complete, so no Content-Length can be computed and
// response.ContentLength is -1. Drop the flush and net/http buffers the whole
// reply, sets Content-Length, and sends it as one segment, which is not how any
// real provider streams and not the read-and-forward cycle the benchmark means
// to measure. Verified at the wire level: with the flush the reply carries
// Transfer-Encoding chunked and one chunk per event, without it a single
// Content-Length body.
//
// Two limits are worth knowing. This distinguishes flush from no-flush only
// while a fixture stays under net/http bufferBeforeChunkingSize, which is 2048
// bytes, and the OpenAI stream is 1751. A larger recapture would fill that
// buffer and go chunked without any explicit flush, weakening the guard. And
// the chunk COUNT cannot be asserted through the client: Go's chunked reader
// coalesces, so Read call counts come back nondeterministic (measured 3 and 4
// against 6 and 7 chunks on the wire). Exact per-event segmentation therefore
// still rests on review.
func TestServe_StreamingUsesChunkedFraming(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	tests := []struct {
		name string
		path string
		body string
		want []byte
	}{
		{"openai", "/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true,"messages":[]}`, fixtures.openAISSE},
		{"anthropic", "/v1/messages", `{"model":"claude-haiku-4-5","stream":true,"messages":[]}`, fixtures.anthropicSSE},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if len(testCase.want) >= 2048 {
				t.Fatalf("fixture grew to %d bytes, past net/http bufferBeforeChunkingSize: this test no longer distinguishes a flushed stream from a buffered one", len(testCase.want))
			}
			response, err := http.Post(server.URL+testCase.path, contentTypeJSON, strings.NewReader(testCase.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = response.Body.Close() }()

			if response.ContentLength != -1 {
				t.Errorf("ContentLength = %d, want -1: the reply was buffered whole instead of flushed per event", response.ContentLength)
			}
			if !slices.Contains(response.TransferEncoding, "chunked") {
				t.Errorf("TransferEncoding = %v, want chunked", response.TransferEncoding)
			}
			if body := readAll(t, response); !bytes.Equal(body, testCase.want) {
				t.Errorf("stream is not the fixture bytes: got %d bytes, want %d", len(body), len(testCase.want))
			}
		})
	}
}

// TestServe_ConcurrentStreamsLeaveFixturesIntact is the test that catches an
// event framed by appending into the loaded fixture buffer. The benchmark
// drives the mock from many connections at once, so an in-place append is an
// unsynchronized write to memory every other in-flight request is reading.
// Serving the same stream twice in sequence does NOT catch it, because the
// appended bytes happen to equal the trailing newlines they overwrite.
func TestServe_ConcurrentStreamsLeaveFixturesIntact(t *testing.T) {
	fixtures := loadTestFixtures(t)
	pristineOpenAI := append([]byte(nil), fixtures.openAISSE...)
	pristineAnthropic := append([]byte(nil), fixtures.anthropicSSE...)

	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	requests := []struct {
		path string
		body string
		want []byte
	}{
		{"/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true,"messages":[]}`, pristineOpenAI},
		{"/v1/messages", `{"model":"claude-haiku-4-5","stream":true,"messages":[]}`, pristineAnthropic},
	}

	var waitGroup sync.WaitGroup
	for i := 0; i < 32; i++ {
		target := requests[i%len(requests)]
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			response, err := http.Post(server.URL+target.path, contentTypeJSON, strings.NewReader(target.body))
			if err != nil {
				t.Errorf("post %s: %v", target.path, err)
				return
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Errorf("read %s: %v", target.path, err)
				return
			}
			if !bytes.Equal(body, target.want) {
				t.Errorf("%s stream is not the fixture bytes: got %d bytes, want %d", target.path, len(body), len(target.want))
			}
		}()
	}
	waitGroup.Wait()

	if !bytes.Equal(fixtures.openAISSE, pristineOpenAI) {
		t.Error("the in-memory OpenAI SSE fixture was mutated by serving it")
	}
	if !bytes.Equal(fixtures.anthropicSSE, pristineAnthropic) {
		t.Error("the in-memory Anthropic SSE fixture was mutated by serving it")
	}
}

// TestServe_MalformedBodyIsRejected holds the loud failure the cheap structural
// check has to keep. A broken harness must not be served as a request that
// merely carries no stream flag, because that silently measures the
// non-streaming path in a cell meant to be streaming. The truncated case is the
// one a body clipped at maxRequestBody produces.
func TestServe_MalformedBodyIsRejected(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	tests := []struct {
		name string
		body string
	}{
		{"not json at all", "not json"},
		{"empty", ""},
		{"whitespace only", "   \n\t"},
		{"truncated before the closing brace", `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"cli`},
		{"json but not an object", `["stream", true]`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			response, err := http.Post(server.URL+"/v1/chat/completions", contentTypeJSON, strings.NewReader(testCase.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", response.StatusCode)
			}
		})
	}
}

// TestRequestIsStreaming covers the spacing an encoder can legitimately produce
// around the flag, plus the values that are not a boolean literal. The mock's
// own client emits the compact form, and nothing should depend on that silently.
func TestRequestIsStreaming(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"compact true", `{"model":"gpt-4o-mini","stream":true,"messages":[]}`, true},
		{"compact false", `{"model":"gpt-4o-mini","stream":false,"messages":[]}`, false},
		{"space after colon", `{"stream": true}`, true},
		{"space before colon", `{"stream" : true}`, true},
		{"indented encoder output", "{\n  \"model\": \"gpt-4o-mini\",\n  \"stream\": true\n}", true},
		{"newline between colon and value", "{\"stream\":\n\ttrue}", true},
		{"flag absent", `{"model":"gpt-4o-mini","messages":[]}`, false},
		{"flag after the prompt", `{"messages":[{"role":"user","content":"hello"}],"stream":true}`, true},
		{"string value is not a boolean literal", `{"stream":"true"}`, false},
		{"null value is not a boolean literal", `{"stream":null,"messages":[]}`, false},
		{"key without a colon does not stop the scan", `{"names":["stream"],"stream":true}`, true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if !json.Valid([]byte(testCase.body)) {
				t.Fatalf("test body is not valid JSON, fix the case: %s", testCase.body)
			}
			if got := requestIsStreaming([]byte(testCase.body)); got != testCase.want {
				t.Errorf("requestIsStreaming(%s) = %v, want %v", testCase.body, got, testCase.want)
			}
		})
	}
}

// TestServe_FlagTextInsideThePromptIsNotStreaming is the false positive that
// would matter, driven through the real handler. The benchmark sends kilobytes
// of filler prompt, so a substring search over the whole body could in principle
// match filler content. It cannot: JSON escapes a double quote inside a string,
// so this prompt reaches the wire as \"stream\":true and the needle's leading
// unescaped quote never matches. This holds that reasoning against the actual
// encoder rather than against an argument about it.
func TestServe_FlagTextInsideThePromptIsNotStreaming(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	hostilePrompt := `levee filler "stream":true and "stream": true and {"stream":true} filler`
	body, err := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": hostilePrompt}},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	// The premise: the encoder escaped every quote the prompt carried, so the
	// only unescaped "stream" left in the document is the real key.
	if count := bytes.Count(body, streamFlagKey); count != 1 {
		t.Fatalf("encoded body carries %d unescaped %s occurrences, want 1: %s", count, streamFlagKey, body)
	}

	response, err := http.Post(server.URL+"/v1/chat/completions", contentTypeJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if got := response.Header.Get("Content-Type"); got != contentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q: the prompt text selected the streaming fixture", got, contentTypeJSON)
	}
	if got := readAll(t, response); !bytes.Equal(got, fixtures.openAIJSON) {
		t.Errorf("body is not the non-streaming fixture: got %d bytes, want %d", len(got), len(fixtures.openAIJSON))
	}
}

func TestServe_UnknownRouteIs404(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/embeddings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

// TestServe_HealthzIdentifiesTheFixturesItServes holds the payload shape the
// orchestrator asserts against. Readiness that says only "ok" cannot tell a
// fresh process from an orphan of an earlier run still holding the port, and an
// orphan loaded from other fixture bytes serves other token usage numbers, which
// changes the budget path under measurement while every sanity band passes.
func TestServe_HealthzIdentifiesTheFixturesItServes(t *testing.T) {
	fixtures := loadTestFixtures(t)
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}

	var identity fixtureIdentity
	if err := json.Unmarshal(readAll(t, response), &identity); err != nil {
		t.Fatalf("readiness body is not JSON: %v", err)
	}
	if identity.Status != "ok" {
		t.Errorf("status = %q, want ok", identity.Status)
	}
	if want := fixtureDir(t); identity.Directory != want {
		t.Errorf("fixtures_dir = %q, want the resolved absolute %q", identity.Directory, want)
	}
	if !filepath.IsAbs(identity.Directory) {
		t.Errorf("fixtures_dir = %q, want an absolute path", identity.Directory)
	}
	// 64 hex characters is a SHA-256 in hex. A short or empty digest would make
	// the orchestrator's assertion vacuous.
	if len(identity.Digest) != 64 {
		t.Errorf("fixtures_digest = %q, want 64 hex characters", identity.Digest)
	}
	wantLengths := fixtureLengths{
		OpenAIJSON:    len(fixtures.openAIJSON),
		OpenAISSE:     len(fixtures.openAISSE),
		AnthropicJSON: len(fixtures.anthropicJSON),
		AnthropicSSE:  len(fixtures.anthropicSSE),
	}
	if identity.Bytes != wantLengths {
		t.Errorf("fixture_bytes = %+v, want %+v", identity.Bytes, wantLengths)
	}
}

// TestServe_HealthzDigestTracksFixtureContent is the property the orchestrator
// relies on: the digest is a function of the bytes being served, so a mock
// loaded from other fixture content is distinguishable, and a mock loaded from
// the same content is recognizable across a restart. The second half is what
// rules out a per-process nonce, which would satisfy "different digests" while
// telling an orchestrator nothing about what is loaded.
func TestServe_HealthzDigestTracksFixtureContent(t *testing.T) {
	first := readinessIdentity(t, writeFixtureTree(t, "alpha"))
	second := readinessIdentity(t, writeFixtureTree(t, "beta"))
	same := readinessIdentity(t, writeFixtureTree(t, "alpha"))

	if first.Digest == second.Digest {
		t.Errorf("two mocks on different fixture content report the same digest %q, so an orphan is indistinguishable", first.Digest)
	}
	if first.Digest != same.Digest {
		t.Errorf("two mocks on identical fixture content report different digests %q and %q, so the digest does not identify content", first.Digest, same.Digest)
	}
	if first.Directory == same.Directory {
		t.Fatalf("the two trees share a directory %q, so this test proves nothing about content", first.Directory)
	}
	if first.Bytes == second.Bytes {
		t.Errorf("fixture_bytes are identical across differing content: %+v", first.Bytes)
	}
}

// readinessIdentity starts a mock on dir and returns what its /healthz reports.
func readinessIdentity(testBench testing.TB, dir string) fixtureIdentity {
	testBench.Helper()
	fixtures, err := loadFixtures(dir)
	if err != nil {
		testBench.Fatalf("loadFixtures(%s): %v", dir, err)
	}
	server := httptest.NewServer(newHandler(fixtures))
	defer server.Close()

	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		testBench.Fatalf("get readiness: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	var identity fixtureIdentity
	if err := json.Unmarshal(readAll(testBench, response), &identity); err != nil {
		testBench.Fatalf("readiness body is not JSON: %v", err)
	}
	return identity
}

// writeFixtureTree lays down a four file fixture tree that loadFixtures accepts,
// with marker woven into every body so two trees differ by content alone. The
// streams carry blank-line framing and every body carries a usage field, because
// loadFixtures rejects a tree that does not.
func writeFixtureTree(testBench testing.TB, marker string) string {
	testBench.Helper()
	dir := testBench.TempDir()
	bodies := map[string]string{
		filepath.Join("openai", "chat-completion.json"): `{"usage":{"total_tokens":9},"marker":"` + marker + `"}`,
		filepath.Join("openai", "chat-completion-stream.sse"): "data: {\"usage\":{\"total_tokens\":9}}\n\n" +
			"data: {\"marker\":\"" + marker + "\"}\n\ndata: [DONE]\n\n",
		filepath.Join("anthropic", "messages.json"): `{"usage":{"input_tokens":9},"marker":"` + marker + `"}`,
		filepath.Join("anthropic", "messages-stream.sse"): "event: message_start\ndata: {\"usage\":{\"input_tokens\":9}}\n\n" +
			"event: message_stop\ndata: {\"marker\":\"" + marker + "\"}\n\n",
	}
	for relativePath, body := range bodies {
		target := filepath.Join(dir, relativePath)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			testBench.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
			testBench.Fatalf("write %s: %v", relativePath, err)
		}
	}
	return dir
}

func TestCheckLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:19099", false},
		{"[::1]:19099", false},
		{"0.0.0.0:19099", true},
		{"192.168.1.10:19099", true},
		{"localhost:19099", true},
		{"127.0.0.1", true},
		{"", true},
	}
	for _, testCase := range tests {
		err := checkLoopbackAddr(testCase.addr)
		if testCase.wantErr && err == nil {
			t.Errorf("checkLoopbackAddr(%q) = nil, want an error", testCase.addr)
		}
		if !testCase.wantErr && err != nil {
			t.Errorf("checkLoopbackAddr(%q) = %v, want nil", testCase.addr, err)
		}
	}
}

// legacyStreamRequest is the struct the mock used to unmarshal every request
// body into. It survives only as the baseline BenchmarkRequestSelection measures
// the cheap scan against.
type legacyStreamRequest struct {
	Stream bool `json:"stream"`
}

// selectByParse is the pre-write work the mock used to do per request: read the
// body with io.ReadAll, then parse the whole document to read one boolean.
func selectByParse(request *http.Request) (bool, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody))
	if err != nil {
		return false, err
	}
	var parsed legacyStreamRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, err
	}
	return parsed.Stream, nil
}

// selectByScanAfterReadAll is the intermediate step, kept as an arm because it
// separates the two costs: it drops the parse but keeps io.ReadAll, so the gap
// between this arm and selectByScan is what the declared-length read is worth
// and the gap to selectByParse is what the parse was worth.
func selectByScanAfterReadAll(request *http.Request) (bool, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody))
	if err != nil {
		return false, err
	}
	return checkAndScan(body)
}

// selectByScan is the pre-write work the mock does now, calling the same three
// functions serveFixture calls.
func selectByScan(request *http.Request) (bool, error) {
	body, err := readRequestBody(request)
	if err != nil {
		return false, err
	}
	return checkAndScan(body)
}

func checkAndScan(body []byte) (bool, error) {
	if !looksLikeJSONObject(body) {
		return false, errors.New("request body is not a JSON object")
	}
	return requestIsStreaming(body), nil
}

// BenchmarkRequestSelection measures the mock's own per-request cost against
// payload size, which is what the package doc claims and what the direct-to-mock
// cell of the experiment rests on. The parse arm is the implementation this
// replaced, kept here so the claim is a measurement rather than an assertion.
// Body size is swept across the prompt sizes the experiment uses.
//
// Both field orders are measured. k6 order puts the flag ahead of the prompt,
// which is what benchmarks/k6/overhead.js sends. Flag-last puts it behind the
// prompt, which is the worst case for the scan because it then has to cross the
// whole document, and it is what a map-keyed encoder emits.
func BenchmarkRequestSelection(b *testing.B) {
	sizes := []struct {
		name        string
		promptBytes int
	}{
		{"150B", 150},
		{"4KB", 4 << 10},
		{"32KB", 32 << 10},
	}
	arms := []struct {
		name          string
		selectFixture func(*http.Request) (bool, error)
	}{
		{"parse", selectByParse},
		{"scanAfterReadAll", selectByScanAfterReadAll},
		{"scan", selectByScan},
	}
	orders := []struct {
		name     string
		flagLast bool
	}{
		{"k6order", false},
		{"flaglast", true},
	}

	for _, order := range orders {
		for _, arm := range arms {
			for _, size := range sizes {
				body := benchmarkRequestBody(b, size.promptBytes, order.flagLast)
				b.Run(order.name+"/"+arm.name+"/"+size.name, func(b *testing.B) {
					// The request is built once and its body rewound per
					// iteration, so the measurement covers the pre-write work
					// and not the cost of constructing a request. Building it
					// from a bytes.Reader is what sets ContentLength, which is
					// the size hint the declared-length read uses and which a
					// real client supplies the same way.
					reader := bytes.NewReader(body)
					request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
					request.Body = rewindableBody{reader: reader}
					if request.ContentLength != int64(len(body)) {
						b.Fatalf("ContentLength = %d, want %d", request.ContentLength, len(body))
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						reader.Reset(body)
						streaming, err := arm.selectFixture(request)
						if err != nil {
							b.Fatalf("select: %v", err)
						}
						if streaming {
							b.Fatal("selected the streaming fixture for a non streaming body")
						}
					}
				})
			}
		}
	}
}

// rewindableBody is a request body backed by a reader the benchmark rewinds
// between iterations. Close is a no-op because nothing here owns the reader.
type rewindableBody struct{ reader *bytes.Reader }

func (body rewindableBody) Read(target []byte) (int, error) { return body.reader.Read(target) }
func (body rewindableBody) Close() error                    { return nil }

// benchmarkRequestBody builds the body benchmarks/k6/overhead.js posts: the same
// deterministic filler prompt at the same sizes, with the stream flag either
// ahead of the prompt as k6 sends it or behind it.
//
// The filler is ASCII with no quote or backslash, so a template needs no
// escaping here, and json.Valid holds that rather than assuming it.
func benchmarkRequestBody(testBench testing.TB, promptBytes int, flagLast bool) []byte {
	testBench.Helper()
	const unit = "levee benchmark filler text "
	var builder strings.Builder
	for builder.Len() < promptBytes {
		builder.WriteString(unit)
	}
	prompt := builder.String()[:promptBytes]

	messages := `"messages":[{"role":"user","content":"` + prompt + `"}]`
	fields := `"model":"gpt-4o-mini-2024-07-18","max_tokens":16,"stream":false,` + messages
	if flagLast {
		fields = `"model":"gpt-4o-mini-2024-07-18","max_tokens":16,` + messages + `,"stream":false`
	}
	body := []byte("{" + fields + "}")
	if !json.Valid(body) {
		testBench.Fatalf("benchmark body is not valid JSON at %d prompt bytes", promptBytes)
	}
	return body
}

func readAll(testBench testing.TB, response *http.Response) []byte {
	testBench.Helper()
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		testBench.Fatalf("read body: %v", err)
	}
	return buffer.Bytes()
}

func tail(body []byte, count int) []byte {
	if len(body) <= count {
		return body
	}
	return body[len(body)-count:]
}
