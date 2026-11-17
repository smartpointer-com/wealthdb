package gold

import (
	"fmt"
	"sort"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

const valueTol = 1e-6

// entity is one computed unit of a grain: its asset accounts and its
// liabilities, its identity, and what every window of it reads alike.
type entity struct {
	grain       *grain
	assets      []*accountData
	liabilities []*accountData
	// src is the entity's silver_source_id: the source every asset shares,
	// "" for the whole, which spans every source.
	src, id, label string
	// value is the entity's value by day (entityValue).
	value func(day int64) (float64, bool)
	// snaps is the union of the assets' real snapshot days, for inception,
	// the canonical headline bucket, and empty- and stale-bucket detection;
	// gap is their median gap (medianSnapGap).
	snaps []int64
	gap   float64
	gapOK bool
}

// entities are the grain's entities over ds, in key order, each with its
// members sorted (groupAccounts). An entity whose every member is hidden
// (entityHidden) is left out: its values and flows live inside every
// aggregate that holds it.
func (ds *returnsDataset) entities(g *grain, excl *ReturnsExclude, hide *ReturnsHide) []*entity {
	groups, order := groupAccounts(g.name, ds.accts, excl)
	out := make([]*entity, 0, len(order))
	for _, key := range order {
		members := groups[key]
		if entityHidden(g, members, hide) {
			continue
		}
		e := &entity{grain: g}
		e.liabilities, e.assets = splitLiabilities(members)
		if len(e.assets) > 0 {
			if !g.whole {
				e.src = e.assets[0].src
			}
			e.id, e.label = g.ident(e.assets[0])
			e.value = entityValue(e.assets)
			e.snaps = unionSnapshotDays(e.assets)
			e.gap, e.gapOK = medianSnapGap(e.snaps)
		}
		out = append(out, e)
	}
	return out
}

// whole is the global grain's entity, nil where it holds no asset.
func (ds *returnsDataset) whole(excl *ReturnsExclude) *entity {
	for _, e := range ds.entities(grainGlobal, excl, nil) {
		if len(e.assets) > 0 {
			return e
		}
	}
	return nil
}

// entityRun is an entity over one window: the window and its flags, and the
// entity's flows there (entityFlows).
type entityRun struct {
	*entity
	p     ReturnParams
	toDay int64
	win   daySpan
	flags []string
	// aggregate: the flows are assembled across accounts, with synthetic
	// onboarding and netting; one account of the single grain is exact.
	aggregate     bool
	flows, rolled []returns.Flow
	tags          []string
}

// run is the entity over p's window, ending no later than toDay; nil where
// the window holds no day.
func (e *entity) run(p ReturnParams, toDay int64) *entityRun {
	from, to, flags := entityWindow(e.assets, p, toDay)
	if to <= from {
		return nil
	}
	r := &entityRun{entity: e, p: p, toDay: toDay, win: daySpan{from, to}, flags: flags,
		aggregate: len(e.assets) > 1 || !e.grain.single}
	r.flows, r.tags, r.rolled = entityFlows(e.assets, p, from, to, r.aggregate)
	return r
}

// rows are the run's per-bucket rows (when --period != total) and its
// since-inception summary row.
func (r *entityRun) rows(fx fxBounds) []ReturnRow {
	p, winFrom, winTo, av, flows := r.p, r.win.from, r.win.to, r.value, r.flows
	base := ReturnRow{
		SilverSourceID: r.src, EntityID: r.id, EntityLabel: r.label,
		StartDay: winFrom, EndDay: winTo,
	}

	// Entity-level quality, carried on the summary row (and the
	// nonpositive-base row that replaces it).
	entityQ := append([]string{}, r.flags...)
	entityQ = append(entityQ, r.tags...)
	entityQ = append(entityQ, regimeFlags(r.assets, winFrom, winTo)...)
	if preFxHistory(r.assets, p.OutCcy, winFrom, fx) {
		entityQ = append(entityQ, "pre_fx_history")
	}
	entityQ = append(entityQ, "after_tax")

	v0, ok0 := av(winFrom)
	if !ok0 || v0 <= valueTol {
		// Non-positive / unpriceable opening base ⇒ no meaningful return.
		row := base
		row.Period, row.IsSummary = "total", true
		row.StartValue = decStrOrNil(v0, ok0)
		if vEnd, okE := av(winTo); okE {
			row.EndValue = decStr(vEnd)
		}
		row.Quality = append(entityQ, "nonpositive_base")
		return []ReturnRow{row}
	}

	var out []ReturnRow

	// Per-bucket display rows (TWR only). prevEmpty tracks whether the preceding
	// bucket carried forward with no fresh snapshot, so the *receiving* bucket
	// (fresh V1, stale carried V0) that over-attributes the accumulated move can
	// be flagged boundary_same_snapshot — distinct from the donor empty_bucket.
	if p.Period != "total" {
		prevEmpty := false
		for _, b := range returns.BucketBoundaries(winFrom, winTo, periodKind(p.Period)) {
			row := bucketRow(base, b[0], b[1], p.Period, av, flows)
			empty := !hasSnapshotIn(r.snaps, b[0], b[1])
			switch {
			case empty:
				row.Quality = append(row.Quality, "empty_bucket", "carried_forward")
			case prevEmpty:
				row.Quality = append(row.Quality, "boundary_same_snapshot")
			}
			// A bucket can hold snapshots yet end on a value far older
			// than the source's own cadence (a feed that died
			// mid-bucket) — invisible to the empty/carried flags.
			if !empty && staleAt(r.snaps, b[1], r.gap, r.gapOK) {
				row.Quality = append(row.Quality, "stale_snapshot")
			}
			prevEmpty = empty
			out = append(out, row)
		}
	}

	// Since-inception summary row, cumulative TWR pinned to the canonical bucket.
	residue := residueEnds(r.assets, p, r.toDay, r.aggregate)
	sr := summaryRow(base, winFrom, winTo, av, flows, r.rolled, r.snaps, residue, r.assets, r.owesOver, p, entityQ)
	if staleAt(r.snaps, winTo, r.gap, r.gapOK) {
		sr.Quality = append(sr.Quality, "stale_snapshot")
	}
	return append(out, sr)
}

// owesOver reports whether the value the chain compounds, the entity's
// assets summed (entityValue), reads below zero on some day of [from, to]:
// debt that outweighs the rest, an overdraft or a margin loan say. A
// liability account the entity holds is no part of that value, so it
// does not count.
func (e *entity) owesOver(from, to int64) bool {
	days := []int64{from}
	for _, a := range e.assets {
		days = a.changeDays(from, to, days)
	}
	for _, day := range days {
		if v, ok := e.value(day); ok && v < -valueTol {
			return true
		}
	}
	return false
}

// entityValue is the aggregate value at a day: each constituent's
// carry-forward value, zeroed from its closure day (ZeroedValue).
func entityValue(assets []*accountData) func(day int64) (float64, bool) {
	return func(day int64) (float64, bool) {
		var sum float64
		var any bool
		for _, a := range assets {
			v, alive := a.valueAt(day)
			if !alive {
				continue
			}
			any = true
			sum += returns.ZeroedValue(v, day, a.closureDay())
		}
		return sum, any
	}
}

func bucketRow(base ReturnRow, bs, be int64, period string, av func(int64) (float64, bool), flows []returns.Flow) ReturnRow {
	r := base
	r.StartDay, r.EndDay = bs, be
	r.Period = PeriodLabel(returns.DayToTimeUTC(be-1), period) // a day inside the bucket
	v0, ok0 := av(bs)
	v1, ok1 := av(be)
	r.StartValue = decStrOrNil(v0, ok0)
	r.EndValue = decStrOrNil(v1, ok1)
	bf := flowsIn(flows, bs, be)
	r.NetFlow = decStr(sumFlows(bf))
	if ok0 && ok1 {
		if twr, ok := returns.ModifiedDietz(v0, v1, bs, be, bf); ok {
			r.TWR = f64(twr)
		} else {
			r.Quality = append(r.Quality, "dietz_degenerate")
		}
	}
	return r
}

// summaryRow is the window's since-inception row. owes says whether the
// entity's value reads below zero over a sub-period (entity.owesOver).
func summaryRow(base ReturnRow, winFrom, winTo int64, av func(int64) (float64, bool), flows, rolled []returns.Flow, snaps []int64, residue map[int64]bool, assets []*accountData, owes func(from, to int64) bool, p ReturnParams, entityQ []string) ReturnRow {
	r := base
	r.Period, r.IsSummary = summaryLabel(winFrom, winTo), true
	v0, _ := av(winFrom)
	v1, _ := av(winTo)
	r.StartValue, r.EndValue = decStr(v0), decStr(v1)
	r.NetFlow = decStr(sumFlowsIn(flows, winFrom, winTo))
	q := append([]string{}, entityQ...)

	// Cumulative TWR chained over the entity's actual valuation (snapshot) days,
	// independent of --period. Snapshot-aligned sub-periods keep every flow in the
	// same bucket as the value change it causes (unvaluedAt), so a flow landing in
	// a gap between sparse snapshots can't manufacture a sub-(-100%) bucket that
	// poisons the geometric chain — a fixed calendar bucket could, when a period
	// boundary falls inside a snapshot gap and splits a flow from its later value
	// realisation (see RETURNS-NOTES §"snapshot-aligned headline").
	//
	// A sub-period that loses more than everything, while the value the chain
	// compounds never reads below zero over it, is no return: a value that
	// stays at zero or above cannot lose more than it held, so the flows and
	// values disagree, as when money paid in has no value yet, or the
	// sub-period is too coarse for the approximation. Chained, such a factor
	// takes the whole window below -100%, so the window's TWR is n/a instead
	// (loss_beyond_total). Where the value reads below zero, the arithmetic
	// stands.
	bounds := canonicalChainBounds(winFrom, winTo, snaps)
	var buckets []returns.Bucket
	degenerate, skipped, beyond := false, false, false
	for _, sp := range chainSubPeriods(bounds, av, rolled, flows) {
		if sp.empty {
			continue
		}
		if residue[sp.to] {
			skipped = true
			continue
		}
		rr, ok := returns.ModifiedDietz(sp.v0, sp.v1, sp.from, sp.to, sp.flows)
		buckets = append(buckets, returns.Bucket{R: rr, OK: ok})
		if !ok {
			degenerate = true
		}
		if ok && returns.BeyondTotalLoss(rr) && !owes(sp.from, sp.to) {
			beyond = true
		}
	}
	if skipped {
		q = append(q, "post_exit_residue")
	}
	if p.Method == "twr" || p.Method == "both" {
		cum, ok := returns.Chain(buckets)
		if len(buckets) == 0 && skipped {
			// A window wholly on a residue: each sub-period a factor of 1.
			cum, ok = 0, true
		}
		switch {
		case ok && !beyond:
			r.TWR = f64(cum)
			days := float64(bounds[len(bounds)-1] - bounds[0])
			if returns.ShouldAnnualize(p.Annualize, days) {
				r.TWRAnnualized = f64(returns.Annualize(cum, days))
			}
		case !ok && degenerate:
			q = append(q, "dietz_degenerate")
		}
		if beyond {
			q = append(q, "loss_beyond_total")
		}
	}

	// MWR over the window, on the values the chain reads: the window's edges with
	// what moved but its accounts do not show yet rolled in. start, end and
	// net_flow stay the carried values, which holdings and the bridge read.
	edges := unvaluedAt([]int64{winFrom, winTo}, rolled)
	edges[0], edges[1] = unshownValue(edges[0]), unshownValue(edges[1])
	if edges[0] != 0 || edges[1] != 0 {
		q = append(q, "rolled_forward")
	}
	if p.Method == "mwr" || p.Method == "both" {
		r.MWR, r.MWRAnnualized, q = computeMWR(v0+edges[0], v1+edges[1], winFrom, winTo, flows, assets, p, q)
	}

	r.Quality = dedupeStrings(q)
	return r
}

// ---- liability + identity -----------------------------------------------

func liabilityRow(a *accountData, toDay int64) ReturnRow {
	from := a.firstDay()
	v0, ok0 := a.valueAt(from)
	v1, ok1 := a.valueAt(toDay)
	return ReturnRow{
		SilverSourceID: a.src, EntityID: a.acct, EntityLabel: a.label,
		Period: "total", IsSummary: true, StartDay: from, EndDay: toDay,
		StartValue: decStrOrNil(v0, ok0), EndValue: decStrOrNil(v1, ok1),
		Quality: []string{"nonpositive_base", "after_tax"},
	}
}

// NoPortfolioLabel names the accounts of a source outside any
// portfolio, wherever a report lists them as one.
const NoPortfolioLabel = "(no portfolio)"

// ---- small helpers -------------------------------------------------------

func periodKind(period string) returns.BucketKind {
	switch period {
	case "monthly":
		return returns.BucketMonthly
	case "annual":
		return returns.BucketAnnual
	case "total":
		return returns.BucketTotal
	default:
		return returns.BucketQuarterly
	}
}

func hasSnapshotIn(snaps []int64, from, to int64) bool {
	i := sort.Search(len(snaps), func(k int) bool { return snaps[k] > from })
	return i < len(snaps) && snaps[i] <= to
}

// monthLabelLayout is a monthly bucket's label, YYYY-MM: the key
// ReconcileStatedPeriods reads the engine's monthly rows by.
const monthLabelLayout = "2006-01"

// PeriodLabel is the calendar label of the period bucket t falls in:
// a day or a week by its date (DuckDB truncates a week to its Monday,
// so a week's first second names it), a month YYYY-MM, a year YYYY, a
// quarter YYYY-Qn. Every report's period column reads it.
func PeriodLabel(t time.Time, period string) string {
	switch period {
	case "daily", "weekly":
		return t.Format("2006-01-02")
	case "monthly":
		return t.Format(monthLabelLayout)
	case "annual":
		return t.Format("2006")
	default: // quarterly
		return fmt.Sprintf("%d-Q%d", t.Year(), (int(t.Month())-1)/3+1)
	}
}

func summaryLabel(from, to int64) string {
	tf := returns.DayToTimeUTC(from)
	tt := returns.DayToTimeUTC(to - 1)
	if tf.Year() == tt.Year() {
		return tf.Format("2006")
	}
	return "total"
}

func f64(v float64) *float64 { return &v }

func decStrOrNil(v float64, ok bool) *string {
	if !ok {
		return nil
	}
	return decStr(v)
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
