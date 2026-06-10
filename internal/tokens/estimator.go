// Package tokens provides token estimation for OpenAI and Anthropic models.
package tokens

import (
	"math"
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tokenloader "github.com/pkoukk/tiktoken-go-loader"
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

// maxOutputReserve is the upper bound on an honored max_tokens reservation.
// max_tokens is otherwise trusted as given, and an oversized but plausible value
// is left to surface as an upstream 4xx. This ceiling exists only to neutralize
// values that would otherwise overflow the int64 estimate and under-count: an
// out-of-range max_tokens (a literal beyond the int64 range, which gjson may wrap
// to a negative or to unrelated bits, or a scientific-notation number that gjson
// saturates to MaxInt64) falls back to the default so the estimate can never
// overflow int64 and under-count. Clamping against the ceiling handles all of
// these uniformly because it bounds the value rather than relying on any single
// overflow shape. The largest real-world model context is on the order of
// millions of tokens, so 100 million is far above any legitimate max_tokens
// (those pass through unchanged) while leaving roughly 9.2e18 of headroom so
// input + reserve can never wrap negative. The only values whose behavior changes
// are ones that would otherwise overflow and bypass the budget check.
const maxOutputReserve int64 = 100_000_000

// Estimator produces a conservative pre-call token reservation for a request.
type Estimator struct {
	fallbackEncoding string

	// encoderCache holds one constructed tiktoken encoder per encoding name.
	// tiktoken.EncodingForModel and GetEncoding rebuild the entire BPE merge
	// table and recompile the split regex on every call (tens of milliseconds
	// and megabytes of allocation each), so calling them per request would blow
	// the latency budget. The constructed encoder is read-only during Encode and
	// the underlying regexp2 is documented safe for concurrent use, so a cached
	// encoder is reused across goroutines. The cache is bounded by the number of
	// distinct tiktoken encodings, not by the number of models. Construction runs
	// under the write lock so concurrent first-time callers single-flight rather
	// than each rebuilding the table.
	encoderMutex sync.RWMutex
	encoderCache map[string]*tiktoken.Tiktoken
}

// offlineLoaderOnce installs the embedded BPE ranks exactly once. Without it
// tiktoken-go downloads vocabulary files over the network on first use, which
// violates the zero-network single-binary tenet. Verified offline with the
// embedded loader.
var offlineLoaderOnce sync.Once

func ensureOfflineLoader() {
	offlineLoaderOnce.Do(func() {
		tiktoken.SetBpeLoader(tokenloader.NewOfflineLoader())
	})
}

// NewEstimator builds an Estimator. fallbackEncoding is the tiktoken encoding
// name used when a model is not recognized (from defaults.unknown_model_tokenizer).
func NewEstimator(fallbackEncoding string) *Estimator {
	ensureOfflineLoader()
	return &Estimator{
		fallbackEncoding: fallbackEncoding,
		encoderCache:     make(map[string]*tiktoken.Tiktoken),
	}
}

// Estimate returns inputEstimate + outputReserve for the given model and body.
// Estimate's contract is a non-negative int64. Saturate rather than wrap so that
// contract holds for any input and reserve, including a future change to the
// reserve ceiling: if input and reserve together would exceed the int64 ceiling,
// it returns MaxInt64 rather than wrapping to a negative estimate that would
// under-count.
func (estimator *Estimator) Estimate(model string, body []byte) int64 {
	input, output := estimator.EstimateSplit(model, body)
	if input > math.MaxInt64-output {
		return math.MaxInt64
	}
	return input + output
}

// EstimateSplit returns the input token estimate and the output reserve
// separately. Dollar pricing needs the two halves at their separate per-1K rates.
// The output reserve is the honored max_tokens (or the default when absent).
func (estimator *Estimator) EstimateSplit(model string, body []byte) (input, output int64) {
	return estimator.EstimateInput(model, body), outputReserve(body)
}

// EstimateInput counts input tokens for the model and body, WITHOUT the output
// reserve. Anthropic models use the character heuristic. Other models use
// tiktoken. The streaming reconciliation fallback uses this to estimate input
// tokens when the provider did not report authoritative usage.
func (estimator *Estimator) EstimateInput(model string, body []byte) int64 {
	text := extractInputText(body)
	if isAnthropic(model) {
		return int64(math.Ceil(float64(len(text)) / anthropicCharsPerToken))
	}
	return estimator.estimateWithTiktoken(model, text)
}

// estimateWithTiktoken counts tokens with the model's tiktoken encoding,
// falling back to the configured encoding for unrecognized models.
func (estimator *Estimator) estimateWithTiktoken(model, text string) int64 {
	encoder := estimator.encoderForModel(model)
	if encoder == nil {
		// The fallback encoding is config-validated, so a nil encoder is
		// unreachable in practice. Degrade to the character heuristic rather
		// than panic.
		return int64(math.Ceil(float64(len(text)) / anthropicCharsPerToken))
	}
	return int64(len(encoder.Encode(text, nil, nil)))
}

// encoderForModel returns a cached tiktoken encoder for the model's encoding,
// falling back to the configured encoding for unrecognized models. It returns
// nil only when neither the model nor the fallback resolves to a known encoding.
func (estimator *Estimator) encoderForModel(model string) *tiktoken.Tiktoken {
	encodingName := encodingNameForModel(model)
	if encodingName == "" {
		encodingName = estimator.fallbackEncoding
	}
	return estimator.cachedEncoder(encodingName)
}

// cachedEncoder returns the constructed encoder for an encoding name, building
// and caching it on first use. It returns nil if the encoding name is unknown.
func (estimator *Estimator) cachedEncoder(encodingName string) *tiktoken.Tiktoken {
	estimator.encoderMutex.RLock()
	encoder, ok := estimator.encoderCache[encodingName]
	estimator.encoderMutex.RUnlock()
	if ok {
		return encoder
	}

	estimator.encoderMutex.Lock()
	defer estimator.encoderMutex.Unlock()
	// Another goroutine may have built it between the read unlock and the write
	// lock, so check again before constructing.
	if encoder, ok := estimator.encoderCache[encodingName]; ok {
		return encoder
	}
	encoder, err := tiktoken.GetEncoding(encodingName)
	if err != nil {
		return nil
	}
	estimator.encoderCache[encodingName] = encoder
	return encoder
}

// encodingNameForModel resolves a model name to its tiktoken encoding name
// using tiktoken-go's own model tables. An exact model match wins first. For
// prefix matches it diverges from tiktoken.EncodingForModel, which returns the
// first prefix match in Go's random map order: this picks the longest matching
// prefix so resolution stays deterministic and stable if overlapping prefixes
// are ever added. It returns an empty string when the model is unrecognized.
// Resolving the name here (rather than calling tiktoken.EncodingForModel) lets
// the constructed encoder be cached by encoding name and reused across requests.
func encodingNameForModel(model string) string {
	if encodingName, ok := tiktoken.MODEL_TO_ENCODING[model]; ok {
		return encodingName
	}
	longestPrefix := ""
	resolved := ""
	for prefix, encodingName := range tiktoken.MODEL_PREFIX_TO_ENCODING {
		if strings.HasPrefix(model, prefix) && len(prefix) > len(longestPrefix) {
			longestPrefix = prefix
			resolved = encodingName
		}
	}
	return resolved
}

func isAnthropic(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

// outputReserve returns max_tokens from the request when present, otherwise
// defaultMaxOutput.
func outputReserve(body []byte) int64 {
	result := gjson.GetBytes(body, "max_tokens")
	if result.Exists() && result.Type == gjson.Number {
		// Honor max_tokens only when it is in range. A value below zero or at or
		// beyond maxOutputReserve (including an int64-overflowing literal or a
		// scientific-notation number that gjson saturates to MaxInt64) falls back
		// to the default so the reserve can never shrink the estimate below the
		// input alone and under-count.
		if value := result.Int(); value >= 0 && value <= maxOutputReserve {
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
