package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Writer wraps a *sql.Tx and inserts/upserts canonical *Change
// records into the gold tables. The insert-only fact tables
// (positions, cash_balances, fx_rates, transactions) go in as
// multi-row VALUES chunks — DuckDB's per-statement cost dwarfs its
// per-row cost, so one statement per row is the slowest way to feed
// it. The dimension tables upsert row-by-row (the §8.4 guard needs
// ON CONFLICT per row); callers keep those batches small by folding
// duplicate emissions first — see ChangeAccumulator.
//
// Caller owns the transaction lifecycle: BeginTx, call writer
// methods, Commit or Rollback. A Writer is not goroutine-safe;
// use one per active transaction.
type Writer struct {
	tx *sql.Tx
}

// InsertChunkRows is the row count per multi-row VALUES statement.
// Large enough that per-statement setup is amortised into noise,
// small enough that the bind-parameter count (rows × columns) stays
// modest. Exported with InsertChunked so a caller's test can seed a
// batch that straddles a chunk boundary, which is where a bind list
// off by a column hides.
const InsertChunkRows = 500

// InsertChunked executes head + an n-row VALUES list in chunks of
// InsertChunkRows, collecting each row's bind args via appendRow
// (which must append exactly one tuple's worth per call). op labels
// errors, which name a ROW RANGE rather than a row: the statement that
// failed carried a chunk of them.
//
// Exported because the bulk writers outside this package want the same
// shape — DuckDB's per-statement cost dwarfs its per-row cost, so a
// statement per row is the slowest way to feed it — and a second copy
// of the loop would drift from this one on the next tuning of the
// chunk size or the error wording.
func InsertChunked(ctx context.Context, tx *sql.Tx, op, head, tuple string, n int, appendRow func(i int, args []any) []any) error {
	argsPerRow := strings.Count(tuple, "?")
	for off := 0; off < n; off += InsertChunkRows {
		end := min(off+InsertChunkRows, n)
		args := make([]any, 0, (end-off)*argsPerRow)
		for i := off; i < end; i++ {
			args = appendRow(i, args)
		}
		q := head + tuple + strings.Repeat(","+tuple, end-off-1)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("%s rows %d..%d: %w", op, off, end-1, err)
		}
	}
	return nil
}

// NewWriter constructs a Writer that writes into the given
// transaction. Caller is responsible for Commit / Rollback.
func NewWriter(tx *sql.Tx) *Writer {
	return &Writer{tx: tx}
}

