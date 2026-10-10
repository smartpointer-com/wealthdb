package gold

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// Every id and figure below is invented. Days are Unix days; a
// snapshot stamped at midnight states the day's close.

const lotDay = 86400

// lotFixture opens a migrated gold with one source, lot-src, of a kind
// no policy is registered for, so each test's config decides its mode.
func lotFixture(t *testing.T, seed string) (*sql.DB, context.Context) {
	t.Helper()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
            VALUES ('lot-src', 'manual', '/tmp/lot.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at) VALUES
            ('lot-src', 'A', 'brokerage', 1, 1), ('lot-src', 'B', 'brokerage', 1, 1);
        DELETE FROM silver_sources WHERE silver_source_id = 'test-src';`+seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, ctx
}

// fillCfg turns lot-src on in fill mode at method m.
func fillCfg(m lots.Method) lots.Config {
	fill := lots.Fill
	return lots.Config{Method: m, Sources: map[string]lots.SourceConfig{"lot-src": {Mode: &fill}}}
}

func rebuild(t *testing.T, db *sql.DB, ctx context.Context, cfg lots.Config, force bool) *LotsSummary {
	t.Helper()
	sum, err := RebuildLots(ctx, db, LotsOptions{Config: cfg, Force: force})
	if err != nil {
		t.Fatalf("RebuildLots: %v", err)
	}
	// The ledger has no key: its lot ids are unique by construction.
	if dup := queryFloat(t, db, `SELECT count(*) - count(DISTINCT lot_id) FROM lots`); dup != 0 {
		t.Fatalf("%v duplicate lot ids", dup)
	}
	return sum
}

func queryFloat(t *testing.T, db *sql.DB, q string, args ...any) float64 {
	t.Helper()
	var v sql.NullFloat64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if !v.Valid {
		return math.NaN()
	}
	return v.Float64
}

func queryString(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v.String
}

func feq(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

// tradesSeed: two buys and a sale of XYZ in account A, with snapshots
// that state no basis. FIFO sells the day-1 lot: cost 100 of 10 units
// sold for 300, the day-400 lot of 10 at 300 stays.
const tradesSeed = `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400 + 3600,   'A', 'XYZ', 'buy',  'USD',  10, -100),
            ('lot-src', 'b2', 400*86400 + 3600, 'A', 'XYZ', 'buy',  'USD',  10, -300),
            ('lot-src', 's1', 500*86400 + 3600, 'A', 'XYZ', 'sell', 'USD', -10,  300);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, vehicle, currency, quantity, market_value) VALUES
            ('lot-src', 2*86400,   'A', 'XYZ', 'XYZ', 'public_equity', 'stock', 'USD', 10, 150),
            ('lot-src', 450*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'stock', 'USD', 20, 600),
            ('lot-src', 600*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'stock', 'USD', 10, 350);`

func TestLotPassFillsPositionsAndRealizes(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	sum := rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if len(sum.Sources) != 1 || sum.Sources[0].RealizedLots != 1 || sum.Sources[0].PositionsFilled != 3 {
		t.Fatalf("summary %+v", sum.Sources)
	}
	for _, c := range []struct {
		day  int
		want float64
	}{{2, 100}, {450, 400}, {600, 300}} {
		got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, c.day*lotDay)
		if !feq(got, c.want) {
			t.Errorf("day %d: book_value %v, want %v", c.day, got, c.want)
		}
	}
	if s := queryString(t, db, `SELECT basis_stamp(basis_origin, basis_method, basis_fees) FROM positions WHERE snapshot_at = ?`, 600*lotDay); s != "rebuilt/fifo/unknown" {
		t.Errorf("stamp %q", s)
	}
	var id, term, kind string
	var primary bool
	var cost, proceeds float64
	if err := db.QueryRow(`SELECT realized_lot_external_id, term, document_kind, is_primary, book_value, proceeds
	                         FROM lot_realized`).Scan(&id, &term, &kind, &primary, &cost, &proceeds); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "s1#") || term != "long" || kind != "engine" || !primary || !feq(cost, 100) || !feq(proceeds, 300) {
		t.Errorf("realized lot %s %s %s %v %v %v", id, term, kind, primary, cost, proceeds)
	}
	// The ledger: two lots, one closed by the sale.
	if n := queryFloat(t, db, `SELECT count(*) FROM lots WHERE closed_at IS NOT NULL`); n != 1 {
		t.Errorf("closed lots %v", n)
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_disposals WHERE kind = 'sell' AND realized_lot_external_id = ?`, id); n != 1 {
		t.Errorf("the disposal should name its realized lot: %v", n)
	}
}

