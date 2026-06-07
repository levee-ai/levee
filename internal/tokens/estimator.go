// Package tokens provides token estimation for OpenAI and Anthropic models.
package tokens

import (
	"math"
	"strings"

	"github.com/tidwall/gjson"
)

// defaultMaxOutput is the output-token reservation used when a request omits
// max_tokens. OpenAI chat models document no small fixed default (generation
// runs toward the context limit), so this is Levee's pragmatic reservation cap,
// not a provider default. Session 6 reconciliation corrects any over- or
// under-reservation against the provider-reported usage, so this value gates
// estimate accuracy, not budget correctness.
const defaultMaxOutput int64 = 4096

// anthropicCharsPerToken is the character heuristic for Anthropic models
// (the settled 4-characters-per-token decision).
const anthropicCharsPerToken = 4

// Estimator produces a conservative pre-call token reservation for a request.
type Estimator struct {
	fallbackEncoding string
}

// NewEstimator builds an Estimator. fallbackEncoding is the tiktoken encoding
// name used when a model is not recognized (from defaults.unknown_model_tokenizer).
func NewEstimator(fallbackEncoding string) *Estimator {
	return &Estimator{fallbackEncoding: fallbackEncoding}
}

// Estimate returns inputEstimate + outputReserve for the given model and body.
func (estimator *Estimator) Estimate(model string, body []byte) int64 {
	input := estimator.estimateInput(model, body)
	return input + outputReserve(body)
}

// estimateInput counts input tokens. Anthropic models use the character
// heuristic. Other models use tiktoken (added in the next task).
func (estimator *Estimator) estimateInput(model string, body []byte) int64 {
	text := extractInputText(body)
	if isAnthropic(model) {
		return int64(math.Ceil(float64(len(text)) / anthropicCharsPerToken))
	}
	return estimator.estimateWithTiktoken(model, text)
}

// estimateWithTiktoken is filled in the next task. For now it falls back to the
// character heuristic so this task builds and tests green.
func (estimator *Estimator) estimateWithTiktoken(model, text string) int64 {
	return int64(math.Ceil(float64(len(text)) / anthropicCharsPerToken))
}

func isAnthropic(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

// outputReserve returns max_tokens from the request when present, otherwise
// defaultMaxOutput.
func outputReserve(body []byte) int64 {
	result := gjson.GetBytes(body, "max_tokens")
	if result.Exists() && result.Type == gjson.Number {
		// A max_tokens that overflows int64 wraps to a negative value. Treat any
		// negative result as absent and fall back to the default so the reserve
		// can never shrink the estimate below the input alone and under-count.
		if value := result.Int(); value >= 0 {
			return value
		}
	}
	return defaultMaxOutput
}

// extractInputText concatenates the prompt text Levee can see: the top-level
// system field plus each messages[].content string. If the body has no
// recognizable message array, the whole body is counted, which over-estimates
// in the safe never-under-count direction.
func extractInputText(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return string(body)
	}
	var builder strings.Builder
	if system := gjson.GetBytes(body, "system"); system.Type == gjson.String {
		builder.WriteString(system.Str)
	}
	messages.ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if content.Type == gjson.String {
			builder.WriteString(content.Str)
			return true
		}
		// Anthropic content can be an array of blocks with text fields.
		content.ForEach(func(_, block gjson.Result) bool {
			if text := block.Get("text"); text.Type == gjson.String {
				builder.WriteString(text.Str)
			}
			return true
		})
		return true
	})
	return builder.String()
}
