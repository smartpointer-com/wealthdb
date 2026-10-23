package gold

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A holding's legal situs is the one country whose law it is an interest
// under: a fund's domicile, a share's issuer's country, the bank that
// holds a deposit. It never looks through a fund. The pass below reads
// what gold knows of every holding and applies the rule of
// docs/EXPOSURE.md §13 in its order; exposure_leaves stamps the result
// on every leaf the holding gives.

// nportProvider is the refdata provider of SEC N-PORT filings
// (collectors/refdata/providers/sec_nport.py). Only a US registered
// investment company files one.
const nportProvider = "sec-nport"

// The bases a situs rests on, in the rule's order.
const (
	situsDeclared   = "declared"
	situsUnderlying = "underlying"
	situsISIN       = "isin"
	situsDomicile   = "domicile"
	situsCustody    = "custody"
	situsCountry    = "country"
	situsUnknown    = canonical.Unknown
)

// situsFacts is what the rule reads of one holding.
type situsFacts struct {
	// coin marks a coin held in a wallet: no issuer, so a security its
	// symbol also names says nothing of it.
	coin bool
	// receipt marks a depositary receipt: a holding no fund, which a
	// lookup or GLEIF calls one, or whose name says so unless it has an
	// ISIN of a country other than the depositary's US. share is the
	// situs of the share it stands for, "" while the share is unknown.
	receipt bool
	share   string
	// isin is the country of the holding's ISIN prefix, where a source
	// states the ISIN and the prefix is a country.
	isin string
	// domicile is a fund's domicile from its attributes, else US for a
	// fund whose latest composition is an N-PORT filing, else the
	// security's own country of domicile; never one its ISIN's prefix
	// implies.
	domicile string
	// banked marks cash at a bank and metal in a vault; custody is the
	// country of its source's custodian.
	banked  bool
	custody string
	// private marks a private holding no composition of its own
	// describes.
	private bool
}

// situsRecord is what one record of the declarations ledger says of the
// situs: the situs it declares, and, where it declares the economic
// dimensions, the one country it declares at 100.
type situsRecord struct {
	asOf           *int64
	situs, country string
	economic       bool
}

// situsOf is the rule: the first basis that applies. declared is the
// situs a record declares on the day; country the one country the
// economic record in force on the day declares at 100.
func situsOf(f situsFacts, declared, country string) (situs, basis string) {
	switch {
	case declared != "":
		return declared, situsDeclared
	case f.coin:
		return "", situsUnknown
	case f.receipt:
		// A receipt's own ISIN is the depositary's: without its share it
		// has no situs.
		if f.share != "" {
			return f.share, situsUnderlying
		}
		return "", situsUnknown
	case f.isin != "":
		return f.isin, situsISIN
	case f.domicile != "":
		return f.domicile, situsDomicile
	case f.banked && f.custody != "":
		return f.custody, situsCustody
	case f.private && country != "":
		return country, situsCountry
	}
	return "", situsUnknown
}

// situsReading is a holding's situs and basis from asOf on, nil for the
// reading before any dated record.
type situsReading struct {
	asOf         *int64
	situs, basis string
}

// situsReadings are a holding's readings over time, records oldest first
// (undated ones first). The situs and the economic dimensions each have
// a precedence of their own: on a day, the declared situs is the latest
// record on or before it that states one, and the country rule reads the
// latest economic record on or before it; an undated record counts as
// before every dated one. A reading starts before any dated record, and
// again on each day it changes.
func situsReadings(f situsFacts, records []situsRecord) []situsReading {
	situs, basis := situsOf(f, "", "")
	out := []situsReading{{situs: situs, basis: basis}}
	var declared, country string
	for i, r := range records {
		if r.situs != "" {
			declared = r.situs
		}
		if r.economic {
			country = r.country
		}
		if i+1 < len(records) && sameDay(records[i+1].asOf, r.asOf) {
			continue // a day's records count together
		}
		situs, basis := situsOf(f, declared, country)
		switch last := &out[len(out)-1]; {
		case r.asOf == nil:
			last.situs, last.basis = situs, basis // the undated records read before any dated one
		case last.situs != situs || last.basis != basis:
			out = append(out, situsReading{asOf: r.asOf, situs: situs, basis: basis})
		}
	}
	return out
}

