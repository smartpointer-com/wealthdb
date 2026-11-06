package gold

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// feeDaysPerYear annualizes a fee rate.
const feeDaysPerYear = 365

// FeeRow is one entity of `wealthdb spending fees` over one bucket: what it
// paid and what it held. The entity is a portfolio, else an account outside
// any portfolio (docs/SPENDING.md §7). Money columns are decimal strings in
// the output currency, a rate a ratio, nil where the row has none.
type FeeRow struct {
	Period           string // "2025-03", "2025-Q1", "2025" or "total"
	StartDay, EndDay int64  // the bucket's first and last day, epoch days

	SilverSourceID string
	EntityKind     string // portfolio | account
	EntityID       string
	EntityLabel    string

	Fees           *string // net of refunds
	FeesInvestment *string
	FeesBank       *string
	FeesATM        *string
	FeesOther      *string

	MeanValue *string  // mean over the days the entity held a value
	FeeRate   *float64 // Fees over MeanValue, annualized
	TWR       *float64 // the months that end in the bucket, chained

	Quality []string
}

// feeBucket is a window's calendar bucket, first and last day inclusive.
type feeBucket struct {
	label      string
	start, end int64
}

// feeBuckets cuts [from, to] into calendar buckets. A bucket keeps its
// calendar label where the window clips it.
func feeBuckets(from, to int64, period string) []feeBucket {
	if period == "total" {
		return []feeBucket{{"total", from, to}}
	}
	var out []feeBucket
	for start := from; start <= to; {
		t := time.Unix(start*SecondsPerDay, 0).UTC()
		var next time.Time
		switch period {
		case "monthly":
			next = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case "annual":
			next = time.Date(t.Year()+1, time.January, 1, 0, 0, 0, 0, time.UTC)
		default: // quarterly
			next = time.Date(t.Year(), time.Month((int(t.Month())-1)/3*3+4), 1, 0, 0, 0, 0, time.UTC)
		}
		end := min(EpochDay(next.Unix())-1, to)
		out = append(out, feeBucket{periodLabel(end+1, period), start, end})
		start = end + 1
	}
	return out
}

// feeEntity accumulates one entity over one bucket, in the output currency.
type feeEntity struct {
	src, kind, id, label string
	fees                 map[string]float64 // by group; a refund counts against it
	unpriced             int
	spans                []*accountData
}

// feeGroups are the detailed groups of migration 0177's fee_group, in
// column order.
var feeGroups = []string{"investment", "bank", "atm", "other"}

// feeLine is one spending line of primary BANK_FEES.
type feeLine struct {
	src, acct, portfolio, label string
	day                         int64
	group                       string
	cost                        *float64 // positive for a fee, negative for a refund; nil with no rate
}

