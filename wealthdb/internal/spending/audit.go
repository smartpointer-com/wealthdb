package spending

import (
	"context"
	"database/sql"
	"math"
	"sort"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// The matcher's audit surface.
//
// The matcher is the one tier whose verdict REMOVES rows from
// spending: a matched pair is an own-account move, and both its legs
// disappear from every chart. That makes it the tier whose mistakes
// are hardest to notice — an over-eager pair does not show up as a
// wrong category, it shows up as a month that was quietly cheaper than
// it was. So the pairs are re-derived and listed on demand, both legs
// together, and the legs it could NOT pair are listed beside them: a
// large one-legged movement is either a real outflow or a pair the
// matcher missed, and only a human can say which.
//
// Each leg also says whether it was in the spending population. The
// pool is wider than the population on purpose — every account, the
// income-side kinds — so most of what the matcher pairs was never a
// spending candidate, and a pair formed entirely outside the
// population removed nothing from spending. The audit surface reports
// every pair regardless; it is the caller's listing that decides what
// that distinction is worth showing.
//
// This re-runs the matcher rather than reading the enrichment table
// back, because the table records per-leg verdicts and has no memory of
// which leg was paired with which. Re-deriving is cheap (the pool is
// the same one the pass reads) and deterministic (MatchTransferLegs
// sorts its input), so the pairs reported here are exactly the pairs
// the last pass acted on, as long as gold has not changed underneath.

// querier is the read subset shared by *sql.DB and *sql.Tx, so the
// pool loader serves both the pass (inside its transaction) and the
// audit surface (a plain read-only handle).
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Leg is one side of a movement as the audit surface reports it: the
// transaction's identity, when and how much, and the narrative folded
// down to its signature. The raw narrative is deliberately not carried
// — this listing is printed to a terminal, and a signature says which
// movement a line refers to without reproducing the counterparty's
// details.
type Leg struct {
	Source    string
	TxID      string
	Account   string
	Day       int64 // epoch day
	Currency  string
	Amount    float64 // native, signed
	Signature string
	// InPopulation reports whether the leg is in the enrichment
	// population — a row spending would have counted had the matcher
	// not paired it. A card payment, the deposit side of a funding
	// wire, either end of a move between two brokerage accounts: none
	// of those is, however correctly they pair.
	InPopulation bool
}

// Pair is one matched own-account move.
type Pair struct {
	Debit  Leg
	Credit Leg
}

// RemovedFromSpending reports whether the pair took anything out of
// spending: at least one leg was in the enrichment population. A pair
// with neither — wallet to wallet, brokerage to brokerage — is a
// correct verdict about a move no report ever charted, and nothing for
// an audit of what LEFT spending to look at.
func (p Pair) RemovedFromSpending() bool {
	return p.Debit.InPopulation || p.Credit.InPopulation
}

// MatchedPairs re-runs the internal-transfer matcher over gold's
// matcher pool and returns the pairs it found together with every leg
// it left unpaired. Both results are sorted for stable output.
func MatchedPairs(ctx context.Context, db querier, windowDays int, tolerancePct float64, rules []gold.TransferOverrideRule) ([]Pair, []Leg, error) {
	legs, narratives, err := loadMatcherPool(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	population, err := loadPopulation(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	inPopulation := make(map[txKey]bool, len(population))
	for _, c := range population {
		inPopulation[c.key] = true
	}
	overrides, _, err := gold.ResolveTransferOverrides(rules, legs)
	if err != nil {
		return nil, nil, err
	}
	raw := matchTransferPairs(legs, windowDays, tolerancePct, overrides)
	matched := matchedLegSet(raw)

	byKey := make(map[txKey]Leg, len(legs))
	for _, l := range legs {
		key := txKey{l.Group, l.ID}
		n := narratives[key]
		byKey[key] = Leg{
			Source:       l.Group,
			TxID:         l.ID,
			Account:      l.Owner,
			Day:          l.Day,
			Currency:     l.Ccy,
			Amount:       l.Amt,
			Signature:    Normalize(n.counterparty, n.description),
			InPopulation: inPopulation[key],
		}
	}
	pairs := liftPairs(raw, byKey)

	unmatched := make([]Leg, 0, len(legs)-len(matched))
	for _, l := range legs {
		key := txKey{l.Group, l.ID}
		if matched[key] {
			continue
		}
		unmatched = append(unmatched, byKey[key])
	}
	sort.Slice(unmatched, func(i, j int) bool {
		a, b := unmatched[i], unmatched[j]
		am, bm := math.Abs(a.Amount), math.Abs(b.Amount)
		if am != bm {
			return am > bm // largest first: the ones worth a look
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.TxID < b.TxID
	})
	return pairs, unmatched, nil
}

// liftPairs turns the core's matched pairs into the audit shape.
func liftPairs(raw []gold.TransferMatchPair, byKey map[txKey]Leg) []Pair {
	out := make([]Pair, 0, len(raw))
	for _, p := range raw {
		out = append(out, Pair{
			Debit:  byKey[txKey{p.Debit.Group, p.Debit.ID}],
			Credit: byKey[txKey{p.Credit.Group, p.Credit.ID}],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Debit.Day != b.Debit.Day {
			return a.Debit.Day < b.Debit.Day
		}
		if a.Debit.Source != b.Debit.Source {
			return a.Debit.Source < b.Debit.Source
		}
		return a.Debit.TxID < b.Debit.TxID
	})
	return out
}
