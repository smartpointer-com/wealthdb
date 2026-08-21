package gold

// Tests for the returns_hide config block: listed accounts/portfolios emit no
// rows of their own at any grain, while their values and flows stay inside
// every aggregate — the display mirror of returns_exclude.

import (
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestReturnsHideAccount pins the account form: the hidden account's row is
// gone from the accounts grain, its portfolio and source rows still show and
// still carry its value and flows, and a sibling account is untouched.
func TestReturnsHideAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "sq", "swissquote")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	pf := "PF1"
	seedAcct(t, db, ctx, "sq", "CASHBOX", canonical.AccountKindCash, &pf,
		[]snap{{a, 500}, {b, 500}},
		[]txn{{dy(2024, time.June, 3), canonical.TxKindDeposit, 100}})
	seedAcct(t, db, ctx, "sq", "BRK", canonical.AccountKindBrokerage, &pf,
		[]snap{{a, 1000}, {b, 1100}}, nil)
	end := eod(2024, time.December, 30)

	hide := &ReturnsHide{Accounts: map[string]map[string]bool{"sq": {"CASHBOX": true}}}

	p := params("accounts", 0, end)
	p.ReturnsHide = hide
	acctRows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns accounts: %v", err)
	}
	if _, ok := summaryFor(acctRows, "CASHBOX"); ok {
		t.Error("hidden account must emit no accounts-grain row")
	}
	if _, ok := summaryFor(acctRows, "BRK"); !ok {
		t.Error("sibling account must keep its row")
	}

	// The portfolio row still shows, with the hidden account's value and
	// deposit inside: start 1500, net flow 100.
	p = params("portfolios", 0, end)
	p.ReturnsHide = hide
	pfRows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns portfolios: %v", err)
	}
	pr, ok := summaryFor(pfRows, "PF1")
	if !ok {
		t.Fatal("no PF1 portfolios row")
	}
	if v, ok := parseFloatPtr(pr.StartValue); !ok || v != 1500 {
		t.Errorf("portfolio start = %v, want 1500 (hidden account included)", pr.StartValue)
	}
	if nf := netFlowOf(t, pfRows, "PF1"); nf != 100 {
		t.Errorf("portfolio net_flow = %.2f, want 100 (hidden account's deposit counted)", nf)
	}
}

// TestReturnsHidePortfolio pins the portfolio form and its cascade: the
// portfolio row and its member accounts' rows are hidden, the source row
// (which has another account) still shows and includes them.
func TestReturnsHidePortfolio(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "sq", "swissquote")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	pf := "PLUMB"
	seedAcct(t, db, ctx, "sq", "CASHBOX", canonical.AccountKindCash, &pf,
		[]snap{{a, 500}, {b, 500}}, nil)
	seedAcct(t, db, ctx, "sq", "BRK", canonical.AccountKindBrokerage, nil,
		[]snap{{a, 1000}, {b, 1100}}, nil)
	end := eod(2024, time.December, 30)

	hide := &ReturnsHide{Portfolios: map[string]map[string]bool{"sq": {"PLUMB": true}}}

	p := params("portfolios", 0, end)
	p.ReturnsHide = hide
	pfRows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns portfolios: %v", err)
	}
	if _, ok := summaryFor(pfRows, "PLUMB"); ok {
		t.Error("hidden portfolio must emit no portfolios-grain row")
	}

	p = params("accounts", 0, end)
	p.ReturnsHide = hide
	acctRows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns accounts: %v", err)
	}
	if _, ok := summaryFor(acctRows, "CASHBOX"); ok {
		t.Error("a hidden portfolio's member account must emit no row")
	}

	p = params("sources", 0, end)
	p.ReturnsHide = hide
	srcRows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns sources: %v", err)
	}
	s, ok := summaryFor(srcRows, "sq")
	if !ok {
		t.Fatal("no sq source row")
	}
	if v, ok := parseFloatPtr(s.StartValue); !ok || v != 1500 {
		t.Errorf("source start = %v, want 1500 (hidden portfolio included)", s.StartValue)
	}
}
