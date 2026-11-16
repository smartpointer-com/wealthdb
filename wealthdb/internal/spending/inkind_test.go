package spending

import (
	"database/sql"
	"testing"
)

// assertLinksSymmetric fails t where a transfer_links row names a
// partner leg whose own row does not name it back, with the far
// account the partner sits on.
func assertLinksSymmetric(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`
        SELECT l.silver_source_id, l.transaction_external_id,
               l.far_silver_source_id, l.far_transaction_external_id
          FROM transfer_links l
          LEFT JOIN transfer_links p
            ON p.silver_source_id = l.far_silver_source_id
           AND p.transaction_external_id = l.far_transaction_external_id
          LEFT JOIN transactions pt
            ON pt.silver_source_id = l.far_silver_source_id
           AND pt.transaction_external_id = l.far_transaction_external_id
         WHERE l.far_transaction_external_id IS NOT NULL
           AND (p.far_silver_source_id IS DISTINCT FROM l.silver_source_id
                OR p.far_transaction_external_id IS DISTINCT FROM l.transaction_external_id
                OR l.far_account_external_id IS DISTINCT FROM pt.account_external_id)
         ORDER BY ALL`)
	if err != nil {
		t.Fatalf("check link symmetry: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, txn, farSrc, farTxn string
		if err := rows.Scan(&src, &txn, &farSrc, &farTxn); err != nil {
			t.Fatal(err)
		}
		t.Errorf("link %s/%s names %s/%s, which does not name it back", src, txn, farSrc, farTxn)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// The two legs of a move one ledger row states link to each other,
// with the ledger's evidence, over the link the matcher's amount guess
// wrote for them. A leg whose partner gold does not hold has none.
func TestStatedMoveLegsLinkEachOther(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount, payload) VALUES
            ('bank', 'xfer:out', ?, 'BRK1', 'VTI', 'transfer_out', 'USD', -10, -900,
             '{"equity_transfer_ledger":true,"pair_silver_source_id":"other-bank","pair_transaction_external_id":"xfer:in"}'),
            ('other-bank', 'xfer:in', ?, 'CUST2', 'VTI', 'transfer_in', 'USD', 10, 900,
             '{"equity_transfer_ledger":true,"pair_silver_source_id":"bank","pair_transaction_external_id":"xfer:out"}'),
            ('bank', 'xfer:half', ?, 'BRK1', 'VTI', 'transfer_out', 'USD', -5, -450,
             '{"equity_transfer_ledger":true,"pair_silver_source_id":"other-bank","pair_transaction_external_id":"xfer:none"}')`,
		day(10), day(10), day(20)); err != nil {
		t.Fatal(err)
	}
	runPass(t, db, ctx, Options{})
	got := scanPairs(t, db, `
        SELECT silver_source_id || '/' || transaction_external_id,
               concat_ws('|', far_silver_source_id, far_account_external_id, far_transaction_external_id,
                         evidence, measure)
          FROM transfer_links WHERE starts_with(transaction_external_id, 'xfer:')`)
	want := map[string]string{
		"bank/xfer:out":      "other-bank|CUST2|xfer:in|equity-ledger|in_kind",
		"other-bank/xfer:in": "bank|BRK1|xfer:out|equity-ledger|in_kind",
	}
	if len(got) != len(want) {
		t.Errorf("links %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: link %q, want %q", k, got[k], v)
		}
	}
	// Idempotent: a second pass writes the same links.
	runPass(t, db, ctx, Options{})
	if again := scanPairs(t, db, `
        SELECT silver_source_id || '/' || transaction_external_id, evidence
          FROM transfer_links WHERE starts_with(transaction_external_id, 'xfer:')`); len(again) != 2 {
		t.Errorf("second pass links %v", again)
	}
	assertLinksSymmetric(t, db)
}

// Where the matcher paired a stated leg with a third leg, the ledger's
// pair wins and the third leg loses its link with it: it reads as an
// own-account move with no far account. Its overlay row keeps the
// matcher's verdict; the links alone answer for the pair.
func TestAStatedPairUnpairsTheMatchersThirdLeg(t *testing.T) {
	db, ctx := openGold(t)
	// The stated receipt's value differs from the departure's, so the
	// amount phase pairs the departure with the cash deposit X instead.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount, payload) VALUES
            ('bank', 'xfer:out', ?, 'BRK1', 'VTI', 'transfer_out', 'USD', -10, -900,
             '{"equity_transfer_ledger":true,"pair_silver_source_id":"other-bank","pair_transaction_external_id":"xfer:in"}'),
            ('other-bank', 'xfer:in', ?, 'CUST2', 'VTI', 'transfer_in', 'USD', 10, 950,
             '{"equity_transfer_ledger":true,"pair_silver_source_id":"bank","pair_transaction_external_id":"xfer:out"}'),
            ('other-bank', 'X', ?, 'CASH2', NULL, 'deposit', 'USD', NULL, 900, NULL)`,
		day(10), day(10), day(10)); err != nil {
		t.Fatal(err)
	}
	runPass(t, db, ctx, Options{})
	if detailed, prov := verdictOf(t, db, ctx, "other-bank", "X"); detailed != internalTransfer ||
		prov != ProvenanceMatcher {
		t.Fatalf("the matcher did not pair X with the departure: (%q, %q)", detailed, prov)
	}
	if src, acct := farOf(t, db, ctx, "other-bank", "X"); src != "" || acct != "" {
		t.Errorf("X keeps a far side (%q, %q); its partner is the ledger's", src, acct)
	}
	got := scanPairs(t, db, `
        SELECT silver_source_id || '/' || transaction_external_id,
               concat_ws('|', far_transaction_external_id, evidence, measure)
          FROM transfer_links`)
	want := map[string]string{
		"bank/xfer:out":      "xfer:in|equity-ledger|in_kind",
		"other-bank/xfer:in": "xfer:out|equity-ledger|in_kind",
	}
	if len(got) != len(want) || got["bank/xfer:out"] != want["bank/xfer:out"] || got["other-bank/xfer:in"] != want["other-bank/xfer:in"] {
		t.Errorf("links %v, want %v", got, want)
	}
	assertLinksSymmetric(t, db)
}
