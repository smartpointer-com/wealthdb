package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// SyncExposure makes the exposure config tables hold exactly the
// config's `exposure` block: each table is replaced whole, so an entry
// taken out of the config leaves on the next load. Returns how many
// proxies, reference and size funds, tags and custody entries it
// stamped.
func SyncExposure(ctx context.Context, db *sql.DB, rows canonical.ExposureRows) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("SyncExposure: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	for _, t := range []string{"exposure_proxies", "exposure_reference_funds", "exposure_size_funds", "exposure_tags",
		"exposure_custody", "exposure_settings"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return 0, fmt.Errorf("SyncExposure: clear %s: %w", t, err)
		}
	}
	insert := func(q string, args ...any) error {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("SyncExposure: %w", err)
		}
		return nil
	}
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
	for _, t := range rows.Tags {
		if err := insert(`INSERT INTO exposure_tags (silver_source_id, instrument_external_id, sector, country, currency)
VALUES (?, ?, ?, ?, ?)`, t.SilverSourceID, t.InstrumentExternalID,
			nullableEnumValue(t.Sector), nullableEnumValue(t.Country), nullableEnumValue(t.Currency)); err != nil {
			return 0, err
		}
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
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("SyncExposure: commit: %w", err)
	}
	return len(rows.Proxies) + len(rows.ReferenceFunds) + len(rows.SizeFunds) + len(rows.Tags) + len(rows.Custody), nil
}
