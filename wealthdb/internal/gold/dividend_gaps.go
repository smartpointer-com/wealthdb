package gold

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The gaps the dividend calendar teaches (migration 0148): each issuer's
// from a distribution's record day to its pay day, and each pool of
// funds', which the yield report's reader takes where the calendar dates
// no distribution of a payment (yield.RecordOf, readDividendGaps).

// gapSecurity is what the gap pass knows of a security the calendar
// states distributions of, as the reader knows it (readYieldHeld): whether
// it is a share, by the instruments that name it, else a fund, by them or
// by a fund table; the currency it is quoted in, else that of its
// distributions; and its EDGAR filer.
type gapSecurity struct {
	share         bool
	currency, cik string
}

// gapSecuritiesSQL lists each security the dividend calendar names, by
// its key as the reader takes it (calendarCTEs): whether an instrument
// that names it is a share (a stock of class public equity), whether one
// is none or a fund table names it, the currency the instruments are
// listed in (the first by name), else its distributions', and the CIK of
// its newest EDGAR identity.
const gapSecuritiesSQL = `,
cur AS (
    SELECT key, MIN(currency) AS currency FROM named WHERE key IS NOT NULL GROUP BY 1),
held AS (
    SELECT exposure_key(k.security_key) AS key,
           bool_or(i.vehicle = 'stock' AND i.asset_class = 'public_equity') AS share,
           bool_or(NOT (i.vehicle = 'stock' AND i.asset_class = 'public_equity')) AS other,
           MIN(NULLIF(i.currency, '')) AS currency
      FROM instrument_keys k
      JOIN instruments i ON i.silver_source_id = k.silver_source_id
                        AND i.instrument_external_id = k.instrument_external_id
     WHERE k.security_key IS NOT NULL
     GROUP BY 1),
funds AS (
    SELECT exposure_key(fund_ref) AS key FROM fund_attributes
    UNION SELECT exposure_key(fund_ref) FROM fund_compositions
    UNION SELECT exposure_key(fund_ref) FROM fund_characteristics),
ciks AS (
    SELECT exposure_key(security_key) AS key, arg_max(json_extract_string(payload, '$.cik'), as_of) AS cik
      FROM security_identities WHERE provider = 'sec-edgar' GROUP BY 1)
SELECT c.key, COALESCE(h.share, FALSE) OR NOT (COALESCE(h.other, FALSE) OR f.key IS NOT NULL),
       COALESCE(h.currency, c.currency, ''), COALESCE(i.cik, '')
  FROM cur c
  LEFT JOIN held h ON h.key = c.key
  LEFT JOIN funds f ON f.key = c.key
  LEFT JOIN ciks i ON i.key = c.key`

// gapPoolPrefix marks a pool's row of dividend_gaps: its key is the
// prefix and the pool (yield.FundPool).
const gapPoolPrefix = "pool:"

// gapRow is one row of dividend_gaps.
type gapRow struct {
	key, issuer string
	fund        bool
	gap         yield.Lag
	events      int
}

