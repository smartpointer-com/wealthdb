package gold

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"slices"
	"sync"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// benchmarkMonths is the benchmark tenor of a curve that has it: three
// months; a curve without it takes its shortest.
const benchmarkMonths = 3

// curveAgeDays bounds how old a curve's newest observation may be to
// stand for the instant.
const curveAgeDays = 31

// readYieldCurves reads each currency's curve at the instant: the newest
// observation of each tenor's series within curveAgeDays, the basis its
// payload states (fixedincome.ParseBasis: none is the Treasury's), and
// the zero curve built on that basis. The benchmark is the zero curve's
// rate at the benchmark tenor, in the curve's semiannual convention; on
// the Treasury basis that is the tenor as published.
//
// A curve whose tenors state two bases, a basis ParseBasis refuses, or a
// curve on another basis that NewCurve refuses gives no curve at all: its
// rates are of no known convention. A Treasury curve that does not
// bootstrap still gives its benchmark as published. Each refusal logs one
// line naming the currency (warnCurve).
func readYieldCurves(ctx context.Context, db querier, in *yield.Inputs) error {
	type tenors struct {
		points []fixedincome.Point
		basis  fixedincome.Basis
		// refused is why the tenors give no curve: a basis ParseBasis
		// refuses, or a tenor that states another basis than the first.
		refused error
	}
	curves := map[string]*tenors{}
	if err := scanAll(ctx, db, `
SELECT c.currency, c.tenor_months, o.value::DOUBLE,
       coalesce(json_extract_string(o.payload, '$.basis'), ''),
       coalesce(json_extract_string(o.payload, '$.compounding'), '')
  FROM yield_curves c
  JOIN rate_observations o ON o.silver_source_id = c.silver_source_id AND o.series_id = c.series_id
 WHERE o.as_of <= $1 AND o.as_of > $1 - $2 * 86400
QUALIFY row_number() OVER (PARTITION BY c.currency, c.tenor ORDER BY o.as_of DESC) = 1
 ORDER BY 1, 2`, func(rows *sql.Rows) error {
		var ccy, kind, compounding string
		var p fixedincome.Point
		if err := rows.Scan(&ccy, &p.Months, &p.Rate, &kind, &compounding); err != nil {
			return err
		}
		b, err := fixedincome.ParseBasis(kind, compounding)
		t := curves[ccy]
		if t == nil {
			t = &tenors{basis: b}
			curves[ccy] = t
		}
		t.points = append(t.points, p)
		switch {
		case t.refused != nil:
		case err != nil:
			t.refused = err
		case b != t.basis:
			t.refused = fmt.Errorf("its tenors state two bases, %v and %v", t.basis, b)
		}
		return nil
	}, in.AsOf, curveAgeDays); err != nil {
		return err
	}
	for ccy, t := range curves {
		if t.refused != nil {
			warnCurve("yield: no %s curve: %v", ccy, t.refused)
			continue
		}
		i := slices.IndexFunc(t.points, func(p fixedincome.Point) bool { return p.Months == benchmarkMonths })
		if i < 0 {
			i = 0
		}
		c := &yield.Curve{Benchmark: t.points[i].Rate, BenchmarkMonths: t.points[i].Months}
		zero, err := fixedincome.NewCurve(t.points, t.basis)
		switch {
		case err != nil && t.basis == fixedincome.Treasury:
			warnCurve("yield: the %s curve does not bootstrap; it gives its benchmark alone: %v", ccy, err)
		case err != nil:
			warnCurve("yield: no %s curve: %v", ccy, err)
			continue
		case t.basis != fixedincome.Treasury:
			c.Zero, c.Benchmark = zero, zero.Zero(c.BenchmarkMonths)
		default:
			c.Zero = zero
		}
		in.Curves[ccy] = c
	}
	return nil
}

// curveWarned holds the curve refusals already logged: one instant's
// refusal is every instant's, and a refresh reads many.
var curveWarned = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// logCurve is where warnCurve writes; a test swaps it.
var logCurve = log.Printf

// warnCurve logs a refused curve's line once per process.
func warnCurve(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	curveWarned.Lock()
	defer curveWarned.Unlock()
	if !curveWarned.seen[line] {
		curveWarned.seen[line] = true
		logCurve("%s", line)
	}
}
