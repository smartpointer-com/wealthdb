package spending

import (
	"regexp"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestPassMemoNeverFiresABuiltInRule pins the memo's place in the rule
// tier, end to end. The payer's words behind the memo separator can
// say anything — "Hypothekarzins" on a wire to a lender gold does not
// track, "Bancomat" on a payment to a shop — and a built-in that read
// them would place a delta from the payer's vocabulary rather than the
// bank's narrative: the first deletes the row from the base as an
// own-account move, the second files it as cash, both with provenance
// `rule` and nothing in a report to show it. So the built-ins read the
// narrative only, and the rows stay unplaced backlog; a config rule
// reads the memo too, because it is the holder's own local input, and
// places what the holder says. Every value is synthetic.
func TestPassMemoNeverFiresABuiltInRule(t *testing.T) {
	const caption = "EXAMPLE PAYEE EXAMPLE STREET 1 9999 EXAMPLETOWN"
	db, ctx := openGold(t)
	seedSwissBank(t, db, ctx)
	seedTxns(t, db, ctx,
		txn{"swiss-bank", "T-MEMO-MORTGAGE", "CASH3", "withdrawal", day(10), -900,
			"EXAMPLE PAYEE", canonical.JoinDescriptionMemo(caption, "Hypothekarzins Q3"), "e-banking payment order"},
		txn{"swiss-bank", "T-MEMO-ATM", "CASH3", "withdrawal", day(11), -60,
			"EXAMPLE PAYEE", canonical.JoinDescriptionMemo(caption, "Bancomat"), "e-banking payment order"},
		// The narrative itself names the machine: the atm rule fires
		// whatever the memo says.
		txn{"swiss-bank", "T-NARRATIVE-ATM", "CASH3", "withdrawal", day(12), -100,
			"", canonical.JoinDescriptionMemo("Bancomat Main Street", "THANKS"), "NTRF"},
	)

	runPass(t, db, ctx, Options{})
	for _, tc := range []struct{ id, detailed, provenance string }{
		{"T-MEMO-MORTGAGE", "", ProvenanceSignatureOnly},
		{"T-MEMO-ATM", "", ProvenanceSignatureOnly},
		{"T-NARRATIVE-ATM", canonical.SpendDetailedCashWithdrawal, ProvenanceRule},
	} {
		detailed, provenance := verdictOf(t, db, ctx, "swiss-bank", tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.id, detailed, provenance, tc.detailed, tc.provenance)
		}
	}
	base := spendingBaseIDs(t, db, ctx)
	for _, id := range []string{"T-MEMO-MORTGAGE", "T-MEMO-ATM", "T-NARRATIVE-ATM"} {
		if _, ok := base[id]; !ok {
			t.Errorf("%s left the spending base", id)
		}
	}
	// The memo never enters the signature: both memo rows share the
	// caption's key.
	if a, b := signatureOf(t, db, ctx, "swiss-bank", "T-MEMO-MORTGAGE"), signatureOf(t, db, ctx, "swiss-bank", "T-MEMO-ATM"); a != b || a != "EXAMPLE PAYEE EXAMPLE STREET 1 EXAMPLETOWN" {
		t.Errorf("memo rows keyed as %q and %q, want both on the caption's signature", a, b)
	}

	// A config rule keyed on the memo is the holder's own word about
	// the row, and it places.
	rules := []Rule{{regexp.MustCompile(`(?i)hypothekarzins`), canonical.SpendDetailedInternalTransfer}}
	runPass(t, db, ctx, Options{Rules: rules})
	if detailed, provenance := verdictOf(t, db, ctx, "swiss-bank", "T-MEMO-MORTGAGE"); detailed != canonical.SpendDetailedInternalTransfer || provenance != ProvenanceRule {
		t.Errorf("T-MEMO-MORTGAGE with a config rule = (%q, %q), want (internal_transfer, rule)", detailed, provenance)
	}
	if detailed, provenance := verdictOf(t, db, ctx, "swiss-bank", "T-MEMO-ATM"); detailed != "" || provenance != ProvenanceSignatureOnly {
		t.Errorf("T-MEMO-ATM with an unrelated config rule = (%q, %q), want unplaced", detailed, provenance)
	}
	base = spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-MEMO-MORTGAGE"]; ok {
		t.Error("T-MEMO-MORTGAGE is still in the spending base after the config rule placed it as an own-account move")
	}
}
