package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// The income readers: spending.go in the other direction.
//
// Same three grains, same helpers, same positional-scan discipline —
// each function scans `SELECT *` over a macro, so the macro's
// projection, the row struct and the scan list are edited together in
// one change. Where the shapes differ there is a reason, and there is
// exactly one: the summary carries `withheld`, which has no spending
// twin (migration 0071).

// IncomeSummaryRow is one period bucket of the income summary.
//
// Income and Reversals are positive MAGNITUDES, mirroring the spending
// summary's Spend and Refunds, and NetIncome is the difference. A
// bucket where nothing converted reports nil rather than zero.
type IncomeSummaryRow struct {
	// PeriodStart is the bucket's opening UTC-midnight epoch second,
	// nil for the single `total` bucket.
	PeriodStart *int64
	TxnCount    int64
	Income      *string
	Reversals   *string
	NetIncome   *string
	// Withheld is tax deducted at source in the same window on the same
	// accounts — a MEMO shown beside the income it was withheld from,
	// never subtracted from it. Income is gross as booked, so the
	// withholding is already the spending side's WITHHOLDING_TAX;
	// netting it here would subtract it from the household's
	// arithmetic twice. Nil where no tax row fell in the bucket.
	Withheld *string
}

// IncomeTypeRow is one (period bucket, income type) of the type
// breakdown. Type is never empty — the macro labels an unresolved one
// `(uncategorized)` before grouping, which is what makes the backlog
// visible in the report rather than absent from it. Share is the
// type's |net_income| over the bucket's Σ|net_income|.
type IncomeTypeRow struct {
	PeriodStart *int64
	Type        string
	// TypeLabel is what Type reads as. Presentation only: Type stays
	// the key a caller groups on.
	TypeLabel string
	TxnCount  int64
	Income    *string
	Reversals *string
	NetIncome *string
	Share     *float64
}

// IncomeTransactionRow is one income line — what a summary or a type
// row is made of, with the payer, the resolved type and the tier that
// decided it.
//
// PayerName is the INSTRUMENT on a line that carries one — the company
// that paid the dividend, the protocol that paid the staking reward —
// the payer store's name for the line's signature otherwise, and the
// signature itself where the store has nothing. It is nil only on a
// delta line, which has no payer to name. It is therefore a narrative
// fold as often as a written name, like PayerSignature and Description
// beside it, and all three can carry a person: the CLI gives every one
// of them the free-text privacy class for that reason.
type IncomeTransactionRow struct {
	SilverSourceID        string
	TransactionExternalID string
	OccurredAt            int64
	AccountExternalID     string
	AccountKind           *string
	DisplayName           *string
	Nickname              *string
	AccountCategory       *string
	Kind                  string
	PayerSignature        *string
	PayerName             *string
	IncomePrimary         *string
	IncomeDetailed        *string
	// IncomeLabel and IncomePrimaryLabel are what the two above read
	// as — presentation only, never a key.
	IncomeLabel        *string
	IncomePrimaryLabel *string
	Provenance         *string
	// ProviderIncomeDetailed and ProviderIncomeLabel are the SOURCE's
	// own filing of the line, mapped to our vocabulary and kept beside
	// ours. Nil where the source published nothing this build
	// translates. Never summed with ours: the two disagree by design.
	ProviderIncomeDetailed *string
	ProviderIncomeLabel    *string
	Currency               string
	NetAmount              *string
	Description            *string
	Counterparty           *string
	// ValueOutCcy is NetAmount converted to the requested output
	// currency at occurred_at by the macro. Nil when no FX path
	// resolves. Canonically signed — a receipt positive, a reversal
	// negative — unlike the aggregate reports' sign-split magnitudes.
	ValueOutCcy *string
}

