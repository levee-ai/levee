package budget

import (
	"math"
	"testing"
)

func TestCostMicrodollars_KnownModels(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		input     int64
		output    int64
		wantCost  int64
		wantKnown bool
	}{
		// gpt-4o: $2.50/Mtok in, $10.00/Mtok out. 1000 in + 500 out =
		// 1000*2_500_000/1e6 + 500*10_000_000/1e6 = 2500 + 5000 = 7500 microdollars.
		{"gpt-4o small", "gpt-4o", 1000, 500, 7500, true},
		// gpt-4o-mini: $0.15 in, $0.60 out. 1000 in + 500 out = 150 + 300 = 450.
		{"gpt-4o-mini small", "gpt-4o-mini", 1000, 500, 450, true},
		{"gpt-4o-mini versioned prefix", "gpt-4o-mini-latest", 1000, 500, 450, true},
		// claude prefix resolution: claude-3-opus -> opus $15 in / $75 out.
		{"claude opus prefix", "claude-3-opus-20240229", 1000, 500, 15000 + 37500, true},
		{"zero tokens", "gpt-4o", 0, 0, 0, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cost, known := CostMicrodollars(testCase.model, testCase.input, testCase.output)
			if cost != testCase.wantCost || known != testCase.wantKnown {
				t.Errorf("CostMicrodollars(%q,%d,%d) = (%d,%v), want (%d,%v)",
					testCase.model, testCase.input, testCase.output, cost, known, testCase.wantCost, testCase.wantKnown)
			}
		})
	}
}

func TestCostMicrodollars_UnknownModelPricesAtMax(t *testing.T) {
	// An unknown model is priced at the most expensive known rate and reports
	// known=false. It must cost at least as much as the priciest known model for
	// the same token split (never under-counts).
	cost, known := CostMicrodollars("some-future-model-x", 1000, 500)
	if known {
		t.Fatal("expected known=false for an unrecognized model")
	}
	opusCost, _ := CostMicrodollars("claude-3-opus-20240229", 1000, 500)
	if cost < opusCost {
		t.Errorf("unknown-model cost %d must be >= max known cost %d (never under-count)", cost, opusCost)
	}
}

func TestCostMicrodollars_OverflowSaturates(t *testing.T) {
	// A token count near the int64 ceiling must saturate to MaxInt64, never wrap
	// negative. A negative cost would inflate the budget: the runaway-bill bug.
	const maxInt64 = int64(^uint64(0) >> 1)
	cost, _ := CostMicrodollars("gpt-4o", maxInt64, maxInt64)
	if cost < 0 {
		t.Fatalf("cost wrapped negative: %d", cost)
	}
	if cost != maxInt64 {
		t.Errorf("expected saturation to MaxInt64, got %d", cost)
	}
}

func TestCostMicrodollars_NegativeTokensZero(t *testing.T) {
	if cost, _ := CostMicrodollars("gpt-4o", -5, -5); cost != 0 {
		t.Errorf("negative tokens must yield 0 cost, got %d", cost)
	}
}

func TestHalfCost_CeilingDivisionAtGuardBoundary(t *testing.T) {
	// Rate 150_000 (gpt-4o-mini input). tokens == MaxInt64/rate passes the
	// overflow guard but the old +million-1 ceiling form wrapped negative here.
	// The result must stay positive (a negative cost is the runaway-bill bug).
	rate := int64(150_000)
	tokens := int64(math.MaxInt64) / rate
	got := halfCost(tokens, rate)
	if got < 0 {
		t.Fatalf("halfCost(%d,%d) = %d: negative cost is the runaway-bill bug", tokens, rate, got)
	}
	want := tokens*rate/1_000_000 + 1 // product is not divisible by 1_000_000
	if got != want {
		t.Errorf("halfCost(%d,%d) = %d, want %d", tokens, rate, got, want)
	}
}
