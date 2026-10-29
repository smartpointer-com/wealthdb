package gold

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The stated-period reconciliation: what a document states about an
// account's period (stated_periods, migration 0159) beside what the
// returns engine computes for the same account over the same months.
// The stated figures are informative. They never create a flow, so the
// engine's figures here are exactly the ones `returns accounts` prints;
// the gaps say where the ledger and the document disagree.

// StatedPeriodRow is one stated period and the engine's figures for it.
// Every amount is in the period's stated currency. The engine's figures
// are nil where the engine has no monthly row for some month of the
// period.
type StatedPeriodRow struct {
	SilverSourceID    string
	AccountExternalID string
	AccountLabel      string
	Period            string // YYYY-MM of the last day, or FROM..TO for a longer period
	StartDay, EndDay  int64  // the stated period's first and last day, epoch days
	Currency          string
	SourceSHA256      string

	StatedBeginning    *string
	StatedEnding       *string
	StatedAccrual      *string
	StatedNetAdditions *string
	StatedNetGain      *string
	StatedIncome       *string
	StatedFees         *string

	StartValue *string // the engine's value at the month before the period
	EndValue   *string // the engine's value at the period's end
	NetFlow    *string // the engine's net external flow over the period

	FlowGap  *string // StatedNetAdditions − NetFlow
	ValueGap *string // StatedEnding − EndValue

	// Quality is the engine rows' quality tags, de-duplicated; where the
	// engine has no row for a month, no_engine_row and the tags of the
	// account's since-inception row, which say why.
	Quality []string
}

