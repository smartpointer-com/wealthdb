package gold

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
)

// Every id and figure below is invented.
//
// The fixture is one source over 1970, chosen so the §4.3 identity of
// docs/GAINS.md can be checked by hand:
//
//	snapshot 1000 (the window's start):
//	  BRK1 AAA  10 @ 1000, cost 600     unrealized 400
//	  BRK1 BBB   5 @  500, cost 400     unrealized 100
//	  BRK1 BND   1 @ 1010 (10 accrued), cost 990   unrealized 10
//	  BRK2 CCC   1 @  200 CHF, no basis
//	snapshot 200000 (the window's end):
//	  BRK1 AAA  10 @ 1300, cost 600     unrealized 700
//	  BRK1 DDD   2 @  210, cost 200     unrealized 10  (bought)
//	  BRK1 BND   1 @ 1012 (12 accrued), cost 990   unrealized 10
//	  BRK2 CCC   1 @  220 CHF, no basis
//	realized, primary:
//	  BBB  sold 1970-01-02 for 550, cost 400   gain 150 (derived)
//	  ZZZ  1970-01-03, nothing stated          gain unknown
//	  AAA  no date, tax year 1970, stated 5    undated
//	  plus a non-primary copy of the BBB sale
//	sells: one on CASH1 (BRK1's portfolio), one of CCC on BRK2
//	also a journal of AAA units and a corporate action on BND, and a
//	deposit held as a cash position at the end and a mortgage being
//	paid down, which no cost basis describes: neither moves a gain nor
//	counts toward coverage
//
// Unrealized moves 510 → 720 (+210), realized is 155, so the gain is
// 365: AAA +300, BBB +50 over its start value, DDD +10, the bond 0
// (its accrued interest is income), and the undated AAA lot's 5.
const (
	gainsFrom = 1001
	gainsTo   = 31535999 // 1970-12-31 23:59:59
)

func seedGainsFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('test-src', 0, 'CHF', 'USD', 0.5),
            ('test-src', 0, 'USD', 'CHF', 2.0);
        INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
            VALUES ('test-src', 'P1', 'Growth', 1, 1);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name,
                              portfolio_external_id, tax_wrapper, first_seen_at, last_seen_at) VALUES
            ('test-src', 'BRK1',  'brokerage', 'Brokerage', 'P1', 'taxable_personal', 1, 1),
            ('test-src', 'CASH1', 'cash',      'Cash',      'P1', NULL,               1, 1),
            ('test-src', 'BRK2',  'brokerage', 'Other',     NULL, NULL,               1, 1),
            ('test-src', 'DEP1',  'cash',      'Deposit',   NULL, NULL,               1, 1),
            ('test-src', 'LOAN1', 'mortgage',  'Home loan', NULL, NULL,               1, 1);
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, name,
                                 currency, first_seen_at, last_seen_at) VALUES
            ('test-src', 'AAA', 'public_equity', 'AAA', 'Alpha',  'USD', 1, 1),
            ('test-src', 'BBB', 'public_equity', 'BBB', 'Beta',   'USD', 1, 1),
            ('test-src', 'DDD', 'public_equity', 'DDD', 'Delta',  'USD', 1, 1),
            ('test-src', 'BND', 'fixed_income',  'BND', 'Bond',   'USD', 1, 1),
            ('test-src', 'CCC', 'public_equity', 'CCC', 'Gamma',  'CHF', 1, 1);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, vehicle, currency, quantity,
                               market_value, book_value, accrued_interest,
                               basis_origin, basis_method, basis_fees) VALUES
            ('test-src', 1000,   'BRK1', 'AAA', 'AAA', 'public_equity', 'stock', 'USD', 10, 1000, 600,  NULL, 'stated', 'lots', 'included'),
            ('test-src', 1000,   'BRK1', 'BBB', 'BBB', 'public_equity', 'stock', 'USD', 5,  500,  400,  NULL, 'stated', 'lots', 'included'),
            ('test-src', 1000,   'BRK1', 'BND', 'BND', 'fixed_income',  'bond',  'USD', 1,  1010, 990,  10,   'stated', 'lots', 'included'),
            ('test-src', 1000,   'BRK2', 'CCC', 'CCC', 'public_equity', 'stock', 'CHF', 1,  200,  NULL, NULL, NULL, NULL, NULL),
            ('test-src', 200000, 'BRK1', 'AAA', 'AAA', 'public_equity', 'stock', 'USD', 10, 1300, 600,  NULL, 'stated', 'lots', 'included'),
            ('test-src', 200000, 'BRK1', 'DDD', 'DDD', 'public_equity', 'stock', 'USD', 2,  210,  200,  NULL, 'stated', 'lots', 'included'),
            ('test-src', 200000, 'BRK1', 'BND', 'BND', 'fixed_income',  'bond',  'USD', 1,  1012, 990,  12,   'stated', 'lots', 'included'),
            ('test-src', 200000, 'BRK2', 'CCC', 'CCC', 'public_equity', 'stock', 'CHF', 1,  220,  NULL, NULL, NULL, NULL, NULL),
            ('test-src', 200000, 'DEP1', 'DEP', NULL,  'cash',          'demand_deposit', 'USD', NULL, 5000, NULL, NULL, NULL, NULL, NULL),
            ('test-src', 1000,   'LOAN1', 'M',  NULL,  'real_estate',   'mortgage', 'USD', NULL, -5500, -6000, NULL, 'stated', 'acquisition_value', 'none'),
            ('test-src', 200000, 'LOAN1', 'M',  NULL,  'real_estate',   'mortgage', 'USD', NULL, -5000, -6000, NULL, 'stated', 'acquisition_value', 'none');
        INSERT INTO position_lots (silver_source_id, snapshot_at, account_external_id, position_key, lot_key,
                                   instrument_external_id, currency, quantity, book_value, market_value,
                                   acquisition_date, term, basis_origin) VALUES
            ('test-src', 200000, 'BRK1', 'AAA', 'L1', 'AAA', 'USD', 4, 200, NULL, DATE '1969-01-01', 'long',  'stated'),
            ('test-src', 200000, 'BRK1', 'AAA', 'L2', 'AAA', 'USD', 6, 400, 780,  DATE '1969-12-01', 'short', 'stated');
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
                                   instrument_external_id, description, document_kind, tax_year,
                                   acquired_various, disposal_date, currency, quantity, proceeds,
                                   book_value, realized_gain_loss, term, basis_origin, basis_method,
                                   basis_fees, is_primary) VALUES
            ('test-src', 'R-BBB',  'BRK1', 'BBB', 'BETA',  'form_1099b',       1970, FALSE, DATE '1970-01-02', 'USD', 5, 550, 400, NULL, 'short', 'stated', 'lots', 'included', TRUE),
            ('test-src', 'R-BBB2', 'BRK1', 'BBB', 'BETA',  'year_end_summary', 1970, FALSE, DATE '1970-01-02', 'USD', 5, 550, 400, 150,  'short', 'stated', 'lots', 'included', FALSE),
            ('test-src', 'R-ZZZ',  'BRK1', 'ZZZ', 'ZETA',  'statement',        1970, FALSE, DATE '1970-01-03', 'USD', 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, TRUE),
            ('test-src', 'R-AAA',  'BRK1', 'AAA', 'ALPHA', 'statement',        1970, FALSE, NULL,              'USD', 1, NULL, NULL, 5,    NULL, NULL, NULL, NULL, TRUE);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'S-CASH', 90000, 'CASH1', 'BBB', 'sell', 'USD', 550),
            ('test-src', 'S-CCC',  90000, 'BRK2',  'CCC', 'sell', 'CHF', 10);
        -- A journal moving AAA units in from elsewhere, and a corporate
        -- action on the bond: neither moves a lot, both are flagged.
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id, kind, currency,
                                  net_amount, quantity) VALUES
            ('test-src', 'J-AAA', 95000, 'BRK1', 'AAA', 'journal',          'USD', 0, 1),
            ('test-src', 'C-BND', 95000, 'BRK1', 'BND', 'corporate_action', 'USD', 0, NULL);
    `); err != nil {
		t.Fatalf("seed the gains fixture: %v", err)
	}
}

func openGainsFixture(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, ctx := openMigrated(t)
	seedGainsFixture(t, db, ctx)
	return db, ctx
}

// num reads a decimal string field as a float, NaN for nil, so a
// missing figure never compares equal to an expected one.
func num(t *testing.T, s *string) float64 {
	t.Helper()
	if s == nil {
		return math.NaN()
	}
	return decimalOf(t, s)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestGainsSummaryHoldsTheIdentity(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rows, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", "total", GainsAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.PeriodStart != nil {
		t.Errorf("total bucket has a start: %v", *r.PeriodStart)
	}
	for _, c := range []struct {
		name string
		got  *string
		want float64
	}{
		{"unrealized_start", r.UnrealizedStart, 510},
		{"unrealized_end", r.UnrealizedEnd, 720},
		{"unrealized_change", r.UnrealizedChange, 210},
		{"realized", r.Realized, 155},
		{"realized_short", r.RealizedShort, 150},
		{"realized_other", r.RealizedOther, 5},
		{"gain", r.Gain, 365},
		{"proceeds", r.Proceeds, 550},
	} {
		if g := num(t, c.got); !near(g, c.want) {
			t.Errorf("%s = %v, want %v", c.name, g, c.want)
		}
	}
	if r.RealizedLong != nil {
		t.Errorf("realized_long = %v, want blank (no long lot)", *r.RealizedLong)
	}
	if r.RealizedLots != 3 || r.Sells != 2 || r.Positions != 6 || r.PositionsWithoutBasis != 1 {
		t.Errorf("counts = lots %d, sells %d, positions %d, without basis %d",
			r.RealizedLots, r.Sells, r.Positions, r.PositionsWithoutBasis)
	}
	// CCC is 220 CHF at 2 USD a franc; everything else carries a basis.
	wantCov := (1300.0 + 210 + 1012) / (1300 + 210 + 1012 + 440)
	if r.BasisCoverage == nil || !near(*r.BasisCoverage, wantCov) {
		t.Errorf("basis_coverage = %v, want %v", r.BasisCoverage, wantCov)
	}
	if want := "sells_without_documents=1;lots_without_gain=1;undated_lots=1;in_kind_moves=1;corporate_actions=1"; r.Quality != want {
		t.Errorf("quality = %q, want %q", r.Quality, want)
	}
}

// Every grain is the same sums grouped differently, so each adds up to
// the summary, and the monthly buckets add up to the total.
func TestGainsGrainsAndBucketsReconcile(t *testing.T) {
	db, ctx := openGainsFixture(t)
	total, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", "total", GainsAll)
	if err != nil {
		t.Fatal(err)
	}
	sum := func(rows []GainsBucketRow) (realized, change, gain float64, sells int64) {
		for _, r := range rows {
			if r.Realized != nil {
				realized += num(t, r.Realized)
			}
			if r.UnrealizedChange != nil {
				change += num(t, r.UnrealizedChange)
			}
			if r.Gain != nil {
				gain += num(t, r.Gain)
			}
			sells += r.Sells
		}
		return
	}
	wr, wc, wg, ws := sum(total)
	for _, grain := range []GainsGrain{GainsSources, GainsPortfolios, GainsAccounts} {
		rows, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", "total", grain)
		if err != nil {
			t.Fatal(err)
		}
		r, c, g, s := sum(rows)
		if !near(r, wr) || !near(c, wc) || !near(g, wg) || s != ws {
			t.Errorf("%s: realized %v change %v gain %v sells %d; summary %v %v %v %d", grain, r, c, g, s, wr, wc, wg, ws)
		}
	}
	monthly, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", "month", GainsAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(monthly) != 12 {
		t.Errorf("got %d monthly buckets, want 12", len(monthly))
	}
	if r, c, g, s := sum(monthly); !near(r, wr) || !near(c, wc) || !near(g, wg) || s != ws {
		t.Errorf("monthly: realized %v change %v gain %v sells %d", r, c, g, s)
	}
}

func TestGainsPortfolioAndAccountRows(t *testing.T) {
	db, ctx := openGainsFixture(t)
	ports, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", "total", GainsPortfolios)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || *ports[0].PortfolioExternalID != "" || *ports[1].PortfolioExternalID != "P1" {
		t.Fatalf("portfolio rows = %+v", ports)
	}
	if ports[1].PortfolioDisplayName == nil || *ports[1].PortfolioDisplayName != "Growth" {
		t.Errorf("P1 display name = %v", ports[1].PortfolioDisplayName)
	}
	// The CASH1 sale is documented by BRK1's lots: same portfolio.
	if strings.Contains(ports[1].Quality, "sells_without_documents") {
		t.Errorf("P1 quality = %q, the portfolio's sale is documented", ports[1].Quality)
	}
	if !strings.Contains(ports[0].Quality, "sells_without_documents=1") {
		t.Errorf("ungrouped quality = %q, want the BRK2 sale flagged", ports[0].Quality)
	}

	accts, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", "total", GainsAccounts)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]GainsBucketRow{}
	for _, r := range accts {
		got[*r.AccountExternalID] = r
	}
	if len(got) != 5 {
		t.Fatalf("account rows = %d, want BRK1, BRK2, CASH1, DEP1 and LOAN1", len(got))
	}
	if r := got["LOAN1"]; r.UnrealizedChange != nil || r.Gain != nil {
		t.Errorf("LOAN1 = %+v, want no gain from paying a mortgage down", r)
	}
	if r := got["DEP1"]; r.Positions != 1 || r.PositionsWithoutBasis != 0 || r.BasisCoverage != nil || r.Gain != nil {
		t.Errorf("DEP1 = %+v, want a held cash line outside every basis figure", r)
	}
	if r := got["BRK1"]; r.TaxWrapper == nil || *r.TaxWrapper != "taxable_personal" || !near(num(t, r.Gain), 365) {
		t.Errorf("BRK1 = %+v", r)
	}
	if r := got["CASH1"]; r.Sells != 1 || r.Positions != 0 || r.Quality != "" {
		t.Errorf("CASH1 = %+v", r)
	}
}

func TestGainsPositionsAttachLotsToTheirHolding(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rows, err := GainsPositions(ctx, db, gainsFrom, gainsTo, "USD")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]GainsPositionRow{}
	var gain float64
	for _, r := range rows {
		k := r.AccountExternalID + "/"
		if r.PositionKey != nil {
			k += *r.PositionKey
		} else {
			k += "lot:" + *r.LotKey
		}
		byKey[k] = r
		if r.Gain != nil {
			gain += num(t, r.Gain)
		}
	}
	if !near(gain, 365) {
		t.Errorf("Σ gain = %v, want the summary's 365", gain)
	}
	if r := byKey["BRK1/BBB"]; !near(num(t, r.Realized), 150) || !near(num(t, r.UnrealizedChange), -100) ||
		!near(num(t, r.Gain), 50) || r.QuantityEnd != nil || *r.QuantityStart != "5" {
		t.Errorf("BBB = %+v", r)
	}
	if r := byKey["BRK1/AAA"]; !near(num(t, r.Gain), 305) || r.OpenLots != 2 || *r.BasisStamp != "stated/lots/included" ||
		r.Quality != "undated_lots=1;in_kind_moves=1" {
		t.Errorf("AAA = %+v", r)
	}
	// The bond's accrued interest is income, not gain.
	if r := byKey["BRK1/BND"]; !near(num(t, r.UnrealizedGain), 10) || !near(num(t, r.Gain), 0) || r.Quality != "corporate_actions=1" {
		t.Errorf("BND = %+v", r)
	}
	if r := byKey["BRK1/lot:ZZZ"]; r.Quality != "lots_without_gain=1;unmatched_lots=1" || r.Gain != nil {
		t.Errorf("ZZZ = %+v", r)
	}
	if r := byKey["BRK2/CCC"]; r.UnrealizedGain != nil || r.Quality != "sells_without_documents=1" {
		t.Errorf("CCC = %+v", r)
	}
}

func TestRealizedLotsPrimaryAllAndOrder(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rows, err := RealizedLotsBetween(ctx, db, gainsFrom, gainsTo, "USD", false, SortAscending)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.RealizedLotExternalID)
	}
	if strings.Join(ids, ",") != "R-BBB,R-ZZZ,R-AAA" {
		t.Errorf("primary order = %v", ids)
	}
	if r := rows[0]; r.GainOrigin != "derived" || !near(num(t, r.Gain), 150) || r.Undated {
		t.Errorf("BBB = %+v", r)
	}
	if r := rows[1]; r.GainOrigin != "unknown" || r.Gain != nil {
		t.Errorf("ZZZ = %+v", r)
	}
	if r := rows[2]; !r.Undated || r.EffectiveDate != "1970-12-31" || r.GainOrigin != "stated" {
		t.Errorf("AAA = %+v", r)
	}
	all, err := RealizedLotsBetween(ctx, db, gainsFrom, gainsTo, "USD", true, SortDescending)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[0].RealizedLotExternalID != "R-AAA" {
		t.Errorf("all, newest first = %d rows, first %s", len(all), all[0].RealizedLotExternalID)
	}
}

func TestOpenLotsValueAndUnrealized(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rows, err := OpenLotsAsOf(ctx, db, 250000, "CHF")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d lots, want 2", len(rows))
	}
	// L1 has no stated value: 4 of the position's 10 units at 1300.
	if r := rows[0]; *r.ValueOrigin != "pro_rata" || !near(num(t, r.MarketValue), 520) ||
		!near(num(t, r.UnrealizedGain), 320) || !near(num(t, r.UnrealizedOutCcy), 160) || *r.HeldDays != 367 {
		t.Errorf("L1 = %+v", r)
	}
	if r := rows[1]; *r.ValueOrigin != "stated" || !near(num(t, r.UnrealizedGain), 380) {
		t.Errorf("L2 = %+v", r)
	}
}

func TestGainsCoverageVerdicts(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rows, err := GainsCoverage(ctx, db, gainsFrom, gainsTo, "USD")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.AccountExternalID] = r.Verdict
	}
	want := map[string]string{"BRK1": "ok", "BRK2": "no_basis", "CASH1": "ok"}
	if _, ok := got["DEP1"]; ok {
		t.Error("the deposit account is listed; a cash position has no cost basis to cover")
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s verdict = %q, want %q", k, got[k], v)
		}
	}
}

func TestPositionsCarryTheCostBasis(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rows, err := PositionsAsOf(ctx, db, 250000, "CHF")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r.PositionKey {
		case "AAA":
			if !near(num(t, r.UnrealizedGain), 700) || !near(num(t, r.UnrealizedOutCcy), 350) ||
				!near(num(t, r.BookValueOutCcy), 300) || !near(*r.UnrealizedRatio, 700.0/600) ||
				*r.BasisStamp != "stated/lots/included" {
				t.Errorf("AAA = %+v", r)
			}
		case "BND":
			if !near(num(t, r.CleanValue), 1000) || !near(num(t, r.UnrealizedGain), 10) {
				t.Errorf("BND = %+v", r)
			}
		case "CCC":
			if r.BookValue != nil || r.UnrealizedGain != nil || r.BasisStamp != nil || !near(num(t, r.CleanValue), 220) {
				t.Errorf("CCC = %+v", r)
			}
		}
	}
}

func TestMigration0116DDLIsRerunnable(t *testing.T) {
	db, ctx := openGainsFixture(t)
	rerunMigrationDDL(t, db, ctx, "0116_gains_reports.sql")
	rows, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", "total", GainsAll)
	if err != nil || len(rows) != 1 || !near(num(t, rows[0].Gain), 365) {
		t.Errorf("after replay: rows %v err %v", rows, err)
	}
}
