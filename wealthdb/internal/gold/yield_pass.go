package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// RebuildYield rewrites the yield report's derived tables from what gold
// holds: each bond-like instrument's terms (instrument_terms), each
// account's stated interest rates (account_rates) and the gaps the
// dividend calendar teaches (dividend_gaps). It runs after the
// exposure pass, whose instrument keys and securities it reads. One
// transaction; the tables are replaced whole.
func RebuildYield(ctx context.Context, db *sql.DB) (YieldCounts, error) {
	var n YieldCounts
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return n, fmt.Errorf("yield pass: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if err := runSteps("yield pass", []step{
		{"terms", func() error { return writeInstrumentTerms(ctx, tx) }},
		{"rates", func() error { return writeAccountRates(ctx, tx) }},
		{"gaps", func() error { return writeDividendGaps(ctx, tx) }},
	}); err != nil {
		return n, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM instrument_terms),
        (SELECT COUNT(*) FROM instrument_terms WHERE maturity IS NOT NULL),
        (SELECT COUNT(DISTINCT (silver_source_id, account_external_id)) FROM account_rates)`).
		Scan(&n.Instruments, &n.WithMaturity, &n.RatedAccounts); err != nil {
		return n, fmt.Errorf("yield pass: counts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("yield pass: commit: %w", err)
	}
	return n, nil
}

// YieldCounts is what the yield pass rebuilt: the instruments with terms,
// those of them with a maturity, and the accounts with a stated rate.
type YieldCounts struct {
	Instruments, WithMaturity, RatedAccounts int64
}

// The origins of an instrument_terms field, best first.
const (
	originPayload     = "payload"
	originDescription = "description"
	originOpenFIGI    = "openfigi"
	originConvention  = "convention"
)

// bondVehicles are the vehicles whose terms the yield pass reads from
// their sources; a ledger record states any other instrument's
// (instrument_terms_at).
var bondVehicles = []string{"bond", "mortgage", "loan", "convertible_note"}

// ubsRatingPath is where a UBS position's payload keeps the Rating
// column of the positions export: the export's row sits under `web`.
const ubsRatingPath = "$.web.Rating"

// termsCandidatesSQL lists each bond-like instrument with what may state
// its terms: its name; its own figures, each canonical.PayloadTermsKeys
// key from its payload, else from its newest position's payload that
// states one (a statement parser's holding, whose instrument a live feed
// may describe later without them), and that position's UBS Rating;
// OpenFIGI's ticker and market sector for its security, its security's
// sector, and whether its key is a US ISIN (canonical.ReadBondDescription's
// usID).
var termsCandidatesSQL = func() string {
	var stated, picked []string
	for _, k := range append(canonical.PayloadTermsKeys(), ubsRatingPath) {
		stated = append(stated, fmt.Sprintf("json_extract_string(p.payload, '%s')", k))
	}
	for _, k := range canonical.PayloadTermsKeys() {
		picked = append(picked, fmt.Sprintf("COALESCE(json_extract_string(i.payload, '%[1]s'), "+
			"json_extract_string(h.payload, '%[1]s'))", k))
	}
	vehicles := "('" + strings.Join(bondVehicles, "', '") + "')"
	return `
WITH figi AS (
    SELECT exposure_key(security_key) AS key, MIN(json_extract_string(payload, 'local_ticker')) AS ticker,
           MIN(json_extract_string(payload, 'market_sector')) AS market_sector
      FROM security_identities
     WHERE provider = 'openfigi' AND json_extract_string(payload, 'local_ticker') IS NOT NULL
     GROUP BY 1),
held AS (
    SELECT DISTINCT ON (p.silver_source_id, p.instrument_external_id)
           p.silver_source_id, p.instrument_external_id, p.payload
      FROM positions p
      SEMI JOIN instruments b ON b.silver_source_id = p.silver_source_id
           AND b.instrument_external_id = p.instrument_external_id AND b.vehicle IN ` + vehicles + `
     WHERE COALESCE(` + strings.Join(stated, ", ") + `) IS NOT NULL
     ORDER BY p.silver_source_id, p.instrument_external_id, p.snapshot_at DESC, p.account_external_id,
              p.position_key)