func TestLotPassMethodChangesTheBasis(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	rebuild(t, db, ctx, fillCfg(lots.LIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 600*lotDay); !feq(got, 100) {
		t.Errorf("LIFO keeps the day-1 lot: %v", got)
	}
	if s := queryString(t, db, `SELECT basis_method FROM lot_realized`); s != "lifo" {
		t.Errorf("realized method %q", s)
	}
}

func TestLotPassStatedWins(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed+`
        UPDATE positions SET book_value = 777, basis_origin = 'stated', basis_method = 'lots', basis_fees = 'included'
         WHERE snapshot_at = 600*86400;
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id, instrument_external_id,
                                   document_kind, tax_year, acquired_various, disposal_date, currency, quantity,
                                   proceeds, book_value, is_primary, basis_origin, basis_method, basis_fees, acquisition_date)
            VALUES ('lot-src', 'R1', 'A', 'XYZ', 'form_1099b', 1971, FALSE, DATE '1971-05-16', 'USD', 10, 300, 110, TRUE,
                    'stated', 'lots', 'included', DATE '1970-01-02');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 600*lotDay); got != 777 {
		t.Errorf("a stated basis must stay: %v", got)
	}
	if p := queryString(t, db, `SELECT is_primary::VARCHAR FROM lot_realized`); p != "false" {
		t.Errorf("the engine's row beside a stated primary must not be primary: %s", p)
	}
}

func TestLotPassSkipsUnchangedInputs(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	cfg := fillCfg(lots.FIFO)
	rebuild(t, db, ctx, cfg, false)
	if !rebuild(t, db, ctx, cfg, false).Unchanged {
		t.Fatal("a second pass over the same inputs should replay nothing")
	}
	if rebuild(t, db, ctx, fillCfg(lots.HIFO), false).Unchanged {
		t.Fatal("a method change must replay")
	}
	// A load rewrites the rows the pass filled; the pass must notice.
	if _, err := db.Exec(`UPDATE positions SET book_value = NULL, basis_origin = NULL, basis_method = NULL,
	                        basis_fees = NULL, book_value_known = NULL, open_lots = NULL, quantity_without_basis = NULL`); err != nil {
		t.Fatal(err)
	}
	if rebuild(t, db, ctx, fillCfg(lots.HIFO), false).Unchanged {
		t.Fatal("outputs a reload dropped must be rewritten")
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM positions WHERE basis_origin = 'rebuilt'`); n != 3 {
		t.Errorf("rebuilt rows %v", n)
	}
}

func TestLotPassOffWritesNothing(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	sum := rebuild(t, db, ctx, lots.Config{}, false)
	if len(sum.Sources) != 0 {
		t.Fatalf("an unregistered kind is off: %+v", sum.Sources)
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM positions WHERE book_value IS NOT NULL`); n != 0 {
		t.Errorf("filled %v", n)
	}
}

func TestLotPassShadowFillsOnlyTheLedger(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	shadow := lots.Shadow
	rebuild(t, db, ctx, lots.Config{Sources: map[string]lots.SourceConfig{"lot-src": {Mode: &shadow}}}, false)
	if n := queryFloat(t, db, `SELECT count(*) FROM lots`); n != 2 {
		t.Errorf("ledger lots %v", n)
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM positions WHERE book_value_known IS NOT NULL`) +
		queryFloat(t, db, `SELECT count(*) FROM lot_realized`); n != 0 {
		t.Errorf("shadow wrote %v rows outside the ledger", n)
	}
}

