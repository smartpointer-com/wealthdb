package main

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

// TestTransactionSpendColumns pins the spending columns migration 0042
// put on report_transactions through to the CLI: they resolve by name
// as a delta on the default set, render the overlay's verdict, and read
// empty for a row the enrichment pass never reached. They are opt-in —
// most of a ledger is investment rows, where all three are blank.
func TestTransactionSpendColumns(t *testing.T) {
	for _, name := range []string{"merchant", "spend_primary", "spend_detailed"} {
		for _, c := range defaultTransactionColumns {
			if c == name {
				t.Errorf("%q is in the default column set; the spending columns are opt-in", name)
			}
		}
	}

	cols, err := resolveTransactionColumns("+merchant,spend_primary,spend_detailed", "USD")
	if err != nil {
		t.Fatalf("resolve spending columns: %v", err)
	}
	byName := map[string]columnSpec[gold.TransactionRow]{}
	for _, c := range cols {
		byName[c.Name] = c
	}

	merchant, primary, detailed := "Corner Market", "FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES"
	categorised := gold.TransactionRow{
		MerchantName: &merchant, SpendPrimary: &primary, SpendDetailed: &detailed,
	}
	for name, want := range map[string]string{
		"merchant": merchant, "spend_primary": primary, "spend_detailed": detailed,
	} {
		c, ok := byName[name]
		if !ok {
			t.Errorf("column %q did not resolve", name)
			continue
		}
		if got := c.Extract(categorised); got != want {
			t.Errorf("column %q = %q, want %q", name, got, want)
		}
		if got := c.Extract(gold.TransactionRow{}); got != "" {
			t.Errorf("column %q on an unenriched row = %q, want empty", name, got)
		}
	}
}

// TestTransactionPrivacyClasses pins the transactions registry against
// the spending one: the same gold column must take the same class on
// both surfaces. `description` is the case that matters — it holds the
// statement narrative verbatim, so it redacts as free text (the cell
// masks whole), and `merchant` goes with it, being a name taken off
// such a narrative; the taxonomy columns beside them stay legible.
func TestTransactionPrivacyClasses(t *testing.T) {
	cols, err := resolveTransactionColumns("all", "USD")
	if err != nil {
		t.Fatalf("resolve columns: %v", err)
	}
	byName := map[string]columnSpec[gold.TransactionRow]{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	want := map[string]PrivacyClass{
		"description":    PrivacyFreeText,
		"merchant":       PrivacyFreeText,
		"spend_primary":  PrivacyNone,
		"spend_detailed": PrivacyNone,
		"asset_class":    PrivacyNone,
		"kind":           PrivacyNone,
		"account":        PrivacyAccountID,
		"account_id":     PrivacyAccountID,
		"tx_id":          PrivacyAccountID,
		"net_amount":     PrivacyMoney,
		"value":          PrivacyMoney,
		"quantity":       PrivacyQuantity,
	}
	for name, class := range want {
		c, ok := byName[name]
		if !ok {
			t.Errorf("column %q is not registered", name)
			continue
		}
		if c.Privacy != class {
			t.Errorf("column %q privacy = %v, want %v", name, c.Privacy, class)
		}
	}
}

// TestTransactionNamePrivacyFollowsFallback pins both halves of the
// `name` column's rule. The column prefers the joined instrument name
// — a public security name, which stays legible under -p — and falls
// back to the statement narrative, which must mask. A single class on
// the column could only get one of the two right.
func TestTransactionNamePrivacyFollowsFallback(t *testing.T) {
	cols, err := resolveTransactionColumns("name,description", "USD")
	if err != nil {
		t.Fatalf("resolve columns: %v", err)
	}
	instrument := "Example Total Market Index ETF"
	narrative := "PAYMENT SAMPLE PAYEE ZZ EXAMPLETOWN REF 000000"

	joined := gold.TransactionRow{Name: &instrument, Description: &narrative}
	fallback := gold.TransactionRow{Description: &narrative}

	on := rowsToTable([]gold.TransactionRow{joined, fallback}, cols, true, output.FormatTable)
	if on.Rows[0][0] != instrument {
		t.Errorf("joined instrument name under -p = %q, want it legible", on.Rows[0][0])
	}
	if on.Rows[1][0] != "***" {
		t.Errorf("narrative fallback under -p = %q, want the free-text placeholder", on.Rows[1][0])
	}
	for i := range on.Rows {
		if on.Rows[i][1] != "***" {
			t.Errorf("description under -p, row %d = %q, want the free-text placeholder", i, on.Rows[i][1])
		}
	}

	off := rowsToTable([]gold.TransactionRow{joined, fallback}, cols, false, output.FormatTable)
	if off.Rows[1][0] != narrative || off.Rows[1][1] != narrative {
		t.Errorf("privacy off, row 1 = %q, want the narrative in both cells", off.Rows[1])
	}
}
