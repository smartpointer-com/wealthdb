package swissquote

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// positionsByKey drains a Snapshots stream into its positions, keyed
// by position_key.
func positionsByKey(t *testing.T, path string) map[string]canonical.PositionChange {
	t.Helper()
	conn := openAdapter(t, path)
	ctx := context.Background()
	w, err := conn.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	out := map[string]canonical.PositionChange{}
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			out[p.PositionKey] = p
		}
		if !more {
			return out
		}
	}
}

func seedCostRows(t *testing.T, seed *sql.DB) {
	t.Helper()
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 6, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, '1000001', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload,
                              source, average_cost, price_quote, market_value_chf, unrealized_gain_loss_chf) VALUES
            -- A live equity row: the export states the unit cost and
            -- the CHF figures.
            (1000, '1000001', 'VTI', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"VTI","quantity":10,"price":150,"total_value":1500,"unit_cost":120,"total_value_chf":1200,"pl_nominal_chf":240}',
             'live', 120, 'unit', 1200, 240),
            -- A statement bond: nominal 10000, prices in percent of
            -- nominal, no total_value, a non-CHF valuation.
            (1000, '1000001', 'Placeholder Note 2030', 'EUR',
             '{"asset_class":"Bonds","currency":"EUR","name":"Placeholder Note 2030","quantity":10000,"avg_price":98.5,"market_price":101,"valuation_chf":9500}',
             'pp:doc-1', 98.5, 'percent', NULL, NULL),
            -- A live row whose export leaves the unit cost blank.
            (1000, '1000001', 'QQQ', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"QQQ","quantity":4,"price":500,"total_value":2000,"unit_cost":""}',
             'live', NULL, 'unit', 1800, NULL);
    `); err != nil {
		t.Fatal(err)
	}
}

func TestBookValueAtAverageCost(t *testing.T) {
	path, seed := newFixtureSilver(t)
	seedCostRows(t, seed)
	got := positionsByKey(t, path)

	cases := []struct {
		key, book string
	}{
		{"VTI@USD", "1200"},                   // 10 × 120
		{"Placeholder Note 2030@EUR", "9850"}, // 10000 × 98.5 %
	}
	for _, c := range cases {
		p, ok := got[c.key]
		if !ok {
			t.Fatalf("no position %s", c.key)
		}
		if p.BookValue == nil || p.BookValue.String() != c.book {
			t.Errorf("%s book_value = %v, want %s", c.key, p.BookValue, c.book)
		}
		if p.Basis != averageCostBasis {
			t.Errorf("%s basis = %+v, want %+v", c.key, p.Basis, averageCostBasis)
		}
		if err := canonical.ValidateBookValue(p.BookValue, p.Basis); err != nil {
			t.Errorf("%s: %v", c.key, err)
		}
	}

	// The CHF figures stay in the payload, as the export states them.
	var payload map[string]any
	if err := json.Unmarshal(got["VTI@USD"].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["total_value_chf"] != 1200.0 || payload["pl_nominal_chf"] != 240.0 {
		t.Errorf("payload lost the CHF figures: %v", payload)
	}

	qqq := got["QQQ@USD"]
	if qqq.BookValue != nil || !qqq.Basis.IsZero() {
		t.Errorf("QQQ without an average cost: book_value = %v, basis = %+v; want neither", qqq.BookValue, qqq.Basis)
	}
}

// A statement prices a bond in percent of nominal, so its market value
// is quantity × market price ÷ 100 where the statement states no value
// in the row's currency.
func TestMarketValueAtPercentQuote(t *testing.T) {
	path, seed := newFixtureSilver(t)
	seedCostRows(t, seed)
	p := positionsByKey(t, path)["Placeholder Note 2030@EUR"]
	if p.MarketValue == nil || p.MarketValue.String() != "10100" {
		t.Errorf("market_value = %v, want 10100 (10000 × 101 %%)", p.MarketValue)
	}
}

// A silver from before migration 0006 has no cost columns: it still
// loads, with no book value and every price per unit.
func TestBookValueOnSilverWithoutCostColumns(t *testing.T) {
	path, seed := newFixtureSilver(t)
	for _, col := range []string{"average_cost", "price_quote", "market_value_chf", "unrealized_gain_loss_chf"} {
		if _, err := seed.Exec(`ALTER TABLE positions DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 5, '/x/1');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload) VALUES
            (1000, '1000001', 'VTI', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"VTI","quantity":10,"total_value":1500,"unit_cost":120}');
    `); err != nil {
		t.Fatal(err)
	}
	p := positionsByKey(t, path)["VTI@USD"]
	if p.BookValue != nil || !p.Basis.IsZero() {
		t.Errorf("book_value = %v, basis = %+v; want neither", p.BookValue, p.Basis)
	}
	if p.MarketValue == nil || p.MarketValue.String() != "1500" {
		t.Errorf("market_value = %v, want 1500", p.MarketValue)
	}
}

// A statement prints a bond's accrued interest beside its clean value.
// A CHF bond's market value includes it and states how much it is; a
// bond in another currency keeps its clean value, the CHF figure staying
// in the payload. A row without the figure states none.
func TestAStatementBondCarriesItsAccruedInterest(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 7, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, '1000001', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload,
                              source, average_cost, price_quote, accrued_interest_chf) VALUES
            (1000, '1000001', 'Placeholder Bond CHF', 'CHF',
             '{"asset_class":"Bonds","currency":"CHF","quantity":10000,"market_price":101,"valuation_chf":10100,"accrued_interest_chf":25}',
             'pp:doc-1', 100, 'percent', 25),
            (1000, '1000001', 'Placeholder Note EUR', 'EUR',
             '{"asset_class":"Bonds","currency":"EUR","quantity":10000,"market_price":101,"valuation_chf":9500,"accrued_interest_chf":20}',
             'pp:doc-1', 100, 'percent', 20),
            (1000, '1000001', 'Placeholder Bond Two', 'CHF',
             '{"asset_class":"Bonds","currency":"CHF","quantity":10000,"market_price":99,"valuation_chf":9900}',
             'pp:doc-1', 100, 'percent', NULL);
    `); err != nil {
		t.Fatal(err)
	}
	got := positionsByKey(t, path)
	for key, want := range map[string]struct{ mv, accrued string }{
		"Placeholder Bond CHF@CHF": {"10125", "25"},
		"Placeholder Note EUR@EUR": {"10100", ""},
		"Placeholder Bond Two@CHF": {"9900", ""},
	} {
		p := got[key]
		accrued := ""
		if p.AccruedInterest != nil {
			accrued = p.AccruedInterest.String()
		}
		if p.MarketValue == nil || p.MarketValue.String() != want.mv || accrued != want.accrued {
			t.Errorf("%s: market value %v, accrued %q; want %s, %q", key, p.MarketValue, accrued, want.mv, want.accrued)
		}
	}
}
