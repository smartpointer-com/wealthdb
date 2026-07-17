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
// semantics the SQL applies (attributes update only when the
// incoming last_seen_at is >= the accumulated one, per-column
// COALESCE so a missing value never clobbers a present one; the
// seen-at range unions either way). Folding first and upserting once
// is therefore equivalent to upserting record-by-record whenever the
// entity is new to gold — which covers every `reload -a` rebuild.
// When gold already holds the entity, one narrow interleave differs:
// an in-load record OLDER than the stored row, followed by a NEWER
// one that lacks a value for some column. Record-by-record, the
// older record's value is discarded against the stored row before
// the newer record arrives, so the column keeps the stored value;
// folded, the older value survives into the merged record and wins.
// That interleave needs an adapter to emit observations predating
// data gold has already loaded (a historical backfill landing in an
// incremental window) — see TestChangeAccumulatorPreexistingRow,
// which pins the folded outcome.
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
// would if next arrived while acc were the stored row.
func foldPortfolio(acc, next canonical.PortfolioChange) canonical.PortfolioChange {
	out := acc
	if next.LastSeenAt >= acc.LastSeenAt {
		out = next
		out.DisplayName = coalesce(next.DisplayName, acc.DisplayName)
		out.BaseCurrency = coalesce(next.BaseCurrency, acc.BaseCurrency)
		out.RelationshipID = coalesce(next.RelationshipID, acc.RelationshipID)
		out.Nickname = coalesce(next.Nickname, acc.Nickname)
		out.Payload = coalesceJSON(next.Payload, acc.Payload)
	}
	out.FirstSeenAt = min(acc.FirstSeenAt, next.FirstSeenAt)
	out.LastSeenAt = max(acc.LastSeenAt, next.LastSeenAt)
	return out
}

// foldAccount mirrors UpsertAccounts' guard. AccountKind is
// non-nullable — the newer record's value wins outright, like the
// SQL's bare CASE (no COALESCE).
func foldAccount(acc, next canonical.AccountChange) canonical.AccountChange {
	out := acc
	if next.LastSeenAt >= acc.LastSeenAt {
		out = next
		out.DisplayName = coalesce(next.DisplayName, acc.DisplayName)
		out.BaseCurrency = coalesce(next.BaseCurrency, acc.BaseCurrency)
		out.RelationshipID = coalesce(next.RelationshipID, acc.RelationshipID)
		out.Nickname = coalesce(next.Nickname, acc.Nickname)
		out.AccountCategory = coalesce(next.AccountCategory, acc.AccountCategory)
		out.PortfolioExternalID = coalesce(next.PortfolioExternalID, acc.PortfolioExternalID)
		out.TaxWrapper = coalesce(next.TaxWrapper, acc.TaxWrapper)
		out.ManagementStyle = coalesce(next.ManagementStyle, acc.ManagementStyle)
		out.Payload = coalesceJSON(next.Payload, acc.Payload)
	}
	out.FirstSeenAt = min(acc.FirstSeenAt, next.FirstSeenAt)
	out.LastSeenAt = max(acc.LastSeenAt, next.LastSeenAt)
	return out
}

// foldInstrument mirrors UpsertInstruments' guard. AssetClass and
// Vehicle are non-nullable — the newer record's pair wins outright.
func foldInstrument(acc, next canonical.InstrumentChange) canonical.InstrumentChange {
	out := acc
	if next.LastSeenAt >= acc.LastSeenAt {
		out = next
		out.ISIN = coalesce(next.ISIN, acc.ISIN)
		out.CUSIP = coalesce(next.CUSIP, acc.CUSIP)
		out.Symbol = coalesce(next.Symbol, acc.Symbol)
		out.Name = coalesce(next.Name, acc.Name)
		out.Currency = coalesce(next.Currency, acc.Currency)
		out.Payload = coalesceJSON(next.Payload, acc.Payload)
	}
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
