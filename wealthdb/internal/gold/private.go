package gold

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// PrivateRow is one private holding of `wealthdb returns private`, or the
// total, which comes last: report_private's row with its money-weighted
// return and its capital account's figures. Money columns are decimal
// strings in the output currency, nil where the row has none.
type PrivateRow struct {
	AsOf              int64 // epoch seconds: the instant the row reads
	IsTotal           bool
	SilverSourceID    string
	AccountExternalID string
	DisplayName       *string
	Nickname          *string
	InstrumentID      string
	Holding           string
	AssetClass        *string // nil on the total
	Vehicle           *string
	Status            *string // held, partially_exited or exited; nil on the total

	FirstCall   int64   // epoch seconds, the day of the first call, else of the first leg or line
	PaidIn      *string // the calls, net of refunds; nil where the holding states no cost (no_cost)
	Commitment  *string // the stated commitment; nil where no capital account states one
	Unfunded    *string // the stated remainder, else commitment less calls (or paid in); 0 once exited; nil if unstated
	Distributed *string // cash and exits in kind back
	NAV         *string // nil where the holding never had a value
	MarkedAt    *int64  // epoch seconds; its last line's once exited, nil where it never had one (no_mark)
	EndAt       int64   // epoch seconds: the later of the NAV's snapshot and the last leg

	DPI, TVPI *float64
	IRR       *float64 // annual, since the first call

	// The multiples of the holding's capital account, as its source
	// states them or as its stated figures give them, and the day it
	// stands at (epoch seconds); nil where none is stated.
	StatedTVPI, StatedDPI *float64
	StatedAsOf            *int64

	Quality []string
	quality string
	group   string // the holding's portfolio, else its account (private_group)
}

// PrivateLatest is the as-of that reads the latest snapshot and every leg.
const PrivateLatest = MaxEpoch

// RunPrivate is the private-markets view at asOf in outCcy:
// report_private's rows, each with the IRR of its legs (private_legs)
// and its NAV, the total with the pooled IRR of the listed holdings'
// legs and of the unattributed exits out of their groups (private_group).
func RunPrivate(ctx context.Context, db *sql.DB, asOf int64, outCcy string) ([]PrivateRow, error) {
	return runPrivateAt(ctx, db, []int64{asOf}, outCcy, map[string]privateIRR{})
}

// runPrivateAt is RunPrivate at every instant of instants, in the order
// of the instants, then of each instant's rows. memo holds the returns
// already solved, by their inputs: a holding whose legs, NAV and days do
// not move from one instant to the next (an exited one) is solved once.
func runPrivateAt(ctx context.Context, db *sql.DB, instants []int64, outCcy string,
	memo map[string]privateIRR) ([]PrivateRow, error) {
	list := privateInstants(instants)
	rows, err := scanRows(ctx, db, "RunPrivate",
		`SELECT as_of, is_total, silver_source_id, account_external_id, display_name, nickname,
		        instrument_external_id, holding, asset_class, vehicle, status, holding_group, first_call,
		        CAST(paid_in AS VARCHAR), CAST(commitment AS VARCHAR), CAST(unfunded AS VARCHAR),
		        CAST(distributed AS VARCHAR), CAST(nav AS VARCHAR),
		        marked_at, end_at, dpi, tvpi, stated_tvpi, stated_dpi, stated_as_of, quality
		   FROM report_private(`+list+`, ?)`,
		[]any{outCcy}, func(r *PrivateRow) []any {
			return []any{&r.AsOf, &r.IsTotal, &r.SilverSourceID, &r.AccountExternalID, str(&r.DisplayName), str(&r.Nickname),
				&r.InstrumentID, &r.Holding, str(&r.AssetClass), str(&r.Vehicle), str(&r.Status), &r.group, &r.FirstCall,
				dec(&r.PaidIn), dec(&r.Commitment), dec(&r.Unfunded), dec(&r.Distributed), dec(&r.NAV),
				i64(&r.MarkedAt), &r.EndAt, flt(&r.DPI), flt(&r.TVPI),
				flt(&r.StatedTVPI), flt(&r.StatedDPI), i64(&r.StatedAsOf), &r.quality}
		})
	if err != nil {
		return nil, err
	}
	legs, err := loadPrivateLegs(ctx, db, list, outCcy)
	if err != nil {
		return nil, err
	}
	type key struct {
		asOf int64
		hold [3]string
	}
	byKey := map[key][]privateLeg{}
	unattributed := map[int64][]privateLeg{}
	for _, l := range legs {
		if l.unattributed {
			unattributed[l.asOf] = append(unattributed[l.asOf], l)
			continue
		}
		k := key{l.asOf, l.key}
		byKey[k] = append(byKey[k], l)
	}
	// The total pools the legs and the NAVs of the holdings listed at its
	// instant that have an IRR's inputs: a paid in, a NAV and a rate for
	// every leg. A holding the view leaves out moved no money, and its
	// legs move no rate. It also pools the priced unattributed exits out
	// of the listed holdings' groups, as report_private counts them.
	var (
		pooled []privateLeg
		navs   returns.Sum
		listed map[[2]string]bool
	)
	for i := range rows {
		r := &rows[i]
		if i == 0 || r.AsOf != rows[i-1].AsOf {
			pooled, navs, listed = nil, returns.Sum{}, map[[2]string]bool{}
		}
		if r.quality != "" {
			r.Quality = strings.Split(r.quality, ";")
		}
		if r.IsTotal {
			own := pooled
			for _, l := range unattributed[r.AsOf] {
				if listed[[2]string{l.key[0], l.group}] && l.paid != nil {
					own = append(own, l)
				}
			}
			if len(own) > 0 {
				withPrivateIRR(r, own, navs.Total(), memo)
			}
			continue
		}
		own := byKey[key{r.AsOf, [3]string{r.SilverSourceID, r.AccountExternalID, r.InstrumentID}}]
		listed[[2]string{r.SilverSourceID, r.group}] = true
		nav, ok := parseFloatPtr(r.NAV)
		if !ok || r.PaidIn == nil || slices.ContainsFunc(own, func(l privateLeg) bool { return l.paid == nil }) {
			continue
		}
		withPrivateIRR(r, own, nav, memo)
		pooled = append(pooled, own...)
		navs.Add(nav)
	}
	return rows, nil
}