// UpsertAccounts inserts/updates `accounts` rows. Implements the
// docs/DESIGN.md §8.4 guard: per column, the newest non-NULL
// observation wins, and an older observation still fills a column
// no newer record has carried.
func (w *Writer) UpsertAccounts(ctx context.Context, batch []canonical.AccountChange) error {
	if len(batch) == 0 {
		return nil
	}
	// Guard semantics per docs/DESIGN.md §8.4: recency arbitrates
	// conflicts, absence never wins. Per-column CASE keeps the
	// seen-at union independent of the attribute guards.
	//
	// On nullable columns BOTH branches are COALESCE-wrapped —
	// NULL means "I don't carry this field", not "set it to NULL":
	//   - newer branch: a later writer's NULL doesn't clobber an
	//     earlier writer's value (schwab-web emits AccountChange
	//     rows without DisplayName; without this they'd erase the
	//     api side's accountNumber);
	//   - older branch: an older record's value still fills a
	//     column that is NULL so far. Without this, a full rebuild
	//     dropped attributes only an older observation carries —
	//     schwab-web's statement-derived tax_wrapper (dated at its
	//     silver snapshot) lost to newer wrapper-less api rows and
	//     silently fell back to the taxable_personal render default.
	const q = `
INSERT INTO accounts (
    silver_source_id, account_external_id, account_kind,
    display_name, base_currency, relationship_id,
    nickname, account_category, portfolio_external_id,
    tax_wrapper, management_style,
    first_seen_at, last_seen_at, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (silver_source_id, account_external_id) DO UPDATE SET
    account_kind     = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN EXCLUDED.account_kind ELSE accounts.account_kind END,
    display_name     = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.display_name, accounts.display_name) ELSE COALESCE(accounts.display_name, EXCLUDED.display_name) END,
    base_currency    = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.base_currency, accounts.base_currency) ELSE COALESCE(accounts.base_currency, EXCLUDED.base_currency) END,
    relationship_id  = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.relationship_id, accounts.relationship_id) ELSE COALESCE(accounts.relationship_id, EXCLUDED.relationship_id) END,
    nickname         = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.nickname, accounts.nickname) ELSE COALESCE(accounts.nickname, EXCLUDED.nickname) END,
    account_category = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.account_category, accounts.account_category) ELSE COALESCE(accounts.account_category, EXCLUDED.account_category) END,
    portfolio_external_id = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.portfolio_external_id, accounts.portfolio_external_id) ELSE COALESCE(accounts.portfolio_external_id, EXCLUDED.portfolio_external_id) END,
    tax_wrapper      = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.tax_wrapper, accounts.tax_wrapper) ELSE COALESCE(accounts.tax_wrapper, EXCLUDED.tax_wrapper) END,
    management_style = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.management_style, accounts.management_style) ELSE COALESCE(accounts.management_style, EXCLUDED.management_style) END,
    payload          = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                            THEN COALESCE(EXCLUDED.payload, accounts.payload) ELSE COALESCE(accounts.payload, EXCLUDED.payload) END,
    first_seen_at    = LEAST   (accounts.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at     = GREATEST(accounts.last_seen_at,  EXCLUDED.last_seen_at)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare UpsertAccounts: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if err := validateAccountEnums("UpsertAccounts", i, r); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.AccountExternalID, string(r.AccountKind),
			nullableString(r.DisplayName), nullableString(r.BaseCurrency),
			nullableString(r.RelationshipID),
			nullableString(r.Nickname), nullableString(r.AccountCategory),
			nullableString(r.PortfolioExternalID),
			nullableEnumString(r.TaxWrapper), nullableEnumString(r.ManagementStyle),
			r.FirstSeenAt, r.LastSeenAt, nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("UpsertAccounts row %d: %w", i, err)
		}
	}
	return nil
}

// nullableEnumString turns a typed-string pointer into a value
// suitable for sql.Exec — nil → NULL, non-nil → the underlying
// string. Generics let us share one helper across TaxWrapper /
// ManagementStyle without writing two near-identical copies.
func nullableEnumString[T ~string](p *T) any {
	if p == nil {
		return nil
	}
	return string(*p)
}

// UpsertPortfolios writes portfolio rows with the same §8.4 guard
// semantics as UpsertAccounts. Portfolios are gold's own entity
// (distinct from accounts) — see migration 0004 and docs/DESIGN.md
// §13.9.
func (w *Writer) UpsertPortfolios(ctx context.Context, batch []canonical.PortfolioChange) error {
	if len(batch) == 0 {
		return nil
	}
	const q = `
INSERT INTO portfolios (
    silver_source_id, portfolio_external_id,
    display_name, base_currency, relationship_id, nickname,
    first_seen_at, last_seen_at, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (silver_source_id, portfolio_external_id) DO UPDATE SET
    display_name     = CASE WHEN EXCLUDED.last_seen_at >= portfolios.last_seen_at
                            THEN COALESCE(EXCLUDED.display_name, portfolios.display_name) ELSE COALESCE(portfolios.display_name, EXCLUDED.display_name) END,
    base_currency    = CASE WHEN EXCLUDED.last_seen_at >= portfolios.last_seen_at
                            THEN COALESCE(EXCLUDED.base_currency, portfolios.base_currency) ELSE COALESCE(portfolios.base_currency, EXCLUDED.base_currency) END,
    relationship_id  = CASE WHEN EXCLUDED.last_seen_at >= portfolios.last_seen_at
                            THEN COALESCE(EXCLUDED.relationship_id, portfolios.relationship_id) ELSE COALESCE(portfolios.relationship_id, EXCLUDED.relationship_id) END,
    nickname         = CASE WHEN EXCLUDED.last_seen_at >= portfolios.last_seen_at
                            THEN COALESCE(EXCLUDED.nickname, portfolios.nickname) ELSE COALESCE(portfolios.nickname, EXCLUDED.nickname) END,
    payload          = CASE WHEN EXCLUDED.last_seen_at >= portfolios.last_seen_at
                            THEN COALESCE(EXCLUDED.payload, portfolios.payload) ELSE COALESCE(portfolios.payload, EXCLUDED.payload) END,
    first_seen_at    = LEAST   (portfolios.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at     = GREATEST(portfolios.last_seen_at,  EXCLUDED.last_seen_at)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare UpsertPortfolios: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.PortfolioExternalID,
			nullableString(r.DisplayName), nullableString(r.BaseCurrency),
			nullableString(r.RelationshipID), nullableString(r.Nickname),
			r.FirstSeenAt, r.LastSeenAt, nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("UpsertPortfolios row %d: %w", i, err)
		}
	}
	return nil
}

