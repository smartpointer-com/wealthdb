package gold

// stale_snapshot: a bucket (or the summary) whose end-day valuation
// rests on a snapshot older than staleFactor x the entity's median
// snapshot gap — the feed-died-mid-bucket case the empty/carried
// flags cannot see. The cadence is inferred per entity, never
// configured; with fewer than 3 observed gaps nothing is flagged.

import (
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

func TestMedianSnapGap(t *testing.T) {
	if _, ok := medianSnapGap([]int64{1, 2, 3}); ok {
		t.Error("2 gaps must not yield a cadence")
	}
	if gap, ok := medianSnapGap([]int64{0, 1, 2, 3, 10}); !ok || gap != 1 {
		t.Errorf("odd median = %v ok=%v, want 1 true", gap, ok)
	}
	if gap, ok := medianSnapGap([]int64{0, 1, 2, 4, 8}); !ok || gap != 1.5 {
		t.Errorf("even median = %v ok=%v, want 1.5 true", gap, ok)
	}
}

func TestStaleAt(t *testing.T) {
	snaps := []int64{10, 11, 12, 13, 14} // daily cadence
	gap, ok := medianSnapGap(snaps)
	if staleAt(snaps, 17, gap, ok) {
		t.Error("3-day age at gap 1 is the weekend allowance, not stale")
	}
	if !staleAt(snaps, 18, gap, ok) {
		t.Error("4-day age at gap 1 must be stale")
	}
	if staleAt(snaps, 9, gap, ok) {
		t.Error("a day before every snapshot cannot be stale")
	}
	if staleAt(snaps, 18, 0, false) {
		t.Error("no cadence must mean no flag")
	}
}

// A daily feed that dies mid-February: the February bucket holds
// snapshots (so it is neither empty nor carried) yet ends on a value
// nine days past the source's own cadence — flagged. January stays
// clean, March is empty_bucket (not stale), and the summary row at the
// window end is stale too. A quarterly-cadence sibling with the same
// absolute last-snapshot age stays unflagged throughout.
func TestRunReturnsStaleSnapshot(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ubs", "ubs")

	var daily []snap
	for d := dy(2024, time.January, 1); d <= dy(2024, time.February, 20); d += 86400 {
		daily = append(daily, snap{d, 1000})
	}
	seedAcct(t, db, ctx, "ubs", "A", canonical.AccountKindBrokerage, nil,
		daily, nil)

	quarterly := []snap{
		{dy(2023, time.March, 1), 500}, {dy(2023, time.June, 1), 510},
		{dy(2023, time.September, 1), 520}, {dy(2023, time.December, 1), 530},
		{dy(2024, time.February, 20), 540},
	}
	seedAcct(t, db, ctx, "ubs", "Q", canonical.AccountKindBrokerage, nil,
		quarterly, nil)

	p := params("accounts", 0, dy(2024, time.March, 31))
	p.Period = "monthly"
	rows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}

	var sawJan, sawFeb, sawMar bool
	for _, r := range rows {
		if r.EntityID == "Q" {
			if qualityHas(r, "stale_snapshot") {
				t.Errorf("quarterly entity flagged stale (%s row, start %d)",
					r.Period, r.StartDay)
			}
			continue
		}
		if r.EntityID != "A" || r.IsSummary {
			continue
		}
		// Bucket start days chain from the previous bucket's end, so
		// months are matched by their period label.
		switch r.Period {
		case "2024-01":
			sawJan = true
			if qualityHas(r, "stale_snapshot") {
				t.Error("January (healthy daily coverage) flagged stale")
			}
		case "2024-02":
			sawFeb = true
			if !qualityHas(r, "stale_snapshot") {
				t.Error("February (feed died on the 20th) not flagged stale")
			}
			if qualityHas(r, "empty_bucket") {
				t.Error("February holds snapshots; must not be empty_bucket")
			}
		case "2024-03":
			sawMar = true
			if !qualityHas(r, "empty_bucket") {
				t.Error("March (no snapshots) must be empty_bucket")
			}
			if qualityHas(r, "stale_snapshot") {
				t.Error("empty March must not also be stale_snapshot")
			}
		}
	}
	if !sawJan || !sawFeb || !sawMar {
		t.Fatalf("missing month rows: jan=%v feb=%v mar=%v", sawJan, sawFeb, sawMar)
	}
	s, ok := summaryFor(rows, "A")
	if !ok || !qualityHas(s, "stale_snapshot") {
		t.Errorf("summary row at a 40-day-stale window end must be flagged (ok=%v)", ok)
	}
}
