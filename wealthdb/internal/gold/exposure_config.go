package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// SyncExposure makes the exposure config tables hold exactly the
// config's `exposure` block: each table is replaced whole, so an entry
// taken out of the config leaves on the next load. The declarations
// ledger's yield records go to the yield_declared_* tables; a record
// that states only those describes no exposure. Returns how many
// proxies, reference and size funds, declarations and custody entries it
// stamped.
func SyncExposure(ctx context.Context, db *sql.DB, rows canonical.ExposureRows) (int, error) {
	return replaceConfigTables(ctx, db, "SyncExposure", []string{"exposure_proxies", "exposure_reference_funds",
		"exposure_size_funds", "exposure_declarations", "exposure_declaration_leaves", "exposure_custody",
		"exposure_settings", "yield_declared_rates", "yield_declared_ratings", "yield_declared_dividends",
		"yield_declared_terms"},
		func(tx *sql.Tx, insert func(q string, args ...any) error) (int, error) {
			for _, p := range rows.Proxies {
				if err := insert(`INSERT INTO exposure_proxies (fund_ref, proxy_ref, always, leverage) VALUES (?, ?, ?, ?)`,
					p.FundRef, p.ProxyRef, p.Always, p.Leverage); err != nil {
					return 0, err
				}
			}
			for _, r := range rows.ReferenceFunds {
				if err := insert(`INSERT INTO exposure_reference_funds (fund_ref) VALUES (?)`, r); err != nil {
					return 0, err
				}
			}
			for _, f := range rows.SizeFunds {
				if err := insert(`INSERT INTO exposure_size_funds (fund_ref, coverage_pct) VALUES (?, ?)`,
					f.FundRef, f.CoveragePct); err != nil {
					return 0, err
				}
			}
			type leaf struct {
				line, no int
				canonical.ExposureDeclaredLeaf
			}
			var leaves []leaf
			for _, d := range rows.Declarations {
				if err := insertYieldDeclaration(insert, d); err != nil {
					return 0, err
				}
				if !d.StatesExposure() {
					continue
				}
				if err := insert(`INSERT INTO exposure_declarations (line_no, silver_source_id, instrument_external_id, as_of,
dimensions, situs, source, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, d.Line, d.SilverSourceID, d.InstrumentExternalID,
					d.AsOf, strings.Join(d.Dimensions, ","), nullableEnumValue(d.Situs), nullableEnumValue(d.Source),
					nullableEnumValue(d.Note)); err != nil {
					return 0, err
				}
				for i, l := range d.Leaves {
					leaves = append(leaves, leaf{d.Line, i + 1, l})
				}
			}
			if err := InsertChunked(ctx, tx, "exposure_declaration_leaves", `INSERT INTO exposure_declaration_leaves
(line_no, leaf_no, share, asset_class, region, country, currency, sector) VALUES `, `(?, ?, ?, ?, ?, ?, ?, ?)`,
				len(leaves), func(i int, args []any) []any {
					l := leaves[i]
					return append(args, l.line, l.no, l.Share, nullableEnumValue(l.AssetClass), nullableEnumValue(l.Region),
						nullableEnumValue(l.Country), nullableEnumValue(l.Currency), nullableEnumValue(l.Sector))
				}); err != nil {
				return 0, err
			}
			for _, c := range rows.Custody {
				if err := insert(`INSERT INTO exposure_custody (silver_source_id, custodian, country) VALUES (?, ?, ?)`,
					c.SilverSourceID, c.Custodian, nullableEnumValue(c.Country)); err != nil {
					return 0, err
				}
			}
			s := rows.Settings
			if err := insert(`INSERT INTO exposure_settings (max_gap_days, aged_days, partial_pct) VALUES (?, ?, ?)`,
				s.MaxGapDays, s.AgedDays, s.PartialPct); err != nil {
				return 0, err
			}
			return len(rows.Proxies) + len(rows.ReferenceFunds) + len(rows.SizeFunds) + len(rows.Declarations) +
				len(rows.Custody), nil
		})
}

// insertYieldDeclaration writes a ledger record's yield records, each to
// its table under the record's line.
func insertYieldDeclaration(insert func(q string, args ...any) error, d canonical.ExposureDeclaration) error {
	src, note := nullableEnumValue(d.Source), nullableEnumValue(d.Note)
	if r := d.Rate; r != nil {
		if err := insert(`INSERT INTO yield_declared_rates (line_no, silver_source_id, account_external_id, as_of,
currency, rate_pct, until, source, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, d.Line, d.SilverSourceID,
			d.AccountExternalID, d.AsOf, nullableEnumValue(r.Currency), r.Pct, r.Until, src, note); err != nil {
			return err
		}
	}
	if r := d.Rating; r != nil {
		if err := insert(`INSERT INTO yield_declared_ratings (line_no, silver_source_id, instrument_external_id, as_of,
rating, agency, source, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, d.Line, d.SilverSourceID, d.InstrumentExternalID,
			d.AsOf, string(r.Bucket), nullableEnumValue(r.Agency), src, note); err != nil {
			return err
		}
	}
	if r := d.Dividend; r != nil {
		if err := insert(`INSERT INTO yield_declared_dividends (line_no, silver_source_id, instrument_external_id,
as_of, amount, frequency, currency, source, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, d.Line, d.SilverSourceID,
			d.InstrumentExternalID, d.AsOf, r.Amount, r.Frequency, nullableEnumValue(r.Currency), src, note); err != nil {
			return err
		}
	}
	if t := d.Terms; t != nil {
		if err := insert(`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, as_of,
coupon_pct, frequency, maturity, call_date, fixed_until, reset_date, coupon_kind, day_count, duration, yield_pct,
source, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, d.Line, d.SilverSourceID,
			d.InstrumentExternalID, d.AsOf, t.Coupon, t.Frequency, t.Maturity, t.CallDate, t.FixedUntil, t.ResetDate,
			nullableEnumValue(string(t.CouponKind)), nullableEnumValue(string(t.DayCount)), t.Duration, t.Yield,
			src, note); err != nil {
			return err
		}
	}
	return nil
}

// replaceConfigTables replaces config tables whole, in one transaction:
// it clears every one of tables, and fill writes the config's rows
// through insert, or through tx for a chunked insert. op prefixes every
// error. Returns fill's count.
func replaceConfigTables(ctx context.Context, db *sql.DB, op string, tables []string,
	fill func(tx *sql.Tx, insert func(q string, args ...any) error) (int, error)) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return 0, fmt.Errorf("%s: clear %s: %w", op, t, err)
		}
	}
	n, err := fill(tx, func(q string, args ...any) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%s: commit: %w", op, err)
	}
	return n, nil
}

// DeclarationsNamingNoHolding are the ledger lines of the declarations
// whose holding, or whose account for a rate, no source states: a
// misspelt instrument, say, or a source not loaded yet. Such a record
// declares nothing.
func DeclarationsNamingNoHolding(ctx context.Context, db *sql.DB) ([]int, error) {
	var lines []int
	err := scanAll(ctx, db, `
WITH named AS (
    SELECT line_no, silver_source_id, instrument_external_id FROM exposure_declarations
    UNION SELECT line_no, silver_source_id, instrument_external_id FROM yield_declared_ratings
    UNION SELECT line_no, silver_source_id, instrument_external_id FROM yield_declared_dividends
    UNION SELECT line_no, silver_source_id, instrument_external_id FROM yield_declared_terms)
SELECT DISTINCT line_no FROM (
    SELECT d.line_no FROM named d
      ANTI JOIN instruments i ON i.silver_source_id = d.silver_source_id
           AND i.instrument_external_id = d.instrument_external_id
    UNION ALL
    SELECT r.line_no FROM yield_declared_rates r
      ANTI JOIN accounts a ON a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id)
 ORDER BY 1`, func(rows *sql.Rows) error {
		var n int
		if err := rows.Scan(&n); err != nil {
			return err
		}
		lines = append(lines, n)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("DeclarationsNamingNoHolding: %w", err)
	}
	return lines, nil
}