func TestLotPassRewritesAShadowLedgerAReloadDropped(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed)
	shadow := lots.Shadow
	cfg := lots.Config{Sources: map[string]lots.SourceConfig{"lot-src": {Mode: &shadow}}}
	rebuild(t, db, ctx, cfg, false)
	// A reload of a shadow source drops its ledger and nothing else.
	if _, err := db.Exec(`DELETE FROM lots`); err != nil {
		t.Fatal(err)
	}
	if rebuild(t, db, ctx, cfg, false).Unchanged {
		t.Fatal("a ledger a reload dropped must be rewritten")
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM lots`); n != 2 {
		t.Errorf("ledger lots %v", n)
	}
}

func TestLotPassMovesLotsBetweenAccounts(t *testing.T) {
	// A journal moves 4 units from A to B a day apart; B's snapshot
	// carries the moved cost and date, and nothing seeds.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',     'USD', 10, -100),
            ('lot-src', 'j1', 10*86400, 'A', 'XYZ', 'journal', 'USD', -4,  -60),
            ('lot-src', 'j2', 11*86400, 'B', 'XYZ', 'journal', 'USD',  4,   60);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 20*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 6, 90),
            ('lot-src', 20*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', 4, 60);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'B'`); !feq(got, 40) {
		t.Errorf("B's basis %v, want 40", got)
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_findings WHERE finding IN ('seed', 'implied', 'unpaired_transfer')`); n != 0 {
		t.Errorf("findings %v", n)
	}
	if d := queryString(t, db, `SELECT acquisition_date::VARCHAR FROM lots WHERE account_external_id = 'B'`); d != "1970-01-02" {
		t.Errorf("moved lot dated %s", d)
	}
}

func TestLotPassCorporateActions(t *testing.T) {
	// A merger stated twice (two feeds) turns 10 OLD into 5 NEW; a cash
	// merger takes 3 CSH for 90; shares that arrive on a held
	// instrument split it.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,          'A', 'OLD', 'buy',              'USD',  10, -100),
            ('lot-src', 'b2', 1*86400,          'A', 'CSH', 'buy',              'USD',   3,  -30),
            ('lot-src', 'b3', 1*86400,          'A', 'SPL', 'buy',              'USD',   2,  -50),
            ('lot-src', 'm1', 10*86400,         'A', 'OLD', 'corporate_action', 'USD', -10, -200),
            ('lot-src', 'm2', 10*86400,         'A', 'NEW', 'corporate_action', 'USD',   5,  210),
            ('lot-src', 'm3', 10*86400 + 7200,  'A', 'OLD', 'corporate_action', 'USD', -10,    0),
            ('lot-src', 'm4', 10*86400 + 7200,  'A', 'NEW', 'corporate_action', 'USD',   5,    0),
            ('lot-src', 'c1', 12*86400,         'A', 'CSH', 'corporate_action', 'USD',  -3,   90),
            ('lot-src', 'd1', 13*86400,         'A', 'SPL', 'corporate_action', 'USD',   6,  600);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 20*86400, 'A', 'NEW', 'NEW', 'public_equity', 'USD', 5, 250),
            ('lot-src', 20*86400, 'A', 'SPL', 'SPL', 'public_equity', 'USD', 8, 800);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE position_key = 'NEW'`); !feq(got, 100) {
		t.Errorf("merged basis %v, want 100", got)
	}
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE position_key = 'SPL'`); !feq(got, 50) {
		t.Errorf("split basis %v, want 50", got)
	}
	var proceeds, cost float64
	var disposal string
	if err := db.QueryRow(`SELECT proceeds, book_value, disposal FROM lot_realized WHERE instrument_external_id = 'CSH'`).
		Scan(&proceeds, &cost, &disposal); err != nil {
		t.Fatal(err)
	}
	if !feq(proceeds, 90) || !feq(cost, 30) || disposal != "tender" {
		t.Errorf("cash merger %v %v %s", proceeds, cost, disposal)
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_findings WHERE finding IN ('seed', 'implied')`); n != 0 {
		t.Errorf("a doubled leg must count once: %v findings", n)
	}
}

func TestLotPassObservesZeroOnlyForAPresentAccount(t *testing.T) {
	// XYZ leaves A's snapshot while A shows other rows: an implied
	// disposal. B is missing from a snapshot altogether: no disposal.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400, 'A', 'XYZ', 'buy', 'USD', 10, -100),
            ('lot-src', 'b2', 1*86400, 'B', 'QQQ', 'buy', 'USD', 10, -100);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 2*86400,   'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 100),
            ('lot-src', 2*86400,   'B', 'QQQ', 'QQQ', 'public_equity', 'USD', 10, 100),
            ('lot-src', 100*86400, 'A', 'CSH', NULL,  'cash',          'USD', NULL, 5),
            ('lot-src', 200*86400, 'B', 'QQQ', 'QQQ', 'public_equity', 'USD', 10, 100);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_findings WHERE finding = 'implied' AND instrument_external_id = 'XYZ'`); n != 1 {
		t.Errorf("XYZ implied %v", n)
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_findings WHERE instrument_external_id = 'QQQ'`); n != 0 {
		t.Errorf("an absent account observes nothing: %v", n)
	}
}