// UpsertInstruments inserts/updates `instruments` rows with the
// same guard semantics as UpsertAccounts.
func (w *Writer) UpsertInstruments(ctx context.Context, batch []canonical.InstrumentChange) error {
	if len(batch) == 0 {
		return nil
	}
	// Same guard semantics as UpsertAccounts; see comment there.
	const q = `
INSERT INTO instruments (
    silver_source_id, instrument_external_id, asset_class, vehicle,
    isin, cusip, symbol, name, currency,
    first_seen_at, last_seen_at, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (silver_source_id, instrument_external_id) DO UPDATE SET
    asset_class   = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.asset_class ELSE instruments.asset_class END,
    vehicle       = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN COALESCE(EXCLUDED.vehicle, instruments.vehicle) ELSE COALESCE(instruments.vehicle, EXCLUDED.vehicle) END,
    isin          = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN COALESCE(EXCLUDED.isin, instruments.isin) ELSE COALESCE(instruments.isin, EXCLUDED.isin) END,
    cusip         = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN COALESCE(EXCLUDED.cusip, instruments.cusip) ELSE COALESCE(instruments.cusip, EXCLUDED.cusip) END,
    symbol        = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN COALESCE(EXCLUDED.symbol, instruments.symbol) ELSE COALESCE(instruments.symbol, EXCLUDED.symbol) END,
    name          = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN COALESCE(EXCLUDED.name, instruments.name) ELSE COALESCE(instruments.name, EXCLUDED.name) END,
    currency      = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN COALESCE(EXCLUDED.currency, instruments.currency) ELSE COALESCE(instruments.currency, EXCLUDED.currency) END,
    payload       = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN COALESCE(EXCLUDED.payload, instruments.payload) ELSE COALESCE(instruments.payload, EXCLUDED.payload) END,
    first_seen_at = LEAST   (instruments.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at  = GREATEST(instruments.last_seen_at,  EXCLUDED.last_seen_at)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare UpsertInstruments: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if err := validateTaxonomyPair("UpsertInstruments", i, r.AssetClass, r.Vehicle); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.InstrumentExternalID, string(r.AssetClass), string(r.Vehicle),
			nullableString(r.ISIN), nullableString(r.CUSIP),
			nullableString(r.Symbol), nullableString(r.Name),
			nullableString(r.Currency),
			r.FirstSeenAt, r.LastSeenAt, nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("UpsertInstruments row %d: %w", i, err)
		}
	}
	return nil
}

// validateAccountEnums validates an AccountChange's enum-typed
// columns. Shared by the upsert (survivor rows) and the
// ChangeAccumulator (every emission, so an invalid record fails the
// load even when a later record supersedes it in the fold).
func validateAccountEnums(op string, i int, r *canonical.AccountChange) error {
	if !r.AccountKind.Valid() {
		return fmt.Errorf("%s row %d: invalid account_kind %q", op, i, r.AccountKind)
	}
	if r.TaxWrapper != nil && !r.TaxWrapper.Valid() {
		return fmt.Errorf("%s row %d: invalid tax_wrapper %q", op, i, *r.TaxWrapper)
	}
	if r.ManagementStyle != nil && !r.ManagementStyle.Valid() {
		return fmt.Errorf("%s row %d: invalid management_style %q", op, i, *r.ManagementStyle)
	}
	return nil
}

// validateTaxonomyPair validates the 2-D taxonomy pair (exposure,
// vehicle) on an instrument/position row before write: both are
// required and must form a taxonomy-admitted combination
// (ValidTaxonomyPair) — a missing or nonsensical pair is an adapter
// bug, caught here before it reaches gold.
func validateTaxonomyPair(op string, i int, a canonical.AssetClass, v canonical.Vehicle) error {
	if !a.Valid() {
		return fmt.Errorf("%s row %d: invalid asset_class %q", op, i, a)
	}
	if !v.Valid() {
		return fmt.Errorf("%s row %d: invalid vehicle %q", op, i, v)
	}
	if !canonical.ValidTaxonomyPair(a, v) {
		return fmt.Errorf("%s row %d: taxonomy pair (%q, %q) is not an admitted combination", op, i, a, v)
	}
	return nil
}

// validateOptionalTaxonomyPair is the same gate for a TRANSACTION,
// where either half may be unset: a trade states only what its
// instrument cannot, so both empty is the ordinary case and one alone
// is legitimate (an option on a share states the wrapper and leaves
// the exposure to the underlying). Each half is checked against its
// own vocabulary when present — which is what keeps a wrapper value
// out of the exposure column — and the two are checked together only
// when both are there to check.
func validateOptionalTaxonomyPair(op string, i int, a canonical.AssetClass, v canonical.Vehicle) error {
	switch {
	case a != "" && v != "":
		return validateTaxonomyPair(op, i, a, v)
	case a != "" && !a.Valid():
		return fmt.Errorf("%s row %d: invalid asset_class %q", op, i, a)
	case v != "" && !v.Valid():
		return fmt.Errorf("%s row %d: invalid vehicle %q", op, i, v)
	}
	return nil
}

// InsertPositions inserts `positions` rows. Snapshot-grain: the
// caller guarantees the window-DELETE step (per docs/DESIGN.md
// §8.1) has already wiped overlapping rows, so a plain INSERT is
// sufficient.
func (w *Writer) InsertPositions(ctx context.Context, batch []canonical.PositionChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i := range batch {
		if err := validateTaxonomyPair("InsertPositions", i, batch[i].AssetClass, batch[i].Vehicle); err != nil {
			return err
		}
	}
	const head = `
INSERT INTO positions (
    silver_source_id, snapshot_at, account_external_id, position_key,
    instrument_external_id, asset_class, vehicle, currency,
    quantity, market_value, book_value, accrued_interest,
    acquisition_date, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "InsertPositions", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SnapshotAt, r.AccountExternalID, r.PositionKey,
				nullableString(r.InstrumentExternalID), string(r.AssetClass), string(r.Vehicle), r.Currency,
				nullableDecimal(r.Quantity), nullableDecimal(r.MarketValue),
				nullableDecimal(r.BookValue), nullableDecimal(r.AccruedInterest),
				nullableTime(r.AcquisitionDate), nullableJSON(r.Payload))
		})
}

