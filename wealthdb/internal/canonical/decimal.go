package canonical

import "github.com/shopspring/decimal"

// Decimal is the exact-arithmetic numeric type used throughout
// canonical records. Aliased to shopspring/decimal so the rest of
// the codebase imports only `canonical.Decimal` and the upstream
// library lives behind one package boundary.
type Decimal = decimal.Decimal

// NewDecimalFromString parses a Decimal from its canonical string
// form. Returns an error on garbage; never panics.
func NewDecimalFromString(s string) (Decimal, error) {
	return decimal.NewFromString(s)
}

// NewDecimalFromInt builds a Decimal from an int64. Convenience
// for tests and constants.
func NewDecimalFromInt(n int64) Decimal {
	return decimal.NewFromInt(n)
}
