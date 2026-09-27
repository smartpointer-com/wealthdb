package gold

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestSyncDeclaredAccounts: the dimension holds exactly the declared
// rows after each sync — one removed from the config leaves, one
// renamed takes the new name — and a collected source's rows are never
// touched.
func TestSyncDeclaredAccounts(t *testing.T) {
	db, ctx := openMigrated(t)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC1",
			AccountKind: canonical.AccountKindCash, FirstSeenAt: 1, LastSeenAt: 1,
		}})
	})
	wrapper := canonical.TaxWrapperTaxablePersonal
	declare := func(id, nickname string) canonical.AccountChange {
		row := canonical.AccountChange{
			SilverSourceID: canonical.DeclaredSourceID, AccountExternalID: id,
			AccountKind: canonical.AccountKindCash, TaxWrapper: &wrapper,
			FirstSeenAt: 10, LastSeenAt: 10,
		}
		if nickname != "" {
			row.Nickname = &nickname
		}
		return row
	}
	read := func() map[string]string {
		rows, err := db.QueryContext(ctx, `
            SELECT account_external_id, COALESCE(nickname, '') FROM accounts
             WHERE silver_source_id = ?`, canonical.DeclaredSourceID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, nick string
			if err := rows.Scan(&id, &nick); err != nil {
				t.Fatal(err)
			}
			out[id] = nick
		}
		return out
	}

	n, err := SyncDeclaredAccounts(ctx, db, []canonical.AccountChange{declare("a", "Bank A"), declare("b", "")})
	if err != nil || n != 2 {
		t.Fatalf("first sync: n=%d err=%v", n, err)
	}
	if got := read(); len(got) != 2 || got["a"] != "Bank A" || got["b"] != "" {
		t.Errorf("after the first sync: %v", got)
	}

	n, err = SyncDeclaredAccounts(ctx, db, []canonical.AccountChange{declare("a", "")})
	if err != nil || n != 1 {
		t.Fatalf("second sync: n=%d err=%v", n, err)
	}
	if got := read(); len(got) != 1 || got["a"] != "" {
		t.Errorf("a removed declaration stayed, or a removed nickname did: %v", got)
	}

	var collected int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE silver_source_id = 'test-src'`).
		Scan(&collected); err != nil || collected != 1 {
		t.Errorf("the collected account was touched: n=%d err=%v", collected, err)
	}

	if _, err := SyncDeclaredAccounts(ctx, db, []canonical.AccountChange{{
		SilverSourceID: "test-src", AccountExternalID: "X", AccountKind: canonical.AccountKindCash,
	}}); err == nil {
		t.Error("a row under a collected source was accepted as a declaration")
	}
}
