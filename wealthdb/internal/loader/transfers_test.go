package loader

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func TestParseTransferLedger(t *testing.T) {
	csv := `silver_source_id,account,occurred_at,direction,quantity,cost_basis,value,currency,instrument,note
brokerco,Brokerage,2020-12-31,in,100,50,12000,USD,VTI,example transfer-in

brokerco,Account B,2021-06-30,out,20,10,3000,USD,VTI,example transfer-out
trustco,Trust,2025-03-31,in,0,0,0,USD,QQQ,placeholder to fill in
`
	got, err := parseTransferLedger(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got["brokerco"]) != 2 {
		t.Fatalf("brokerco rows = %d, want 2 (blank line skipped)", len(got["brokerco"]))
	}
	if len(got["trustco"]) != 1 {
		t.Fatalf("trustco rows = %d, want 1", len(got["trustco"]))
	}
	e := got["brokerco"][0]
	if e.Account != "Brokerage" || e.Direction != "in" || e.Quantity != 100 || e.CostBasis != 50 || e.Value != 12000 || e.Instrument != "VTI" {
		t.Errorf("row 0 mis-parsed: %+v", e)
	}
	// 0-value placeholder is allowed (a not-yet-known transfer books nothing).
	if p := got["trustco"][0]; p.Value != 0 || p.Quantity != 0 || p.CostBasis != 0 {
		t.Errorf("placeholder should be all-zero, got %+v", p)
	}
}

func TestParseTransferLedgerColumnOrderIndependent(t *testing.T) {
	csv := `note,value,currency,direction,occurred_at,account,silver_source_id
x,1000,usd,IN,2024-01-02,A,brokerco
`
	got, err := parseTransferLedger(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := got["brokerco"][0]
	if e.Account != "A" || e.Value != 1000 || e.Direction != "in" || e.Currency != "USD" {
		t.Errorf("reordered/cased columns mis-parsed: %+v", e)
	}
}

func TestParseTransferLedgerErrors(t *testing.T) {
	cases := map[string]string{
		"missing required column": "silver_source_id,account,occurred_at,direction,value\nbrokerco,A,2024-01-01,in,1\n",
		"bad direction":           "silver_source_id,account,occurred_at,direction,value,currency\nbrokerco,A,2024-01-01,sideways,1,USD\n",
		"bad date":                "silver_source_id,account,occurred_at,direction,value,currency\nbrokerco,A,01/02/2024,in,1,USD\n",
		"bad currency":            "silver_source_id,account,occurred_at,direction,value,currency\nbrokerco,A,2024-01-01,in,1,US\n",
		"negative value":          "silver_source_id,account,occurred_at,direction,value,currency\nbrokerco,A,2024-01-01,in,-1,USD\n",
		"missing account":         "silver_source_id,account,occurred_at,direction,value,currency\nbrokerco,,2024-01-01,in,1,USD\n",
	}
	for name, csv := range cases {
		if _, err := parseTransferLedger(strings.NewReader(csv)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseTransferLedgerMissingFile(t *testing.T) {
	got, err := ParseTransferLedger("/no/such/ledger.csv")
	if err != nil || got != nil {
		t.Errorf("missing file should be (nil, nil); got (%v, %v)", got, err)
	}
	if got, err := ParseTransferLedger(""); err != nil || got != nil {
		t.Errorf("empty path should be (nil, nil); got (%v, %v)", got, err)
	}
}

func TestTransferEntryToChange(t *testing.T) {
	in := TransferEntry{SilverSourceID: "brokerco", Account: "H", OccurredAt: 1609372800, Direction: "in", Value: 12000, Currency: "USD", Quantity: 100, CostBasis: 50, Instrument: "VTI", Note: "x"}
	ch := in.toChange("H")
	if ch.Kind != canonical.TxKindTransferIn {
		t.Errorf("in → %q, want transfer_in", ch.Kind)
	}
	if ch.NetAmount == nil || ch.NetAmount.InexactFloat64() <= 0 {
		t.Errorf("transfer_in net_amount should be positive, got %v", ch.NetAmount)
	}
	if !strings.HasPrefix(ch.TransactionExternalID, transferIDPrefix) {
		t.Errorf("id %q lacks the ledger prefix", ch.TransactionExternalID)
	}
	if !strings.Contains(string(ch.Payload), `"cost_basis":50`) {
		t.Errorf("payload should carry cost_basis, got %s", ch.Payload)
	}
	// Out flips the sign; id is stable across value edits (keyed on content, not amount).
	out := in
	out.Direction = "out"
	co := out.toChange("H")
	if co.Kind != canonical.TxKindTransferOut || co.NetAmount.InexactFloat64() >= 0 {
		t.Errorf("transfer_out should be negative, got kind=%q net=%v", co.Kind, co.NetAmount)
	}
	edited := in
	edited.Value = 99999
	if edited.toChange("H").TransactionExternalID != ch.TransactionExternalID {
		t.Error("editing value must not change the synthetic id (so re-load updates in place)")
	}
}

func TestApplyTransferLedger(t *testing.T) {
	ctx := context.Background()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("gold.OpenFresh: %v", err)
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	nick, usd := "Brokerage", "USD"
	if err := gold.NewWriter(tx).UpsertAccounts(ctx, []canonical.AccountChange{{
		SilverSourceID: "brokerco", AccountExternalID: "HASH1", AccountKind: canonical.AccountKindBrokerage,
		Nickname: &nick, BaseCurrency: &usd, FirstSeenAt: 1000, LastSeenAt: 2000,
	}}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	entries := []TransferEntry{
		{SilverSourceID: "brokerco", Account: "HASH1", OccurredAt: 1609372800, Direction: "in", Value: 12000, Currency: "USD", Quantity: 100, Instrument: "VTI"},
		{SilverSourceID: "brokerco", Account: "Brokerage", OccurredAt: 1625011200, Direction: "out", Value: 3000, Currency: "USD", Quantity: 20, Instrument: "VTI"}, // by nickname
	}
	n, err := applyTransferLedger(ctx, tx, "brokerco", entries)
	if err != nil {
		t.Fatalf("applyTransferLedger: %v", err)
	}
	if n != 2 {
		t.Fatalf("inserted %d, want 2", n)
	}
	// Both resolved onto the same account; ids carry the ledger prefix.
	var cnt int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transactions WHERE silver_source_id='brokerco' AND account_external_id='HASH1' AND transaction_external_id LIKE ?`,
		transferIDPrefix+"%").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Errorf("ledger transactions on HASH1 = %d, want 2 (nickname resolved)", cnt)
	}

	// Idempotent re-apply: replaces rather than duplicates.
	if _, err := applyTransferLedger(ctx, tx, "brokerco", entries); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM transactions WHERE transaction_external_id LIKE ?`, transferIDPrefix+"%").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Errorf("after re-apply count = %d, want 2 (no duplicates)", cnt)
	}

	// Unknown account is a clear error.
	if _, err := applyTransferLedger(ctx, tx, "brokerco", []TransferEntry{
		{SilverSourceID: "brokerco", Account: "Nope", OccurredAt: 1609372800, Direction: "in", Value: 1, Currency: "USD"},
	}); err == nil {
		t.Error("unknown account should error")
	}

	// Empty ledger clears the source's prior ledger rows.
	if _, err := applyTransferLedger(ctx, tx, "brokerco", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM transactions WHERE transaction_external_id LIKE ?`, transferIDPrefix+"%").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("empty ledger should clear prior rows, got %d", cnt)
	}
}
