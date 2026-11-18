package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// readDividendCalendar reads the dividend calendar (readCalendar), the
// settlement cycles (readSettlementCycles) and the learned gaps
// (readDividendGaps) on its first call.
func (s *yieldShared) readDividendCalendar(ctx context.Context, db querier) error {
	if s.calendar != nil {
		return nil
	}
	calendar, cycles := map[string][]yield.CalendarEvent{}, yield.SettlementCycles{}
	if err := readCalendar(ctx, db, calendar); err != nil {
		return err
	}
	if err := readSettlementCycles(ctx, db, cycles); err != nil {
		return fmt.Errorf("settlement cycles: %w", err)
	}
	gaps, err := readDividendGaps(ctx, db)
	if err != nil {
		return fmt.Errorf("dividend gaps: %w", err)
	}
	s.calendar, s.cycles, s.gaps = calendar, cycles, gaps
	return nil
}

// japaneseExchanges are the codes a lookup names a Japanese exchange by:
// OpenFIGI's for Japan and its exchanges, and the market identifier codes
// of the Tokyo Stock Exchange and of the Japan Exchange Group. An
// exchange named in words (TOKYO, JAPAN) counts too (readCalendar).
var japaneseExchanges = []string{"JP", "JT", "JO", "JN", "JF", "JS", "JQ", "XTKS", "XJPX"}

// tseCodeSQL reads expr as a Tokyo Stock Exchange code, as the
// collector's jpx provider reads a symbol: trimmed, upper case, without
// the exchange a ticker may add (.T, " JP", :JT, TYO, TSE), and a
// five-character code without its trailing check digit 0, as a list may
// state it.
func tseCodeSQL(expr string) string {
	bare := `regexp_replace(upper(trim(` + expr + `)), '[.\s:](T|JP|JT|TYO|TSE)$', '')`
	return `regexp_replace(` + bare + `, '^([0-9A-Z]{4})0$', '\1')`
}

// readCalendar reads the distributions the dividend calendar states into
// calendar, by the key of the security, ordered by provider and event; an
// event of kind other (a split, a stock dividend) is no distribution. It
// reads every row, whatever the instant: a row's as_of is the day a
// source stated it, not the day of the distribution, which a statement
// after the instant states as well as one before. A row's ref reads
// through the security aliases (exposure_key), as the wanted list names
// it. A JPX row (jpx:<code>) names a Japanese share by its Tokyo Stock
// Exchange code: it reads as each security a lookup lists by that ticker
// on a Japanese exchange (OpenFIGI states the code as its local ticker),
// and else stays unread.
func readCalendar(ctx context.Context, db querier, calendar map[string][]yield.CalendarEvent) error {
	ctes, args := calendarCTEs()
	return scanAll(ctx, db, ctes+`
SELECT key, ex_day, record_day, pay_day, estimated FROM named
 WHERE key IS NOT NULL
 ORDER BY key, provider, event_id`, func(rows *sql.Rows) error {
		var key string
		var ex, record, pay sql.NullTime
		var e yield.CalendarEvent
		if err := rows.Scan(&key, &ex, &record, &pay, &e.Estimated); err != nil {
			return err
		}
		e.Ex, e.Record, e.Pay = nullDate(ex), nullDate(record), nullDate(pay)
		calendar[key] = append(calendar[key], e)
		return nil
	}, args...)
}

// calendarCTEs are the CTEs that name each distribution of the dividend
// calendar by the key of its security, as readCalendar reads them
// (`named`: key, provider, event_id, the days, estimated, currency), and
// the arguments they take; `named` keys a JPX row no lookup lists as
// NULL.
func calendarCTEs() (string, []any) {
	exchanges, args := sqlList(japaneseExchanges)
	return `
WITH tse AS (
    SELECT DISTINCT 'jpx:' || code AS ref, exposure_key(security_key) AS key
      FROM (SELECT security_key, upper(trim(exchange)) AS exchange,
                   ` + tseCodeSQL("COALESCE(ticker, json_extract_string(payload, '$.local_ticker'))") + ` AS code
              FROM security_identities)
     WHERE code <> '' AND (list_contains(` + exchanges + `, exchange) OR exchange LIKE '%TOKYO%' OR exchange LIKE '%JAPAN%')),
cal AS (
    SELECT CASE WHEN starts_with(ref, 'jpx:') THEN 'jpx:' || ` + tseCodeSQL("substr(ref, 5)") + ` END AS jpx,
           exposure_key(ref) AS key, provider, event_id, ex_day, record_day, pay_day,
           COALESCE(status = '` + string(canonical.DividendEstimated) + `', FALSE) AS estimated, currency
      FROM dividend_calendar
     WHERE kind <> '` + string(canonical.DividendKindOther) + `'),
named AS (
    SELECT CASE WHEN c.jpx IS NULL THEN c.key ELSE t.key END AS key, c.provider, c.event_id, c.ex_day, c.record_day,
           c.pay_day, c.estimated, c.currency
      FROM cal c LEFT JOIN tse t ON t.ref = c.jpx)`, args()
}

// readSettlementCycles reads each market's settlement cycles, oldest
// first (yield_settlement_cycles).
func readSettlementCycles(ctx context.Context, db querier, out yield.SettlementCycles) error {
	return scanAll(ctx, db, `SELECT market, from_day, days FROM yield_settlement_cycles
 ORDER BY market, from_day NULLS FIRST`, func(rows *sql.Rows) error {
		var c canonical.SettlementCycle
		var from sql.NullTime
		if err := rows.Scan(&c.Market, &from, &c.Days); err != nil {
			return err
		}
		c.From = nullDate(from)
		out[c.Market] = append(out[c.Market], c)
		return nil
	})
}
