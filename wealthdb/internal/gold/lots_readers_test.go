package gold

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// Every id and figure below is invented. The readers over what a lot
// pass wrote: the missing-basis readings, the engine's flags, its lots.

// partialSeed: COIN bought at 10, then staked twice; the second staking
// day has no rate, so one coin has no cost. A sale of a seed shows a
// realized lot without a cost.
const partialSeed = `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('lot-src', 3*86400, 'USD', 'COIN', 40);
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, name, currency, first_seen_at, last_seen_at)
            VALUES ('lot-src', 'COIN', 'crypto', 'COIN', 'Coin', 'USD', 1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,  'A', 'COIN', 'buy',     'USD',  1, -10),
            ('lot-src', 'k1', 3*86400,  'A', 'COIN', 'staking', 'COIN', 2,   2),
            ('lot-src', 'k2', 30*86400, 'A', 'COIN', 'staking', 'COIN', 1,   1),
            ('lot-src', 's1', 40*86400, 'B', 'ZZZ',  'sell',    'USD', -5, 500);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 4*86400,  'A', 'COIN', 'COIN', 'crypto', 'USD', 3, 120),
            ('lot-src', 31*86400, 'A', 'COIN', 'COIN', 'crypto', 'USD', 4, 160),
            ('lot-src', 41*86400, 'A', 'COIN', 'COIN', 'crypto', 'USD', 4, 200);`

func TestReadersUnderBothReadingsOfAMissingBasis(t *testing.T) {
	db, ctx := lotFixture(t, partialSeed)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)

	for _, c := range []struct {
		missing           lots.MissingBasis
		book, unrealized  float64
		bookNil, missingQ bool
	}{
		{lots.MissingIgnore, 0, 0, true, true},
		{lots.MissingZero, 90, 110, false, true},
	} {
		rows, err := PositionsAsOf(ctx, db, 41*lotDay+86399, "USD", c.missing)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %v %v", c.missing, rows, err)
		}
		r := rows[0]
		if (r.BookValue == nil) != c.bookNil || (!c.bookNil && (!near(num(t, r.BookValue), c.book) || !near(num(t, r.UnrealizedGain), c.unrealized))) {
			t.Errorf("%s: position %+v", c.missing, r)
		}
		if r.QuantityWithoutBasis == nil || *r.QuantityWithoutBasis != "1" {
			t.Errorf("%s: quantity without basis %v", c.missing, r.QuantityWithoutBasis)
		}
	}

	// The sale of ZZZ seeds: its realized lot has proceeds and no cost.
	for _, c := range []struct {
		missing lots.MissingBasis
		origin  string
		gain    *float64
	}{
		{lots.MissingIgnore, "unknown", nil},
		{lots.MissingZero, "assumed_zero", ptrF(500)},
	} {
		lots, err := RealizedLotsBetween(ctx, db, 0, 100*lotDay, "USD", false, SortAscending, c.missing)
		if err != nil || len(lots) != 1 {
			t.Fatalf("%s: %v %v", c.missing, lots, err)
		}
		l := lots[0]
		if l.GainOrigin != c.origin || (c.gain == nil) != (l.Gain == nil) || (c.gain != nil && !near(num(t, l.Gain), *c.gain)) ||
			l.Disposal != "sell" || l.DocumentKind != "engine" {
			t.Errorf("%s: realized %+v", c.missing, l)
		}
	}

	// The buckets: under zero the coverage is whole and the quality
	// says how much is assumption.
	for _, c := range []struct {
		missing  lots.MissingBasis
		flag     string
		coverage float64
	}{
		{lots.MissingIgnore, "", 0},
		{lots.MissingZero, "basis_assumed_zero=2", 1},
	} {
		rows, err := GainsBuckets(ctx, db, 35*lotDay, 41*lotDay+86399, "USD", "total", GainsAll, c.missing)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %v %v", c.missing, rows, err)
		}
		q := rows[0].Quality
		if !strings.Contains(q, "sells_rebuilt=1") || (c.flag != "" && !strings.Contains(q, c.flag)) ||
			(c.flag == "" && strings.Contains(q, "basis_assumed_zero")) {
			t.Errorf("%s: quality %q", c.missing, q)
		}
		if c.missing == lots.MissingZero && (rows[0].BasisCoverage == nil || !near(*rows[0].BasisCoverage, c.coverage)) {
			t.Errorf("%s: coverage %v", c.missing, rows[0].BasisCoverage)
		}
		if strings.Contains(q, "sells_without_documents") {
			t.Errorf("%s: a rebuilt sale is documented: %q", c.missing, q)
		}
	}
}

