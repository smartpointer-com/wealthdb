package main

import (
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// TestCollectMerchantCandidatesDropsTheMemo pins the memo contract at
// the third place it matters. A description's memo — the payer's own
// words after canonical.DescriptionMemoSeparator — names no merchant,
// so when the description stands in for a missing counterparty the
// descriptor stops at the separator and the memo never reaches the
// prompt. Candidacy is unaffected: the signature never held the memo.
// Every value is synthetic.
func TestCollectMerchantCandidatesDropsTheMemo(t *testing.T) {
	const narrative = "Blue Harbour Cafe; Harbour Road 3"
	db, ctx := openCategorizeGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description)
             VALUES ('bank', 'T1', ?, 'CASH1', 'withdrawal', 'USD', -40, ?)`,
		10*gold.SecondsPerDay, canonical.JoinDescriptionMemo(narrative, "for the birthday table")); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
	runEnrichment(t, db, ctx)

	cands, _, err := collectMerchantCandidates(ctx, db, spendingCategorizeFamily, config.SpendContextDescriptor, 3, backlogUnplaced)
	if err != nil {
		t.Fatalf("collectMerchantCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Signature != "BLUE HARBOUR CAFE HARBOUR ROAD 3" {
		t.Fatalf("candidates = %v, want the narrative's signature alone", signaturesOf(cands))
	}
	if len(cands[0].Samples) != 1 || cands[0].Samples[0].Descriptor != narrative {
		t.Errorf("descriptor = %v, want the narrative without its memo", cands[0].Samples)
	}
	if p := buildCategorizeUserPrompt(spendingCategorizeFamily, cands, nil, config.SpendContextDescriptor, nil); strings.Contains(p, "birthday") {
		t.Errorf("a memo must never reach the prompt:\n%s", p)
	}
}

// TestCollectMerchantCandidatesRefusesBareFiling pins the filing gate.
// A reference-led order on the UBS web export splits into a memo (the
// reference) and the booking type `order`, so every such row is keyed
// `ORDER` — a word, so not Uninformative, but nothing except the
// bank's own filing of the row. One verdict at that key would cover
// every order the bank ever filed that way, so candidacy refuses it and
// counts it with the uninformative; a narrative that carries more than
// the filing (`credit; Ref 7`) is still a candidate. Every value is
// synthetic.
func TestCollectMerchantCandidatesRefusesBareFiling(t *testing.T) {
	db, ctx := openCategorizeGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('swiss-bank', 'ubs', '/tmp/swiss.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('swiss-bank', 'CASH3', 'cash', 'Swiss cash', 1, 1);
    `); err != nil {
		t.Fatalf("seed ubs source: %v", err)
	}
	for _, row := range []struct {
		id, description, providerCategory string
		amount                            float64
	}{
		{"T-ORDER-1", canonical.JoinDescriptionMemo("order", "EXAMPLEREF1"), "order", -42},
		{"T-ORDER-2", canonical.JoinDescriptionMemo("order", "EXAMPLEREF2"), "order", -17},
		{"T-CREDIT", "credit; Ref 7", "credit", -9},
	} {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                      account_external_id, kind, currency, net_amount,
                                      description, provider_category)
                 VALUES ('swiss-bank', ?, ?, 'CASH3', 'withdrawal', 'CHF', ?, ?, ?)`,
			row.id, 20*gold.SecondsPerDay, row.amount, row.description, row.providerCategory); err != nil {
			t.Fatalf("seed transaction %s: %v", row.id, err)
		}
	}
	runEnrichment(t, db, ctx)

	for _, all := range []bool{false, true} {
		cands, skipped, err := collectMerchantCandidates(ctx, db, spendingCategorizeFamily, config.SpendContextMerchant, 3, backlogOf(all, false))
		if err != nil {
			t.Fatalf("collectMerchantCandidates(all=%v): %v", all, err)
		}
		if got := signaturesOf(cands); len(got) != 1 || got[0] != "CREDIT REF 7" {
			t.Errorf("all=%v candidates = %v, want just CREDIT REF 7: a signature that is nothing but the bank's filing names no merchant", all, got)
		}
		if skipped.Uninformative != 1 {
			t.Errorf("all=%v uninformative = %d, want 1 (ORDER, counted once however many rows share it)", all, skipped.Uninformative)
		}
	}
}
