package spending

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
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
// Each pair says which PHASE asserted it, because the check a reader
// performs on two legs depends on the answer. On a pair the amounts
// produced, two legs that disagree in size or currency are the defect
// this listing exists to catch; on a pair something ASSERTED — the
// holder's own ledger, one reference the source stamped on both legs,
// one leg's narrative stating the other's currency and figure — they
// are the expected shape, those roads existing precisely to reach
// movements no amount test can.
//
// Each leg also says whether it was in the spending population. The
// pool is wider than the population on purpose — every account, the
// income-side kinds — so most of what the matcher pairs was never a
// spending candidate, and a pair formed entirely outside the
// population removed nothing from spending. The audit surface reports
// every pair regardless; it is the caller's listing that decides what
// that distinction is worth showing.
//
// The pairs are the ones the last pass stored in transfer_links, so they
// are exactly the pairs that pass acted on, and listing them runs no
// matcher.

// querier is the read subset shared by *sql.DB and *sql.Tx, so the
// readers serve both the pass (inside its transaction) and the audit
// surface (a plain read-only handle).
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

// Pair is one matched own-account move, and the phase that asserted it.
//
// By is what makes the listing readable. A reader checking a pair has always
// asked whether its two legs agree in amount and currency; an ASSERTED pair is
// supposed to disagree, an assertion being what reaches a currency conversion
// the amount pass partitions away. Unlabelled, the correct answer and the
// over-eager one look the same on the page.
type Pair struct {
	Debit  Leg
	Credit Leg
	By     gold.TransferMatchPhase
}

// RemovedFromSpending reports whether the pair took anything out of
// spending: at least one leg was in the enrichment population. A pair
// with neither — wallet to wallet, brokerage to brokerage — is a
// correct verdict about a move no report ever charted, and nothing for
// an audit of what LEFT spending to look at.
func (p Pair) RemovedFromSpending() bool {
	return p.Debit.InPopulation || p.Credit.InPopulation
}

