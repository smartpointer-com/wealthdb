package gold

import (
	"context"
	"encoding/json"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// ChangeAccumulator folds a load's dimension-change records
// (portfolios, accounts, instruments) down to one record per entity
// before they reach the upsert SQL. Adapters re-emit dimension rows
// alongside every snapshot they walk — tens of thousands of
// emissions folding onto a few hundred entities — and each
// single-row upsert costs a full DuckDB statement, so upserting per
// emission dominated load time.
//
// Add applies records in arrival order with exactly the §8.4 guard
// semantics the SQL applies: per column, the newest non-nil
// observation wins, an older observation still fills a column no
// newer record has carried, and the seen-at range unions either way.
// Recency arbitrates conflicts; absence never wins — the guard that
// makes attribute survival independent of emission order, so a full
// rebuild folding an old attribute-bearing record after newer
// attribute-less ones (schwab-web's statement-derived tax_wrapper vs
// fresher api rows) keeps the attribute. Because every step is
// symmetric-coalescing, folding first and upserting once is
// equivalent to upserting record-by-record — see
// TestChangeAccumulatorPreexistingRow.
//
// An accumulator is single-use: Add batches, then Flush once.
type ChangeAccumulator struct {
	portfolioIdx  map[string]int
	portfolios    []canonical.PortfolioChange
	accountIdx    map[string]int
	accounts      []canonical.AccountChange
	instrumentIdx map[string]int
	instruments   []canonical.InstrumentChange
}

// NewChangeAccumulator returns an empty accumulator.
func NewChangeAccumulator() *ChangeAccumulator {
	return &ChangeAccumulator{
		portfolioIdx:  map[string]int{},
		accountIdx:    map[string]int{},
		instrumentIdx: map[string]int{},
	}
}

// AddBatch folds the batch's dimension records into the
// accumulator. Fact records (positions, cash, fx) are not touched —
// they are insert-only and stay on the per-batch write path.
//
// Enum-typed columns are validated on every record, not just the
// folded survivors the upsert re-checks, so an adapter emitting an
// invalid record fails the load even when a later record supersedes
// it.
func (a *ChangeAccumulator) AddBatch(b *canonical.SnapshotBatch) error {
	for i := range b.Portfolios {
		r := b.Portfolios[i]
		k := dimKey(r.SilverSourceID, r.PortfolioExternalID)
		if j, ok := a.portfolioIdx[k]; ok {
			a.portfolios[j] = foldPortfolio(a.portfolios[j], r)
			continue
		}
		a.portfolioIdx[k] = len(a.portfolios)
		a.portfolios = append(a.portfolios, r)
	}
	for i := range b.Accounts {
		r := b.Accounts[i]
		if err := validateAccountEnums("AddBatch account", i, &r); err != nil {
			return err
		}
		k := dimKey(r.SilverSourceID, r.AccountExternalID)
		if j, ok := a.accountIdx[k]; ok {
			a.accounts[j] = foldAccount(a.accounts[j], r)
			continue
		}
		a.accountIdx[k] = len(a.accounts)
		a.accounts = append(a.accounts, r)
	}
	for i := range b.Instruments {
		r := b.Instruments[i]
		if err := validateTaxonomyPair("AddBatch instrument", i, r.AssetClass, r.Vehicle); err != nil {
			return err
		}
		k := dimKey(r.SilverSourceID, r.InstrumentExternalID)
		if j, ok := a.instrumentIdx[k]; ok {
			a.instruments[j] = foldInstrument(a.instruments[j], r)
			continue
		}
		a.instrumentIdx[k] = len(a.instruments)
		a.instruments = append(a.instruments, r)
	}
	return nil
}

// Flush upserts the folded records, portfolios before accounts
// (accounts name their parent portfolio) before instruments —
// the same relative order the per-batch path used. Records keep
// their first-arrival order within each dimension.
func (a *ChangeAccumulator) Flush(ctx context.Context, w *Writer) error {
	if err := w.UpsertPortfolios(ctx, a.portfolios); err != nil {
		return err
	}
	if err := w.UpsertAccounts(ctx, a.accounts); err != nil {
		return err
	}
	return w.UpsertInstruments(ctx, a.instruments)
}

// dimKey scopes an external id by its silver_source_id, matching
// the dimension tables' composite primary keys. Loads stamp one
// source id across the whole stream, but the fold shouldn't have
// to rely on that.
func dimKey(sourceID, externalID string) string {
	return sourceID + "\x00" + externalID
}

// foldPortfolio applies next onto acc exactly as the §8.4 upsert
// would if next arrived while acc were the stored row: the newer
// record's non-nil attributes win, and the older record's fill
// whatever is still absent.
func foldPortfolio(acc, next canonical.PortfolioChange) canonical.PortfolioChange {
	newer, older := next, acc
	if next.LastSeenAt < acc.LastSeenAt {
		newer, older = acc, next
	}
	out := newer
	out.DisplayName = coalesce(newer.DisplayName, older.DisplayName)
	out.BaseCurrency = coalesce(newer.BaseCurrency, older.BaseCurrency)
	out.RelationshipID = coalesce(newer.RelationshipID, older.RelationshipID)
	out.Nickname = coalesce(newer.Nickname, older.Nickname)
	out.Payload = coalesceJSON(newer.Payload, older.Payload)
	out.FirstSeenAt = min(acc.FirstSeenAt, next.FirstSeenAt)
	out.LastSeenAt = max(acc.LastSeenAt, next.LastSeenAt)
	return out
}

// foldAccount mirrors UpsertAccounts' guard. AccountKind is
// non-nullable — the newer record's value wins outright, like the
// SQL's bare CASE (no COALESCE). Ties (equal LastSeenAt) go to
// `next`, matching the SQL's `>=`.
func foldAccount(acc, next canonical.AccountChange) canonical.AccountChange {
	newer, older := next, acc
	if next.LastSeenAt < acc.LastSeenAt {
		newer, older = acc, next
	}
	out := newer
	out.DisplayName = coalesce(newer.DisplayName, older.DisplayName)
	out.BaseCurrency = coalesce(newer.BaseCurrency, older.BaseCurrency)
	out.RelationshipID = coalesce(newer.RelationshipID, older.RelationshipID)
	out.Nickname = coalesce(newer.Nickname, older.Nickname)
	out.AccountCategory = coalesce(newer.AccountCategory, older.AccountCategory)
	out.PortfolioExternalID = coalesce(newer.PortfolioExternalID, older.PortfolioExternalID)
	out.TaxWrapper = coalesce(newer.TaxWrapper, older.TaxWrapper)
	out.ManagementStyle = coalesce(newer.ManagementStyle, older.ManagementStyle)
	out.Payload = coalesceJSON(newer.Payload, older.Payload)
	out.FirstSeenAt = min(acc.FirstSeenAt, next.FirstSeenAt)
	out.LastSeenAt = max(acc.LastSeenAt, next.LastSeenAt)
	return out
}

// foldInstrument mirrors UpsertInstruments' guard. AssetClass and
// Vehicle are non-nullable — the newer record's pair wins outright.
func foldInstrument(acc, next canonical.InstrumentChange) canonical.InstrumentChange {
	newer, older := next, acc
	if next.LastSeenAt < acc.LastSeenAt {
		newer, older = acc, next
	}
	out := newer
	out.ISIN = coalesce(newer.ISIN, older.ISIN)
	out.CUSIP = coalesce(newer.CUSIP, older.CUSIP)
	out.Symbol = coalesce(newer.Symbol, older.Symbol)
	out.Name = coalesce(newer.Name, older.Name)
	out.Currency = coalesce(newer.Currency, older.Currency)
	out.Payload = coalesceJSON(newer.Payload, older.Payload)
	out.FirstSeenAt = min(acc.FirstSeenAt, next.FirstSeenAt)
	out.LastSeenAt = max(acc.LastSeenAt, next.LastSeenAt)
	return out
}

// coalesce returns next unless it is nil — the Go-side twin of the
// upserts' COALESCE(EXCLUDED.col, stored.col).
func coalesce[T any](next, acc *T) *T {
	if next != nil {
		return next
	}
	return acc
}

// coalesceJSON is coalesce for payloads, where "not carried" is an
// empty RawMessage rather than a nil pointer (see nullableJSON).
func coalesceJSON(next, acc json.RawMessage) json.RawMessage {
	if len(next) > 0 {
		return next
	}
	return acc
}
