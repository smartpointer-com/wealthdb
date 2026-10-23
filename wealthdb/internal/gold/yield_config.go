package gold

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// SyncYield makes the yield config tables hold exactly the config's
// `yield` block, the built-in conventions, cycles and settlement cycles
// merged in: each table is replaced whole, so an entry taken out of the
// config leaves on the next load. Returns how many curve points,
// withholding rates and rating funds it stamped, and how many
// conventions, cycles and settlement cycles the config sets.
func SyncYield(ctx context.Context, db *sql.DB, rows canonical.YieldRows) (int, error) {
	return replaceConfigTables(ctx, db, "SyncYield", []string{"yield_curves", "yield_withholding", "yield_rating_funds",
		"yield_conventions", "yield_cycles", "yield_settlement_cycles"},
		func(_ *sql.Tx, insert func(q string, args ...any) error) (int, error) {
			n := len(rows.Curves) + len(rows.Withholding) + len(rows.RatingFunds)
			for _, p := range rows.Curves {
				if err := insert(`INSERT INTO yield_curves (currency, tenor, tenor_months, silver_source_id, series_id)
VALUES (?, ?, ?, ?, ?)`, p.Currency, p.Tenor, p.TenorMonths, p.SilverSourceID, p.SeriesID); err != nil {
					return 0, err
				}
			}
			for _, w := range rows.Withholding {
				if err := insert(`INSERT INTO yield_withholding (country, wrapper, rate_pct) VALUES (?, ?, ?)`,
					w.Country, w.Wrapper, w.RatePct); err != nil {
					return 0, err
				}
			}
			for _, f := range rows.RatingFunds {
				if err := insert(`INSERT INTO yield_rating_funds (fund_ref, rating, bucket_high, bucket_low) VALUES (?, ?, ?, ?)`,
					f.FundRef, f.Rating, string(f.BucketHigh), string(f.BucketLow)); err != nil {
					return 0, err
				}
			}
			for _, c := range rows.Conventions {
				if err := insert(`INSERT INTO yield_conventions (market, frequency, day_count, origin) VALUES (?, ?, ?, ?)`,
					c.Market, c.Frequency, string(c.DayCount), string(c.Origin)); err != nil {
					return 0, err
				}
				if c.Origin == canonical.OriginConfig {
					n++
				}
			}
			for _, c := range rows.Cycles {
				if err := insert(`INSERT INTO yield_cycles (sector, cycle, origin) VALUES (?, ?, ?)`,
					string(c.Sector), string(c.Cycle), string(c.Origin)); err != nil {
					return 0, err
				}
				if c.Origin == canonical.OriginConfig {
					n++
				}
			}
			for _, c := range rows.SettlementCycles {
				if err := insert(`INSERT INTO yield_settlement_cycles (market, from_day, days, origin) VALUES (?, ?, ?, ?)`,
					c.Market, nullable(c.From), c.Days, string(c.Origin)); err != nil {
					return 0, err
				}
				if c.Origin == canonical.OriginConfig {
					n++
				}
			}
			return n, nil
		})
}
