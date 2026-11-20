package gold

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

func computeMWR(v0, v1 float64, winFrom, winTo int64, flows []returns.Flow, assets []*accountData, p ReturnParams, q []string) (*float64, *float64, []string) {
	rate, q := solveMWR(returns.XIRR, v0, v1, winFrom, winTo, flowsIn(flows, winFrom, winTo), assets, q)
	if rate == nil {
		return nil, nil, q
	}
	days := float64(winTo - winFrom)
	var ann *float64
	if returns.ShouldAnnualize(p.Annualize, days) {
		ann = rate // XIRR is already an annual rate
	}
	// The mwr_pct column shows the PERIOD (cumulative-equivalent) figure so it is
	// consistent with the twr_pct column; mwr_ann_pct holds the annualized XIRR.
	// For a full-year window the two coincide.
	return f64(returns.DeAnnualize(*rate, days)), ann, q
}

// solveMWR is the annual money-weighted rate of a window's flows through
// solve, or nil with the mwr_* flag that says why there is none.
func solveMWR(solve func(v0, v1 float64, from, to int64, flows []returns.Flow) (float64, error),
	v0, v1 float64, winFrom, winTo int64, windowFlows []returns.Flow, assets []*accountData, q []string) (*float64, []string) {
	if allNavOnly(assets) {
		return nil, append(q, "mwr_no_flows")
	}
	return solveIRR(solve, v0, v1, winFrom, winTo, windowFlows, anyNavOnly(assets), false, q)
}

// solveIRR is solveMWR on flows that are every flow there was, or, with
// incomplete, a part of them, which a rate found says (mwr_incomplete_flows).
// With rootStands, a rate stands wherever the solver finds one, whatever
// the net capital: a private holding that has paid back more than it took
// and still holds a value has the IRR of a successful fund, and a pool of
// such holdings has one too. mwr_nonunique says where the flows change
// sign more than once.
func solveIRR(solve func(v0, v1 float64, from, to int64, flows []returns.Flow) (float64, error),
	v0, v1 float64, winFrom, winTo int64, windowFlows []returns.Flow, incomplete, rootStands bool, q []string) (*float64, []string) {
	if len(windowFlows) == 0 {
		return nil, append(q, "mwr_no_flows")
	}
	signChanges := returns.MWRSignChanges(v0, v1, winFrom, winTo, windowFlows)
	// If the entity's net invested capital over the window — the opening base plus
	// the total net external flow (deposits minus withdrawals) — is zero or below,
	// the investor has on net been repaid at least everything they committed, so
	// the money-weighted return is a return on non-positive capital: XIRR yields an
	// extreme, non-unique root. Surface n/a. This is a WHOLE-WINDOW measure on
	// purpose: an interim contribution dip (one constituent's large distribution
	// before another's later deposits, or a journal-out whose funding trade isn't a
	// counted flow) does not mean the capital was ever truly negative, so a window
	// that ends net-positive still gets a valid MWR.
	//
	// One shape is exempt: a FULLY-REALIZED window (end value ~0) whose cash-flow
	// sequence has exactly one sign change — the textbook profitable exit
	// (committed capital repaid at more than cost, e.g. a ledger-exact closure
	// sold above its invested basis). Its IRR is unique and well-defined; blanking
	// it would deny an MWR to precisely the gains the flow-counting policies
	// exist to surface, while losses (net capital still positive) compute one.
	// With rootStands the guard does not apply.
	if !rootStands && mwrNetCapitalNonPositive(v0, windowFlows) && (signChanges != 1 || v1 > valueTol) {
		return nil, append(q, "mwr_negative_net_capital")
	}
	rate, err := solve(v0, v1, winFrom, winTo, windowFlows)
	if err != nil {
		switch err {
		case returns.ErrNoSignChange:
			q = append(q, "mwr_no_sign_change")
		case returns.ErrNoFlows:
			q = append(q, "mwr_no_flows")
		default:
			q = append(q, "mwr_no_converge")
		}
		return nil, q
	}
	if incomplete {
		q = append(q, "mwr_incomplete_flows")
	}
	if signChanges > 1 {
		q = append(q, "mwr_nonunique")
	}
	return &rate, q
}

// mwrNetCapitalNonPositive reports whether the entity's net invested capital over
// the window — the opening base v0 plus the total net external flow (deposits
// positive, withdrawals negative, per the net_flow convention) — is zero or
// below. It is a whole-window sum, not a running minimum: an interim dip in
// cumulative contributions is not a genuinely-negative capital position (the
// account value never went there), so it must not disqualify the MWR.
func mwrNetCapitalNonPositive(v0 float64, flows []returns.Flow) bool {
	return v0+sumFlows(flows) <= valueTol
}