SELECT i.silver_source_id, i.instrument_external_id, COALESCE(i.name, ''), COALESCE(i.currency, ''),
       COALESCE(i.vehicle, ''), ` + strings.Join(picked, ", ") + `,
       json_extract_string(h.payload, '` + ubsRatingPath + `'), f.ticker, COALESCE(s.sector, ''),
       COALESCE(f.market_sector, ''), COALESCE(exposure_key(ik.security_key) LIKE 'isin:US%', FALSE)
  FROM instruments i
  LEFT JOIN held h ON h.silver_source_id = i.silver_source_id AND h.instrument_external_id = i.instrument_external_id
  LEFT JOIN instrument_keys ik ON ik.silver_source_id = i.silver_source_id
       AND ik.instrument_external_id = i.instrument_external_id
  LEFT JOIN securities s ON s.security_key = exposure_key(ik.security_key)
  LEFT JOIN figi f ON f.key = exposure_key(ik.security_key)
 WHERE i.vehicle IN ` + vehicles + `
 ORDER BY 1, 2`
}()

// termsField is one instrument_terms field and where it comes from.
type termsField[T any] struct {
	v      *T
	origin string
}

// offer sets the field from origin where it holds nothing yet: the
// origins are offered best first.
func (f *termsField[T]) offer(v *T, origin string) {
	if f.v == nil && v != nil {
		f.v, f.origin = v, origin
	}
}

// offerStated offers v where it is not its type's zero: an empty text or
// a frequency of 0 states nothing.
func offerStated[T comparable](f *termsField[T], v T, origin string) {
	var zero T
	if v != zero {
		f.offer(&v, origin)
	}
}

func (f termsField[T]) value() any { return nullable(f.v) }

func (f termsField[T]) from() any {
	if f.v == nil {
		return nil
	}
	return f.origin
}

type termsRow struct {
	src, iid, vehicle      string
	coupon                 termsField[float64]
	frequency              termsField[int]
	maturity, call         termsField[time.Time]
	kind, dayCount, rating termsField[string]
	makeWhole, inDefault   bool
}

// rate sets the rating and the default flag. A grade outranks an
// agency's word that it rates nothing, as lowestRating weighs one
// source's grades, so the source's own grade comes first, then the
// description's, then either's unrated. The bond is in default where the
// grades that give the rating say so.
func (r *termsRow) rate(own canonical.PayloadTerms, desc canonical.BondTerms) {
	for _, unrated := range []bool{false, true} {
		for _, g := range []struct {
			bucket canonical.RatingBucket
			origin string
		}{{own.Rating, originPayload}, {desc.Rating, originDescription}} {
			if (g.bucket == canonical.RatingUnrated) == unrated {
				offerStated(&r.rating, string(g.bucket), g.origin)
			}
		}
	}
	r.inDefault = desc.Default
	if r.rating.origin == originPayload {
		r.inDefault = own.Default
	}
}

// figiSectors maps OpenFIGI's market sector to a bond sector.
var figiSectors = map[string]canonical.Sector{"Govt": canonical.SectorGovernment, "Muni": canonical.SectorMunicipal,
	"Corp": canonical.SectorCorporate, "Mtge": canonical.SectorSecuritised}

// writeInstrumentTerms rewrites instrument_terms: per field, the source's
// own figure, else the description's, else OpenFIGI's ticker's; a bond's
// frequency and day count else its market's convention. A bond is in
// default where the grades that give its rating say so. A call date of
// the source's own clears the description's make-whole call, as a ledger
// record's does. The ledger's records are laid over at report time;
// instrument_terms_at gives a bond with a coupon and no stated kind its
// kind by convention.
func writeInstrumentTerms(ctx context.Context, tx *sql.Tx) error {
	conventions, err := readConventions(ctx, tx)
	if err != nil {
		return err
	}
	keys := canonical.PayloadTermsKeys()
	var rows []termsRow
	if err := scanAll(ctx, tx, termsCandidatesSQL, func(sc *sql.Rows) error {
		var r termsRow
		var name, ccy, sector, figiSector string
		var ubsRating, ticker sql.NullString
		var usID bool
		values := make([]sql.NullString, len(keys))
		dst := []any{&r.src, &r.iid, &name, &ccy, &r.vehicle}
		for i := range values {
			dst = append(dst, &values[i])
		}
		if err := sc.Scan(append(dst, &ubsRating, &ticker, &sector, &figiSector, &usID)...); err != nil {
			return err
		}
		stated := map[string]string{}
		for i, k := range keys {
			stated[k] = values[i].String
		}
		own := canonical.ReadPayloadTerms(stated, ubsRating.String)
		r.coupon.offer(own.Coupon, originPayload)
		r.maturity.offer(own.Maturity, originPayload)
		offerStated(&r.frequency, own.Frequency, originPayload)
		r.call.offer(own.CallDate, originPayload)
		offerStated(&r.kind, string(own.CouponKind), originPayload)
		desc := canonical.ReadBondTerms(name)
		r.coupon.offer(desc.Coupon, originDescription)
		r.maturity.offer(desc.Maturity, originDescription)
		r.call.offer(desc.CallDate, originDescription)
		r.makeWhole = desc.MakeWhole && own.CallDate == nil
		offerStated(&r.frequency, desc.Frequency, originDescription)
		offerStated(&r.kind, string(desc.CouponKind), originDescription)
		r.rate(own, desc)
		figi := canonical.ReadFigiTicker(ticker.String)
		r.coupon.offer(figi.Coupon, originOpenFIGI)
		r.maturity.offer(figi.Maturity, originOpenFIGI)
		offerStated(&r.kind, string(figi.CouponKind), originOpenFIGI)
		if r.vehicle == "bond" {
			if sector == "" {
				if b, ok := canonical.ReadBondDescription(name, usID); ok {
					sector = string(b.Sector)
				} else {
					sector = string(figiSectors[figiSector])
				}
			}
			r.applyConvention(conventions, ccy, sector, desc.Bill || figi.Bill)
		}
		rows = append(rows, r)
		return nil
	}); err != nil {
		return fmt.Errorf("candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM instrument_terms`); err != nil {
		return err
	}
	return InsertChunked(ctx, tx, "instrument_terms", `INSERT INTO instrument_terms (`+instrumentTermsCols+`) VALUES `,
		placeholders(instrumentTermsCols), len(rows),
		func(i int, args []any) []any {
			r := rows[i]
			return append(args, r.src, r.iid, r.vehicle, r.coupon.value(), r.coupon.from(), r.maturity.value(),
				r.maturity.from(), r.frequency.value(), r.frequency.from(), r.kind.value(), r.kind.from(),
				r.dayCount.value(), r.dayCount.from(), r.call.value(), r.call.from(), r.makeWhole,
				r.rating.value(), r.rating.from(), r.inDefault)
		})
}

