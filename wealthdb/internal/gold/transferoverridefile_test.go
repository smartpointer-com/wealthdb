package gold

import (
	"strings"
	"testing"
)

// The override ledger is hand-written, so the parser's job is to accept
// what a person plausibly types and to refuse — by line — what it cannot
// act on. Every value here is invented.
func TestParseTransferOverrideLedger(t *testing.T) {
	const header = "verb,silver_source_id,account,occurred_at,amount,currency," +
		"silver_source_id_b,account_b,occurred_at_b,amount_b,currency_b,note\n"

	t.Run("a one-leg unmatch isolates its row", func(t *testing.T) {
		got, err := parseTransferOverrideLedger(strings.NewReader(header +
			"unmatch,bank,CHECKING,2099-02-01,-1234.56,USD,,,,,,a cheque is spending\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d rules, want 1", len(got))
		}
		r := got[0]
		if r.Verb != "unmatch" || r.B != nil {
			t.Errorf("got verb %q with B=%v, want a one-leg unmatch", r.Verb, r.B)
		}
		if r.A.Source != "bank" || r.A.Account != "CHECKING" ||
			r.A.Amount != -1234.56 || r.A.Currency != "USD" {
			t.Errorf("first leg = %+v", r.A)
		}
		if r.Note != "a cheque is spending" {
			t.Errorf("note = %q", r.Note)
		}
	})

	t.Run("a two-leg match carries both legs", func(t *testing.T) {
		got, err := parseTransferOverrideLedger(strings.NewReader(header +
			"match,bank,CHECKING,2099-03-10,-10000.00,USD,exchange,Wallet,2099-03-04,10000.00,USD,late ACH\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got[0].B == nil {
			t.Fatal("second leg was dropped")
		}
		if got[0].B.Source != "exchange" || got[0].B.Amount != 10000.00 {
			t.Errorf("second leg = %+v", *got[0].B)
		}
		if got[0].A.Day == got[0].B.Day {
			t.Error("the two legs should land on different days")
		}
	})

	t.Run("the verb decides whether one leg is enough", func(t *testing.T) {
		_, err := parseTransferOverrideLedger(strings.NewReader(header +
			"match,bank,CHECKING,2099-02-01,-100.00,USD,,,,,,\n"))
		if err == nil || !strings.Contains(err.Error(), "both legs") {
			t.Errorf("a one-leg match must be refused, got %v", err)
		}
	})

	t.Run("an error names the line it is on", func(t *testing.T) {
		_, err := parseTransferOverrideLedger(strings.NewReader(header +
			"unmatch,bank,CHECKING,2099-02-01,-100.00,USD,,,,,,fine\n" +
			"sideways,bank,CHECKING,2099-02-02,-100.00,USD,,,,,,\n"))
		if err == nil || !strings.Contains(err.Error(), "line 3") {
			t.Errorf("want the offending line named, got %v", err)
		}
	})

	t.Run("columns are found by name, not position", func(t *testing.T) {
		got, err := parseTransferOverrideLedger(strings.NewReader(
			"note,amount,currency,occurred_at,account,silver_source_id,verb\n" +
				"reordered,-42.00,CHF,2099-04-01,SAVINGS,bank,unmatch\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got[0].A.Account != "SAVINGS" || got[0].A.Amount != -42.00 {
			t.Errorf("header order changed the reading: %+v", got[0].A)
		}
	})

	t.Run("a missing required column is refused by name", func(t *testing.T) {
		_, err := parseTransferOverrideLedger(strings.NewReader(
			"verb,silver_source_id,account,occurred_at,amount\n"))
		if err == nil || !strings.Contains(err.Error(), "currency") {
			t.Errorf("want the missing column named, got %v", err)
		}
	})

	t.Run("blank lines and an empty ledger are not errors", func(t *testing.T) {
		got, err := parseTransferOverrideLedger(strings.NewReader(header +
			"\n" + "unmatch,bank,CHECKING,2099-02-01,-1.00,USD,,,,,,\n" + "\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("got %d rules, want 1", len(got))
		}
		if empty, err := parseTransferOverrideLedger(strings.NewReader("")); err != nil || empty != nil {
			t.Errorf("an empty ledger = %v, %v; want nil, nil", empty, err)
		}
	})
}

// A missing ledger is the normal case — the override surface is opt-in —
// and must not be reported as a failure.
func TestParseTransferOverrideLedgerMissingFileIsNotAnError(t *testing.T) {
	got, err := ParseTransferOverrideLedger(t.TempDir() + "/absent.csv")
	if err != nil || got != nil {
		t.Errorf("got %v, %v; want nil, nil", got, err)
	}
}