// privateInstants is instants as the SQL list the private macros read.
func privateInstants(instants []int64) string {
	parts := make([]string, len(instants))
	for i, t := range instants {
		parts[i] = strconv.FormatInt(t, 10)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// privateLeg is one leg of private_legs at one instant: the holder's
// capital in, positive on a call and negative on what came back, nil with
// no rate. An unattributed leg is an exit in kind no holding names.
type privateLeg struct {
	asOf         int64
	key          [3]string // source, the holding's account and instrument
	group        string    // the holding's portfolio, else its account
	day          int64
	paid         *float64
	unattributed bool
}

// loadPrivateLegs reads every private leg at the instants of list, in
// outCcy, by instant and holding, in the macro's order.
func loadPrivateLegs(ctx context.Context, db *sql.DB, list, outCcy string) ([]privateLeg, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT as_of, silver_source_id, account_external_id, COALESCE(instrument_external_id, ''), holding_group, day,
		       CAST(paid AS DOUBLE), unattributed
		  FROM private_legs(`+list+`, ?)`, outCcy)
	if err != nil {
		return nil, fmt.Errorf("private legs: %w", err)
	}
	defer rows.Close()
	var out []privateLeg
	for rows.Next() {
		var (
			l privateLeg
			p sql.NullFloat64
		)
		if err := rows.Scan(&l.asOf, &l.key[0], &l.key[1], &l.key[2], &l.group, &l.day, &p, &l.unattributed); err != nil {
			return nil, fmt.Errorf("private legs scan: %w", err)
		}
		if p.Valid {
			l.paid = &p.Float64
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// privateIRR is one solved return: the rate, and the tags the solver
// added to the row's quality.
type privateIRR struct {
	rate *float64
	tags []string
}

// withPrivateIRR sets r's IRR: the engine's money-weighted rate (solveIRR)
// of legs, every one priced, none held before the first call, and nav
// received on the day the return ends. A holding with no NAV (no_mark),
// no cost (no_cost), or a leg of no rate (unpriced), has none: the caller
// does not solve it. solveIRR's mwr_* flag says why where it finds none.
// A rate stands wherever the solver finds one (rootStands), so a holding
// that has paid back more than it took and still holds a value has one;
// mwr_nonunique says where its flows change sign more than once. memo
// holds the rates solved, by their inputs.
func withPrivateIRR(r *PrivateRow, legs []privateLeg, nav float64, memo map[string]privateIRR) {
	flows := make([]returns.Flow, 0, len(legs))
	for _, l := range legs {
		flows = append(flows, returns.Flow{Day: l.day, Amount: *l.paid})
	}
	start, end := EpochDay(r.FirstCall)-1, EpochDay(r.EndAt)
	window := flowsIn(flows, start, end)
	k := privateIRRKey(nav, start, end, window)
	got, ok := memo[k]
	if !ok {
		rate, tags := solveIRR(returns.XIRR, 0, nav, start, end, window, false, true, nil)
		got = privateIRR{rate, tags}
		memo[k] = got
	}
	r.IRR = got.rate
	r.Quality = append(r.Quality, got.tags...)
}

// privateIRRKey is the inputs of one return as a map key: the NAV, the
// window's days and every flow, the floats by their bits.
func privateIRRKey(nav float64, start, end int64, flows []returns.Flow) string {
	b := make([]byte, 0, 24+16*len(flows))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(nav))
	b = binary.LittleEndian.AppendUint64(b, uint64(start))
	b = binary.LittleEndian.AppendUint64(b, uint64(end))
	for _, f := range flows {
		b = binary.LittleEndian.AppendUint64(b, uint64(f.Day))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(f.Amount))
	}
	return string(b)
}

// ---- materialization -----------------------------------------------------

const privateCols = `computed_at, as_of, currency, is_total, silver_source_id, account_external_id,
    instrument_external_id, holding, asset_class, vehicle, status, first_call, paid_in, distributed, nav,
    marked_at, end_at, dpi, tvpi, irr, quality, commitment, unfunded, stated_tvpi, stated_dpi, stated_as_of`

// privateMaterial is each reporting currency's rows of `returns private`
// at every instant report_private_markets holds.
type privateMaterial map[string][]PrivateRow

// computeMaterialPrivate is RunPrivate in every reporting currency at
// every instant privateMaterialInstants names for today, the last second
// of the day: the rows report_private_markets holds. They start at the
// total's first_call today: the first call, else leg, else line of a
// listed holding.
func computeMaterialPrivate(ctx context.Context, db *sql.DB, today int64) (privateMaterial, error) {
	var first sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT min(first_call) FROM report_private(`+privateInstants([]int64{today})+`, 'USD')
		 WHERE is_total`).Scan(&first); err != nil {
		return nil, fmt.Errorf("private first call: %w", err)
	}
	var instants []int64
	if first.Valid {
		instants = privateMaterialInstants(first.Int64, today)
	} else {
		instants = []int64{today}
	}
	out := privateMaterial{}
	for _, ccy := range materializeCurrencies {
		rows, err := runPrivateAt(ctx, db, instants, ccy, map[string]privateIRR{})
		if err != nil {
			return nil, err
		}
		out[ccy] = rows
	}
	return out, nil
}

// privateMaterialInstants are the instants report_private_markets holds:
// the last second of every calendar quarter, UTC, from the quarter of
// first up to the one before today's end, then today.
func privateMaterialInstants(first, today int64) []int64 {
	t := time.Unix(first, 0).UTC()
	q := time.Date(t.Year(), t.Month()-(t.Month()-1)%3, 1, 0, 0, 0, 0, time.UTC)
	var out []int64
	for {
		q = q.AddDate(0, 3, 0)
		end := q.Unix() - 1
		if end >= today {
			break
		}
		out = append(out, end)
	}
	return append(out, today)
}

// write inserts report_private_markets' rows, currency by currency, each
// as the CLI prints it.
func (m privateMaterial) write(ctx context.Context, tx *sql.Tx, computedAt int64) error {
	type ref struct {
		ccy string
		row int
	}
	var refs []ref
	for _, ccy := range materializeCurrencies {
		for j := range m[ccy] {
			refs = append(refs, ref{ccy, j})
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return insertColumns(ctx, tx, "report_private_markets", "report_private_markets", privateCols, len(refs),
		func(i int, args []any) []any {
			ccy := refs[i].ccy
			r := &m[ccy][refs[i].row]
			return append(args, computedAt, r.AsOf, ccy, r.IsTotal, r.SilverSourceID, r.AccountExternalID,
				r.InstrumentID, r.Holding, r.AssetClass, r.Vehicle, r.Status, r.FirstCall, r.PaidIn, r.Distributed,
				r.NAV, r.MarkedAt, r.EndAt, r.DPI, r.TVPI, r.IRR, strings.Join(r.Quality, ";"), r.Commitment,
				r.Unfunded, r.StatedTVPI, r.StatedDPI, r.StatedAsOf)
		})
}
