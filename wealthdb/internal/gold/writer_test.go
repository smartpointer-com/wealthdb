package gold

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// openMigrated returns a fresh copy of the migrated gold image, opened
// read-write. A silver_sources row is seeded so subsequent inserts can
// satisfy the foreign key.
func openMigrated(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("OpenFresh: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(
            silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at
        ) VALUES ('test-src', 'schwab', '/tmp/test.db', -1, 0, 0)
    `); err != nil {
		t.Fatalf("seed silver_sources: %v", err)
	}
	return db, ctx
}

// inTx runs fn inside a transaction with a Writer, committing on
// success. Cleans up by ensuring rollback on failure.
func inTx(t *testing.T, db *sql.DB, ctx context.Context, fn func(*Writer) error) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := fn(NewWriter(tx)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("writer fn: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestUpsertAccountsFreshInsert(t *testing.T) {
	db, ctx := openMigrated(t)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID:    "test-src",
			AccountExternalID: "ACC0001",
			AccountKind:       canonical.AccountKindBrokerage,
			DisplayName:       ptr("Test Brokerage"),
			BaseCurrency:      ptr("USD"),
			FirstSeenAt:       1000,
			LastSeenAt:        2000,
			Payload:           json.RawMessage(`{"raw":"ok"}`),
		}})
	})

	var (
		kind, dn, bc, payload string
		fs, ls                int64
	)
	err := db.QueryRowContext(ctx, `
        SELECT account_kind, display_name, base_currency,
               first_seen_at, last_seen_at, CAST(payload AS VARCHAR)
          FROM accounts
         WHERE silver_source_id='test-src' AND account_external_id='ACC0001'
    `).Scan(&kind, &dn, &bc, &fs, &ls, &payload)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if kind != "brokerage" || dn != "Test Brokerage" || bc != "USD" {
		t.Errorf("got kind=%q dn=%q bc=%q", kind, dn, bc)
	}
	if fs != 1000 || ls != 2000 {
		t.Errorf("got first_seen=%d last_seen=%d, want 1000/2000", fs, ls)
	}
	if payload != `{"raw":"ok"}` {
		t.Errorf("got payload=%q", payload)
	}
}

// TestUpsertAccountsGuard exercises the §8.4 guard:
// re-emitting an account with last_seen_at < stored.last_seen_at
// must not overwrite the stored attributes.
func TestUpsertAccountsGuard(t *testing.T) {
	db, ctx := openMigrated(t)

	// First insert at t=2000 with display_name "Newer"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID:    "test-src",
			AccountExternalID: "ACC0002",
			AccountKind:       canonical.AccountKindCash,
			DisplayName:       ptr("Newer"),
			FirstSeenAt:       1500,
			LastSeenAt:        2000,
		}})
	})

	// Re-emit at t=1000 with display_name "Older". Should NOT
	// overwrite; first_seen_at should retreat to 1000.
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID:    "test-src",
			AccountExternalID: "ACC0002",
			AccountKind:       canonical.AccountKindCash,
			DisplayName:       ptr("Older"),
			FirstSeenAt:       1000,
			LastSeenAt:        1500, // older than stored
		}})
	})

	var dn string
	var fs, ls int64
	if err := db.QueryRowContext(ctx,
		`SELECT display_name, first_seen_at, last_seen_at
           FROM accounts WHERE account_external_id='ACC0002'`,
	).Scan(&dn, &fs, &ls); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if dn != "Newer" {
		t.Errorf("display_name = %q, want %q (guard should have blocked older overwrite)", dn, "Newer")
	}
	if ls != 2000 {
		t.Errorf("last_seen_at = %d, want 2000 (must not retreat)", ls)
	}
	if fs != 1000 {
		t.Errorf("first_seen_at = %d, want 1000 (should expand backward)", fs)
	}
}

// TestUpsertAccountsAdvance exercises the happy path: a newer
// last_seen_at observation overwrites attributes.
func TestUpsertAccountsAdvance(t *testing.T) {
	db, ctx := openMigrated(t)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID:    "test-src",
			AccountExternalID: "ACC0003",
			AccountKind:       canonical.AccountKindBrokerage,
			DisplayName:       ptr("Old"),
			FirstSeenAt:       1000,
			LastSeenAt:        1500,
		}})
	})
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID:    "test-src",
			AccountExternalID: "ACC0003",
			AccountKind:       canonical.AccountKindBrokerage,
			DisplayName:       ptr("New"),
			FirstSeenAt:       1200,
			LastSeenAt:        2000,
		}})
	})

	var dn string
	var fs, ls int64
	if err := db.QueryRowContext(ctx,
		`SELECT display_name, first_seen_at, last_seen_at
           FROM accounts WHERE account_external_id='ACC0003'`,
	).Scan(&dn, &fs, &ls); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if dn != "New" {
		t.Errorf("display_name = %q, want %q", dn, "New")
	}
	if fs != 1000 {
		t.Errorf("first_seen_at = %d, want 1000 (min)", fs)
	}
	if ls != 2000 {
		t.Errorf("last_seen_at = %d, want 2000 (max)", ls)
	}
}

func TestInsertPositionsNullable(t *testing.T) {
	db, ctx := openMigrated(t)

	// Seed an account so the FK is satisfied.
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC", AccountKind: canonical.AccountKindBrokerage,
			FirstSeenAt: 1, LastSeenAt: 1,
		}})
	})

	qty := canonical.NewDecimalFromInt(100)
	mv, _ := canonical.NewDecimalFromString("12345.6789")
	acqDate := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			// Fully populated row
			{
				SilverSourceID: "test-src", SnapshotAt: 1000,
				AccountExternalID: "ACC", PositionKey: "AAPL",
				InstrumentExternalID: ptr("US0000000010"),
				AssetClass:           canonical.AssetClassPublicEquity,
				Vehicle:              canonical.VehicleStock,
				Currency:             "USD",
				Quantity:             &qty,
				MarketValue:          &mv,
				AcquisitionDate:      &acqDate,
			},
			// Minimal row, all nullables NULL
			{
				SilverSourceID: "test-src", SnapshotAt: 1000,
				AccountExternalID: "ACC", PositionKey: "MINIMAL",
				AssetClass: canonical.AssetClassOther,
				Vehicle:    canonical.VehicleOther,
				Currency:   "USD",
			},
		})
	})

	// Verify the populated row preserves decimal precision and the date.
	var (
		isin     sql.NullString
		quantity sql.NullString
		mvScan   sql.NullString
		date     sql.NullTime
	)
	err := db.QueryRowContext(ctx, `
        SELECT instrument_external_id, CAST(quantity AS VARCHAR),
               CAST(market_value AS VARCHAR), acquisition_date
          FROM positions
         WHERE position_key='AAPL'
    `).Scan(&isin, &quantity, &mvScan, &date)
	if err != nil {
		t.Fatalf("read populated row: %v", err)
	}
	if !isin.Valid || isin.String != "US0000000010" {
		t.Errorf("isin = %v, want US0000000010", isin)
	}
	if !quantity.Valid || quantity.String != "100.00000000" {
		t.Errorf("quantity = %v, want 100.00000000", quantity)
	}
	if !mvScan.Valid || mvScan.String != "12345.6789" {
		t.Errorf("market_value = %v, want 12345.6789", mvScan)
	}
	if !date.Valid || !date.Time.Equal(acqDate) {
		t.Errorf("acquisition_date = %v, want %v", date, acqDate)
	}

	// Verify the minimal row's nullables are actually NULL.
	var (
		minISIN sql.NullString
		minQty  sql.NullString
		minDate sql.NullTime
	)
	err = db.QueryRowContext(ctx, `
        SELECT instrument_external_id, CAST(quantity AS VARCHAR), acquisition_date
          FROM positions
         WHERE position_key='MINIMAL'
    `).Scan(&minISIN, &minQty, &minDate)
	if err != nil {
		t.Fatalf("read minimal row: %v", err)
	}
	if minISIN.Valid || minQty.Valid || minDate.Valid {
		t.Errorf("expected NULLs, got isin=%v qty=%v date=%v", minISIN, minQty, minDate)
	}
}

func TestInsertCashBalancesAndFxRatesAndTransactions(t *testing.T) {
	db, ctx := openMigrated(t)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC", AccountKind: canonical.AccountKindCash,
			FirstSeenAt: 1, LastSeenAt: 1,
		}})
	})

	usd1k := canonical.NewDecimalFromInt(1000)
	fxRate, _ := canonical.NewDecimalFromString("0.8765432100")
	grossAmt, _ := canonical.NewDecimalFromString("550.25")

	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertCashBalances(ctx, []canonical.CashBalanceChange{{
			SilverSourceID: "test-src", SnapshotAt: 1000,
			AccountExternalID: "ACC", Currency: "USD",
			BalanceKind: canonical.BalanceKindClosing,
			Amount:      usd1k,
		}}); err != nil {
			return err
		}
		if err := w.InsertFxRates(ctx, []canonical.FxRateChange{{
			SilverSourceID: "test-src", SnapshotAt: 1000,
			BaseCurrency: "USD", QuoteCurrency: "CHF",
			MidRate: fxRate,
		}}); err != nil {
			return err
		}
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: "test-src", TransactionExternalID: "TX1",
			OccurredAt: 1500, AccountExternalID: "ACC",
			Kind: canonical.TxKindDividend, Currency: "USD",
			GrossAmount: &grossAmt,
		}})
	})

	var amount, rate, gross string
	if err := db.QueryRowContext(ctx,
		`SELECT CAST(amount AS VARCHAR) FROM cash_balances`).Scan(&amount); err != nil {
		t.Fatalf("read cash_balances: %v", err)
	}
	if amount != "1000.0000" {
		t.Errorf("cash amount = %q, want 1000.0000", amount)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT CAST(mid_rate AS VARCHAR) FROM fx_rates`).Scan(&rate); err != nil {
		t.Fatalf("read fx_rates: %v", err)
	}
	if rate != "0.8765432100" {
		t.Errorf("fx rate = %q, want 0.8765432100", rate)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT CAST(gross_amount AS VARCHAR) FROM transactions`).Scan(&gross); err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	if gross != "550.2500" {
		t.Errorf("gross_amount = %q, want 550.2500", gross)
	}
}

// TestInsertTransactionsEnrichment covers the 0038 columns through
// the writer: a set pair round-trips verbatim (counterparty feeds the
// merchant signature, so byte-for-byte fidelity is the contract), and
// the nil pair — every non-card source — lands as SQL NULL.
func TestInsertTransactionsEnrichment(t *testing.T) {
	db, ctx := openMigrated(t)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: "test-src", TransactionExternalID: "TX-CARD",
			OccurredAt: 1500, AccountExternalID: "CARD",
			Kind: canonical.TxKindPurchase, Currency: "USD",
			Counterparty:     ptr("EXAMPLE COFFEE BAR #0042"),
			ProviderCategory: ptr("Food & Drink"),
		}, {
			SilverSourceID: "test-src", TransactionExternalID: "TX-PLAIN",
			OccurredAt: 1500, AccountExternalID: "ACC",
			Kind: canonical.TxKindDividend, Currency: "USD",
		}})
	})

	var cp, pc sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT counterparty, provider_category FROM transactions
         WHERE transaction_external_id = 'TX-CARD'`).Scan(&cp, &pc); err != nil {
		t.Fatalf("read back card row: %v", err)
	}
	if cp.String != "EXAMPLE COFFEE BAR #0042" || pc.String != "Food & Drink" {
		t.Errorf("got counterparty=%q provider_category=%q", cp.String, pc.String)
	}

	if err := db.QueryRowContext(ctx, `
        SELECT counterparty, provider_category FROM transactions
         WHERE transaction_external_id = 'TX-PLAIN'`).Scan(&cp, &pc); err != nil {
		t.Fatalf("read back plain row: %v", err)
	}
	if cp.Valid || pc.Valid {
		t.Errorf("unset pair = (%v,%v), want both NULL", cp, pc)
	}
}