func TestLotPassPoolsAPortfolio(t *testing.T) {
	// Two wallets of one portfolio: a coin bought in W1 sweeps to W2
	// minus a network fee; the pool's basis is shared by quantity, the
	// fee is a disposal at market value, and nothing moves.
	db, ctx := lotFixture(t, `
        INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
            VALUES ('lot-src', 'P', 'Pool', 1, 1);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, portfolio_external_id, first_seen_at, last_seen_at) VALUES
            ('lot-src', 'W1', 'crypto', 'P', 1, 1), ('lot-src', 'W2', 'crypto', 'P', 1, 1);
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('lot-src', 5*86400, 'USD', 'COIN', 20);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400 + 60, 'W1', 'COIN', 'buy',          'USD',  10, -100),
            ('lot-src', 'w1', 5*86400 + 60, 'W1', 'COIN', 'transfer_out', 'COIN', -6,   -6),
            ('lot-src', 'd1', 5*86400 + 90, 'W2', 'COIN', 'transfer_in',  'COIN', 5.8, 5.8);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 6*86400, 'W1', 'COIN', 'COIN', 'crypto', 'USD', 4,   80),
            ('lot-src', 6*86400, 'W2', 'COIN', 'COIN', 'crypto', 'USD', 5.8, 116);`)
	cfg := fillCfg(lots.FIFO)
	pooled := lots.GrainPortfolio
	src := cfg.Sources["lot-src"]
	src.Grain = &pooled
	cfg.Sources["lot-src"] = src
	rebuild(t, db, ctx, cfg, false)
	w1 := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'W1'`)
	w2 := queryFloat(t, db, `SELECT book_value FROM positions WHERE account_external_id = 'W2'`)
	if !feq(w1+w2, 98) || !feq(w1/w2, 4/5.8) {
		t.Errorf("pool shares %v, %v; want 98 split by quantity", w1, w2)
	}
	var proceeds, cost float64
	if err := db.QueryRow(`SELECT proceeds, book_value FROM lot_realized WHERE disposal = 'fee'`).Scan(&proceeds, &cost); err != nil {
		t.Fatal(err)
	}
	if !feq(proceeds, 4) || !feq(cost, 2) {
		t.Errorf("network fee realized %v for cost %v; want 4 and 2", proceeds, cost)
	}
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_disposals WHERE kind = 'move'`); n != 0 {
		t.Errorf("a sweep inside the pool moves nothing: %v moves", n)
	}
	if a := queryString(t, db, `SELECT DISTINCT account_external_id FROM lots`); a != "P" {
		t.Errorf("pooled lots belong to the portfolio, got %q", a)
	}
}

func TestLotPassValuesReceiptsInKind(t *testing.T) {
	// Staking income is costed at the day's market value; a day with
	// no rate leaves the lot without a cost.
	db, ctx := lotFixture(t, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('lot-src', 3*86400, 'USD', 'COIN', 40);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,  'A', 'COIN', 'buy',     'USD',  1, -10),
            ('lot-src', 'k1', 3*86400,  'A', 'COIN', 'staking', 'COIN', 2,   2),
            ('lot-src', 'k2', 30*86400, 'A', 'COIN', 'staking', 'COIN', 1,   1);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 4*86400,  'A', 'COIN', 'COIN', 'crypto', 'USD', 3, 120),
            ('lot-src', 31*86400, 'A', 'COIN', 'COIN', 'crypto', 'USD', 4, 160);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 4*lotDay); !feq(got, 90) {
		t.Errorf("basis with the staked coins at market value %v, want 90", got)
	}
	var known, missing float64
	if err := db.QueryRow(`SELECT book_value_known, quantity_without_basis FROM positions WHERE snapshot_at = ?`, 31*lotDay).
		Scan(&known, &missing); err != nil {
		t.Fatal(err)
	}
	if !feq(known, 90) || !feq(missing, 1) {
		t.Errorf("partial basis %v with %v units uncosted; want 90 and 1", known, missing)
	}
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 31*lotDay); !math.IsNaN(got) {
		t.Errorf("a partial basis is no complete basis: %v", got)
	}
}

