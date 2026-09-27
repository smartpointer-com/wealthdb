package ubs

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// newWebFixture builds an in-memory ubs-web silver with just the
// historical tables snapshotsHistorical reads. Same "sqlite"
// driver the PSN fixture uses.
func newWebFixture(t *testing.T) *webReader {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/ubs-web.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	const schema = `
CREATE TABLE historical_position_snapshots (
    as_of_date INTEGER, portfolio_external_id TEXT, account_external_id TEXT,
    instrument_isin TEXT, currency_iso TEXT, units REAL, market_value REAL,
    market_value_currency TEXT, cost_price REAL, market_price REAL,
    accrued_interest REAL, exchange_rate_to_base REAL, description TEXT,
    sector TEXT, source_doc_token TEXT, payload TEXT);
CREATE TABLE historical_cash_balances (
    period_end INTEGER, account_external_id TEXT, currency_iso TEXT,
    period_start INTEGER, opening_balance REAL, closing_balance REAL,
    total_debits REAL, total_credits REAL, source_doc_token TEXT, payload TEXT);
CREATE TABLE historical_mortgages (
    as_of_date INTEGER, account_external_id TEXT, currency_iso TEXT,
    outstanding_balance REAL, product_name TEXT, rate_type TEXT,
    collateral_description TEXT, source_doc_token TEXT, payload TEXT);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &webReader{db: db}
}

// TestHistoricalMortgageAnchoring locks in the fix for the
// "mortgage-only snapshot hijacks the today/portfolio view" bug:
// a historical mortgage row is projected as a Position ONLY when a
// real portfolio snapshot already exists at its as_of_date. A
// future-dated Maturity Notice (UBS issues next quarter's notice
// early) has no securities/cash behind it and must NOT spawn a
// position at that date.
func TestHistoricalMortgageAnchoring(t *testing.T) {
	r := newWebFixture(t)
	ctx := context.Background()
	// Anchored quarter-end: one security + one mortgage at t=1000.
	// Unanchored future date: mortgage only at t=9000.
	if _, err := r.db.ExecContext(ctx, `
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id,
             instrument_isin, currency_iso, units, market_value,
             market_value_currency, source_doc_token, payload)
        VALUES (1000, '0999AAAAAAAA01', '', 'CH0000000001', 'CHF',
                10, 1500, 'CHF', 'tok', '{}');
        INSERT INTO historical_mortgages
            (as_of_date, account_external_id, currency_iso,
             outstanding_balance, product_name, rate_type,
             collateral_description, source_doc_token, payload)
        VALUES
            (1000, '0999 AAAAAAAA.MMM 0001', 'CHF', -1234.56,
             'UBS Fixed-Rate Mortgage', 'fixed', 'EXAMPLE', 'tok', '{}'),
            (9000, '0999 AAAAAAAA.MMM 0001', 'CHF', -1234.56,
             'UBS Fixed-Rate Mortgage', 'fixed', 'EXAMPLE', 'tok', '{}');
    `); err != nil {
		t.Fatal(err)
	}

	w := canonical.Window{Start: 0, End: 100000, HasChanges: true}
	stream, err := r.snapshotsHistorical(ctx, w, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	// Collect every position keyed by snapshot_at.
	posBySnap := map[int64][]canonical.PositionChange{}
	var accounts []canonical.AccountChange
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			posBySnap[p.SnapshotAt] = append(posBySnap[p.SnapshotAt], p)
		}
		accounts = append(accounts, batch.Accounts...)
		if !more {
			break
		}
	}

	// A mortgage is a liability of the same relationship as the cash
	// and custody accounts, so it carries the same wrapper rather than
	// reaching gold with the column unset.
	assertEveryAccountOfKind(t, accounts, canonical.AccountKindMortgage)

	// t=1000 (anchored): security + mortgage both present.
	got1000 := posBySnap[1000]
	if len(got1000) != 2 {
		t.Fatalf("t=1000 positions = %d, want 2 (security + mortgage)", len(got1000))
	}
	var sawMortgage bool
	for _, p := range got1000 {
		if p.Vehicle == canonical.VehicleMortgage {
			sawMortgage = true
		}
	}
	if !sawMortgage {
		t.Errorf("t=1000 missing the mortgage position")
	}

	// t=9000 (unanchored future): NO position at all — the
	// mortgage-only date must not spawn a snapshot.
	if got := posBySnap[9000]; len(got) != 0 {
		t.Errorf("t=9000 positions = %d, want 0 (unanchored mortgage must not "+
			"create a snapshot)", len(got))
	}
}

// TestHistoricalSecuritiesSafekeepingRepointing locks in the
// web→PSN account-continuity fix: a historical security whose
// portfolio has a 1:1 PSN safekeeping account attaches to that
// real account (kind=safekeeping, no display name so PSN's wins),
// while one whose portfolio has no mapping falls back to the
// synthetic overlay account.
func TestHistoricalSecuritiesSafekeepingRepointing(t *testing.T) {
	r := newWebFixture(t)
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx, `
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id,
             instrument_isin, currency_iso, units, market_value,
             market_value_currency, source_doc_token, payload)
        VALUES
            (1000, '0999AAAAAAAA02', '', 'CH0000000001', 'CHF',
             10, 1500, 'CHF', 'tok', '{}'),
            (1000, '0999AAAAAAAA09', '', 'CH0000000002', 'CHF',
             20, 2500, 'CHF', 'tok', '{}');
    `); err != nil {
		t.Fatal(err)
	}

	// Only portfolio …02 has a 1:1 safekeeping mapping; …09 does not.
	mapping := map[string]string{"0999AAAAAAAA02": "0999 AAAAAAAA.MMM SK1"}

	w := canonical.Window{Start: 0, End: 100000, HasChanges: true}
	stream, err := r.snapshotsHistorical(ctx, w, mapping, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var positions []canonical.PositionChange
	var accounts []canonical.AccountChange
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		positions = append(positions, batch.Positions...)
		accounts = append(accounts, batch.Accounts...)
		if !more {
			break
		}
	}

	acctByISIN := map[string]string{}
	for _, p := range positions {
		acctByISIN[p.PositionKey] = p.AccountExternalID
	}
	if got := acctByISIN["CH0000000001"]; got != "0999 AAAAAAAA.MMM SK1" {
		t.Errorf("mapped security account = %q, want the PSN safekeeping ID", got)
	}
	if got := acctByISIN["CH0000000002"]; got != "0999AAAAAAAA09:overlay" {
		t.Errorf("unmapped security account = %q, want overlay fallback", got)
	}

	// The mapped account must be emitted as kind=safekeeping with no
	// display name (so the PSN-era name wins); the unmapped one as
	// overlay with the historical placeholder name.
	kindByID := map[string]canonical.AccountKind{}
	nameByID := map[string]*string{}
	for _, a := range accounts {
		kindByID[a.AccountExternalID] = a.AccountKind
		nameByID[a.AccountExternalID] = a.DisplayName
	}
	if kindByID["0999 AAAAAAAA.MMM SK1"] != canonical.AccountKindSafekeeping {
		t.Errorf("mapped account kind = %q, want safekeeping",
			kindByID["0999 AAAAAAAA.MMM SK1"])
	}
	if nameByID["0999 AAAAAAAA.MMM SK1"] != nil {
		t.Errorf("mapped account should have nil DisplayName (PSN name wins), got %q",
			*nameByID["0999 AAAAAAAA.MMM SK1"])
	}
	if kindByID["0999AAAAAAAA09:overlay"] != canonical.AccountKindOverlay {
		t.Errorf("unmapped account kind = %q, want overlay",
			kindByID["0999AAAAAAAA09:overlay"])
	}
	// The synthetic overlay carries the relationship's wrapper; the
	// real safekeeping account beside it deliberately does NOT get one
	// from here, because its wrapper is the AcctTpCd tables' answer.
	assertEveryAccountOfKind(t, accounts, canonical.AccountKindOverlay)
	for _, a := range accounts {
		if a.AccountKind == canonical.AccountKindSafekeeping && a.TaxWrapper != nil {
			t.Errorf("the PDF era supplied a wrapper for a safekeeping account, "+
				"overruling the product-code tables: %v", *a.TaxWrapper)
		}
	}
}

// TestHistoricalSyntheticISIN locks in the overview-derived
// precious-metals path: a row whose instrument_isin is a synthetic,
// non-ISIN-shaped key ("PM-<portfolio>") still flows to gold as a
// Position + Instrument, but must NOT claim a canonical ISIN — the
// InstrumentChange.ISIN stays nil while a real-ISIN security keeps
// its ISIN. Both still produce a position with the correct value.
func TestHistoricalSyntheticISIN(t *testing.T) {
	r := newWebFixture(t)
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx, `
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id,
             instrument_isin, currency_iso, units, market_value,
             market_value_currency, source_doc_token, payload)
        VALUES
            (1000, '0999AAAAAAAA02', '', 'CH0000000001', 'CHF',
             10, 1500, 'CHF', 'tok', '{}'),
            (1000, '0999AAAAAAAA01', '', 'PM-0999AAAAAAAA01', 'USD',
             NULL, 1000000, 'USD', 'tok', '{}');
    `); err != nil {
		t.Fatal(err)
	}

	w := canonical.Window{Start: 0, End: 100000, HasChanges: true}
	stream, err := r.snapshotsHistorical(ctx, w, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var positions []canonical.PositionChange
	var instruments []canonical.InstrumentChange
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		positions = append(positions, batch.Positions...)
		instruments = append(instruments, batch.Instruments...)
		if !more {
			break
		}
	}

	// Both positions must reach gold, keyed by their (synthetic or
	// real) instrument key.
	mvByKey := map[string]*canonical.Decimal{}
	for _, p := range positions {
		mvByKey[p.PositionKey] = p.MarketValue
	}
	if _, ok := mvByKey["CH0000000001"]; !ok {
		t.Errorf("real-ISIN security missing from positions")
	}
	if mv, ok := mvByKey["PM-0999AAAAAAAA01"]; !ok || mv == nil {
		t.Errorf("synthetic precious-metals position missing or has nil value")
	}

	// The canonical ISIN must be set for the real security and nil
	// for the synthetic key.
	isinByID := map[string]*string{}
	for _, in := range instruments {
		isinByID[in.InstrumentExternalID] = in.ISIN
	}
	if got := isinByID["CH0000000001"]; got == nil || *got != "CH0000000001" {
		t.Errorf("real security ISIN = %v, want CH0000000001", got)
	}
	if got := isinByID["PM-0999AAAAAAAA01"]; got != nil {
		t.Errorf("synthetic key must not claim a canonical ISIN, got %q", *got)
	}
}

// TestHistoricalPreciousMetalsDedup locks in the year-end gold-bar
// dedup: when a portfolio carries the synthetic overview precious-
// metals row (PM-<portfolio>), the year-end "Gold bar(s)" detail line
// in that SAME portfolio+snapshot is suppressed so the metal is not
// double-counted. The suppression is portfolio-scoped (a real security
// in a portfolio with no overview row is untouched) and metal-scoped
// (a genuine non-metal security in the overlay portfolio is untouched).
func TestHistoricalPreciousMetalsDedup(t *testing.T) {
	r := newWebFixture(t)
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx, `
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id,
             instrument_isin, currency_iso, units, market_value,
             market_value_currency, description, source_doc_token, payload)
        VALUES
            -- overlay portfolio …01: overview metal row (kept)
            (1000, '0999AAAAAAAA01', '', 'PM-0999AAAAAAAA01', 'USD',
             NULL, 1000000, 'USD', 'Precious metals & commodities', 'tok',
             '{"kind":"overview_asset_class","asset_class":"precious_metals"}'),
            -- overlay portfolio …01: year-end gold-bar detail (SUPPRESSED)
            (1000, '0999AAAAAAAA01', '', 'CH0000000001', 'USD',
             1000, 50000, 'CHF', 'Gold bar(s) fine weight', 'tok',
             '{"headline":"1 000 Gold bar(s) fine weight USD ..."}'),
            -- overlay portfolio …01: a genuine non-metal security (kept)
            (1000, '0999AAAAAAAA01', '', 'CH0000000003', 'CHF',
             100, 50000, 'CHF', 'UBS Money Market Fund CHF', 'tok', '{}'),
            -- portfolio …02: a real equity, NO overview row here (kept)
            (1000, '0999AAAAAAAA02', '', 'CH0000000002', 'CHF',
             20, 2500, 'CHF', 'Some Equity', 'tok', '{}');
    `); err != nil {
		t.Fatal(err)
	}

	w := canonical.Window{Start: 0, End: 100000, HasChanges: true}
	stream, err := r.snapshotsHistorical(ctx, w, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	mvByKey := map[string]*canonical.Decimal{}
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			mvByKey[p.PositionKey] = p.MarketValue
		}
		if !more {
			break
		}
	}

	// The overview metal row survives; the gold-bar detail is dropped.
	if _, ok := mvByKey["PM-0999AAAAAAAA01"]; !ok {
		t.Errorf("overview precious-metals row missing — should be kept")
	}
	if _, ok := mvByKey["CH0000000001"]; ok {
		t.Errorf("year-end gold-bar detail present — should be suppressed " +
			"(it double-counts the overview metal)")
	}
	// Portfolio-scope guard: a real security in a portfolio with no
	// overview row is never suppressed.
	if _, ok := mvByKey["CH0000000002"]; !ok {
		t.Errorf("real security in a non-overlay portfolio was wrongly suppressed")
	}
	// Metal-scope guard: a non-metal security in the overlay portfolio
	// is never suppressed.
	if _, ok := mvByKey["CH0000000003"]; !ok {
		t.Errorf("non-metal security in the overlay portfolio was wrongly suppressed")
	}
}

// The historical span reaches every table's dates — a cash period's start
// as well as its end — and is empty both on a silver that predates the
// tables and on one whose tables hold nothing, so neither widens a window.
func TestHistoricalRangeSpansEveryTable(t *testing.T) {
	ctx := context.Background()
	r := newWebFixture(t)
	if lo, hi, err := r.historicalRange(ctx); err != nil || lo != -1 || hi != -1 {
		t.Fatalf("empty tables: (%d, %d, %v), want (-1, -1, nil)", lo, hi, err)
	}
	for _, q := range []string{
		`INSERT INTO historical_position_snapshots (as_of_date) VALUES (200), (300)`,
		`INSERT INTO historical_cash_balances (period_start, period_end) VALUES (100, 250)`,
		`INSERT INTO historical_mortgages (as_of_date) VALUES (400)`,
	} {
		if _, err := r.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if lo, hi, err := r.historicalRange(ctx); err != nil || lo != 100 || hi != 400 {
		t.Errorf("span = (%d, %d, %v), want (100, 400, nil)", lo, hi, err)
	}
	if _, err := r.db.Exec(`DROP TABLE historical_cash_balances`); err != nil {
		t.Fatal(err)
	}
	if lo, hi, err := r.historicalRange(ctx); err != nil || lo != -1 || hi != -1 {
		t.Errorf("pre-migration silver: (%d, %d, %v), want (-1, -1, nil)", lo, hi, err)
	}
}
