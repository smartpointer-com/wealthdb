package gold

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedMaterializeFixture builds a two-source synthetic gold DB (a
// flow-complete brokerage in a portfolio plus a portfolio-less account on the
// pre-seeded 'test-src') with USD/CHF and USD/EUR rates, so every grain has
// entities and all three output currencies resolve. Returns the window end.
func seedMaterializeFixture(t *testing.T, db *sql.DB, ctx context.Context) int64 {
	t.Helper()
	seedReturnsSource(t, db, ctx, "src-a", "schwab")

	t0, tMid, t1 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)
	pf := "PF1"
	seedAcct(t, db, ctx, "src-a", "A1", canonical.AccountKindBrokerage, &pf,
		[]snap{{t0, 1000}, {t1, 1200}}, []txn{{tMid, canonical.TxKindDeposit, 100}})
	seedAcct(t, db, ctx, "test-src", "B1", canonical.AccountKindBrokerage, nil,
		[]snap{{t0, 500}, {t1, 560}}, nil)

	seedFX(t, db, dy(2024, time.January, 1), "USD", "CHF", "1.10")
	seedFX(t, db, dy(2024, time.January, 1), "USD", "EUR", "1.05")

	return time.Date(2024, time.July, 2, 23, 59, 59, 0, time.UTC).Unix()
}

// TestMaterializeReturnsMatrix verifies one run populates every
// (grain, granularity, currency) partition and reports the row count.
func TestMaterializeReturnsMatrix(t *testing.T) {
	db, ctx := openMigrated(t)
	end := seedMaterializeFixture(t, db, ctx)

	n, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1000})
	if err != nil {
		t.Fatalf("MaterializeReturns: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT grain, granularity, currency, COUNT(*) FROM report_returns GROUP BY 1, 2, 3`)
	if err != nil {
		t.Fatalf("partition counts: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var grain, granularity, ccy string
		var c int
		if err := rows.Scan(&grain, &granularity, &ccy, &c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[grain+"/"+granularity+"/"+ccy] = c
		total += c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, grain := range materializeGrains {
		for _, period := range materializePeriods {
			for _, ccy := range materializeCurrencies {
				if counts[grain+"/"+period+"/"+ccy] == 0 {
					t.Errorf("empty partition (%s, %s, %s)", grain, period, ccy)
				}
			}
		}
	}
	if want := len(materializeGrains) * len(materializePeriods) * len(materializeCurrencies); len(counts) != want {
		t.Errorf("got %d partitions, want %d", len(counts), want)
	}
	if total != n {
		t.Errorf("table has %d rows, MaterializeReturns reported %d", total, n)
	}

	// The Period parameter must actually thread through to each run: 'total'
	// partitions carry only summary rows, and the finer the granularity the
	// more bucket rows — a Period stuck on one value would flunk both.
	var strayBuckets int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM report_returns WHERE granularity = 'total' AND NOT is_summary`).
		Scan(&strayBuckets); err != nil {
		t.Fatalf("total-granularity check: %v", err)
	}
	if strayBuckets != 0 {
		t.Errorf("'total' partitions carry %d bucket rows, want summary rows only", strayBuckets)
	}
	buckets := func(granularity string) int {
		var c int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM report_returns
			  WHERE grain = 'global' AND currency = 'USD' AND granularity = ? AND NOT is_summary`,
			granularity).Scan(&c); err != nil {
			t.Fatalf("bucket count (%s): %v", granularity, err)
		}
		return c
	}
	m, q, a := buckets("monthly"), buckets("quarterly"), buckets("annual")
	if !(m > q && q > a) {
		t.Errorf("bucket counts not strictly finer by granularity: monthly=%d quarterly=%d annual=%d", m, q, a)
	}
}

// materializedRow is one report_returns row read back for comparison, money
// columns cast to DOUBLE (the engine's decimal strings round-trip exactly at
// 2dp through DECIMAL(28,4)).
type materializedRow struct {
	src, entityID, entityLabel, period     string
	isSummary                              bool
	startDay, endDay                       int64
	startValue, endValue, netFlow          sql.NullFloat64
	twr, twrAnnualized, mwr, mwrAnnualized sql.NullFloat64
	quality                                string
}

func matchMoney(t *testing.T, key, col string, got sql.NullFloat64, want *string) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Errorf("%s: %s = %v, want NULL", key, col, got.Float64)
		}
		return
	}
	w, err := strconv.ParseFloat(*want, 64)
	if err != nil {
		t.Fatalf("%s: bad engine decimal %q", key, *want)
	}
	if !got.Valid || math.Abs(got.Float64-w) > 1e-6 {
		t.Errorf("%s: %s = %v, want %s", key, col, got, *want)
	}
}

func matchRate(t *testing.T, key, col string, got sql.NullFloat64, want *float64) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Errorf("%s: %s = %v, want NULL", key, col, got.Float64)
		}
		return
	}
	if !got.Valid || math.Abs(got.Float64-*want) > 1e-12 {
		t.Errorf("%s: %s = %v, want %v", key, col, got, *want)
	}
}

