package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// The net worth bridge: why net worth moved over a bucket
// (docs/RETURNS-NOTES.md, "The net worth bridge"). Its rows add up to the
// cent because every figure is rounded once, on a month, and a coarser
// bucket sums whole months:
//
//	end_net_worth = start_net_worth + net_saved + onboarded + gain

// BridgeRow is one bucket of `wealthdb returns bridge`. Money columns are
// decimal strings in the output currency, with two decimals.
type BridgeRow struct {
	Period           string // "2025-03", "2025-Q1", "2025" or "total"
	StartDay, EndDay int64  // the bucket's edges, epoch days

	StartNetWorth *string
	EndNetWorth   *string
	NetFlow       *string // returns' real flows, less any tracking move
	Onboarded     *string // value that entered or left tracking, less the same
	DebtRepaid    *string // the fall in what is owed; negative when debt rose
	NetSaved      *string // NetFlow + DebtRepaid
	Gain          *string // the residual: returns' gain plus the FX move on debt
	Gap           *string // net worth less returns' value less the liabilities

	Quality []string
}

// bridgeMonth is one month of the bridge, in cents. The first month starts
// at the window's first day and the last ends on the spine's last day, so
// those two can be part months, as `returns --period monthly` has them.
type bridgeMonth struct {
	startDay, endDay                                    int64
	startNW, endNW, netFlow, onboarded, debtRepaid, gap int64
	tags                                                []string // the bucket's own and the bridge's
}

// debtLine is one liability's balance in one currency: an ascending daily
// series, negative when owed.
type debtLine struct {
	src, acct, ccy string
	series         []dayVal
}

func (d *debtLine) firstDay() int64 { return d.series[0].day }
func (d *debtLine) lastDay() int64  { return d.series[len(d.series)-1].day }

// owedAt is what the line owes at the end of day: nothing before its first
// row or after its last, as net worth reads it.
func (d *debtLine) owedAt(day int64) float64 {
	a := accountData{series: d.series}
	if v, alive := a.valueAt(day); alive {
		return -v
	}
	return 0
}

// bridgeInput is what the bridge reads for one currency: the returns
// dataset, every liability line, and each line currency's rate by day.
type bridgeInput struct {
	ds    *returnsDataset
	debts []*debtLine
	rates map[string]map[int64]float64
}

// bridgeTol is the cent fraction below which a gap or a balance is rounding.
const bridgeTol = 0.005

// RunBridge computes the bridge. p carries the output currency, the
// config's returns settings, the window (whole months; FromEpoch <= 0 is
// since inception) and the bucket size in Period.
func RunBridge(ctx context.Context, db *sql.DB, p ReturnParams) ([]BridgeRow, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return nil, err
	}
	ds, err := loadReturnsDataset(ctx, db, p.OutCcy, fx, p.PolicyOverrides, p.TransferMatching)
	if err != nil {
		return nil, err
	}
	debts, err := loadDebtLines(ctx, db)
	if err != nil {
		return nil, err
	}
	rates, err := loadDebtRates(ctx, db, p.OutCcy)
	if err != nil {
		return nil, err
	}
	return computeBridge(bridgeInput{ds: ds, debts: debts, rates: rates}, p), nil
}

// loadDebtLines reads every liability line's daily balance.
func loadDebtLines(ctx context.Context, db *sql.DB) ([]*debtLine, error) {
	rows, err := db.QueryContext(ctx, `SELECT day, silver_source_id, account_external_id, currency,
		CAST(balance AS DOUBLE) FROM report_debt_history()`)
	if err != nil {
		return nil, fmt.Errorf("bridge debts: %w", err)
	}
	defer rows.Close()
	var out []*debtLine
	var cur *debtLine
	for rows.Next() {
		var (
			day           int64
			src, acct, cc string
			balance       float64
		)
		if err := rows.Scan(&day, &src, &acct, &cc, &balance); err != nil {
			return nil, fmt.Errorf("bridge debts scan: %w", err)
		}
		if cur == nil || cur.src != src || cur.acct != acct || cur.ccy != cc {
			cur = &debtLine{src: src, acct: acct, ccy: cc}
			out = append(out, cur)
		}
		cur.series = append(cur.series, dayVal{day: day, val: balance})
	}
	return out, rows.Err()
}

// loadDebtRates reads each liability currency's rate to outCcy by day. A
// day with no path to outCcy is absent.
func loadDebtRates(ctx context.Context, db *sql.DB, outCcy string) (map[string]map[int64]float64, error) {
	rows, err := db.QueryContext(ctx, `SELECT day, currency, rate FROM report_debt_fx(?) WHERE rate IS NOT NULL`, outCcy)
	if err != nil {
		return nil, fmt.Errorf("bridge rates: %w", err)
	}
	defer rows.Close()
	out := map[string]map[int64]float64{}
	for rows.Next() {
		var (
			day  int64
			ccy  string
			rate float64
		)
		if err := rows.Scan(&day, &ccy, &rate); err != nil {
			return nil, fmt.Errorf("bridge rates scan: %w", err)
		}
		if out[ccy] == nil {
			out[ccy] = map[int64]float64{}
		}
		out[ccy][day] = rate
	}
	return out, rows.Err()
}