func TestLotPassAdoptsAnchorsAndResolvesSeeds(t *testing.T) {
	// The history opens holding 10 of unknown cost; a later snapshot
	// states the lots: the seed takes their cost back to its opening.
	db, ctx := lotFixture(t, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value, book_value,
                               basis_origin, basis_method, basis_fees) VALUES
            ('lot-src', 10*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 100, NULL, NULL, NULL, NULL),
            ('lot-src', 20*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 120, 70, 'stated', 'lots', 'unknown');
        INSERT INTO position_lots (silver_source_id, snapshot_at, account_external_id, position_key, lot_key,
                                   instrument_external_id, currency, quantity, book_value, acquisition_date, basis_origin) VALUES
            ('lot-src', 20*86400, 'A', 'XYZ', 'L1', 'XYZ', 'USD', 4, 20, DATE '1970-01-02', 'stated'),
            ('lot-src', 20*86400, 'A', 'XYZ', 'L2', 'XYZ', 'USD', 6, 50, DATE '1970-01-05', 'stated');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 10*lotDay); !feq(got, 70) {
		t.Errorf("the snapshot before the anchor resolves to %v, want 70", got)
	}
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 20*lotDay); got != 70 {
		t.Errorf("the anchor's own row is stated: %v", got)
	}
	if got := queryFloat(t, db, `SELECT resolved_quantity FROM lot_anchors`); !feq(got, 10) {
		t.Errorf("resolved %v", got)
	}
}

func TestLotPassTakesALedgerReceiptsStatedBasis(t *testing.T) {
	// A hand-kept equity-transfer ledger row moves 10 XYZ in with its
	// pre-transfer cost basis; nothing in gold sends them.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount, payload) VALUES
            ('lot-src', 'xfer:1', 1*86400, 'A', 'XYZ', 'transfer_in', 'USD', 10, 900,
             '{"equity_transfer_ledger":true,"cost_basis":140,"instrument":"XYZ"}');
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 2*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 900);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM positions`); !feq(got, 140) {
		t.Errorf("basis %v, want the ledger's 140", got)
	}
	if co := queryString(t, db, `SELECT cost_origin FROM lots`); co != "stated" {
		t.Errorf("cost origin %q", co)
	}
}

func TestLotPassPairsAcrossSourcesOfOneKindOnly(t *testing.T) {
	// The same instrument leaves one source and arrives at another the
	// next day. Between two sources of one kind it is a move; between
	// kinds the ids need not mean the same thing, so it is not.
	for _, c := range []struct {
		kindB string
		want  float64
	}{{"manual", 40}, {"equityzen", math.NaN()}} {
		db, ctx := lotFixture(t, `
            INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
                VALUES ('lot-b', '`+c.kindB+`', '/tmp/b.db', -1, 0, 0);
            INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at)
                VALUES ('lot-b', 'B', 'brokerage', 1, 1);
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount) VALUES
                ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',          'USD', 10, -100),
                ('lot-src', 'o1', 10*86400, 'A', 'XYZ', 'transfer_out', 'USD', -4,  -60),
                ('lot-b',   'i1', 11*86400, 'B', 'XYZ', 'transfer_in',  'USD',  4,   60);
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   instrument_external_id, asset_class, currency, quantity, market_value) VALUES
                ('lot-b', 20*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', 4, 60);`)
		cfg := fillCfg(lots.FIFO)
		fill := lots.Fill
		cfg.Sources["lot-b"] = lots.SourceConfig{Mode: &fill}
		rebuild(t, db, ctx, cfg, false)
		got := queryFloat(t, db, `SELECT book_value FROM positions WHERE silver_source_id = 'lot-b'`)
		if (math.IsNaN(c.want) && !math.IsNaN(got)) || (!math.IsNaN(c.want) && !feq(got, c.want)) {
			t.Errorf("into a %s source: basis %v, want %v", c.kindB, got, c.want)
		}
	}
}

