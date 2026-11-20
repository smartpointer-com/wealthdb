package loader

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// An SPV's exit in the listed shares it received, as two ledger rows:
// the out-leg of the private-market account and the in-leg at the
// broker both name the shares. The out-leg's holding names the SPV.
// Every id and figure is invented.
const (
	exitHeader = "silver_source_id,account,occurred_at,direction,quantity,cost_basis,value,currency,instrument,note"
	exitOut    = "pmco,LP,2098-01-10,out,10,400,1000,USD,PUB,the SPV paid out in shares"
	exitIn     = "brokerco,BRK,2098-01-10,in,10,400,1000,USD,PUB,the SPV paid out in shares"
)

// A ledger with the holding column carries it onto the out-leg; one
// without it parses as before, and its legs' payloads name no holding.
func TestParseTransferLedgerHolding(t *testing.T) {
	with, err := parseTransferLedger(strings.NewReader(exitHeader + ",holding\n" +
		exitOut + ",SPVX\n" + exitIn + ",\n"))
	if err != nil {
		t.Fatalf("parse with holding: %v", err)
	}
	without, err := parseTransferLedger(strings.NewReader(exitHeader + "\n" + exitOut + "\n" + exitIn + "\n"))
	if err != nil {
		t.Fatalf("parse without holding: %v", err)
	}
	out, in := with["pmco"][0], with["brokerco"][0]
	if out.Holding != "SPVX" || in.Holding != "" {
		t.Fatalf("holdings %q and %q, want the out-leg's alone", out.Holding, in.Holding)
	}
	if p := string(out.toChange("LP").Payload); !strings.Contains(p, `"`+canonical.PayloadHolding+`":"SPVX"`) {
		t.Errorf("out-leg payload %s names no holding", p)
	}
	for src, legs := range without {
		got := legs[0]
		if got.Holding != "" || strings.Contains(string(got.toChange("X").Payload), canonical.PayloadHolding) {
			t.Errorf("%s: a ledger without the column names a holding: %+v", src, got)
		}
		// Naming a holding keeps the leg's id, so it updates in place.
		if w := with[src][0]; w.syntheticID() != got.syntheticID() {
			t.Errorf("%s: id %s with a holding, %s without", src, w.syntheticID(), got.syntheticID())
		}
	}
	// A one-row move gives the holding to the sender alone.
	one, err := parseTransferLedger(strings.NewReader(exitHeader + ",holding,to_silver_source_id,to_account\n" +
		exitOut + ",SPVX,brokerco,BRK\n"))
	if err != nil {
		t.Fatalf("parse one-row move: %v", err)
	}
	if one["pmco"][0].Holding != "SPVX" || one["brokerco"][0].Holding != "" {
		t.Errorf("one-row move holdings %q and %q", one["pmco"][0].Holding, one["brokerco"][0].Holding)
	}
}

// A row with more or fewer fields than the header fails with its line:
// an unquoted comma in a note would shift a fragment into the holding,
// and a short row has lost a field. Each gets its own advice. A quoted
// comma parses.
func TestParseTransferLedgerFieldCount(t *testing.T) {
	for _, c := range []struct{ name, row, advice string }{
		{"unquoted comma", "pmco,LP,2098-01-10,out,10,400,1000,USD,PUB,paid out, in shares,SPVX\n", "quote a note"},
		{"short row", "pmco,LP,2098-01-10,out,10,400,1000,USD,PUB\n", "a field for every column"},
	} {
		_, err := parseTransferLedger(strings.NewReader(exitHeader + ",holding\n" + c.row))
		if err == nil || !strings.Contains(err.Error(), "line 2: ") || !strings.Contains(err.Error(), c.advice) {
			t.Errorf("%s: err %v, want the line and %q", c.name, err, c.advice)
		}
	}
	got, err := parseTransferLedger(strings.NewReader(exitHeader + ",holding\n" +
		`pmco,LP,2098-01-10,out,10,400,1000,USD,PUB,"paid out, in shares",SPVX` + "\n"))
	if err != nil || got["pmco"][0].Note != "paid out, in shares" || got["pmco"][0].Holding != "SPVX" {
		t.Errorf("quoted note: %+v, %v", got, err)
	}
}

