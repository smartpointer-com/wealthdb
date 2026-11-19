package gold

import (
	"fmt"
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

// A security never arrives short, a coin can. 100 shares leave A and 99
// land in B the next day: no guess pairs them, so B's shares arrive at a
// cost nothing states. 1.0 coin leaves A and 0.9995 lands in B: the guess
// pairs them, B's coin carries A's cost, and the shortfall leaves as a fee.
// A coin is class crypto held physical, the row's class and vehicle where
// it states them, else the instrument's. A share of a fund that holds coins
// is a security, and so is a class or vehicle gold does not state.
func TestLotPassPairsASecurityOnlyOnTheExactQuantity(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                 string
		rowClass, rowVehicle string // the transfer rows' class and vehicle ("" for none)
		instClass, instVeh   string // an instruments row's ("" for no row)
		out, in              float64
		wantPaired           bool
	}{
		{"shares short by one", "public_equity", "stock", "", "", 100, 99, false},
		{"the same shares", "public_equity", "stock", "", "", 100, 100, true},
		{"a class no row states", "", "", "", "", 100, 99, false},
		{"a crypto fund's shares short by one", "crypto", "etf", "", "", 100, 99, false},
		{"a crypto holding of no stated vehicle", "", "", "crypto", "", 1, 0.9995, false},
		{"a coin short by a fee, the row's class", "crypto", "physical", "", "", 1, 0.9995, true},
		{"a coin short by a fee, the instrument's class", "", "", "crypto", "physical", 1, 0.9995, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lit := func(v string) string {
				if v == "" {
					return "NULL"
				}
				return "'" + v + "'"
			}
			seed := fmt.Sprintf(`
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount, asset_class, vehicle) VALUES
                ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',          'USD', %[1]v, -1000, NULL, NULL),
                ('lot-src', 'o1', 10*86400, 'A', 'XYZ', 'transfer_out', 'USD', -%[1]v, -1200, %[3]s, %[4]s),
                ('lot-src', 'i1', 11*86400, 'B', 'XYZ', 'transfer_in',  'USD', %[2]v,  1200, %[3]s, %[4]s);
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   instrument_external_id, asset_class, currency, quantity, market_value) VALUES
                ('lot-src', 20*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', %[2]v, 1200);`,
				c.out, c.in, lit(c.rowClass), lit(c.rowVehicle))
			if c.instClass != "" {
				seed += fmt.Sprintf(`
            INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, first_seen_at, last_seen_at)
                VALUES ('lot-src', 'XYZ', '%s', %s, 1, 1);`, c.instClass, lit(c.instVeh))
			}
			db, ctx := lotFixture(t, seed)
			rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
			got := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'B'`)
			if paired := !math.IsNaN(got); paired != c.wantPaired {
				t.Errorf("B's basis %v: paired %t, want %t", got, paired, c.wantPaired)
			}
			if want := 1000 * c.in / c.out; c.wantPaired && !closeEnough(got, want) {
				t.Errorf("B's basis %v, want %v: A's cost for what arrived", got, want)
			}
		})
	}
}

// A coin pairs by the coin rule, as the matcher and returns pair it: 0.1
// coin leaves A, and B receives it short by a fee worth a few dollars at the
// coin's price that day, within two days either way. Three days apart, or
// short by a fee worth more than 100 USD and more than 0.1%, the receipt
// arrives at a cost nothing states.
func TestLotPassPairsACoinByTheCoinRule(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		inDay      int
		in         float64
		wantPaired bool
	}{
		{"short by a fee worth 30 USD, a day later", 11, 0.099, true},
		{"received two days before it left", 8, 0.099, true},
		{"three days later", 13, 0.099, false},
		{"short by a fee worth 150 USD", 11, 0.095, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := lotFixture(t, fmt.Sprintf(`
            INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
                ('lot-src', 1*86400, 'USD', 'XYZ', 30000);
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount, asset_class, vehicle) VALUES
                ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',          'USD', 0.1, -3000, NULL, NULL),
                ('lot-src', 'o1', 10*86400, 'A', 'XYZ', 'transfer_out', 'USD', -0.1, -3000, 'crypto', 'physical'),
                ('lot-src', 'i1', %[1]d*86400, 'B', 'XYZ', 'transfer_in', 'USD', %[2]v, 3000, 'crypto', 'physical');
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   instrument_external_id, asset_class, currency, quantity, market_value) VALUES
                ('lot-src', 20*86400, 'B', 'XYZ', 'XYZ', 'crypto', 'USD', %[2]v, 3000);`, c.inDay, c.in))
			rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
			got := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'B'`)
			if paired := !math.IsNaN(got); paired != c.wantPaired {
				t.Errorf("B's basis %v: paired %t, want %t", got, paired, c.wantPaired)
			}
		})
	}
}