// computeBridge is the bridge's rows: the whole history's months, then
// those the window holds, summed into p.Period's buckets.
func computeBridge(in bridgeInput, p ReturnParams) []BridgeRow {
	assets := bridgeAssets(in.ds, p)
	months := in.months(assets, p)
	var firstDay int64
	if len(months) > 0 {
		firstDay = months[0].startDay
	}
	var out []BridgeRow
	var bucket []bridgeMonth
	flush := func() {
		if len(bucket) == 0 {
			return
		}
		row := sumBridgeMonths(bucket, p.Period)
		row.Quality = dedupeStrings(append(in.windowTags(assets, p, bucket, firstDay), bucketTags(bucket)...))
		out = append(out, row)
		bucket = nil
	}
	for _, m := range selectBridgeMonths(months, p.FromEpoch, p.ToEpoch) {
		if len(bucket) > 0 && bridgeLabel(bucket[0].endDay, p.Period) != bridgeLabel(m.endDay, p.Period) {
			flush()
		}
		bucket = append(bucket, m)
	}
	flush()
	return out
}

// bridgeAssets is what the global grain of returns measures.
func bridgeAssets(ds *returnsDataset, p ReturnParams) []*accountData {
	groups, _ := groupAccounts("global", ds.accts, p.ReturnsExclude)
	_, assets := splitLiabilities(groups["global"])
	return assets
}

// bridgeRunParams is p as the since-inception global run the months come
// from: the run `returns global` makes with its default settings.
func bridgeRunParams(p ReturnParams) ReturnParams {
	p.Level, p.FromEpoch, p.Method, p.Period, p.Annualize = "global", 0, "twr", "monthly", "never"
	p.Netting, p.Inception = true, "full"
	return p
}

// months computes the history's months from one since-inception run of the
// global grain. The window never reaches this far, so a month reads the
// same in every window and months add up to quarters and years.
func (in bridgeInput) months(assets []*accountData, p ReturnParams) []bridgeMonth {
	if len(assets) == 0 {
		return nil
	}
	run := bridgeRunParams(p)
	toDay := in.ds.globalMax
	run.ToEpoch = toDay * SecondsPerDay
	winFrom, winTo, _ := entityWindow(assets, run, toDay)
	if winTo <= winFrom {
		return nil
	}
	flows, _, _ := entityFlows(assets, run, winFrom, winTo, true)
	bucketQuality := map[int64][]string{}
	for _, r := range computeEntityReturn(assets, run, toDay, in.ds.fx) {
		if !r.IsSummary {
			bucketQuality[r.EndDay] = r.Quality
		}
	}
	av := entityValue(assets)
	var out []bridgeMonth
	for _, b := range returns.BucketBoundaries(winFrom, winTo, returns.BucketMonthly) {
		m := bridgeMonth{startDay: b[0], endDay: b[1], tags: bucketQuality[b[1]]}
		m.startNW, m.endNW = toCents(in.ds.holdings.netWorth[b[0]]), toCents(in.ds.holdings.netWorth[b[1]])
		var real, synthetic float64
		monthFlows := flowsIn(flows, b[0], b[1])
		for _, f := range monthFlows {
			if f.Synthetic {
				synthetic += f.Amount
			} else {
				real += f.Amount
			}
		}
		m.netFlow, m.onboarded = toCents(real), toCents(synthetic)
		if moved := trackingMove(m.netFlow, m.onboarded); moved != 0 {
			m.netFlow += moved
			m.onboarded -= moved
			m.tags = append(append([]string{}, m.tags...), "tracking_netted")
		}
		repaid, debtTags := in.debtRepaid(b[0], b[1], monthFlows)
		m.debtRepaid = toCents(repaid)
		m.tags = append(append([]string{}, m.tags...), debtTags...)
		m.gap = toCents(in.worstGap(av, b[0], b[1]))
		if m.gap != 0 {
			m.tags = append(m.tags, "bridge_gap")
		}
		out = append(out, m)
	}
	return out
}

