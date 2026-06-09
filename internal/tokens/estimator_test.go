package tokens

import (
	"sync"
	"testing"
)

func TestEstimate_Anthropic_UsesCharacterHeuristic(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	// 40 characters of content -> ceil(40/4) = 10 input tokens.
	body := []byte(`{"model":"claude-3-opus","max_tokens":100,"messages":[{"role":"user","content":"0123456789012345678901234567890123456789"}]}`)
	got := estimator.Estimate("claude-3-opus", body)
	// input 10 + output reserve 100 = 110.
	if got != 110 {
		t.Fatalf("Estimate: got %d, want 110", got)
	}
}

func TestEstimate_MaxTokensAbsent_UsesDefault(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	body := []byte(`{"model":"claude-3-opus","messages":[{"role":"user","content":"hello world"}]}`)
	got := estimator.Estimate("claude-3-opus", body)
	// input ceil(11/4)=3 + default output 4096 = 4099.
	if got != 4099 {
		t.Fatalf("Estimate: got %d, want 4099 (defaultMaxOutput fallback)", got)
	}
}

// TestExtractInputText_Paths pins the three extraction paths this estimator
// introduces. All cases use an Anthropic model so the input math is the
// deterministic 4-characters-per-token heuristic with no tiktoken involved.
func TestExtractInputText_Paths(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	cases := []struct {
		name string
		body string
		want int64
	}{
		{
			// No messages array: the whole 25-byte body is counted.
			// ceil(25/4)=7 input + default output 4096 = 4103.
			name: "no messages array falls back to whole body",
			body: `{"model":"claude-3-opus"}`,
			want: 4103,
		},
		{
			// Block-array content: only the text fields ("abcd" + "efghij")
			// are summed, the image block contributes nothing. Extracted text
			// is "abcdefghij" (10 chars). ceil(10/4)=3 input + max_tokens 50 = 53.
			name: "block array content sums text fields",
			body: `{"model":"claude-3-opus","max_tokens":50,"messages":[{"role":"user","content":[{"type":"text","text":"abcd"},{"type":"image"},{"type":"text","text":"efghij"}]}]}`,
			want: 53,
		},
		{
			// Top-level system string is included: "sys" + content "hi" gives
			// "syshi" (5 chars). ceil(5/4)=2 input + max_tokens 7 = 9.
			name: "top-level system string is counted",
			body: `{"model":"claude-3-opus","max_tokens":7,"system":"sys","messages":[{"role":"user","content":"hi"}]}`,
			want: 9,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := estimator.Estimate("claude-3-opus", []byte(testCase.body))
			if got != testCase.want {
				t.Fatalf("Estimate: got %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestEstimate_OpenAI_UsesTiktoken(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	// "The quick brown fox" encodes to a known small token count under cl100k.
	body := []byte(`{"model":"gpt-4","max_tokens":50,"messages":[{"role":"user","content":"The quick brown fox"}]}`)
	got := estimator.Estimate("gpt-4", body)
	// 4 input tokens for "The quick brown fox" + 50 output reserve.
	if got != 54 {
		t.Fatalf("Estimate: got %d, want 54", got)
	}
}

func TestEstimate_UnknownModel_FallsBackToConfiguredEncoding(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	body := []byte(`{"model":"some-future-model-x","max_tokens":10,"messages":[{"role":"user","content":"The quick brown fox"}]}`)
	got := estimator.Estimate("some-future-model-x", body)
	// Unknown model -> cl100k_base fallback -> 4 input + 10 output = 14.
	if got != 14 {
		t.Fatalf("Estimate: got %d, want 14 (fallback encoding)", got)
	}
}

// TestEstimate_OutOfRangeMaxTokens pins the overflow guard. A max_tokens at or
// beyond the int64 ceiling (literal or scientific notation, both of which gjson
// saturates to MaxInt64) must not wrap the sum negative. It falls back to the
// default reserve so the estimate stays non-negative and bounded, which keeps
// budget enforcement honest. "hi" is 1 input token under cl100k_base, so the
// bounded estimate is 1 + defaultMaxOutput (4096) = 4097.
func TestEstimate_OutOfRangeMaxTokens(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	const wantBounded = 1 + 4096 // input "hi" + defaultMaxOutput fallback
	cases := []struct {
		name string
		body string
	}{
		{
			name: "int64 max literal saturates and falls back to default",
			body: `{"model":"gpt-4","max_tokens":9223372036854775807,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "scientific notation beyond int64 falls back to default",
			body: `{"model":"gpt-4","max_tokens":1e30,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := estimator.Estimate("gpt-4", []byte(testCase.body))
			if got < 0 {
				t.Fatalf("Estimate: got %d, want non-negative (overflow wrapped the sum)", got)
			}
			if got != wantBounded {
				t.Fatalf("Estimate: got %d, want %d (input + defaultMaxOutput)", got, wantBounded)
			}
		})
	}
}

// TestEstimate_LegitimateLargeMaxTokens proves the overflow guard does not
// over-clamp a realistic large request. A max_tokens of 1000000 (1e6) is well
// below the ceiling, so it passes through unchanged: 1 input token for "hi" plus
// the full 1000000 reserve.
func TestEstimate_LegitimateLargeMaxTokens(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	body := []byte(`{"model":"gpt-4","max_tokens":1000000,"messages":[{"role":"user","content":"hi"}]}`)
	got := estimator.Estimate("gpt-4", body)
	const want = 1 + 1000000 // input "hi" + full max_tokens (not clamped)
	if got != want {
		t.Fatalf("Estimate: got %d, want %d (legitimate max_tokens must pass through unclamped)", got, want)
	}
}

// TestEstimate_ConcurrentReuse races first-time encoder construction across
// goroutines on a freshly constructed estimator whose cache starts cold. The
// two models map to different encodings (gpt-4 to cl100k_base, gpt-4o to
// o200k_base), so both distinct encodings build concurrently and the race
// detector exercises the double-checked write-lock single-flight branch from
// cold. The unknown model adds the configured fallback path. Expected counts
// were measured against the real encodings: "The quick brown fox" is 4 input
// tokens under both cl100k_base and o200k_base, plus each request's reserve.
func TestEstimate_ConcurrentReuse(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	cl100kBody := []byte(`{"model":"gpt-4","max_tokens":50,"messages":[{"role":"user","content":"The quick brown fox"}]}`)
	o200kBody := []byte(`{"model":"gpt-4o","max_tokens":60,"messages":[{"role":"user","content":"The quick brown fox"}]}`)
	unknownBody := []byte(`{"model":"some-future-model-x","max_tokens":10,"messages":[{"role":"user","content":"The quick brown fox"}]}`)

	var waitGroup sync.WaitGroup
	for i := 0; i < 64; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if got := estimator.Estimate("gpt-4", cl100kBody); got != 54 {
				t.Errorf("Estimate(gpt-4, cl100k_base): got %d, want 54", got)
			}
			if got := estimator.Estimate("gpt-4o", o200kBody); got != 64 {
				t.Errorf("Estimate(gpt-4o, o200k_base): got %d, want 64", got)
			}
			if got := estimator.Estimate("some-future-model-x", unknownBody); got != 14 {
				t.Errorf("Estimate(unknown, fallback): got %d, want 14", got)
			}
		}()
	}
	waitGroup.Wait()
}

func TestEstimateInput_ExcludesOutputReserve(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	body := []byte(`{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hello world"}]}`)

	input := estimator.EstimateInput("gpt-4", body)
	full := estimator.Estimate("gpt-4", body)

	if input <= 0 {
		t.Fatalf("EstimateInput returned %d, want > 0", input)
	}
	// Full estimate includes the 4096 output reserve, so it must exceed input alone.
	if full <= input {
		t.Errorf("Estimate (%d) should exceed EstimateInput (%d) by the output reserve", full, input)
	}
}

func TestEstimateInput_AnthropicHeuristic(t *testing.T) {
	estimator := NewEstimator("cl100k_base")
	body := []byte(`{"model":"claude-3-opus","messages":[{"role":"user","content":"hello"}]}`)
	input := estimator.EstimateInput("claude-3-opus", body)
	if input <= 0 {
		t.Fatalf("EstimateInput returned %d for Anthropic model, want > 0", input)
	}
}

func BenchmarkEstimate_OpenAI(b *testing.B) {
	estimator := NewEstimator("cl100k_base")
	body := []byte(`{"model":"gpt-4","max_tokens":1024,"messages":[{"role":"user","content":"Summarize the following document in three sentences for a busy executive."}]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = estimator.Estimate("gpt-4", body)
	}
}

func BenchmarkEstimate_Anthropic(b *testing.B) {
	estimator := NewEstimator("cl100k_base")
	body := []byte(`{"model":"claude-3-opus","max_tokens":1024,"messages":[{"role":"user","content":"Summarize the following document in three sentences for a busy executive."}]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = estimator.Estimate("claude-3-opus", body)
	}
}
