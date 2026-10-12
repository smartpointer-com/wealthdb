package gold

import (
	"database/sql"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestSourcesAsOfRollupAndReconcile covers the core SourcesAsOf
// contract:
//
//   - One row per silver source, aggregating positions + cash across
//     all of that source's accounts.
//   - The source's base currency is rolled up agree-or-NULL from its
//     non-overlay accounts.
//   - sum(sources.total_<CCY>) == sum(accounts.total_<CCY>).
func TestSourcesAsOfRollupAndReconcile(t *testing.T) {
	db, ctx := openMigrated(t)

	chf := "CHF"
	usd := "USD"

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			// Source "ubs" — two CHF accounts.
			{SilverSourceID: "ubs", AccountExternalID: "CASH1",
				AccountKind: canonical.AccountKindCash, BaseCurrency: &chf,
				FirstSeenAt: 1000, LastSeenAt: 1000},
			{SilverSourceID: "ubs", AccountExternalID: "SAFE1",
				AccountKind: canonical.AccountKindSafekeeping, BaseCurrency: &chf,
				FirstSeenAt: 1000, LastSeenAt: 1000},
			// Source "schwab" — one USD account.
			{SilverSourceID: "schwab", AccountExternalID: "BROK1",
				AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &usd,
				FirstSeenAt: 1000, LastSeenAt: 1000},
		})
	})

	chf1000 := canonical.NewDecimalFromInt(1000)
	chf500 := canonical.NewDecimalFromInt(500)
	usd200 := canonical.NewDecimalFromInt(200)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "ubs", SnapshotAt: 1000, AccountExternalID: "SAFE1",
				PositionKey: "X", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
				Currency: "CHF", MarketValue: &chf1000},
			{SilverSourceID: "schwab", SnapshotAt: 1000, AccountExternalID: "BROK1",
				PositionKey: "Y", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
				Currency: "USD", MarketValue: &usd200},
		}); err != nil {
			return err
		}
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{{
			SilverSourceID: "ubs", SnapshotAt: 1000, AccountExternalID: "CASH1",
			Currency: "CHF", BalanceKind: canonical.BalanceKindClosing, Amount: chf500,
		}})
	})

	rows, err := SourcesAsOf(ctx, db, 2000, "CHF")
	if err != nil {
		t.Fatalf("SourcesAsOf: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per source)", len(rows))
	}

	bySrc := map[string]SourceRow{}
	for _, r := range rows {
		bySrc[r.SilverSourceID] = r
	}

	// ubs: positions=1000 CHF, cash=500 CHF, total=1500 CHF; base CHF.
	ubs := bySrc["ubs"]
	if ubs.BaseCurrency == nil || *ubs.BaseCurrency != "CHF" {
		t.Errorf("ubs base_currency = %v, want CHF", ubs.BaseCurrency)
	}
	if v := ubs.PositionsValueBase; v == nil || *v != "1000" {
		t.Errorf("ubs positions_value = %v, want 1000", v)
	}
	if v := ubs.CashBalanceBase; v == nil || *v != "500" {
		t.Errorf("ubs cash_balance = %v, want 500", v)
	}
	if v := ubs.TotalValueBase; v == nil || *v != "1500" {
		t.Errorf("ubs total_value = %v, want 1500", v)
	}
	if ubs.SnapshotAt != 1000 {
		t.Errorf("ubs snapshot_at = %d, want 1000", ubs.SnapshotAt)
	}

	// schwab: single USD account, $200 position; base USD.
	schwab := bySrc["schwab"]
	if schwab.BaseCurrency == nil || *schwab.BaseCurrency != "USD" {
		t.Errorf("schwab base_currency = %v, want USD", schwab.BaseCurrency)
	}
	if v := schwab.TotalValueBase; v == nil || *v != "200" {
		t.Errorf("schwab total_value = %v, want 200", v)
	}

	// Reconciliation in USD (FX CHF→USD=0.80): sum over sources must
	// equal the sum over accounts.
	seedFX(t, db, 1000, "CHF", "USD", "0.80")
	sRows, _ := SourcesAsOf(ctx, db, 2000, "USD")
	aRows, _ := AccountsAsOf(ctx, db, 2000, "USD")

	sum := func(getter func(idx int) *string, n int) string {
		var total canonical.Decimal
		for i := 0; i < n; i++ {
			s := getter(i)
			if s == nil {
				continue
			}
			d, _ := canonical.NewDecimalFromString(*s)
			total = total.Add(d)
		}
		return total.String()
	}
	sSum := sum(func(i int) *string { return sRows[i].TotalValueOutCcy }, len(sRows))
	aSum := sum(func(i int) *string { return aRows[i].TotalValueOutCcy }, len(aRows))
	if sSum != aSum {
		t.Errorf("sum(sources.total_USD)=%s != sum(accounts.total_USD)=%s", sSum, aSum)
	}
}