// trackingMove is what moves from onboarded to net_flow in a month, in cents.
// Money that leaves tracked accounts for an account tracking first sees that
// month finds no partner, and shows up as the new account's first value. Read
// apart, that is a fall in saving beside a rise in tracking. Where the month's
// net real flow and net synthetic flow have opposite signs, the smaller size
// is one movement and leaves both, whichever way it ran. The synthetic flows
// net first, as a closure beside a debut is value going from one account to
// another. The measure is the net real flow, since a bank can book such a
// move as a plain withdrawal. The sum of the two columns does not change.
func trackingMove(netFlow, onboarded int64) int64 {
	switch {
	case netFlow < 0 && onboarded > 0:
		return min(-netFlow, onboarded)
	case netFlow > 0 && onboarded < 0:
		return -min(netFlow, -onboarded)
	}
	return 0
}

// debtRepaid is the fall in what is owed from one day to the other, each
// line measured in its own currency and converted at the later day's rate,
// so an FX move on a debt stays out of it. The tags say where the fall
// rests on something other than a payment. flows are the month's.
func (in bridgeInput) debtRepaid(from, to int64, flows []returns.Flow) (float64, []string) {
	var repaid float64
	var tags []string
	for _, d := range in.debts {
		fall := d.owedAt(from) - d.owedAt(to)
		if fall == 0 {
			continue
		}
		rate, ok := in.rates[d.ccy][to]
		if !ok {
			tags = append(tags, "debt_unpriced")
			continue
		}
		repaid += fall * rate
		first, last := d.firstDay(), d.lastDay()
		if first > from && first <= to {
			if owed := d.owedAt(first) * rate; owed > bridgeTol && !cashBeside(flows, owed) {
				tags = append(tags, "debt_first_seen")
			}
		}
		if last >= from && last < to && d.owedAt(last) > bridgeTol {
			tags = append(tags, "debt_ended")
		}
	}
	return repaid, tags
}

// cashBeside reports whether a real inflow matches a new debt, within the
// tolerance that pairs two legs of a transfer: the loan's own cash.
func cashBeside(flows []returns.Flow, owed float64) bool {
	for _, f := range flows {
		eps := math.Max(nettingEpsFloor, nettingEpsRel*math.Max(f.Amount, owed))
		if !f.Synthetic && math.Abs(f.Amount-owed) <= eps {
			return true
		}
	}
	return false
}

// worstGap is the larger of the two edges' gaps: net worth less returns'
// value less the mortgages and cards. It is zero unless returns leaves an
// asset out, as returns_exclude does, or a day could not be priced.
func (in bridgeInput) worstGap(av func(int64) (float64, bool), from, to int64) float64 {
	worst := 0.0
	for _, day := range []int64{from, to} {
		if g := in.gapAt(av, day); math.Abs(g) > math.Abs(worst) {
			worst = g
		}
	}
	if math.Abs(worst) < bridgeTol {
		return 0
	}
	return worst
}

func (in bridgeInput) gapAt(av func(int64) (float64, bool), day int64) float64 {
	gap := in.ds.holdings.netWorth[day]
	if v, ok := av(day); ok {
		gap -= v
	}
	for _, a := range in.ds.accts {
		if a.kind == string(canonical.AccountKindMortgage) {
			if v, ok := a.valueAt(day); ok {
				gap -= v
			}
		}
	}
	for _, c := range in.ds.holdings.cards {
		if v, ok := c.valueAt(day); ok {
			gap -= v
		}
	}
	return gap
}

// windowTags are returns' tags for the bucket's own window: what
// `returns global FROM TO --period total` flags. They carry first, as the
// summary row's do, and the window's opening tag joins the first bucket.
func (in bridgeInput) windowTags(assets []*accountData, p ReturnParams, bucket []bridgeMonth, firstDay int64) []string {
	start, end := bucket[0].startDay, bucket[len(bucket)-1].endDay
	w := bridgeRunParams(p)
	w.FromEpoch, w.ToEpoch, w.Period = start*SecondsPerDay, end*SecondsPerDay, "total"
	var tags []string
	if start == firstDay {
		switch {
		case p.FromEpoch <= 0:
			tags = append(tags, "since_data_inception")
		case EpochDay(p.FromEpoch) < firstDay:
			tags = append(tags, "partial_window")
		}
	}
	if rows := computeEntityReturn(assets, w, end, in.ds.fx); len(rows) > 0 {
		tags = append(tags, rows[len(rows)-1].Quality...)
	}
	return tags
}

func bucketTags(bucket []bridgeMonth) []string {
	var tags []string
	for _, m := range bucket {
		tags = append(tags, m.tags...)
	}
	return tags
}

// selectBridgeMonths keeps the months whose calendar month lies inside the
// window. FromEpoch <= 0 reads as the first month.
func selectBridgeMonths(months []bridgeMonth, fromEpoch, toEpoch int64) []bridgeMonth {
	fromDay, toDay := EpochDay(fromEpoch), EpochDay(toEpoch)
	var out []bridgeMonth
	for _, m := range months {
		if (fromEpoch <= 0 || monthStartDay(m.endDay) >= fromDay) && m.endDay <= toDay {
			out = append(out, m)
		}
	}
	return out
}