// MatchedPairs reads the pairs the last enrichment pass stored in
// transfer_links, and every leg of the matcher pool it left unpaired.
// Both results are sorted for stable output. A gold without the table
// (TransferLinksStored) holds no stored pairs, so it lists none, and
// every leg of the pool as unpaired.
func MatchedPairs(ctx context.Context, db querier) ([]Pair, []Leg, error) {
	population, err := loadPopulation(ctx, db, spendingFamily)
	if err != nil {
		return nil, nil, err
	}
	inPopulation := make(map[txKey]bool, len(population))
	for _, c := range population {
		inPopulation[c.key] = true
	}
	stored, err := TransferLinksStored(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	var pairs []Pair
	paired := ""
	if stored {
		if pairs, err = readLinkedPairs(ctx, db, inPopulation); err != nil {
			return nil, nil, err
		}
		paired = `
           AND NOT EXISTS (
               SELECT 1 FROM transfer_links l
                WHERE l.silver_source_id        = p.silver_source_id
                  AND l.transaction_external_id = p.transaction_external_id
                  AND l.far_transaction_external_id IS NOT NULL)`
	}

	// The pool's legs with no partner. A leg with no amount was never
	// offered to the matcher (loadMatcherPool).
	rows, err := db.QueryContext(ctx, `
        SELECT `+legColumns("p")+`
          FROM spend_matcher_pool(?, ?) p
         WHERE p.net_amount IS NOT NULL`+paired, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, nil, fmt.Errorf("spending: read unpaired legs: %w", err)
	}
	defer rows.Close()
	var unmatched []Leg
	for rows.Next() {
		var l legScan
		if err := rows.Scan(l.dest()...); err != nil {
			return nil, nil, fmt.Errorf("spending: scan unpaired legs: %w", err)
		}
		unmatched = append(unmatched, l.leg(inPopulation))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("spending: iterate unpaired legs: %w", err)
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

// TransferLinksStored reports whether gold holds transfer_links. A gold
// that no build with migration 0182 has opened read-write lacks it, and
// a read-only open does not migrate.
func TransferLinksStored(ctx context.Context, db querier) (bool, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT 1 FROM duckdb_tables()
         WHERE table_name = 'transfer_links' AND schema_name = 'main'
           AND database_name = current_database()`)
	if err != nil {
		return false, fmt.Errorf("spending: look for transfer links: %w", err)
	}
	defer rows.Close()
	found := rows.Next()
	return found, rows.Err()
}

// readLinkedPairs reads the stored pairs, sorted (sortPairs).
func readLinkedPairs(ctx context.Context, db querier, inPopulation map[txKey]bool) ([]Pair, error) {
	// One row per pair: the leg that sent the money, joined to its
	// partner. A pair is one debit and one credit (gold.TransferLeg.Amt),
	// so its debit names it once.
	rows, err := db.QueryContext(ctx, `
        SELECT l.evidence, `+legColumns("d")+`, `+legColumns("c")+`
          FROM transfer_links l
          JOIN transactions d
            ON d.silver_source_id        = l.silver_source_id
           AND d.transaction_external_id = l.transaction_external_id
          JOIN transactions c
            ON c.silver_source_id        = l.far_silver_source_id
           AND c.transaction_external_id = l.far_transaction_external_id
         WHERE l.far_transaction_external_id IS NOT NULL
           AND CAST(d.net_amount AS DOUBLE) < 0`)
	if err != nil {
		return nil, fmt.Errorf("spending: read transfer links: %w", err)
	}
	defer rows.Close()
	var pairs []Pair
	for rows.Next() {
		var (
			evidence      string
			debit, credit legScan
		)
		if err := rows.Scan(append(append([]any{&evidence}, debit.dest()...), credit.dest()...)...); err != nil {
			return nil, fmt.Errorf("spending: scan transfer links: %w", err)
		}
		pairs = append(pairs, Pair{
			Debit:  debit.leg(inPopulation),
			Credit: credit.leg(inPopulation),
			By:     evidencePhase[evidence],
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("spending: iterate transfer links: %w", err)
	}
	sortPairs(pairs)
	return pairs, nil
}

// legColumns are the columns of the transactions row alias that a Leg
// is read from, in legScan's order.
func legColumns(alias string) string {
	return alias + ".silver_source_id, " + alias + ".transaction_external_id, " +
		alias + ".account_external_id, " + alias + ".occurred_at, " + alias + ".currency, " +
		"CAST(" + alias + ".net_amount AS DOUBLE), " +
		"COALESCE(" + alias + ".counterparty, ''), COALESCE(" + alias + ".description, '')"
}

// legScan is one leg as legColumns reads it.
type legScan struct {
	base                      Leg
	occurredAt                int64
	counterparty, description string
}

func (s *legScan) dest() []any {
	return []any{&s.base.Source, &s.base.TxID, &s.base.Account, &s.occurredAt,
		&s.base.Currency, &s.base.Amount, &s.counterparty, &s.description}
}

// leg is the scanned leg with its day, its signature and its population
// flag.
func (s *legScan) leg(inPopulation map[txKey]bool) Leg {
	l := s.base
	l.Day = gold.EpochDay(s.occurredAt)
	l.Signature = Normalize(s.counterparty, s.description)
	l.InPopulation = inPopulation[txKey{l.Source, l.TxID}]
	return l
}

// sortPairs orders the pairs so that the ones worth a human's attention
// survive the listing's cap.
//
// ASSERTED pairs (override, reference, stated-counter) come FIRST, ahead of the
// chronological order the named and amount pairs keep, and in MatchTransferLegs'
// phase order among themselves (assertedRank). The cap exists so the listing
// stays a signal rather than a dump, but a day-ordered listing of a long
// history is the OLDEST pairs, and a road that only started asserting pairs
// on the last reload would never appear in one — which is the newest road
// every time, and so the one most in need of a reader. Asserted pairs are
// also the ones whose legs a reader is told to expect to disagree, so they
// are precisely the ones the cut must not swallow.
func sortPairs(pairs []Pair) {
	sort.Slice(pairs, func(i, j int) bool {
		a, b := pairs[i], pairs[j]
		if ar, br := phaseRank(a.By), phaseRank(b.By); ar != br {
			return ar < br
		}
		if a.Debit.Day != b.Debit.Day {
			return a.Debit.Day < b.Debit.Day
		}
		if a.Debit.Source != b.Debit.Source {
			return a.Debit.Source < b.Debit.Source
		}
		return a.Debit.TxID < b.Debit.TxID
	})
}

// assertedRank is the listing order of the asserted phases: MatchTransferLegs'
// own phase order. A phase absent from it asserted nothing.
var assertedRank = map[gold.TransferMatchPhase]int{
	gold.MatchedByOverride:      0,
	gold.MatchedByReference:     1,
	gold.MatchedByStatedCounter: 2,
}

// phaseRank places the asserted phases in assertedRank order and every other
// phase after them, all at one rank.
func phaseRank(by gold.TransferMatchPhase) int {
	if r, ok := assertedRank[by]; ok {
		return r
	}
	return len(assertedRank)
}
