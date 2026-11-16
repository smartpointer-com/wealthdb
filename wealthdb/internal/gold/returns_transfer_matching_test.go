package gold

import (
	"context"
	"database/sql"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// tmParams returns CLI-default params with the stored transfer links read.
func tmParams(level string, from, to int64) ReturnParams {
	p := params(level, from, to)
	p.TransferMatching = &TransferMatching{}
	return p
}

// TestBindStoredLinks pins which stored links bind: both legs attached
// flows, across sources and inside one.
func TestBindStoredLinks(t *testing.T) {
	t.Parallel()
	links := []storedTransferLink{
		{src: "s1", txID: "out", farSrc: "s2", farTxID: "in"},
		{src: "s2", txID: "in", farSrc: "s1", farTxID: "out"},
		{src: "s1", txID: "w", farSrc: "s1", farTxID: "d"},
		{src: "s1", txID: "lone", farSrc: "s2", farTxID: "card"},
	}
	flows := attachedFlows{
		{"s1", "out"}:  {acct: "A", day: 100},
		{"s2", "in"}:   {acct: "B", day: 102},
		{"s1", "w"}:    {acct: "A", day: 110},
		{"s1", "d"}:    {acct: "C", day: 110},
		{"s1", "lone"}: {acct: "A", day: 120},
	}
	byKey := map[string]*accountData{
		acctKey("s1", "A"): {src: "s1", acct: "A"},
		acctKey("s2", "B"): {src: "s2", acct: "B"},
		acctKey("s1", "C"): {src: "s1", acct: "C"},
	}
	bindStoredLinks(links, flows, byKey)
	if got, want := byKey[acctKey("s1", "A")].crossLinks, map[string]crossLink{
		"out": {src: "s2", acct: "B", txID: "in", day: 102},
		"w":   {src: "s1", acct: "C", txID: "d", day: 110},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("A: %+v; want %+v", got, want)
	}
	if got := byKey[acctKey("s2", "B")].crossLinks["in"]; got != (crossLink{src: "s1", acct: "A", txID: "out", day: 100}) {
		t.Errorf("B: %+v", got)
	}
	if got := byKey[acctKey("s1", "C")].crossLinks["d"]; got != (crossLink{src: "s1", acct: "A", txID: "w", day: 110}) {
		t.Errorf("C: %+v; a link inside one source binds too", got)
	}
	if _, ok := byKey[acctKey("s1", "A")].crossLinks["lone"]; ok {
		t.Error("a link whose partner is no flow is not bound")
	}
	if byKey[acctKey("s1", "A")].transferLikeIDs == nil {
		t.Error("a linked account is indexed")
	}
}

// seedTransferLink stores the transfer pair (src, tx) and (farSrc, farTx),
// one row per leg, as the load's enrichment pass does.
func seedTransferLink(t *testing.T, db *sql.DB, ctx context.Context, src, tx, farSrc, farTx string) {
	t.Helper()
	for _, l := range [][4]string{{src, tx, farSrc, farTx}, {farSrc, farTx, src, tx}} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO transfer_links (silver_source_id, transaction_external_id,
			    far_silver_source_id, far_transaction_external_id, evidence, measure)
			VALUES (?, ?, ?, ?, 'amount', 'cash')`, l[0], l[1], l[2], l[3]); err != nil {
			t.Fatal(err)
		}
	}
}

// seedCrossPair seeds two flow-complete sources: s1/A1 sends a 10000 journal
// out on Mar 1, s2/B1 books a 10000 deposit on Mar 3 — the classic one-sided
// cross-custodian move that per-source heuristics can never pair — and the
// link the load stores between the two legs.
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
	seedTransferLink(t, db, ctx, "s1", "A1-tx0", "s2", "B1-tx0")
}

// TestRunReturnsCrossSourceNetting drives the links end to end: the pair
// nets at global (both legs inside), stays counted at the sources grain (one
// leg each).
func TestRunReturnsCrossSourceNetting(t *testing.T) {
	t.Parallel()
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

	// Links read: the pair nets at global — tag flips, unmatched count clears.
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
// the links read but none stored, output equals the baseline rows exactly,
// even where the amounts would nearly pair.
func TestCrossSourceNettingNoPairIsByteIdentical(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	db, ctx := openMigrated(t)
	seedCrossPair(t, db, ctx, dy(2024, time.January, 2))
	tm := &TransferMatching{}

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
	t.Parallel()
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

// TestCrossMatchedDropsOnboardNoneTransferLike pins the per-slice liveness
// rule directly: a pre-debut TRANSFER-LIKE leg is subsumed unconditionally
// (even under OnboardNone, unlike the nonTransfer slice), so its pair must
// not net — dropping the partner's live leg would count the capital
// asymmetrically. No registered policy produces this shape today; the pin
// guards the invariant for future OnboardNone sources with transfer kinds.
func TestCrossMatchedDropsOnboardNoneTransferLike(t *testing.T) {
	t.Parallel()
	winFrom, winTo := int64(100), int64(300)
	// bindStoredLinks indexes every linked account's transferLike slice;
	// direct construction must uphold that invariant.
	sender := &accountData{
		src: "s1", acct: "A",
		series:          []dayVal{{day: 100, val: 50000}, {day: 300, val: 40000}},
		transferLike:    []returns.Flow{{Day: 150, Amount: -10000, ID: "out"}},
		crossLinks:      map[string]crossLink{"out": {src: "s2", acct: "B", txID: "in", day: 150}},
		transferLikeIDs: map[string]bool{"out": true},
	}
	receiver := &accountData{
		src: "s2", acct: "B",
		// Debuts AFTER the flow: day 150 is pre-debut for the day-200 spine.
		series:          []dayVal{{day: 200, val: 10000}, {day: 300, val: 11000}},
		transferLike:    []returns.Flow{{Day: 150, Amount: 10000, ID: "in"}},
		crossLinks:      map[string]crossLink{"in": {src: "s1", acct: "A", txID: "out", day: 150}},
		transferLikeIDs: map[string]bool{"in": true},
	}
	receiver.rpolicy.OnboardScope = returns.OnboardNone

	drops, pairs := crossMatchedDrops([]*accountData{sender, receiver}, winFrom, winTo)
	if pairs != 0 || len(drops) != 0 {
		t.Errorf("pre-debut transferLike leg under OnboardNone must not net: pairs=%d drops=%v", pairs, drops)
	}

	// Control: the same shape on the nonTransfer slice IS live under
	// OnboardNone (the deposit is the capital event there) and nets.
	sender.nonTransfer, sender.transferLike, sender.transferLikeIDs = sender.transferLike, nil, nil
	receiver.nonTransfer, receiver.transferLike, receiver.transferLikeIDs = receiver.transferLike, nil, nil
	drops, pairs = crossMatchedDrops([]*accountData{sender, receiver}, winFrom, winTo)
	if pairs != 1 || len(drops) != 2 {
		t.Errorf("OnboardNone nonTransfer pre-debut pair must net: pairs=%d drops=%v", pairs, drops)
	}
}

// seedStoredLinkWorld seeds a cross-source pair and a deposit and
// withdrawal pair inside s1, and stores both as links.
func seedStoredLinkWorld(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedReturnsSource(t, db, ctx, "s1", "schwab")
	seedReturnsSource(t, db, ctx, "s2", "schwab")
	seedAcct(t, db, ctx, "s1", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2024, time.January, 2), 100000}, {dy(2024, time.June, 3), 86000}, {dy(2024, time.December, 2), 87000}},
		[]txn{{dy(2024, time.March, 1), canonical.TxKindWithdrawal, -10000},
			{dy(2024, time.April, 2), canonical.TxKindWithdrawal, -4000}})
	seedAcct(t, db, ctx, "s1", "A2", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2024, time.January, 2), 20000}, {dy(2024, time.June, 3), 24100}, {dy(2024, time.December, 2), 24500}},
		[]txn{{dy(2024, time.April, 3), canonical.TxKindDeposit, 4000}})
	seedAcct(t, db, ctx, "s2", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{dy(2024, time.January, 2), 50000}, {dy(2024, time.December, 2), 61000}},
		[]txn{{dy(2024, time.March, 3), canonical.TxKindDeposit, 10000}})
	seedTransferLink(t, db, ctx, "s1", "A1-tx0", "s2", "B1-tx0")
	seedTransferLink(t, db, ctx, "s1", "A1-tx1", "s1", "A2-tx0")
}

// TestRunReturnsStoredLinks drives the links end to end: both pairs net at
// global, the pair inside s1 also at the sources grain, the accounts grain
// reads every leg, and with the block off no link is read.
func TestRunReturnsStoredLinks(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedStoredLinkWorld(t, db, ctx)
	end := eod(2024, time.December, 2)
	run := func(p ReturnParams) []ReturnRow {
		t.Helper()
		rows, err := RunReturns(ctx, db, p)
		if err != nil {
			t.Fatalf("%s: %v", p.Level, err)
		}
		return rows
	}
	if on, off := run(tmParams("accounts", 0, end)), run(params("accounts", 0, end)); !reflect.DeepEqual(on, off) {
		t.Error("the accounts grain moved with the links read")
	}
	rows := run(tmParams("global", 0, end))
	if g, _ := summaryFor(rows, ""); !qualityHas(g, "cross_source_netted=2") {
		t.Errorf("global flags = %v; want cross_source_netted=2", g.Quality)
	}
	if nf := netFlowOf(t, rows, ""); math.Abs(nf) > 0.01 {
		t.Errorf("global net flow = %v, want 0", nf)
	}
	rows = run(tmParams("sources", 0, end))
	// The window holds both legs of the pair inside s1, so its net flow is
	// the cross leg whether the pair nets or not.
	if s1, _ := summaryFor(rows, "s1"); !qualityHas(s1, "cross_source_netted=1") {
		t.Errorf("s1 flags = %v; the pair inside one source nets", s1.Quality)
	}
	if nf := netFlowOf(t, rows, "s1"); math.Abs(nf+10000) > 0.01 {
		t.Errorf("s1 net flow = %v, want the cross leg alone", nf)
	}
	if g, _ := summaryFor(run(params("global", 0, end)), ""); qualityHas(g, "cross_source_netted=1") {
		t.Errorf("with transfer matching off a link was read: %v", g.Quality)
	}
}