func monthStartDay(day int64) int64 {
	t := returns.DayToTimeUTC(day)
	return EpochDay(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Unix())
}

// bridgeLabel names the bucket a month belongs to, by the month's end: a
// part month after the last month end is a month of its own.
func bridgeLabel(endDay int64, period string) string {
	if period == "total" {
		return "total"
	}
	return periodLabel(endDay+1, period)
}

// sumBridgeMonths adds a bucket's months. The start is the first month's,
// the end the last's, and gain is what the identity leaves, in cents.
func sumBridgeMonths(months []bridgeMonth, period string) BridgeRow {
	first, last := months[0], months[len(months)-1]
	var netFlow, onboarded, repaid, gap int64
	for _, m := range months {
		netFlow += m.netFlow
		onboarded += m.onboarded
		repaid += m.debtRepaid
		if absCents(m.gap) > absCents(gap) {
			gap = m.gap
		}
	}
	netSaved := netFlow + repaid
	return BridgeRow{
		Period: bridgeLabel(last.endDay, period), StartDay: first.startDay, EndDay: last.endDay,
		StartNetWorth: centsStr(first.startNW), EndNetWorth: centsStr(last.endNW),
		NetFlow: centsStr(netFlow), Onboarded: centsStr(onboarded), DebtRepaid: centsStr(repaid),
		NetSaved: centsStr(netSaved), Gain: centsStr(last.endNW - first.startNW - netSaved - onboarded),
		Gap: centsStr(gap),
	}
}

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }

func absCents(c int64) int64 {
	if c < 0 {
		return -c
	}
	return c
}

// centsStr renders cents as a decimal string with two decimals.
func centsStr(c int64) *string {
	sign := ""
	if c < 0 {
		sign = "-"
	}
	s := fmt.Sprintf("%s%d.%02d", sign, absCents(c)/100, absCents(c)%100)
	return &s
}

// ---- materialization -----------------------------------------------------

// bridgeInsertSQL writes one report_bridge row.
const bridgeInsertSQL = `INSERT INTO report_bridge (computed_at, currency, period, start_day, end_day,
    start_net_worth, end_net_worth, net_flow, onboarded, debt_repaid, net_saved, gain, bridge_gap, quality)
    VALUES (?, ?, ?, ?, ?, CAST(? AS DECIMAL(24,2)), CAST(? AS DECIMAL(24,2)), CAST(? AS DECIMAL(24,2)),
            CAST(? AS DECIMAL(24,2)), CAST(? AS DECIMAL(24,2)), CAST(? AS DECIMAL(24,2)),
            CAST(? AS DECIMAL(24,2)), CAST(? AS DECIMAL(24,2)), ?)`

// bridgeMaterial is each reporting currency's monthly rows, ready to write.
type bridgeMaterial map[string][]BridgeRow

// computeMaterialBridge is every reporting currency's monthly bridge over
// the loaded datasets: the rows of `returns bridge --period monthly`.
func computeMaterialBridge(ctx context.Context, db *sql.DB, datasets map[string]*returnsDataset, p MaterializeParams) (bridgeMaterial, error) {
	debts, err := loadDebtLines(ctx, db)
	if err != nil {
		return nil, err
	}
	out := bridgeMaterial{}
	for _, ccy := range materializeCurrencies {
		rates, err := loadDebtRates(ctx, db, ccy)
		if err != nil {
			return nil, err
		}
		out[ccy] = computeBridge(bridgeInput{ds: datasets[ccy], debts: debts, rates: rates}, ReturnParams{
			ToEpoch: p.ToEpoch, OutCcy: ccy, Period: "monthly",
			InceptionOverrides: p.InceptionOverrides, ReturnsExclude: p.ReturnsExclude, ReturnsHide: p.ReturnsHide,
		})
	}
	return out, nil
}

// write replaces report_bridge's rows.
func (m bridgeMaterial) write(ctx context.Context, tx *sql.Tx, computedAt int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM report_bridge`); err != nil {
		return fmt.Errorf("report_bridge delete: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, bridgeInsertSQL)
	if err != nil {
		return fmt.Errorf("report_bridge prepare: %w", err)
	}
	defer stmt.Close()
	for _, ccy := range materializeCurrencies {
		for _, r := range m[ccy] {
			if _, err := stmt.ExecContext(ctx, computedAt, ccy, r.Period, r.StartDay*SecondsPerDay, r.EndDay*SecondsPerDay,
				*r.StartNetWorth, *r.EndNetWorth, *r.NetFlow, *r.Onboarded, *r.DebtRepaid, *r.NetSaved, *r.Gain, *r.Gap,
				strings.Join(r.Quality, ";")); err != nil {
				return fmt.Errorf("report_bridge insert: %w", err)
			}
		}
	}
	return nil
}
