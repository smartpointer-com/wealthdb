package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// RebuildExposure rewrites the exposure report's derived tables
// (migrations 0126, 0129) from what gold holds: the label maps and
// vocabularies, the labels the fund lines carry, each instrument's
// security key, the aliases that tie two keys of one security together,
// the best-known attributes of every security, and the share each
// depositary receipt stands for. It runs after the
// per-source loads, like the lot pass: an alias may tie a line one
// source loaded to an instrument another one did. One transaction; the
// tables are replaced whole.
func RebuildExposure(ctx context.Context, db *sql.DB) (ExposureCounts, error) {
	var n ExposureCounts
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return n, fmt.Errorf("exposure pass: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	steps := []struct {
		name string
		run  func() error
	}{
		{"vocabularies", func() error { return syncVocabularies(ctx, tx) }},
		{"line labels", func() error { return execAll(ctx, tx, lineLabelsSQL) }},
		{"instrument keys", func() error { return writeInstrumentKeys(ctx, tx) }},
		{"aliases", func() error { return writeAliases(ctx, tx) }},
		{"securities", func() error { return execAll(ctx, tx, securitiesSQL) }},
		{"receipts", func() error { return writeReceipts(ctx, tx) }},
	}
	for _, s := range steps {
		if err := s.run(); err != nil {
			return n, fmt.Errorf("exposure pass: %s: %w", s.name, err)
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT
        (SELECT COUNT(*) FROM securities),
        (SELECT COUNT(*) FROM security_aliases),
        (SELECT COUNT(*) FROM fund_line_labels WHERE value IS NULL)`).
		Scan(&n.Securities, &n.Aliases, &n.UnmappedLabels); err != nil {
		return n, fmt.Errorf("exposure pass: counts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("exposure pass: commit: %w", err)
	}
	return n, nil
}

// ExposureCounts is what the exposure pass rebuilt.
type ExposureCounts struct {
	Securities, Aliases, UnmappedLabels int64
}

func execAll(ctx context.Context, tx *sql.Tx, stmts ...string) error {
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// syncVocabularies writes the label maps, the countries and the SIC
// ranges of internal/canonical into gold.
func syncVocabularies(ctx context.Context, tx *sql.Tx) error {
	if err := execAll(ctx, tx, `DELETE FROM exposure_labels`, `DELETE FROM exposure_countries`, `DELETE FROM exposure_sic`); err != nil {
		return err
	}
	labels := canonical.ExposureLabels()
	if err := InsertChunked(ctx, tx, "exposure_labels", `INSERT INTO exposure_labels (provider, field, label, value) VALUES `,
		`(?, ?, ?, ?)`, len(labels), func(i int, args []any) []any {
			l := labels[i]
			return append(args, l.Provider, string(l.Field), l.Label, l.Value)
		}); err != nil {
		return err
	}
	countries := canonical.Countries()
	if err := InsertChunked(ctx, tx, "exposure_countries", `INSERT INTO exposure_countries (code, name, region) VALUES `,
		`(?, ?, ?)`, len(countries), func(i int, args []any) []any {
			c := countries[i]
			return append(args, c.Code, c.Name, string(c.Region))
		}); err != nil {
		return err
	}
	return InsertChunked(ctx, tx, "exposure_sic", `INSERT INTO exposure_sic (lo, hi, sector, rank) VALUES `,
		`(?, ?, ?, ?)`, len(canonical.SICRanges), func(i int, args []any) []any {
			r := canonical.SICRanges[i]
			return append(args, r.Lo, r.Hi, string(r.Sector), i)
		})
}

// lineLabelsSQL rebuilds fund_line_labels: every distinct label of the
// fund lines, per provider and field, with what the provider's own map,
// else the generic one, says it is.
const lineLabelsSQL = `
DELETE FROM fund_line_labels;
INSERT INTO fund_line_labels
WITH raw AS (
    SELECT DISTINCT c.provider, 'sector' AS field, l.sector AS raw
      FROM fund_lines l JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
     WHERE l.sector IS NOT NULL
    UNION SELECT DISTINCT c.provider, 'country', l.country
      FROM fund_lines l JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
     WHERE l.country IS NOT NULL
    UNION SELECT DISTINCT c.provider, 'country', l.risk_country
      FROM fund_lines l JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
     WHERE l.risk_country IS NOT NULL
    UNION SELECT DISTINCT c.provider, 'asset_type', l.asset_type
      FROM fund_lines l JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
     WHERE l.asset_type IS NOT NULL)
SELECT r.provider, r.field, r.raw, COALESCE(p.value, g.value)
  FROM raw r
  LEFT JOIN exposure_labels p ON p.provider = r.provider AND p.field = r.field AND p.label = exposure_norm(r.raw)
  LEFT JOIN exposure_labels g ON g.provider = '' AND g.field = r.field AND g.label = exposure_norm(r.raw)`

// writeInstrumentKeys gives every instrument its security key
// (canonical.SecurityKey), from its ISIN, CUSIP and symbol (a resolved
// symbol where the source states none).
func writeInstrumentKeys(ctx context.Context, tx *sql.Tx) error {
	if err := execAll(ctx, tx, `DELETE FROM instrument_keys`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT i.silver_source_id, i.instrument_external_id, COALESCE(i.isin, ''), COALESCE(i.cusip, ''), COALESCE(il.symbol, '')
  FROM instruments i
  LEFT JOIN instrument_labels() il ON il.silver_source_id = i.silver_source_id
       AND il.instrument_external_id = i.instrument_external_id`)
	if err != nil {
		return err
	}
	type row struct{ src, iid, key string }
	var out []row
	for rows.Next() {
		var src, iid, isin, cusip, symbol string
		if err := rows.Scan(&src, &iid, &isin, &cusip, &symbol); err != nil {
			rows.Close()
			return err
		}
		if k := canonical.SecurityKey(isin, cusip, "", symbol, ""); k != "" {
			out = append(out, row{src, iid, k})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return InsertChunked(ctx, tx, "instrument_keys",
		`INSERT INTO instrument_keys (silver_source_id, instrument_external_id, security_key) VALUES `,
		`(?, ?, ?)`, len(out), func(i int, args []any) []any {
			return append(args, out[i].src, out[i].iid, out[i].key)
		})
}

// writeAliases rebuilds security_aliases. An identifier a source states
// beside an ISIN (a line's ticker, a lookup's FIGI, an instrument's
// CUSIP) reads as that ISIN. Where two disagree, an account's own
// instrument wins, then a lookup, then a fund line; within one kind, the
// ISIN stated most often, then the newest. The US CUSIP rule ties a CUSIP
// to an ISIN only where no source states one: a CUSIP that starts with a
// digit may as well be Canadian. A broker's symbol and CUSIP fill each
// other's gaps (aliasesSQL).
func writeAliases(ctx context.Context, tx *sql.Tx) error {
	if err := execAll(ctx, tx,
		`DELETE FROM security_aliases`,
		`CREATE TEMP TABLE IF NOT EXISTS exposure_cusip_rule (alias TEXT, security_key TEXT)`,
		`DELETE FROM exposure_cusip_rule`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT k FROM (
    SELECT security_key AS k FROM instrument_keys
    UNION ALL SELECT security_key FROM fund_lines
    UNION ALL SELECT 'cusip:' || upper(trim(cusip)) FROM fund_lines WHERE cusip IS NOT NULL
    UNION ALL SELECT fund_ref FROM fund_compositions
    UNION ALL SELECT security_key FROM security_identities
    UNION ALL SELECT fund_ref FROM exposure_proxies UNION ALL SELECT proxy_ref FROM exposure_proxies)
 WHERE k LIKE 'cusip:%'`)
	if err != nil {
		return err
	}
	type pair struct{ alias, key string }
	var rule []pair
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		if isin := canonical.ISINFromCUSIP(strings.TrimPrefix(k, canonical.KeyCUSIP)); isin != "" {
			rule = append(rule, pair{k, canonical.KeyISIN + isin})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if err := InsertChunked(ctx, tx, "exposure_cusip_rule", `INSERT INTO exposure_cusip_rule (alias, security_key) VALUES `,
		`(?, ?)`, len(rule), func(i int, args []any) []any {
			return append(args, rule[i].alias, rule[i].key)
		}); err != nil {
		return err
	}
	return execAll(ctx, tx, aliasesSQL)
}

// aliasesSQL picks one ISIN per identifier from what the sources state
// beside each other, the CUSIP rule last. A fund line's ticker names a
// US symbol only where the line names a US listing: a US ISIN, a US
// exchange, or, where the line states no exchange, a price in US
// dollars. A broker's symbol does so only where the instrument has a US
// ISIN or is priced in US dollars. A local ticker elsewhere (ROP in
// Zurich) is a different US symbol's (ROP in New York). The exchange
// patterns are the collector's (collectors/refdata/providers/table.py);
// keep them equal.
//
// A broker's symbol and CUSIP, stated together with no ISIN, fill each
// other's gaps. The CUSIP reads as the ISIN a source states for it. Else,
// for a CINS code, which no rule ties to an ISIN, it reads as the ISIN a
// source states for its symbol, never a US one (a CINS issuer sits
// outside North America); for a US or Canadian CUSIP, as that ISIN only
// where it holds the CUSIP (a Canadian one), else as the rule's. A reused
// ticker so leaves the CUSIP alone. A symbol no source names
// reads as its CUSIP does, or as the CUSIP itself, so a lookup's answer
// under it stays with the holding. Only a symbol priced in US dollars
// takes part, by the US-listing rule above.
const aliasesSQL = `
INSERT INTO security_aliases
WITH valid AS (
    SELECT * FROM (
        SELECT 'symbol:' || upper(trim(ticker)) AS alias, upper(trim(isin)) AS isin, 'fund_line' AS origin, as_of
          FROM fund_lines
         WHERE ticker IS NOT NULL AND trim(ticker) <> ''
           AND (upper(trim(isin)) LIKE 'US%'
                OR (exchange IS NULL AND upper(trim(COALESCE(currency, ''))) = 'USD')
                OR (regexp_matches(lower(COALESCE(exchange, '')),
                                   'nyse|new york|nasdaq|cboe|bats|arca|american stock exchange|\biex\b|otc markets')
                    AND NOT regexp_matches(lower(COALESCE(exchange, '')),
                                           'euronext|omx|nordic|baltic|helsinki|stockholm|copenhagen|iceland|tallinn|riga|vilnius|armenia|dubai|canada|australia|europe|japan|\bneo\b')))
        UNION ALL SELECT 'cusip:' || upper(trim(cusip)), upper(trim(isin)), 'fund_line', as_of
          FROM fund_lines WHERE cusip IS NOT NULL AND trim(cusip) <> ''
        UNION ALL SELECT 'figi:' || upper(trim(figi)), upper(trim(isin)), 'fund_line', as_of
          FROM fund_lines WHERE figi IS NOT NULL AND trim(figi) <> ''
        UNION ALL SELECT security_key, upper(trim(isin)), 'lookup', as_of FROM security_identities
        UNION ALL SELECT 'symbol:' || upper(trim(ticker)), upper(trim(isin)), 'lookup', as_of
          FROM security_identities WHERE ticker IS NOT NULL AND trim(ticker) <> ''
        UNION ALL SELECT 'figi:' || upper(trim(figi)), upper(trim(isin)), 'lookup', as_of
          FROM security_identities WHERE figi IS NOT NULL AND trim(figi) <> ''
        UNION ALL SELECT 'cusip:' || upper(trim(cusip)), upper(trim(isin)), 'lookup', as_of
          FROM security_identities WHERE cusip IS NOT NULL AND trim(cusip) <> ''
        UNION ALL SELECT 'symbol:' || upper(trim(symbol)), upper(trim(isin)), 'instrument', last_seen_at
          FROM instruments WHERE symbol IS NOT NULL AND trim(symbol) <> ''
           AND (upper(trim(isin)) LIKE 'US%' OR upper(trim(COALESCE(currency, ''))) = 'USD')
        UNION ALL SELECT 'cusip:' || upper(trim(cusip)), upper(trim(isin)), 'instrument', last_seen_at
          FROM instruments WHERE cusip IS NOT NULL AND trim(cusip) <> '')
     WHERE isin IS NOT NULL AND regexp_full_match(isin, '[A-Z]{2}[A-Z0-9]{9}[0-9]')),
stated AS (
    SELECT alias, 'isin:' || isin AS security_key, origin, COUNT(*) AS n, MAX(as_of) AS latest, 1 AS rnk,
           CASE origin WHEN 'instrument' THEN 1 WHEN 'lookup' THEN 2 ELSE 3 END AS orank
      FROM valid WHERE alias NOT LIKE 'isin:%' GROUP BY alias, isin, origin),
ruled AS (
    SELECT alias, security_key, 'cusip_rule' AS origin, 0 AS n, 0 AS latest, 2 AS rnk, 0 AS orank
      FROM exposure_cusip_rule),
picked AS (
    SELECT * FROM (SELECT * FROM stated UNION ALL SELECT * FROM ruled)
    QUALIFY ROW_NUMBER() OVER (PARTITION BY alias ORDER BY rnk, orank, n DESC, latest DESC, security_key) = 1),
pairs AS (
    SELECT 'symbol:' || upper(trim(symbol)) AS sym, 'cusip:' || upper(trim(cusip)) AS cusip,
           MAX(last_seen_at) AS latest, BOOL_OR(upper(trim(COALESCE(currency, ''))) = 'USD') AS usd
      FROM instruments
     WHERE symbol IS NOT NULL AND trim(symbol) <> '' AND regexp_full_match(upper(trim(cusip)), '[A-Z0-9]{9}')
       AND (isin IS NULL OR trim(isin) = '')
     GROUP BY 1, 2),
stated_isin AS (
    SELECT p.cusip, ps.security_key, p.latest
      FROM pairs p JOIN picked ps ON ps.alias = p.sym
     WHERE p.usd),
held AS (
    SELECT k.cusip,
           CASE WHEN pc.rnk = 1 THEN pc.security_key
                WHEN pc.security_key IS NULL THEN COALESCE(cins.security_key, k.cusip)
                ELSE COALESCE(ca.security_key, pc.security_key) END AS security_key
      FROM (SELECT DISTINCT cusip FROM pairs) k
      LEFT JOIN picked pc ON pc.alias = k.cusip
      LEFT JOIN (SELECT cusip, security_key FROM stated_isin WHERE security_key NOT LIKE 'isin:US%'
                 QUALIFY ROW_NUMBER() OVER (PARTITION BY cusip ORDER BY latest DESC, security_key) = 1) cins
             USING (cusip)
      LEFT JOIN (SELECT cusip, security_key FROM stated_isin WHERE substr(security_key, 8, 9) = substr(cusip, 7)
                 QUALIFY ROW_NUMBER() OVER (PARTITION BY cusip ORDER BY latest DESC, security_key) = 1) ca
             USING (cusip)),
tied AS (
    SELECT cusip AS alias, security_key FROM held WHERE security_key <> cusip
    UNION ALL
    SELECT * FROM (
        SELECT p.sym, h.security_key FROM pairs p JOIN held h USING (cusip)
         WHERE p.usd AND p.sym NOT IN (SELECT alias FROM picked)
        QUALIFY ROW_NUMBER() OVER (PARTITION BY p.sym ORDER BY p.latest DESC, h.security_key) = 1))
SELECT alias, security_key, 'instrument' FROM tied
UNION ALL
SELECT alias, security_key, origin FROM picked WHERE alias NOT IN (SELECT alias FROM tied)`

// securitiesSQL rebuilds securities: per security, the best value of
// each field, by the precedence of docs/EXPOSURE.md §3, and where it
// came from. A label is mapped first; a candidate no map knows never
// wins over one a map knows.
const securitiesSQL = `
DELETE FROM securities;
INSERT INTO securities
WITH ln AS (
    SELECT COALESCE(sa.security_key, l.security_key) AS k, l.as_of, c.provider, l.name, l.currency,
           l.sector, COALESCE(l.risk_country, l.country) AS country
      FROM fund_lines l
      JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
      LEFT JOIN security_aliases sa ON sa.alias = l.security_key
     WHERE l.security_key IS NOT NULL),
-- cmap: the country map a fact reads, the provider's own where it has
-- one (EDGAR's codes would read as other countries in the generic map).
fx AS (
    SELECT COALESCE(sa.security_key, f.security_key) AS k, f.field, f.value, f.provider, f.as_of,
           CASE WHEN f.provider IN (SELECT provider FROM exposure_labels WHERE field = 'country' AND provider <> '')
                THEN f.provider ELSE '' END AS cmap
      FROM security_facts f LEFT JOIN security_aliases sa ON sa.alias = f.security_key),
idn AS (
    SELECT COALESCE(sa.security_key, i.security_key) AS k, i.name, i.listing_currency, i.provider, i.as_of
      FROM security_identities i LEFT JOIN security_aliases sa ON sa.alias = i.security_key),
sic AS (
    SELECT fx.k, NULLIF(s.sector, '') AS sector, fx.as_of
      FROM fx JOIN exposure_sic s ON TRY_CAST(fx.value AS INTEGER) BETWEEN s.lo AND s.hi
     WHERE fx.field = 'sic'
   QUALIFY ROW_NUMBER() OVER (PARTITION BY fx.k, fx.as_of, fx.provider ORDER BY s.rank) = 1),
allkeys AS (
    SELECT k FROM ln UNION SELECT k FROM fx UNION SELECT k FROM idn
    UNION SELECT COALESCE(sa.security_key, ik.security_key) FROM instrument_keys ik
      LEFT JOIN security_aliases sa ON sa.alias = ik.security_key),
cand AS (
    SELECT fx.k, 'sector' AS f, COALESCE(lp.value, lg.value) AS v, 'fact:' || fx.provider AS origin,
           CASE WHEN fx.provider = 'eodhd' THEN 1 ELSE 3 END AS rnk, fx.as_of
      FROM fx
      LEFT JOIN exposure_labels lp ON lp.provider = fx.provider AND lp.field = 'sector' AND lp.label = exposure_norm(fx.value)
      LEFT JOIN exposure_labels lg ON lg.provider = '' AND lg.field = 'sector' AND lg.label = exposure_norm(fx.value)
     WHERE fx.field = 'sector'
    UNION ALL
    SELECT ln.k, 'sector', lab.value, 'fund_line:' || ln.provider, 2, ln.as_of
      FROM ln JOIN fund_line_labels lab ON lab.provider = ln.provider AND lab.field = 'sector' AND lab.raw = ln.sector
    UNION ALL
    SELECT k, 'sector', sector, 'sic', 4, as_of FROM sic
    UNION ALL
    SELECT fx.k, 'risk_country', cl.value, 'fact:' || fx.provider,
           CASE WHEN fx.field = 'hq_country' THEN 4 WHEN fx.provider = 'eodhd' THEN 1 ELSE 3 END, fx.as_of
      FROM fx JOIN exposure_labels cl ON cl.provider = fx.cmap AND cl.field = 'country' AND cl.label = exposure_norm(fx.value)
     WHERE fx.field IN ('risk_country', 'hq_country')
    UNION ALL
    SELECT ln.k, 'risk_country', lab.value, 'fund_line:' || ln.provider, 2, ln.as_of
      FROM ln JOIN fund_line_labels lab ON lab.provider = ln.provider AND lab.field = 'country' AND lab.raw = ln.country
    UNION ALL
    SELECT fx.k, 'domicile', cl.value, 'fact:' || fx.provider,
           CASE fx.field WHEN 'legal_country' THEN 1 ELSE 2 END, fx.as_of
      FROM fx JOIN exposure_labels cl ON cl.provider = fx.cmap AND cl.field = 'country' AND cl.label = exposure_norm(fx.value)
     WHERE fx.field IN ('legal_country', 'incorporation')
    UNION ALL
    SELECT k, 'domicile', substr(k, 6, 2), 'isin_prefix', 3, 0
      FROM allkeys WHERE k LIKE 'isin:%' AND substr(k, 6, 2) IN (SELECT code FROM exposure_countries)
    UNION ALL
    SELECT fx.k, 'market_cap', CAST(TRY_CAST(fx.value AS DOUBLE) AS VARCHAR), 'fact:' || fx.provider,
           CASE WHEN fx.provider = 'eodhd' THEN 1 ELSE 2 END, fx.as_of
      FROM fx WHERE fx.field = 'market_cap'
    UNION ALL
    SELECT fx.k, 'shares_outstanding', CAST(TRY_CAST(fx.value AS DOUBLE) AS VARCHAR), 'fact:' || fx.provider, 1, fx.as_of
      FROM fx WHERE fx.field = 'shares_outstanding'
    UNION ALL
    SELECT k, 'name', name, 'lookup:' || provider, 1, as_of FROM idn WHERE name IS NOT NULL
    UNION ALL
    SELECT k, 'name', name, 'fund_line:' || provider, 2, as_of FROM ln WHERE name IS NOT NULL
    UNION ALL
    SELECT k, 'currency', upper(listing_currency), 'lookup:' || provider, 1, as_of FROM idn
     WHERE regexp_full_match(upper(COALESCE(listing_currency, '')), '[A-Z]{3}')
    UNION ALL
    SELECT k, 'currency', upper(currency), 'fund_line:' || provider, 2, as_of FROM ln
     WHERE regexp_full_match(upper(COALESCE(currency, '')), '[A-Z]{3}')),
best AS (
    SELECT k, f, v, origin FROM cand WHERE v IS NOT NULL
   QUALIFY ROW_NUMBER() OVER (PARTITION BY k, f ORDER BY rnk, as_of DESC, origin, v) = 1)
SELECT a.k,
       MAX(b.v) FILTER (WHERE b.f = 'name'),
       MAX(b.v) FILTER (WHERE b.f = 'currency'),
       MAX(b.v) FILTER (WHERE b.f = 'sector'), MAX(b.origin) FILTER (WHERE b.f = 'sector'),
       MAX(b.v) FILTER (WHERE b.f = 'risk_country'), MAX(b.origin) FILTER (WHERE b.f = 'risk_country'),
       MAX(b.v) FILTER (WHERE b.f = 'domicile'), MAX(b.origin) FILTER (WHERE b.f = 'domicile'),
       TRY_CAST(MAX(b.v) FILTER (WHERE b.f = 'market_cap') AS DOUBLE), MAX(b.origin) FILTER (WHERE b.f = 'market_cap'),
       TRY_CAST(MAX(b.v) FILTER (WHERE b.f = 'shares_outstanding') AS DOUBLE)
  FROM allkeys a LEFT JOIN best b ON b.k = a.k
 WHERE a.k IS NOT NULL
 GROUP BY a.k`