// instrumentTermsCols are the columns writeInstrumentTerms fills, in its
// order.
const instrumentTermsCols = `silver_source_id, instrument_external_id, vehicle, coupon_pct, coupon_origin, maturity,
    maturity_origin, frequency, frequency_origin, coupon_kind, coupon_kind_origin, day_count, day_count_origin,
    call_date, call_origin, make_whole, rating, rating_origin, in_default`

// applyConvention fills a bond's frequency and day count from its
// market's convention (yield_conventions): a bill's is its currency's
// bills' (USD:bill), else no coupon on ACT/360; any other bond's its
// currency and sector's (USD:government), else its currency's. A
// floating note takes the day count alone: the frequency is its reset's,
// quarterly where its terms state none.
func (r *termsRow) applyConvention(conventions map[string]canonical.YieldConvention, ccy, sector string, bill bool) {
	var c canonical.YieldConvention
	var ok bool
	if bill {
		c, ok = conventions[ccy+":"+canonical.MarketBill]
		if !ok {
			c, ok = canonical.YieldConvention{Frequency: 0, DayCount: canonical.DayCountAct360}, true
		}
	} else if c, ok = conventions[ccy+":"+sector]; !ok {
		c, ok = conventions[ccy]
	}
	if !ok {
		return
	}
	f, dc := c.Frequency, string(c.DayCount)
	floating := r.kind.v != nil && *r.kind.v == string(canonical.CouponFloating)
	if (bill || f > 0) && !floating {
		r.frequency.offer(&f, originConvention)
	}
	r.dayCount.offer(&dc, originConvention)
}

