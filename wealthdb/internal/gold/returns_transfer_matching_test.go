package gold

import (
	"context"
	"database/sql"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// tmParams returns CLI-default params with the cross-source transfer matcher
// enabled at the documented defaults.
func tmParams(level string, from, to int64) ReturnParams {
	p := params(level, from, to)
	p.TransferMatching = &TransferMatching{WindowDays: 5, TolerancePct: 0.5}
	return p
}

// TestMatchCrossTransfers pins the pure matcher: cross-source only, same
// native currency, amount within tolerance, day window, (amount gap, day
// distance)-ranked greedy, one-to-one, deterministic.
func TestMatchCrossTransfers(t *testing.T) {
	mk := func() map[string]*accountData {
		return map[string]*accountData{
			acctKey("s1", "A"): {src: "s1", acct: "A"},
			acctKey("s2", "B"): {src: "s2", acct: "B"},
			acctKey("s2", "C"): {src: "s2", acct: "C"},
		}
	}
	tm := &TransferMatching{WindowDays: 5, TolerancePct: 0.5}
	cand := func(src, acct, txID string, day int64, amt float64) crossCandidate {
		return crossCandidate{src: src, acct: acct, txID: txID, day: day, ccy: "USD", amt: amt}
	}

	t.Run("basic pair links both accounts", func(t *testing.T) {
		byKey := mk()
		matchCrossTransfers([]crossCandidate{
			cand("s1", "A", "t1", 100, -5000),
			cand("s2", "B", "t2", 102, 5000),
		}, tm, byKey)
		a, b := byKey[acctKey("s1", "A")], byKey[acctKey("s2", "B")]
		if l, ok := a.crossLinks["t1"]; !ok || l.src != "s2" || l.acct != "B" || l.txID != "t2" || l.day != 102 {
			t.Errorf("debit link = %+v, %v", l, ok)
		}
		if l, ok := b.crossLinks["t2"]; !ok || l.txID != "t1" {
			t.Errorf("credit link = %+v, %v", l, ok)
		}
	})

	// Equal amounts on both candidates: the gap ties, so day distance decides.
	t.Run("prefers the nearest-day credit", func(t *testing.T) {
		byKey := mk()
		matchCrossTransfers([]crossCandidate{
			cand("s1", "A", "t1", 100, -5000),
			cand("s2", "B", "far", 104, 5000),
			cand("s2", "C", "near", 101, 5000),
		}, tm, byKey)
		if l := byKey[acctKey("s1", "A")].crossLinks["t1"]; l.txID != "near" {
			t.Errorf("picked %q, want the nearest-day credit", l.txID)
		}
	})

	t.Run("tolerance admits a wire fee, not more", func(t *testing.T) {
		byKey := mk()
		matchCrossTransfers([]crossCandidate{
			cand("s1", "A", "t1", 100, -10000),
			cand("s2", "B", "fee", 100, 9960), // 0.4% short: within 0.5%
		}, tm, byKey)
		if _, ok := byKey[acctKey("s1", "A")].crossLinks["t1"]; !ok {
			t.Error("0.4% fee gap must still match")
		}
		byKey = mk()
		matchCrossTransfers([]crossCandidate{
			cand("s1", "A", "t1", 100, -10000),
			cand("s2", "B", "off", 100, 9900), // 1% short: beyond tolerance
		}, tm, byKey)
		if len(byKey[acctKey("s1", "A")].crossLinks) != 0 {
			t.Error("1% amount gap must not match at 0.5% tolerance")
		}
	})

	t.Run("guards: window, source, currency", func(t *testing.T) {
		byKey := mk()
		matchCrossTransfers([]crossCandidate{
			cand("s1", "A", "t1", 100, -5000),
			cand("s2", "B", "late", 106, 5000), // 6 days: outside the 5-day window
			{src: "s1", acct: "A", txID: "same-src", day: 100, ccy: "USD", amt: 5000},
			{src: "s2", acct: "C", txID: "chf", day: 100, ccy: "CHF", amt: 5000},
		}, tm, byKey)
		if n := len(byKey[acctKey("s1", "A")].crossLinks); n != 0 {
			t.Errorf("no candidate should have matched, got %d links", n)
		}
	})

	t.Run("one-to-one", func(t *testing.T) {
		byKey := mk()
		matchCrossTransfers([]crossCandidate{
			cand("s1", "A", "d1", 100, -5000),
			cand("s1", "A", "d2", 100, -5000),
			cand("s2", "B", "c1", 100, 5000),
		}, tm, byKey)
		links := 0
		for _, id := range []string{"d1", "d2"} {
			if _, ok := byKey[acctKey("s1", "A")].crossLinks[id]; ok {
				links++
			}
		}
		if links != 1 {
			t.Errorf("one credit must satisfy exactly one debit, got %d", links)
		}
	})

	t.Run("nil config is inert", func(t *testing.T) {
		byKey := mk()
		matchCrossTransfers([]crossCandidate{
			cand("s1", "A", "t1", 100, -5000),
			cand("s2", "B", "t2", 100, 5000),
		}, nil, byKey)
		if byKey[acctKey("s1", "A")].crossLinks != nil {
			t.Error("nil TransferMatching must produce no links")
		}
	})
}

// TestAppendCrossCandidate pins the candidate filters: equity-transfer ledger
// legs and zero/valueless amounts never enter the pool.
func TestAppendCrossCandidate(t *testing.T) {
	amt := "5000.00"
	zero := "0"
	if got := appendCrossCandidate(nil, "s", "a", "xfer:abc", "USD", 86400, &amt); len(got) != 0 {
		t.Error("xfer: ledger leg must be skipped")
	}
	if got := appendCrossCandidate(nil, "s", "a", "t1", "USD", 86400, nil); len(got) != 0 {
		t.Error("nil amount must be skipped")
	}
	if got := appendCrossCandidate(nil, "s", "a", "t1", "USD", 86400, &zero); len(got) != 0 {
		t.Error("zero amount must be skipped")
	}
	got := appendCrossCandidate(nil, "s", "a", "t1", "USD", 2*86400, &amt)
	if len(got) != 1 || got[0].day != 2 || got[0].amt != 5000 {
		t.Errorf("candidate = %+v", got)
	}
}

// seedCrossPair seeds two flow-complete sources: s1/A1 sends a 10000 journal
// out on Mar 1, s2/B1 books a 10000 deposit on Mar 3 — the classic one-sided
// cross-custodian move that per-source heuristics can never pair.
func seedCrossPair(t *testing.T, db *sql.DB, ctx context.Context, b1FirstSnap int64) {
	t.Helper()
	seedReturnsSource(t, db, ctx, "s1", "schwab")
	seedReturnsSource(t, db, ctx, "s2", "schwab")
	seedAcct(t, db, ctx, "s1", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2024, time.January, 2), 100000}, {dy(2024, time.June, 3), 90500}, {dy(2024, time.December, 2), 91000}},
		[]txn{{dy(2024, time.March, 1), canonical.TxKindJournal, -10000}})
	seedAcct(t, db, ctx, "s2", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{b1FirstSnap, 50000}, {dy(2024, time.December, 2), 61000}},
		[]txn{{dy(2024, time.March, 3), canonical.TxKindDeposit, 10000}})
}