func TestInvalidEnumRejected(t *testing.T) {
	db, ctx := openMigrated(t)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	err = NewWriter(tx).UpsertAccounts(ctx, []canonical.AccountChange{{
		SilverSourceID:    "test-src",
		AccountExternalID: "ACC",
		AccountKind:       canonical.AccountKind("garbage"),
		FirstSeenAt:       1, LastSeenAt: 1,
	}})
	if err == nil {
		t.Fatal("expected error for invalid account_kind, got nil")
	}
}

func TestEmptyBatchIsNoop(t *testing.T) {
	db, ctx := openMigrated(t)

	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertAccounts(ctx, nil); err != nil {
			return err
		}
		return w.InsertPositions(ctx, nil)
	})
	// No assertion beyond "doesn't error / doesn't insert".
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("accounts count = %d, want 0", n)
	}
}

// TestPositionTaxonomyPair covers the 2-D taxonomy columns: a valid
// (asset_class, vehicle) pair round-trips, and a missing, invalid,
// or nonsensical pair is rejected before it reaches gold.
func TestPositionTaxonomyPair(t *testing.T) {
	db, ctx := openMigrated(t)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "src", AccountExternalID: "ACC", AccountKind: canonical.AccountKindBrokerage,
			FirstSeenAt: 1, LastSeenAt: 1,
		}})
	})
	base := func(key string) canonical.PositionChange {
		return canonical.PositionChange{
			SilverSourceID: "src", SnapshotAt: 1000, AccountExternalID: "ACC",
			PositionKey: key, AssetClass: canonical.AssetClassPublicEquity,
			Vehicle: canonical.VehicleStock, Currency: "USD",
		}
	}

	// A valid pair round-trips into asset_class + vehicle.
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{base("WITH")})
	})
	var ac, veh string
	db.QueryRowContext(ctx, `SELECT asset_class, vehicle FROM positions WHERE position_key='WITH'`).Scan(&ac, &veh)
	if ac != "public_equity" || veh != "stock" {
		t.Errorf("WITH pair = (%q,%q), want (public_equity,stock)", ac, veh)
	}

	// The pair is required and must be admitted.
	reject := func(name string, mut func(*canonical.PositionChange)) {
		p := base("REJ")
		mut(&p)
		if err := insertOne(ctx, db, p); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
	reject("missing vehicle", func(p *canonical.PositionChange) { p.Vehicle = "" })
	reject("invalid exposure", func(p *canonical.PositionChange) { p.AssetClass = "bogus" })
	reject("legacy value in asset_class", func(p *canonical.PositionChange) { p.AssetClass = canonical.AssetClassEquity })
	reject("nonsensical pair", func(p *canonical.PositionChange) {
		p.AssetClass, p.Vehicle = canonical.AssetClassCrypto, canonical.VehicleMortgage
	})
}

