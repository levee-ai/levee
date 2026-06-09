package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runStream drives streamResponse against a live httptest upstream so the
// watchdog and context behavior are exercised against a real connection, not a
// canned io.Reader (which would return EOF instantly and never block a Scan).
func runStream(t *testing.T, idle time.Duration, handler http.HandlerFunc) (*streamState, *httptest.ResponseRecorder) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	request, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("upstream request: %v", err)
	}
	recorder := httptest.NewRecorder()
	state := streamResponse(recorder, request, response, providerOpenAI, idle)
	return state, recorder
}

func TestStreamResponse_NormalCompletionReconcilable(t *testing.T) {
	state, recorder := runStream(t, 5*time.Second, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":29,\"completion_tokens\":11}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		flusher.Flush()
	})
	if state.endReason != endNormal {
		t.Errorf("endReason = %v, want endNormal", state.endReason)
	}
	if !state.sawAuthoritativeUsage || state.inputTokens != 29 || state.outputTokens != 11 {
		t.Errorf("usage not extracted: saw=%v input=%d output=%d", state.sawAuthoritativeUsage, state.inputTokens, state.outputTokens)
	}
	if !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Error("expected full stream forwarded to client")
	}
}

func TestStreamResponse_IdleTimeoutForfeits(t *testing.T) {
	state, _ := runStream(t, 150*time.Millisecond, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		_, _ = io.WriteString(writer, "data: first\n\n")
		flusher.Flush()
		// Go silent well past the idle timeout.
		<-request.Context().Done()
	})
	if state.endReason != endIdleTimeout {
		t.Errorf("endReason = %v, want endIdleTimeout", state.endReason)
	}
}

func TestStreamResponse_SlowButAliveStaysOpen(t *testing.T) {
	// Emit one event every 90ms under a 150ms idle: the watchdog must keep
	// resetting and the stream must reach the terminal marker. This is the
	// ROADMAP "done when" assertion.
	state, recorder := runStream(t, 150*time.Millisecond, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(writer, "data: tick\n\n")
			flusher.Flush()
			time.Sleep(90 * time.Millisecond)
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		flusher.Flush()
	})
	if state.endReason != endNormal {
		t.Errorf("endReason = %v, want endNormal (slow but alive should not time out)", state.endReason)
	}
	if !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Error("slow-but-alive stream did not reach terminal marker")
	}
}

func TestStreamResponse_ContentBytesAccumulated(t *testing.T) {
	state, _ := runStream(t, 5*time.Second, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		_, _ = io.WriteString(writer, "data: hello\n\n") // payload "hello" = 5 bytes
		flusher.Flush()
		_, _ = io.WriteString(writer, "data: world\n\n") // payload "world" = 5 bytes
		flusher.Flush()
	})
	if state.contentBytes != 10 {
		t.Errorf("contentBytes = %d, want 10", state.contentBytes)
	}
	// No terminal marker and no usage: this is an upstream drop.
	if state.endReason != endUpstreamDrop {
		t.Errorf("endReason = %v, want endUpstreamDrop", state.endReason)
	}
}

func TestStreamResponse_ClientDisconnectForfeits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		_, _ = io.WriteString(writer, "data: first\n\n")
		flusher.Flush()
		<-request.Context().Done()
	}))
	defer upstream.Close()

	// Build a request whose context we cancel after the first event reaches the
	// streamResponse loop, simulating the downstream client going away.
	clientContext, clientCancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(clientContext, http.MethodGet, upstream.URL, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("upstream request: %v", err)
	}
	recorder := httptest.NewRecorder()

	go func() {
		time.Sleep(100 * time.Millisecond)
		clientCancel()
	}()

	state := streamResponse(recorder, request, response, providerOpenAI, 5*time.Second)
	if state.endReason != endClientDisconnect {
		t.Errorf("endReason = %v, want endClientDisconnect", state.endReason)
	}
}
