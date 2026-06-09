package proxy

import "testing"

func TestShouldParseUsage(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"usage null intermediate chunk", `{"choices":[{"delta":{"content":"hi"}}],"usage":null}`, false},
		{"real usage chunk", `{"choices":[],"usage":{"prompt_tokens":29,"completion_tokens":11}}`, true},
		{"no usage field", `{"choices":[{"delta":{"content":"hi"}}]}`, false},
		{"done marker", `[DONE]`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := shouldParseUsage([]byte(testCase.data))
			if got != testCase.want {
				t.Errorf("shouldParseUsage(%s) = %v, want %v", testCase.data, got, testCase.want)
			}
		})
	}
}

func TestExtractOpenAIUsage(t *testing.T) {
	state := &streamState{provider: providerOpenAI}
	data := []byte(`{"choices":[],"usage":{"prompt_tokens":29,"completion_tokens":11,"total_tokens":40}}`)
	inspectUsage(data, state)
	if !state.sawAuthoritativeUsage {
		t.Fatal("expected sawAuthoritativeUsage = true")
	}
	if state.inputTokens != 29 || state.outputTokens != 11 {
		t.Errorf("got input=%d output=%d, want 29/11", state.inputTokens, state.outputTokens)
	}
}

func TestExtractOpenAIUsage_LastWins(t *testing.T) {
	state := &streamState{provider: providerOpenAI}
	inspectUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`), state)
	inspectUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":29,"completion_tokens":11}}`), state)
	if state.inputTokens != 29 || state.outputTokens != 11 {
		t.Errorf("expected last usage to win, got input=%d output=%d", state.inputTokens, state.outputTokens)
	}
}

func TestExtractAnthropicUsage_MessageStartThenDelta(t *testing.T) {
	state := &streamState{provider: providerAnthropic}
	state.lastEventType = "message_start"
	inspectUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1}}}`), state)
	if state.inputTokens != 25 {
		t.Errorf("after message_start: input=%d, want 25", state.inputTokens)
	}
	state.lastEventType = "message_delta"
	inspectUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":15}}`), state)
	if state.outputTokens != 15 {
		t.Errorf("after message_delta: output=%d, want 15", state.outputTokens)
	}
}

func TestExtractNonStreamingUsage(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		body       string
		wantTokens int64
		wantOK     bool
	}{
		{"openai total_tokens", providerOpenAI, `{"usage":{"prompt_tokens":29,"completion_tokens":11,"total_tokens":40}}`, 40, true},
		{"anthropic input+output", providerAnthropic, `{"usage":{"input_tokens":25,"output_tokens":15}}`, 40, true},
		{"missing usage", providerOpenAI, `{"id":"x","choices":[]}`, 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tokens, ok := extractNonStreamingUsage(testCase.provider, []byte(testCase.body))
			if ok != testCase.wantOK || tokens != testCase.wantTokens {
				t.Errorf("got (%d, %v), want (%d, %v)", tokens, ok, testCase.wantTokens, testCase.wantOK)
			}
		})
	}
}

func TestHeuristicOutputTokens(t *testing.T) {
	// 12 content bytes / 4 chars-per-token = 3 tokens.
	if got := heuristicOutputTokens(12); got != 3 {
		t.Errorf("heuristicOutputTokens(12) = %d, want 3", got)
	}
	// Rounds up: 13 / 4 = 3.25 -> 4.
	if got := heuristicOutputTokens(13); got != 4 {
		t.Errorf("heuristicOutputTokens(13) = %d, want 4", got)
	}
	if got := heuristicOutputTokens(0); got != 0 {
		t.Errorf("heuristicOutputTokens(0) = %d, want 0", got)
	}
}