// TestRunReturnsCrossSourceNetting drives the matcher end to end: the pair
// nets at global (both legs inside), stays counted at the sources grain (one
// leg each), and the whole feature is a no-op when the amounts can't pair.
func TestRunReturnsCrossSourceNetting(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCrossPair(t, db, ctx, dy(2024, time.January, 2))
	end := eod(2024, time.December, 2)

	// Baseline: the journal leg survives per-entity netting unmatched.
	base, err := RunReturns(ctx, db, params("global", 0, end))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	gBase, ok := summaryFor(base, "")
	if !ok {
		t.Fatal("no global row")
	}
	if !qualityHas(gBase, "unmatched_transfers=1") || qualityHas(gBase, "cross_source_netted=1") {
		t.Errorf("baseline flags = %v; want unmatched_transfers=1 and no cross tag", gBase.Quality)
	}

	// Matching on: the pair nets at global — tag flips, unmatched count clears.
	on, err := RunReturns(ctx, db, tmParams("global", 0, end))
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	g, ok := summaryFor(on, "")
	if !ok {
		t.Fatal("no global row (matching)")
	}
	if !qualityHas(g, "cross_source_netted=1") || qualityHas(g, "unmatched_transfers=1") {
		t.Errorf("matching flags = %v; want cross_source_netted=1 and no unmatched", g.Quality)
	}
	if nf := netFlowOf(t, on, ""); math.Abs(nf) > 0.01 {
		t.Errorf("global net flow = %v, want 0 (pair netted)", nf)
	}

	// Sources grain: each leg is still that source's real boundary flow.
	srcRows, err := RunReturns(ctx, db, tmParams("sources", 0, end))
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	s1, ok := summaryFor(srcRows, "s1")
	if !ok {
		t.Fatal("no s1 row")
	}
	if !qualityHas(s1, "unmatched_transfers=1") || qualityHas(s1, "cross_source_netted=1") {
		t.Errorf("s1 flags = %v; want its leg still unmatched, never cross-netted", s1.Quality)
	}
	if nf := netFlowOf(t, srcRows, "s2"); math.Abs(nf-10000) > 0.01 {
		t.Errorf("s2 net flow = %v, want the deposit counted", nf)
	}
}

