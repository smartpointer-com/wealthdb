package gold

import (
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestReturnsExcludeGrains locks the grain semantics: never excluded at its own
// grain (accounts always show; an excluded PORTFOLIO still shows its own row),
// but at sources/global an excluded account OR any account of an excluded
// portfolio drops out.
func TestReturnsExcludeGrains(t *testing.T) {
	e := &ReturnsExclude{
		Portfolios: map[string]map[string]bool{"ct": {"P1": true}},
		Accounts:   map[string]map[string]bool{"ct": {"A1": true}},
	}
	cases := []struct {
		level, src, pf, acct string
		want                 bool
	}{
		{"accounts", "ct", "P1", "A1", false},   // own grain: never excluded
		{"portfolios", "ct", "P1", "A9", false}, // excluded portfolio still shows its own row
		{"portfolios", "ct", "P2", "A1", true},  // excluded account drops from its portfolio
		{"portfolios", "ct", "P2", "A9", false}, // neither excluded
		{"sources", "ct", "P1", "A9", true},     // excluded portfolio -> out of source
		{"sources", "ct", "P2", "A1", true},     // excluded account -> out of source
		{"sources", "ct", "P2", "A9", false},    // neither -> stays
		{"global", "ct", "P1", "A9", true},      // same at global
		{"sources", "zz", "P1", "A1", false},    // unknown source
	}
	for _, c := range cases {
		if got := e.excluded(c.level, c.src, c.pf, c.acct); got != c.want {
			t.Errorf("excluded(%q,%q,%q,%q)=%v want %v", c.level, c.src, c.pf, c.acct, got, c.want)
		}
	}
	var nilE *ReturnsExclude
	if nilE.excluded("sources", "ct", "P1", "A1") {
		t.Error("nil ReturnsExclude must exclude nothing")
	}
}

// TestRunReturnsExclude drives the exclusion end to end: an excluded portfolio
// drops out of the source aggregate but still reports at the portfolios grain.
func TestRunReturnsExclude(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")
	pfKeep, pfDrop := "KEEP", "DROP"
	t0, t1 := dy(2024, time.January, 2), dy(2024, time.July, 2)
	seedAcct(t, db, ctx, "ubs", "AK", canonical.AccountKindBrokerage, &pfKeep, []snap{{t0, 1000}, {t1, 1100}}, nil)
	seedAcct(t, db, ctx, "ubs", "AD", canonical.AccountKindBrokerage, &pfDrop, []snap{{t0, 500}, {t1, 400}}, nil)

	excl := &ReturnsExclude{Portfolios: map[string]map[string]bool{"ubs": {"DROP": true}}}
	withExcl := func(level string) ReturnParams {
		p := params(level, 0, eod(2024, time.July, 2))
		p.ReturnsExclude = excl
		return p
	}

	// sources grain: only KEEP's account contributes -> end 1100 (not 1500).
	src, err := RunReturns(ctx, db, withExcl("sources"))
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	s, ok := summaryFor(src, "ubs")
	if !ok || s.EndValue == nil || *s.EndValue != "1100.00" {
		t.Errorf("source end = %v, want 1100 (DROP excluded)", s.EndValue)
	}

	// portfolios grain: the excluded portfolio still shows its own row.
	pf, err := RunReturns(ctx, db, withExcl("portfolios"))
	if err != nil {
		t.Fatalf("portfolios: %v", err)
	}
	if _, ok := summaryFor(pf, "DROP"); !ok {
		t.Error("excluded portfolio DROP must still show at the portfolios grain")
	}
	if _, ok := summaryFor(pf, "KEEP"); !ok {
		t.Error("KEEP portfolio missing")
	}
}