func ptrF(v float64) *float64 { return &v }

func TestReadersFlagTheEnginesFindings(t *testing.T) {
	// XYZ leaves A's snapshot unexplained, and the history opens with
	// a holding of unknown cost.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400, 'A', 'XYZ', 'buy', 'USD', 10, -100);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 2*86400,   'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 100),
            ('lot-src', 2*86400,   'A', 'OLD', 'OLD', 'public_equity', 'USD', 3, 30),
            ('lot-src', 100*86400, 'A', 'OLD', 'OLD', 'public_equity', 'USD', 3, 33);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	rows, err := GainsBuckets(ctx, db, 0, 100*lotDay+86399, "USD", "total", GainsAll, lots.MissingIgnore)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	if q := rows[0].Quality; !strings.Contains(q, "seed_lots=1") || !strings.Contains(q, "implied_disposals=1") {
		t.Errorf("quality %q", q)
	}
}

func TestOpenLotsShowTheEnginesLots(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	got, err := OpenLotsAsOf(ctx, db, 600*lotDay+86399, "USD", lots.MissingIgnore)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	l := got[0]
	if *l.Quantity != "10" || !near(num(t, l.BookValue), 300) || !near(num(t, l.MarketValue), 350) ||
		*l.AcquisitionDate != "1971-02-05" || *l.Term != "short" || *l.BasisOrigin != "rebuilt" || *l.ValueOrigin != "pro_rata" {
		t.Errorf("engine lot qty %s cost %s value %s acquired %s term %s origin %s value from %s",
			*l.Quantity, *l.BookValue, *l.MarketValue, *l.AcquisitionDate, *l.Term, *l.BasisOrigin, *l.ValueOrigin)
	}
}

func TestCoverageCountsRebuiltSales(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	rows, err := GainsCoverage(ctx, db, 0, 600*lotDay+86399, "USD")
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	r := rows[0]
	if r.Verdict != "ok" || r.SellsRebuilt != 1 || r.PositionsRebuilt != 1 || r.LotMode == nil || *r.LotMode != "fill" ||
		r.LotMethods == nil || *r.LotMethods != "fifo" {
		t.Errorf("coverage %+v", r)
	}
}