// TestMaterializeReturnsVerbatimAllPartitions checks the core contract row for
// row across EVERY (grain, granularity, currency) partition: each must equal
// the output of one RunReturns call with the same CLI-default parameters. This
// exercises the single-pass multi-currency loader (CHF/EUR, not just USD) and
// the batched insert end to end.
func TestMaterializeReturnsVerbatimAllPartitions(t *testing.T) {
	db, ctx := openMigrated(t)
	end := seedMaterializeFixture(t, db, ctx)

	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1000}); err != nil {
		t.Fatalf("MaterializeReturns: %v", err)
	}

	checked := 0
	for _, ccy := range materializeCurrencies {
		for _, grain := range materializeGrains {
			for _, period := range materializePeriods {
				want, err := RunReturns(ctx, db, ReturnParams{
					Level: grain, FromEpoch: 0, ToEpoch: end, OutCcy: ccy,
					Method: "both", Period: period, Annualize: "auto", Netting: true, Inception: "full",
				})
				if err != nil {
					t.Fatalf("RunReturns %s/%s/%s: %v", grain, period, ccy, err)
				}

				// window_from_year = 0 is the since-inception base matrix; the
				// 'total' granularity also holds windowed (>0) summaries.
				rows, err := db.QueryContext(ctx, `
					SELECT silver_source_id, entity_id, entity_label, period, is_summary,
					       start_day, end_day,
					       CAST(start_value AS DOUBLE), CAST(end_value AS DOUBLE), CAST(net_flow AS DOUBLE),
					       twr, twr_annualized, mwr, mwr_annualized, quality
					  FROM report_returns
					 WHERE grain = ? AND granularity = ? AND currency = ? AND window_from_year = 0`, grain, period, ccy)
				if err != nil {
					t.Fatalf("read partition %s/%s/%s: %v", grain, period, ccy, err)
				}
				got := map[string]materializedRow{}
				for rows.Next() {
					var m materializedRow
					if err := rows.Scan(&m.src, &m.entityID, &m.entityLabel, &m.period, &m.isSummary,
						&m.startDay, &m.endDay, &m.startValue, &m.endValue, &m.netFlow,
						&m.twr, &m.twrAnnualized, &m.mwr, &m.mwrAnnualized, &m.quality); err != nil {
						rows.Close()
						t.Fatalf("scan: %v", err)
					}
					got[m.src+"|"+m.entityID+"|"+m.period+"|"+strconv.FormatBool(m.isSummary)] = m
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				rows.Close()

				part := grain + "/" + period + "/" + ccy
				if len(got) != len(want) {
					t.Errorf("%s: partition has %d rows, RunReturns produced %d", part, len(got), len(want))
					continue
				}
				for _, r := range want {
					key := r.SilverSourceID + "|" + r.EntityID + "|" + r.Period + "|" + strconv.FormatBool(r.IsSummary)
					m, ok := got[key]
					if !ok {
						t.Errorf("%s: missing materialized row %s", part, key)
						continue
					}
					id := part + " " + key
					if m.entityLabel != r.EntityLabel {
						t.Errorf("%s: label %q, want %q", id, m.entityLabel, r.EntityLabel)
					}
					if m.startDay != r.StartDay*86400 || m.endDay != r.EndDay*86400 {
						t.Errorf("%s: days (%d, %d), want (%d, %d)",
							id, m.startDay, m.endDay, r.StartDay*86400, r.EndDay*86400)
					}
					matchMoney(t, id, "start_value", m.startValue, r.StartValue)
					matchMoney(t, id, "end_value", m.endValue, r.EndValue)
					matchMoney(t, id, "net_flow", m.netFlow, r.NetFlow)
					matchRate(t, id, "twr", m.twr, r.TWR)
					matchRate(t, id, "twr_annualized", m.twrAnnualized, r.TWRAnnualized)
					matchRate(t, id, "mwr", m.mwr, r.MWR)
					matchRate(t, id, "mwr_annualized", m.mwrAnnualized, r.MWRAnnualized)
					if m.quality != strings.Join(r.Quality, ";") {
						t.Errorf("%s: quality %q, want %q", id, m.quality, strings.Join(r.Quality, ";"))
					}
				}
				checked++
			}
		}
	}
	if want := len(materializeCurrencies) * len(materializeGrains) * len(materializePeriods); checked != want {
		t.Errorf("checked %d partitions, want %d", checked, want)
	}
}

// TestMaterializeReturnsRerunReplaces verifies a second run fully replaces
// the first: same row count, no leftover rows from the earlier computed_at.
func TestMaterializeReturnsRerunReplaces(t *testing.T) {
	db, ctx := openMigrated(t)
	end := seedMaterializeFixture(t, db, ctx)

	n1, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 1000})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	n2, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: end, ComputedAt: 2000})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n1 != n2 {
		t.Errorf("row counts differ across identical runs: %d vs %d", n1, n2)
	}

	var total int
	var stamps int
	var stamp int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT computed_at), MAX(computed_at) FROM report_returns`).
		Scan(&total, &stamps, &stamp); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != n2 {
		t.Errorf("table has %d rows after rerun, want %d", total, n2)
	}
	if stamps != 1 || stamp != 2000 {
		t.Errorf("computed_at not fully replaced: %d distinct stamps, max %d", stamps, stamp)
	}
}
