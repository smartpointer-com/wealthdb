package gold

import (
	"context"
	"database/sql"
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
// signature itself where the store has nothing. It is nil where none of
// the three holds: on a delta line, which has no payer to name, and on
// a line the kind floor placed off a narrative that reduced to nothing
// — interest the bank credited under its own booking code, most of
// all. It is therefore a narrative
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
	// own filing of the line, mapped to the wealthdb vocabulary and
	// kept beside IncomeDetailed. Nil where the source published nothing
	// this build translates. Never summed with IncomeDetailed: the two
	// disagree by design.
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
	return scanRows(ctx, db, "IncomeSummary", `SELECT * FROM report_income_summary(?, ?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy, period}, func(r *IncomeSummaryRow) []any {
			return []any{
				i64(&r.PeriodStart), &r.TxnCount, dec(&r.Income), dec(&r.Reversals), dec(&r.NetIncome), dec(&r.Withheld),
			}
		})
}

// IncomeTypes returns the (bucket, type) breakdown. `level` is
// `primary` or `detailed` and is handed to the macro verbatim.
func IncomeTypes(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy, period, level string) ([]IncomeTypeRow, error) {
	return scanRows(ctx, db, "IncomeTypes", `SELECT * FROM report_income_types(?, ?, ?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy, period, level}, func(r *IncomeTypeRow) []any {
			return []any{
				i64(&r.PeriodStart), &r.Type, &r.TypeLabel, &r.TxnCount,
				dec(&r.Income), dec(&r.Reversals), dec(&r.NetIncome), flt(&r.Share),
			}
		})
}

// IncomeTransactions returns the income lines themselves, oldest first.
func IncomeTransactions(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string) ([]IncomeTransactionRow, error) {
	return scanRows(ctx, db, "IncomeTransactions", `SELECT * FROM report_income_transactions(?, ?, ?)`,
		[]any{fromEpoch, toEpoch, outCcy}, func(r *IncomeTransactionRow) []any {
			return []any{
				&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt,
				&r.AccountExternalID, str(&r.AccountKind), str(&r.DisplayName), str(&r.Nickname), str(&r.AccountCategory),
				&r.Kind, str(&r.PayerSignature), str(&r.PayerName), str(&r.IncomePrimary), str(&r.IncomeDetailed),
				str(&r.IncomeLabel), str(&r.IncomePrimaryLabel),
				str(&r.Provenance), str(&r.ProviderIncomeDetailed), str(&r.ProviderIncomeLabel), &r.Currency, dec(&r.NetAmount),
				str(&r.Description), str(&r.Counterparty), dec(&r.ValueOutCcy),
			}
		})
}