// InsertCashBalances inserts `cash_balances` rows.
func (w *Writer) InsertCashBalances(ctx context.Context, batch []canonical.CashBalanceChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i := range batch {
		if !batch[i].BalanceKind.Valid() {
			return fmt.Errorf("InsertCashBalances row %d: invalid balance_kind %q", i, batch[i].BalanceKind)
		}
	}
	const head = `
INSERT INTO cash_balances (
    silver_source_id, snapshot_at, account_external_id,
    currency, balance_kind, amount, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "InsertCashBalances", head,
		`(?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SnapshotAt, r.AccountExternalID,
				r.Currency, string(r.BalanceKind), r.Amount, nullableJSON(r.Payload))
		})
}

// InsertFxRates inserts `fx_rates` rows.
func (w *Writer) InsertFxRates(ctx context.Context, batch []canonical.FxRateChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO fx_rates (
    silver_source_id, snapshot_at,
    base_currency, quote_currency,
    mid_rate, bid_rate, ask_rate, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "InsertFxRates", head,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SnapshotAt,
				r.BaseCurrency, r.QuoteCurrency,
				r.MidRate, nullableDecimal(r.BidRate), nullableDecimal(r.AskRate),
				nullableJSON(r.Payload))
		})
}

// InsertTransactions inserts `transactions` rows. Like positions,
// caller has already wiped the overlapping occurred_at window.
//
// The stored `description` is composed here, from the change's
// Description and Memo (storedDescription): this is the one path
// every adapter's rows take into gold, so it is where the memo
// separator gets its single meaning.
func (w *Writer) InsertTransactions(ctx context.Context, batch []canonical.TransactionChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i := range batch {
		if !batch[i].Kind.Valid() {
			return fmt.Errorf("InsertTransactions row %d: invalid kind %q", i, batch[i].Kind)
		}
		if err := validateOptionalTaxonomyPair("InsertTransactions", i,
			batch[i].AssetClass, batch[i].Vehicle); err != nil {
			return err
		}
	}
	const head = `
INSERT INTO transactions (
    silver_source_id, transaction_external_id, occurred_at,
    account_external_id, instrument_external_id, asset_class, vehicle,
    instrument_hint, kind, currency,
    gross_amount, net_amount, quantity, price, description,
    counterparty, provider_category, check_number, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "InsertTransactions", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.TransactionExternalID, r.OccurredAt,
				r.AccountExternalID, nullableString(r.InstrumentExternalID),
				nullableEnumValue(r.AssetClass), nullableEnumValue(r.Vehicle),
				nullableEnumValue(r.InstrumentHint),
				string(r.Kind), r.Currency,
				nullableDecimal(r.GrossAmount), nullableDecimal(r.NetAmount),
				nullableDecimal(r.Quantity), nullableDecimal(r.Price),
				storedDescription(r),
				nullableString(r.Counterparty), nullableString(r.ProviderCategory),
				nullableString(r.CheckNumber),
				nullableJSON(r.Payload))
		})
}

// nullableEnumValue is nullableEnumString's by-value twin, for a
// typed-string column whose "unset" is the empty string rather than a
// nil pointer — a trade's own exposure, wrapper, or instrument hint,
// all of which the adapter leaves empty where it has nothing to add.
func nullableEnumValue[T ~string](v T) any {
	if v == "" {
		return nil
	}
	return string(v)
}

// storedDescription composes the `description` column from a change's
// narrative and memo (canonical.JoinDescriptionMemo): the narrative
// with any memo separator it carried folded, then the memo, if any,
// behind the separator. A change with neither is NULL; one with a
// narrative and no memo stores the narrative byte for byte unless it
// carried the separator, which no memo-free narrative may.
func storedDescription(r *canonical.TransactionChange) any {
	var narrative, memo string
	if r.Description != nil {
		narrative = *r.Description
	}
	if r.Memo != nil {
		memo = *r.Memo
	}
	if r.Description == nil && strings.TrimSpace(memo) == "" {
		return nil
	}
	return canonical.JoinDescriptionMemo(narrative, memo)
}

// ---- nullable conversion helpers ------------------------------------------

// nullableString returns nil for a nil pointer, the dereferenced
// value otherwise. DuckDB's database/sql driver interprets a nil
// interface as SQL NULL.
func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableDecimal(p *canonical.Decimal) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullableJSON forwards a json.RawMessage as []byte (DuckDB's JSON
// column accepts strings/bytes verbatim). An empty RawMessage maps
// to SQL NULL rather than the literal string "null", because the
// canonical convention is that "no payload" is the absence of the
// column, not a JSON null value.
func nullableJSON(p []byte) any {
	if len(p) == 0 {
		return nil
	}
	return p
}
