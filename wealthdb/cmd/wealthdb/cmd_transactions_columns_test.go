package main

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// TestTransactionSpendColumns pins the spending columns migration 0042
// put on report_transactions through to the CLI: they resolve by name
// as a delta on the default set, render the overlay's verdict, and read
// empty for a row the enrichment pass never reached. They are opt-in —
// most of a ledger is investment rows, where all three are blank.
func TestTransactionSpendColumns(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"merchant", "spend_primary", "spend_detailed"} {
		for _, c := range defaultTransactionColumns {
			if c == name {
				t.Errorf("%q is in the default column set; the spending columns are opt-in", name)
			}
		}
	}

	cols, err := resolveColumns("+merchant,spend_primary,spend_detailed", defaultTransactionColumns, buildTransactionColumnRegistry("USD"))
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
	t.Parallel()
	cols, err := resolveColumns("all", defaultTransactionColumns, buildTransactionColumnRegistry("USD"))
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
	t.Parallel()
	cols, err := resolveColumns("name,description", defaultTransactionColumns, buildTransactionColumnRegistry("USD"))
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

// TestTransactionsCarriesTheIncomeTrio pins migration 0071's addition
// to `wealthdb transactions`: the income columns are in the registry,
// off by default, and carry the privacy classes their contents need.
func TestTransactionsCarriesTheIncomeTrio(t *testing.T) {
	t.Parallel()
	all, err := resolveColumns("all", defaultTransactionColumns, buildTransactionColumnRegistry("USD"))
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	want := map[string]PrivacyClass{
		"payer":           PrivacyFreeText,
		"income_primary":  PrivacyNone,
		"income_detailed": PrivacyNone,
	}
	seen := map[string]bool{}
	for _, c := range all {
		if class, ok := want[c.Name]; ok {
			seen[c.Name] = true
			if c.Privacy != class {
				t.Errorf("%s: privacy = %v, want %v", c.Name, c.Privacy, class)
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("column %q is missing from the registry", name)
		}
	}

	// Defaults are unchanged: the trio is available through -C and is
	// not forced on every reader.
	def, err := resolveColumns("default", defaultTransactionColumns, buildTransactionColumnRegistry("USD"))
	if err != nil {
		t.Fatalf("default columns: %v", err)
	}
	for _, c := range def {
		if _, isIncome := want[c.Name]; isIncome {
			t.Errorf("%q is on by default; the income trio is opt-in", c.Name)
		}
	}
}

// TestTransactionCheckNumberColumn pins the cheque-number column
// migration 0075 put on report_transactions through to the CLI: opt-in,
// rendered verbatim, empty on the overwhelming majority of rows that
// carry no cheque, and masked by -p.
//
// The privacy class is the part worth pinning. A cheque number names no
// third party, so it is not free text — but it IS an identifier tied to
// the holder's own account, and printing it beside a masked account in
// a redacted readout would undo the masking around it.
func TestTransactionCheckNumberColumn(t *testing.T) {
	t.Parallel()
	for _, c := range defaultTransactionColumns {
		if c == "check_no" {
			t.Error("check_no is in the default column set; it is opt-in like the other identifiers")
		}
	}

	cols, err := resolveColumns("+check_no", defaultTransactionColumns, buildTransactionColumnRegistry("USD"))
	if err != nil {
		t.Fatalf("resolve check_no: %v", err)
	}
	var spec *columnSpec[gold.TransactionRow]
	for i := range cols {
		if cols[i].Name == "check_no" {
			spec = &cols[i]
		}
	}
	if spec == nil {
		t.Fatal("check_no did not resolve")
	}
	if spec.Privacy != PrivacyAccountID {
		t.Errorf("check_no privacy = %v, want PrivacyAccountID", spec.Privacy)
	}
	if spec.Align != output.AlignLeft {
		t.Errorf("check_no align = %v, want AlignLeft — it is an identifier, not a number", spec.Align)
	}

	number := "9042"
	if got := spec.Extract(gold.TransactionRow{CheckNumber: &number}); got != number {
		t.Errorf("check_no on a cheque = %q, want %q", got, number)
	}
	if got := spec.Extract(gold.TransactionRow{}); got != "" {
		t.Errorf("check_no on a row with no cheque = %q, want empty", got)
	}
}
