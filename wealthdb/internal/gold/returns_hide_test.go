package gold

// Tests for the returns_hide config block: listed accounts/portfolios emit no
// rows of their own at any grain, while their values and flows stay inside
// every aggregate — the display mirror of returns_exclude. The fixtures'
// hidden accounts are brokerage: a cash account hides whatever the config
// says (TestCashAccountsHiddenWhateverTheSource).

import (
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// TestReturnsHideAccount pins the account form: the hidden account's row is
// gone from the accounts grain, its portfolio and source rows still show and
// still carry its value and flows, and a sibling account is untouched.
func TestReturnsHideAccount(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "sq", "swissquote")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	pf := "PF1"
	seedAcct(t, db, ctx, "sq", "SLEEVE", canonical.AccountKindBrokerage, &pf,
		[]snap{{a, 500}, {b, 500}},
		[]txn{{dy(2024, time.June, 3), canonical.TxKindDeposit, 100}})
	seedAcct(t, db, ctx, "sq", "BRK", canonical.AccountKindBrokerage, &pf,
		[]snap{{a, 1000}, {b, 1100}}, nil)
	end := eod(2024, time.December, 30)

	hide := &ReturnsHide{Accounts: map[string]map[string]bool{"sq": {"SLEEVE": true}}}

	p := params("accounts", 0, end)
	p.ReturnsHide = hide
	acctRows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns accounts: %v", err)
	}
	if _, ok := summaryFor(acctRows, "SLEEVE"); ok {
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
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "sq", "swissquote")

	a, b := dy(2024, time.January, 2), dy(2024, time.December, 30)
	pf := "PLUMB"
	seedAcct(t, db, ctx, "sq", "SLEEVE", canonical.AccountKindBrokerage, &pf,
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
	if _, ok := summaryFor(acctRows, "SLEEVE"); ok {
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

// TestRegisteredHiddenModeHasNoEffect: the four sources that register the
// hidden accounts-grain mode emit only cash and card accounts (the bank
// engine knows no other kind), and the engine hides a cash account and drops
// a card on any source. So their output is the same with the mode overridden
// back to normal, at every grain.
func TestRegisteredHiddenModeHasNoEffect(t *testing.T) {
	t.Parallel()
	normal := returns.AccountsGrainNormal
	for _, kind := range []string{"amex", "chase", "firstcitizens", "raiffeisen_at"} {
		db, ctx := openMigrated(t)
		seedReturnsSource(t, db, ctx, "dep", kind)
		seedReturnsSource(t, db, ctx, "brk", "schwab")
		t0, t1, t2 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)
		seedAcct(t, db, ctx, "brk", "B1", canonical.AccountKindBrokerage, nil,
			[]snap{{t0, 1000}, {t2, 1300}}, []txn{{t1, canonical.TxKindDeposit, 200}})
		seedCashOnlyAcct(t, db, ctx, "dep", "CASH1", canonical.AccountKindCash,
			[]snap{{t0, 500}, {t1, 300}, {t2, 450}}, []txn{{t1, canonical.TxKindWithdrawal, -200}})
		seedCashOnlyAcct(t, db, ctx, "dep", "CARD1", canonical.AccountKindCard,
			[]snap{{t0, -80}, {t2, -60}}, []txn{{t1, canonical.TxKindPurchase, -15}})
		for _, level := range []string{"accounts", "portfolios", "sources", "global"} {
			for _, period := range []string{"total", "monthly"} {
				p := params(level, 0, eod(2024, time.July, 2))
				p.Period = period
				registered, err := RunReturns(ctx, db, p)
				if err != nil {
					t.Fatal(err)
				}
				p.PolicyOverrides = map[string]ReturnsPolicyOverride{"dep": {AccountsGrain: &normal}}
				overridden, err := RunReturns(ctx, db, p)
				if err != nil {
					t.Fatal(err)
				}
				if g, w := renderReturnRows(overridden), renderReturnRows(registered); g != w {
					t.Errorf("%s %s/%s: the registered hidden mode changes the rows:\n normal:\n%s\n registered:\n%s",
						kind, level, period, g, w)
				}
			}
		}
	}
}