func TestLotPassSharesAReorgByStatedValue(t *testing.T) {
	// One instrument becomes two; the cost goes 3:1 by the legs' values,
	// and each new instrument holds a slice of both lots.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,  'A', 'OLD',  'buy',              'USD', 5,  -25),
            ('lot-src', 'b2', 2*86400,  'A', 'OLD',  'buy',              'USD', 5,  -75),
            ('lot-src', 'm1', 10*86400, 'A', 'OLD',  'corporate_action', 'USD', -10, -400),
            ('lot-src', 'm2', 10*86400, 'A', 'NEW1', 'corporate_action', 'USD',  6,  300),
            ('lot-src', 'm3', 10*86400, 'A', 'NEW2', 'corporate_action', 'USD',  2,  100);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 20*86400, 'A', 'NEW1', 'NEW1', 'public_equity', 'USD', 6, 300),
            ('lot-src', 20*86400, 'A', 'NEW2', 'NEW2', 'public_equity', 'USD', 2, 100);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	for k, want := range map[string]float64{"NEW1": 75, "NEW2": 25} {
		if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE position_key = ?`, k); !feq(got, want) {
			t.Errorf("%s: basis %v, want %v", k, got, want)
		}
		if n := queryFloat(t, db, `SELECT count(DISTINCT acquisition_date) FROM lots
		                           WHERE instrument_external_id = ? AND closed_at IS NULL`, k); n != 2 {
			t.Errorf("%s holds lots of %v dates, want 2", k, n)
		}
	}
}

func TestLotPassKeepsAStatedIdTheTradesUse(t *testing.T) {
	// The source keys the security by its CUSIP in the trades and the
	// documents alike, and its instruments also link the CUSIP to a
	// ticker row: the stated lot stays with the CUSIP and resolves the
	// seed the sale relieves.
	db, ctx := lotFixture(t, `
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, cusip, currency, first_seen_at, last_seen_at) VALUES
            ('lot-src', 'XYZ', 'public_equity', 'XYZ', NULL, 'USD', 1, 1),
            ('lot-src', '000000XY9', 'public_equity', 'XYZ', '000000XY9', 'USD', 1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 's1', 50*86400, 'A', '000000XY9', 'sell', 'USD', -10, 300);
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id, instrument_external_id,
                                   document_kind, tax_year, acquired_various, disposal_date, acquisition_date, currency,
                                   quantity, proceeds, book_value, is_primary, basis_origin, basis_method, basis_fees)
            VALUES ('lot-src', 'Y1', 'A', '000000XY9', 'form_1099b', 1970, FALSE, DATE '1970-02-20',
                    DATE '1969-06-01', 'USD', 10, 300, 120, TRUE, 'stated', 'lots', 'included');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM lot_realized`); !feq(got, 120) {
		t.Errorf("the sale's seed should take the stated cost: %v", got)
	}
}

func TestLotPassTakesTheSalesOwnAccountsStatedLotsFirst(t *testing.T) {
	// A and B share a portfolio. A's sale relieves a seed; B's statement
	// lot of the day before must not resolve it while A's own is there.
	db, ctx := lotFixture(t, `
        INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
            VALUES ('lot-src', 'P', 'Section', 1, 1);
        UPDATE accounts SET portfolio_external_id = 'P';
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 's1', 50*86400, 'A', 'XYZ', 'sell', 'USD', -10, 300);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 10*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 250);
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id, instrument_external_id,
                                   document_kind, tax_year, acquired_various, disposal_date, acquisition_date, currency,
                                   quantity, proceeds, book_value, is_primary, basis_origin, basis_method, basis_fees) VALUES
            ('lot-src', 'RA', 'A', 'XYZ', 'form_1099b', 1970, FALSE, DATE '1970-02-20', DATE '1969-06-01', 'USD',
             10, 300, 120, TRUE, 'stated', 'lots', 'included'),
            ('lot-src', 'RB', 'B', 'XYZ', 'form_1099b', 1970, FALSE, DATE '1970-02-19', DATE '1969-12-01', 'USD',
             10, 300, 999, TRUE, 'stated', 'lots', 'included');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if got := queryFloat(t, db, `SELECT book_value FROM lot_realized`); !feq(got, 120) {
		t.Errorf("A's sale took %v, want A's own stated 120", got)
	}
}

func TestLotPassLeavesADocumentedYearsCashMergerToItsDocuments(t *testing.T) {
	// A cash merger is a disposal the corporate actions make; in a year
	// the account's documents state, it is theirs, as a sale is.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,  'A', 'CSH', 'buy',              'USD', 3, -30),
            ('lot-src', 'c1', 12*86400, 'A', 'CSH', 'corporate_action', 'USD', -3, 90);
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id, instrument_external_id,
                                   document_kind, tax_year, acquired_various, disposal_date, currency, quantity,
                                   proceeds, book_value, is_primary, basis_origin, basis_method, basis_fees)
            VALUES ('lot-src', 'R1', 'A', 'CSH', 'form_1099b', 1970, FALSE, DATE '1970-01-13', 'USD', 3, 90, 30, TRUE,
                    'stated', 'lots', 'included');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if p := queryString(t, db, `SELECT is_primary::VARCHAR FROM lot_realized`); p != "false" {
		t.Errorf("the engine's cash merger in a documented year must not be primary: %s", p)
	}
}

