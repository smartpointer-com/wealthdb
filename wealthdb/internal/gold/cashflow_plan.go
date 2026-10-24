package gold

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The cash flow projection (docs/CASHFLOW.md §12): the income the income
// projection expects set against the outflows the household's look-back
// shows, month by month. The income projection gives investment income
// and principal, split by the pool's boundary; the look-back gives the
// rest as monthly averages over the cash flow statement's lines
// (cashflow_lines_outccy), so a part of the cash flow projection is the
// statement's own figure for the trailing months spread evenly.

// projectedIncomeDetailed are the income values the income projection
// projects (incomeTypeSQL): their lines are its received half, so the
// cash flow projection's other income leaves them out and counts none
// twice.
var projectedIncomeDetailed = []string{"INCOME_INTEREST_EARNED", "INCOME_DIVIDENDS", "INCOME_DISTRIBUTIONS",
	"INCOME_RENTAL", "INCOME_RETIREMENT_PENSION"}

// PlanOutClasses are the operating-out classes the cash flow projection
// breaks operating_out into (`-C +<class>`), in the vocabulary's order, the
// unresolved class last, as the statement names it.
var PlanOutClasses = []string{string(canonical.ClassConsumption), string(canonical.ClassFees),
	string(canonical.ClassTaxes), string(canonical.ClassGiving), "(uncategorized)"}

// PlanParts are the look-back's parts of report_cashflow_plan (migration
// 0149), in the order web/provision.py's PLAN_LOOKBACK_PARTS reads them;
// operating_out holds a row per class of PlanOutClasses.
var PlanParts = []string{"other_income", "operating_out", "debt_service", "vehicles"}

// PlanThinMonths is the fewest look-back months whose averages read as
// more than thin.
const PlanThinMonths = 6

// PlanLookback is what the look-back window shows, per month, in the
// output currency: the operating income the projection does not project,
// the operating outflows (positive) and each class's, the vehicles'
// flows (signed as the statement signs them), each mortgage's
// instalment, and the financing outflows no mortgage the product holds
// names (positive): a loan's repayments, an instalment to a servicer of
// no account. Months is how many months of the window the averages
// spread over: from the month of its first line to its end.
type PlanLookback struct {
	Months                              int
	OtherIncome, OperatingOut, Vehicles float64
	OtherDebt                           float64
	OutClasses                          map[string]float64
	Mortgages                           []PlanMortgage
}

// PlanMortgage is one mortgage's instalment in the look-back: what was
// paid into it per month, interest and amortization together, over the
// months from its first instalment ever (or the window's start) to the
// window's end.
type PlanMortgage struct {
	Source, Account string
	Instalment      float64
}

// planLookbackSQL sums the window's lines by part, and each mortgage's
// instalments, with the first of the pool's lines ever and each
// mortgage's first instalment ever. An operating inflow is the
// projection's by the verdict the statement classes it by.
const planLookbackSQL = `
WITH l AS MATERIALIZED (SELECT * FROM cashflow_lines_outccy(?1, ?2, ?3)),
parts AS (
    SELECT CASE WHEN section = 'operating_in' AND NOT list_contains(?4, COALESCE(verdict, ''))
                     THEN 'other_income'
                WHEN section = 'operating_out' THEN 'operating_out'
                WHEN section = 'vehicles' THEN 'vehicles'
                WHEN section = 'financing' AND net_amount < 0
                     AND NOT (class = 'mortgage' AND far_account_external_id IS NOT NULL) THEN 'other_debt' END AS part,
           CASE WHEN section = 'operating_out' THEN class END AS class,
           SUM(value_outccy)::DOUBLE AS value
      FROM l GROUP BY ALL),
mortgages AS (
    SELECT far_silver_source_id AS src, far_account_external_id AS acct, -SUM(value_outccy)::DOUBLE AS paid
      FROM l
     WHERE class = 'mortgage' AND net_amount < 0 AND far_account_external_id IS NOT NULL
     GROUP BY ALL),
hist AS MATERIALIZED (
    SELECT n.occurred_at, n.class, n.net_amount, n.far_silver_source_id, n.far_account_external_id
      FROM cashflow_txn_nodes(0, ?2) n
      JOIN cashflow_pool_accounts() p ON p.silver_source_id = n.silver_source_id
           AND p.account_external_id = n.account_external_id
     WHERE n.disposition = 'line'),
firsts AS (
    SELECT h.far_silver_source_id AS src, h.far_account_external_id AS acct, MIN(h.occurred_at) AS first
      FROM hist h
      JOIN mortgages m ON m.src = h.far_silver_source_id AND m.acct = h.far_account_external_id
     WHERE h.class = 'mortgage' AND h.net_amount < 0
     GROUP BY ALL)
SELECT 'part', part, COALESCE(class, ''), value, NULL FROM parts WHERE part IS NOT NULL
UNION ALL
SELECT 'mortgage', m.src, m.acct, m.paid, f.first FROM mortgages m JOIN firsts f USING (src, acct)
UNION ALL
SELECT 'first', '', '', NULL, MIN(occurred_at) FROM hist`

