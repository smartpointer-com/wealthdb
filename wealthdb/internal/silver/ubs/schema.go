package ubs

import (
	"context"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// webOptionalTables are the ubs-web silver tables a newer silver adds.
// Older silvers predate some of them, and a reader that serves every
// vintage asks rather than assumes.
var webOptionalTables = []string{
	"advices", "account_rates", "statement_trades", "portfolio_transactions",
	"historical_position_snapshots", "historical_cash_balances", "historical_mortgages", "mortgages",
	"card_accounts", "card_transactions", "card_invoices",
	reportsTable, reportLinesTable,
}

// webSchema is which optional parts of the ubs-web silver schema a file
// holds, probed once (webReader.schema).
type webSchema struct {
	tables []bool // each of webOptionalTables
	// historicalCost: historical_position_snapshots.cost_basis (ubs-web
	// migration 0013).
	historicalCost bool
	// windows: dump_runs.window_since, the ledger window of each run.
	windows bool
}

func (s *webSchema) optional() []silver.Optional {
	s.tables = make([]bool, len(webOptionalTables))
	out := []silver.Optional{
		{Table: "historical_position_snapshots", Columns: []string{"cost_basis"}, Has: &s.historicalCost},
		{Table: "dump_runs", Columns: []string{"window_since"}, Has: &s.windows},
	}
	for i, t := range webOptionalTables {
		out = append(out, silver.Optional{Table: t, Has: &s.tables[i]})
	}
	return out
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (r *webReader) schema(ctx context.Context) (webSchema, error) {
	if !r.probed {
		if err := silver.ProbeOptional(ctx, r.db, r.parts.optional()...); err != nil {
			return webSchema{}, err
		}
		r.probed = true
	}
	return r.parts, nil
}

// hasTable reports whether the silver carries an optional table
// (webOptionalTables).
func (r *webReader) hasTable(ctx context.Context, name string) (bool, error) {
	i := slices.Index(webOptionalTables, name)
	if i < 0 {
		return silver.HasTables(ctx, r.db, name)
	}
	s, err := r.schema(ctx)
	if err != nil {
		return false, err
	}
	return s.tables[i], nil
}

// psnSchema is which optional parts of the ubs-psn silver schema a file
// holds, probed once (psnReader.schema).
type psnSchema struct {
	holdingCost bool // holdings.cost_basis
}

func (s *psnSchema) optional() []silver.Optional {
	return []silver.Optional{{Table: "holdings", Columns: []string{"cost_basis"}, Has: &s.holdingCost}}
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (r *psnReader) schema(ctx context.Context) (psnSchema, error) {
	if !r.probed {
		if err := silver.ProbeOptional(ctx, r.db, r.parts.optional()...); err != nil {
			return psnSchema{}, err
		}
		r.probed = true
	}
	return r.parts, nil
}
