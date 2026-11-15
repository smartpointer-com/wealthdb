package angellist

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// assertAdvice checks that the connection names exactly the missing parts
// for the silver at path.
func assertAdvice(t *testing.T, conn silver.Connection, path string, version int, missing ...string) {
	t.Helper()
	notes := conn.(silver.SchemaAdviser).SchemaAdvice()
	if len(notes) != 1 || notes[0].Silver != filepath.Base(path) || notes[0].Version != version ||
		!slices.Equal(notes[0].Missing, missing) {
		t.Errorf("advice = %+v, want one note at version %d naming %v", notes, version, missing)
	}
}

// A silver older than migration 0006 holds no funding ledger: it loads its
// positions, reports no transaction range, yields no transactions and no
// cash, and the load names both tables.
func TestASilverWithoutTheFundingLedgerLoadsItsPositions(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	silvertest.Exec(t, db, `DROP TABLE funding_transactions; DROP TABLE funding_accounts;
        DELETE FROM schema_meta; INSERT INTO schema_meta (silver_schema_version) VALUES (5)`)
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestSnapshotAt != 100 || s.LatestSnapshotAt != 1000 ||
		s.OldestTransactionAt != -1 || s.LatestTransactionAt != -1 {
		t.Errorf("Status = %+v, want snapshots [100,1000] and no transaction range", s)
	}
	w := silvertest.WindowSince0(t, conn)
	if n := len(silvertest.Snapshots(t, conn, w)); n != 4 {
		t.Errorf("snapshot batches = %d, want 4 (event dates 100/200/300/1000)", n)
	}
	if cbs := silvertest.Merged(t, conn, w).CashBalances; len(cbs) != 0 {
		t.Errorf("cash balances = %v, want none", cbs)
	}
	if txs := silvertest.Transactions(t, conn, w); len(txs) != 0 {
		t.Errorf("transactions = %d, want none", len(txs))
	}
	assertAdvice(t, conn, path, 5, "funding_accounts", "funding_transactions",
		"funding_transactions.position_external_id")
}

// A silver older than migration 0007 holds the funding ledger without the
// position link: every flow loads unlinked, the cash balance still loads,
// no exited instrument is derived, and the load names the column.
func TestASilverWithoutTheFundingLinkLoadsTheLedgerUnlinked(t *testing.T) {
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `
        ALTER TABLE funding_transactions DROP COLUMN position_external_id;
        DELETE FROM schema_meta; INSERT INTO schema_meta (silver_schema_version) VALUES (6);
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name) VALUES ('pos-x', 'spv', 'X');
        INSERT INTO funding_accounts(funding_account_external_id, currency, balance_minor)
            VALUES ('fa1', 'USD', 4000);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type, amount_minor, currency) VALUES
            ('t1', 100, 'deposit',       10000, 'USD'),
            ('t2', 300, 'disbursement',  -6000, 'USD')`)
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestTransactionAt != 100 || s.LatestTransactionAt != 300 {
		t.Errorf("tx range = [%d,%d], want [100,300]", s.OldestTransactionAt, s.LatestTransactionAt)
	}
	w := silvertest.WindowSince0(t, conn)
	txs := silvertest.Transactions(t, conn, w)
	if len(txs) != 2 {
		t.Fatalf("transactions = %d, want 2", len(txs))
	}
	for _, tx := range txs {
		if tx.InstrumentExternalID != nil {
			t.Errorf("%s instrument = %q, want none", tx.TransactionExternalID, *tx.InstrumentExternalID)
		}
	}
	if txs[1].Kind != canonical.TxKindDistribution {
		t.Errorf("unlinked disbursement kind = %q, want distribution", txs[1].Kind)
	}
	sb := silvertest.Merged(t, conn, w)
	if len(sb.CashBalances) != 1 || sb.CashBalances[0].SnapshotAt != 300 {
		t.Errorf("cash balances = %+v, want one at 300", sb.CashBalances)
	}
	if len(sb.Instruments) != 0 {
		t.Errorf("instruments = %+v, want none", sb.Instruments)
	}
	assertAdvice(t, conn, path, 6, "funding_transactions.position_external_id")
}