// CashflowPlanLookback reads the look-back window [from, to] (whole
// months, to inclusive) in outCcy: each part's monthly average over the
// window's months from the month of the pool's first line ever, and each
// mortgage's over its months from its first instalment ever. So a
// household or a mortgage newer than the window spreads over the months
// it has. A line no rate converts counts nothing, as in the statement.
func CashflowPlanLookback(ctx context.Context, db querier, from, to int64, outCcy string) (PlanLookback, error) {
	out := PlanLookback{OutClasses: map[string]float64{}}
	months := func(first int64) int {
		f, t := time.Unix(max(first, from), 0).UTC(), time.Unix(to, 0).UTC()
		return (t.Year()-f.Year())*12 + int(t.Month()-f.Month()) + 1
	}
	parts := map[string]float64{}
	type paid struct {
		m     PlanMortgage
		first int64
	}
	var mortgages []paid
	err := scanAll(ctx, db, planLookbackSQL, func(rows *sql.Rows) error {
		var kind, a, b string
		var value sql.NullFloat64
		var first sql.NullInt64
		if err := rows.Scan(&kind, &a, &b, &value, &first); err != nil {
			return err
		}
		switch kind {
		case "part":
			parts[a] += value.Float64
			if a == "operating_out" {
				out.OutClasses[b] -= value.Float64
			}
		case "mortgage":
			mortgages = append(mortgages, paid{PlanMortgage{Source: a, Account: b, Instalment: value.Float64}, first.Int64})
		case "first":
			if first.Valid {
				out.Months = months(first.Int64)
			}
		}
		return nil
	}, from, to, outCcy, projectedIncomeDetailed)
	if err != nil {
		return out, fmt.Errorf("CashflowPlanLookback: %w", err)
	}
	if out.Months == 0 {
		return out, nil
	}
	n := float64(out.Months)
	// An outflow reads positive; 0 - v keeps a zero from printing -0.
	out.OtherIncome, out.OperatingOut, out.Vehicles = parts["other_income"]/n, 0-parts["operating_out"]/n,
		parts["vehicles"]/n
	out.OtherDebt = 0 - parts["other_debt"]/n
	for c := range out.OutClasses {
		out.OutClasses[c] /= n
	}
	for _, p := range mortgages {
		p.m.Instalment /= float64(months(p.first))
		out.Mortgages = append(out.Mortgages, p.m)
	}
	slices.SortFunc(out.Mortgages, func(a, b PlanMortgage) int {
		return strings.Compare(a.Source+"\x00"+a.Account, b.Source+"\x00"+b.Account)
	})
	return out, nil
}

// PlanWindow is the look-back window of a cash flow projection made at
// now: the months whole before the current one, lookback of them.
func PlanWindow(now time.Time, lookback int) (from, to int64) {
	n := now.UTC()
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start.AddDate(0, -lookback, 0).Unix(), start.Unix() - 1
}