// writeDividendGaps rebuilds dividend_gaps from the dividend calendar
// (readCalendar) and the settlement cycles. A security's issuer is its
// EDGAR filer where it is a share and EDGAR knows it, so a company's
// share classes learn together, within one market: a receipt's
// depositary keeps its own days. Any other security is its own issuer: a
// fund by its share class. A gap counts in the business days of the
// security's market as the reader reads it (yield.SettlementMarket, its
// ISIN's country standing for its situs): a fund's that of its country,
// else its currency's; a share's that of the currency it is quoted in;
// its ISIN's country where that gives none. Each fund with a gap of its
// own also counts in its pool's (yield.FundPool, yield.PoolGaps), in the
// pool's market: the United States's, else TARGET2's for the European
// pool, else the pool's own.
func writeDividendGaps(ctx context.Context, tx *sql.Tx) error {
	calendar := map[string][]yield.CalendarEvent{}
	if err := readCalendar(ctx, tx, calendar); err != nil {
		return fmt.Errorf("calendar: %w", err)
	}
	cycles := yield.SettlementCycles{}
	if err := readSettlementCycles(ctx, tx, cycles); err != nil {
		return fmt.Errorf("settlement cycles: %w", err)
	}
	securities := map[string]gapSecurity{}
	ctes, args := calendarCTEs()
	if err := scanAll(ctx, tx, ctes+gapSecuritiesSQL, func(rows *sql.Rows) error {
		var key string
		var s gapSecurity
		if err := rows.Scan(&key, &s.share, &s.currency, &s.cik); err != nil {
			return err
		}
		securities[key] = s
		return nil
	}, args...); err != nil {
		return fmt.Errorf("securities: %w", err)
	}
	type issuer struct {
		name   string
		fund   bool
		market string
		keys   []string
		events []yield.CalendarEvent
	}
	issuers := map[string]*issuer{}
	for key, events := range calendar {
		s := securities[key]
		country := ""
		if isin, ok := strings.CutPrefix(key, canonical.KeyISIN); ok && len(isin) >= 2 {
			country = isin[:2]
		}
		market := yield.SettlementMarket(country, s.currency, false, s.share)
		if market == "" {
			market = country
		}
		name, id := key, key
		if s.share && s.cik != "" {
			name = "cik:" + s.cik
			id = name + "|" + market
		}
		is := issuers[id]
		if is == nil {
			is = &issuer{name: name, fund: !s.share, market: market}
			issuers[id] = is
		}
		is.keys = append(is.keys, key)
		is.events = append(is.events, events...)
	}
	var rows []gapRow
	pools := map[string][]yield.Lag{}
	for _, is := range issuers {
		gap, n, ok := yield.LearnGap(is.events, is.market, cycles)
		if !ok {
			continue
		}
		for _, key := range is.keys {
			rows = append(rows, gapRow{key: key, issuer: is.name, fund: is.fund, gap: gap, events: n})
		}
		if pool := yield.FundPool(is.market); is.fund && pool != "" {
			pools[pool] = append(pools[pool], gap)
		}
	}
	for pool, gaps := range pools {
		market := pool
		if pool == "EU" {
			market = ""
		}
		if gap, ok := yield.PoolGaps(gaps, market); ok {
			rows = append(rows, gapRow{key: gapPoolPrefix + pool, issuer: gapPoolPrefix + pool, fund: true, gap: gap,
				events: len(gaps)})
		}
	}
	slices.SortFunc(rows, func(a, b gapRow) int { return strings.Compare(a.key, b.key) })
	if _, err := tx.ExecContext(ctx, `DELETE FROM dividend_gaps`); err != nil {
		return fmt.Errorf("clear: %w", err)
	}
	return InsertChunked(ctx, tx, "dividend_gaps", `
INSERT INTO dividend_gaps (key, issuer, fund, market, days, near, far, events) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(rows), func(i int, args []any) []any {
			r := &rows[i]
			return append(args, r.key, r.issuer, r.fund, r.gap.Market, r.gap.Days, r.gap.Near, r.gap.Far, r.events)
		})
}

// dividendGaps are the gaps the reader takes (yieldShared.gaps): by a
// security's key, and by pool (yield.FundPool).
type dividendGaps struct {
	security, pool map[string]yield.Lag
}

// readDividendGaps reads dividend_gaps into business-day Lags.
func readDividendGaps(ctx context.Context, db querier) (dividendGaps, error) {
	g := dividendGaps{security: map[string]yield.Lag{}, pool: map[string]yield.Lag{}}
	err := scanAll(ctx, db, `SELECT key, market, days, near, far FROM dividend_gaps`, func(rows *sql.Rows) error {
		var key string
		l := yield.Lag{Business: true}
		if err := rows.Scan(&key, &l.Market, &l.Days, &l.Near, &l.Far); err != nil {
			return err
		}
		if pool, ok := strings.CutPrefix(key, gapPoolPrefix); ok {
			g.pool[pool] = l
		} else {
			g.security[key] = l
		}
		return nil
	})
	return g, err
}
