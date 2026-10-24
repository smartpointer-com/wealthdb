package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// pinLeft says what of a pin the connection still holds: its temporary
// tables and macros. The pool holds one connection, so every read meets
// the one a pin would have been left on.
func pinLeft(t *testing.T, db *sql.DB, ctx context.Context) string {
	t.Helper()
	var tables, macros int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM duckdb_tables() WHERE temporary),
	    (SELECT count(*) FROM duckdb_functions() WHERE database_name = 'temp')`).Scan(&tables, &macros); err != nil {
		t.Fatal(err)
	}
	if tables == 0 && macros == 0 {
		return ""
	}
	return fmt.Sprintf("%d temporary tables, %d temporary macros", tables, macros)
}

// activeCounts are the keys pos_active_at and cash_active_at give at an
// instant.
func activeCounts(t *testing.T, ctx context.Context, q querier, at int64) (pos, cash int) {
	t.Helper()
	if err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pos_active_at([$1])),
	    (SELECT count(*) FROM cash_active_at([$1]))`, at).Scan(&pos, &cash); err != nil {
		t.Fatal(err)
	}
	return pos, cash
}

// TestPinnedKeySpansHoldForTheTransaction: inside the transaction that
// pins them, every point-in-time reader picks from the endings and spans
// as pinned, a position or a balance written after the pin included in
// none, even under a `main.` name. Committed or rolled back, the
// connection keeps nothing of the pin, and the readers compute them again.
func TestPinnedKeySpansHoldForTheTransaction(t *testing.T) {
	db, ctx := openMigrated(t)
	db.SetMaxOpenConns(1)
	pitPosition(t, db, ctx, "pin-src", "A-0000", "AX", pitAt(-10, 12), 100)
	pitCash(t, db, ctx, "pin-src", "A-0000", "USD", pitAt(-10, 12), 10)
	tx, commit, err := beginPinned(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value)
		    VALUES ('pin-src', ?, 'B-0000', 'BX', 'BX', 'public_equity', 'stock', 'USD', 1, 50)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES ('pin-src', ?, 'B-0000', 'USD', 'current', 5)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, pitAt(-5, 12)); err != nil {
			t.Fatal(err)
		}
	}
	if pos, cash := activeCounts(t, ctx, tx, pitAt(0, 12)); pos != 1 || cash != 1 {
		t.Errorf("pinned: %d position keys, %d cash keys; want the 1 and 1 the pin saw", pos, cash)
	}
	var qualified int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM main.pos_key_spans()`).Scan(&qualified); err != nil ||
		qualified != 1 {
		t.Errorf("main.pos_key_spans() reads %d spans (%v), want the pinned 1", qualified, err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	if left := pinLeft(t, db, ctx); left != "" {
		t.Errorf("after commit the connection holds %s", left)
	}
	if pos, cash := activeCounts(t, ctx, db, pitAt(0, 12)); pos != 2 || cash != 2 {
		t.Errorf("committed: %d position keys, %d cash keys; want 2 and 2", pos, cash)
	}

	tx, _, err = beginPinned(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if left := pinLeft(t, db, ctx); left != "" {
		t.Errorf("after a rollback the connection holds %s", left)
	}
}

// TestPinnedKeysReadAsTheLiveOnes: on the book of every ending shape,
// the pinned endings and spans give every reader the answers the live
// macros give: the history, and the picks at each observation, a day,
// 40 days and 400 days after it.
func TestPinnedKeysReadAsTheLiveOnes(t *testing.T) {
	db, ctx := openMigrated(t)
	db.SetMaxOpenConns(1)
	seedPartialRuns(t, db, ctx)
	reads := []string{`SELECT * FROM hist_active_pos()`, `SELECT * FROM hist_active_cash()`}
	for _, picks := range []string{"pos_active_at", "cash_active_at"} {
		reads = append(reads, `SELECT * FROM `+picks+`((SELECT list(DISTINCT snap + d) FROM (
		    SELECT snapshot_at AS snap FROM positions UNION SELECT snapshot_at FROM cash_balances),
		    (VALUES (0), (86400), (40 * 86400), (400 * 86400)) v(d)))`)
	}
	live := make([]string, len(reads))
	for i, r := range reads {
		live[i] = rowsDigest(t, ctx, db, r)
	}
	tx, commit, err := beginPinned(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range reads {
		if got := rowsDigest(t, ctx, tx, r); got != live[i] || got == "" {
			t.Errorf("%s reads otherwise pinned (%d bytes against %d)", r, len(got), len(live[i]))
		}
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
}

// TestAMaterializerLeavesNoPin: every materializer that pins leaves its
// connection as it found it, on an empty gold and on one with a holding,
// and a position written next is read.
func TestAMaterializerLeavesNoPin(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprint("seeded=", seeded), func(t *testing.T) {
			db, ctx := openMigrated(t)
			db.SetMaxOpenConns(1)
			if seeded {
				pitPosition(t, db, ctx, "pin-src", "A-0000", "AX", pitAt(-10, 12), 100)
			}
			at := pitAt(0, 12)
			for name, run := range map[string]func() error{
				"serving":  func() error { _, err := MaterializeServingTables(ctx, db); return err },
				"gains":    func() error { _, err := MaterializeGains(ctx, db, at, at); return err },
				"exposure": func() error { _, err := MaterializeExposure(ctx, db, at, at); return err },
				"yield":    func() error { _, err := MaterializeYield(ctx, db, at, at, 12); return err },
			} {
				if err := run(); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if left := pinLeft(t, db, ctx); left != "" {
					t.Errorf("%s leaves %s", name, left)
				}
			}
			pitPosition(t, db, ctx, "pin-src", "B-0000", "BX", pitAt(-5, 12), 50)
			if pos, _ := activeCounts(t, ctx, db, at); pos != map[bool]int{false: 1, true: 2}[seeded] {
				t.Errorf("a position written after the materializers: %d keys", pos)
			}
		})
	}
}

// TestMigration0145DDLIsRerunnable holds the migration to the replay bar:
// a second application changes no ending, span or day of the history.
// It holds 0144's endings too, which 0145 re-issues.
func TestMigration0145DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedPartialRuns(t, db, ctx)
	const answers = `
        SELECT 'end' AS k, src, acct, NULL AS ccy, end_snap AS snap, NULL AS until FROM pos_key_ends()
        UNION ALL SELECT 'end', src, acct, ccy, end_snap, NULL FROM cash_key_ends()
        UNION ALL SELECT 'span', src, acct, NULL, snap, until FROM pos_key_spans()
        UNION ALL SELECT 'span', src, acct, ccy, snap, until FROM cash_key_spans()
        UNION ALL SELECT 'day', src, acct, NULL, snap, day FROM hist_active_pos()
        UNION ALL SELECT 'day', src, acct, ccy, snap, day FROM hist_active_cash()`
	before := rowsDigest(t, ctx, db, answers)
	rerunMigrationDDL(t, db, ctx, "0145_key_observations.sql")
	if after := rowsDigest(t, ctx, db, answers); after != before || before == "" {
		t.Error("the replay changed an answer")
	}
}

// planBlockwiseJoins counts the joins in a macro's query plan that test
// their condition on every pair of rows, for want of an equality between
// both inputs to hash on.
func planBlockwiseJoins(t *testing.T, db *sql.DB, ctx context.Context, from string) int {
	t.Helper()
	rows, err := db.QueryContext(ctx, `EXPLAIN (FORMAT json) SELECT * FROM `+from)
	if err != nil {
		t.Fatalf("explain %s: %v", from, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var kind, plan string
		if err := rows.Scan(&kind, &plan); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n += strings.Count(plan, `"BLOCKWISE_NL_JOIN"`)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s: %v", from, err)
	}
	return n
}

// TestYieldHoldingsReadsEachKindsOwnRow holds yield_holdings, whose joins
// key on the holding's kind (migration 0145), to 0136's answers: a cash
// holding reads its balance and no position, though a position at the
// same snapshot bears its key, a position reads its own row, and a
// position with no value reads no balance. The kind in the keys is what
// lets DuckDB hash both joins: a condition on the holding alone would
// test every holding against every position.
func TestYieldHoldingsReadsEachKindsOwnRow(t *testing.T) {
	db, ctx := openMigrated(t)
	seedHistAccount(t, db, ctx, "bank", "A1", "brokerage", "Brokerage")
	seedHistCash(t, db, ctx, "bank", "A1", -1, "100")
	for _, p := range []struct {
		key   string
		value any
		qty   string
	}{{"QAAA", "50", "5"}, {"cash:USD", nil, "7"}} {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                                   asset_class, vehicle, currency, market_value, quantity)
            VALUES ('bank', ?, 'A1', ?, 'public_equity', 'stock', 'USD',
                    CAST(? AS DECIMAL(28,4)), CAST(? AS DECIMAL(28,8)))`,
			snapAt(-1), p.key, p.value, p.qty); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	if err := scanAll(ctx, db, `SELECT position_key, is_cash, amount, quantity FROM yield_holdings(?, 'USD')
	    ORDER BY ALL`, func(rows *sql.Rows) error {
		var key string
		var cash bool
		var amount, qty sql.NullFloat64
		if err := rows.Scan(&key, &cash, &amount, &qty); err != nil {
			return err
		}
		a := "NULL"
		if amount.Valid {
			a = fmt.Sprint(amount.Float64)
		}
		got = append(got, fmt.Sprintf("%s cash=%t %s %t", key, cash, a, qty.Valid))
		return nil
	}, snapAt(0)); err != nil {
		t.Fatal(err)
	}
	want := []string{"QAAA cash=false 50 true", "cash:USD cash=false NULL true", "cash:USD cash=true 100 false"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("yield_holdings = %q, want %q", got, want)
	}
	if n := planBlockwiseJoins(t, db, ctx, fmt.Sprintf("yield_holdings(%d, 'USD')", snapAt(0))); n != 0 {
		t.Errorf("yield_holdings plans %d blockwise join(s), want each join on an equality", n)
	}
}