func sameDay(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// statedISINsSQL is the CTEs countries, the codes a situs may be (every
// country exposure_countries knows but the European Union), and isins,
// each ISIN a source states under a country's prefix, with that country.
// An ISIN only the US CUSIP rule gives is a guess and is not among them.
const statedISINsSQL = `countries AS (SELECT code FROM exposure_countries WHERE code <> '` + canonical.EuropeanUnion + `'),
stated AS (
    SELECT 'isin:' || upper(trim(isin)) AS key FROM instruments WHERE isin IS NOT NULL
    UNION SELECT 'isin:' || upper(trim(isin)) FROM security_identities WHERE isin IS NOT NULL
    UNION SELECT 'isin:' || upper(trim(isin)) FROM fund_lines WHERE isin IS NOT NULL
    UNION SELECT fund_ref FROM fund_compositions WHERE fund_ref LIKE 'isin:%'
    UNION SELECT fund_ref FROM fund_attributes WHERE fund_ref LIKE 'isin:%'),
isins AS (
    SELECT key, substr(key, 6, 2) AS country FROM stated WHERE substr(key, 6, 2) IN (SELECT code FROM countries))`

// situsHoldingsSQL lists every holding gold has held, keyed as a leaf
// names its holding, with the facts situsOf reads. A position reads as
// its latest snapshot; a cash balance is the holding cash:<currency>.
// Its key is read as exposure_holdings reads it (0126): through
// holdingAliasJoins, then instrument_keys and security_aliases. The
// parameter is receiptWording (receiptKeysSQL). Beside countries and
// isins (statedISINsSQL), domiciles are a security's own country of
// domicile, never one its ISIN's prefix implies.
var situsHoldingsSQL = `
WITH ` + securityNamesSQL + `,
held AS (
    (SELECT silver_source_id AS src, account_external_id AS acct, position_key,
            instrument_external_id AS iid, asset_class, vehicle
       FROM positions
    QUALIFY ROW_NUMBER() OVER (PARTITION BY silver_source_id, account_external_id, position_key
                               ORDER BY snapshot_at DESC) = 1)
    UNION ALL
    SELECT DISTINCT silver_source_id, account_external_id, 'cash:' || currency, NULL, 'cash', 'demand_deposit'
      FROM cash_balances),
keyed AS (
    SELECT h.*, exposure_key(COALESCE(ik1.security_key, ik2.security_key)) AS key
      FROM (SELECT h.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
              FROM held h` + holdingAliasJoins("h.src", "h.acct", "h.iid", "h.position_key") + `) h
      LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = h.src AND ik1.instrument_external_id = h.akey
      LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = h.src AND ik2.instrument_external_id = h.iid),
` + statedISINsSQL + `,
domiciles AS (
    SELECT security_key AS key, domicile FROM securities
     WHERE domicile_origin <> 'isin_prefix' AND domicile IN (SELECT code FROM countries)),
` + receiptKeysSQL + `,
shares AS (
    SELECT u.security_key AS key, COALESCE(i.country, d.domicile) AS situs
      FROM security_underlyings u
      LEFT JOIN isins i ON i.key = u.underlying_key
      LEFT JOIN domiciles d ON d.key = u.underlying_key),
fund_domiciles AS (
    SELECT exposure_key(fund_ref) AS key, domicile
      FROM fund_attributes WHERE domicile IN (SELECT code FROM countries)
   QUALIFY ROW_NUMBER() OVER (PARTITION BY exposure_key(fund_ref) ORDER BY as_of DESC, silver_source_id) = 1),
nport AS (
    SELECT key FROM (SELECT exposure_key(fund_ref) AS key, provider,
                            as_of = MAX(as_of) OVER (PARTITION BY exposure_key(fund_ref)) AS latest
                       FROM fund_compositions)
     WHERE latest GROUP BY key HAVING BOOL_OR(provider = '` + nportProvider + `')),
comps AS (SELECT DISTINCT fund_ref AS key FROM exposure_comps())
SELECT k.src, k.acct, k.position_key, COALESCE(k.iid, ''),
       COALESCE(k.asset_class = 'crypto' AND k.vehicle = 'physical', FALSE),
       COALESCE(k.vehicle NOT IN ('etf', 'fund') AND rk.key IS NOT NULL, FALSE),
       COALESCE(sh.situs, ''), COALESCE(i.country, ''),
       COALESCE(fd.domicile, CASE WHEN np.key IS NOT NULL THEN 'US' END, d.domicile, ''),
       COALESCE((k.asset_class = 'cash' AND k.vehicle IN ('demand_deposit', 'time_deposit'))
                OR (k.asset_class = 'metal' AND k.vehicle = 'physical'), FALSE),
       COALESCE(cu.country, ''),
       COALESCE(NOT exposure_public(k.vehicle, k.asset_class) AND k.asset_class NOT IN ('cash', 'metal', 'crypto')
                AND c.key IS NULL, FALSE)
  FROM keyed k
  LEFT JOIN isins i ON i.key = k.key
  LEFT JOIN receipt_keys rk ON rk.key = k.key
  LEFT JOIN shares sh ON sh.key = k.key
  LEFT JOIN fund_domiciles fd ON fd.key = k.key
  LEFT JOIN nport np ON np.key = k.key
  LEFT JOIN domiciles d ON d.key = k.key
  LEFT JOIN exposure_custody cu ON cu.silver_source_id = k.src
  LEFT JOIN comps c ON c.key = k.key`

// situsRecordsSQL lists every record of the declarations ledger, oldest
// first, with the situs it declares, the one country it declares at 100
// if any (a country every part names, other than unknown), and
// whether it declares the economic dimensions.
const situsRecordsSQL = `
SELECT d.silver_source_id, d.instrument_external_id, d.as_of, COALESCE(d.situs, ''), COALESCE(c.country, ''),
       d.dimensions <> ''
  FROM exposure_declarations d
  LEFT JOIN (SELECT line_no, MIN(country) AS country FROM exposure_declaration_leaves GROUP BY 1
              HAVING COUNT(country) = COUNT(*) AND MIN(country) = MAX(country) AND MIN(country) <> '` + canonical.Unknown + `') c
         USING (line_no)
 ORDER BY d.as_of NULLS FIRST, d.line_no`

// writeSitus rewrites exposure_situs: per holding, its readings
// (situsReadings).
func writeSitus(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM exposure_situs`); err != nil {
		return err
	}
	type instrument struct{ src, iid string }
	records := map[instrument][]situsRecord{}
	if err := scanAll(ctx, tx, situsRecordsSQL, func(rows *sql.Rows) error {
		var in instrument
		var r situsRecord
		if err := rows.Scan(&in.src, &in.iid, &r.asOf, &r.situs, &r.country, &r.economic); err != nil {
			return err
		}
		records[in] = append(records[in], r)
		return nil
	}); err != nil {
		return err
	}
	type row struct {
		src, acct, positionKey string
		situsReading
	}
	var out []row
	if err := scanAll(ctx, tx, situsHoldingsSQL, func(rows *sql.Rows) error {
		var h row
		var f situsFacts
		var iid string
		if err := rows.Scan(&h.src, &h.acct, &h.positionKey, &iid, &f.coin, &f.receipt, &f.share, &f.isin, &f.domicile,
			&f.banked, &f.custody, &f.private); err != nil {
			return err
		}
		for _, r := range situsReadings(f, records[instrument{h.src, iid}]) {
			h.situsReading = r
			out = append(out, h)
		}
		return nil
	}, receiptWording.String()); err != nil {
		return err
	}
	return InsertChunked(ctx, tx, "exposure_situs", `INSERT INTO exposure_situs
(silver_source_id, account_external_id, position_key, as_of, situs, basis) VALUES `, `(?, ?, ?, ?, ?, ?)`,
		len(out), func(i int, args []any) []any {
			r := out[i]
			return append(args, r.src, r.acct, r.positionKey, r.asOf, nullableEnumValue(r.situs), r.basis)
		})
}
