package budget

import (
	"encoding/json"
	"math"
	"testing"
)

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		unit   string
		amount int64
		want   string
	}{
		{"tokens", 100000, "100000"},
		{"tokens", 0, "0"},
		{"tokens", -5, "-5"},
		{"dollars", 50_000_000, "50.00"},
		{"dollars", 49_999_550, "49.99955"},
		{"dollars", 1_000_000, "1.00"},
		{"dollars", 10_000, "0.01"},
		{"dollars", 0, "0.00"},
		{"dollars", 1, "0.000001"},
		{"dollars", 999_999, "0.999999"},
		{"dollars", 100, "0.0001"},
		{"dollars", -1_500_000, "-1.50"},
		{"dollars", math.MinInt64, "-9223372036854.775808"},
	}
	for _, testCase := range cases {
		if got := FormatAmount(testCase.unit, testCase.amount); got != testCase.want {
			t.Errorf("FormatAmount(%q, %d) = %q, want %q", testCase.unit, testCase.amount, got, testCase.want)
		}
	}
}

func TestFormatAmount_MinInt64MarshalsValidJSON(t *testing.T) {
	rendered := FormatAmount("dollars", math.MinInt64)
	if _, err := json.Marshal(json.Number(rendered)); err != nil {
		t.Errorf("MinInt64 render %q is not valid JSON: %v", rendered, err)
	}
}
