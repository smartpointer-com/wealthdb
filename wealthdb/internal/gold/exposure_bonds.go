package gold

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// bondDescriptionSQL lists each bond held directly that no source gives a
// sector, with every description a broker gives it and whether its key
// is a US ISIN (a lone US CUSIP reads as one by the CUSIP rule; a
// Canadian bond's CUSIP starts with a digit too, but its ISIN does not
// with US).
const bondDescriptionSQL = `
SELECT s.security_key, ins.name, s.security_key LIKE 'isin:US%'
  FROM (SELECT *, exposure_key(security_key) AS key FROM instrument_keys) ik
  JOIN instruments ins USING (silver_source_id, instrument_external_id)
  JOIN securities s ON s.security_key = ik.key
 WHERE ins.vehicle = 'bond' AND s.sector IS NULL AND trim(COALESCE(ins.name, '')) <> ''
 ORDER BY 1, 2`

// bondFillSQL completes a bond from what its description says: only the
// fields no source states, each with the origin `description`.
const bondFillSQL = `
UPDATE securities SET
    sector = ?, sector_origin = 'description',
    risk_country = COALESCE(risk_country, NULLIF(?, '')),
    risk_country_origin = CASE WHEN risk_country IS NULL AND ? <> '' THEN 'description' ELSE risk_country_origin END,
    currency = COALESCE(currency, NULLIF(?, ''))
 WHERE security_key = ? AND sector IS NULL`

// writeBondReadings completes each bond held directly that no source
// describes from its broker's description (canonical.ReadBondDescription),
// as the last source of its sector. Two descriptions that read otherwise
// leave the bond as it is.
func writeBondReadings(ctx context.Context, tx *sql.Tx) error {
	type reading struct {
		canonical.BondReading
		mixed bool
	}
	readings := map[string]*reading{}
	var keys []string
	if err := scanAll(ctx, tx, bondDescriptionSQL, func(rows *sql.Rows) error {
		var key, desc string
		var usID bool
		if err := rows.Scan(&key, &desc, &usID); err != nil {
			return err
		}
		got, ok := canonical.ReadBondDescription(desc, usID)
		if !ok {
			return nil
		}
		switch r := readings[key]; {
		case r == nil:
			readings[key] = &reading{BondReading: got}
			keys = append(keys, key)
		case r.BondReading != got:
			r.mixed = true
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		r := readings[key]
		if r.mixed {
			continue
		}
		if _, err := tx.ExecContext(ctx, bondFillSQL, string(r.Sector), r.Country, r.Country, r.Currency,
			key); err != nil {
			return err
		}
	}
	return nil
}