// A linked move of a security pairs what both legs hold and leaves the
// rest unpaired, never a fee: 100 shares leave A, the ledger links them to
// 99 that land in B. The 99 move with A's cost, the 100th leaves A as a
// transfer nothing pairs, a finding, and no fee is realized. Where 100
// land for 99 sent, the 100th is B's receipt nothing pairs. A coin keeps
// its fee within the coin rule: here up to a quarter of what it sent, as
// the coin is cheap and 100 USD buys far more of it.
func TestLotPassLeavesALinkedSecurityExcessUnpaired(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, class, vehicle string
		out, in              float64
		wantFee              bool
	}{
		{"shares", "public_equity", "stock", 100, 99, false},
		{"more shares received", "public_equity", "stock", 99, 100, false},
		{"a coin within the coin rule", "crypto", "physical", 100, 90, true},
		{"a coin beyond it", "crypto", "physical", 100, 70, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := lotFixture(t, fmt.Sprintf(`
            INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
                ('lot-src', 10*86400, 'USD', 'XYZ', 0.1);
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount, asset_class, vehicle) VALUES
                ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',          'USD', %[1]v, -1000, NULL, NULL),
                ('lot-src', 'o1', 10*86400, 'A', 'XYZ', 'transfer_out', 'USD', -%[1]v, -1200, '%[3]s', '%[4]s'),
                ('lot-src', 'i1', 11*86400, 'B', 'XYZ', 'transfer_in',  'USD', %[2]v,  1200, '%[3]s', '%[4]s');
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   instrument_external_id, asset_class, currency, quantity, market_value) VALUES
                ('lot-src', 20*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', %[2]v, 1200);
            INSERT INTO transfer_links (silver_source_id, transaction_external_id, far_silver_source_id,
                                        far_account_external_id, far_transaction_external_id, evidence, measure) VALUES
                ('lot-src', 'o1', 'lot-src', 'B', 'i1', 'equity-ledger', 'in_kind'),
                ('lot-src', 'i1', 'lot-src', 'A', 'o1', 'equity-ledger', 'in_kind');`, c.out, c.in, c.class, c.vehicle))
			rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
			rest, restAcct := c.out-c.in, "A"
			if rest < 0 {
				rest, restAcct = -rest, "B"
			} else if got, want := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'B'`), 1000*c.in/c.out; !closeEnough(got, want) {
				t.Errorf("B's basis %v, want %v: A's cost for what both legs hold", got, want)
			}
			fees := queryFloat(t, db, `SELECT count(*) FROM lot_realized WHERE disposal = 'fee'`)
			unpaired := queryFloat(t, db, `SELECT count(*) FROM lot_findings WHERE finding = 'unpaired_transfer'
			                                   AND account_external_id = ? AND quantity = ?`, restAcct, rest)
			if (fees > 0) != c.wantFee || (unpaired > 0) == c.wantFee {
				t.Errorf("fees %v, unpaired findings of the rest %v; want a fee %t", fees, unpaired, c.wantFee)
			}
		})
	}
}

// A transfer pairs across brokers as returns pairs it: 10 shares leave A at
// one source and land in B at a source of another kind, the instrument
// named by its own id at each and one ISIN in both. Within 7 calendar days
// they pair and B's shares carry A's cost; 8 days apart they do not. The
// window counts days, not hours: the last minutes of the 7th day still pair.
func TestLotPassPairsAcrossBrokersByISIN(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		out, in    int64 // seconds
		wantPaired bool
	}{
		{"3 days apart", 10*86400 + 3600, 13*86400 + 3600, true},
		{"8 calendar days apart", 10*86400 + 3600, 18*86400 + 3600, false},
		{"7 calendar days, more than 168 hours", 10*86400 + 1800, 17*86400 + 84600, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := lotFixture(t, fmt.Sprintf(`
            INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
                VALUES ('lot-b', 'schwab', '/tmp/lot-b.db', -1, 0, 0);
            INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at)
                VALUES ('lot-b', 'B', 'brokerage', 1, 1);
            INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, first_seen_at, last_seen_at) VALUES
                ('lot-src', 'XYZ', 'public_equity', 'stock', 'XX0000000001', 1, 1),
                ('lot-b', 'XYZ.B', 'public_equity', 'stock', 'XX0000000001', 1, 1);
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount) VALUES
                ('lot-src', 'b1', 1*86400, 'A', 'XYZ',   'buy',          'USD',  10, -100),
                ('lot-src', 'o1', %d,      'A', 'XYZ',   'transfer_out', 'USD', -10, -150),
                ('lot-b',   'i1', %d,      'B', 'XYZ.B', 'transfer_in',  'USD',  10,  150);
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   instrument_external_id, asset_class, currency, quantity, market_value) VALUES
                ('lot-b', 30*86400, 'B', 'XYZ.B', 'XYZ.B', 'public_equity', 'USD', 10, 150);`, c.out, c.in))
			cfg := fillCfg(lots.FIFO)
			cfg.Sources["lot-b"] = cfg.Sources["lot-src"]
			rebuild(t, db, ctx, cfg, false)
			got := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'B'`)
			if paired := !math.IsNaN(got); paired != c.wantPaired {
				t.Errorf("B's basis %v: paired %t, want %t", got, paired, c.wantPaired)
			}
			if c.wantPaired && !closeEnough(got, 100) {
				t.Errorf("B's basis %v, want 100: A's cost", got)
			}
		})
	}
}