func TestLotPassMovesAtTheReceiptWhenItComesFirst(t *testing.T) {
	// B's journal in comes two days before A's journal out. The lots
	// move at the receipt, so B's snapshot between the two carries their
	// cost. A's snapshot there still shows the 4 that left: the book does
	// not hold them, so their cost is unknown there, not zero.
	db, ctx := lotFixture(t, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400,  'A', 'XYZ', 'buy',     'USD', 10, -100),
            ('lot-src', 'j2', 10*86400, 'B', 'XYZ', 'journal', 'USD',  4,   60),
            ('lot-src', 'j1', 12*86400, 'A', 'XYZ', 'journal', 'USD', -4,  -60);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 11*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 150),
            ('lot-src', 11*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', 4, 60),
            ('lot-src', 20*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 6, 90),
            ('lot-src', 20*86400, 'B', 'XYZ', 'XYZ', 'public_equity', 'USD', 4, 60);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	basis := func(acct string, day int64) sql.NullFloat64 {
		var v sql.NullFloat64
		if err := db.QueryRow(`SELECT book_value FROM positions WHERE account_external_id = ? AND snapshot_at = ?`,
			acct, day*lotDay).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if b := basis("B", 11); !b.Valid || !feq(b.Float64, 40) {
		t.Errorf("B between the legs: basis %v, want 40", b)
	}
	if b := basis("A", 11); b.Valid {
		t.Errorf("A between the legs: basis %v, want unknown", b)
	}
	if missing := queryFloat(t, db, `SELECT quantity_without_basis FROM positions WHERE account_external_id = 'A' AND snapshot_at = ?`,
		11*lotDay); !feq(missing, 4) {
		t.Errorf("A between the legs: %v without basis, want 4", missing)
	}
	if b := basis("A", 20); !b.Valid || !feq(b.Float64, 60) {
		t.Errorf("A after the legs: basis %v, want 60", b)
	}
}

func TestLotPassCarriesAWalletASnapshotLeavesOut(t *testing.T) {
	// Two wallets of a pooled portfolio hold the coin; one is missing
	// from a snapshot. The pool keeps its basis whole, and nothing is
	// implied away.
	db, ctx := lotFixture(t, `
        INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
            VALUES ('lot-src', 'P', 'Pool', 1, 1);
        UPDATE accounts SET portfolio_external_id = 'P';
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400, 'A', 'COIN', 'buy', 'USD', 4, -40),
            ('lot-src', 'b2', 1*86400, 'B', 'COIN', 'buy', 'USD', 6, -60);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 2*86400,   'A', 'COIN', 'COIN', 'crypto', 'USD', 4, 40),
            ('lot-src', 2*86400,   'B', 'COIN', 'COIN', 'crypto', 'USD', 6, 60),
            ('lot-src', 10*86400,  'A', 'COIN', 'COIN', 'crypto', 'USD', 4, 40),
            ('lot-src', 100*86400, 'A', 'COIN', 'COIN', 'crypto', 'USD', 4, 40),
            ('lot-src', 100*86400, 'B', 'COIN', 'COIN', 'crypto', 'USD', 6, 60);`)
	fill, pooled := lots.Fill, lots.GrainPortfolio
	rebuild(t, db, ctx, lots.Config{Sources: map[string]lots.SourceConfig{"lot-src": {Mode: &fill, Grain: &pooled}}}, false)
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_findings WHERE finding IN ('seed', 'implied')`); n != 0 {
		t.Errorf("findings %v", n)
	}
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 10*lotDay); !feq(got, 40) {
		t.Errorf("A's share of the pool with B left out: %v, want 40", got)
	}
}

func TestLotPassDropsAWalletItsTradesEmptied(t *testing.T) {
	// A sweeps its coins to B inside the pool and leaves the snapshots:
	// it holds nothing, and B's coins are not counted twice.
	db, ctx := lotFixture(t, `
        INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
            VALUES ('lot-src', 'P', 'Pool', 1, 1);
        UPDATE accounts SET portfolio_external_id = 'P';
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400, 'A', 'COIN', 'buy',          'USD', 10, -100),
            ('lot-src', 'o1', 5*86400, 'A', 'COIN', 'transfer_out', 'USD', -10, 0),
            ('lot-src', 'i1', 5*86400, 'B', 'COIN', 'transfer_in',  'USD', 10, 0);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 2*86400,  'A', 'COIN', 'COIN', 'crypto', 'USD', 10, 100),
            ('lot-src', 10*86400, 'B', 'COIN', 'COIN', 'crypto', 'USD', 10, 100);`)
	fill, pooled := lots.Fill, lots.GrainPortfolio
	rebuild(t, db, ctx, lots.Config{Sources: map[string]lots.SourceConfig{"lot-src": {Mode: &fill, Grain: &pooled}}}, false)
	if n := queryFloat(t, db, `SELECT count(*) FROM lot_findings WHERE finding IN ('seed', 'implied')`); n != 0 {
		t.Errorf("findings %v", n)
	}
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE snapshot_at = ?`, 10*lotDay); !feq(got, 100) {
		t.Errorf("B's basis %v, want 100", got)
	}
}