// readConventions is yield_conventions by market.
func readConventions(ctx context.Context, q querier) (map[string]canonical.YieldConvention, error) {
	out := map[string]canonical.YieldConvention{}
	err := scanAll(ctx, q, `SELECT market, frequency, day_count, origin FROM yield_conventions`,
		func(rows *sql.Rows) error {
			var c canonical.YieldConvention
			var dc, origin string
			if err := rows.Scan(&c.Market, &c.Frequency, &dc, &origin); err != nil {
				return err
			}
			c.DayCount, c.Origin = canonical.DayCount(dc), canonical.YieldOrigin(origin)
			out[c.Market] = c
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("conventions: %w", err)
	}
	return out, nil
}

// accountRatesSQL rewrites account_rates from the rates the sources'
// records state, read by the shape each source writes, and the ledger's
// rate records:
//
//   - a cash balance that states UBS's tiers: its credit and debit tiers
//     (CdtIntrstRate1 to 4, each up to its CdtLmtAmt; a tier past the
//     first only after a tier with a limit), the day each holds from,
//     the day count, and an end before the open one (9999) as a term
//     deposit's. Each snapshot gives its rows, from the balance
//     cash_chosen prefers, so a day reads the snapshot of its own;
//   - an account whose details state Raiffeisen's credit and debit rate
//     ("0,111% ab 01.01.2024": the rate, its decimal a comma or a dot,
//     and the day it holds from);
//   - an account whose strategy states VIAC's rate on its cash;
//   - the ledger: each rate record of an account a source states, from
//     its day on.
//
// A day count the source names is stored as canonical.DayCount names it
// (act/360 reads ACT/360), else NULL.
var accountRatesSQL = func() []string {
	var ubs []string
	for _, side := range []struct{ name, prefix string }{{"credit", "Cdt"}, {"debit", "Dbt"}} {
		for tier := 1; tier <= 4; tier++ {
			prev := "TRUE"
			if tier > 1 {
				prev = fmt.Sprintf(`TRY_CAST(json_extract_string(payload, '%sLmtAmt%d') AS DOUBLE) BETWEEN 0.005 AND 1e14`,
					side.prefix, tier-1)
			}
			ubs = append(ubs, fmt.Sprintf(`
SELECT silver_source_id, account_external_id, currency, '%[1]s' AS side, %[3]d AS tier,
       TRY_CAST(json_extract_string(payload, '%[2]sIntrstRate%[3]d') AS DOUBLE) AS rate_pct,
       CASE WHEN TRY_CAST(json_extract_string(payload, '%[2]sLmtAmt%[3]d') AS DOUBLE) BETWEEN 0.005 AND 1e14
            THEN TRY_CAST(json_extract_string(payload, '%[2]sLmtAmt%[3]d') AS DOUBLE) END AS up_to,
       TRY_CAST(json_extract_string(payload, '%[2]sIntrstVldFrmDt') AS DATE) AS valid_from,
       CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
            THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END AS term_end,
       `+dayCountSQL("json_extract_string(payload, 'CalIntrstBasisDesc')")+` AS day_count,
       '%[2]sIntrstRate%[3]d' AS field, snapshot_at AS observed_at
  FROM ubs_balances
 WHERE %[4]s`, side.name, side.prefix, tier, prev))
		}
	}
	return []string{`DELETE FROM account_rates`, `
INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, up_to, valid_from,
                           term_end, day_count, origin, field, observed_at)
WITH ubs_balances AS MATERIALIZED (
    SELECT DISTINCT ON (cb.silver_source_id, cb.account_external_id, cb.currency, cb.snapshot_at)
           cb.silver_source_id, cb.account_external_id, cb.currency, cb.snapshot_at, cb.payload
      FROM cash_balances cb
     WHERE json_extract_string(cb.payload, 'CdtIntrstRate1') IS NOT NULL
     ORDER BY cb.silver_source_id, cb.account_external_id, cb.currency, cb.snapshot_at,
              ` + cashKindRank + `, cb.balance_kind),
tiers AS (` + strings.Join(ubs, "\nUNION ALL") + `)
SELECT silver_source_id, account_external_id, currency, side, tier, rate_pct, up_to, valid_from, term_end,
       day_count, 'stated', field, observed_at
  FROM tiers WHERE rate_pct IS NOT NULL`, `
INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, valid_from,
                           origin, field)
SELECT a.silver_source_id, a.account_external_id, a.base_currency, s.side, 1,
       TRY_CAST(replace(regexp_extract(json_extract_string(a.payload, '$.details.' || s.key),
                                       '(-?[0-9]+([.,][0-9]+)?)\s*%', 1), ',', '.') AS DOUBLE),
       TRY_STRPTIME(regexp_extract(json_extract_string(a.payload, '$.details.' || s.key),
                                   '([0-9]{2}\.[0-9]{2}\.[0-9]{4})', 1), '%d.%m.%Y')::DATE,
       'stated', 'details.' || s.key
  FROM accounts a
 CROSS JOIN (VALUES ('credit', 'interest_credit'), ('debit', 'interest_debit')) s(side, key)
 WHERE regexp_matches(COALESCE(json_extract_string(a.payload, '$.details.' || s.key), ''), '[0-9]\s*%')`, `
INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, origin, field)
SELECT a.silver_source_id, a.account_external_id, a.base_currency, 'credit', 1,
       TRY_CAST(json_extract_string(a.payload, '$.strategy.interestRate') AS DOUBLE), 'stated', 'strategy.interestRate'
  FROM accounts a
 WHERE TRY_CAST(json_extract_string(a.payload, '$.strategy.interestRate') AS DOUBLE) IS NOT NULL`, `
INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, valid_from,
                           term_end, origin, line_no)
SELECT r.silver_source_id, r.account_external_id, r.currency, 'credit', 1, r.rate_pct,
       DATE '1970-01-01' + CAST(r.as_of // 86400 AS INTEGER), r.until, 'declared', r.line_no
  FROM yield_declared_rates r
  SEMI JOIN accounts a ON a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id`}
}()

// dayCountSQL maps a source's day-count text to canonical.DayCount's
// names: case and spaces aside, ACT/360, ACT/365, ACT/ACT, 30/360 and
// 30E/360; anything else is NULL.
func dayCountSQL(expr string) string {
	var cases []string
	for _, dc := range canonical.DayCounts() {
		cases = append(cases, fmt.Sprintf("WHEN '%s' THEN '%s'", strings.ToUpper(string(dc)), dc))
	}
	return "CASE upper(replace(" + expr + ", ' ', '')) " + strings.Join(cases, " ") + " END"
}

// statedRatesSQL lists the payloads that state a rate under the keys
// canonical.ReadStatedRates reads: each cash balance's, observed at its
// snapshot (of a snapshot's balances, the one cash_chosen prefers among
// those that state one), and each account's, stated once in its base
// currency.
var statedRatesSQL = func() string {
	var has []string
	for _, k := range []string{canonical.PayloadInterestRate, canonical.PayloadInterestTiers,
		canonical.PayloadInterestConditions} {
		has = append(has, fmt.Sprintf("json_exists(%%[1]s.payload, '$.%s')", k))
	}
	states := strings.Join(has, " OR ")
	return `
SELECT * FROM (
    SELECT DISTINCT ON (cb.silver_source_id, cb.account_external_id, cb.currency, cb.snapshot_at)
           cb.silver_source_id, cb.account_external_id, cb.currency, cb.snapshot_at, cb.payload::VARCHAR
      FROM cash_balances cb
     WHERE ` + fmt.Sprintf(states, "cb") + `
     ORDER BY cb.silver_source_id, cb.account_external_id, cb.currency, cb.snapshot_at,
              ` + cashKindRank + `, cb.balance_kind)
UNION ALL
SELECT a.silver_source_id, a.account_external_id, a.base_currency, NULL, a.payload::VARCHAR
  FROM accounts a
 WHERE ` + fmt.Sprintf(states, "a") + `
 ORDER BY 1, 2, 3, 4`
}()

// writeStatedRates adds to account_rates the rates the sources' payloads
// state under the canonical keys, one row per side and tier.
func writeStatedRates(ctx context.Context, tx *sql.Tx) error {
	type row struct {
		src, acct string
		ccy       sql.NullString
		observed  sql.NullInt64
		rate      canonical.StatedRate
	}
	var rows []row
	var tiers int
	if err := scanAll(ctx, tx, statedRatesSQL, func(sc *sql.Rows) error {
		var r row
		var payload sql.NullString
		if err := sc.Scan(&r.src, &r.acct, &r.ccy, &r.observed, &payload); err != nil {
			return err
		}
		for _, rate := range canonical.ReadStatedRates([]byte(payload.String)) {
			r.rate = rate
			rows = append(rows, r)
			tiers += len(rate.Tiers)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("stated rates: %w", err)
	}
	const cols = `silver_source_id, account_external_id, currency, side, tier, rate_pct, up_to, valid_from, origin, field,
    observed_at`
	flat := make([]func(args []any) []any, 0, tiers)
	for _, r := range rows {
		for i, t := range r.rate.Tiers {
			flat = append(flat, func(args []any) []any {
				return append(args, r.src, r.acct, r.ccy, r.rate.Side, i+1, t.Pct, nullable(t.UpTo),
					nullable(r.rate.ValidFrom), "stated", r.rate.Field, r.observed)
			})
		}
	}
	return InsertChunked(ctx, tx, "account_rates", `INSERT INTO account_rates (`+cols+`) VALUES `, placeholders(cols),
		len(flat), func(i int, args []any) []any { return flat[i](args) })
}

// writeAccountRates rewrites account_rates from every shape a source
// states a rate in and the ledger's records.
func writeAccountRates(ctx context.Context, tx *sql.Tx) error {
	if err := execAll(ctx, tx, accountRatesSQL...); err != nil {
		return err
	}
	return writeStatedRates(ctx, tx)
}
