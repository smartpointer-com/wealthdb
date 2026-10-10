package gold

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// The gains reports read the cost basis, open lots and realized lots
// the adapters load (migration 0115) and the lot engine writes, through
// the report macros of migrations 0116 to 0118. docs/GAINS.md defines every figure; these are the
// scans. Money comes back in the output currency unless a field says
// otherwise, as trimmed decimal strings; ratios as float64.

// GainsGrain is the row grain of GainsBuckets.
type GainsGrain string

const (
	GainsAll        GainsGrain = "all"
	GainsSources    GainsGrain = "sources"
	GainsPortfolios GainsGrain = "portfolios"
	GainsAccounts   GainsGrain = "accounts"
)

// GainsBucketRow is one period bucket at one grain: the whole
// portfolio, a source, a portfolio, or an account. The identifying
// fields the grain does not split by are nil.
type GainsBucketRow struct {
	// PeriodStart is the bucket's first second, nil for `total`.
	PeriodStart *int64

	SilverSourceID       *string
	PortfolioExternalID  *string // "" for a source's accounts outside any portfolio
	PortfolioDisplayName *string
	AccountExternalID    *string
	DisplayName          *string
	Nickname             *string
	AccountKind          *string
	TaxWrapper           *string
	AccountCategory      *string
	RelationshipID       *string

	Realized, RealizedShort, RealizedLong, RealizedOther *string
	UnrealizedStart, UnrealizedEnd, UnrealizedChange     *string
	Gain                                                 *string
	Proceeds, WashDisallowed                             *string

	RealizedLots, Sells, Positions, PositionsWithoutBasis int64
	BasisCoverage                                         *float64
	Quality                                               string
}

// GainsBuckets runs report_gains_buckets over [from, to]. period is the
// macro's bucket unit (`month`, …, `total`).
func GainsBuckets(ctx context.Context, db *sql.DB, from, to int64, outCcy, period string, grain GainsGrain, missing lots.MissingBasis) ([]GainsBucketRow, error) {
	return scanRows(ctx, db, "GainsBuckets", `SELECT * FROM report_gains_buckets(?, ?, ?, ?, ?, p_missing := ?)`,
		[]any{from, to, outCcy, period, string(grain), string(missing)}, func(r *GainsBucketRow) []any {
			return []any{
				i64(&r.PeriodStart),
				str(&r.SilverSourceID), str(&r.PortfolioExternalID), str(&r.PortfolioDisplayName),
				str(&r.AccountExternalID), str(&r.DisplayName), str(&r.Nickname), str(&r.AccountKind),
				str(&r.TaxWrapper), str(&r.AccountCategory), str(&r.RelationshipID),
				dec(&r.Realized), dec(&r.RealizedShort), dec(&r.RealizedLong), dec(&r.RealizedOther),
				dec(&r.UnrealizedStart), dec(&r.UnrealizedEnd), dec(&r.UnrealizedChange),
				dec(&r.Gain), dec(&r.Proceeds), dec(&r.WashDisallowed),
				&r.RealizedLots, &r.Sells, &r.Positions, &r.PositionsWithoutBasis,
				flt(&r.BasisCoverage), &r.Quality,
			}
		})
}

// GainsPositionRow is one account and instrument over a window: its
// holding at both ends, and the lots realized and the events in
// between. LotKey is set only on a row the lots or events alone make,
// where no position line names the instrument at either end.
type GainsPositionRow struct {
	SilverSourceID    string
	AccountExternalID string
	DisplayName       *string
	Nickname          *string
	PositionKey       *string
	LotKey            *string
	Symbol            *string
	Name              *string
	AssetClass        *string
	Vehicle           *string
	Currency          *string

	QuantityStart, QuantityEnd *string
	// At the window's end, in the position's currency.
	BookValue, MarketValue *string
	// The same, in the output currency.
	BookValueOutCcy, ValueOutCcy *string

	UnrealizedStart, UnrealizedEnd, UnrealizedChange *string
	Realized, Gain                                   *string
	UnrealizedRatio                                  *float64

	BasisStamp      *string
	AcquisitionDate *string
	OpenLots        int64
	Quality         string
}