// IncomeSummary returns one row per period bucket over
// [fromEpoch, toEpoch], valued in outCcy. `period` is handed to the
// macro verbatim, so validating it belongs to the caller.
func IncomeSummary(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period string) ([]IncomeSummaryRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_income_summary(?, ?, ?, ?)`,
		fromEpoch, toEpoch, outCcy, period)
	if err != nil {
		return nil, fmt.Errorf("IncomeSummary: %w", err)
	}
	defer rows.Close()

	var out []IncomeSummaryRow
	for rows.Next() {
		var (
			r                                      IncomeSummaryRow
			bucket                                 sql.NullInt64
			income, reversals, netIncome, withheld sql.NullString
		)
		if err := rows.Scan(&bucket, &r.TxnCount, &income, &reversals, &netIncome, &withheld); err != nil {
			return nil, fmt.Errorf("IncomeSummary scan: %w", err)
		}
		r.PeriodStart = nullInt64ToPtr(bucket)
		r.Income = trimmedDecimalPtr(income)
		r.Reversals = trimmedDecimalPtr(reversals)
		r.NetIncome = trimmedDecimalPtr(netIncome)
		r.Withheld = trimmedDecimalPtr(withheld)
		out = append(out, r)
	}
	return out, rows.Err()
}

// IncomeTypes returns the (bucket, type) breakdown. `level` is
// `primary` or `detailed` and is handed to the macro verbatim.
func IncomeTypes(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period, level string) ([]IncomeTypeRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_income_types(?, ?, ?, ?, ?)`,
		fromEpoch, toEpoch, outCcy, period, level)
	if err != nil {
		return nil, fmt.Errorf("IncomeTypes: %w", err)
	}
	defer rows.Close()

	var out []IncomeTypeRow
	for rows.Next() {
		var (
			r                            IncomeTypeRow
			bucket                       sql.NullInt64
			income, reversals, netIncome sql.NullString
			share                        sql.NullFloat64
		)
		if err := rows.Scan(&bucket, &r.Type, &r.TypeLabel, &r.TxnCount,
			&income, &reversals, &netIncome, &share); err != nil {
			return nil, fmt.Errorf("IncomeTypes scan: %w", err)
		}
		r.PeriodStart = nullInt64ToPtr(bucket)
		r.Income = trimmedDecimalPtr(income)
		r.Reversals = trimmedDecimalPtr(reversals)
		r.NetIncome = trimmedDecimalPtr(netIncome)
		if share.Valid {
			v := share.Float64
			r.Share = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IncomeTransactions returns the income lines themselves, oldest first.
func IncomeTransactions(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string) ([]IncomeTransactionRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_income_transactions(?, ?, ?)`,
		fromEpoch, toEpoch, outCcy)
	if err != nil {
		return nil, fmt.Errorf("IncomeTransactions: %w", err)
	}
	defer rows.Close()

	var out []IncomeTransactionRow
	for rows.Next() {
		var (
			r                                    IncomeTransactionRow
			acctKind, displayName                sql.NullString
			nickname, category                   sql.NullString
			signature, payer                     sql.NullString
			primary, detailed, provenance        sql.NullString
			incomeLabel, primaryLabel            sql.NullString
			providerDetailed, providerLabel      sql.NullString
			netAmount, description, counterparty sql.NullString
			valueOut                             sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt,
			&r.AccountExternalID, &acctKind, &displayName, &nickname, &category,
			&r.Kind, &signature, &payer, &primary, &detailed,
			&incomeLabel, &primaryLabel,
			&provenance, &providerDetailed, &providerLabel, &r.Currency, &netAmount,
			&description, &counterparty, &valueOut,
		); err != nil {
			return nil, fmt.Errorf("IncomeTransactions scan: %w", err)
		}
		r.AccountKind = nullStringToPtr(acctKind)
		r.DisplayName = nullStringToPtr(displayName)
		r.Nickname = nullStringToPtr(nickname)
		r.AccountCategory = nullStringToPtr(category)
		r.PayerSignature = nullStringToPtr(signature)
		r.PayerName = nullStringToPtr(payer)
		r.IncomePrimary = nullStringToPtr(primary)
		r.IncomeDetailed = nullStringToPtr(detailed)
		r.IncomeLabel = nullStringToPtr(incomeLabel)
		r.IncomePrimaryLabel = nullStringToPtr(primaryLabel)
		r.Provenance = nullStringToPtr(provenance)
		r.ProviderIncomeDetailed = nullStringToPtr(providerDetailed)
		r.ProviderIncomeLabel = nullStringToPtr(providerLabel)
		r.Description = nullStringToPtr(description)
		r.Counterparty = nullStringToPtr(counterparty)
		r.NetAmount = trimmedDecimalPtr(netAmount)
		r.ValueOutCcy = trimmedDecimalPtr(valueOut)
		out = append(out, r)
	}
	return out, rows.Err()
}