// Two legs move one holding where any identity agrees: the sender's
// instrument states an ISIN and a CUSIP, the receiver's at a source of
// another kind only the CUSIP; or the receiver's, at a source of the same
// kind, states nothing but the instrument id both share. Either way the
// legs pair and the receipt carries the sender's cost. A receiver of
// another kind naming the instrument by its own id alone does not pair.
func TestLotPassPairsWhereAnyIdentityAgrees(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, kind, inst, isin, cusip string // the receiving source's kind, and its instrument
		wantPaired                    bool
	}{
		{"the CUSIP alone, another kind", "schwab", "XYZ.B", "", "000000001", true},
		{"the instrument id alone, the same kind", "fred", "XYZ", "", "", true},
		{"an id of its own, another kind", "schwab", "XYZ.B", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lit := func(v string) string {
				if v == "" {
					return "NULL"
				}
				return "'" + v + "'"
			}
			db, ctx := lotFixture(t, fmt.Sprintf(`
            INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
                VALUES ('lot-b', '%[1]s', '/tmp/lot-b.db', -1, 0, 0);
            INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at)
                VALUES ('lot-b', 'B', 'brokerage', 1, 1);
            INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, first_seen_at, last_seen_at) VALUES
                ('lot-src', 'XYZ', 'public_equity', 'stock', 'XX0000000001', '000000001', 1, 1),
                ('lot-b', '%[2]s', 'public_equity', 'stock', %[3]s, %[4]s, 1, 1);
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount) VALUES
                ('lot-src', 'b1', 1*86400,  'A', 'XYZ',   'buy',          'USD',  10, -100),
                ('lot-src', 'o1', 10*86400, 'A', 'XYZ',   'transfer_out', 'USD', -10, -150),
                ('lot-b',   'i1', 11*86400, 'B', '%[2]s', 'transfer_in',  'USD',  10,  150);
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   instrument_external_id, asset_class, currency, quantity, market_value) VALUES
                ('lot-b', 30*86400, 'B', '%[2]s', '%[2]s', 'public_equity', 'USD', 10, 150);`, c.kind, c.inst, lit(c.isin), lit(c.cusip)))
			cfg := fillCfg(lots.FIFO)
			cfg.Sources["lot-b"] = cfg.Sources["lot-src"]
			rebuild(t, db, ctx, cfg, false)
			got := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'B'`)
			if paired := !math.IsNaN(got); paired != c.wantPaired || (paired && !closeEnough(got, 100)) {
				t.Errorf("B's basis %v: paired %t, want %t at A's cost", got, paired, c.wantPaired)
			}
		})
	}
}
