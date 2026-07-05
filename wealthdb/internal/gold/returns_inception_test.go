package gold

import (
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestInceptionOverridesResolve locks the most-specific-first resolution and
// the per-grain scoping (global is never anchored; a nil receiver is inert).
func TestInceptionOverridesResolve(t *testing.T) {
	o := &InceptionOverrides{
		Sources:    map[string]int64{"ct": 100},
		Portfolios: map[string]map[string]int64{"ct": {"P1": 200}},
		Accounts:   map[string]map[string]int64{"ct": {"A1": 300}},
	}
	cases := []struct {
		level, src, pf, acct string
		want                 int64
		ok                   bool
	}{
		{"accounts", "ct", "P1", "A1", 300, true}, // account wins
		{"accounts", "ct", "P1", "A9", 200, true}, // fall through to portfolio
		{"accounts", "ct", "P9", "A9", 100, true}, // fall through to source
		{"accounts", "zz", "P1", "A1", 0, false},  // unknown source
		{"portfolios", "ct", "P1", "", 200, true}, // portfolio
		{"portfolios", "ct", "P9", "", 100, true}, // fall through to source
		{"sources", "ct", "", "", 100, true},      // source
		{"sources", "ct", "P1", "A1", 100, true},  // portfolio/account ignored at source grain
		{"global", "ct", "P1", "A1", 0, false},    // global is never anchored
	}
	for _, c := range cases {
		got, ok := o.resolve(c.level, c.src, c.pf, c.acct)
		if got != c.want || ok != c.ok {
			t.Errorf("resolve(%q,%q,%q,%q) = (%d,%v), want (%d,%v)",
				c.level, c.src, c.pf, c.acct, got, ok, c.want, c.ok)
		}
	}
	var nilO *InceptionOverrides
	if _, ok := nilO.resolve("accounts", "ct", "P1", "A1"); ok {
		t.Error("nil InceptionOverrides must resolve to (_, false)")
	}
}

// TestRunReturnsConfiguredInception drives the windowing end to end: with no
// override the window opens at the first snapshot; a configured inception moves
// the opening base to its date and flags configured_inception; an inception
// BEFORE the data is a no-op (the floor can only move an anchor later).
func TestRunReturnsConfiguredInception(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	pf := "PF1"
	t0 := dy(2020, time.January, 2)
	tMid := dy(2022, time.January, 3)
	end := eod(2023, time.January, 2)
	seedAcct(t, db, ctx, "ubs", "A1", canonical.AccountKindBrokerage, &pf,
		[]snap{{t0, 100}, {tMid, 500}, {dy(2023, time.January, 2), 600}}, nil)

	startOf := func(rows []ReturnRow) (string, ReturnRow) {
		t.Helper()
		r, ok := summaryFor(rows, "PF1")
		if !ok {
			t.Fatal("no PF1 row")
		}
		if r.StartValue == nil {
			t.Fatalf("PF1 start value nil: %+v", r)
		}
		return *r.StartValue, r
	}

	// Baseline: opens at the first snapshot; no configured_inception flag.
	base, err := RunReturns(ctx, db, params("portfolios", 0, end))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	if sv, r := startOf(base); sv != "100.00" || qualityHas(r, "configured_inception") {
		t.Errorf("base start=%s flags=%v, want start 100 and no configured_inception", sv, r.Quality)
	}

	// Override at tMid: opening base becomes the value there (500) + the flag.
	p := params("portfolios", 0, end)
	p.InceptionOverrides = &InceptionOverrides{
		Portfolios: map[string]map[string]int64{"ubs": {"PF1": tMid}},
	}
	over, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if sv, r := startOf(over); sv != "500.00" || !qualityHas(r, "configured_inception") {
		t.Errorf("override start=%s flags=%v, want start 500 + configured_inception", sv, r.Quality)
	}

	// Inception before the data is a no-op: the floor only moves anchors later.
	p2 := params("portfolios", 0, end)
	p2.InceptionOverrides = &InceptionOverrides{
		Portfolios: map[string]map[string]int64{"ubs": {"PF1": dy(2010, time.January, 1)}},
	}
	early, err := RunReturns(ctx, db, p2)
	if err != nil {
		t.Fatalf("early: %v", err)
	}
	if sv, r := startOf(early); sv != "100.00" || qualityHas(r, "configured_inception") {
		t.Errorf("pre-data override start=%s flags=%v, want unchanged (100, no flag)", sv, r.Quality)
	}

	// Inception past all data collapses the window (winTo <= winFrom): the row vanishes.
	p3 := params("portfolios", 0, end)
	p3.InceptionOverrides = &InceptionOverrides{
		Portfolios: map[string]map[string]int64{"ubs": {"PF1": dy(2030, time.January, 1)}},
	}
	gone, err := RunReturns(ctx, db, p3)
	if err != nil {
		t.Fatalf("post-data: %v", err)
	}
	if _, ok := summaryFor(gone, "PF1"); ok {
		t.Error("inception after all data must collapse the window (no PF1 row)")
	}
}