// A holding names what an exit distributes, so an in-leg carries none.
func TestParseTransferLedgerHoldingOnAnInLeg(t *testing.T) {
	_, err := parseTransferLedger(strings.NewReader(exitHeader + ",holding\n" + exitIn + ",SPVX\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2: holding") {
		t.Errorf("err %v, want the row's line and the holding", err)
	}
}

// exitGold loads ledger into a gold where the private-market account,
// nicknamed lp-nick, subscribed to SPVX and the broker later sells part
// of the shares,
// runs the enrichment and lot passes, and dumps the lots they wrote.
// The two sources are of two kinds, so the lot engine pairs no legs.
func exitGold(t *testing.T, ledger string) ([]string, error) {
	t.Helper()
	ctx := context.Background()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The ledger's day, 2098-01-10, is Unix day 46761.
	day := func(n int) int64 { return int64(n) * gold.SecondsPerDay }
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
            VALUES ('pmco', 'carta', '/tmp/a.db', -1, 0, 0), ('brokerco', 'fred', '/tmp/b.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, nickname, account_kind, first_seen_at, last_seen_at)
            VALUES ('pmco', 'LP', 'lp-nick', 'custody', 1, 1), ('brokerco', 'BRK', NULL, 'brokerage', 1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('pmco',     'c1', %d, 'LP',  'SPVX', 'contribution', 'USD', NULL, -400),
            ('brokerco', 's1', %d, 'BRK', 'PUB',  'sell',         'USD', -4,    500);`,
		day(46700), day(46770))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entries, err := parseTransferLedger(strings.NewReader(ledger))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, src := range []string{"pmco", "brokerco"} {
		if _, err := applyTransferLedger(ctx, tx, src, entries[src]); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := spending.RunDeterministicPass(ctx, db, spending.Options{MatchWindowDays: 5, Now: 1}); err != nil {
		t.Fatal(err)
	}
	fill := lots.Fill
	cfg := lots.Config{Method: lots.FIFO, Sources: map[string]lots.SourceConfig{"pmco": {Mode: &fill}, "brokerco": {Mode: &fill}}}
	if _, err := gold.RebuildLots(ctx, db, gold.LotsOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	// The sale relieves the shares at the basis the ledger carries over.
	var basis float64
	if err := db.QueryRowContext(ctx, `SELECT CAST(book_value AS DOUBLE) FROM lot_realized`).Scan(&basis); err != nil || basis != 160 {
		t.Errorf("the sale's basis %v (%v), want 4 of the 10 shares' 400", basis, err)
	}
	var out []string
	for _, q := range []string{
		`SELECT * EXCLUDE (build_id) FROM lots ORDER BY ALL`,
		`SELECT * EXCLUDE (build_id) FROM lot_disposals ORDER BY ALL`,
		`SELECT * FROM lot_realized ORDER BY ALL`,
	} {
		out = append(out, dumpRows(t, db, q)...)
	}
	return out, nil
}

// Naming the holding changes no lot: the broker's shares carry the
// ledger's basis either way.
func TestTransferHoldingKeepsTheLots(t *testing.T) {
	with, err := exitGold(t, exitHeader+",holding\n"+exitOut+",SPVX\n"+exitIn+",\n")
	if err != nil {
		t.Fatal(err)
	}
	without, err := exitGold(t, exitHeader+"\n"+exitOut+"\n"+exitIn+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(with, without) {
		t.Errorf("with holding:\n%s\nwithout:\n%s", strings.Join(with, "\n"), strings.Join(without, "\n"))
	}
}

// A holding no contribution of the out-leg's account names fails the
// load, naming the row by its line. The check reads the account the
// row's nickname resolves to.
func TestTransferHoldingMustBeHeld(t *testing.T) {
	byNick := strings.Replace(exitOut, ",LP,", ",lp-nick,", 1)
	if _, err := exitGold(t, exitHeader+",holding\n"+exitIn+",\n"+byNick+",SPVX\n"); err != nil {
		t.Errorf("the account by its nickname: %v", err)
	}
	for _, c := range []struct{ out, holding, account string }{
		{exitOut, "NOPE", "LP"}, {exitOut, "PUB", "LP"}, {byNick, "NOPE", "lp-nick"},
	} {
		_, err := exitGold(t, exitHeader+",holding\n"+exitIn+",\n"+c.out+","+c.holding+"\n")
		want := `equity_transfers: line 3: the row of account "` + c.account + `" on 2098-01-10: holding "` +
			c.holding + `" is no private holding`
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("holding %s of %s: err %v, want %q", c.holding, c.account, err, want)
		}
	}
}
