package ubs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// historicalPositions runs the statement stream over a full window and
// keys its positions by ISIN.
func historicalPositions(t *testing.T, web *webReader) map[string]canonical.PositionChange {
	t.Helper()
	ctx := context.Background()
	w := canonical.Window{Start: 0, End: 2000, HasChanges: true}
	stream, err := web.snapshotsHistorical(ctx, w, nil, map[string]int64{}, map[string]int64{})
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

type costPayload struct {
	Kind         string `json:"kind"`
	CostPrice    string `json:"cost_price"`
	CostCurrency string `json:"cost_currency"`
}

func decodeCostPayload(t *testing.T, p canonical.PositionChange) costPayload {
	t.Helper()
	var out costPayload
	if err := json.Unmarshal(p.Payload, &out); err != nil {
		t.Fatalf("payload %s: %v", p.Payload, err)
	}
	return out
}

// TestStatementCostValueIsTheBookValue: the statement's cost value is
// the units at their average cost and average buy rate, in the
// portfolio's currency, which is the position's. It is the book value
// as stated, whatever the instrument's currency. A bond's cost price is
// a percent of the nominal, so the cost value is what makes it right:
// units × cost price would be a hundred times too high.
func TestStatementCostValueIsTheBookValue(t *testing.T) {
	ctx := context.Background()
	web := newWebFixture(t)
	if _, err := web.db.ExecContext(ctx, `
        ALTER TABLE historical_position_snapshots ADD COLUMN cost_basis REAL;
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id,
             instrument_isin, currency_iso, units, market_value,
             market_value_currency, cost_price, cost_basis, source_doc_token, payload)
        VALUES
            (1000, '0999AAAAAAAA02', '', 'CH0000000001', 'CHF', 10, 1200, 'CHF', 100, 1000, 'tok', '{"kind":"equity"}'),
            (1000, '0999AAAAAAAA02', '', 'US0000000002', 'USD', 10, 900,  'CHF', 80, 720, 'tok', '{"kind":"equity"}'),
            (1000, '0999AAAAAAAA02', '', 'XS0000000003', 'CHF', 10000, 10100, 'CHF', 101.5, 10150, 'tok', '{"kind":"bond"}'),
            (1000, '0999AAAAAAAA02', '', 'XS0000000004', 'CHF', 10000, 10100, 'CHF', 99, NULL, 'tok', '{"kind":"bond"}');
    `); err != nil {
		t.Fatal(err)
	}
	got := historicalPositions(t, web)

	for isin, want := range map[string]string{
		"CH0000000001": "1000",
		"US0000000002": "720",
		"XS0000000003": "10150",
	} {
		p := got[isin]
		if p.BookValue == nil || p.BookValue.String() != want {
			t.Errorf("%s book value = %v, want %s", isin, p.BookValue, want)
		}
		if p.Basis != statedAverageBasis {
			t.Errorf("%s basis = %+v, want stated/average/excluded", isin, p.Basis)
		}
		if p.Currency != "CHF" {
			t.Errorf("%s currency = %q, want the portfolio's CHF", isin, p.Currency)
		}
		if c := decodeCostPayload(t, p); c.CostPrice != "" {
			t.Errorf("%s payload = %s, want it untouched", isin, p.Payload)
		}
	}

	// A cost price without a cost value is never multiplied out: for
	// this bond that would be a hundred times its cost.
	bare := got["XS0000000004"]
	if bare.BookValue != nil || !bare.Basis.IsZero() {
		t.Errorf("no cost value: book value %v basis %+v, want NULL and no stamp", bare.BookValue, bare.Basis)
	}
	if c := decodeCostPayload(t, bare); c.Kind != "bond" || c.CostPrice != "99" || c.CostCurrency != "CHF" {
		t.Errorf("no cost value: payload = %s, want kind kept, cost_price 99, cost_currency CHF", bare.Payload)
	}
}

// TestAStatementSilverWithoutCostValueStatesNone: a silver that predates
// the cost value states no book value for any holding. The cost price
// it does carry travels in the payload with its currency, and a row
// without one is untouched.
func TestAStatementSilverWithoutCostValueStatesNone(t *testing.T) {
	ctx := context.Background()
	web := newWebFixture(t)
	if _, err := web.db.ExecContext(ctx, `
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id,
             instrument_isin, currency_iso, units, market_value,
             market_value_currency, cost_price, source_doc_token, payload)
        VALUES
            (1000, '0999AAAAAAAA02', '', 'CH0000000001', 'CHF', 10, 1200, 'CHF', 100, 'tok', '{"kind":"equity"}'),
            (1000, '0999AAAAAAAA02', '', 'US0000000002', 'USD', 10, 900,  'CHF', 80.5, 'tok', '{"kind":"equity"}'),
            (1000, '0999AAAAAAAA02', '', 'US0000000003', 'USD', 10, 900,  'CHF', NULL, 'tok', '{"kind":"equity"}');
    `); err != nil {
		t.Fatal(err)
	}
	got := historicalPositions(t, web)

	for isin, want := range map[string]costPayload{
		"CH0000000001": {Kind: "equity", CostPrice: "100", CostCurrency: "CHF"},
		"US0000000002": {Kind: "equity", CostPrice: "80.5", CostCurrency: "USD"},
	} {
		p := got[isin]
		if p.BookValue != nil {
			t.Errorf("%s book value = %v, want NULL", isin, p.BookValue)
		}
		if c := decodeCostPayload(t, p); c != want {
			t.Errorf("%s payload = %s, want %+v", isin, p.Payload, want)
		}
	}
	if none := got["US0000000003"]; none.BookValue != nil || string(none.Payload) != `{"kind":"equity"}` {
		t.Errorf("no cost price: book value %v, payload %s; want NULL and untouched", none.BookValue, none.Payload)
	}
}
