package gold

import (
	"math"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// A move the ledger states pairs its two legs before the quantity
// guess pairs the rest. 4 XYZ leave A; on the next day B and C each
// receive 4. The guess pairs the departure with the first receipt in
// its order, B's, unless a link names C's.
func TestLotPassPairsALinkedMoveFirst(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		links        string
		wantB, wantC float64 // the basis of B and of C
	}{
		{"guessed", "", 40, math.NaN()},
		{"linked", `
            INSERT INTO transfer_links (silver_source_id, transaction_external_id, far_silver_source_id,
                                        far_account_external_id, far_transaction_external_id, evidence, measure) VALUES
                ('lot-src', 'o1', 'lot-src', 'C', 'i2', 'equity-ledger', 'in_kind'),
                ('lot-src', 'i2', 'lot-src', 'A', 'o1', 'equity-ledger', 'in_kind');`, math.NaN(), 40},
		{"a cash link is no move", `
            INSERT INTO transfer_links (silver_source_id, transaction_external_id, far_silver_source_id,
                                        far_account_external_id, far_transaction_external_id, evidence, measure) VALUES
                ('lot-src', 'o1', 'lot-src', 'C', 'i2', 'amount', 'cash'),
                ('lot-src', 'i2', 'lot-src', 'A', 'o1', 'amount', 'cash');`, 40, math.NaN()},
	} {
		db, ctx := lotFixture(t, `
            INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at)
                VALUES ('lot-src', 'C', 'brokerage', 1, 1);
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount) VALUES
                ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',          'USD', 10, -100),
                ('lot-src', 'o1', 10*86400, 'A', 'XYZ', 'transfer_out', 'USD', -4,  -60),
                ('lot-src', 'i1', 11*86400, 'B', 'XYZ', 'transfer_in',  'USD',  4,   60),
                ('lot-src', 'i2', 11*86400, 'C', 'XYZ', 'transfer_in',  'USD',  4,   60);
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   instrument_external_id, asset_class, currency, quantity, market_value) VALUES
                ('lot-src', 20*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', 4, 60),
                ('lot-src', 20*86400, 'C', 'XYZ', 'XYZ', 'public_equity', 'USD', 4, 60);`+c.links)
		first := rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
		for acct, want := range map[string]float64{"B": c.wantB, "C": c.wantC} {
			got := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = ?`, acct)
			if (math.IsNaN(want) && !math.IsNaN(got)) || (!math.IsNaN(want) && !closeEnough(got, want)) {
				t.Errorf("%s: basis of %s %v, want %v", c.name, acct, got, want)
			}
		}
		// The links enter the fingerprint: dropping them replays.
		if _, err := db.ExecContext(ctx, `DELETE FROM transfer_links`); err != nil {
			t.Fatal(err)
		}
		again := rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
		if linked := c.name == "linked"; again.Unchanged == linked || first.Unchanged {
			t.Errorf("%s: a pass after the links went replayed = %v", c.name, !again.Unchanged)
		}
	}
}