func TestLotPassTakesAKeysCurrencyFromItsPositions(t *testing.T) {
	// The trade settles in CHF; the position is in USD. The cost is
	// converted into the position's currency.
	db, ctx := lotFixture(t, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('lot-src', 1*86400, 'USD', 'CHF', 1.25);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 'b1', 1*86400, 'A', 'XYZ', 'buy', 'CHF', 10, -100);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 2*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 150);`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	want := queryFloat(t, db, `SELECT 100 * rate FROM fx_daily WHERE from_ccy = 'CHF' AND to_ccy = 'USD'`)
	if got := queryFloat(t, db, `SELECT book_value FROM positions`); math.IsNaN(want) || !feq(got, want) || feq(got, 100) {
		t.Errorf("basis %v, want %v in USD", got, want)
	}
}

func TestLotPassLeavesADocumentedYearToItsDocuments(t *testing.T) {
	// A year-end summary states the account's sales by CUSIP; the trades
	// use the ticker. The engine's rows for the year are not primary, so
	// the sale is not counted twice, and the stated lot still resolves
	// the seed the sale relieves.
	db, ctx := lotFixture(t, `
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, cusip, currency, first_seen_at, last_seen_at) VALUES
            ('lot-src', 'XYZ', 'public_equity', 'XYZ', NULL, 'USD', 1, 1),
            ('lot-src', '000000XY9', 'public_equity', 'XYZ', '000000XY9', 'USD', 1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('lot-src', 's1', 50*86400, 'A', 'XYZ', 'sell', 'USD', -10, 300);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('lot-src', 10*86400, 'A', 'XYZ', 'XYZ', 'public_equity', 'USD', 10, 250),
            ('lot-src', 10*86400, 'A', 'OTH', 'OTH', 'public_equity', 'USD', 1, 1),
            ('lot-src', 60*86400, 'A', 'OTH', 'OTH', 'public_equity', 'USD', 1, 1);
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id, instrument_external_id,
                                   document_kind, tax_year, acquired_various, disposal_date, acquisition_date, currency,
                                   quantity, proceeds, book_value, is_primary, basis_origin, basis_method, basis_fees)
            VALUES ('lot-src', 'Y1', 'A', '000000XY9', 'year_end_summary', 1970, FALSE, DATE '1970-02-20',
                    DATE '1969-06-01', 'USD', 10, 300, 120, TRUE, 'stated', 'lots', 'included');`)
	rebuild(t, db, ctx, fillCfg(lots.FIFO), false)
	if p := queryString(t, db, `SELECT is_primary::VARCHAR FROM lot_realized`); p != "false" {
		t.Errorf("the engine's row in a documented year must not be primary: %s", p)
	}
	if got := queryFloat(t, db, `SELECT book_value FROM positions WHERE position_key = 'XYZ' AND snapshot_at = ?`, 10*lotDay); !feq(got, 120) {
		t.Errorf("the stated lot resolves the seed back to its opening: %v", got)
	}
}

// The writer stamps a rebuilt basis with the engine's method and fees
// names, so each must be a value the stamp vocabulary knows.
func TestLotNamesAreBasisStampValues(t *testing.T) {
	for m := lots.FIFO; m <= lots.Average; m++ {
		if !canonical.BasisMethod(m.String()).Valid() {
			t.Errorf("method %q is no basis_method", m)
		}
	}
	for f := lots.FeesUnknown; f <= lots.FeesExcluded; f++ {
		if !canonical.BasisFees(f.String()).Valid() {
			t.Errorf("fees %q is no basis_fees", f)
		}
	}
}
