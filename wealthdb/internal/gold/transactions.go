package gold

import (
	"context"
	"database/sql"
)

// TransactionRow is one row of the gold transactions readout.
// Decimal columns come back as canonical strings (DuckDB CAST to
// VARCHAR with trailing-zero trim, matching PositionRow). The
// joined account / instrument fields are nullable and the consumer
// chooses fallbacks for display.
//
// The field ORDER is load-bearing: TransactionsBetween scans
// report_transactions positionally, so this struct, the scan targets
// there, and the macro's projection must be edited together.
type TransactionRow struct {
	SilverSourceID        string
	TransactionExternalID string
	OccurredAt            int64
	AccountExternalID     string
	AccountKind           *string // accounts.account_kind
	DisplayName           *string // accounts.display_name
	RelationshipID        *string
	Nickname              *string
	AccountCategory       *string
	InstrumentExternalID  *string
	Symbol                *string // instruments.symbol
	Name                  *string // instruments.name
	AssetClass            *string // instruments.asset_class
	Kind                  string
	Currency              string
	GrossAmount           *string
	NetAmount             *string
	Quantity              *string
	Price                 *string
	Description           *string // transactions.description; free-text label
	// MerchantName, SpendPrimary and SpendDetailed come from the
	// spending overlay (migration 0042's spend_txn_categories): the
	// merchant the row's signature resolved to and the category the
	// enrichment tiers settled on. Nil for every row the enrichment
	// pass does not reach — investment transactions, and anything
	// outside the spending account scope. MerchantName is the store's
	// name where the store holds one and the row's own signature where
	// it does not (migration 0054); it is nil on a delta row — an
	// own-account move, capital deployed, a gift — which carries its
	// issuer label or nothing (migrations 0048 and 0052).
	MerchantName  *string
	SpendPrimary  *string
	SpendDetailed *string
	// PayerName, IncomePrimary and IncomeDetailed are the same three
	// from the INCOME overlay (income_txn_categories, migration 0070
	// and re-issued by 0073): who paid, and what kind of income the
	// tiers and the kind floor settled on. Nil for every row the income
	// pass does not reach — a purchase, a sale, anything outside the
	// income account scope.
	//
	// A row can carry both trios, and routinely does: a deposit the
	// matcher paired is `internal_transfer` in each overlay, and this
	// is the one surface that shows a transaction from both sides at
	// once. PayerName is the instrument on a dividend, the store's name
	// or the row's own signature on a deposit, and nil on a delta row,
	// which has no payer to name.
	PayerName      *string
	IncomePrimary  *string
	IncomeDetailed *string
	// CheckNumber is the cheque number for an outgoing paper cheque,
	// nil on everything else (gold migration 0075).
	CheckNumber *string
	// InstrumentHint is the token an adapter resolved this row's
	// instrument from when that resolution failed; nil where none was
	// needed or it succeeded. `transaction_instruments` is keyed by it.
	InstrumentHint *string
	// CashflowSection, CashflowClass and CashflowGroup are the node the
	// cash flow statement resolved the row to (migration 0081): which
	// section of the statement, which inner node, which leaf. Read from
	// the RESOLUTION rather than from the base, so a row on an account
	// no cashflow report charts still carries its node.
	//
	// Nil where the resolution reached NO node, which covers two cases
	// the columns do not tell apart: a movement it calls pool-internal —
	// a wire between two of the household's own accounts — and a kind it
	// declines, such as an FX leg or an unpaired card bill. Both mean
	// only that the statement does not draw the row; which of the two it
	// was is `cashflow_txn_nodes`' `disposition`.
	CashflowSection *string
	CashflowClass   *string
	CashflowGroup   *string
	// ValueOutCcy is NetAmount converted to the requested output
	// currency at occurred_at by the report_transactions macro (flat
	// nearest-rate FX in SQL). Nil when no FX path resolves.
	ValueOutCcy *string
}

// SortOrder controls the row ordering for TransactionsBetween.
// Ascending is the default — oldest first; Descending is newest
// first.
type SortOrder int

const (
	SortAscending  SortOrder = iota // oldest first
	SortDescending                  // newest first
)

// TransactionsBetween returns every transaction whose occurred_at
// falls in [fromEpoch, toEpoch], inclusive, with net_amount
// converted to outCcy at occurred_at. Sorted by occurred_at (asc or
// desc per `order`), then (silver_source_id,
// transaction_external_id) as a stable tiebreaker. The query and FX
// are the report_transactions table macro (migration 0099); the macro
// emits ascending, so the descending case re-sorts here.
//
// `SELECT *` with a positional Scan: any column added to the macro
// must be added to TransactionRow and to the scan list below in the
// same position, in the same change as the migration.
func TransactionsBetween(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string, order SortOrder) ([]TransactionRow, error) {
	q := `SELECT * FROM report_transactions(?, ?, ?)`
	if order == SortDescending {
		q += ` ORDER BY occurred_at DESC, silver_source_id, transaction_external_id`
	}
	return scanRows(ctx, db, "TransactionsBetween", q, []any{fromEpoch, toEpoch, outCcy}, func(r *TransactionRow) []any {
		return []any{
			&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt, &r.AccountExternalID,
			str(&r.AccountKind), str(&r.DisplayName), str(&r.RelationshipID), str(&r.Nickname), str(&r.AccountCategory),
			str(&r.InstrumentExternalID), str(&r.Symbol), str(&r.Name), str(&r.AssetClass),
			&r.Kind, &r.Currency,
			dec(&r.GrossAmount), dec(&r.NetAmount), dec(&r.Quantity), dec(&r.Price),
			str(&r.Description), str(&r.MerchantName), str(&r.SpendPrimary), str(&r.SpendDetailed),
			str(&r.PayerName), str(&r.IncomePrimary), str(&r.IncomeDetailed), str(&r.CheckNumber), str(&r.InstrumentHint),
			str(&r.CashflowSection), str(&r.CashflowClass), str(&r.CashflowGroup), dec(&r.ValueOutCcy),
		}
	})
}