// The quality tags of a cash flow projection row: what each part rests on.
const (
	PlanProjected = "projected" // the income projection's
	PlanTrailing  = "trailing"  // a monthly average of the look-back
	PlanTerms     = "terms"     // a mortgage's instalments end at its terms' maturity
	PlanThin      = "thin"      // the look-back holds under PlanThinMonths months
)

// PlanMonth is what the projection expects in one month, in the output
// currency: on the pool's accounts its income and the principal its
// maturities return; on the accounts outside the pool, both together.
type PlanMonth struct {
	Start                             time.Time
	Income, Principal, VehiclesIncome float64
}

// CashflowPool are the accounts of the cash pool
// (cashflow_pool_accounts), by source and account.
func CashflowPool(ctx context.Context, db querier) (map[[2]string]bool, error) {
	pool := map[[2]string]bool{}
	err := scanAll(ctx, db, `SELECT silver_source_id, account_external_id FROM cashflow_pool_accounts()`,
		func(rows *sql.Rows) error {
			var k [2]string
			if err := rows.Scan(&k[0], &k[1]); err != nil {
				return err
			}
			pool[k] = true
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("CashflowPool: %w", err)
	}
	return pool, nil
}

// PlanMonths are the months of the projection's amounts over w
// (yield.Book.ProjectionRows), each split by the pool: a holding of a
// pool account counts in Income and Principal, any other in
// VehiclesIncome. So the three sum to the projection's expected and
// principal.
func PlanMonths(b *yield.Book, amounts []yield.Amount, w yield.Window, pool map[[2]string]bool) []PlanMonth {
	var in, out []yield.Amount
	for _, a := range amounts {
		if pool[[2]string{a.Holding.Source, a.Holding.Account}] {
			in = append(in, a)
		} else {
			out = append(out, a)
		}
	}
	var months []PlanMonth
	vehicles := b.ProjectionRows(out, w, 1)
	for i, r := range b.ProjectionRows(in, w, 1) {
		if !r.Total && !r.Average {
			months = append(months, PlanMonth{Start: r.Period, Income: r.Expected, Principal: r.Principal,
				VehiclesIncome: vehicles[i].Expected + vehicles[i].Principal})
		}
	}
	return months
}

// PlanRow is one row of the cash flow projection: a month, or a period
// of them, and what each part brings in or takes out of it. Net is the
// pool's: it leaves out VehiclesIncome, which stays inside the vehicles,
// and counts principal only where it lands in cash: under the roll rule
// a maturity's principal buys its successor.
type PlanRow struct {
	Period                                              time.Time
	Total, Average                                      bool
	InvestmentIncome, PrincipalReturned, VehiclesIncome float64
	OtherIncome, OperatingOut, DebtService              float64
	Vehicles, Net                                       float64
	OutClasses                                          map[string]float64
	Quality                                             string
}

// CashflowPlan sets the projection's months against the look-back's
// averages. The debt service holds, every month, the financing outflows
// no held mortgage names, and each mortgage's instalment while the book
// holds it (standing, by source and account, with its maturity: the
// latest of its tranches' terms, nil where one states none), each month
// whose first day is on or before its maturity; one the book no longer
// holds was repaid. A row is a calendar period of `period` months (1, 3,
// 12), as the projection's rows are, a window's first and last perhaps
// shorter; a total and a monthly average row come last.
func CashflowPlan(lb PlanLookback, standing map[[2]string]*time.Time, months []PlanMonth, period int,
	roll bool) []PlanRow {
	if len(months) == 0 {
		return nil
	}
	var rows []PlanRow
	tags := map[string]bool{}
	for _, m := range months {
		start := time.Date(m.Start.Year(), time.Month((int(m.Start.Month())-1)/period*period+1), 1, 0, 0, 0, 0,
			time.UTC)
		if len(rows) == 0 || !rows[len(rows)-1].Period.Equal(start) {
			rows = append(rows, PlanRow{Period: start, OutClasses: map[string]float64{}})
			tags = map[string]bool{PlanProjected: true}
		}
		r := &rows[len(rows)-1]
		r.InvestmentIncome += m.Income
		r.PrincipalReturned += m.Principal
		r.VehiclesIncome += m.VehiclesIncome
		if lb.Months > 0 {
			r.OtherIncome += lb.OtherIncome
			r.OperatingOut += lb.OperatingOut
			r.Vehicles += lb.Vehicles
			r.DebtService += lb.OtherDebt
			for c, v := range lb.OutClasses {
				r.OutClasses[c] += v
			}
			tags[PlanTrailing] = true
		}
		if lb.Months < PlanThinMonths {
			tags[PlanThin] = true
		}
		for _, mg := range lb.Mortgages {
			maturity, held := standing[[2]string{mg.Source, mg.Account}]
			if !held {
				continue
			}
			if maturity != nil {
				tags[PlanTerms] = true
				if m.Start.After(*maturity) {
					continue
				}
			}
			r.DebtService += mg.Instalment
		}
		r.Quality = planQuality(tags)
	}
	total := PlanRow{Total: true, OutClasses: map[string]float64{}}
	all := map[string]bool{}
	for i := range rows {
		r := &rows[i]
		r.Net = r.InvestmentIncome + r.OtherIncome - r.OperatingOut - r.DebtService + r.Vehicles
		if !roll {
			r.Net += r.PrincipalReturned
		}
		total.add(*r)
		for _, t := range strings.Fields(r.Quality) {
			all[t] = true
		}
	}
	total.Quality = planQuality(all)
	avg := total.scaled(1 / float64(len(months)))
	avg.Total, avg.Average = false, true
	return append(rows, total, avg)
}

// add sums r's figures into p.
func (p *PlanRow) add(r PlanRow) {
	p.InvestmentIncome += r.InvestmentIncome
	p.PrincipalReturned += r.PrincipalReturned
	p.VehiclesIncome += r.VehiclesIncome
	p.OtherIncome += r.OtherIncome
	p.OperatingOut += r.OperatingOut
	p.DebtService += r.DebtService
	p.Vehicles += r.Vehicles
	p.Net += r.Net
	for c, v := range r.OutClasses {
		p.OutClasses[c] += v
	}
}

// scaled is p with every figure times k.
func (p PlanRow) scaled(k float64) PlanRow {
	out := PlanRow{Quality: p.Quality, OutClasses: map[string]float64{}}
	out.InvestmentIncome, out.PrincipalReturned, out.VehiclesIncome = p.InvestmentIncome*k, p.PrincipalReturned*k,
		p.VehiclesIncome*k
	out.OtherIncome, out.OperatingOut, out.DebtService = p.OtherIncome*k, p.OperatingOut*k, p.DebtService*k
	out.Vehicles, out.Net = p.Vehicles*k, p.Net*k
	for c, v := range p.OutClasses {
		out.OutClasses[c] = v * k
	}
	return out
}

// planQuality lists the tags set, in the order the docs give them.
func planQuality(tags map[string]bool) string {
	var out []string
	for _, t := range []string{PlanProjected, PlanTrailing, PlanTerms, PlanThin} {
		if tags[t] {
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
}

// PlanStanding are the mortgages a book holds, by source and account,
// each with its maturity: the latest of its tranches' terms, nil where
// one states none.
func PlanStanding(b *yield.Book) map[[2]string]*time.Time {
	out := map[[2]string]*time.Time{}
	known := map[[2]string]bool{}
	for _, h := range b.In.Holdings {
		if h.Vehicle != "mortgage" {
			continue
		}
		k := [2]string{h.Source, h.Account}
		maturity := b.In.Terms[yield.InstrumentKey{Source: h.Source, Instrument: h.Instrument}].Maturity
		prev, seen := out[k]
		switch {
		case !seen:
			out[k], known[k] = maturity, maturity != nil
		case !known[k] || maturity == nil:
			out[k], known[k] = nil, false
		case maturity.After(*prev):
			out[k] = maturity
		}
	}
	return out
}
