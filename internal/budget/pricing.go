package budget

import (
	"math"
	"strings"
)

// modelPrice holds per-token rates in microdollars per 1,000,000 tokens, so a
// $2.50 / 1M-token input rate is 2_500_000. Integer so all cost math stays
// integer (no float money). Source: provider public pricing pages, verified
// 2026-06-09. Prices drift, so this table is the documented stale-risk surface
// and a phase-2 config-override candidate. Re-verify when next touched.
type modelPrice struct {
	inputPerMillion  int64
	outputPerMillion int64
}

// modelPrices maps an exact model id to its rates. Lookup falls back to the
// longest matching prefix (mirrors the tokens estimator), so versioned ids like
// "claude-3-opus-20240229" resolve via the "claude-3-opus" prefix.
var modelPrices = map[string]modelPrice{
	"gpt-4o":            {2_500_000, 10_000_000},
	"gpt-4o-mini":       {150_000, 600_000},
	"gpt-4-turbo":       {10_000_000, 30_000_000},
	"gpt-4":             {30_000_000, 60_000_000},
	"gpt-3.5-turbo":     {500_000, 1_500_000},
	"claude-3-opus":     {15_000_000, 75_000_000},
	"claude-3-5-sonnet": {3_000_000, 15_000_000},
	"claude-3-sonnet":   {3_000_000, 15_000_000},
	"claude-3-5-haiku":  {800_000, 4_000_000},
	"claude-3-haiku":    {250_000, 1_250_000},
}

// maxKnownPrice is the most expensive known rate per half, used to price an
// unrecognized model so an unknown model never under-counts (Tenet 3). Computed
// once at package init from the table so adding a pricier model keeps it correct.
var maxKnownPrice = computeMaxKnownPrice()

func computeMaxKnownPrice() modelPrice {
	var maximum modelPrice
	for _, price := range modelPrices {
		if price.inputPerMillion > maximum.inputPerMillion {
			maximum.inputPerMillion = price.inputPerMillion
		}
		if price.outputPerMillion > maximum.outputPerMillion {
			maximum.outputPerMillion = price.outputPerMillion
		}
	}
	return maximum
}

// CostMicrodollars returns the cost in microdollars for a model and token split,
// and whether the model was found in the table. An unknown model is priced at the
// maximum known rate (never under-counts) and returns known=false so the caller
// can warn. Each half is ceil(tokens * ratePerMillion / 1e6). The multiply
// saturates to MaxInt64 on overflow rather than wrapping negative. Negative token
// counts contribute zero.
func CostMicrodollars(model string, inputTokens, outputTokens int64) (cost int64, known bool) {
	price, known := priceForModel(model)
	inputCost := halfCost(inputTokens, price.inputPerMillion)
	outputCost := halfCost(outputTokens, price.outputPerMillion)
	return saturatingAdd(inputCost, outputCost), known
}

// priceForModel resolves a model to its rates: exact match first, then the
// longest matching prefix. An unrecognized model returns the max known price and
// known=false.
func priceForModel(model string) (modelPrice, bool) {
	if price, ok := modelPrices[model]; ok {
		return price, true
	}
	longestPrefix := ""
	var resolved modelPrice
	for prefix, price := range modelPrices {
		if strings.HasPrefix(model, prefix) && len(prefix) > len(longestPrefix) {
			longestPrefix = prefix
			resolved = price
		}
	}
	if longestPrefix != "" {
		return resolved, true
	}
	return maxKnownPrice, false
}

// halfCost returns ceil(tokens * ratePerMillion / 1_000_000), saturating to
// MaxInt64 if the multiply would overflow. Non-positive tokens yield zero.
func halfCost(tokens, ratePerMillion int64) int64 {
	if tokens <= 0 || ratePerMillion <= 0 {
		return 0
	}
	if tokens > math.MaxInt64/ratePerMillion {
		return math.MaxInt64
	}
	const million = int64(1_000_000)
	product := tokens * ratePerMillion
	quotient := product / million
	if product%million != 0 {
		quotient++
	}
	return quotient
}

// saturatingAdd returns a+b, clamped to MaxInt64 instead of wrapping negative.
func saturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
