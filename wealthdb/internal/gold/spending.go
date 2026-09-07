package gold

import (
	"context"
	"database/sql"
	"fmt"
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
	TxnCount    int64
	Spend       *string
	Refunds     *string
	NetSpend    *string
	Share       *float64
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
	Provenance            *string
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
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_spending_summary(?, ?, ?, ?)`,
		fromEpoch, toEpoch, outCcy, period)
	if err != nil {
		return nil, fmt.Errorf("SpendingSummary: %w", err)
	}
	defer rows.Close()

	var out []SpendSummaryRow
	for rows.Next() {
		var (
			r                        SpendSummaryRow
			bucket                   sql.NullInt64
			spend, refunds, netSpend sql.NullString
		)
		if err := rows.Scan(&bucket, &r.TxnCount, &spend, &refunds, &netSpend); err != nil {
			return nil, fmt.Errorf("SpendingSummary scan: %w", err)
		}
		r.PeriodStart = nullInt64ToPtr(bucket)
		r.Spend = trimmedDecimalPtr(spend)
		r.Refunds = trimmedDecimalPtr(refunds)
		r.NetSpend = trimmedDecimalPtr(netSpend)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpendingCategories returns one row per (period bucket, category)
// over [fromEpoch, toEpoch], valued in outCcy. `level` is 'primary'
// (group by the coarse buckets) or 'detailed'; like `period` it
// reaches the macro verbatim and the caller validates it.
func SpendingCategories(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period, level string) ([]SpendCategoryRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_spending_categories(?, ?, ?, ?, ?)`,
		fromEpoch, toEpoch, outCcy, period, level)
	if err != nil {
		return nil, fmt.Errorf("SpendingCategories: %w", err)
	}
	defer rows.Close()

	var out []SpendCategoryRow
	for rows.Next() {
		var (
			r                        SpendCategoryRow
			bucket                   sql.NullInt64
			spend, refunds, netSpend sql.NullString
			share                    sql.NullFloat64
		)
		if err := rows.Scan(&bucket, &r.Category, &r.TxnCount,
			&spend, &refunds, &netSpend, &share); err != nil {
			return nil, fmt.Errorf("SpendingCategories scan: %w", err)
		}
		r.PeriodStart = nullInt64ToPtr(bucket)
		r.Spend = trimmedDecimalPtr(spend)
		r.Refunds = trimmedDecimalPtr(refunds)
		r.NetSpend = trimmedDecimalPtr(netSpend)
		if share.Valid {
			v := share.Float64
			r.Share = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpendingTransactions returns the spending population at transaction
// grain over [fromEpoch, toEpoch], with net_amount converted to
// outCcy at occurred_at. Ordered by (occurred_at, silver_source_id,
// transaction_external_id) by the macro.
func SpendingTransactions(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string) ([]SpendTransactionRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_spending_transactions(?, ?, ?)`,
		fromEpoch, toEpoch, outCcy)
	if err != nil {
		return nil, fmt.Errorf("SpendingTransactions: %w", err)
	}
	defer rows.Close()

	var out []SpendTransactionRow
	for rows.Next() {
		var (
			r                                    SpendTransactionRow
			acctKind, displayName                sql.NullString
			nickname, category                   sql.NullString
			signature, merchant                  sql.NullString
			primary, detailed, provenance        sql.NullString
			netAmount, description, counterparty sql.NullString
			valueOut                             sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt,
			&r.AccountExternalID, &acctKind, &displayName, &nickname, &category,
			&r.Kind, &signature, &merchant, &primary, &detailed,
			&provenance, &r.Currency, &netAmount,
			&description, &counterparty, &valueOut,
		); err != nil {
			return nil, fmt.Errorf("SpendingTransactions scan: %w", err)
		}
		r.AccountKind = nullStringToPtr(acctKind)
		r.DisplayName = nullStringToPtr(displayName)
		r.Nickname = nullStringToPtr(nickname)
		r.AccountCategory = nullStringToPtr(category)
		r.MerchantSignature = nullStringToPtr(signature)
		r.MerchantName = nullStringToPtr(merchant)
		r.SpendPrimary = nullStringToPtr(primary)
		r.SpendDetailed = nullStringToPtr(detailed)
		r.Provenance = nullStringToPtr(provenance)
		r.Description = nullStringToPtr(description)
		r.Counterparty = nullStringToPtr(counterparty)
		r.NetAmount = trimmedDecimalPtr(netAmount)
		r.ValueOutCcy = trimmedDecimalPtr(valueOut)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullInt64ToPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
