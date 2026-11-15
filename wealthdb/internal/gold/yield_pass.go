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

// termsCandidatesSQL lists each bond-like instrument with what may state
// its terms: its name; its own figures, each canonical.PayloadTermsKeys
// key from its payload, else from its newest position's payload that
// states one (a statement parser's holding, whose instrument a live feed
// may describe later without them); OpenFIGI's ticker and market
// sector for its security, its security's sector, and whether its key is a
// US ISIN (canonical.ReadBondDescription's usID).
var termsCandidatesSQL = func() string {
	var stated, picked []string
	for _, k := range canonical.PayloadTermsKeys() {
		stated = append(stated, fmt.Sprintf("json_extract_string(p.payload, '%s')", k))
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
       COALESCE(i.vehicle, ''), ` + strings.Join(picked, ", ") + `, f.ticker, COALESCE(s.sector, ''),
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
		var ticker sql.NullString
		var usID bool
		values := make([]sql.NullString, len(keys))
		dst := []any{&r.src, &r.iid, &name, &ccy, &r.vehicle}
		for i := range values {
			dst = append(dst, &values[i])
		}
		if err := sc.Scan(append(dst, &ticker, &sector, &figiSector, &usID)...); err != nil {
			return err
		}
		stated := map[string]string{}
		for i, k := range keys {
			stated[k] = values[i].String
		}
		own := canonical.ReadPayloadTerms(stated)
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

// accountRatesSQL rewrites account_rates with the ledger's rate records:
// each rate record of an account a source states, from its day on.
// writeStatedRates adds the rates the sources state.
var accountRatesSQL = []string{`DELETE FROM account_rates`, `
INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, valid_from,
                           term_end, origin, line_no)
SELECT r.silver_source_id, r.account_external_id, r.currency, 'credit', 1, r.rate_pct,
       DATE '1970-01-01' + CAST(r.as_of // 86400 AS INTEGER), r.until, 'declared', r.line_no
  FROM yield_declared_rates r
  SEMI JOIN accounts a ON a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id`}

// statedRatesSQL lists the payloads that state a rate under the keys
// canonical.ReadStatedRates reads: each cash balance's, observed at its
// snapshot (of a snapshot's balances, the one cash_chosen prefers among
// those that state one), and each account's of a kind that holds cash
// (cashAccountKinds), stated once in its base currency. An account
// payload is read on a kind that holds cash alone: a liability's rate is
// its terms', and a kind with no deposit has none.
var statedRatesSQL = func() string {
	kinds := make([]string, len(cashAccountKinds))
	for i, k := range cashAccountKinds {
		kinds[i] = "'" + string(k) + "'"
	}
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
     WHERE (` + fmt.Sprintf(states, "cb") + `)
     ORDER BY cb.silver_source_id, cb.account_external_id, cb.currency, cb.snapshot_at,
              ` + cashKindRank + `, cb.balance_kind)
UNION ALL
SELECT a.silver_source_id, a.account_external_id, a.base_currency, NULL, a.payload::VARCHAR
  FROM accounts a
 WHERE (` + fmt.Sprintf(states, "a") + `) AND a.account_kind IN (` + strings.Join(kinds, ", ") + `)
 ORDER BY 1, 2, 3, 4`
}()

// cashAccountKinds are the kinds of account that hold cash at a bank or
// a broker: an account payload states a deposit's rate on these alone.
var cashAccountKinds = []canonical.AccountKind{canonical.AccountKindCash, canonical.AccountKindBrokerage,
	canonical.AccountKindCustody, canonical.AccountKindSafekeeping}

// writeStatedRates adds to account_rates the rates the sources' payloads
// state under the canonical keys, one row per side and tier, each with
// its day, its term's end and its day count. A tier is numbered by its
// own number where the payload states one, else by its place. The rows
// go in as one statement over a list per column (statedRatesInsertSQL),
// so the insert binds fifteen lists however many rows there are.
func writeStatedRates(ctx context.Context, tx *sql.Tx) error {
	var c statedRateColumns
	if err := scanAll(ctx, tx, statedRatesSQL, func(sc *sql.Rows) error {
		var src, acct string
		var ccy, payload sql.NullString
		var observed sql.NullInt64
		if err := sc.Scan(&src, &acct, &ccy, &observed, &payload); err != nil {
			return err
		}
		for _, rate := range canonical.ReadStatedRates([]byte(payload.String)) {
			for i, t := range rate.Tiers {
				c.add(src, acct, ccy, observed, rate, i, t)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("stated rates: %w", err)
	}
	if len(c.src) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, statedRatesInsertSQL, c.src, c.acct, c.ccy, c.hasCcy, c.side, c.tier, c.pct,
		c.upTo, c.hasUpTo, c.validFrom, c.termEnd, c.dayCount, c.field, c.observed, c.hasObserved); err != nil {
		return fmt.Errorf("stated rates insert: %w", err)
	}
	return nil
}

// statedRateColumns are the stated rows of account_rates a column at a
// time. A nullable column is a list of values and a list of whether each
// is set; a date or a day count is text, "" where it is unset.
type statedRateColumns struct {
	src, acct, ccy, side, validFrom, termEnd, dayCount, field []string
	hasCcy, hasUpTo, hasObserved                              []bool
	tier, observed                                            []int64
	pct, upTo                                                 []float64
}

// add appends tier i of rate, which the payload of src's acct observed
// at observed states in ccy.
func (c *statedRateColumns) add(src, acct string, ccy sql.NullString, observed sql.NullInt64,
	rate canonical.StatedRate, i int, t canonical.StatedTier) {
	number := i + 1
	if t.Number != 0 {
		number = t.Number
	}
	day := func(d *time.Time) string {
		if d == nil {
			return ""
		}
		return d.Format(time.DateOnly)
	}
	var upTo float64
	if t.UpTo != nil {
		upTo = *t.UpTo
	}
	c.src, c.acct = append(c.src, src), append(c.acct, acct)
	c.ccy, c.hasCcy = append(c.ccy, ccy.String), append(c.hasCcy, ccy.Valid)
	c.side, c.tier, c.pct = append(c.side, rate.Side), append(c.tier, int64(number)), append(c.pct, t.Pct)
	c.upTo, c.hasUpTo = append(c.upTo, upTo), append(c.hasUpTo, t.UpTo != nil)
	c.validFrom, c.termEnd = append(c.validFrom, day(rate.ValidFrom)), append(c.termEnd, day(rate.TermEnd))
	c.dayCount, c.field = append(c.dayCount, string(rate.DayCount)), append(c.field, rate.Field)
	c.observed, c.hasObserved = append(c.observed, observed.Int64), append(c.hasObserved, observed.Valid)
}

// statedRatesInsertSQL inserts the stated rows of statedRateColumns, its
// lists bound in the order of its fields, one row per position.
const statedRatesInsertSQL = `
INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, up_to, valid_from,
                           term_end, day_count, origin, field, observed_at)
SELECT src, acct, CASE WHEN has_ccy THEN ccy END, side, tier, pct, CASE WHEN has_up_to THEN up_to END,
       CAST(NULLIF(valid_from, '') AS DATE), CAST(NULLIF(term_end, '') AS DATE), NULLIF(day_count, ''), 'stated',
       field, CASE WHEN has_observed THEN observed END
  FROM (SELECT unnest($1::VARCHAR[]) AS src, unnest($2::VARCHAR[]) AS acct, unnest($3::VARCHAR[]) AS ccy,
               unnest($4::BOOLEAN[]) AS has_ccy, unnest($5::VARCHAR[]) AS side, unnest($6::BIGINT[]) AS tier,
               unnest($7::DOUBLE[]) AS pct, unnest($8::DOUBLE[]) AS up_to, unnest($9::BOOLEAN[]) AS has_up_to,
               unnest($10::VARCHAR[]) AS valid_from, unnest($11::VARCHAR[]) AS term_end,
               unnest($12::VARCHAR[]) AS day_count, unnest($13::VARCHAR[]) AS field,
               unnest($14::BIGINT[]) AS observed, unnest($15::BOOLEAN[]) AS has_observed)`

// writeAccountRates rewrites account_rates from the ledger's records
// and the rates the sources' payloads state. A card takes no rate
// (YIELD.md §3).
func writeAccountRates(ctx context.Context, tx *sql.Tx) error {
	if err := execAll(ctx, tx, accountRatesSQL...); err != nil {
		return err
	}
	if err := writeStatedRates(ctx, tx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM account_rates r USING accounts a
 WHERE a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id
   AND a.account_kind = ?`, string(canonical.AccountKindCard))
	return err
}
