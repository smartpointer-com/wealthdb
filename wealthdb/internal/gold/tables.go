package gold

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// SourceTable is one gold table that holds rows per silver source, keyed
// by silver_source_id. The loader reads its three per-source deletes
// from SourceTables: a reset, a load's windowed delete and the sweep of
// excluded accounts.
type SourceTable struct {
	Name string
	// TimeCol is the column a load's windowed delete clears the table
	// by. A table without one is not windowed: a load upserts it (the
	// dimensions, the registration), appends to it (load_audit) or
	// replaces it whole (realized_lots), or a pass or a model run
	// rewrites it.
	TimeCol string
	// RefKind is the kind of record a reference table holds
	// (canonical.Window.Spans), so the windowed delete clears it over
	// that kind's span. A windowed table with no kind takes the window
	// whole.
	RefKind string
	// Account marks a table keyed by account_external_id whose rows the
	// sweep of an excluded account removes with the account.
	Account bool
	Class   TableClass
}

// TableClass says what writes a source table, and so what rebuilds it.
type TableClass string

const (
	// ClassStore is a model's verdicts, per source: a reset drops them,
	// and nothing regenerates them, so a rebuild carries them.
	ClassStore TableClass = "store"
	// ClassOverlay is derived from the source's rows by a load or a
	// pass, which writes it again.
	ClassOverlay TableClass = "overlay"
	// ClassLotLedger is the lot engine's ledger (LotLedgerTables), which
	// every lot pass rewrites whole.
	ClassLotLedger TableClass = "lot_ledger"
	// ClassReference is the reference records (ReferenceTables), dated
	// by as_of.
	ClassReference TableClass = "reference"
	// ClassFact is the source's dated facts.
	ClassFact TableClass = "fact"
	// ClassDimension is the source's entities, upserted under the
	// DESIGN §8.4 guard.
	ClassDimension TableClass = "dimension"
	// ClassRegistry is the source's own rows: its registration and its
	// load log.
	ClassRegistry TableClass = "registry"
)

// SourceTables are every per-source table, in the order a reset deletes
// them: the derived tables, the reference records and the facts, then
// the dimensions, the audit and the registration.
var SourceTables = []SourceTable{
	{Name: "symbol_resolutions", Class: ClassStore},
	{Name: "spend_txn_enrichment", Class: ClassOverlay},
	{Name: "income_txn_enrichment", Class: ClassOverlay},
	{Name: "transfer_links", Class: ClassOverlay},
	{Name: "spend_reimbursement_pairs", Class: ClassOverlay},
	{Name: "account_wrapper_history", Class: ClassOverlay},

	{Name: "lots", Class: ClassLotLedger},
	{Name: "lot_disposals", Class: ClassLotLedger},
	{Name: "lot_realized", Class: ClassLotLedger},
	{Name: "lot_findings", Class: ClassLotLedger},
	{Name: "lot_anchors", Class: ClassLotLedger},

	{Name: "fund_compositions", TimeCol: "as_of", RefKind: canonical.RefFundDays, Class: ClassReference},
	{Name: "fund_lines", TimeCol: "as_of", RefKind: canonical.RefFundDays, Class: ClassReference},
	{Name: "fund_attributes", TimeCol: "as_of", RefKind: canonical.RefFundDays, Class: ClassReference},
	{Name: "fund_counterparties", TimeCol: "as_of", RefKind: canonical.RefFundDays, Class: ClassReference},
	{Name: "security_identities", TimeCol: "as_of", RefKind: canonical.RefSecurities, Class: ClassReference},
	{Name: "security_facts", TimeCol: "as_of", RefKind: canonical.RefSecurityFacts, Class: ClassReference},
	{Name: "fund_risk", TimeCol: "as_of", RefKind: canonical.RefFundDays, Class: ClassReference},
	{Name: "fund_returns", TimeCol: "as_of", RefKind: canonical.RefFundDays, Class: ClassReference},
	{Name: "fund_characteristics", TimeCol: "as_of", RefKind: canonical.RefFundDays, Class: ClassReference},
	{Name: "security_period_facts", TimeCol: "as_of", RefKind: canonical.RefPeriodFacts, Class: ClassReference},
	{Name: "rate_observations", TimeCol: "as_of", Class: ClassReference},
	{Name: "dividend_calendar", TimeCol: "as_of", RefKind: canonical.RefDividendEvents, Class: ClassReference},
	{Name: "stated_periods", TimeCol: "as_of", Account: true, Class: ClassReference},
	{Name: "exposure_reports", TimeCol: "as_of", Class: ClassReference},

	{Name: "transactions", TimeCol: "occurred_at", Account: true, Class: ClassFact},
	{Name: "realized_lots", Account: true, Class: ClassFact},
	{Name: "fx_rates", TimeCol: "snapshot_at", Class: ClassFact},
	{Name: "cash_balances", TimeCol: "snapshot_at", Account: true, Class: ClassFact},
	{Name: "position_lots", TimeCol: "snapshot_at", Account: true, Class: ClassFact},
	{Name: "positions", TimeCol: "snapshot_at", Account: true, Class: ClassFact},

	{Name: "instruments", Class: ClassDimension},
	{Name: "accounts", Account: true, Class: ClassDimension},
	{Name: "portfolios", Class: ClassDimension},

	{Name: "load_audit", Class: ClassRegistry},
	{Name: "silver_sources", Class: ClassRegistry},
}

// LotLedgerTables are the tables of the engine's ledger that every lot
// pass rewrites whole.
var LotLedgerTables = sourceTableNames(ClassLotLedger)

// ReferenceTables are the tables the reference records load into, every
// one keyed by silver_source_id and windowed by as_of: a load's windowed
// delete and a reset reach them by those two columns.
var ReferenceTables = sourceTableNames(ClassReference)

// sourceTableNames are the names of the source tables of class c, in
// SourceTables order.
func sourceTableNames(c TableClass) []string {
	var out []string
	for _, t := range SourceTables {
		if t.Class == c {
			out = append(out, t.Name)
		}
	}
	return out
}
