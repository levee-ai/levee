// Package logcost measures what levee's structured logging costs per request.
//
// The enforcement figure annotates the logging share of the enforce minus
// passthrough delta separately, so a reader never mistakes it for enforcement
// work. That annotation has to be MEASURED on the host that produced the run,
// for the same reason every other component in microbench.txt is measured per
// run: a constant copied from another machine becomes part of a published figure
// and then nothing ever re-checks it. No benchmark elsewhere in this repository
// measures slog, and the harness work is not allowed to touch internal/, so the
// measurement lives here and run.sh captures it into microbench.txt every run.
//
// This package is test-only on purpose. Nothing in levee calls it and nothing
// imports it, so a non-test file would put code in the shipped binary that
// exists only to be benchmarked. Verified at go1.26.3 on this host: both
// go build ./... and go vet ./... accept a directory whose only Go file is a
// test file.
//
// Nothing here imports levee either. The point is to measure slog at the shipped
// CONFIGURATION, and the handler construction and the attribute shapes are
// deliberately duplicated from the call sites rather than driven through them.
// A duplicate fails loudly in review when a call site gains an attribute,
// whereas a benchmark that reached into the real proxy would silently track the
// change and keep reporting a number the figure no longer describes.
package logcost

import (
	"log/slog"
	"os"
	"testing"
)

// The attribute values a benchmark cell actually produces, so the measured cost
// carries real key and value lengths rather than placeholder ones.
//
// A proxied cell posts to /openai/v1/chat/completions (benchmarks/harness/run.sh
// line 639), which the proxy splits into the provider name and the remaining
// upstream path before logging them. It identifies itself with the
// X-Levee-Agent: bench header (benchmarks/k6/overhead.js line 51, matching
// header_value in both benchmarks/configs templates).
const (
	cellProvider  = "openai"
	cellPath      = "/v1/chat/completions"
	cellAgentName = "bench"
	cellStatus    = 200
	cellStreaming = false
)

// The token counts a non-streaming small-payload cell settles on, measured
// rather than invented so the logged integers have the right magnitude.
//
// estimate is 36, from the real estimator against the body overhead.js builds at
// the 150-byte prompt size: 20 input tokens plus the 16-token output reserve it
// takes from max_tokens. actual is 16, the total_tokens in
// testdata/fixtures/openai/chat-completion.json, which the mock replays
// unchanged for every payload size. So drift is negative here, and it is logged
// as a signed value, which is worth keeping in the benchmark because a negative
// number encodes one byte wider than its magnitude suggests.
const (
	cellTokenEstimate int64 = 36
	cellActualTokens  int64 = 16
	cellReconcileReason     = "reconciled"
)

// shippedLogger builds the logger cmd/levee/main.go builds, writing where
// benchmarks/harness/run.sh points it.
//
// Both halves matter and the destination matters more. cmd/levee/main.go line
// 130 constructs a JSON handler at slog.LevelInfo on os.Stdout, and Config has
// no logging section, so no run can turn these lines off or change their level.
// run.sh line 312 starts levee with stdout redirected to /dev/null, so the write
// under measurement goes to a real /dev/null file descriptor.
//
// Measuring against io.Discard instead understates the per-line cost by about 81
// percent, because io.Discard skips the write syscall that the shipped path
// pays. The encoder choice is minor by comparison: text versus JSON moves the
// number by only 7.6 percent. Anyone re-measuring has to get the destination
// right before worrying about the handler.
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

// upstreamResponseLine is internal/proxy/proxy.go line 271, the one line both
// cells write. Four attributes, two strings and an int and a bool.
func upstreamResponseLine(logger *slog.Logger) {
	logger.Info("upstream response",
		"provider", cellProvider,
		"path", cellPath,
		"status", cellStatus,
		"streaming", cellStreaming)
}

// budgetReservedLine is internal/proxy/enforcement.go line 253, the admission
// line only an enforced request writes. Three attributes, two strings and an
// int64.
func budgetReservedLine(logger *slog.Logger) {
	logger.Info("Budget reserved",
		"agent", cellAgentName,
		"action", "reserve",
		"tokens", cellTokenEstimate)
}

// budgetReconciledLine is internal/proxy/reconcile.go line 190, the settlement
// line only an enforced request writes. Six attributes and the widest of the
// three, which is why the composites are not just three times one shape.
//
// This is also the only shape that allocates, and the reason is structural
// rather than incidental: log/slog carries the first five attributes in a fixed
// array inside the Record (nAttrsInline is 5 and front is [nAttrsInline]Attr in
// the stdlib record.go), so a sixth attribute spills to the heap. Adding an
// attribute to any of the other two shapes would cross the same line.
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
// request: ONE line.
//
// A passthrough agent has no budget block, so enforcement returns before Admit
// and settlement is skipped, which leaves only the upstream response line in
// internal/proxy/proxy.go that every forwarded request writes. The count was
// confirmed against the running binary for both streaming and non-streaming
// rather than read off the control flow.
//
// This benchmark deliberately calls the same single function as
// BenchmarkUpstreamResponseLine rather than aliasing it, because the gap between
// their two numbers is a free read on the measurement noise floor. They execute
// identical work, so any difference is host scheduling between benchmark slots.
// The first captured run on the reference host reported 1029 and 1433 ns/op for
// the pair, so the per-line figure carries tens of percent of run-to-run spread
// and the enforcement annotation should be read as roughly a microsecond per
// line rather than as a precise constant.
func BenchmarkPassthroughRequestLines(b *testing.B) {
	logger := shippedLogger(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		upstreamResponseLine(logger)
	}
}

// BenchmarkEnforceRequestLines is the whole logging cost of one enforced
// request: THREE lines.
//
// An admitted request logs the reservation before forwarding, the upstream
// response the passthrough cell also logs, and the settlement after the body is
// read. The rejection and error lines are alternatives to these rather than
// additions, and a benchmark cell that produced any of them would already have
// failed the run on its k6 thresholds, so the steady-state enforced request
// writes exactly these three. This benchmark minus the passthrough one is the
// logging component the enforcement figure annotates, and it is why band 3's
// window has to accommodate two extra lines that are not enforcement work.
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