// GainsPositions runs report_gains_positions over [from, to].
func GainsPositions(ctx context.Context, db *sql.DB, from, to int64, outCcy string, missing lots.MissingBasis) ([]GainsPositionRow, error) {
	return scanRows(ctx, db, "GainsPositions", `SELECT * FROM report_gains_positions(?, ?, ?, p_missing := ?)`,
		[]any{from, to, outCcy, string(missing)}, func(r *GainsPositionRow) []any {
			return []any{
				&r.SilverSourceID, &r.AccountExternalID, str(&r.DisplayName), str(&r.Nickname),
				str(&r.PositionKey), str(&r.LotKey), str(&r.Symbol), str(&r.Name),
				str(&r.AssetClass), str(&r.Vehicle), str(&r.Currency),
				dec(&r.QuantityStart), dec(&r.QuantityEnd),
				dec(&r.BookValue), dec(&r.BookValueOutCcy), dec(&r.MarketValue), dec(&r.ValueOutCcy),
				dec(&r.UnrealizedStart), dec(&r.UnrealizedEnd), dec(&r.UnrealizedChange),
				dec(&r.Realized), dec(&r.Gain), flt(&r.UnrealizedRatio),
				str(&r.BasisStamp), str(&r.AcquisitionDate), &r.OpenLots, &r.Quality,
			}
		})
}

// RealizedLotRow is one realized lot whose effective date falls in the
// window. Amounts without OutCcy are in Currency.
type RealizedLotRow struct {
	SilverSourceID       string
	AccountExternalID    string
	DisplayName          *string
	Nickname             *string
	EffectiveDate        string // YYYY-MM-DD
	Undated              bool   // dated at its tax year's last day
	InstrumentExternalID *string
	Symbol               *string
	Description          *string
	Quantity             *string
	AcquisitionDate      *string
	AcquiredVarious      bool
	HeldDays             *int64
	Term                 *string
	Covered              *bool
	Form8949Box          *string
	Currency             string

	Proceeds, BookValue, Gain, WashDisallowed, AccruedMarketDiscount *string
	GainOrigin                                                       string
	ProceedsOutCcy, BookValueOutCcy, GainOutCcy                      *string

	DocumentKind          string
	TaxYear               int64
	IsPrimary             bool
	BasisStamp            *string
	RealizedLotExternalID string
	SourceDocument        *string
	// Disposal is what the lot realized: sell, fee, spend, tender,
	// cash_in_lieu or expiry.
	Disposal string
}

// RealizedLotsBetween runs report_gains_realized over [from, to]: the
// primary lots, or every copy of a sale with all.
func RealizedLotsBetween(ctx context.Context, db *sql.DB, from, to int64, outCcy string, all bool, order SortOrder, missing lots.MissingBasis) ([]RealizedLotRow, error) {
	q := `SELECT * FROM report_gains_realized(?, ?, ?, ?, p_missing := ?)`
	if order == SortDescending {
		q += ` ORDER BY effective_date DESC, silver_source_id, account_external_id, realized_lot_external_id`
	}
	return scanRows(ctx, db, "RealizedLotsBetween", q, []any{from, to, outCcy, all, string(missing)}, func(r *RealizedLotRow) []any {
		return []any{
			&r.SilverSourceID, &r.AccountExternalID, str(&r.DisplayName), str(&r.Nickname),
			&r.EffectiveDate, &r.Undated,
			str(&r.InstrumentExternalID), str(&r.Symbol), str(&r.Description),
			dec(&r.Quantity), str(&r.AcquisitionDate), &r.AcquiredVarious, i64(&r.HeldDays),
			str(&r.Term), boolp(&r.Covered), str(&r.Form8949Box), &r.Currency,
			dec(&r.Proceeds), dec(&r.BookValue), dec(&r.Gain), dec(&r.WashDisallowed), dec(&r.AccruedMarketDiscount),
			&r.GainOrigin, dec(&r.ProceedsOutCcy), dec(&r.BookValueOutCcy), dec(&r.GainOutCcy),
			&r.DocumentKind, &r.TaxYear, &r.IsPrimary, str(&r.BasisStamp),
			&r.RealizedLotExternalID, str(&r.SourceDocument), &r.Disposal,
		}
	})
}

// OpenLotRow is one open lot of a position held at the as-of instant:
// a lot its source states, or on a position the lot engine filled, one
// of the engine's. Amounts without OutCcy are in Currency.
type OpenLotRow struct {
	SilverSourceID    string
	SnapshotAt        int64
	AccountExternalID string
	DisplayName       *string
	Nickname          *string
	PositionKey       string
	Symbol            *string
	Name              *string
	LotKey            string
	AcquisitionDate   *string
	HeldDays          *int64
	Term              *string
	Covered           *bool
	Quantity          *string
	Currency          string

	BookValue, MarketValue *string
	ValueOrigin            *string // stated | pro_rata
	UnrealizedGain         *string
	UnrealizedRatio        *float64

	BookValueOutCcy, ValueOutCcy, UnrealizedOutCcy *string
	BasisOrigin                                    *string
	SourceDocument                                 *string
}

