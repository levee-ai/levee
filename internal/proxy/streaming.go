package proxy

import (
	"bufio"
	"bytes"
	"net/http"
)

var (
	dataPrefix  = []byte("data: ")
	eventPrefix = []byte("event: ")
	newline     = []byte("\n")

	// Terminal markers
	openAIDone         = []byte("data: [DONE]")
	anthropicStop      = []byte("event: message_stop")
	openAIResponseDone = []byte("event: response.completed")
)

// streamState tracks state accumulated while forwarding an SSE stream. The
// usage fields are populated by inspectUsage (usage.go); the lifecycle fields
// by streamResponse (this file).
type streamState struct {
	provider          string
	lastEventType     string
	completedNormally bool
	scanErr           error
	endReason         streamEndReason

	// Usage extraction.
	inputTokens           int64
	outputTokens          int64
	sawAuthoritativeUsage bool

	// contentBytes accumulates forwarded content-payload byte length on the hot
	// path (no per-event parse). Feeds the fallback estimate when no
	// authoritative usage arrives.
	contentBytes int64
}

// streamEndReason classifies why the scan loop ended, which drives the
// reconciliation decision (reconcile.go).
type streamEndReason int

const (
	endNormal           streamEndReason = iota // terminal marker seen, clean EOF
	endUpstreamDrop                            // clean EOF, no terminal marker
	endIdleTimeout                             // idle watchdog fired
	endClientDisconnect                        // downstream client went away
	endScanError                               // scanner error (overflow, read error)
)

// hopByHopHeaders lists headers that must not be forwarded between connections.
// Per RFC 9110 section 7.6.1 and security best practices.
var hopByHopHeaders = map[string]bool{
	"Transfer-Encoding":  true,
	"Connection":         true,
	"Keep-Alive":         true,
	"Upgrade":            true,
	"Proxy-Authenticate": true,
	"Proxy-Authorization": true,
	"Proxy-Connection":   true,
	"Te":                 true,
	"Trailer":            true,
}

// streamResponse forwards an SSE stream from the upstream response to the
// client writer. It flushes at event boundaries (blank lines) and tracks
// terminal markers for budget reconciliation.
func streamResponse(w http.ResponseWriter, resp *http.Response) *streamState {
	state := &streamState{}

	// Copy upstream headers, skipping hop-by-hop.
	for key, vals := range resp.Header {
		if hopByHopHeaders[key] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}

	// Set required SSE headers (override any upstream values).
	// Note: Connection and Keep-Alive are NOT set here. They are hop-by-hop
	// headers (RFC 9113 section 8.2.2 forbids them in HTTP/2), and Go's HTTP
	// server manages connection persistence at the transport layer.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Content-Length")

	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		flusher = noopFlusher{}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 8*1024), 4*1024*1024)

	// TODO(session-6): Check r.Context().Err() periodically to detect client
	// disconnect proactively, rather than relying solely on w.Write() failure.
	// The write-error path detects disconnect one event late. Once budget
	// enforcement arrives, tokens consumed between disconnect and detection
	// are charged to the agent (~10-50 tokens per event, acceptable for MVP).
	for scanner.Scan() {
		line := scanner.Bytes()

		// Track event type for Anthropic multi-line format.
		if bytes.HasPrefix(line, eventPrefix) {
			state.lastEventType = string(bytes.TrimSpace(line[len(eventPrefix):]))
		}

		// Check terminal markers.
		if isTerminalMarker(line, state) {
			state.completedNormally = true
		}

		// Write line + newline. Break on write error.
		if _, err := w.Write(line); err != nil {
			break
		}
		if _, err := w.Write(newline); err != nil {
			break
		}

		// Flush at event boundaries (blank line).
		if len(line) == 0 {
			flusher.Flush()
		}
	}

	// Record scanner error (e.g., bufio.ErrTooLong for lines exceeding 4MB).
	state.scanErr = scanner.Err()

	return state
}

// isTerminalMarker returns true if the line matches a known stream termination
// signal from any supported provider.
func isTerminalMarker(line []byte, state *streamState) bool {
	trimmed := bytes.TrimRight(line, " \t\r")

	// OpenAI chat completions: "data: [DONE]"
	if bytes.HasPrefix(trimmed, dataPrefix) && bytes.Equal(trimmed, openAIDone) {
		return true
	}
	// Anthropic: "event: message_stop"
	if bytes.HasPrefix(trimmed, eventPrefix) && bytes.Equal(trimmed, anthropicStop) {
		return true
	}
	// OpenAI Responses API: "event: response.completed"
	if bytes.HasPrefix(trimmed, eventPrefix) && bytes.Equal(trimmed, openAIResponseDone) {
		return true
	}

	return false
}

// noopFlusher satisfies http.Flusher for writers that do not support flushing.
type noopFlusher struct{}

func (noopFlusher) Flush() {}
