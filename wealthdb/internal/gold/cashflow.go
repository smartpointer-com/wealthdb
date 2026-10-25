package gold

import (
	"context"
	"database/sql"
)

// The cashflow readers: spending.go and income.go with five grains
// instead of three.
//
// Same positional-scan discipline — each function scans `SELECT *` over
// a macro, so the macro's projection, the row struct and the scan list
// are edited together in one change — and the same nil-for-unconverted
// convention, where a bucket nothing could value reports nil rather
// than zero.
//
// THE SIGN, once for every struct below: positive is cash arriving in
// the pool and negative is cash leaving it. The magnitude columns
// (OperatingIn, OperatingOut, Inflow, Outflow) are positive, as the
// families' spend and refund columns are; the net and section columns
// are signed, the Cash row included.

// CashflowSummaryRow is one period bucket of the statement.
//
// OperatingIn and OperatingOut are positive MAGNITUDES — the two halves
// of operating, each summed over its own section — and Operating is
// their signed difference. They are NOT the income and spending
// features' numbers and are deliberately not named after them: the
// three populations differ by construction (docs/CASHFLOW.md §8).
// Investing, Financing and Vehicles are signed nets, and NetCashFlow
// is the four summed. That the four sum to NetCashFlow is structural
// and therefore a guard against arithmetic alone; the reconciliation
// that can catch a wrong population is the memo.
type CashflowSummaryRow struct {
	// PeriodStart is the bucket's opening UTC-midnight epoch second,
	// nil for the single `total` bucket.
	PeriodStart  *int64
	TxnCount     int64
	OperatingIn  *string
	OperatingOut *string
	Operating    *string
	Investing    *string
	Financing    *string
	Vehicles     *string
	NetCashFlow  *string
	// Yield is the yield class alone: what the household's assets
	// produced without its labour. Taxes, Fees and Giving are the three
	// classes lifted out of spending, each a positive magnitude.
	Yield  *string
	Taxes  *string
	Fees   *string
	Giving *string
	// SavingsRate is Operating over OperatingIn, nil where nothing
	// came in.
	SavingsRate *float64
	// The reconciliation memo, off by default and never read by
	// NetCashFlow. CashMeasured is the pool's observed value at the
	// bucket's end minus at its start; FXEffect is the revaluation part
	// of that change; CashFlowMeasured is the net flow over the same
	// measured accounts; Unexplained is what is left, a native-currency
	// mismatch that is near zero when every line is placed and every
	// own-account move paired.
	//
	// Near zero, NOT zero: balances are snapshot-grain and carried
	// forward, so a bucket boundary falling between two snapshots reads
	// a stale balance and the miss reverses next bucket. All four are
	// nil together on a bucket whose boundary has no observed snapshot
	// — a figure computed from part of the pool would be worse than
	// none.
	CashMeasured     *string
	FXEffect         *string
	CashFlowMeasured *string
	Unexplained      *string
}

// CashflowFlowRow is one (bucket, node) of the flows view, netted at
// the level the view was asked for.
//
// Class and Group are nil at the levels that do not reach them: a
// section-level row names no class, a class-level row no group. Inflow
// and Outflow are the line-level gross magnitudes under the node at
// every level, so a class with a small net and a large gross shows its
// churn one column away — and they are nil on the Cash row, which is
// synthesised from the other sections rather than summed from lines.
type CashflowFlowRow struct {
	PeriodStart *int64
	Section     string
	Class       *string
	// ClassLabel and GroupLabel are what Class and Group read as.
	// Presentation only: the keys stay what a caller groups or joins on.
	ClassLabel *string
	Group      *string
	GroupLabel *string
	TxnCount   int64
	Inflow     *string
	Outflow    *string
	Net        *string
	// Share is |Net| over the bucket's hub at the level drawn — the sum
	// of the positive nets there, which equals the sum of the negative
	// ones once cash is a node.
	Share *float64
}

// CashflowSankeyRow is one edge of the window's diagram.
//
// Source and Target are node NAMES, which is what a Sankey
// visualisation keys a node by, and SourceID / TargetID are the
// `section.class.group` keys behind them. Section is the edge's own —
// the section of whichever end is not the household hub.
type CashflowSankeyRow struct {
	Stage    int64
	Source   string
	Target   string
	SourceID string
	TargetID string
	Section  *string
	Value    *string
	Share    *float64
}

// CashflowTransactionRow is one cashflow line: what a summary, a flows
// row or an edge is made of, with the node it resolved to and the
// family verdict behind it.
//
// Name is the instrument on an investing line and the payer or merchant
// on an operating line; it is nil on a financing, vehicle or cash line,
// whose counterparty is an ACCOUNT and which this feature therefore
// never names. Like the families' merchant and payer columns it can
// carry a person, which is why the CLI gives it the free-text privacy
// class.
type CashflowTransactionRow struct {
	SilverSourceID        string
	TransactionExternalID string
	OccurredAt            int64
	AccountExternalID     string
	AccountKind           *string
	DisplayName           *string
	Nickname              *string
	AccountCategory       *string
	Kind                  string
	Section               string
	Class                 string
	ClassLabel            string
	Group                 string
	GroupLabel            string
	Name                  *string
	// Verdict is the family value the node was resolved from, and
	// SpendDetailed / IncomeDetailed are the two overlays' own. A row
	// can carry both: a deposit the matcher paired is
	// `internal_transfer` in each.
	Verdict        *string
	SpendDetailed  *string
	IncomeDetailed *string
	Provenance     *string
	Currency       string
	NetAmount      *string
	Description    *string
	Counterparty   *string
	// ValueOutCcy is NetAmount converted to the requested output
	// currency at occurred_at by the macro. Nil when no FX path
	// resolves.
	ValueOutCcy *string
}

