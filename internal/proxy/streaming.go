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

// streamState tracks state accumulated while forwarding an SSE stream.
type streamState struct {
	lastEventType    string
	completedNormally bool
}

// hopByHopHeaders lists headers that must not be forwarded from upstream.
var hopByHopHeaders = map[string]bool{
	"Transfer-Encoding": true,
	"Connection":        true,
	"Keep-Alive":        true,
	"Upgrade":           true,
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Content-Length")

	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		flusher = noopFlusher{}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 8*1024), 4*1024*1024)

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