func TestCheckComparesTheEngineWithStatedLots(t *testing.T) {
	// A 1099-B states the sale's cost as 110; the engine's FIFO says
	// 100. The check shows the delta.
	db, ctx := lotFixture(t, tradesSeed+`
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id, instrument_external_id,
                                   document_kind, tax_year, acquired_various, disposal_date, currency, quantity,
                                   proceeds, book_value, is_primary, basis_origin, basis_method, basis_fees)
            VALUES ('lot-src', 'R1', 'A', 'XYZ', 'form_1099b', 1971, FALSE, DATE '1971-05-16', 'USD', 10, 300, 110, TRUE,
                    'stated', 'lots', 'included');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	rows, err := GainsCheck(ctx, db, 0, 600*lotDay, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var realized *GainsCheckRow
	for i := range rows {
		if rows[i].Check == "realized" {
			realized = &rows[i]
		}
	}
	if realized == nil || *realized.Period != "1971" || !near(num(t, realized.EngineCost), 100) ||
		!near(num(t, realized.StatedCost), 110) || !near(num(t, realized.CostDelta), -10) || !near(num(t, realized.GainDelta), 10) {
		t.Fatalf("check rows %+v", rows)
	}
}

func TestCheckComparesAShadowLedgerWithTheStatedAverage(t *testing.T) {
	// The source states an average cost of 410 for its 20 units; the
	// shadow ledger's average is 400.
	db, ctx := lotFixture(t, tradesSeed+`
        UPDATE positions SET book_value = 410 WHERE snapshot_at = 450*86400;`)
	shadow := lots.Shadow
	avg := lots.Average
	rebuild(t, db, ctx, lots.Config{Sources: map[string]lots.SourceConfig{"lot-src": {Mode: &shadow, Method: &avg}}}, false)
	rows, err := GainsCheck(ctx, db, 0, 500*lotDay, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Check != "positions" || !near(num(t, rows[0].EngineCost), 400) ||
		!near(num(t, rows[0].StatedCost), 410) || !near(num(t, rows[0].CostDelta), -10) {
		t.Fatalf("check rows %+v", rows)
	}
}

func TestCheckCountsAHandoverOnce(t *testing.T) {
	// Two lots move to another source of the same kind, which states
	// their cost as 160. The handover carries 10 units and 150.
	db, ctx := lotFixture(t, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
            VALUES ('lot-b', 'manual', '/tmp/b.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at)
            VALUES ('lot-b', 'B', 'brokerage', 1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',          'USD', 5, -50),
            ('lot-src', 'b2', 2*86400,  'A', 'XYZ', 'buy',          'USD', 5, -100),
            ('lot-src', 'o1', 10*86400, 'A', 'XYZ', 'transfer_out', 'USD', -10, 0),
            ('lot-b',   'i1', 11*86400, 'B', 'XYZ', 'transfer_in',  'USD', 10, 0);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value, book_value) VALUES
            ('lot-b', 20*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 200, 160);`)
	cfg := fillCfg(lots.FIFO)
	fill := lots.Fill
	cfg.Sources["lot-b"] = lots.SourceConfig{Mode: &fill}
	rebuild(t, db, ctx, cfg, false)
	rows, err := GainsCheck(ctx, db, 0, 30*lotDay, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var h *GainsCheckRow
	for i := range rows {
		if rows[i].Check == "handover" {
			h = &rows[i]
		}
	}
	if h == nil || h.Items != 1 || !near(num(t, h.Quantity), 10) || !near(num(t, h.EngineCost), 150) ||
		!near(num(t, h.StatedCost), 160) {
		t.Fatalf("check rows %+v", rows)
	}
}

func TestMigration0118DDLIsRerunnable(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	rerunMigrationDDL(t, db, ctx, "0118_lot_engine.sql")
	if n := queryFloat(t, db, `SELECT count(*) FROM lots`); n != 2 {
		t.Errorf("a replay must keep the ledger: %v lots", n)
	}
	if !rebuild(t, db, ctx, fillCfg(lots.FIFO), false).Unchanged {
		t.Error("a replay changes no input")
	}
}

func TestOpenLotsStandAtTheirPositionsSnapshot(t *testing.T) {
	// A buy after the last snapshot is not in the lots of the position
	// that snapshot shows: they add up to its 10 units.
	db, ctx := lotFixture(t, tradesSeed+`
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b3', 650*86400, 'A', 'XYZ', 'buy', 'USD', 10, -500);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	rows, err := OpenLotsAsOf(ctx, db, 700*lotDay, "USD", lots.MissingIgnore)
	if err != nil {
		t.Fatal(err)
	}
	qty := 0.0
	for _, r := range rows {
		qty += num(t, r.Quantity)
	}
	if !near(qty, 10) {
		t.Fatalf("lots add up to %v under a 10-unit position: %+v", qty, rows)
	}
}

func TestCheckComparesEachAccountOfAPortfolio(t *testing.T) {
	// Two accounts of one portfolio each sell 10 XYZ with a statement
	// lot of their own: one realized row each.
	db, ctx := lotFixture(t, `
        INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
            VALUES ('lot-src', 'P', 'Section', 1, 1);
        UPDATE accounts SET portfolio_external_id = 'P';
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'ba', 1*86400,  'A', 'XYZ', 'buy',  'USD',  10, -100),
            ('lot-src', 'bb', 1*86400,  'B', 'XYZ', 'buy',  'USD',  10, -100),
            ('lot-src', 'sa', 50*86400, 'A', 'XYZ', 'sell', 'USD', -10,  300),
            ('lot-src', 'sb', 50*86400, 'B', 'XYZ', 'sell', 'USD', -10,  300);
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id, instrument_external_id,
                                   document_kind, tax_year, acquired_various, disposal_date, currency, quantity,
                                   proceeds, book_value, is_primary, basis_origin, basis_method, basis_fees) VALUES
            ('lot-src', 'RA', 'A', 'XYZ', 'form_1099b', 1970, FALSE, DATE '1970-02-20', 'USD', 10, 300, 110, TRUE, 'stated', 'lots', 'included'),
            ('lot-src', 'RB', 'B', 'XYZ', 'form_1099b', 1970, FALSE, DATE '1970-02-20', 'USD', 10, 300, 120, TRUE, 'stated', 'lots', 'included');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	rows, err := GainsCheck(ctx, db, 0, 100*lotDay, "USD")
	if err != nil {
		t.Fatal(err)
	}
	stated := map[string]float64{}
	for _, r := range rows {
		if r.Check == "realized" {
			stated[r.AccountExternalID] = num(t, r.StatedCost)
		}
	}
	if len(stated) != 2 || !near(stated["A"], 110) || !near(stated["B"], 120) {
		t.Fatalf("check rows %+v", rows)
	}
}
