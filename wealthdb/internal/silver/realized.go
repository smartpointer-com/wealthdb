package silver

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// RealizedLotReader is the optional Connection extension for a source
// whose silver states realized lots: 1099-B lots, a year-end summary's
// realized sections, a statement's sales. The loader replaces the
// source's `realized_lots` rows with what it returns on every load that
// has changes (docs/DESIGN.md §8.1).
//
// It is not windowed. A sale's tax document arrives months after the
// sale and restates it, so no change window derived from dumps or
// transaction dates is guaranteed to cover it; the table is small, and
// re-reading it whole is what keeps it right.
type RealizedLotReader interface {
	RealizedLots(ctx context.Context) ([]canonical.RealizedLotChange, error)
}

// MarkPrimary sets IsPrimary on the rows that count each sale once:
// per (account, tax year), every eligible row of the best-ranked
// document kind present. rank orders the kinds, lower first; a negative
// rank is never primary. eligible, when non-nil, excludes the rows a
// source knows to be a second copy within one kind (a superseded
// correction, a sale restated by a later statement); excluded rows
// neither become primary nor make their kind present.
func MarkPrimary(lots []canonical.RealizedLotChange,
	rank func(canonical.RealizedDocKind) int,
	eligible func(*canonical.RealizedLotChange) bool) {
	type key struct {
		account string
		year    int
	}
	ok := func(r *canonical.RealizedLotChange) bool {
		return rank(r.DocumentKind) >= 0 && (eligible == nil || eligible(r))
	}
	best := map[key]int{}
	for i := range lots {
		r := &lots[i]
		if !ok(r) {
			continue
		}
		k := key{r.AccountExternalID, r.TaxYear}
		if b, seen := best[k]; !seen || rank(r.DocumentKind) < b {
			best[k] = rank(r.DocumentKind)
		}
	}
	for i := range lots {
		r := &lots[i]
		b, seen := best[key{r.AccountExternalID, r.TaxYear}]
		r.IsPrimary = seen && ok(r) && rank(r.DocumentKind) == b
	}
}
