package gold

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
)

// Writer wraps a *sql.Tx and inserts/upserts canonical *Change
// records into the gold tables. Each method operates on a batch
// to amortise prepare-statement overhead.
//
// Caller owns the transaction lifecycle: BeginTx, call writer
// methods, Commit or Rollback. A Writer is not goroutine-safe;
// use one per active transaction.
type Writer struct {
	tx *sql.Tx
}

// NewWriter constructs a Writer that writes into the given
// transaction. Caller is responsible for Commit / Rollback.
func NewWriter(tx *sql.Tx) *Writer {
	return &Writer{tx: tx}
}

// UpsertAccounts inserts/updates `accounts` rows. Implements the
// docs/DESIGN.md §8.4 guard: a re-emitted older observation does
// not overwrite newer-observed attributes.
func (w *Writer) UpsertAccounts(ctx context.Context, batch []canonical.AccountChange) error {
	if len(batch) == 0 {
		return nil
	}
	// Guard semantics per docs/DESIGN.md §8.4: re-emitting an
	// older observation must not overwrite newer-observed
	// attributes, but the seen-at range still expands in both
	// directions. We use per-column CASE to keep attributes
	// pinned when EXCLUDED is older, while letting first_seen_at
	// and last_seen_at always reflect the union.
	const q = `
INSERT INTO accounts (
    silver_source_id, account_external_id, account_kind,
    display_name, base_currency, relationship_id,
    first_seen_at, last_seen_at, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (silver_source_id, account_external_id) DO UPDATE SET
    account_kind    = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                           THEN EXCLUDED.account_kind ELSE accounts.account_kind END,
    display_name    = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                           THEN EXCLUDED.display_name ELSE accounts.display_name END,
    base_currency   = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                           THEN EXCLUDED.base_currency ELSE accounts.base_currency END,
    relationship_id = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                           THEN EXCLUDED.relationship_id ELSE accounts.relationship_id END,
    payload         = CASE WHEN EXCLUDED.last_seen_at >= accounts.last_seen_at
                           THEN EXCLUDED.payload ELSE accounts.payload END,
    first_seen_at   = LEAST   (accounts.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at    = GREATEST(accounts.last_seen_at,  EXCLUDED.last_seen_at)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare UpsertAccounts: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if !r.AccountKind.Valid() {
			return fmt.Errorf("UpsertAccounts row %d: invalid account_kind %q", i, r.AccountKind)
		}
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.AccountExternalID, string(r.AccountKind),
			nullableString(r.DisplayName), nullableString(r.BaseCurrency),
			nullableString(r.RelationshipID),
			r.FirstSeenAt, r.LastSeenAt, nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("UpsertAccounts row %d: %w", i, err)
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
    silver_source_id, instrument_external_id, asset_class,
    isin, cusip, symbol, name, currency,
    first_seen_at, last_seen_at, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (silver_source_id, instrument_external_id) DO UPDATE SET
    asset_class   = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.asset_class ELSE instruments.asset_class END,
    isin          = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.isin ELSE instruments.isin END,
    cusip         = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.cusip ELSE instruments.cusip END,
    symbol        = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.symbol ELSE instruments.symbol END,
    name          = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.name ELSE instruments.name END,
    currency      = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.currency ELSE instruments.currency END,
    payload       = CASE WHEN EXCLUDED.last_seen_at >= instruments.last_seen_at
                         THEN EXCLUDED.payload ELSE instruments.payload END,
    first_seen_at = LEAST   (instruments.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at  = GREATEST(instruments.last_seen_at,  EXCLUDED.last_seen_at)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare UpsertInstruments: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if !r.AssetClass.Valid() {
			return fmt.Errorf("UpsertInstruments row %d: invalid asset_class %q", i, r.AssetClass)
		}
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.InstrumentExternalID, string(r.AssetClass),
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

// InsertPositions inserts `positions` rows. Snapshot-grain: the
// caller guarantees the window-DELETE step (per docs/DESIGN.md
// §8.1) has already wiped overlapping rows, so a plain INSERT is
// sufficient.
func (w *Writer) InsertPositions(ctx context.Context, batch []canonical.PositionChange) error {
	if len(batch) == 0 {
		return nil
	}
	const q = `
INSERT INTO positions (
    silver_source_id, snapshot_at, account_external_id, position_key,
    instrument_external_id, asset_class, currency,
    quantity, market_value, book_value, accrued_interest,
    acquisition_date, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare InsertPositions: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if !r.AssetClass.Valid() {
			return fmt.Errorf("InsertPositions row %d: invalid asset_class %q", i, r.AssetClass)
		}
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.SnapshotAt, r.AccountExternalID, r.PositionKey,
			nullableString(r.InstrumentExternalID), string(r.AssetClass), r.Currency,
			nullableDecimal(r.Quantity), nullableDecimal(r.MarketValue),
			nullableDecimal(r.BookValue), nullableDecimal(r.AccruedInterest),
			nullableTime(r.AcquisitionDate), nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("InsertPositions row %d: %w", i, err)
		}
	}
	return nil
}

