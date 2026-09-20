// Command mockprovider is a zero-latency stand-in for an LLM provider API. It
// replays fixtures recorded from the real OpenAI and Anthropic APIs so a
// benchmark measures proxy overhead rather than provider variance. It never
// synthesizes a response body: a body without a usage field would make the proxy
// forfeit its reservation on every request instead of reconciling, which would
// silently measure the wrong code path.
//
// It binds loopback only and never logs per request. Its per-request work is one
// read of the request body plus one bounded scan of it, which is what lets a
// direct-to-mock cell at a 32KB prompt establish the floor. That work has to stay
// small because the enforce versus passthrough signal the experiment resolves is
// only 10 to 20us, so the measured costs sit at the two functions that pay them,
// requestIsStreaming and readRequestBody.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// Content types recorded at capture time. The authoritative values live in the
// metadata block of testdata/fixtures/README.md, which the proxy replay tests
// parse and which this package's tests hold these constants against. The mock has
// to serve exactly what the real providers served, because the proxy selects its
// streaming code path by looking for text/event-stream in the Content-Type.
const (
	contentTypeJSON = "application/json"
	contentTypeSSE  = "text/event-stream; charset=utf-8"
)

// maxRequestBody bounds the body read so a misdirected client cannot balloon
// memory. Benchmark request bodies are tens of kilobytes at most. A body that
// hits this bound arrives truncated, which looksLikeJSONObject then rejects.
const maxRequestBody = 1 << 20

// eventSeparator is the SSE blank-line boundary between events.
var eventSeparator = []byte("\n\n")

// fixtureSet holds the four recorded bodies, read once at startup, along with
// the identity this process reports on /healthz.
type fixtureSet struct {
	openAIJSON    []byte
	openAISSE     []byte
	anthropicJSON []byte
	anthropicSSE  []byte
	identity      fixtureIdentity
	identityJSON  string
}

// loadFixtures reads the four recorded bodies from dir, which is the repo's
// testdata/fixtures tree. Any missing file is fatal and names itself: a
// partially loaded fixture set would produce a silently wrong benchmark.
//
// There is no go:embed here on purpose. Embed patterns cannot traverse "..",
// and the go tool ignores testdata directories, so the path arrives as a flag.
func loadFixtures(dir string) (*fixtureSet, error) {
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve fixtures dir %q: %w", dir, err)
	}
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
		body, err := os.ReadFile(filepath.Join(absoluteDir, fixture.relativePath))
		if err != nil {
			return nil, fmt.Errorf("read fixture %s: %w", fixture.relativePath, err)
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("fixture %s is empty", fixture.relativePath)
		}
		// A stream that does not split on blank lines goes out as one write, so
		// the proxy would see the whole response in a single read and measure a
		// path production never takes. internal/proxy/fixture_test.go
		// serveSSEFixture guards the same way.
		if fixture.streaming {
			if count := sseEventCount(body); count < 2 {
				return nil, fmt.Errorf("fixture %s split into %d events, blank-line framing is broken (CRLF fixture?)", fixture.relativePath, count)
			}
		}
		*fixture.target = body
	}

	set.identity = describeFixtures(absoluteDir, set)
	encoded, err := json.Marshal(set.identity)
	if err != nil {
		return nil, fmt.Errorf("encode fixture identity: %w", err)
	}
	// Encoded once so the readiness endpoint stays a single write.
	set.identityJSON = string(encoded) + "\n"
	return set, nil
}

// fixtureIdentity is the /healthz payload. It names what this process is actually
// serving, which is the difference between a fresh mock and an orphan from an
// earlier run still holding the port. An orphan is otherwise invisible: the new
// process dies on bind, the readiness poll succeeds on its first try, and the
// orphan serves the results. Fixture bytes carry the token usage that drives
// budget accounting, so an orphan loaded from a different fixtures path changes
// the code path under measurement while every sanity band still passes.
type fixtureIdentity struct {
	Status    string `json:"status"`
	Directory string `json:"fixtures_dir"`
	// Digest is SHA-256 over the four fixture bodies concatenated in fixtureLengths
	// field order. A concatenation digest alone cannot see a byte moved across a
	// fixture boundary, so the four lengths travel with it.
	Digest string         `json:"fixtures_digest"`
	Bytes  fixtureLengths `json:"fixture_bytes"`
}

