package gold

import (
	"database/sql"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// movedBook is an invented bank portfolio P: the cash account CASH
// beside the custody accounts SAFE and SAFE2. SAFE held 100 of a share
// to December, SAFE2 holds them from January, priced at 50. The bank
// books each quarter's dividend of 0.5 a share on CASH, twice while SAFE
// held the share and twice since SAFE2 does. BRK, a brokerage account of
// the same bank, holds 100 of another share and is paid on itself.
func movedBook() dividendBook {
	early := ymd(2025, 1, 1)
	var txns []bookTxn
	for _, d := range []time.Time{ymd(2025, 9, 10), ymd(2025, 12, 10), ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"bank", "CASH", "QMOV", d, "dividend", 50, 0},
			bookTxn{"bank", "BRK", "QOWN", d, "dividend", 50, 0})
	}
	return dividendBook{instruments: []bookInstrument{
		{"bank", "QMOV", "XX0000000003", "", "QMOV"}, {"bank", "QOWN", "XX0000000004", "", "QOWN"},
	}, lines: []bookLine{
		{"bank", "SAFE", "QMOV", []bookSeg{{early, 100, 5000}, {ymd(2026, 1, 31), 0, 0}}},
		{"bank", "SAFE2", "QMOV", []bookSeg{{ymd(2026, 1, 31), 100, 5000}}},
		{"bank", "BRK", "QOWN", []bookSeg{{early, 100, 5000}}},
	}, txns: txns, portfolios: map[string]string{"CASH": "P", "SAFE": "P", "SAFE2": "P", "BRK": "P"},
		after: []string{`UPDATE accounts SET account_kind = 'cash' WHERE account_external_id = 'CASH'`}}
}

// A dividend booked on the cash account belongs to the account of its
// portfolio that held the share as it was paid: SAFE's sold position
// before the move, SAFE2's holding after it. A line booked on the
// account that holds its share stays its own. What pools the share
// across accounts reads the same as where the holdings of the instant
// place every line: the realized yield and the indicated dividend of the
// holding, and the income received by source, by portfolio and in all.
func TestALineIsItsHolderAsItWasPaid(t *testing.T) {
	t.Parallel()
	in, db, ctx := movedBook().inputs(t)
	key := func(acct, id string) yield.HoldingKey {
		return yield.HoldingKey{Source: "bank", Account: acct, PositionKey: id}
	}
	sold, held, own := key("SAFE", "QMOV"), key("SAFE2", "QMOV"), key("BRK", "QOWN")
	for k, n := range map[yield.HoldingKey]int{sold: 2, held: 2, own: 4} {
		if ps := in.Payments[k]; len(ps) != n || in.Income[k].Total() != 50*float64(n) {
			t.Errorf("%s/%s: payments %+v, income %v, want %d", k.Account, k.PositionKey, ps, in.Income[k], n)
		}
	}
	ps, err := placeTransactions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	bases := map[string]int{}
	for _, p := range ps {
		bases[p.key.Account+" "+string(p.basis)]++
	}
	if want := map[string]int{"SAFE portfolio": 2, "SAFE2 portfolio": 2, "BRK own": 4}; !reflect.DeepEqual(bases, want) {
		t.Errorf("placements %v, want %v", bases, want)
	}

	// The holdings of the instant place every line where no placement is
	// read.
	base, err := readYieldBase(ctx, db, ymd(2026, 6, 30).Unix()+86399, &yieldShared{placed: map[yield.TxnKey]unitKey{}},
		[]string{"USD"})
	if err != nil {
		t.Fatal(err)
	}
	instant := base.inputs("USD")
	if ps := instant.Payments[held]; len(ps) != 4 {
		t.Fatalf("by the instant's holdings, SAFE2 is paid %+v", ps)
	}
	b, o := yield.Compute(in), yield.Compute(instant)
	for _, k := range []yield.HoldingKey{held, own} {
		p, q := position(b, k), position(o, k)
		if p.Realized == nil || q.Realized == nil || *p.Realized != *q.Realized ||
			p.DividendYield == nil || q.DividendYield == nil || *p.DividendYield != *q.DividendYield {
			t.Errorf("%s: realized %s / %s, dividend yield %s / %s", k.Account, fmtFloatPtr(p.Realized),
				fmtFloatPtr(q.Realized), fmtFloatPtr(p.DividendYield), fmtFloatPtr(q.DividendYield))
		}
	}
	if p := position(b, held); p.DividendYield == nil || math.Abs(*p.DividendYield-4) > 1e-9 {
		t.Errorf("SAFE2's dividend yield %s, want 4", fmtFloatPtr(p.DividendYield))
	}
	received := func(in *yield.Inputs) (map[string]float64, map[string]int) {
		lines, err := incomeReceived(ctx, db, in, ymd(2025, 7, 1).Unix(), ymd(2026, 6, 30).Unix()+86399)
		if err != nil {
			t.Fatal(err)
		}
		sums, accounts := map[string]float64{}, map[string]int{}
		for _, l := range lines {
			a := in.Accounts[yield.AccountKey{Source: l.Holding.Source, Account: l.Holding.Account}]
			sums["all"] += l.Value
			sums["source "+l.Holding.Source] += l.Value
			sums["portfolio "+a.Portfolio] += l.Value
			accounts[l.Holding.Account]++
		}
		return sums, accounts
	}
	sums, accounts := received(in)
	instantSums, instantAccounts := received(instant)
	if !reflect.DeepEqual(sums, instantSums) || sums["all"] != 400 {
		t.Errorf("received %v, by the instant's holdings %v", sums, instantSums)
	}
	if want := map[string]int{"SAFE": 2, "SAFE2": 2, "BRK": 4}; !reflect.DeepEqual(accounts, want) ||
		!reflect.DeepEqual(instantAccounts, map[string]int{"SAFE2": 4, "BRK": 4}) {
		t.Errorf("received lines by account %v, by the instant's holdings %v", accounts, instantAccounts)
	}
}

