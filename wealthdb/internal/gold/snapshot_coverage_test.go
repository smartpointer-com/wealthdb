package gold

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A key whose account a later snapshot covers ends at the first such
// snapshot, or at its inferred ending where that comes first
// (pos_key_ends, cash_key_ends). These tests pin the rule on invented
// books beside the inferred endings.

// cover states that src's snapshot at reports acct whole.
func cover(t *testing.T, db *sql.DB, ctx context.Context, src, acct string, at int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO snapshot_coverage (silver_source_id, snapshot_at, account_external_id) VALUES (?, ?, ?)`,
		src, at, acct); err != nil {
		t.Fatal(err)
	}
}

// TestACoveringSnapshotEndsTheKey: A and B are read nightly together;
// A closes a day before the newest run. With no coverage stated, A ends
// at B's first run without it only once the source runs one and a half
// cadences past A, so it stands. A run that states it covered A ends A
// there at once, in either table. A key read alone, which no company
// could re-cover, ends at a covering snapshot too.
func TestACoveringSnapshotEndsTheKey(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"positions", "cash"} {
		t.Run(table, func(t *testing.T) {
			db, ctx := openMigrated(t)
			seed := func(src, acct string, at int64) {
				if table == "cash" {
					pitCash(t, db, ctx, src, acct, "USD", at, 100)
				} else {
					pitPosition(t, db, ctx, src, acct, acct, at, 100)
				}
			}
			ccy := ""
			if table == "cash" {
				ccy = "USD"
			}
			for _, src := range []string{"plain-src", "cover-src"} {
				for off := int64(-20); off <= 0; off++ {
					seed(src, "B-0000", pitAt(off, 2))
					if off <= -1 {
						seed(src, "A-0000", pitAt(off, 2))
					}
					if src == "cover-src" {
						cover(t, db, ctx, src, "A-0000", pitAt(off, 2))
						cover(t, db, ctx, src, "B-0000", pitAt(off, 2))
					}
				}
			}
			// The alone key is read on five days; the source states it
			// covered it once more, two days after.
			for off := int64(-30); off <= -26; off++ {
				seed("alone-src", "S-0000", pitAt(off, 2))
				cover(t, db, ctx, "alone-src", "S-0000", pitAt(off, 2))
			}
			cover(t, db, ctx, "alone-src", "S-0000", pitAt(-24, 2))

			// plain-src: cadence one day, so the grace is 36 hours; the
			// source's newest run lies a day past A's last.
			if end, ok := cadenceEnd(t, db, ctx, "plain-src", "A-0000", ccy); ok {
				t.Errorf("plain A ends at %d, want it standing within its grace", end)
			}
			if end, ok := cadenceEnd(t, db, ctx, "cover-src", "A-0000", ccy); !ok || end != pitAt(0, 2) {
				t.Errorf("covered A ends at %d (%v), want the covering run %d", end, ok, pitAt(0, 2))
			}
			if end, ok := cadenceEnd(t, db, ctx, "alone-src", "S-0000", ccy); !ok || end != pitAt(-24, 2) {
				t.Errorf("the alone key ends at %d (%v), want the covering snapshot %d", end, ok, pitAt(-24, 2))
			}
			if _, ok := cadenceEnd(t, db, ctx, "cover-src", "B-0000", ccy); ok {
				t.Error("B, read in the newest run, has an ending")
			}
		})
	}
}

// TestCoverageNeverEndsAKeyLater: a partial run reads B alone, beside the
// full runs that read A and B. Inferred, the partial run re-covers A's
// company and ends A once the grace has run. Where the source states
// what each run covered, the first run that covers A and leaves it out
// comes later: A still ends at the inferred ending, the earlier of the
// two. With no covering run after A's last, A takes the inferred ending
// too.
func TestCoverageNeverEndsAKeyLater(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, src := range []string{"plain-src", "cover-src", "open-src"} {
		seedHistAccount(t, db, ctx, src, "A-0000", "cash", "Account EXAMPLE")
		for off := int64(-40); off <= -30; off++ {
			for _, acct := range []string{"A-0000", "B-0000"} {
				pitCash(t, db, ctx, src, acct, "USD", pitAt(off, 2), 100)
				if src != "plain-src" {
					cover(t, db, ctx, src, acct, pitAt(off, 2))
				}
			}
		}
		// The partial runs read B alone, every day on.
		for off := int64(-29); off <= 0; off++ {
			pitCash(t, db, ctx, src, "B-0000", "USD", pitAt(off, 4), 100)
			if src != "plain-src" {
				cover(t, db, ctx, src, "B-0000", pitAt(off, 4))
			}
		}
	}
	// cover-src runs in full once more, and A is gone from it.
	cover(t, db, ctx, "cover-src", "A-0000", pitAt(-5, 6))
	cover(t, db, ctx, "cover-src", "B-0000", pitAt(-5, 6))
	pitCash(t, db, ctx, "cover-src", "B-0000", "USD", pitAt(-5, 6), 100)

	if end, ok := cadenceEnd(t, db, ctx, "plain-src", "A-0000", "USD"); !ok || end != pitAt(-29, 4) {
		t.Errorf("plain A ends at %d (%v), want the partial run %d", end, ok, pitAt(-29, 4))
	}
	if end, ok := cadenceEnd(t, db, ctx, "cover-src", "A-0000", "USD"); !ok || end != pitAt(-29, 4) {
		t.Errorf("covered A ends at %d (%v), want the inferred %d, before the full run", end, ok, pitAt(-29, 4))
	}
	// open-src: no covering run follows A's last, so A takes the
	// inferred ending.
	if end, ok := cadenceEnd(t, db, ctx, "open-src", "A-0000", "USD"); !ok || end != pitAt(-29, 4) {
		t.Errorf("uncovered A ends at %d (%v), want the inferred %d", end, ok, pitAt(-29, 4))
	}
	for _, c := range []struct {
		src  string
		off  int64
		want float64
	}{
		{"plain-src", -30, 100}, {"plain-src", -20, 0}, {"cover-src", -30, 100}, {"cover-src", -20, 0},
	} {
		hist, pit, _ := accountOn(t, db, ctx, c.src, "A-0000", c.off)
		if !near(hist, c.want) || !near(pit, c.want) {
			t.Errorf("%s A on day %+d: history %v, at the day's end %v, want %v", c.src, c.off, hist, pit, c.want)
		}
	}
}

// TestAnAccountCoveredAfterItsLastHoldingEndsAtTheFirstCover: A's
// holdings come from a statement stream, stamped at midnight, in which
// A is alone, so nothing re-covers its company. The source's runs, at
// midday, cover A at many instants after its last statement and state
// no holding of it. A ends at the first of them. Without the coverage,
// A has no ending.
func TestAnAccountCoveredAfterItsLastHoldingEndsAtTheFirstCover(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, src := range []string{"plain-src", "cover-src"} {
		seedHistAccount(t, db, ctx, src, "A-0000", "brokerage", "Account EXAMPLE")
		for off := int64(-400); off <= -200; off += 30 {
			pitPosition(t, db, ctx, src, "A-0000", "AX", pitAt(off, 0), 100)
		}
		for off := int64(-190); off <= 0; off += 10 {
			pitPosition(t, db, ctx, src, "C-0000", "CX", pitAt(off, 12), 100)
			if src == "cover-src" {
				cover(t, db, ctx, src, "A-0000", pitAt(off, 12))
				cover(t, db, ctx, src, "C-0000", pitAt(off, 12))
			}
		}
	}
	if end, ok := cadenceEnd(t, db, ctx, "plain-src", "A-0000", ""); ok {
		t.Errorf("plain A ends at %d, want no ending", end)
	}
	if end, ok := cadenceEnd(t, db, ctx, "cover-src", "A-0000", ""); !ok || end != pitAt(-190, 12) {
		t.Errorf("covered A ends at %d (%v), want the first covering run %d", end, ok, pitAt(-190, 12))
	}
	for _, c := range []struct {
		off  int64
		want float64
	}{{-191, 100}, {-190, 0}, {0, 0}} {
		hist, pit, _ := accountOn(t, db, ctx, "cover-src", "A-0000", c.off)
		if !near(hist, c.want) || !near(pit, c.want) {
			t.Errorf("covered A on day %+d: history %v, at the day's end %v, want %v", c.off, hist, pit, c.want)
		}
	}
}

// TestACoveringSnapshotEndsOnlyWhatItOmits: a run that covers an account
// and states its cash only ends the account's positions, and the cash
// currency it no longer states; it ends nothing it reports, and a
// covering snapshot before a key's last observation ends nothing.
func TestACoveringSnapshotEndsOnlyWhatItOmits(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedFX(t, db, pitAt(-200, 12), "USD", "EUR", "1.0")
	for off := int64(-10); off <= -5; off++ {
		pitPosition(t, db, ctx, "src", "A-0000", "AX", pitAt(off, 2), 100)
		pitCash(t, db, ctx, "src", "A-0000", "USD", pitAt(off, 2), 10)
		pitCash(t, db, ctx, "src", "A-0000", "EUR", pitAt(off, 2), 20)
		cover(t, db, ctx, "src", "A-0000", pitAt(off, 2))
	}
	pitCash(t, db, ctx, "src", "A-0000", "USD", pitAt(-4, 2), 10)
	cover(t, db, ctx, "src", "A-0000", pitAt(-4, 2))

	if end, ok := cadenceEnd(t, db, ctx, "src", "A-0000", ""); !ok || end != pitAt(-4, 2) {
		t.Errorf("A's positions end at %d (%v), want %d", end, ok, pitAt(-4, 2))
	}
	if end, ok := cadenceEnd(t, db, ctx, "src", "A-0000", "EUR"); !ok || end != pitAt(-4, 2) {
		t.Errorf("A's EUR cash ends at %d (%v), want %d", end, ok, pitAt(-4, 2))
	}
	if end, ok := cadenceEnd(t, db, ctx, "src", "A-0000", "USD"); ok {
		t.Errorf("A's USD cash, stated in the newest run, ends at %d", end)
	}
}

// TestInsertCoverageLandsADuplicateOnce: a source that states one
// account at one instant in two batches lands one coverage row.
func TestInsertCoverageLandsADuplicateOnce(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	c := canonical.CoverageChange{SilverSourceID: "test-src", SnapshotAt: 1700000000, AccountExternalID: "ACCT0001"}
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertCoverage(ctx, []canonical.CoverageChange{c}); err != nil {
			return err
		}
		return w.InsertCoverage(ctx, []canonical.CoverageChange{c, c})
	})
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshot_coverage`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("coverage rows = %d, want 1", n)
	}
}