// fixtureLengths carries each fixture's byte length in the digest order.
type fixtureLengths struct {
	OpenAIJSON    int `json:"openai_json"`
	OpenAISSE     int `json:"openai_sse"`
	AnthropicJSON int `json:"anthropic_json"`
	AnthropicSSE  int `json:"anthropic_sse"`
}

// describeFixtures digests a loaded set. Order is fixed by the slice below and
// must stay in step with the fixtureLengths field order.
func describeFixtures(absoluteDir string, set *fixtureSet) fixtureIdentity {
	digest := sha256.New()
	for _, body := range [][]byte{set.openAIJSON, set.openAISSE, set.anthropicJSON, set.anthropicSSE} {
		// hash.Hash never reports a write error.
		_, _ = digest.Write(body)
	}
	return fixtureIdentity{
		Status:    "ok",
		Directory: absoluteDir,
		Digest:    hex.EncodeToString(digest.Sum(nil)),
		Bytes: fixtureLengths{
			OpenAIJSON:    len(set.openAIJSON),
			OpenAISSE:     len(set.openAISSE),
			AnthropicJSON: len(set.anthropicJSON),
			AnthropicSSE:  len(set.anthropicSSE),
		},
	}
}

// sseEventCount reports how many blank-line delimited events a recorded stream
// carries. The trailing terminator is trimmed first so it does not count as an
// empty final event.
func sseEventCount(sseBody []byte) int {
	return len(bytes.Split(bytes.TrimRight(sseBody, "\n"), eventSeparator))
}

// streamFlagKey is the only part of a request body the mock inspects. It is
// quoted on both sides so a match can only be an object key, never text a prompt
// happened to carry.
var streamFlagKey = []byte(`"stream"`)

// jsonWhitespace is the whitespace JSON permits around a colon, so both the
// compact "stream":true JSON.stringify emits and the "stream": true an indenting
// encoder emits are accepted.
const jsonWhitespace = " \t\r\n"

var (
	trueLiteral  = []byte("true")
	falseLiteral = []byte("false")
)

// requestIsStreaming reports whether the request body sets the stream flag.
//
// This mock sits on the measurement path, so the check has to stay cheap. It
// replaced a json.Unmarshal of the whole body, which BenchmarkRequestSelection
// measured at 93 percent of the per-request pre-write work at a 32KB prompt: the
// parse walked the entire document, allocating a copy of the 32KB prompt string
// it then discarded, to read one boolean.
//
// The scan stops at the first stream key, and benchmarks/k6/overhead.js puts the
// flag ahead of the prompt, so the common case never reaches the filler. A body
// that puts the flag behind the prompt costs the full crossing, measured at 11us
// at 32KB against the parse's 133us, and no client of this mock sends that order.
//
// The filler prompt text cannot fool the scan. JSON requires a double quote
// inside a string to be escaped, so filler carrying the literal "stream":true
// reaches the wire as \"stream\":true and cannot match a needle whose leading
// byte is an unescaped quote. Verified against both encoders in play, Go's
// encoding/json and the JSON.stringify k6 runs, and held by
// TestServe_FlagTextInsideThePromptIsNotStreaming.
//
// A nested object carrying its own stream key would still match. No client of
// this mock sends one, and the first key whose value is a boolean literal wins.
func requestIsStreaming(body []byte) bool {
	remaining := body
	for {
		index := bytes.Index(remaining, streamFlagKey)
		if index < 0 {
			return false
		}
		remaining = remaining[index+len(streamFlagKey):]
		value := bytes.TrimLeft(remaining, jsonWhitespace)
		if len(value) == 0 || value[0] != ':' {
			continue
		}
		value = bytes.TrimLeft(value[1:], jsonWhitespace)
		switch {
		case bytes.HasPrefix(value, trueLiteral):
			return true
		case bytes.HasPrefix(value, falseLiteral):
			return false
		}
	}
}