// loadFeeLines reads the window's spending lines of primary BANK_FEES, each
// with its account's portfolio and its detailed group, turned to a cost.
func loadFeeLines(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string) ([]feeLine, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.silver_source_id, t.account_external_id, COALESCE(a.portfolio_external_id, ''),
		       account_display_name(a.display_name, t.account_external_id),
		       t.occurred_at, fee_group(t.spend_detailed), -CAST(t.value_outccy AS DOUBLE)
		  FROM report_spending_transactions(?, ?, ?) t
		  LEFT JOIN accounts a ON a.silver_source_id = t.silver_source_id
		       AND a.account_external_id = t.account_external_id
		 WHERE t.spend_primary = 'BANK_FEES'`, fromEpoch, toEpoch, outCcy)
	if err != nil {
		return nil, fmt.Errorf("fee lines: %w", err)
	}
	defer rows.Close()
	var out []feeLine
	for rows.Next() {
		var (
			l  feeLine
			at int64
			c  sql.NullFloat64
		)
		if err := rows.Scan(&l.src, &l.acct, &l.portfolio, &l.label, &at, &l.group, &c); err != nil {
			return nil, fmt.Errorf("fee lines scan: %w", err)
		}
		l.day = EpochDay(at)
		if c.Valid {
			l.cost = &c.Float64
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// RunFees computes the fee view. p carries the window, the output currency,
// the config's returns settings and the bucket size in Period.
func RunFees(ctx context.Context, db *sql.DB, p ReturnParams) ([]FeeRow, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return nil, err
	}
	ds, err := loadReturnsDataset(ctx, db, p.OutCcy, fx, p.PolicyOverrides, p.TransferMatching)
	if err != nil {
		return nil, err
	}
	lines, err := loadFeeLines(ctx, db, p.FromEpoch, p.ToEpoch, p.OutCcy)
	if err != nil {
		return nil, err
	}
	names, err := loadPortfolioNames(ctx, db)
	if err != nil {
		return nil, err
	}
	return computeFees(ds, lines, names, p), nil
}

// computeFees is the fee rows: per bucket, the entities with a fee line, the
// largest cost first.
func computeFees(ds *returnsDataset, lines []feeLine, portfolioNames map[string]string, p ReturnParams) []FeeRow {
	if len(lines) == 0 {
		return nil
	}
	from, to := feeSpan(ds, lines, p)
	months := feeMonths(ds, p)
	var out []FeeRow
	for _, b := range feeBuckets(from, to, p.Period) {
		entities := map[string]*feeEntity{}
		for _, l := range lines {
			if l.day < b.start || l.day > b.end {
				continue
			}
			key := l.entityKey()
			e := entities[key]
			if e == nil {
				e = l.newEntity(portfolioNames, ds)
				entities[key] = e
			}
			if l.cost == nil {
				e.unpriced++
			} else {
				e.fees[l.group] += *l.cost
			}
		}
		if len(entities) == 0 {
			continue
		}
		rows := make([]FeeRow, 0, len(entities))
		for _, e := range entities {
			rows = append(rows, e.row(b, months[acctKey(e.src, e.kind+"\x00"+e.id)]))
		}
		slices.SortFunc(rows, func(x, y FeeRow) int {
			if c := compareCost(y, x); c != 0 {
				return c
			}
			return strings.Compare(x.SilverSourceID+"\x00"+x.EntityID, y.SilverSourceID+"\x00"+y.EntityID)
		})
		out = append(out, rows...)
	}
	return out
}

// feeSpan is the window's first and last day, inside the data: an open
// start reads as the first day anything was held or paid.
func feeSpan(ds *returnsDataset, lines []feeLine, p ReturnParams) (from, to int64) {
	first, last := lines[0].day, ds.globalMax
	for _, l := range lines {
		first, last = min(first, l.day), max(last, l.day)
	}
	for day := range ds.holdings.netWorth {
		first = min(first, day)
	}
	return max(EpochDay(p.FromEpoch), first), min(EpochDay(p.ToEpoch), last)
}

func compareCost(x, y FeeRow) int {
	a, _ := parseFloatPtr(x.Fees)
	b, _ := parseFloatPtr(y.Fees)
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (l feeLine) entityKey() string {
	if l.portfolio != "" {
		return acctKey(l.src, "portfolio\x00"+l.portfolio)
	}
	return acctKey(l.src, "account\x00"+l.acct)
}

// newEntity is the line's entity, with the accounts whose value it holds:
// every account of the portfolio, cards and mortgages too, or the account.
func (l feeLine) newEntity(portfolioNames map[string]string, ds *returnsDataset) *feeEntity {
	e := &feeEntity{src: l.src, kind: "account", id: l.acct, label: l.label, fees: map[string]float64{}}
	if l.portfolio != "" {
		e.kind, e.id, e.label = "portfolio", l.portfolio, l.portfolio
		if n, ok := portfolioNames[acctKey(l.src, l.portfolio)]; ok {
			e.label = n
		}
	}
	for _, set := range []map[string]*accountData{ds.accts, ds.holdings.cards} {
		for _, a := range set {
			if a.src == l.src && (a.acct == l.acct && l.portfolio == "" || l.portfolio != "" && a.portfolio == l.portfolio) {
				e.spans = append(e.spans, a)
			}
		}
	}
	return e
}

// feeMonths is each entity's monthly rows of the since-inception runs of
// `returns portfolios` and `returns accounts`, keyed as a feeLine's entity:
// the rows report_returns holds, which the fee tiles chain. The account rows
// of accounts inside a portfolio are never read.
func feeMonths(ds *returnsDataset, p ReturnParams) map[string][]ReturnRow {
	out := map[string][]ReturnRow{}
	for _, level := range []struct{ grain, kind string }{{"portfolios", "portfolio"}, {"accounts", "account"}} {
		w := p
		w.Level, w.Method, w.Period, w.Annualize = level.grain, "twr", "monthly", "auto"
		w.Netting, w.Inception = true, "full"
		w.FromEpoch, w.ToEpoch = 0, ds.globalMax*SecondsPerDay
		for _, r := range computeReturns(ds, w) {
			if !r.IsSummary {
				key := acctKey(r.SilverSourceID, level.kind+"\x00"+r.EntityID)
				out[key] = append(out[key], r)
			}
		}
	}
	return out
}

// chainMonths compounds the returns of the months that end in the bucket. A
// month with no return, or one that lost everything, is left out, as the
// fee tiles' logarithms leave it out. It returns nil with nothing to chain,
// and the months' quality tags.
func chainMonths(months []ReturnRow, b feeBucket) (*float64, []string) {
	growth, chained := 1.0, false
	var tags []string
	for _, m := range months {
		if m.EndDay < b.start || m.EndDay > b.end {
			continue
		}
		tags = append(tags, m.Quality...)
		if m.TWR != nil && *m.TWR > -1 {
			growth *= 1 + *m.TWR
			chained = true
		}
	}
	if !chained {
		return nil, dedupeStrings(tags)
	}
	return f64(growth - 1), dedupeStrings(tags)
}

// meanValue is the entity's mean value over the bucket's days on which it
// held one, and how many days those were.
func (e *feeEntity) meanValue(b feeBucket) (float64, int64) {
	var sum float64
	var days int64
	for day := b.start; day <= b.end; day++ {
		alive := false
		for _, a := range e.spans {
			if v, ok := a.valueAt(day); ok {
				sum += v
				alive = true
			}
		}
		if alive {
			days++
		}
	}
	if days == 0 {
		return 0, 0
	}
	return sum / float64(days), days
}

func (e *feeEntity) row(b feeBucket, months []ReturnRow) FeeRow {
	r := FeeRow{Period: b.label, StartDay: b.start, EndDay: b.end,
		SilverSourceID: e.src, EntityKind: e.kind, EntityID: e.id, EntityLabel: e.label}
	var total float64
	for _, g := range feeGroups {
		total += e.fees[g]
	}
	r.Fees = decStr(total)
	r.FeesInvestment, r.FeesBank = decStr(e.fees["investment"]), decStr(e.fees["bank"])
	r.FeesATM, r.FeesOther = decStr(e.fees["atm"]), decStr(e.fees["other"])
	mean, days := e.meanValue(b)
	if days > 0 {
		r.MeanValue = decStr(mean)
	}
	if mean > valueTol {
		r.FeeRate = f64(total / mean * feeDaysPerYear / float64(days))
	} else {
		r.Quality = append(r.Quality, "no_mean_value")
	}
	if days > 0 && days < b.end-b.start+1 {
		r.Quality = append(r.Quality, "part_window_value")
	}
	if e.unpriced > 0 {
		r.Quality = append(r.Quality, fmt.Sprintf("unpriced_fees=%d", e.unpriced))
	}
	twr, tags := chainMonths(months, b)
	r.TWR = twr
	if r.TWR == nil {
		r.Quality = append(r.Quality, "no_return")
	}
	r.Quality = append(r.Quality, tags...)
	return r
}
