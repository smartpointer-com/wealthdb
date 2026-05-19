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

// NewDecimalFromFloat builds a Decimal from a float64. Used by
// silver readers whose source format stores numerics as REAL
// (SQLite). The conversion uses shopspring/decimal's float
// helper which does best-effort base-10 reconstruction; for
// values that originated as text in silver, prefer
// NewDecimalFromString to avoid float-precision artefacts.
func NewDecimalFromFloat(f float64) Decimal {
	return decimal.NewFromFloat(f)
}
