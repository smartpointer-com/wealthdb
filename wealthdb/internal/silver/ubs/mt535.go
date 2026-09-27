package ubs

import (
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// MT535 subfield parsing. Each tag in holdings.payload.fields
// (19A monetary amounts, 93B quantities) is an array of raw
// SWIFT subfield strings with format
//
//   :<qualifier>//[<format>/]<value>
//
// SWIFT amount-value convention: comma is the decimal separator.
// A trailing comma means "no fractional digits" — the comma is
// the end-of-amount terminator. Examples:
//
//   :HOLD//USD1500000,        → market value USD 1,500,000.00
//   :HOLD//CHF1200000,        → same position's value in reporting CHF
//   :BOOK//USD600000,         → book/cost basis
//   :AGGR//UNIT/10000,        → aggregate quantity, 10000 units
//   :AGGR//FAMT/100000,       → aggregate FACE amount, for bonds
//   :INDC//ACTU/USD150,123456 → indicative actual price USD 150.123456
//
// We only need market_value and quantity for gold's positions
// table; everything else stays in the raw payload for forensics.

var (
	re19A = regexp.MustCompile(`^:([A-Z]{3,4})//([A-Z]{3})([0-9,]+?),?$`)
	re93B = regexp.MustCompile(`^:([A-Z]{3,4})//([A-Z]+)/([0-9,]+?),?$`)
)

// mt535Money is one parsed 19A entry.
type mt535Money struct {
	Qualifier string            // HOLD, BOOK, ACRU, ...
	Currency  string            // ISO 4217
	Amount    canonical.Decimal // parsed; comma → dot, trailing terminator dropped
}

// mt535Qty is one parsed 93B entry.
type mt535Qty struct {
	Qualifier string // AGGR, AVAI, ...
	Format    string // UNIT, FAMT, AMOR
	Amount    canonical.Decimal
}

// parse19A maps the raw array of strings into typed mt535Money
// entries. Subfields that don't match the expected shape are
// skipped silently — silver guarantees the bytes are present
// but doesn't guarantee every variant is currently expected.
func parse19A(raw []string) []mt535Money {
	out := make([]mt535Money, 0, len(raw))
	for _, s := range raw {
		m := re19A.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		amt, err := parseSwiftDecimal(m[3])
		if err != nil {
			continue
		}
		out = append(out, mt535Money{
			Qualifier: m[1],
			Currency:  m[2],
			Amount:    amt,
		})
	}
	return out
}

// parse93B maps the raw array of strings into typed mt535Qty
// entries.
func parse93B(raw []string) []mt535Qty {
	out := make([]mt535Qty, 0, len(raw))
	for _, s := range raw {
		m := re93B.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		amt, err := parseSwiftDecimal(m[3])
		if err != nil {
			continue
		}
		out = append(out, mt535Qty{
			Qualifier: m[1],
			Format:    m[2],
			Amount:    amt,
		})
	}
	return out
}

// findHoldEntry picks one 19A:HOLD entry and returns its currency
// AND amount, in that order. Preference: an entry whose currency
// matches `preferredCurrency` (the instrument's natural currency,
// when known); else the first HOLD entry of any currency. Returns
// ok=false when no HOLD entry exists at all.
//
// Exposing the currency lets the UBS adapter sync the position's
// currency to whatever the chosen HOLD entry reports — important
// for positions whose instrument metadata lacks a currency
// (otherwise routed to the "XXX" sentinel).
func findHoldEntry(amounts []mt535Money, preferredCurrency string) (canonical.Decimal, string, bool) {
	var (
		fallbackAmt canonical.Decimal
		fallbackCcy string
		hasFallback bool
	)
	for _, a := range amounts {
		if a.Qualifier != "HOLD" {
			continue
		}
		if preferredCurrency != "" && a.Currency == preferredCurrency {
			return a.Amount, a.Currency, true
		}
		if !hasFallback {
			fallbackAmt = a.Amount
			fallbackCcy = a.Currency
			hasFallback = true
		}
	}
	return fallbackAmt, fallbackCcy, hasFallback
}

// findQuantity returns the aggregate quantity (AGGR qualifier)
// regardless of format (UNIT for shares / contracts, FAMT for
// bond face amounts). Both surface in gold as a Decimal; the
// distinction is recorded in the position's raw payload.
func findQuantity(qtys []mt535Qty) (canonical.Decimal, bool) {
	for _, q := range qtys {
		if q.Qualifier == "AGGR" {
			return q.Amount, true
		}
	}
	// No AGGR — try AVAI (available) as fallback. Rare in
	// practice but documented in MT535.
	for _, q := range qtys {
		if q.Qualifier == "AVAI" {
			return q.Amount, true
		}
	}
	return canonical.Decimal{}, false
}

// parseSwiftDecimal converts a SWIFT amount string (comma as
// decimal separator, trailing-comma terminator) into a Decimal.
// Inputs that come through the regex have already been stripped
// of the trailing terminator comma; here we only need to swap
// the embedded comma for a dot.
//
//	"1500000"     → 1500000
//	"150,123456"  → 150.123456
//	"60,839"      → 60.839
func parseSwiftDecimal(s string) (canonical.Decimal, error) {
	s = strings.ReplaceAll(s, ",", ".")
	return canonical.NewDecimalFromString(s)
}
