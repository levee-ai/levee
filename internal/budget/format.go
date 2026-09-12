package budget

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatAmount formats a budget amount for the wire. Token budgets render as a
// base-10 integer. Dollar budgets are stored in microdollars (1e-6 USD) and
// render as a dollars decimal, trimmed of trailing zeros but keeping at least
// two decimal places. Shared by the proxy 429 body and the admin API so the
// two surfaces can never disagree on money formatting.
func FormatAmount(unit string, amount int64) string {
	if unit != "dollars" {
		return strconv.FormatInt(amount, 10)
	}
	return microdollarsToDecimal(amount)
}

// microdollarsToDecimal converts an integer microdollar amount to a dollars
// decimal string (e.g. 49_999_550 -> "49.99955", 50_000_000 -> "50.00"). The
// function is total: it handles math.MinInt64 correctly by negating in uint64
// space, where the positive of MinInt64 is representable (plain int64 negation
// overflows for that one value, producing a malformed string like
// "--9223372036854.-775808" that breaks json.Marshal).
func microdollarsToDecimal(microdollars int64) string {
	sign := ""
	magnitude := uint64(microdollars)
	if microdollars < 0 {
		sign = "-"
		// Negate in uint64 space so math.MinInt64 (whose positive has no int64
		// representation) does not overflow. uint64(-(n+1)) + 1 == abs(n).
		magnitude = uint64(-(microdollars + 1)) + 1
	}
	whole := magnitude / 1_000_000
	fraction := magnitude % 1_000_000
	// Six-digit zero-padded fractional part, then trim trailing zeros to a
	// minimum of two decimal places.
	fractionText := fmt.Sprintf("%06d", fraction)
	fractionText = strings.TrimRight(fractionText, "0")
	for len(fractionText) < 2 {
		fractionText += "0"
	}
	return fmt.Sprintf("%s%d.%s", sign, whole, fractionText)
}