// ReconcileStatedPeriods sets every stated period whose last day falls in
// [p.FromEpoch, p.ToEpoch] against the engine's monthly account rows in
// its currency, the engine run from inception. p supplies the config's
// returns settings; its level, period, method, currency and start are
// set here.
func ReconcileStatedPeriods(ctx context.Context, db *sql.DB, p ReturnParams) ([]StatedPeriodRow, error) {
	to := p.ToEpoch
	if to <= 0 {
		to = MaxEpoch
	}
	rows, err := db.QueryContext(ctx, `
SELECT s.silver_source_id, s.account_external_id, COALESCE(a.display_name, s.account_external_id),
       s.period_start, s.as_of, s.currency, s.source_sha256,
       CAST(s.beginning_value AS VARCHAR), CAST(s.ending_value AS VARCHAR),
       CAST(s.accrual AS VARCHAR), CAST(s.net_additions AS VARCHAR),
       CAST(s.net_gain AS VARCHAR), CAST(s.income AS VARCHAR), CAST(s.fees AS VARCHAR)
  FROM stated_periods s
  LEFT JOIN accounts a USING (silver_source_id, account_external_id)
 WHERE s.as_of BETWEEN ? AND ?
 ORDER BY s.silver_source_id, s.account_external_id, s.as_of, s.source_sha256`, max(p.FromEpoch, 0), to)
	if err != nil {
		return nil, fmt.Errorf("stated periods: %w", err)
	}
	defer rows.Close()
	var out []StatedPeriodRow
	currencies := map[string]bool{}
	for rows.Next() {
		var (
			r          StatedPeriodRow
			start, end int64
		)
		if err := rows.Scan(&r.SilverSourceID, &r.AccountExternalID, &r.AccountLabel,
			&start, &end, &r.Currency, &r.SourceSHA256,
			&r.StatedBeginning, &r.StatedEnding, &r.StatedAccrual, &r.StatedNetAdditions,
			&r.StatedNetGain, &r.StatedIncome, &r.StatedFees); err != nil {
			return nil, fmt.Errorf("stated periods scan: %w", err)
		}
		r.StartDay, r.EndDay = EpochDay(start), EpochDay(end)
		r.Period = statedPeriodLabel(start, end)
		currencies[r.Currency] = true
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The engine's monthly account rows, per currency the periods state.
	type monthKey struct{ source, account, ccy, month string }
	months := map[monthKey]ReturnRow{}
	// The account's since-inception row, whose quality says why the
	// engine has no month for it, where it has none.
	summaries := map[monthKey]ReturnRow{}
	for ccy := range currencies {
		// From inception, whatever the window: a calendar month's bucket
		// starts at its first day only when the run does not, and a period
		// that begins before the window still needs its first months.
		q := p
		q.Level, q.Period, q.Method, q.OutCcy = "accounts", "monthly", "twr", ccy
		q.FromEpoch = 0
		got, err := RunReturns(ctx, db, q)
		if err != nil {
			return nil, err
		}
		for _, r := range got {
			if r.IsSummary {
				summaries[monthKey{r.SilverSourceID, r.EntityID, ccy, ""}] = r
			} else {
				months[monthKey{r.SilverSourceID, r.EntityID, ccy, r.Period}] = r
			}
		}
	}
	for i := range out {
		r := &out[i]
		var rs []ReturnRow
		for _, m := range monthsOf(r.StartDay, r.EndDay) {
			e, ok := months[monthKey{r.SilverSourceID, r.AccountExternalID, r.Currency, m}]
			if !ok {
				rs = nil
				r.Quality = append([]string{"no_engine_row"},
					summaries[monthKey{r.SilverSourceID, r.AccountExternalID, r.Currency, ""}].Quality...)
				break
			}
			rs = append(rs, e)
		}
		if len(rs) == 0 {
			continue
		}
		r.StartValue, r.EndValue = rs[0].StartValue, rs[len(rs)-1].EndValue
		flows := make([]*string, len(rs))
		seen := map[string]bool{}
		for k, e := range rs {
			flows[k] = e.NetFlow
			for _, q := range e.Quality {
				if !seen[q] {
					seen[q] = true
					r.Quality = append(r.Quality, q)
				}
			}
		}
		r.NetFlow = sumDecimals(flows...)
		r.FlowGap = subDecimals(r.StatedNetAdditions, r.NetFlow)
		r.ValueGap = subDecimals(r.StatedEnding, r.EndValue)
	}
	return out, nil
}

// statedPeriodLabel names a stated period: its month when it lies in one
// calendar month (a period may start on the month's first business day),
// else its first and last day.
func statedPeriodLabel(start, end int64) string {
	s, e := time.Unix(start, 0).UTC(), time.Unix(end, 0).UTC()
	if s.Year() == e.Year() && s.Month() == e.Month() {
		return e.Format(monthLabelLayout)
	}
	return s.Format("2006-01-02") + ".." + e.Format("2006-01-02")
}

// monthsOf lists the YYYY-MM labels of every calendar month from the
// month of firstDay through the month of lastDay, epoch days.
func monthsOf(firstDay, lastDay int64) []string {
	var out []string
	t := time.Unix(firstDay*SecondsPerDay, 0).UTC()
	t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Unix(lastDay*SecondsPerDay, 0).UTC()
	for !t.After(end) {
		out = append(out, t.Format(monthLabelLayout))
		t = t.AddDate(0, 1, 0)
	}
	return out
}

// sumDecimals adds decimal strings exactly; nil when any is nil or does
// not parse.
func sumDecimals(xs ...*string) *string {
	var sum canonical.Decimal
	for _, x := range xs {
		if x == nil {
			return nil
		}
		d, err := canonical.NewDecimalFromString(*x)
		if err != nil {
			return nil
		}
		sum = sum.Add(d)
	}
	s := sum.String()
	return &s
}

// subDecimals is a − b on decimal strings; nil when either is nil.
func subDecimals(a, b *string) *string {
	if a == nil || b == nil {
		return nil
	}
	x, err1 := canonical.NewDecimalFromString(*a)
	y, err2 := canonical.NewDecimalFromString(*b)
	if err1 != nil || err2 != nil {
		return nil
	}
	s := x.Sub(y).String()
	return &s
}
