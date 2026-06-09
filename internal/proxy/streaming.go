package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"
)

var (
	dataPrefix  = []byte("data: ")
	eventPrefix = []byte("event: ")
	newline     = []byte("\n")

	// Terminal markers
	openAIDone         = []byte("data: [DONE]")
	anthropicStop      = []byte("event: message_stop")
	openAIResponseDone = []byte("event: response.completed")

	// doneSentinel is the bare [DONE] payload (after the "data: " prefix is
	// stripped), excluded from the content-byte heuristic accumulator.
	doneSentinel = []byte("[DONE]")
)

// streamState tracks state accumulated while forwarding an SSE stream. The
// usage fields are populated by inspectUsage (usage.go). The lifecycle fields
// are populated by streamResponse (this file).
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

// errIdleTimeout is the cancel cause set by the idle watchdog. After the scan
// loop, context.Cause is compared against it to distinguish an idle timeout
// from a client disconnect (context.Canceled). Verified: probes P1 and P3.
var errIdleTimeout = errors.New("levee: stream idle timeout")

// streamResponse forwards an SSE stream from the upstream response to the
// client, flushing at event boundaries. It runs the upstream read under a
// cancellable child of request.Context() so that a downstream client disconnect
// and the idle watchdog both unblock a blocked Scan. It inspects usage on the
// fast path and classifies why the stream ended for budget reconciliation.
//
// PRECONDITION: response is the upstream *http.Response whose Body was obtained
// from a request built with a context that is a child of request.Context(), so
// that cancelling here propagates a Body.Read cancellation. ServeHTTP builds
// the upstream request that way (Task 7). The watchdog reset and the
// client-disconnect / idle-timeout distinction are verified by probes P1 to P4.
func streamResponse(
	responseWriter http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	provider string,
	idleTimeout time.Duration,
) *streamState {
	state := &streamState{provider: provider}

	// Copy upstream headers, skipping hop-by-hop.
	for key, values := range response.Header {
		if hopByHopHeaders[key] {
			continue
		}
		for _, value := range values {
			responseWriter.Header().Add(key, value)
		}
	}
	responseWriter.Header().Set("Content-Type", "text/event-stream")
	responseWriter.Header().Set("Cache-Control", "no-cache")
	responseWriter.Header().Set("X-Accel-Buffering", "no")
	responseWriter.Header().Del("Content-Length")
	responseWriter.WriteHeader(response.StatusCode)

	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		flusher = noopFlusher{}
	}

	// Track why the stream ended. The watchdog sets the cancel cause to
	// errIdleTimeout before closing the body. A downstream client disconnect
	// propagates to streamContext as context.Canceled, which classifyStreamEnd
	// detects via context.Cause after the scan loop exits.
	streamContext, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)

	// The watchdog fires if no bytes arrive for idleTimeout. It records the cause
	// then closes the body directly, which unblocks the blocked scanner.Scan
	// regardless of which context the upstream connection was created under.
	// Closing the body is the load-bearing unblock: the connection read began
	// under the caller context before streamContext existed, so cancelling
	// streamContext alone does not abort it. The cause is set first so
	// classifyStreamEnd attributes the idle timeout rather than the resulting
	// closed-connection scan error. A watchdog fire that races a Reset forfeits
	// conservatively (Tenet 3), and activity is expected well inside the budget.
	watchdog := time.AfterFunc(idleTimeout, func() {
		cancel(errIdleTimeout)
		_ = response.Body.Close()
	})
	defer watchdog.Stop()

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 8*1024), 4*1024*1024)

	for scanner.Scan() {
		// Any received line (including Anthropic ping) is activity (probe P2).
		watchdog.Reset(idleTimeout)
		line := scanner.Bytes()

		if bytes.HasPrefix(line, eventPrefix) {
			state.lastEventType = string(bytes.TrimSpace(line[len(eventPrefix):]))
		}

		if bytes.HasPrefix(line, dataPrefix) {
			payload := line[len(dataPrefix):]
			// Accumulate content bytes for the fallback heuristic (no parse).
			// Exclude the [DONE] sentinel, which is not content.
			if !bytes.Equal(bytes.TrimRight(payload, " \t\r"), doneSentinel) {
				state.contentBytes += int64(len(payload))
			}
			if shouldParseUsage(payload) {
				inspectUsage(payload, state)
			}
		}

		if isTerminalMarker(line, state) {
			state.completedNormally = true
		}

		if _, err := responseWriter.Write(line); err != nil {
			break
		}
		if _, err := responseWriter.Write(newline); err != nil {
			break
		}
		if len(line) == 0 {
			flusher.Flush()
		}
	}

	state.scanErr = scanner.Err()
	state.endReason = classifyStreamEnd(state, streamContext)
	return state
}

// classifyStreamEnd determines why the scan loop ended. The cancel cause
// distinguishes the idle watchdog from a client disconnect (probe P3). A clean
// EOF with a terminal marker is normal. Without one it is an upstream drop. A
// scanner error (overflow, read error not caused by our cancel) is a scan error.
func classifyStreamEnd(state *streamState, streamContext context.Context) streamEndReason {
	switch context.Cause(streamContext) {
	case errIdleTimeout:
		return endIdleTimeout
	case context.Canceled:
		// Parent (request) context was cancelled: the downstream client went away.
		return endClientDisconnect
	}
	// Context was not cancelled. Look at the scanner outcome.
	if state.scanErr != nil {
		return endScanError
	}
	if state.completedNormally {
		return endNormal
	}
	return endUpstreamDrop
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