// OpenLotsAsOf runs report_lots at asOf.
func OpenLotsAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string, missing lots.MissingBasis) ([]OpenLotRow, error) {
	return scanRows(ctx, db, "OpenLotsAsOf", `SELECT * FROM report_lots(?, ?, p_missing := ?)`,
		[]any{asOf, outCcy, string(missing)}, func(r *OpenLotRow) []any {
			return []any{
				&r.SilverSourceID, &r.SnapshotAt, &r.AccountExternalID, str(&r.DisplayName), str(&r.Nickname),
				&r.PositionKey, str(&r.Symbol), str(&r.Name), &r.LotKey,
				str(&r.AcquisitionDate), i64(&r.HeldDays), str(&r.Term), boolp(&r.Covered),
				dec(&r.Quantity), &r.Currency,
				dec(&r.BookValue), dec(&r.MarketValue), str(&r.ValueOrigin),
				dec(&r.UnrealizedGain), flt(&r.UnrealizedRatio),
				dec(&r.BookValueOutCcy), dec(&r.ValueOutCcy), dec(&r.UnrealizedOutCcy),
				str(&r.BasisOrigin), str(&r.SourceDocument),
			}
		})
}

// GainsCoverageRow says, per account, how far a window's gains can be
// trusted.
type GainsCoverageRow struct {
	SilverSourceID    string
	AccountExternalID string
	DisplayName       *string
	Nickname          *string
	TaxWrapper        *string

	Value, ValueWithBasis *string
	BasisCoverage         *float64
	BasisStamps           *string
	OpenLots              int64
	Sells                 int64
	RealizedLots          int64
	Documents             *string
	Verdict               string // ok | no_basis | no_realized | partial

	// The lot engine's part: the sales it rebuilt, the positions whose
	// cost basis it wrote, the seeds it opened in the window, and the
	// source's mode and the account's methods in the last lot pass.
	SellsRebuilt, PositionsRebuilt, SeedLots int64
	LotMode, LotMethods                      *string
}

// GainsCoverage runs report_gains_coverage over [from, to].
func GainsCoverage(ctx context.Context, db *sql.DB, from, to int64, outCcy string) ([]GainsCoverageRow, error) {
	return scanRows(ctx, db, "GainsCoverage", `SELECT * FROM report_gains_coverage(?, ?, ?)`,
		[]any{from, to, outCcy}, func(r *GainsCoverageRow) []any {
			return []any{
				&r.SilverSourceID, &r.AccountExternalID, str(&r.DisplayName), str(&r.Nickname), str(&r.TaxWrapper),
				dec(&r.Value), dec(&r.ValueWithBasis), flt(&r.BasisCoverage), str(&r.BasisStamps),
				&r.OpenLots, &r.Sells, &r.RealizedLots, str(&r.Documents), &r.Verdict,
				&r.SellsRebuilt, &r.PositionsRebuilt, &r.SeedLots, str(&r.LotMode), str(&r.LotMethods),
			}
		})
}

// GainsCheckRow is one comparison of the lot engine with what a source
// states, in the output currency (docs/LOTS.md §9).
type GainsCheckRow struct {
	Check             string // realized | positions | anchors | handover
	SilverSourceID    string
	AccountExternalID string
	DisplayName       *string
	Nickname          *string
	Period            *string // the tax year of a realized check
	Items             int64
	Quantity          *string

	EngineCost, StatedCost, CostDelta *string
	EngineGain, StatedGain, GainDelta *string
}

// GainsCheck runs report_gains_check over [from, to].
func GainsCheck(ctx context.Context, db *sql.DB, from, to int64, outCcy string) ([]GainsCheckRow, error) {
	return scanRows(ctx, db, "GainsCheck", `SELECT * FROM report_gains_check(?, ?, ?)`,
		[]any{from, to, outCcy}, func(r *GainsCheckRow) []any {
			return []any{
				&r.Check, &r.SilverSourceID, &r.AccountExternalID, str(&r.DisplayName), str(&r.Nickname),
				str(&r.Period), &r.Items, dec(&r.Quantity),
				dec(&r.EngineCost), dec(&r.StatedCost), dec(&r.CostDelta),
				dec(&r.EngineGain), dec(&r.StatedGain), dec(&r.GainDelta),
			}
		})
}
