// Package logcost measures what levee's structured logging costs per request.
//
// The enforcement figure annotates the logging share of the enforce minus
// passthrough delta separately, so a reader never mistakes it for enforcement
// work. That annotation has to be measured on the host that produced the run,
// because a constant copied from another machine becomes part of a published
// figure and then nothing re-checks it. No benchmark elsewhere in this repository
// measures slog and the harness is not allowed to touch internal/, so the
// measurement lives here and run.sh captures it into microbench.txt every run.
//
// This package is test-only on purpose. Nothing in levee calls it and nothing
// imports it, so a non-test file would put code in the shipped binary that exists
// only to be benchmarked. Verified at go1.26.3 on this host: both go build ./...
// and go vet ./... accept a directory whose only Go file is a test file.
//
// Nothing here imports levee either. The point is to measure slog at the shipped
// configuration, so the handler construction and the attribute shapes are
// deliberately duplicated from the call sites rather than driven through them. A
// duplicate fails loudly in review when a call site gains an attribute, whereas a
// benchmark that reached into the real proxy would silently track the change and
// keep reporting a number the figure no longer describes.
package logcost

import (
	"log/slog"
	"os"
	"testing"
)

// The attribute values a benchmark cell actually produces, so the measured cost
// carries real key and value lengths rather than placeholder ones.
//
// A proxied cell posts to the /openai/v1/chat/completions target run_matrix builds
// in benchmarks/harness/run.sh, which the proxy splits into the provider name and
// the remaining upstream path before logging them. It identifies itself with the
// X-Levee-Agent: bench header from the requestParams block in
// benchmarks/k6/overhead.js, matching header_value in both benchmarks/configs
// templates.
const (
	cellProvider  = "openai"
	cellPath      = "/v1/chat/completions"
	cellAgentName = "bench"
	cellStatus    = 200
	cellStreaming = false
)

// The token counts a non-streaming small-payload cell settles on, measured rather
// than invented so the logged integers have the right magnitude.
//
// estimate is 36, from the real estimator against the body overhead.js builds at
// the 150-byte prompt size: 20 input tokens plus the 16-token output reserve it
// takes from max_tokens. actual is 16, the total_tokens in
// testdata/fixtures/openai/chat-completion.json, which the mock replays unchanged
// for every payload size. So drift is negative here and is logged as a signed
// value, which is worth keeping because a negative number encodes one byte wider
// than its magnitude suggests.
const (
	cellTokenEstimate   int64 = 36
	cellActualTokens    int64 = 16
	cellReconcileReason       = "reconciled"
)

// shippedLogger builds the logger runServe in cmd/levee/main.go builds, writing
// where start_levee in benchmarks/harness/run.sh points it.
//
// Both halves matter and the destination matters more. runServe constructs a JSON
// handler at slog.LevelInfo on os.Stdout, and Config has no logging section, so no
// run can turn these lines off or change their level. start_levee starts levee
// with stdout redirected to /dev/null, so the write under measurement goes to a
// real /dev/null file descriptor.
//
// Measuring against io.Discard instead understates the per-line cost by about 81
// percent, because io.Discard skips the write syscall the shipped path pays. The
// encoder choice is minor by comparison: text versus JSON moves the number by only
// 7.6 percent.
func shippedLogger(b *testing.B) *slog.Logger {
	b.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatalf("open %s: %v", os.DevNull, err)
	}
	b.Cleanup(func() {
		if closeErr := devNull.Close(); closeErr != nil {
			b.Errorf("close %s: %v", os.DevNull, closeErr)
		}
	})
	return slog.New(slog.NewJSONHandler(devNull, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// upstreamResponseLine is the "upstream response" line Proxy.ServeHTTP writes in
// internal/proxy/proxy.go, the one line both cells write.
func upstreamResponseLine(logger *slog.Logger) {
	logger.Info("upstream response",
		"provider", cellProvider,
		"path", cellPath,
		"status", cellStatus,
		"streaming", cellStreaming)
}

// budgetReservedLine is the "Budget reserved" line Proxy.enforce writes in
// internal/proxy/enforcement.go, the admission line only an enforced request
// writes.
func budgetReservedLine(logger *slog.Logger) {
	logger.Info("Budget reserved",
		"agent", cellAgentName,
		"action", "reserve",
		"tokens", cellTokenEstimate)
}

// budgetReconciledLine is the "Budget reconciled" line Proxy.applyReconcile writes
// in internal/proxy/reconcile.go, the settlement line only an enforced request
// writes. Six attributes and the widest of the three, which is why the composites
// are not just three times one shape.
//
// This is also the only shape that allocates, and the reason is structural rather
// than incidental: log/slog carries the first five attributes in a fixed array
// inside the Record (nAttrsInline is 5 and front is [nAttrsInline]Attr in the
// stdlib record.go), so a sixth attribute spills to the heap. Adding an attribute
// to either of the other two shapes would cross the same line.
func budgetReconciledLine(logger *slog.Logger) {
	logger.Info("Budget reconciled",
		"agent", cellAgentName,
		"action", "reconcile",
		"estimate", cellTokenEstimate,
		"actual", cellActualTokens,
		"drift", cellActualTokens-cellTokenEstimate,
		"reason", cellReconcileReason)
}

func BenchmarkUpstreamResponseLine(b *testing.B) {
	logger := shippedLogger(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		upstreamResponseLine(logger)
	}
}

func BenchmarkBudgetReservedLine(b *testing.B) {
	logger := shippedLogger(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		budgetReservedLine(logger)
	}
}

func BenchmarkBudgetReconciledLine(b *testing.B) {
	logger := shippedLogger(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		budgetReconciledLine(logger)
	}
}

// BenchmarkPassthroughRequestLines is the whole logging cost of one passthrough
// request, which is one line. A passthrough agent has no budget block, so
// enforcement returns before Admit and settlement is skipped, leaving only the
// upstream response line every forwarded request writes. The count was confirmed
// against the running binary for both streaming and non-streaming rather than read
// off the control flow.
//
// This deliberately calls the same single function as BenchmarkUpstreamResponseLine
// rather than aliasing it, because the gap between their two numbers is a free read
// on the measurement noise floor. They execute identical work, so any difference is
// host scheduling between benchmark slots. The first captured run on the reference
// host reported 1029 and 1433 ns/op for the pair, so the per-line figure should be
// read as roughly a microsecond rather than as a precise constant.
func BenchmarkPassthroughRequestLines(b *testing.B) {
	logger := shippedLogger(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		upstreamResponseLine(logger)
	}
}

// BenchmarkEnforceRequestLines is the whole logging cost of one enforced request,
// which is three lines: the reservation before forwarding, the upstream response
// the passthrough cell also logs, and the settlement after the body is read. The
// rejection and error lines are alternatives to these rather than additions, and a
// cell producing any of them would already have failed its k6 thresholds.
//
// This benchmark minus the passthrough one is the logging component the enforcement
// figure annotates, and it is why band 3's window has to accommodate two extra
// lines that are not enforcement work.
func BenchmarkEnforceRequestLines(b *testing.B) {
	logger := shippedLogger(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		budgetReservedLine(logger)
		upstreamResponseLine(logger)
		budgetReconciledLine(logger)
	}
}
