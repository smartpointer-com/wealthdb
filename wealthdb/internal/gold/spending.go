package gold

import (
	"context"
	"database/sql"
)

// The read side of the spending reports (migration 0042). Three
// grains, three query funcs, one shape each: run the macro, scan it
// positionally, hand the caller decimal strings.
//
// Everything that decides WHAT a spending line is — which accounts are
// in scope, which kinds are spend, how a category resolves, that
// own-account moves are out — lives in SQL (migrations 0041/0042).
// Nothing here restates any of it; the period bucketing, the sign
// split and the shares are the macros' work too.
//
// `SELECT *` with a positional Scan, like TransactionsBetween: any
// column added to a macro must be added to the row struct and to the
// scan list below in the same position, in the same change as the
// migration.

// SpendSummaryRow is one period bucket of the spending summary.
// Money columns are canonical decimal strings; nil where no line in
// the bucket had an FX path to the output currency (a bucket with no
// refunds reads "0", not nil — see the macro's NULL-vs-0 note).
type SpendSummaryRow struct {
	// PeriodStart is the bucket's opening UTC-midnight epoch second,
	// nil for the single `total` bucket.
	PeriodStart *int64
	TxnCount    int64
	Spend       *string
	Refunds     *string
	NetSpend    *string
}

// SpendCategoryRow is one (period bucket, category) of the category
// breakdown. Category is never empty — the macro labels an unresolved
// one `(uncategorized)` before grouping. Share is the category's
// |net_spend| over the bucket's Σ|net_spend|, nil when the bucket's
// magnitudes sum to zero.
type SpendCategoryRow struct {
	PeriodStart *int64
	Category    string
	// CategoryLabel is what Category reads as — the taxonomy value with
	// its primary's prefix taken off and opened out (migration 0058).
	// Presentation only: Category stays the key a caller groups on.
	CategoryLabel string
	TxnCount      int64
	Spend         *string
	Refunds       *string
	NetSpend      *string
	Share         *float64
}

// SpendTransactionRow is one spending line — what a summary or a
// category row is made of, with the merchant, the resolved category
// and the tier that decided it.
//
// MerchantName is the merchant store's name for the line's signature
// where the store holds one, and the signature itself where it does
// not (migration 0054) — nil only on a delta line, which carries its
// issuer label or nothing at all (migrations 0048 and 0052). It is
// therefore a narrative fold as often as a written name, like
// MerchantSignature and Description beside it, and all three can carry
// a person. The CLI gives every one of them the free-text privacy
// class for that reason.
type SpendTransactionRow struct {
	SilverSourceID        string
	TransactionExternalID string
	OccurredAt            int64
	AccountExternalID     string
	AccountKind           *string
	DisplayName           *string
	Nickname              *string
	AccountCategory       *string
	Kind                  string
	MerchantSignature     *string
	MerchantName          *string
	SpendPrimary          *string
	SpendDetailed         *string
	// SpendLabel and SpendPrimaryLabel are what the two above read as
	// (migration 0058) — presentation only, never a key.
	SpendLabel        *string
	SpendPrimaryLabel *string
	Provenance        *string
	// ProviderSpendDetailed and ProviderSpendLabel are the ISSUER's own
	// classification of the line, mapped to the wealthdb vocabulary
	// and kept beside SpendDetailed (migration 0057). Nil where the
	// issuer published nothing this build translates. Never summed
	// with SpendDetailed: the two disagree by design.
	ProviderSpendDetailed *string
	ProviderSpendLabel    *string
	Currency              string
	NetAmount             *string
	Description           *string
	Counterparty          *string
	// ValueOutCcy is NetAmount converted to the requested output
	// currency at occurred_at by the macro. Nil when no FX path
	// resolves. Canonically signed — spend negative — unlike the
	// aggregate reports' sign-split magnitudes.
	ValueOutCcy *string
}

// SpendingSummary returns one row per period bucket over
// [fromEpoch, toEpoch], valued in outCcy. `period` is a date_trunc
// part (day | week | month | quarter | year) or the `total` sentinel
// that collapses the window into one bucket; it is handed to the macro
// verbatim, so validating it belongs to the caller — an unrecognised
// part raises rather than bucketing wrongly.
func SpendingSummary(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period string) ([]SpendSummaryRow, error) {
	return scanRows(ctx, db, "SpendingSummary", `SELECT * FROM report_spending_summary(?, ?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy, period}, func(r *SpendSummaryRow) []any {
			return []any{i64(&r.PeriodStart), &r.TxnCount, dec(&r.Spend), dec(&r.Refunds), dec(&r.NetSpend)}
		})
}

// SpendingCategories returns one row per (period bucket, category)
// over [fromEpoch, toEpoch], valued in outCcy. `level` is 'primary'
// (group by the coarse buckets) or 'detailed'; like `period` it
// reaches the macro verbatim and the caller validates it.
func SpendingCategories(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period, level string) ([]SpendCategoryRow, error) {
	return scanRows(ctx, db, "SpendingCategories", `SELECT * FROM report_spending_categories(?, ?, ?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy, period, level}, func(r *SpendCategoryRow) []any {
			return []any{
				i64(&r.PeriodStart), &r.Category, &r.CategoryLabel, &r.TxnCount,
				dec(&r.Spend), dec(&r.Refunds), dec(&r.NetSpend), flt(&r.Share),
			}
		})
}

// SpendingTransactions returns the spending population at transaction
// grain over [fromEpoch, toEpoch], with net_amount converted to
// outCcy at occurred_at. Ordered by (occurred_at, silver_source_id,
// transaction_external_id) by the macro.
func SpendingTransactions(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string) ([]SpendTransactionRow, error) {
	return scanRows(ctx, db, "SpendingTransactions", `SELECT * FROM report_spending_transactions(?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy}, func(r *SpendTransactionRow) []any {
			return []any{
				&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt,
				&r.AccountExternalID, str(&r.AccountKind), str(&r.DisplayName), str(&r.Nickname), str(&r.AccountCategory),
				&r.Kind, str(&r.MerchantSignature), str(&r.MerchantName), str(&r.SpendPrimary), str(&r.SpendDetailed),
				str(&r.SpendLabel), str(&r.SpendPrimaryLabel),
				str(&r.Provenance), str(&r.ProviderSpendDetailed), str(&r.ProviderSpendLabel), &r.Currency, dec(&r.NetAmount),
				str(&r.Description), str(&r.Counterparty), dec(&r.ValueOutCcy),
			}
		})
}