// TestCrossSourceNettingNoPairIsByteIdentical pins the off-equivalence: with
// matching enabled but no eligible pair, output equals the baseline rows
// exactly.
func TestCrossSourceNettingNoPairIsByteIdentical(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "s1", "schwab")
	seedReturnsSource(t, db, ctx, "s2", "schwab")
	seedAcct(t, db, ctx, "s1", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2024, time.January, 2), 100000}, {dy(2024, time.December, 2), 91000}},
		[]txn{{dy(2024, time.March, 1), canonical.TxKindWithdrawal, -10000}})
	seedAcct(t, db, ctx, "s2", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2024, time.January, 2), 50000}, {dy(2024, time.December, 2), 61000}},
		[]txn{{dy(2024, time.March, 3), canonical.TxKindDeposit, 10100}}) // 1% > tolerance
	end := eod(2024, time.December, 2)

	base, err := RunReturns(ctx, db, params("global", 0, end))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	on, err := RunReturns(ctx, db, tmParams("global", 0, end))
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if !reflect.DeepEqual(base, on) {
		t.Error("no eligible pair: matching-on output must equal the baseline")
	}
}

// TestCrossSourceNettingWindowStraddle pins the both-legs-inside rule: a
// window ending between the legs keeps the in-window leg as a real flow.
func TestCrossSourceNettingWindowStraddle(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCrossPair(t, db, ctx, dy(2024, time.January, 2))

	rows, err := RunReturns(ctx, db, tmParams("global", 0, eod(2024, time.March, 2)))
	if err != nil {
		t.Fatalf("straddle: %v", err)
	}
	g, ok := summaryFor(rows, "")
	if !ok {
		t.Fatal("no global row")
	}
	if qualityHas(g, "cross_source_netted=1") {
		t.Errorf("straddling pair must not net; flags = %v", g.Quality)
	}
	if nf := netFlowOf(t, rows, ""); math.Abs(nf-(-10000)) > 0.01 {
		t.Errorf("net flow = %v, want the in-window journal leg counted", nf)
	}
}

// TestCrossSourceNettingSubsumedDebutLegKeptOut pins the onboarding
// interplay: a deposit funding a constituent that debuts AFTER it is
// represented by the synthetic onboarding step-up, so the pair must NOT net —
// dropping the sender's live leg would count the capital asymmetrically.
func TestCrossSourceNettingSubsumedDebutLegKeptOut(t *testing.T) {
	db, ctx := openMigrated(t)
	// B1's first snapshot lands the day after its funding deposit.
	seedCrossPair(t, db, ctx, dy(2024, time.March, 4))
	end := eod(2024, time.December, 2)

	rows, err := RunReturns(ctx, db, tmParams("global", 0, end))
	if err != nil {
		t.Fatalf("subsumed: %v", err)
	}
	g, ok := summaryFor(rows, "")
	if !ok {
		t.Fatal("no global row")
	}
	if qualityHas(g, "cross_source_netted=1") {
		t.Errorf("pair with a debut-subsumed leg must not net; flags = %v", g.Quality)
	}
	// The journal leg still counts (-10000); B1's arrival enters as its 50000
	// onboarding step-up, not as the subsumed deposit.
	if nf := netFlowOf(t, rows, ""); math.Abs(nf-40000) > 0.01 {
		t.Errorf("net flow = %v, want -10000 + 50000 onboarding", nf)
	}
}

// TestCrossMatchingMultiSingleParity pins the multi-currency loader: the
// links the single-currency USD dataset derives are exactly the links the
// multi loader's USD dataset derives.
func TestCrossMatchingMultiSingleParity(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCrossPair(t, db, ctx, dy(2024, time.January, 2))
	tm := &TransferMatching{WindowDays: 5, TolerancePct: 0.5}

	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		t.Fatalf("fx: %v", err)
	}
	single, err := loadReturnsDataset(ctx, db, "USD", fx, nil, tm)
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	multi, err := loadReturnsDatasetsMulti(ctx, db, fx, nil, tm)
	if err != nil {
		t.Fatalf("multi: %v", err)
	}
	for key, a := range single.accts {
		b := multi["USD"].accts[key]
		if b == nil {
			t.Fatalf("multi lacks %q", key)
		}
		if !reflect.DeepEqual(a.crossLinks, b.crossLinks) {
			t.Errorf("%q links differ: single %+v multi %+v", key, a.crossLinks, b.crossLinks)
		}
	}
}

