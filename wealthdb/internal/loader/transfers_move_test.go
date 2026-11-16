package loader

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// A move stated in one ledger row and in its two-row twin gives the
// same transactions, lots, disposals and realized lots, once the
// enrichment pass and the lot pass have run. Only the payload's pair
// keys tell the two apart. Every id and figure is invented.
func TestOneRowMoveEqualsItsTwoRowTwin(t *testing.T) {
	const header = "silver_source_id,account,occurred_at,direction,quantity,cost_basis,value,currency,instrument,note"
	one := header + ",to_silver_source_id,to_account\n" +
		"brokerco,BRK,2098-01-10,out,4,40,60,USD,XYZ,a move,trustco,TRUST\n"
	two := header + "\n" +
		"brokerco,BRK,2098-01-10,out,4,40,60,USD,XYZ,a move\n" +
		"trustco,TRUST,2098-01-10,in,4,40,60,USD,XYZ,a move\n"
	a, b := moveGold(t, one), moveGold(t, two)
	if !slices.Equal(a, b) {
		t.Errorf("one row:\n%s\ntwo rows:\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
}

// moveGold loads ledger into a gold holding a buy and a later sale of
// the moved shares, runs both passes, and dumps what they wrote.
func moveGold(t *testing.T, ledger string) []string {
	t.Helper()
	ctx := context.Background()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The ledger's day, 2098-01-10, is Unix day 46761: the buy is
	// before it, the sale and the snapshot after.
	day := func(n int) int64 { return int64(n) * gold.SecondsPerDay }
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
            VALUES ('brokerco', 'fred', '/tmp/a.db', -1, 0, 0), ('trustco', 'fred', '/tmp/b.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, first_seen_at, last_seen_at)
            VALUES ('brokerco', 'BRK', 'brokerage', 1, 1), ('trustco', 'TRUST', 'brokerage', 1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('brokerco', 'b1', %d, 'BRK',   'XYZ', 'buy',  'USD', 10, -100),
            ('trustco',  's1', %d, 'TRUST', 'XYZ', 'sell', 'USD', -4,   80);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, currency, quantity, market_value) VALUES
            ('brokerco', %d, 'BRK', 'XYZ', 'XYZ', 'public_equity', 'USD', 6, 90);`,
		day(46700), day(46770), day(46780))); err != nil {
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
	for _, src := range []string{"brokerco", "trustco"} {
		if _, err := applyTransferLedger(ctx, tx, src, entries[src]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := spending.RunDeterministicPass(ctx, db, spending.Options{MatchWindowDays: 5, Now: 1}); err != nil {
		t.Fatal(err)
	}
	fill := lots.Fill
	cfg := lots.Config{Method: lots.FIFO, Sources: map[string]lots.SourceConfig{"brokerco": {Mode: &fill}, "trustco": {Mode: &fill}}}
	if _, err := gold.RebuildLots(ctx, db, gold.LotsOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	// The sale relieves the lot the move carried over, at its cost.
	var basis float64
	if err := db.QueryRowContext(ctx, `SELECT CAST(book_value AS DOUBLE) FROM lot_realized`).Scan(&basis); err != nil || basis != 40 {
		t.Errorf("the sale's basis %v (%v), want the moved lot's 40", basis, err)
	}
	var out []string
	for _, q := range []string{
		`SELECT * EXCLUDE (payload), json_remove(json_remove(payload, '$.pair_silver_source_id'), '$.pair_transaction_external_id')
           FROM transactions ORDER BY ALL`,
		`SELECT * EXCLUDE (build_id) FROM lots ORDER BY ALL`,
		`SELECT * EXCLUDE (build_id) FROM lot_disposals ORDER BY ALL`,
		`SELECT * FROM lot_realized ORDER BY ALL`,
		`SELECT silver_source_id, account_external_id, snapshot_at, book_value FROM positions ORDER BY ALL`,
	} {
		out = append(out, dumpRows(t, db, q)...)
	}
	return out
}

func dumpRows(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals...))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