// TestSourcesAsOfMixedBaseNull confirms a source whose accounts
// disagree on base currency reports a NULL base trio (the agree-or-
// NULL rollup), while the output-currency trio is still computed.
func TestSourcesAsOfMixedBaseNull(t *testing.T) {
	db, ctx := openMigrated(t)

	chf := "CHF"
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "mixed", AccountExternalID: "A_CHF",
				AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &chf,
				FirstSeenAt: 1000, LastSeenAt: 1000},
			{SilverSourceID: "mixed", AccountExternalID: "A_USD",
				AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &usd,
				FirstSeenAt: 1000, LastSeenAt: 1000},
		})
	})

	chf1000 := canonical.NewDecimalFromInt(1000)
	usd1000 := canonical.NewDecimalFromInt(1000)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "mixed", SnapshotAt: 1000, AccountExternalID: "A_CHF",
				PositionKey: "X", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "CHF", MarketValue: &chf1000},
			{SilverSourceID: "mixed", SnapshotAt: 1000, AccountExternalID: "A_USD",
				PositionKey: "Y", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: &usd1000},
		})
	})
	// Seed 1 USD = 0.80 CHF so the USD output trio resolves for both
	// lines (CHF→USD multiplies by the 1/0.80 = 1.25 reciprocal).
	seedFX(t, db, 1000, "CHF", "USD", "0.80")

	rows, err := SourcesAsOf(ctx, db, 2000, "USD")
	if err != nil {
		t.Fatalf("SourcesAsOf: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	// Accounts disagree on base → no rolled base currency, NULL base trio.
	if r.BaseCurrency != nil {
		t.Errorf("base_currency = %v, want nil (mixed)", r.BaseCurrency)
	}
	if r.TotalValueBase != nil {
		t.Errorf("total_value_base = %v, want nil (mixed base)", r.TotalValueBase)
	}
	// Output currency still totals: 1000 CHF→1250 USD + 1000 USD = 2250 USD.
	if v := r.TotalValueOutCcy; v == nil || *v != "2250" {
		t.Errorf("total_value_USD = %v, want 2250", derefOr(v))
	}
}

// TestSourcesAsOfEmpty confirms an empty gold DB yields no rows
// (no accounts ⇒ no source buckets) rather than an error.
func TestSourcesAsOfEmpty(t *testing.T) {
	db, ctx := openMigrated(t)
	rows, err := SourcesAsOf(ctx, db, 3000, "USD")
	if err != nil {
		t.Fatalf("SourcesAsOf: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestSourcesHistoryCarryForward exercises the daily-series macro
// report_sources_history_multi. It has no Go scan path, so this asserts
// it via raw SQL: two snapshots a few days apart should produce a daily
// spine with the value carried forward on the gap day, and history@today
// should equal the latest non-history total.
func TestSourcesHistoryCarryForward(t *testing.T) {
	db, ctx := openMigrated(t)
	chf := "CHF"

	// Snapshots near "today" so the spine (first snapshot → today) stays
	// small; the macro's upper bound is DB now().
	today := time.Now().Unix() / 86400
	day1 := (today - 4) * 86400
	gap := (today - 3) * 86400 // a day with no new snapshot
	day2 := (today - 2) * 86400

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "ubs", AccountExternalID: "A1",
			AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &chf,
			FirstSeenAt: day1, LastSeenAt: day2,
		}})
	})

	v1000 := canonical.NewDecimalFromInt(1000)
	v1500 := canonical.NewDecimalFromInt(1500)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "ubs", SnapshotAt: day1, AccountExternalID: "A1",
				PositionKey: "X", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "CHF", MarketValue: &v1000},
			{SilverSourceID: "ubs", SnapshotAt: day2, AccountExternalID: "A1",
				PositionKey: "X", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "CHF", MarketValue: &v1500},
		})
	})

	rows, err := db.QueryContext(ctx,
		`SELECT as_of_day, CAST(total_value_chf AS VARCHAR) FROM report_sources_history_multi()
		   WHERE silver_source_id = 'ubs' ORDER BY as_of_day`)
	if err != nil {
		t.Fatalf("report_sources_history_multi: %v", err)
	}
	defer rows.Close()
	series := map[int64]string{}
	var maxDay int64
	for rows.Next() {
		var d int64
		var tot sql.NullString
		if err := rows.Scan(&d, &tot); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// The macro emits an untrimmed DECIMAL ("1000.0000"); canonicalise
		// the way the CLI's trimmedDecimalPtr would.
		val := tot.String
		if dec, err := canonical.NewDecimalFromString(val); err == nil {
			val = dec.String()
		}
		series[d] = val
		if d > maxDay {
			maxDay = d
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if got := series[day1]; got != "1000" {
		t.Errorf("history[day1] total = %q, want 1000", got)
	}
	if got := series[gap]; got != "1000" {
		t.Errorf("history[gap] total = %q, want 1000 (carried forward)", got)
	}
	if got := series[day2]; got != "1500" {
		t.Errorf("history[day2] total = %q, want 1500", got)
	}
	// The spine runs to today, carrying the last snapshot forward; that
	// latest value reconciles with the non-history report.
	if maxDay != today*86400 {
		t.Errorf("max as_of_day = %d, want %d (today)", maxDay, today*86400)
	}
	if got := series[maxDay]; got != "1500" {
		t.Errorf("history@today total = %q, want 1500", got)
	}
}

// A source's observed date is when a run last read its holdings, capped
// at the as-of date; NULL in silver_sources reads as 0.
func TestSourcesAsOfCarriesTheObservedDate(t *testing.T) {
	db, ctx := openMigrated(t)
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "test-src", AccountExternalID: "BROK1",
			AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &usd, FirstSeenAt: 1000, LastSeenAt: 1000}})
	})
	v := canonical.NewDecimalFromInt(200)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "test-src", SnapshotAt: 1000,
			AccountExternalID: "BROK1", PositionKey: "Y", AssetClass: canonical.AssetClassPublicEquity,
			Vehicle: canonical.VehicleStock, Currency: "USD", MarketValue: &v}})
	})
	read := func(asOf int64) SourceRow {
		t.Helper()
		rows, err := SourcesAsOf(ctx, db, asOf, "USD")
		if err != nil || len(rows) != 1 {
			t.Fatalf("SourcesAsOf: %v %v", rows, err)
		}
		return rows[0]
	}
	if r := read(5000); r.ObservedAt != 0 || r.SnapshotAt != 1000 {
		t.Errorf("unknown: %+v", r)
	}
	if _, err := db.ExecContext(ctx, `UPDATE silver_sources SET observed_snapshots_through = 4000 WHERE silver_source_id = 'test-src'`); err != nil {
		t.Fatal(err)
	}
	if r := read(5000); r.ObservedAt != 4000 || r.SnapshotAt != 1000 {
		t.Errorf("observed after the snapshot: %+v", r)
	}
	if r := read(3000); r.ObservedAt != 3000 {
		t.Errorf("capped at the as-of date: %+v", r)
	}
}

