package ubs

import (
	"context"
	"database/sql"
	"testing"
)

// Which safekeeping account a printed Statement of assets belongs to is
// the one question the document itself cannot answer: it names its
// portfolio and nothing else. PSN's roster answers it when a portfolio
// has one safekeeping account; the tests below are about what happens
// when it has more.

const (
	sbpPortfolio = "0000000000000001"
	sbpOther     = "0000000000000002"
	sbpHolder    = "00000000000000S1" // PSN reports holdings for it
	sbpQuiet     = "00000000000000S2" // PSN has never reported one
)

func seedSafekeeping(t *testing.T, db *sql.DB, snapshot int64, portfolio, account string) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO safekeeping_accounts (snapshot_at, relationship_id,
            account_external_id, portfolio_external_id, payload)
        VALUES (?, 'R1', ?, ?, json_object('PrtflId', ?))`,
		snapshot, account, portfolio, portfolio); err != nil {
		t.Fatalf("seed safekeeping account %s: %v", account, err)
	}
}

func seedHolding(t *testing.T, db *sql.DB, snapshot int64, account, isin string) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO holdings (snapshot_at, relationship_id,
            safekeeping_external_id, isin, payload)
        VALUES (?, 'R1', ?, ?, '{}')`, snapshot, account, isin); err != nil {
		t.Fatalf("seed holding on %s: %v", account, err)
	}
}

func safekeepingMap(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	got, err := (&psnReader{db: db}).safekeepingByPortfolio(context.Background())
	if err != nil {
		t.Fatalf("safekeepingByPortfolio: %v", err)
	}
	return got
}

// One safekeeping account, one portfolio: the answer has never been in
// doubt and must not become so.
func TestOneSafekeepingAccountMapsStraightThrough(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedSafekeeping(t, db, 1, sbpPortfolio, sbpHolder)
	seedHolding(t, db, 1, sbpHolder, "XX0000000001")

	if got := safekeepingMap(t, db)[sbpPortfolio]; got != sbpHolder {
		t.Errorf("portfolio maps to %q, want the one account it has", got)
	}
}

// A safekeeping account need not hold securities: UBS opens one per
// service line, and a portfolio can carry one that never holds paper
// beside one that does. PSN reports a position against the account
// that owns it, so that is what tells them apart.
func TestASecondAccountThatHoldsNothingIsNotACandidate(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedSafekeeping(t, db, 1, sbpPortfolio, sbpHolder)
	seedSafekeeping(t, db, 1, sbpPortfolio, sbpQuiet)
	seedHolding(t, db, 1, sbpHolder, "XX0000000001")

	if got := safekeepingMap(t, db)[sbpPortfolio]; got != sbpHolder {
		t.Errorf("portfolio maps to %q, want the one PSN reports holdings for", got)
	}
}

// An account that held paper once is still an account that holds
// securities, even if the latest snapshot shows it empty — so the
// witness is every snapshot, not the newest.
func TestAnAccountThatHeldPaperOnlyOnceStillCounts(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedSafekeeping(t, db, 9, sbpPortfolio, sbpHolder)
	seedSafekeeping(t, db, 9, sbpPortfolio, sbpQuiet)
	seedHolding(t, db, 1, sbpHolder, "XX0000000001") // long ago
	seedHolding(t, db, 9, sbpQuiet, "XX0000000002")  // and today

	if got, ok := safekeepingMap(t, db)[sbpPortfolio]; ok {
		t.Errorf("portfolio maps to %q, want the ambiguity left unresolved", got)
	}
}

// Two accounts that could equally hold the security is the case the
// overlay exists for. Narrowing must not turn a genuine ambiguity into
// a guess.
func TestTwoAccountsThatBothHoldSecuritiesStayAmbiguous(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedSafekeeping(t, db, 1, sbpPortfolio, sbpHolder)
	seedSafekeeping(t, db, 1, sbpPortfolio, sbpQuiet)
	seedHolding(t, db, 1, sbpHolder, "XX0000000001")
	seedHolding(t, db, 1, sbpQuiet, "XX0000000002")

	if got, ok := safekeepingMap(t, db)[sbpPortfolio]; ok {
		t.Errorf("portfolio maps to %q, want no mapping at all", got)
	}
}

// A load whose PSN side carries no holdings yet — the roster arrives
// before the first MT535 batch — must resolve exactly as it did before
// holdings were consulted, or a quiet account would stop mapping.
func TestNoHoldingsAtAllLeavesTheRosterAnswerAlone(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedSafekeeping(t, db, 1, sbpPortfolio, sbpHolder)
	seedSafekeeping(t, db, 1, sbpOther, sbpQuiet)

	got := safekeepingMap(t, db)
	if got[sbpPortfolio] != sbpHolder || got[sbpOther] != sbpQuiet {
		t.Errorf("map = %v, want each single-account portfolio mapped", got)
	}
}

// Narrowing is per portfolio: holdings elsewhere in the relationship
// say nothing about this portfolio's accounts, and must not empty its
// candidate list.
func TestHoldingsOnAnotherPortfolioDoNotNarrowThisOne(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedSafekeeping(t, db, 1, sbpPortfolio, sbpHolder)
	seedSafekeeping(t, db, 1, sbpOther, sbpQuiet)
	seedHolding(t, db, 1, sbpQuiet, "XX0000000002")

	if got := safekeepingMap(t, db)[sbpPortfolio]; got != sbpHolder {
		t.Errorf("portfolio maps to %q, want its own quiet account", got)
	}
}

// A roster row with no portfolio on it belongs to no portfolio.
func TestARosterRowWithNoPortfolioIsSkipped(t *testing.T) {
	_, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO safekeeping_accounts (snapshot_at, relationship_id,
            account_external_id, portfolio_external_id, payload)
        VALUES (1, 'R1', ?, NULL, '{}')`, sbpHolder); err != nil {
		t.Fatal(err)
	}
	if got := safekeepingMap(t, db); len(got) != 0 {
		t.Errorf("map = %v, want nothing", got)
	}
}

// A web-only load has no PSN reader to ask.
func TestNoPSNReaderMapsNothing(t *testing.T) {
	var r *psnReader
	got, err := r.safekeepingByPortfolio(context.Background())
	if err != nil || got != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", got, err)
	}
}