// The yield pass stores each transaction's holder with the stamp of
// its inputs, and a reader reads them there. A reader whose inputs
// stamp otherwise, such as a gold the pass has not placed since they
// moved, places the lines itself; the materializer also stores them.
func TestTransactionHoldingsAreReadWhereCurrent(t *testing.T) {
	t.Parallel()
	bk := movedBook()
	bk.after = nil
	in, db, ctx := bk.inputs(t)
	count := func(where string) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM transaction_holdings WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	read := func(q querier, write *sql.Tx) map[yield.TxnKey]unitKey {
		placed, err := readPlacements(ctx, q, write)
		if err != nil {
			t.Fatal(err)
		}
		return placed
	}
	if len(in.Placed) != 8 || count("basis = 'portfolio'") != 4 {
		t.Fatalf("placed %v", in.Placed)
	}
	execEach(t, db, `DELETE FROM transaction_holdings`)
	if placed := read(db, nil); len(placed) != 0 {
		t.Errorf("the stored holders are none, read %v", placed)
	}
	// SAFE2 leaves the portfolio: CASH's lines since the move are the
	// one account of the bank's that holds the share.
	execEach(t, db, `UPDATE accounts SET portfolio_external_id = 'Q' WHERE account_external_id = 'SAFE2'`)
	placed := read(db, nil)
	if !reflect.DeepEqual(placed, in.Placed) || count("TRUE") != 0 {
		t.Errorf("placed %v, want %v, none stored", placed, in.Placed)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	read(tx, tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if again := read(db, nil); !reflect.DeepEqual(again, in.Placed) || count("basis = 'source'") != 2 {
		t.Errorf("stored %v, want %v", again, in.Placed)
	}
	// A load moves the source's row, and the config a holding alias:
	// either leaves the stored holders stale.
	for _, stmt := range []string{`UPDATE silver_sources SET last_loaded_at = last_loaded_at + 1`,
		`INSERT INTO holding_aliases (silver_source_id, account_external_id, from_key, to_key)
		    VALUES ('bank', 'BRK', 'QNONE', 'QNONE2')`} {
		execEach(t, db, `DELETE FROM transaction_holdings`)
		if placed := read(db, nil); len(placed) != 0 {
			t.Fatalf("before %s: read %v, want the stored none", stmt, placed)
		}
		execEach(t, db, stmt)
		if placed := read(db, nil); !reflect.DeepEqual(placed, in.Placed) {
			t.Errorf("after %s: placed %v, want %v", stmt, placed, in.Placed)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		read(tx, tx)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

// A dividend booked on a cash account of a portfolio whose account held
// the share once, and paid while one account of another portfolio holds
// it, is that account's: its quantity is the holder's. The book is
// invented: SAFE in P held 100 to September, OTHER in Q holds 100 of the
// same share, and the bank books two dividends of 0.5 a share on CASH,
// in P, after SAFE sold.
func TestALinePlacedAtItsSourceReadsItsHoldersQuantity(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	var txns []bookTxn
	for _, d := range []time.Time{ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"bank", "CASH", "QSRC", d, "dividend", 50, 0})
	}
	in, db, ctx := dividendBook{instruments: []bookInstrument{{"bank", "QSRC", "XX0000000005", "", "QSRC"}},
		lines: []bookLine{
			{"bank", "SAFE", "QSRC", []bookSeg{{early, 100, 5000}, {ymd(2025, 10, 31), 0, 0}}},
			{"bank", "OTHER", "QSRC", []bookSeg{{early, 100, 5000}}},
		}, txns: txns, portfolios: map[string]string{"CASH": "P", "SAFE": "P", "OTHER": "Q"}}.inputs(t)
	ps, err := placeTransactions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.key.Account != "OTHER" || p.basis != yield.PlacedSource {
			t.Errorf("%s placed on %s, %s", p.id, p.key.Account, p.basis)
		}
	}
	paid := in.Payments[yield.HoldingKey{Source: "bank", Account: "OTHER", PositionKey: "QSRC"}]
	if len(paid) != 2 {
		t.Fatalf("OTHER's payments %+v", paid)
	}
	for _, p := range paid {
		if p.Quantity == nil || *p.Quantity != 100 || (p.Record != nil && *p.Record != 100) {
			t.Errorf("payment of %s read on %s (record %s), want OTHER's 100", p.Day.Format(time.DateOnly),
				fmtFloatPtr(p.Quantity), fmtFloatPtr(p.Record))
		}
	}
}

// The points the placements read from the positions alone are those
// the units reader reads over the grid of each account's snapshots: a
// key named at its account's first snapshot or later, one it leaves
// and is named in again, a quantity a snapshot states as none, and an
// account whose positions end.
func TestHoldingPointsAreTheGrids(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := func(m int) int64 { return ymd(2026, m+1, 1).Unix() - 43200 }
	var rows []string
	add := func(acct, iid string, m int, qty string) {
		rows = append(rows, "('src', "+itoa(snap(m))+", '"+acct+"', '"+iid+"', '"+iid+"', 'public_equity', 'stock', "+
			"'USD', "+qty+", 100)")
	}
	for m := 1; m <= 8; m++ {
		add("A", "QBASE", m, "10")
		if m >= 3 {
			add("A", "QLATE", m, "5")
		}
		if m <= 2 || m >= 5 {
			add("A", "QGAP", m, itoa(int64(m)))
		}
		if m == 4 {
			add("A", "QNULL", m, "NULL")
		} else {
			add("A", "QNULL", m, "7")
		}
		if m <= 6 {
			add("A", "QZERO", m, map[bool]string{true: "0", false: "3"}[m == 2])
		}
		if m <= 3 {
			add("B", "QBASE", m, "4")
		}
	}
	execEach(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
	    instrument_external_id, asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(rows, ", "))
	in := &yield.Inputs{}
	srcs := []string{"src", "src", "src", "src", "src"}
	iids := []string{"QBASE", "QLATE", "QGAP", "QNULL", "QZERO"}
	grid, runs := newHolders(in), newHolders(in)
	if _, err := readPoints(ctx, db, grid, srcs, iids, 0, math.MaxInt64, map[string]int64{}); err != nil {
		t.Fatal(err)
	}
	if err := readHoldingPoints(ctx, db, runs, srcs, iids); err != nil {
		t.Fatal(err)
	}
	for k, ps := range grid.Points {
		for i := range ps {
			ps[i].Prev = 0
		}
		grid.Points[k] = ps
	}
	if len(grid.Points) != 6 || !reflect.DeepEqual(grid.Points, runs.Points) ||
		!reflect.DeepEqual(grid.Accounts, runs.Accounts) {
		t.Errorf("grid %v %v, runs %v %v", grid.Points, grid.Accounts, runs.Points, runs.Accounts)
	}
}

// A dividend paid after its account's holding moved to another account
// of the portfolio stays with the account it is booked on: the bank pays
// it on what that account held on the record day. The book is invented:
// A and B in P each held 100 of a share; B moved its 100 to A in
// January, and the bank books February's dividend of 0.5 a share on
// both accounts.
func TestALinePaidAfterItsHoldingMovedStaysOnItsAccount(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	var txns []bookTxn
	for _, d := range []time.Time{ymd(2025, 11, 10), ymd(2026, 2, 10), ymd(2026, 5, 10)} {
		txns = append(txns, bookTxn{"bank", "A", "QMVD", d, "dividend", 50, 0})
		if d.Before(ymd(2026, 5, 1)) {
			txns = append(txns, bookTxn{"bank", "B", "QMVD", d, "dividend", 50, 0})
		}
	}
	in, db, ctx := dividendBook{instruments: []bookInstrument{{"bank", "QMVD", "XX0000000006", "", "QMVD"}},
		lines: []bookLine{
			{"bank", "A", "QMVD", []bookSeg{{early, 100, 5000}, {ymd(2026, 1, 31), 200, 10000}}},
			{"bank", "B", "QMVD", []bookSeg{{early, 100, 5000}, {ymd(2026, 1, 31), 0, 0}}},
		}, txns: txns, portfolios: map[string]string{"A": "P", "B": "P"}}.inputs(t)
	ps, err := placeTransactions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.basis != yield.PlacedOwn {
			t.Errorf("%s placed on %s, %s", p.id, p.key.Account, p.basis)
		}
	}
	key := func(acct string) yield.HoldingKey {
		return yield.HoldingKey{Source: "bank", Account: acct, PositionKey: "QMVD"}
	}
	if a, b := in.Payments[key("A")], in.Payments[key("B")]; len(a) != 3 || len(b) != 2 {
		t.Errorf("A's payments %+v, B's %+v", a, b)
	}
}
