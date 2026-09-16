// Command mockprovider is a zero-latency stand-in for an LLM provider API.
// It replays the response fixtures recorded from the real OpenAI and
// Anthropic APIs so a benchmark measures proxy overhead rather than provider
// variance. It never synthesizes a response body: a body without a usage
// field would make the proxy forfeit its reservation on every request
// instead of reconciling, which would silently measure the wrong code path.
//
// It binds loopback only and never logs per request, so it adds no
// measurable work to the path under test.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Content types recorded at capture time. The authoritative values live in
// the metadata block of testdata/fixtures/README.md, which the proxy replay
// tests parse and which this package's tests hold these constants against.
// They are constants here because the mock must serve exactly what the real
// providers served: the proxy selects its streaming code path by looking for
// text/event-stream in the response Content-Type.
const (
	contentTypeJSON = "application/json"
	contentTypeSSE  = "text/event-stream; charset=utf-8"
)

// maxRequestBody bounds the body read so a misdirected client cannot balloon
// memory. Benchmark request bodies are tens of kilobytes at most.
const maxRequestBody = 1 << 20

// eventSeparator is the SSE blank-line boundary between events.
var eventSeparator = []byte("\n\n")

// fixtureSet holds the four recorded bodies, read once at startup.
type fixtureSet struct {
	openAIJSON    []byte
	openAISSE     []byte
	anthropicJSON []byte
	anthropicSSE  []byte
}

// loadFixtures reads the four recorded bodies from dir, which is the repo's
// testdata/fixtures tree. Any missing file is fatal and names itself: a
// partially loaded fixture set would produce a silently wrong benchmark.
//
// There is no go:embed here on purpose. Embed patterns cannot traverse "..",
// and the go tool ignores testdata directories, so the path arrives as a flag.
func loadFixtures(dir string) (*fixtureSet, error) {
	set := &fixtureSet{}
	fixtures := []struct {
		relativePath string
		streaming    bool
		target       *[]byte
	}{
		{filepath.Join("openai", "chat-completion.json"), false, &set.openAIJSON},
		{filepath.Join("openai", "chat-completion-stream.sse"), true, &set.openAISSE},
		{filepath.Join("anthropic", "messages.json"), false, &set.anthropicJSON},
		{filepath.Join("anthropic", "messages-stream.sse"), true, &set.anthropicSSE},
	}

	for _, fixture := range fixtures {
		body, err := os.ReadFile(filepath.Join(dir, fixture.relativePath))
		if err != nil {
			return nil, fmt.Errorf("read fixture %s: %w", fixture.relativePath, err)
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("fixture %s is empty", fixture.relativePath)
		}
		// A stream that does not split on blank lines would go out as one
		// write, so the proxy would see the whole response in a single read
		// and the benchmark would measure a path production never takes.
		// internal/proxy/fixture_test.go serveSSEFixture guards the same way.
		if fixture.streaming {
			if count := sseEventCount(body); count < 2 {
				return nil, fmt.Errorf("fixture %s split into %d events, blank-line framing is broken (CRLF fixture?)", fixture.relativePath, count)
			}
		}
		*fixture.target = body
	}
	return set, nil
}

// sseEventCount reports how many blank-line delimited events a recorded stream
// carries. The trailing terminator is trimmed first so it does not count as an
// empty final event.
func sseEventCount(sseBody []byte) int {
	return len(bytes.Split(bytes.TrimRight(sseBody, "\n"), eventSeparator))
}

// streamRequest is the only part of a request body the mock inspects.
type streamRequest struct {
	Stream bool `json:"stream"`
}

// newHandler routes the two provider endpoints plus a readiness probe.
func newHandler(fixtures *fixtureSet) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(responseWriter http.ResponseWriter, request *http.Request) {
		serveFixture(responseWriter, request, fixtures.openAIJSON, fixtures.openAISSE)
	})
	mux.HandleFunc("POST /v1/messages", func(responseWriter http.ResponseWriter, request *http.Request) {
		serveFixture(responseWriter, request, fixtures.anthropicJSON, fixtures.anthropicSSE)
	})
	mux.HandleFunc("GET /healthz", func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.WriteHeader(http.StatusOK)
		_, _ = responseWriter.Write([]byte("ok\n"))
	})
	return mux
}