func TestMigration0122DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `UPDATE silver_sources SET observed_snapshots_through = 4000`); err != nil {
		t.Fatal(err)
	}
	rerunMigrationDDL(t, db, ctx, "0122_observed_through.sql")
	var got int64
	if err := db.QueryRowContext(ctx, `SELECT observed_snapshots_through FROM source_observed`).Scan(&got); err != nil || got != 4000 {
		t.Errorf("after replay: %d %v", got, err)
	}
}

// TestSourcesAsOfCarriesTheValuedDate pins the sources report's
// valued_at: the latest mark the source's latest snapshot rides on,
// which is the snapshot itself where no position states an older one.
func TestSourcesAsOfCarriesTheValuedDate(t *testing.T) {
	db, ctx := openMigrated(t)
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "test-src", AccountExternalID: "CUST",
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, FirstSeenAt: 1000, LastSeenAt: 1000}})
	})
	v := canonical.NewDecimalFromInt(200)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "CUST", PositionKey: "A",
				AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleSPV, Currency: "USD", MarketValue: &v},
			{SilverSourceID: "test-src", SnapshotAt: 3000, ValuedAt: 400, AccountExternalID: "CUST", PositionKey: "A",
				AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleSPV, Currency: "USD", MarketValue: &v},
			{SilverSourceID: "test-src", SnapshotAt: 3000, ValuedAt: 700, AccountExternalID: "CUST", PositionKey: "B",
				AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleSPV, Currency: "USD", MarketValue: &v},
		})
	})
	read := func(asOf int64) SourceRow {
		t.Helper()
		rows, err := SourcesAsOf(ctx, db, asOf, "USD")
		if err != nil || len(rows) != 1 {
			t.Fatalf("SourcesAsOf: %v %v", rows, err)
		}
		return rows[0]
	}
	if r := read(2000); r.SnapshotAt != 1000 || r.ValuedAt != 1000 {
		t.Errorf("no mark date stated: %+v, want valued at the snapshot", r)
	}
	if r := read(5000); r.SnapshotAt != 3000 || r.ValuedAt != 700 {
		t.Errorf("marks older than the snapshot: %+v, want the latest mark", r)
	}
}

func TestMigration0123DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "test-src", AccountExternalID: "CUST",
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, FirstSeenAt: 1000, LastSeenAt: 1000}})
	})
	v := canonical.NewDecimalFromInt(200)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "test-src", SnapshotAt: 1000,
			ValuedAt: 400, AccountExternalID: "CUST", PositionKey: "A", AssetClass: canonical.AssetClassPrivateEquity,
			Vehicle: canonical.VehicleSPV, Currency: "USD", MarketValue: &v}})
	})
	rerunMigrationDDL(t, db, ctx, "0123_valued_at.sql")
	if r, err := SourcesAsOf(ctx, db, 5000, "USD"); err != nil || len(r) != 1 || r[0].ValuedAt != 400 {
		t.Errorf("after replay: %+v %v", r, err)
	}
}
