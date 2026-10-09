package equityzen

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedFees builds three deals with invented figures:
//
//   - f1 (spv): 100 shares bought for 1,000 plus a 30 execution fee; 40
//     sold on 2023-01-01, which charged its own fee.
//   - f2 (private_fund): bought for 2,000 plus a 40 execution fee.
//   - f3 (spv): 50 shares bought for 500, the purchase stating no fee.
func seedFees(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at) VALUES (1700000000);
        INSERT INTO offerings(deal_external_id, kind, company_name, shares_original, currency) VALUES
            ('f1', 'spv',          'Example SPV',  100, 'USD'),
            ('f2', 'private_fund', 'Example Fund', 200, 'USD'),
            ('f3', 'spv',          'Example Two',   50, 'USD');
        INSERT INTO positions(deal_external_id, event_seq, as_of_date, event_type,
            is_open, shares_held, cost_basis_remaining, market_value) VALUES
            ('f1', 0, '2022-01-01', 'investment',  1, 100, 1000, 1000),
            ('f1', 1, '2023-01-01', 'disposition', 1,  60,  600, 1200),
            ('f2', 0, '2022-06-01', 'investment',  1, 200, 2000, 2000),
            ('f2', 1, '2023-06-01', 'statement',   1, 200, 2000, 2500),
            ('f3', 0, '2022-03-01', 'investment',  1,  50,  500,  500);
        INSERT INTO cash_flows(cash_flow_external_id, deal_external_id, kind,
            flow_date, amount, execution_fee, shares, price_per_share) VALUES
            ('cf-f1-buy',  'f1', 'purchase',     '2022-01-01', 1000, 30,   100, 10),
            ('cf-f1-dist', 'f1', 'distribution', '2023-01-01',  800,  8,    40, 20),
            ('cf-f2-buy',  'f2', 'purchase',     '2022-06-01', 2000, 40,   200, 10),
            ('cf-f3-buy',  'f3', 'purchase',     '2022-03-01',  500, NULL,  50, 10);
    `); err != nil {
		t.Fatal(err)
	}
}

// The execution fee paid on a purchase is part of the basis. A partial
// sale takes the same share of the fee as it takes of the cost; a fund
// keeps the whole fee; a purchase that states no fee leaves the cost
// alone, stamped as excluding it. A sale's own fee is no purchase fee.
func TestTheBookValueIncludesThePurchaseFeeProRata(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedFees(t, db)
	conn := openAdapter(t, path)
	ctx := context.Background()
	w, _ := conn.ChangeWindow(ctx, -1)
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := map[int64]map[string]canonical.PositionChange{}
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range b.Positions {
			if got[p.SnapshotAt] == nil {
				got[p.SnapshotAt] = map[string]canonical.PositionChange{}
			}
			got[p.SnapshotAt][p.PositionKey] = p
		}
		if !more {
			break
		}
	}
	for _, c := range []struct {
		day, deal, book, fee string
		basis                canonical.Basis
	}{
		{"2022-01-01", "f1", "1030.00", "30", feeBasis},
		{"2023-01-01", "f1", "618.00", "18", feeBasis},
		{"2022-06-01", "f2", "2040.00", "40", feeBasis},
		{"2023-06-01", "f2", "2040.00", "40", feeBasis},
		{"2023-06-01", "f3", "500.00", "", costBasis},
	} {
		p, ok := got[iso(t, c.day)][c.deal]
		if !ok {
			t.Errorf("%s %s: no position", c.day, c.deal)
			continue
		}
		if p.BookValue == nil || p.BookValue.StringFixed(2) != c.book {
			t.Errorf("%s %s: book value %v, want %s", c.day, c.deal, p.BookValue, c.book)
		}
		if p.Basis != c.basis {
			t.Errorf("%s %s: stamp %+v, want %+v", c.day, c.deal, p.Basis, c.basis)
		}
		want := ""
		if c.fee != "" {
			want = `{"execution_fee_in_basis":"` + c.fee + `"}`
		}
		if string(p.Payload) != want {
			t.Errorf("%s %s: payload %s, want %s", c.day, c.deal, p.Payload, want)
		}
	}
}

// An SPV whose shares bought are not stated cannot divide its fee, so its
// book value stays the cost alone. A deal with no cost has no book value.
func TestABookValueWithoutTheFiguresToApportionTheFee(t *testing.T) {
	f := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	var null sql.NullFloat64

	v, b, share := bookValue(true, f(600), f(60), null, f(30))
	if v == nil || v.String() != "600" || b != costBasis || share != nil {
		t.Errorf("no shares bought: %v %+v %v, want 600 stated excluding the fee", v, b, share)
	}
	v, b, share = bookValue(true, null, f(60), f(100), f(30))
	if v != nil || !b.IsZero() || share != nil {
		t.Errorf("no cost: %v %+v %v, want no book value and no stamp", v, b, share)
	}
	v, b, share = bookValue(true, f(600), f(60), f(100), f(0))
	if v == nil || v.String() != "600" || b != feeBasis || share == nil || !share.IsZero() {
		t.Errorf("zero fee: %v %+v %v, want 600 including the (zero) fee", v, b, share)
	}
}