// serveFixture answers with the streaming or non-streaming fixture depending
// on the request's stream field.
func serveFixture(responseWriter http.ResponseWriter, request *http.Request, jsonBody, sseBody []byte) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody))
	if err != nil {
		http.Error(responseWriter, "read request body", http.StatusBadRequest)
		return
	}
	var parsed streamRequest
	// An unparseable body is a harness bug, not a case to tolerate quietly.
	if err := json.Unmarshal(body, &parsed); err != nil {
		http.Error(responseWriter, "request body is not JSON", http.StatusBadRequest)
		return
	}
	if parsed.Stream {
		writeSSE(responseWriter, sseBody)
		return
	}
	responseWriter.Header().Set("Content-Type", contentTypeJSON)
	responseWriter.WriteHeader(http.StatusOK)
	_, _ = responseWriter.Write(jsonBody)
}

// writeSSE replays a recorded stream one event per write plus flush, with no
// pacing. The per-event flush is load bearing: it produces distinct TCP
// segments so the proxy performs a real read and forward cycle per event
// rather than seeing the whole stream in one read. Measured on the OpenAI
// fixture, the flush yields Transfer-Encoding chunked with six wire chunks of
// 341, 309, 309, 293, 485, and 14 bytes, one per event, while dropping it
// yields one buffered Content-Length body.
// TestServe_StreamingUsesChunkedFraming holds that framing.
//
// The events concatenate back to the exact fixture bytes, which is what lets a
// caller assert byte-exact forwarding.
func writeSSE(responseWriter http.ResponseWriter, sseBody []byte) {
	flusher, isFlusher := responseWriter.(http.Flusher)
	if !isFlusher {
		http.Error(responseWriter, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	responseWriter.Header().Set("Content-Type", contentTypeSSE)
	responseWriter.WriteHeader(http.StatusOK)

	trimmed := bytes.TrimRight(sseBody, "\n")
	for _, event := range bytes.Split(trimmed, eventSeparator) {
		// Each event is framed in a buffer of its own. Appending the
		// separator onto the split subslice would write INTO the loaded
		// fixture, because bytes.Split caps every element to its own length
		// except the last, whose capacity runs to the end of the file the
		// fixture was read from. Concurrent requests would then write that
		// shared memory while others read it.
		framed := make([]byte, 0, len(event)+len(eventSeparator))
		framed = append(framed, event...)
		framed = append(framed, eventSeparator...)
		if _, err := responseWriter.Write(framed); err != nil {
			return
		}
		flusher.Flush()
	}
}

// checkLoopbackAddr refuses any bind that is not a literal loopback address.
// The mock only ever serves already-public sanitized fixtures, so exposure
// would be minor, but a benchmark advertised as loopback-only should be one.
// Hostnames including "localhost" are rejected for the same reason
// internal/config rejects them for plaintext upstreams: the bind happens later
// through the OS resolver, and a literal address cannot be redirected.
func checkLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr %q must be host:port: %w", addr, err)
	}
	hostIP := net.ParseIP(host)
	if hostIP == nil || !hostIP.IsLoopback() {
		return errors.New("addr must use a literal loopback address such as 127.0.0.1")
	}
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:19099", "loopback address to listen on")
	fixturesDir := flag.String("fixtures", "", "path to the repo testdata/fixtures directory")
	flag.Parse()

	if strings.TrimSpace(*fixturesDir) == "" {
		log.Fatal("mockprovider: --fixtures is required")
	}
	if err := checkLoopbackAddr(*addr); err != nil {
		log.Fatalf("mockprovider: %v", err)
	}
	fixtures, err := loadFixtures(*fixturesDir)
	if err != nil {
		log.Fatalf("mockprovider: %v", err)
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(fixtures),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("mockprovider: listen %s: %v", *addr, err)
	}
	// One startup line so an orchestrator can wait for readiness, carrying the
	// bound address so a port of 0 is still discoverable. Nothing is logged
	// per request.
	log.Printf("mockprovider listening on %s", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("mockprovider: serve: %v", err)
	}
}
