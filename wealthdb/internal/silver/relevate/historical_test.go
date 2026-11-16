package relevate

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// seedHistorical builds two dumps and one quarterly-report date before
// them: two holdings keyed by ISIN, a cash and an accrued-interest row
// (one balance), and a securities row (not cash). Every value is
// invented.
func seedHistorical(t *testing.T) string {
	t.Helper()
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (5000, 2, '/x/1'), (6000, 2, '/x/2');
        INSERT INTO accounts(snapshot_at, account_external_id, currency_code, name, product_name, payload) VALUES
            (5000, 'ACC1', 'CHF', 'Strategy 50', 'PensFree', '{}'),
            (6000, 'ACC1', 'CHF', 'Strategy 50', 'PensFree', '{}');
        INSERT INTO historical_position_snapshots(snapshot_at, account_external_id, isin,
            security_name, asset_class, currency, units, market_value, source_sha256, payload) VALUES
            (1000, 'ACC1', 'CH0000000001', 'Equity Fund', 'Stocks', 'CHF', 10, 1500, 'h', '{}'),
            (1000, 'ACC1', 'CH0000000002', 'Bond Fund',   'Bonds',  'CHF', 20, 800,  'h', '{}');
        INSERT INTO historical_cash_balances(snapshot_at, account_external_id, currency,
            balance_kind, amount, source_sha256, payload) VALUES
            (1000, 'ACC1', 'CHF', 'cash',             100,  'h', '{}'),
            (1000, 'ACC1', 'CHF', 'accrued_interest', 2.5,  'h', '{}'),
            (1000, 'ACC1', 'CHF', 'securities',       2300, 'h', '{}');
    `)
	return path
}

// The quarterly-report rows widen the snapshot range Status reports, and
// an incremental window reaches back to the earliest of them, so a reload
// replaces their rows in place.
func TestHistoricalRowsWidenTheStatusAndTheWindow(t *testing.T) {
	conn := openAdapter(t, seedHistorical(t))
	ctx := context.Background()
	s, err := conn.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestSnapshotAt != 1000 || s.LatestSnapshotAt != 6000 {
		t.Errorf("snapshot range = [%d,%d], want [1000,6000]", s.OldestSnapshotAt, s.LatestSnapshotAt)
	}
	w, err := conn.ChangeWindow(ctx, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != 1000 || w.End < 6000 {
		t.Errorf("ChangeWindow(5000) = %+v, want changes from 1000 through 6000", w)
	}
}

// A quarterly-report date gets one batch: one instrument and one position
// per ISIN, and one cash balance that sums cash and accrued interest.
func TestHistoricalSnapshotsProjectTheReportDate(t *testing.T) {
	conn := openAdapter(t, seedHistorical(t))
	var batch *canonical.SnapshotBatch
	for _, b := range silvertest.Snapshots(t, conn, silvertest.WindowSince0(t, conn)) {
		if len(b.Positions) > 0 && b.Positions[0].SnapshotAt == 1000 {
			batch = &b
		}
	}
	if batch == nil {
		t.Fatal("no batch at the report date")
	}
	// The holdings pass and the cash pass each state the account once,
	// not once per row.
	if len(batch.Accounts) != 2 {
		t.Errorf("accounts = %d, want 2 (one per pass)", len(batch.Accounts))
	}
	for _, a := range batch.Accounts {
		if a.AccountExternalID != "ACC1" || a.TaxWrapper == nil || *a.TaxWrapper != canonical.TaxWrapperVestedBenefits {
			t.Errorf("account = %+v, want ACC1, vested benefits", a)
		}
	}
	if len(batch.Instruments) != 2 {
		t.Errorf("instruments = %d, want 2", len(batch.Instruments))
	}
	for _, in := range batch.Instruments {
		if in.ISIN == nil || *in.ISIN != in.InstrumentExternalID {
			t.Errorf("instrument %s ISIN = %v, want its key", in.InstrumentExternalID, in.ISIN)
		}
	}
	byISIN := map[string]canonical.PositionChange{}
	for _, p := range batch.Positions {
		byISIN[p.PositionKey] = p
	}
	eq := byISIN["CH0000000001"]
	if len(byISIN) != 2 || eq.InstrumentExternalID == nil || *eq.InstrumentExternalID != "CH0000000001" ||
		eq.Quantity == nil || eq.Quantity.String() != "10" ||
		eq.MarketValue == nil || eq.MarketValue.String() != "1500" ||
		eq.AssetClass != canonical.AssetClassPublicEquity {
		t.Errorf("positions = %+v, want two by ISIN, the equity fund 10 units worth 1500", batch.Positions)
	}
	if len(batch.CashBalances) != 1 || batch.CashBalances[0].Amount.String() != "102.5" ||
		batch.CashBalances[0].BalanceKind != canonical.BalanceKindCurrent {
		t.Errorf("cash balances = %+v, want one current balance of 102.5", batch.CashBalances)
	}
}

// A dump past the watermark brings a credit note dated before every
// snapshot: the window reaches back to it, so the load's windowed delete
// and re-insert cover it. A silver without the run stamp opens the
// window its other rows allow, from the earliest report date.
func TestChangeWindowCoversACreditNoteBeforeTheFirstSnapshot(t *testing.T) {
	for _, stamped := range []bool{true, false} {
		path, db := newFixtureSilver(t)
		silvertest.Exec(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (5000, 5, '/x/1'), (6000, 5, '/x/2');
        INSERT INTO historical_cash_balances(snapshot_at, account_external_id, currency,
            balance_kind, amount, source_sha256, payload) VALUES
            (1000, 'ACC1', 'CHF', 'cash', 100, 'h', '{}');
        INSERT INTO transactions(transaction_external_id, snapshot_at, occurred_at,
            account_external_id, kind, currency, gross_amount, net_amount, source,
            payload, run_snapshot_at) VALUES
            ('credit_note:1', 6000, 4000, 'ACC1', 'contribution', 'CHF', 1, 1,
             'credit_note_pdf', '{}', 5000),
            ('credit_note:2', 6000, 500, 'ACC1', 'contribution', 'CHF', 2, 2,
             'credit_note_pdf', '{}', 6000);
    `)
		want := int64(500)
		if !stamped {
			silvertest.Exec(t, db, `ALTER TABLE transactions DROP COLUMN run_snapshot_at`)
			want = 1000
		}
		w, err := openAdapter(t, path).ChangeWindow(context.Background(), 5000)
		if err != nil {
			t.Fatal(err)
		}
		if !w.HasChanges || w.Start != want || w.End != 6000 {
			t.Errorf("stamped=%v: window = %+v, want changes over [%d, 6000]",
				stamped, w, want)
		}
	}
}
