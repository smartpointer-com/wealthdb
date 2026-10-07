package ubs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestHistoricalBookValueCurrency: a statement prints the cost price in
// the instrument's currency and the position in the portfolio's base
// currency. units × cost_price becomes the book value only when the two
// agree; otherwise the book value stays NULL and the cost price rides in
// the payload with its currency.
func TestHistoricalBookValueCurrency(t *testing.T) {
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

	same := got["CH0000000001"]
	if same.BookValue == nil || same.BookValue.String() != "1000" {
		t.Errorf("same-currency book value = %v, want 1000", same.BookValue)
	}
	if string(same.Payload) != `{"kind":"equity"}` {
		t.Errorf("same-currency payload = %s, want it untouched", same.Payload)
	}

	cross := got["US0000000002"]
	if cross.Currency != "CHF" {
		t.Fatalf("cross-currency position currency = %q, want CHF", cross.Currency)
	}
	if cross.BookValue != nil {
		t.Errorf("cross-currency book value = %v, want NULL", cross.BookValue)
	}
	var p struct {
		Kind         string `json:"kind"`
		CostPrice    string `json:"cost_price"`
		CostCurrency string `json:"cost_currency"`
	}
	if err := json.Unmarshal(cross.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", cross.Payload, err)
	}
	if p.Kind != "equity" || p.CostPrice != "80.5" || p.CostCurrency != "USD" {
		t.Errorf("cross-currency payload = %s, want kind kept, cost_price 80.5, cost_currency USD", cross.Payload)
	}

	if none := got["US0000000003"]; none.BookValue != nil || string(none.Payload) != `{"kind":"equity"}` {
		t.Errorf("no cost price: book value %v, payload %s; want NULL and untouched", none.BookValue, none.Payload)
	}
}