func insertOne(ctx context.Context, db *sql.DB, p canonical.PositionChange) error {
	tx, _ := db.BeginTx(ctx, nil)
	defer tx.Rollback()
	return NewWriter(tx).InsertPositions(ctx, []canonical.PositionChange{p})
}

// TestInsertPositionsChunkBoundaries exercises the multi-row VALUES
// chunking across its edges: exactly one chunk, one row over, and a
// multiple-chunks-plus-remainder batch. Every row must land, with
// values intact at both ends of the batch.
func TestInsertPositionsChunkBoundaries(t *testing.T) {
	db, ctx := openMigrated(t)

	for _, n := range []int{1, InsertChunkRows - 1, InsertChunkRows, InsertChunkRows + 1, 2*InsertChunkRows + 3} {
		batch := make([]canonical.PositionChange, n)
		for i := range batch {
			qty := canonical.NewDecimalFromInt(int64(i))
			batch[i] = canonical.PositionChange{
				SilverSourceID: "test-src", SnapshotAt: int64(i),
				AccountExternalID: "ACC", PositionKey: "VTI",
				AssetClass: canonical.AssetClassPublicEquity,
				Vehicle:    canonical.VehicleETF,
				Currency:   "USD",
				Quantity:   &qty,
			}
		}
		inTx(t, db, ctx, func(w *Writer) error {
			return w.InsertPositions(ctx, batch)
		})

		var count int
		var firstQty, lastQty string
		if err := db.QueryRowContext(ctx, `
            SELECT COUNT(*),
                   CAST(MIN(quantity) AS VARCHAR),
                   CAST(MAX(quantity) AS VARCHAR)
              FROM positions
        `).Scan(&count, &firstQty, &lastQty); err != nil {
			t.Fatalf("n=%d: read back: %v", n, err)
		}
		if count != n {
			t.Errorf("n=%d: row count = %d", n, count)
		}
		wantLast := fmt.Sprintf("%d.00000000", n-1)
		if firstQty != "0.00000000" || lastQty != wantLast {
			t.Errorf("n=%d: quantity range = [%s,%s], want [0.00000000,%s]", n, firstQty, lastQty, wantLast)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM positions`); err != nil {
			t.Fatalf("n=%d: clear: %v", n, err)
		}
	}
}

// TestInsertTransactionsChunked pushes one multi-chunk batch through
// the widest remaining insert path and spot-checks both ends.
func TestInsertTransactionsChunked(t *testing.T) {
	db, ctx := openMigrated(t)

	n := InsertChunkRows + 7
	batch := make([]canonical.TransactionChange, n)
	for i := range batch {
		amt := canonical.NewDecimalFromInt(int64(i))
		batch[i] = canonical.TransactionChange{
			SilverSourceID:        "test-src",
			TransactionExternalID: fmt.Sprintf("TX%06d", i),
			OccurredAt:            int64(i),
			AccountExternalID:     "ACC",
			Kind:                  canonical.TxKindDividend,
			Currency:              "USD",
			GrossAmount:           &amt,
		}
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, batch)
	})

	var count int
	var lastGross string
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*), CAST(MAX(gross_amount) AS VARCHAR) FROM transactions
    `).Scan(&count, &lastGross); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if count != n {
		t.Errorf("row count = %d, want %d", count, n)
	}
	if want := fmt.Sprintf("%d.0000", n-1); lastGross != want {
		t.Errorf("max gross_amount = %s, want %s", lastGross, want)
	}
}

// TestInsertTransactionsComposesDescription pins where the memo
// separator gets its one meaning. Every adapter's rows enter gold
// through InsertTransactions, which stores the narrative with any
// separator it carried folded and the memo, if any, behind the
// separator — so a description read back splits into exactly the
// narrative and memo the change carried, and a narrative copied
// verbatim from a source that prints a spaced em dash can never be
// read as a memo. Every value is synthetic.
func TestInsertTransactionsComposesDescription(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('test-src', 'ACC', 'cash', 'Everyday', 1, 1)`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	cases := []struct {
		id                      string
		description, memo       *string
		want                    string
		wantNarrative, wantMemo string
	}{
		{"T-PLAIN", ptr("EXAMPLE PAYEE; EXAMPLE STREET 1"), nil,
			"EXAMPLE PAYEE; EXAMPLE STREET 1", "EXAMPLE PAYEE; EXAMPLE STREET 1", ""},
		{"T-MEMO", ptr("EXAMPLE PAYEE; EXAMPLE STREET 1"), ptr("THANKS"),
			"EXAMPLE PAYEE; EXAMPLE STREET 1 — THANKS", "EXAMPLE PAYEE; EXAMPLE STREET 1", "THANKS"},
		{"T-STRAY", ptr("EXAMPLE PAYEE — EXAMPLE BRANCH"), nil,
			"EXAMPLE PAYEE - EXAMPLE BRANCH", "EXAMPLE PAYEE - EXAMPLE BRANCH", ""},
		{"T-STRAY-MEMO", ptr("EXAMPLE PAYEE — EXAMPLE BRANCH"), ptr("THANKS"),
			"EXAMPLE PAYEE - EXAMPLE BRANCH — THANKS", "EXAMPLE PAYEE - EXAMPLE BRANCH", "THANKS"},
		{"T-MEMO-ONLY", nil, ptr("THANKS"), "— THANKS", "", "THANKS"},
		{"T-BLANK-MEMO", ptr("credit"), ptr("  "), "credit", "credit", ""},
	}
	batch := make([]canonical.TransactionChange, 0, len(cases)+1)
	for _, tc := range cases {
		batch = append(batch, canonical.TransactionChange{
			SilverSourceID: "test-src", TransactionExternalID: tc.id,
			OccurredAt: 1500, AccountExternalID: "ACC",
			Kind: canonical.TxKindWithdrawal, Currency: "USD",
			Description: tc.description, Memo: tc.memo,
		})
	}
	batch = append(batch, canonical.TransactionChange{
		SilverSourceID: "test-src", TransactionExternalID: "T-NONE",
		OccurredAt: 1500, AccountExternalID: "ACC",
		Kind: canonical.TxKindWithdrawal, Currency: "USD",
	})
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertTransactions(ctx, batch) })

	for _, tc := range cases {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT description FROM transactions WHERE transaction_external_id = ?`, tc.id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("%s description = %q, want %q", tc.id, got, tc.want)
		}
		if narrative, memo := canonical.SplitDescriptionMemo(got); narrative != tc.wantNarrative || memo != tc.wantMemo {
			t.Errorf("%s splits to (%q, %q), want (%q, %q)", tc.id, narrative, memo, tc.wantNarrative, tc.wantMemo)
		}
	}
	var none sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT description FROM transactions WHERE transaction_external_id = 'T-NONE'`).Scan(&none); err != nil {
		t.Fatalf("read T-NONE: %v", err)
	}
	if none.Valid {
		t.Errorf("a change with neither narrative nor memo stored %q, want NULL", none.String)
	}
}

// TestInsertTransactionsRefusesAnInadmissiblePair holds a trade to the
// bar an instrument is held to. Two individually valid halves can still
// be a combination the taxonomy does not admit, and a trade is no freer
// to invent one than a holding is — the gate checked each half alone
// and let the pair through.
func TestInsertTransactionsRefusesAnInadmissiblePair(t *testing.T) {
	db, ctx := openMigrated(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	w := NewWriter(tx)
	err = w.InsertTransactions(ctx, []canonical.TransactionChange{{
		SilverSourceID:        "cf",
		TransactionExternalID: "T1",
		AccountExternalID:     "A1",
		Kind:                  canonical.TxKindBuy,
		Currency:              "USD",
		// Cash exposure in an option wrapper: each half is a real
		// value, the pair is not one the taxonomy admits.
		AssetClass: canonical.AssetClassCash,
		Vehicle:    canonical.VehicleOption,
	}})
	if err == nil {
		t.Fatal("an inadmissible (asset_class, vehicle) pair reached gold")
	}
	if !strings.Contains(err.Error(), "InsertTransactions") {
		t.Errorf("error does not name the writer: %v", err)
	}
}
