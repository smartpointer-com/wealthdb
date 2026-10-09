package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The cost an MT535 holding states, as the collector promotes it. Every
// account, ISIN, amount and rate below is invented.

// psnHoldingPositions seeds one snapshot of holdings and returns the
// positions the PSN-only adapter emits, keyed by ISIN. withCost adds
// the cost columns of collector migration 0005; without them the
// silver is the older shape.
func psnHoldingPositions(t *testing.T, withCost bool, seed string) map[string]canonical.PositionChange {
	t.Helper()
	path, db := newFixtureSilver(t)
	if withCost {
		if _, err := db.Exec(`
            ALTER TABLE holdings ADD COLUMN cost_basis          REAL;
            ALTER TABLE holdings ADD COLUMN cost_currency       TEXT;
            ALTER TABLE holdings ADD COLUMN average_cost        REAL;
            ALTER TABLE holdings ADD COLUMN acquisition_fx_rate REAL;
            ALTER TABLE holdings ADD COLUMN acquisition_fx_from TEXT;
            ALTER TABLE holdings ADD COLUMN acquisition_fx_to   TEXT;`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 5, '/x/1');
        INSERT INTO instruments(snapshot_at, relationship_id, isin, payload) VALUES
            (1000, 'R1', 'XX0000000001', '{"InstrCtgyCFI":"ESVUFR","GacInstrRskCcyIsoCd":"USD"}'),
            (1000, 'R1', 'XX0000000002', '{"InstrCtgyCFI":"ESVUFR","GacInstrRskCcyIsoCd":"CHF"}'),
            (1000, 'R1', 'XX0000000003', '{"InstrCtgyCFI":"ESVUFR","GacInstrRskCcyIsoCd":"CHF"}'),
            (1000, 'R1', 'XX0000000004', '{"InstrCtgyCFI":"ESVUFR","GacInstrRskCcyIsoCd":"CHF"}');`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(seed); err != nil {
		t.Fatal(err)
	}
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
	got := map[string]canonical.PositionChange{}
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			got[p.PositionKey] = p
		}
		if !more {
			break
		}
	}
	return got
}

// holdingPayload is an MT535 holding with a HOLD leg per currency.
const (
	holdUSD    = `'{"fields":{"19A":[":HOLD//USD1500,",":BOOK//USD1000,"],"93B":[":AGGR//UNIT/10,"]}}'`
	holdCHFUSD = `'{"fields":{"19A":[":HOLD//CHF1200,",":HOLD//USD1500,",":BOOK//USD1000,"],"93B":[":AGGR//UNIT/10,"]}}'`
)

// TestPSNBookValue: BOOK in the position's currency is the book value
// as stated. BOOK in another currency converts only at the holding's
// own AEXR, and only when that rate runs from BOOK's currency to the
// position's; otherwise the book value is NULL and the stated cost
// travels in the payload.
func TestPSNBookValue(t *testing.T) {
	got := psnHoldingPositions(t, true, `
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload,
                             cost_basis, cost_currency, average_cost,
                             acquisition_fx_rate, acquisition_fx_from, acquisition_fx_to) VALUES
            -- Same currency.
            (1000, 'R1', 'SAFE1', 'XX0000000001', `+holdUSD+`, 1000, 'USD', 100, NULL, NULL, NULL),
            -- The position is in CHF, BOOK in USD, and AEXR runs USD to CHF.
            (1000, 'R1', 'SAFE1', 'XX0000000002', `+holdCHFUSD+`, 1000, 'USD', 100, 0.9, 'USD', 'CHF'),
            -- Same, without a rate.
            (1000, 'R1', 'SAFE1', 'XX0000000003', `+holdCHFUSD+`, 1000, 'USD', 100, NULL, NULL, NULL),
            -- A rate that runs the other way converts nothing.
            (1000, 'R1', 'SAFE1', 'XX0000000004', `+holdCHFUSD+`, 1000, 'USD', 100, 1.1, 'CHF', 'USD');`)

	same := got["XX0000000001"]
	if same.Currency != "USD" || same.BookValue == nil || same.BookValue.String() != "1000" {
		t.Errorf("same currency: %s book value %v, want USD 1000", same.Currency, same.BookValue)
	}
	if same.Basis != statedAverageBasis {
		t.Errorf("same currency: basis %+v, want stated/average/excluded", same.Basis)
	}

	converted := got["XX0000000002"]
	if converted.Currency != "CHF" || converted.BookValue == nil || converted.BookValue.String() != "900" {
		t.Errorf("AEXR: %s book value %v, want CHF 900", converted.Currency, converted.BookValue)
	}
	if converted.Basis != derivedAverageBasis {
		t.Errorf("AEXR: basis %+v, want derived/average/excluded", converted.Basis)
	}

	type costFields struct {
		CostBasis    string `json:"cost_basis"`
		CostCurrency string `json:"cost_currency"`
		Rate         string `json:"acquisition_fx_rate"`
		From         string `json:"acquisition_fx_from"`
		To           string `json:"acquisition_fx_to"`
	}
	for isin, want := range map[string]costFields{
		"XX0000000003": {CostBasis: "1000", CostCurrency: "USD"},
		"XX0000000004": {CostBasis: "1000", CostCurrency: "USD", Rate: "1.1", From: "CHF", To: "USD"},
	} {
		p := got[isin]
		if p.Currency != "CHF" || p.BookValue != nil || !p.Basis.IsZero() {
			t.Errorf("%s: %s book value %v basis %+v, want CHF, NULL and no stamp", isin, p.Currency, p.BookValue, p.Basis)
		}
		var c costFields
		if err := json.Unmarshal(p.Payload, &c); err != nil {
			t.Fatalf("%s payload %s: %v", isin, p.Payload, err)
		}
		if c != want {
			t.Errorf("%s payload cost = %+v, want %+v", isin, c, want)
		}
	}
}

// TestAPSNSilverWithoutCostColumnsStatesNoBookValue: a silver older than
// the cost columns still loads, with no book value.
func TestAPSNSilverWithoutCostColumnsStatesNoBookValue(t *testing.T) {
	got := psnHoldingPositions(t, false, `
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (1000, 'R1', 'SAFE1', 'XX0000000001', `+holdUSD+`);`)
	p, ok := got["XX0000000001"]
	if !ok {
		t.Fatal("holding not emitted")
	}
	if p.BookValue != nil || !p.Basis.IsZero() {
		t.Errorf("book value %v basis %+v, want NULL and no stamp", p.BookValue, p.Basis)
	}
}

// TestNoBookAndNoCostLeavesThePayloadAlone: a holding stating no cost
// (a private-markets fund's units) keeps its payload as delivered.
func TestNoBookAndNoCostLeavesThePayloadAlone(t *testing.T) {
	book, basis, payload := psnBookValue(holdingCost{}, "USD", `{"fields":{}}`)
	if book != nil || !basis.IsZero() || string(payload) != `{"fields":{}}` {
		t.Errorf("got %v %+v %s, want NULL, no stamp, payload untouched", book, basis, payload)
	}
	// A stated cost in the position's currency needs no rate at all.
	book, _, _ = psnBookValue(holdingCost{
		basis:    sql.NullFloat64{Float64: 5, Valid: true},
		currency: sql.NullString{String: "USD", Valid: true},
	}, "USD", `{}`)
	if book == nil || book.String() != "5" {
		t.Errorf("same currency without a rate: book %v, want 5", book)
	}
}
