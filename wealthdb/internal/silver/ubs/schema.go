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
	reportsTable, reportLinesTable, overviewTotalsTable,
}

// overviewTotalsTable holds the totals a Statement of assets' relationship
// overview prints (ubs-web migration 0018).
const overviewTotalsTable = "statement_overview_totals"

// webTableVersions is the ubs-web migration that adds each of
// webOptionalTables.
var webTableVersions = map[string]int{
	"advices": 13, "account_rates": 16, "statement_trades": 14, "portfolio_transactions": 12,
	"historical_position_snapshots": 2, "historical_cash_balances": 2, "historical_mortgages": 5, "mortgages": 4,
	"card_accounts": 7, "card_transactions": 7, "card_invoices": 7,
	reportsTable: 17, reportLinesTable: 17, overviewTotalsTable: 18,
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
		{Table: "historical_position_snapshots", Columns: []string{"cost_basis"}, Version: 13, Has: &s.historicalCost},
		{Table: "dump_runs", Columns: []string{"window_since"}, Version: 6, Has: &s.windows},
	}
	for i, t := range webOptionalTables {
		out = append(out, silver.Optional{Table: t, Version: webTableVersions[t], Has: &s.tables[i]})
	}
	return out
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (r *webReader) schema(ctx context.Context) (webSchema, error) {
	return r.parts.Get(func() (webSchema, error) {
		var s webSchema
		note, err := silver.ProbeOptional(ctx, r.db, r.file, s.optional()...)
		if err != nil {
			return webSchema{}, err
		}
		r.note = note
		return s, nil
	})
}

// hasTable reports whether the silver carries an optional table. name
// is one of webOptionalTables; any other name is a programming error.
func (r *webReader) hasTable(ctx context.Context, name string) (bool, error) {
	i := slices.Index(webOptionalTables, name)
	if i < 0 {
		panic("ubs: hasTable(" + name + "): not one of webOptionalTables")
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
	return []silver.Optional{{Table: "holdings", Columns: []string{"cost_basis"}, Version: 5, Has: &s.holdingCost}}
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (r *psnReader) schema(ctx context.Context) (psnSchema, error) {
	return r.parts.Get(func() (psnSchema, error) {
		var s psnSchema
		note, err := silver.ProbeOptional(ctx, r.db, r.file, s.optional()...)
		if err != nil {
			return psnSchema{}, err
		}
		r.note = note
		return s, nil
	})
}

// SchemaAdvice is the notes of the feeds whose silver is older than the
// adapter (silver.SchemaAdviser).
func (c *Connection) SchemaAdvice() []silver.SchemaNote {
	var a silver.Advice
	if c.psn != nil {
		a.Note(c.psn.note)
	}
	if c.web != nil {
		a.Note(c.web.note)
	}
	return a.SchemaAdvice()
}
