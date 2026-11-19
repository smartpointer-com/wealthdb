package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// PrivateRow is one private holding of `wealthdb returns private`, or the
// total, which comes last: report_private's row with its money-weighted
// return. Money columns are decimal strings in the output currency, nil
// where the row has none.
type PrivateRow struct {
	IsTotal           bool
	SilverSourceID    string
	AccountExternalID string
	DisplayName       *string
	Nickname          *string
	InstrumentID      string
	Holding           string

	FirstCall   int64   // epoch seconds, the day of the first call
	PaidIn      *string // the calls, net of refunds
	Distributed *string // cash and exits in kind back
	NAV         *string // nil where the holding never had a value
	MarkedAt    *int64  // epoch seconds; nil with no line at the as-of
	EndAt       int64   // epoch seconds: the later of the NAV's snapshot and the last leg

	DPI, TVPI *float64
	IRR       *float64 // annual, since the first call

	Quality []string
	quality string
}

// privateAsOf is the as-of the view reads: the latest snapshot.
const privateAsOf = MaxEpoch

// RunPrivate is the private-markets view in outCcy: report_private's rows,
// each with the IRR of its legs (private_legs) and its NAV, the total with
// the pooled IRR of every leg.
func RunPrivate(ctx context.Context, db *sql.DB, outCcy string) ([]PrivateRow, error) {
	rows, err := scanRows(ctx, db, "RunPrivate",
		`SELECT is_total, silver_source_id, account_external_id, display_name, nickname,
		        instrument_external_id, holding, first_call,
		        CAST(paid_in AS VARCHAR), CAST(distributed AS VARCHAR), CAST(nav AS VARCHAR),
		        marked_at, end_at, dpi, tvpi, quality
		   FROM report_private(?, ?)`,
		[]any{privateAsOf, outCcy}, func(r *PrivateRow) []any {
			return []any{&r.IsTotal, &r.SilverSourceID, &r.AccountExternalID, str(&r.DisplayName), str(&r.Nickname),
				&r.InstrumentID, &r.Holding, &r.FirstCall,
				dec(&r.PaidIn), dec(&r.Distributed), dec(&r.NAV),
				i64(&r.MarkedAt), &r.EndAt, flt(&r.DPI), flt(&r.TVPI), &r.quality}
		})
	if err != nil {
		return nil, err
	}
	legs, err := loadPrivateLegs(ctx, db, outCcy)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].quality != "" {
			rows[i].Quality = strings.Split(rows[i].quality, ";")
		}
		withPrivateIRR(&rows[i], legs)
	}
	return rows, nil
}

// privateLeg is one leg of private_legs: the holder's capital in, positive
// on a call and negative on what came back, nil with no rate.
type privateLeg struct {
	key  [3]string // source, account, instrument
	day  int64
	paid *float64
}

// loadPrivateLegs reads every private leg in outCcy, by holding, in the
// macro's order.
func loadPrivateLegs(ctx context.Context, db *sql.DB, outCcy string) ([]privateLeg, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT silver_source_id, account_external_id, instrument_external_id, day, CAST(paid AS DOUBLE)
		  FROM private_legs(?, ?)`, privateAsOf, outCcy)
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
		if err := rows.Scan(&l.key[0], &l.key[1], &l.key[2], &l.day, &p); err != nil {
			return nil, fmt.Errorf("private legs scan: %w", err)
		}
		if p.Valid {
			l.paid = &p.Float64
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// withPrivateIRR sets r's IRR: the engine's money-weighted rate (solveIRR)
// of the legs, none held before the first call, and the NAV received on
// the day the return ends. The total pools every holding's legs. A row
// with no NAV (no_mark), or with a leg of no rate (unpriced), has none;
// solveIRR's mwr_* flag says why where it finds none.
func withPrivateIRR(r *PrivateRow, legs []privateLeg) {
	nav, ok := parseFloatPtr(r.NAV)
	if !ok {
		return
	}
	var flows []returns.Flow
	for _, l := range legs {
		if !r.IsTotal && l.key != [3]string{r.SilverSourceID, r.AccountExternalID, r.InstrumentID} {
			continue
		}
		if l.paid == nil {
			return
		}
		flows = append(flows, returns.Flow{Day: l.day, Amount: *l.paid})
	}
	start, end := EpochDay(r.FirstCall)-1, EpochDay(r.EndAt)
	r.IRR, r.Quality = solveIRR(returns.XIRR, 0, nav, start, end, flowsIn(flows, start, end), false, r.Quality)
}

// ---- materialization -----------------------------------------------------

const privateCols = `computed_at, currency, is_total, silver_source_id, account_external_id,
    instrument_external_id, holding, first_call, paid_in, distributed, nav, marked_at, end_at,
    dpi, tvpi, irr, quality`

// privateMaterial is each reporting currency's rows of `returns private`.
type privateMaterial map[string][]PrivateRow

// computeMaterialPrivate is RunPrivate in every reporting currency, the
// rows report_private_markets holds.
func computeMaterialPrivate(ctx context.Context, db *sql.DB) (privateMaterial, error) {
	out := privateMaterial{}
	for _, ccy := range materializeCurrencies {
		rows, err := RunPrivate(ctx, db, ccy)
		if err != nil {
			return nil, err
		}
		out[ccy] = rows
	}
	return out, nil
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
			return append(args, computedAt, ccy, r.IsTotal, r.SilverSourceID, r.AccountExternalID, r.InstrumentID,
				r.Holding, r.FirstCall, r.PaidIn, r.Distributed, r.NAV, r.MarkedAt, r.EndAt,
				r.DPI, r.TVPI, r.IRR, strings.Join(r.Quality, ";"))
		})
}