// TestCrossSourceNettingObeysNettingSwitch pins that --netting off disables
// cross-source pair drops too: the diagnostic view shows every raw leg.
func TestCrossSourceNettingObeysNettingSwitch(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCrossPair(t, db, ctx, dy(2024, time.January, 2))
	end := eod(2024, time.December, 2)

	p := tmParams("global", 0, end)
	p.Netting = false
	rows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("netting off: %v", err)
	}
	g, ok := summaryFor(rows, "")
	if !ok {
		t.Fatal("no global row")
	}
	if qualityHas(g, "cross_source_netted=1") {
		t.Errorf("netting off must disable cross-source drops; flags = %v", g.Quality)
	}
}

// TestMatchCrossTransfersPrefersExactAmount pins the greedy rank: within the
// tolerance, an exact-amount partner beats a nearer-day coincidence.
func TestMatchCrossTransfersPrefersExactAmount(t *testing.T) {
	byKey := map[string]*accountData{
		acctKey("s1", "A"): {src: "s1", acct: "A"},
		acctKey("s2", "B"): {src: "s2", acct: "B"},
		acctKey("s2", "C"): {src: "s2", acct: "C"},
	}
	tm := &TransferMatching{WindowDays: 5, TolerancePct: 0.5}
	matchCrossTransfers([]crossCandidate{
		{src: "s1", acct: "A", txID: "t1", day: 100, ccy: "USD", amt: -10000},
		{src: "s2", acct: "B", txID: "near", day: 100, ccy: "USD", amt: 9970}, // same day, fee-sized gap
		{src: "s2", acct: "C", txID: "exact", day: 103, ccy: "USD", amt: 10000},
	}, tm, byKey)
	if l := byKey[acctKey("s1", "A")].crossLinks["t1"]; l.txID != "exact" {
		t.Errorf("picked %q, want the exact-amount partner over the nearer-day one", l.txID)
	}
}

// TestCrossMatchedDropsOnboardNoneTransferLike pins the per-slice liveness
// rule directly: a pre-debut TRANSFER-LIKE leg is subsumed unconditionally
// (even under OnboardNone, unlike the nonTransfer slice), so its pair must
// not net — dropping the partner's live leg would count the capital
// asymmetrically. No registered policy produces this shape today; the pin
// guards the invariant for future OnboardNone sources with transfer kinds.
func TestCrossMatchedDropsOnboardNoneTransferLike(t *testing.T) {
	winFrom, winTo := int64(100), int64(300)
	sender := &accountData{
		src: "s1", acct: "A",
		series:       []dayVal{{day: 100, val: 50000}, {day: 300, val: 40000}},
		transferLike: []returns.Flow{{Day: 150, Amount: -10000, ID: "out"}},
		crossLinks:   map[string]crossLink{"out": {src: "s2", acct: "B", txID: "in", day: 150}},
	}
	receiver := &accountData{
		src: "s2", acct: "B",
		// Debuts AFTER the flow: day 150 is pre-debut for the day-200 spine.
		series:       []dayVal{{day: 200, val: 10000}, {day: 300, val: 11000}},
		transferLike: []returns.Flow{{Day: 150, Amount: 10000, ID: "in"}},
		crossLinks:   map[string]crossLink{"in": {src: "s1", acct: "A", txID: "out", day: 150}},
	}
	receiver.rpolicy.OnboardScope = returns.OnboardNone

	drops, pairs := crossMatchedDrops([]*accountData{sender, receiver}, winFrom, winTo)
	if pairs != 0 || len(drops) != 0 {
		t.Errorf("pre-debut transferLike leg under OnboardNone must not net: pairs=%d drops=%v", pairs, drops)
	}

	// Control: the same shape on the nonTransfer slice IS live under
	// OnboardNone (the deposit is the capital event there) and nets.
	sender.nonTransfer, sender.transferLike = sender.transferLike, nil
	receiver.nonTransfer, receiver.transferLike = receiver.transferLike, nil
	drops, pairs = crossMatchedDrops([]*accountData{sender, receiver}, winFrom, winTo)
	if pairs != 1 || len(drops) != 2 {
		t.Errorf("OnboardNone nonTransfer pre-debut pair must net: pairs=%d drops=%v", pairs, drops)
	}
}