// CashflowSummary returns one row per period bucket over
// [fromEpoch, toEpoch], valued in outCcy. `period` is handed to the
// macro verbatim, so validating it belongs to the caller.
func CashflowSummary(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period string) ([]CashflowSummaryRow, error) {
	return scanRows(ctx, db, "CashflowSummary", `SELECT * FROM report_cashflow_summary(?, ?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy, period}, func(r *CashflowSummaryRow) []any {
			return []any{
				i64(&r.PeriodStart), &r.TxnCount, dec(&r.OperatingIn), dec(&r.OperatingOut), dec(&r.Operating),
				dec(&r.Investing), dec(&r.Financing), dec(&r.Vehicles), dec(&r.NetCashFlow),
				dec(&r.Yield), dec(&r.Taxes), dec(&r.Fees), dec(&r.Giving), flt(&r.SavingsRate),
				dec(&r.CashMeasured), dec(&r.FXEffect), dec(&r.CashFlowMeasured), dec(&r.Unexplained),
			}
		})
}

// CashflowFlows returns the (bucket, node) breakdown. `level` is
// `section`, `class` or `group` and `investing` is `whole` or `class`;
// both are handed to the macro verbatim.
func CashflowFlows(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period, level, investing string) ([]CashflowFlowRow, error) {
	return scanRows(ctx, db, "CashflowFlows", `SELECT * FROM report_cashflow_flows(?, ?, ?, ?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy, period, level, investing}, func(r *CashflowFlowRow) []any {
			return []any{
				i64(&r.PeriodStart), &r.Section, str(&r.Class), str(&r.ClassLabel), str(&r.Group), str(&r.GroupLabel),
				&r.TxnCount, dec(&r.Inflow), dec(&r.Outflow), dec(&r.Net), flt(&r.Share),
			}
		})
}

// CashflowSankey returns the window's edge list. It takes no period:
// a diagram is a window, not a series.
func CashflowSankey(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, level, investing string) ([]CashflowSankeyRow, error) {
	return scanRows(ctx, db, "CashflowSankey", `SELECT * FROM report_cashflow_sankey(?, ?, ?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy, level, investing}, func(r *CashflowSankeyRow) []any {
			return []any{
				&r.Stage, &r.Source, &r.Target, &r.SourceID, &r.TargetID,
				str(&r.Section), dec(&r.Value), flt(&r.Share),
			}
		})
}

// CashflowTransactions returns the cashflow lines themselves, oldest
// first.
func CashflowTransactions(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string) ([]CashflowTransactionRow, error) {
	return scanRows(ctx, db, "CashflowTransactions", `SELECT * FROM report_cashflow_transactions(?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy}, func(r *CashflowTransactionRow) []any {
			return []any{
				&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt,
				&r.AccountExternalID, str(&r.AccountKind), str(&r.DisplayName), str(&r.Nickname), str(&r.AccountCategory),
				&r.Kind, &r.Section, &r.Class, &r.ClassLabel, &r.Group, &r.GroupLabel,
				str(&r.Name), str(&r.Verdict), str(&r.SpendDetailed), str(&r.IncomeDetailed), str(&r.Provenance),
				&r.Currency, dec(&r.NetAmount), str(&r.Description), str(&r.Counterparty), dec(&r.ValueOutCcy),
			}
		})
}

// CashflowCoverageRow is one (account, currency, period) of the coverage
// report: what the ledger says the account's cash did, what its balances
// say it did, and whether the two are comparable at all.
//
// Amounts are in the ACCOUNT'S OWN currency. A per-account gap converted
// to an output currency carries a rate error on top of whatever it is
// meant to expose, and the question the report answers — does this
// account's ledger agree with its own balances — is not a question about
// any other currency.
type CashflowCoverageRow struct {
	PeriodStart *int64
	SourceID    string
	// Account is the account's label: its nickname, else its display
	// name, else its id. Nickname is the nickname, nil where it has none.
	Account     string
	Nickname    *string
	AccountKind string
	Currency    string
	TxnCount    int64
	// Ledger is the signed cash delta the transactions imply.
	// UnsignedVolume is the gross the six unsigned kinds carried and
	// that therefore could NOT be summed into it — the report's own
	// blind spot, published rather than hidden.
	Ledger         *string
	UnsignedVolume *string
	// Measured is the balance delta across the period, nil where the
	// balances cannot answer. Gap is Ledger minus Measured.
	Measured *string
	Gap      *string
	// Status is `measured` (the gap stands, zero where the account
	// reconciles), `obscured` (the account carries unsigned volume and
	// the gap is no larger than it, so it is not answerable), `opening`
	// (the balance series begins inside the period), `ended` (the series
	// stopped before it) or `unmeasurable` (no balances).
	Status string
}

// CashflowCoverage reads the per-account coverage report.
func CashflowCoverage(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, period string) ([]CashflowCoverageRow, error) {
	return scanRows(ctx, db, "CashflowCoverage", `SELECT * FROM report_cashflow_coverage(?, ?, ?)`,
		[]any{fromEpoch, toEpoch, period}, func(r *CashflowCoverageRow) []any {
			return []any{
				i64(&r.PeriodStart), &r.SourceID, &r.Account, str(&r.Nickname), &r.AccountKind,
				&r.Currency, &r.TxnCount, dec(&r.Ledger), dec(&r.UnsignedVolume), dec(&r.Measured), dec(&r.Gap), &r.Status,
			}
		})
}
