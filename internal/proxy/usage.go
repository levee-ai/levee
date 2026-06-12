package proxy

import (
	"bytes"
	"math"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Provider identifiers, matching the path prefix from splitProviderPath.
const (
	providerOpenAI    = "openai"
	providerAnthropic = "anthropic"
)

// charsPerTokenHeuristic is the fallback output estimate ratio (the settled
// 4-characters-per-token decision). Used only when a stream delivers content
// but no authoritative usage. Over-counts in the safe direction.
const charsPerTokenHeuristic = 4

var (
	usageMarker     = []byte(`"usage"`)
	usageNullMarker = []byte(`"usage":null`)
)

// shouldParseUsage is the hot-path byte check from 002. It returns true only
// when a data payload contains a usage field that is not explicitly null, so
// intermediate chunks (usage:null) and content chunks (no usage) are skipped
// without a JSON parse. Cost is a couple of byte scans (about 5 to 15ns).
func shouldParseUsage(data []byte) bool {
	return bytes.Contains(data, usageMarker) && !bytes.Contains(data, usageNullMarker)
}

// inspectUsage extracts provider usage from one SSE data payload into state.
// Caller has already confirmed shouldParseUsage(data) is true for the OpenAI
// path. For Anthropic, the event type in state.lastEventType selects the field.
func inspectUsage(data []byte, state *streamState) {
	switch state.provider {
	case providerAnthropic:
		inspectAnthropicUsage(data, state)
	default:
		inspectOpenAIUsage(data, state)
	}
}

// inspectOpenAIUsage extracts prompt_tokens and completion_tokens from an
// OpenAI usage chunk. The last non-null usage seen wins (002: most recent is
// authoritative). This handles the Chat Completions shape only. The Responses
// API (/v1/responses) nests usage as response.usage.input_tokens/output_tokens,
// which is not read here, so a /v1/responses stream falls through to the
// content-byte fallback (a safe over-count). Full Responses API extraction is
// deferred, tracked as a follow-up.
func inspectOpenAIUsage(data []byte, state *streamState) {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() || usage.Type == gjson.Null {
		return
	}
	prompt := usage.Get("prompt_tokens")
	completion := usage.Get("completion_tokens")
	if !prompt.Exists() && !completion.Exists() {
		return
	}
	state.inputTokens = prompt.Int()
	state.outputTokens = completion.Int()
	state.sawAuthoritativeUsage = true
}

// inspectAnthropicUsage extracts input_tokens from message_start and
// output_tokens from message_delta. message_delta output_tokens is cumulative
// and final per 002. sawAuthoritativeUsage is set once output_tokens arrives,
// which is the component that completes the count.
func inspectAnthropicUsage(data []byte, state *streamState) {
	switch state.lastEventType {
	case "message_start":
		if input := gjson.GetBytes(data, "message.usage.input_tokens"); input.Exists() {
			state.inputTokens = input.Int()
		}
	case "message_delta":
		if output := gjson.GetBytes(data, "usage.output_tokens"); output.Exists() {
			state.outputTokens = output.Int()
			state.sawAuthoritativeUsage = true
		}
	}
}

// extractNonStreamingUsage pulls input and output token usage from a complete
// JSON response body. Returns (input, output, true) when usage is present,
// (0, 0, false) when missing. Anthropic reports input_tokens/output_tokens
// directly. OpenAI reports prompt_tokens/completion_tokens; when only
// total_tokens is present, the whole total is attributed to output (the more
// expensive half) so a derived dollar cost never under-counts.
func extractNonStreamingUsage(provider string, body []byte) (input, output int64, ok bool) {
	if provider == providerAnthropic {
		inputResult := gjson.GetBytes(body, "usage.input_tokens")
		outputResult := gjson.GetBytes(body, "usage.output_tokens")
		if !inputResult.Exists() && !outputResult.Exists() {
			return 0, 0, false
		}
		return inputResult.Int(), outputResult.Int(), true
	}
	prompt := gjson.GetBytes(body, "usage.prompt_tokens")
	completion := gjson.GetBytes(body, "usage.completion_tokens")
	// Prefer the split. OpenAI sends prompt_tokens, completion_tokens, and
	// total_tokens together on a 200, so a mixed-presence shape (one half present,
	// the other absent) is not expected. If it ever occurs the absent half is 0
	// and total_tokens is intentionally not consulted here. The streaming path
	// backfills an absent half in composeStreamTokens, the non-streaming path
	// accepts it because the shape does not occur in practice.
	if prompt.Exists() || completion.Exists() {
		return prompt.Int(), completion.Int(), true
	}
	// No split present. Fall back to total_tokens, attributed entirely to output.
	total := gjson.GetBytes(body, "usage.total_tokens")
	if total.Exists() {
		return 0, total.Int(), true
	}
	return 0, 0, false
}

// heuristicOutputTokens estimates output tokens from forwarded content bytes
// when no authoritative usage arrived. ceil(bytes / charsPerToken), an
// over-count in the safe direction.
func heuristicOutputTokens(contentBytes int64) int64 {
	if contentBytes <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(contentBytes) / charsPerTokenHeuristic))
}

// streamOptionsValue is the value Levee forces into OpenAI streaming requests
// so the provider emits a final usage chunk.
var streamOptionsValue = []byte(`{"include_usage":true}`)

// injectStreamOptions sets stream_options.include_usage=true on an OpenAI
// streaming request body, overwriting any existing value (002: honoring
// include_usage=false would be a budget bypass). Returns the new body and true
// on success. On an sjson error (should not happen for the valid JSON that
// readRequestBody already accepted) it returns the original body and false so
// the caller forwards unmodified and falls back to the content heuristic.
func injectStreamOptions(body []byte) ([]byte, bool) {
	injected, err := sjson.SetRawBytes(body, "stream_options", streamOptionsValue)
	if err != nil {
		return body, false
	}
	return injected, true
}

// streamOptionsIncludeUsage is the verify-readback from 002: confirms the
// injected body reports include_usage=true.
func streamOptionsIncludeUsage(body []byte) bool {
	return gjson.GetBytes(body, "stream_options.include_usage").Bool()
}