// InsertCashBalances inserts `cash_balances` rows.
func (w *Writer) InsertCashBalances(ctx context.Context, batch []canonical.CashBalanceChange) error {
	if len(batch) == 0 {
		return nil
	}
	const q = `
INSERT INTO cash_balances (
    silver_source_id, snapshot_at, account_external_id,
    currency, balance_kind, amount, payload
) VALUES (?, ?, ?, ?, ?, ?, ?)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare InsertCashBalances: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if !r.BalanceKind.Valid() {
			return fmt.Errorf("InsertCashBalances row %d: invalid balance_kind %q", i, r.BalanceKind)
		}
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.SnapshotAt, r.AccountExternalID,
			r.Currency, string(r.BalanceKind), r.Amount, nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("InsertCashBalances row %d: %w", i, err)
		}
	}
	return nil
}

// InsertFxRates inserts `fx_rates` rows.
func (w *Writer) InsertFxRates(ctx context.Context, batch []canonical.FxRateChange) error {
	if len(batch) == 0 {
		return nil
	}
	const q = `
INSERT INTO fx_rates (
    silver_source_id, snapshot_at,
    base_currency, quote_currency,
    mid_rate, bid_rate, ask_rate, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare InsertFxRates: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.SnapshotAt,
			r.BaseCurrency, r.QuoteCurrency,
			r.MidRate, nullableDecimal(r.BidRate), nullableDecimal(r.AskRate),
			nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("InsertFxRates row %d: %w", i, err)
		}
	}
	return nil
}

// InsertTransactions inserts `transactions` rows. Like positions,
// caller has already wiped the overlapping occurred_at window.
func (w *Writer) InsertTransactions(ctx context.Context, batch []canonical.TransactionChange) error {
	if len(batch) == 0 {
		return nil
	}
	const q = `
INSERT INTO transactions (
    silver_source_id, transaction_external_id, occurred_at,
    account_external_id, instrument_external_id, kind, currency,
    gross_amount, net_amount, quantity, price, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	stmt, err := w.tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare InsertTransactions: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		r := &batch[i]
		if !r.Kind.Valid() {
			return fmt.Errorf("InsertTransactions row %d: invalid kind %q", i, r.Kind)
		}
		if _, err := stmt.ExecContext(ctx,
			r.SilverSourceID, r.TransactionExternalID, r.OccurredAt,
			r.AccountExternalID, nullableString(r.InstrumentExternalID),
			string(r.Kind), r.Currency,
			nullableDecimal(r.GrossAmount), nullableDecimal(r.NetAmount),
			nullableDecimal(r.Quantity), nullableDecimal(r.Price),
			nullableJSON(r.Payload),
		); err != nil {
			return fmt.Errorf("InsertTransactions row %d: %w", i, err)
		}
	}
	return nil
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
