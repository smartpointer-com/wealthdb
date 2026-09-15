package gold

import (
	"context"
	"database/sql"
	"testing"
)

// seedCheckFixture: two outflows on one account, one of them a paper
// cheque, plus a neighbour on either side of the new column so a
// mis-ordered scan shows up as a crossed value rather than an error.
func seedCheckFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              first_seen_at, last_seen_at)
        VALUES ('chk-src', 'CASH1', 'cash', 0, 0);

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount,
                                  description, check_number)
        VALUES ('chk-src', 'T-CHEQUE', 1000, 'CASH1', 'withdrawal', 'USD', -250,
                'Check 9042', '9042');

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount,
                                  description)
        VALUES ('chk-src', 'T-PLAIN', 1001, 'CASH1', 'withdrawal', 'USD', -75,
                'Corner Market');
    `); err != nil {
		t.Fatalf("seed check fixture: %v", err)
	}
}

// TestTransactionsCarriesTheCheckNumberInOrder is the positional-scan
// guard for the column migration 0075 added.
//
// report_transactions is scanned POSITIONALLY, so the row struct, the
// scan list and the macro projection are one contract. A column
// inserted in the wrong slot does not fail — it silently swaps two
// same-typed VARCHARs — so this reads the value back AND checks the
// neighbours either side of it still land where they belong.
func TestTransactionsCarriesTheCheckNumberInOrder(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCheckFixture(t, db, ctx)

	rows, err := TransactionsBetween(ctx, db, 0, 5000, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	byID := map[string]TransactionRow{}
	for _, r := range rows {
		byID[r.TransactionExternalID] = r
	}

	cheque, ok := byID["T-CHEQUE"]
	if !ok {
		t.Fatal("the cheque row is missing from report_transactions")
	}
	if cheque.CheckNumber == nil || *cheque.CheckNumber != "9042" {
		t.Errorf("check_number = %v, want 9042", cheque.CheckNumber)
	}
	// The neighbours. income_detailed sits immediately before the new
	// column and value_outccy immediately after it: if the insertion
	// shifted the projection, one of these carries the cheque number.
	if cheque.IncomeDetailed != nil {
		t.Errorf("income_detailed = %q on a cheque row; the scan is shifted",
			*cheque.IncomeDetailed)
	}
	if cheque.ValueOutCcy == nil || *cheque.ValueOutCcy != "-250" {
		t.Errorf("value_outccy = %v, want -250; the scan is shifted", cheque.ValueOutCcy)
	}
	if cheque.Description == nil || *cheque.Description != "Check 9042" {
		t.Errorf("description = %v, want the raw bank text", cheque.Description)
	}

	// A row with no cheque keeps nil — the column is nullable and
	// nearly always null, which is the shape every consumer must expect.
	plain, ok := byID["T-PLAIN"]
	if !ok {
		t.Fatal("the plain withdrawal is missing")
	}
	if plain.CheckNumber != nil {
		t.Errorf("a withdrawal with no cheque carries check_number %q", *plain.CheckNumber)
	}
}

// TestMigration0075DDLIsRerunnable replays the migration over an
// already-migrated database, which is what gold.Migrate's REPLAY note
// requires of every migration body.
func TestMigration0075DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCheckFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0075_transactions_check_number.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM report_transactions(0, 5000, 'USD')").Scan(&n); err != nil {
		t.Fatalf("report_transactions after the re-run: %v", err)
	}
	if n == 0 {
		t.Error("the re-run left the macro answering nothing")
	}
	// The ALTER is IF NOT EXISTS, so the replay must not have dropped
	// the value the column already held.
	rows, err := TransactionsBetween(ctx, db, 0, 5000, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween after the re-run: %v", err)
	}
	for _, r := range rows {
		if r.TransactionExternalID == "T-CHEQUE" {
			if r.CheckNumber == nil || *r.CheckNumber != "9042" {
				t.Errorf("after the re-run check_number = %v, want 9042", r.CheckNumber)
			}
			return
		}
	}
	t.Error("the cheque row vanished across the re-run")
}