// readRequestBody reads the whole body in one allocation when the client declared
// a length, which every client of this mock does.
//
// io.ReadAll cannot be given a size, so it grows its buffer as it reads. Garbage
// on the mock side still costs the experiment, because the resulting GC pauses
// land in the tail percentiles of every cell measured through this process.
// Measured by BenchmarkRequestSelection at 32KB: 9.2us across 16 allocations and
// 89KB with io.ReadAll, 2.8us across 2 allocations and 41KB with the declared
// length.
func readRequestBody(request *http.Request) ([]byte, error) {
	limited := io.LimitReader(request.Body, maxRequestBody)
	// A chunked request reports -1, and one declaring more than the bound gets
	// read up to the bound and rejected for its missing closing brace.
	if request.ContentLength <= 0 || request.ContentLength > maxRequestBody {
		return io.ReadAll(limited)
	}
	body := make([]byte, request.ContentLength)
	// net/http caps the body reader at the declared length, so a short read means
	// the client stopped early. Returning the error makes that a 400 rather than a
	// silently short body.
	if _, err := io.ReadFull(limited, body); err != nil {
		return nil, err
	}
	return body, nil
}

// looksLikeJSONObject is the structural check that stands in for the parse the
// mock no longer performs. A body that is not JSON has to fail loudly rather than
// be served as a request with no stream flag. Requiring the closing brace also
// catches a body truncated at maxRequestBody, which would otherwise select the
// non-streaming fixture.
func looksLikeJSONObject(body []byte) bool {
	trimmed := bytes.Trim(body, jsonWhitespace)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
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
		responseWriter.Header().Set("Content-Type", contentTypeJSON)
		responseWriter.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(responseWriter, fixtures.identityJSON)
	})
	return mux
}

// serveFixture answers with the streaming or non-streaming fixture depending on
// the request's stream field.
func serveFixture(responseWriter http.ResponseWriter, request *http.Request, jsonBody, sseBody []byte) {
	body, err := readRequestBody(request)
	if err != nil {
		http.Error(responseWriter, "read request body", http.StatusBadRequest)
		return
	}
	if !looksLikeJSONObject(body) {
		http.Error(responseWriter, "request body is not a JSON object", http.StatusBadRequest)
		return
	}
	if requestIsStreaming(body) {
		writeSSE(responseWriter, sseBody)
		return
	}
	responseWriter.Header().Set("Content-Type", contentTypeJSON)
	responseWriter.WriteHeader(http.StatusOK)
	_, _ = responseWriter.Write(jsonBody)
}

// writeSSE replays a recorded stream one event per write plus flush, with no
// pacing. The per-event flush is load bearing: it produces distinct TCP segments
// so the proxy performs a real read and forward cycle per event rather than seeing
// the whole stream in one read. Measured on the OpenAI fixture it yields
// Transfer-Encoding chunked with six wire chunks, one per event, while dropping it
// yields one buffered Content-Length body. TestServe_StreamingUsesChunkedFraming
// holds that framing, and the events concatenate back to the exact fixture bytes.
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
// Hostnames including "localhost" are rejected for the same reason internal/config
// rejects them for plaintext upstreams: the bind happens later through the OS
// resolver, and a literal address cannot be redirected.
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
		Handler:           newHandler(fixtures),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("mockprovider: listen %s: %v", *addr, err)
	}
	// One startup line so an orchestrator can wait for readiness. It carries the
	// bound address so a port of 0 is still discoverable, plus the resolved
	// fixtures path and digest so a finished run's log says what was served.
	log.Printf("mockprovider listening on %s, fixtures %s, digest %s",
		listener.Addr(), fixtures.identity.Directory, fixtures.identity.Digest)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("mockprovider: serve: %v", err)
	}
}
