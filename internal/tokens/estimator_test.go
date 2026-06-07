package tokens

import "testing"

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
