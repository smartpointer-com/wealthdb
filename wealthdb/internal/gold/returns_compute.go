package gold

import (
	"fmt"
	"sort"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

const valueTol = 1e-6

// computeEntityReturn produces the per-bucket rows (when --period != total) plus
// the since-inception summary row for one entity built from its constituent
// asset accounts. A single-account accounts-grain entity is the degenerate case:
// no synthetic onboarding, no netting — exact.
func computeEntityReturn(assets []*accountData, p ReturnParams, toDay int64, fx fxBounds) []ReturnRow {
	rows, _ := computeEntityReturnFlows(assets, p, toDay, fx)
	return rows
}

// computeEntityReturnFlows is computeEntityReturn with the entity's flow series
// beside its rows, for a caller that reads both (the bridge).
func computeEntityReturnFlows(assets []*accountData, p ReturnParams, toDay int64, fx fxBounds) ([]ReturnRow, []returns.Flow) {
	// The accounts/sources/portfolios grains group by source, so every asset
	// shares one src; the global grain spans all sources, so there is no single
	// silver_source — assets[0].src would be a non-deterministic map pick.
	src := ""
	if p.Level != "global" {
		src = assets[0].src
	}
	entityID, label := entityIdentity(p.Level, assets)

	winFrom, winTo, incFlags := entityWindow(assets, p, toDay)
	if winTo <= winFrom {
		return nil, nil
	}
	aggregate := len(assets) > 1 || p.Level != "accounts"

	av := entityValue(assets)

	flows, qFlowTags, rolled := entityFlows(assets, p, winFrom, winTo, aggregate)

	// Snapshot-day union (real snapshots) for inception, the canonical headline
	// bucket, and empty- / stale-bucket detection.
	snaps := unionSnapshotDays(assets)
	snapGap, snapGapOK := medianSnapGap(snaps)

	base := ReturnRow{
		SilverSourceID: src, EntityID: entityID, EntityLabel: label,
		StartDay: winFrom, EndDay: winTo,
	}

	// Entity-level quality, carried on the summary row (and the
	// nonpositive-base row that replaces it).
	entityQ := append([]string{}, incFlags...)
	entityQ = append(entityQ, qFlowTags...)
	entityQ = append(entityQ, regimeFlags(assets, winFrom, winTo)...)
	if preFxHistory(assets, p.OutCcy, winFrom, fx) {
		entityQ = append(entityQ, "pre_fx_history")
	}
	entityQ = append(entityQ, "after_tax")

	v0, ok0 := av(winFrom)
	if !ok0 || v0 <= valueTol {
		// Non-positive / unpriceable opening base ⇒ no meaningful return.
		r := base
		r.Period, r.IsSummary = "total", true
		r.StartValue = decStrOrNil(v0, ok0)
		if vEnd, okE := av(winTo); okE {
			r.EndValue = decStr(vEnd)
		}
		r.Quality = append(entityQ, "nonpositive_base")
		return []ReturnRow{r}, flows
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
			empty := !hasSnapshotIn(snaps, b[0], b[1])
			switch {
			case empty:
				row.Quality = append(row.Quality, "empty_bucket", "carried_forward")
			case prevEmpty:
				row.Quality = append(row.Quality, "boundary_same_snapshot")
			}
			// A bucket can hold snapshots yet end on a value far older
			// than the source's own cadence (a feed that died
			// mid-bucket) — invisible to the empty/carried flags.
			if !empty && staleAt(snaps, b[1], snapGap, snapGapOK) {
				row.Quality = append(row.Quality, "stale_snapshot")
			}
			prevEmpty = empty
			out = append(out, row)
		}
	}

	// Since-inception summary row, cumulative TWR pinned to the canonical bucket.
	residue := residueEnds(assets, p, toDay, aggregate)
	sr := summaryRow(base, winFrom, winTo, av, flows, rolled, snaps, residue, assets, p, entityQ)
	if staleAt(snaps, winTo, snapGap, snapGapOK) {
		sr.Quality = append(sr.Quality, "stale_snapshot")
	}
	out = append(out, sr)
	return out, flows
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

func summaryRow(base ReturnRow, winFrom, winTo int64, av func(int64) (float64, bool), flows, rolled []returns.Flow, snaps []int64, residue map[int64]bool, assets []*accountData, p ReturnParams, entityQ []string) ReturnRow {
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
	bounds := canonicalChainBounds(winFrom, winTo, snaps)
	var buckets []returns.Bucket
	degenerate, skipped := false, false
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
		if ok {
			r.TWR = f64(cum)
			days := float64(bounds[len(bounds)-1] - bounds[0])
			if returns.ShouldAnnualize(p.Annualize, days) {
				r.TWRAnnualized = f64(returns.Annualize(cum, days))
			}
		} else if degenerate {
			q = append(q, "dietz_degenerate")
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

func entityIdentity(level string, assets []*accountData) (id, label string) {
	a := assets[0]
	switch level {
	case "accounts":
		return a.acct, a.label
	case "sources":
		return a.src, a.src
	case "portfolios":
		if a.portfolio == "" {
			return "", NoPortfolioLabel
		}
		// EntityID stays the stable portfolio_external_id; the label prefers the
		// resolved display name (falling back to the id when unresolved).
		if a.portfolioName != "" {
			return a.portfolio, a.portfolioName
		}
		return a.portfolio, a.portfolio
	default:
		return "", "global"
	}
}

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
