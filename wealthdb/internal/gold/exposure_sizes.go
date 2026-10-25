package gold

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// marketCapSQL completes a US filer's market cap where no source states
// one: the shares outstanding EDGAR states, times the newest US-dollar
// price of the stock: a position held directly (value over quantity), or
// an equity line of a fund whose values are in US dollars. A depositary
// receipt is left out, as a lookup, a link to its share or its own name
// (the parameter is receiptWording) calls it: its price is the receipt's,
// its shares the issuer's. A share count below minListedShares is a
// filing's placeholder, not a count.
const marketCapSQL = `
UPDATE securities SET market_cap = securities.shares_outstanding * p.price, market_cap_origin = 'shares_x_price'
  FROM (
    WITH keyed AS (
        SELECT silver_source_id, instrument_external_id, exposure_key(security_key) AS k FROM instrument_keys),
    priced AS (
        SELECT k.k, p.market_value::DOUBLE / p.quantity::DOUBLE AS price, p.snapshot_at AS seen_at
          FROM positions p JOIN keyed k USING (silver_source_id, instrument_external_id)
         WHERE p.vehicle = 'stock' AND upper(p.currency) = 'USD' AND p.quantity > 0 AND p.market_value > 0
        UNION ALL
        SELECT exposure_key(l.security_key), l.market_value::DOUBLE / l.quantity::DOUBLE, l.as_of
          FROM fund_lines l
          JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
          LEFT JOIN fund_line_labels la ON la.provider = c.provider AND la.field = 'asset_type' AND la.raw = l.asset_type
         WHERE l.security_key IS NOT NULL AND COALESCE(la.value, 'public_equity') = 'public_equity'
           AND upper(c.currency) = 'USD' AND upper(l.currency) = 'USD' AND l.quantity > 0 AND l.market_value > 0),
    receipts AS (
        SELECT security_key AS k FROM security_underlyings
        UNION
        SELECT exposure_key(i.security_key) FROM security_identities i
         WHERE i.security_type = '` + receiptType + `' OR regexp_matches(upper(COALESCE(i.name, '')), $1)
        UNION
        SELECT k.k FROM keyed k JOIN instruments ins USING (silver_source_id, instrument_external_id)
         WHERE regexp_matches(upper(COALESCE(ins.name, '')), $1))
    SELECT k, price FROM priced
     WHERE k NOT IN (SELECT k FROM receipts)
   QUALIFY ROW_NUMBER() OVER (PARTITION BY k ORDER BY seen_at DESC, price) = 1) p
 WHERE p.k = securities.security_key AND securities.market_cap IS NULL AND securities.shares_outstanding >= $2`

// minListedShares is the fewest shares a listed company has outstanding;
// a filing that states fewer states no count.
const minListedShares = 10000

// sizeByCapSQL sizes each security whose market cap is known.
const sizeByCapSQL = `
UPDATE securities SET size = CASE WHEN market_cap >= $1 THEN $3 WHEN market_cap >= $2 THEN $4 ELSE $5 END,
       size_origin = 'market_cap'
 WHERE market_cap > 0`

// sizeRanksSQL ranks every stock of every size fund's newest
// composition: among the fund's stocks of the stock's country (the
// fund's own label for it first), by falling weight, the share of that
// country's weight the stocks before it hold, scaled by the share of the
// market the fund covers, read as a size. A stock on two lines under
// one key counts once, its weights summed; two lines that name one share
// by name alone rank apart (sizeByLineSQL). peers is how many stocks of
// its country the fund lists, and market_pct the share of the market
// before the stock.
const sizeRanksSQL = `
CREATE OR REPLACE TEMP TABLE exposure_size_ranks AS
WITH f AS (
    SELECT c.*, sf.coverage_pct
      FROM exposure_comps() c
      JOIN (SELECT exposure_key(fund_ref) AS fund_ref, MAX(coverage_pct) AS coverage_pct
              FROM exposure_size_funds GROUP BY 1) sf USING (fund_ref)
   QUALIFY ROW_NUMBER() OVER (PARTITION BY c.fund_ref ORDER BY c.as_of DESC) = 1),
lines AS (
    SELECT f.fund_ref, f.coverage_pct, l.key AS k, SUM(l.weight)::DOUBLE AS w,
           MIN(COALESCE(lc.value, sec.risk_country, sec.domicile)) AS country
      FROM f
      JOIN (SELECT *, exposure_key(security_key) AS key FROM fund_lines) l
        ON l.silver_source_id = f.silver_source_id AND l.fund_ref = f.stored_ref AND l.as_of = f.as_of AND l.basis = f.basis
      LEFT JOIN fund_line_labels la ON la.provider = f.provider AND la.field = 'asset_type' AND la.raw = l.asset_type
      LEFT JOIN fund_line_labels lc ON lc.provider = f.provider AND lc.field = 'country'
                                   AND lc.raw = COALESCE(l.risk_country, l.country)
      LEFT JOIN securities sec ON sec.security_key = l.key
     WHERE COALESCE(la.value, 'public_equity') = 'public_equity' AND l.security_key IS NOT NULL
     GROUP BY 1, 2, 3
    HAVING SUM(l.weight) > 0),
ranked AS (
    SELECT *,
           100 * COALESCE(SUM(w) OVER (PARTITION BY fund_ref, country ORDER BY w DESC, k
                                       ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0)
               / SUM(w) OVER (PARTITION BY fund_ref, country) AS before_pct,
           COUNT(*) OVER (PARTITION BY fund_ref, country) AS peers
      FROM lines WHERE country IS NOT NULL)
SELECT k, fund_ref, coverage_pct, peers, market_pct,
       CASE WHEN market_pct < $1 THEN $3 WHEN market_pct < $2 THEN $4 ELSE $5 END AS size
  FROM (SELECT *, before_pct * coverage_pct / 100 AS market_pct FROM ranked)`

// sizeByRankSQL sizes each stock no market cap sizes and an identifier
// names by its rank in a size fund. A stock in several takes the one
// that covers most of its market, then the one that lists most stocks of
// its country. A line named by name alone takes no rank of its own: its
// share's size reaches it (sizeFillSQL).
const sizeByRankSQL = `
UPDATE securities SET size = r.size, size_origin = 'size_fund:' || r.fund_ref
  FROM (SELECT * FROM exposure_size_ranks WHERE k NOT LIKE 'name:%'
       QUALIFY ROW_NUMBER() OVER (PARTITION BY k ORDER BY coverage_pct DESC, peers DESC, fund_ref) = 1) r
 WHERE r.k = securities.security_key AND securities.size IS NULL`

// sizeByLineSQL sizes a share no market cap and no rank sizes by the rank
// of a size fund's line that names it by name alone (security_underlyings,
// a match by name): a fund file that states no identifier, such as a US
// issuer's file of foreign stocks, still ranks its stocks. Of several
// such lines, the one whose fund covers most of its market, then the
// one that lists most stocks of its country. Of two lines of one fund,
// the heavier wins: it ranks first, so its size is the larger. The
// line's key breaks a tie, so the choice never rests on storage order.
// The two lines' weights are not summed, so the share can rank lower
// than one line of their combined weight would. The origin says the
// size rests on a name.
const sizeByLineSQL = `
UPDATE securities SET size = n.size, size_origin = 'size_fund_name:' || n.fund_ref
  FROM (SELECT x.underlying_key AS k, r.size, r.fund_ref
          FROM security_underlyings x JOIN exposure_size_ranks r ON r.k = x.security_key
         WHERE x.security_key LIKE 'name:%'
       QUALIFY ROW_NUMBER() OVER (PARTITION BY x.underlying_key
                                  ORDER BY r.coverage_pct DESC, r.peers DESC, r.fund_ref,
                                           r.market_pct, x.security_key) = 1) n
 WHERE n.k = securities.security_key AND securities.size IS NULL`

// sizeFillSQL sizes a depositary receipt, and a line a report names by
// name alone, as the share it stands for (security_underlyings): the
// issuer's size. The origin names the share as receiptFillSQL's do.
const sizeFillSQL = `
UPDATE securities SET size = u.size, size_origin = x.how || u.security_key
  FROM ` + underlyingsHowSQL + ` x
  JOIN securities u ON u.security_key = x.underlying_key
 WHERE x.security_key = securities.security_key AND securities.size IS NULL AND u.size IS NOT NULL`

// writeSizes completes each security's market cap where EDGAR's shares
// and a price give it, then sizes each stock: by its market cap where
// known, else by its rank in a size fund (canonical/size.go), else as a
// size fund's line that names it; a receipt or a line named by name
// alone as its share.
func writeSizes(ctx context.Context, tx *sql.Tx) error {
	buckets := []any{string(canonical.SizeLarge), string(canonical.SizeMid), string(canonical.SizeSmall)}
	for _, s := range []struct {
		q    string
		args []any
	}{
		{marketCapSQL, []any{receiptWording.String(), minListedShares}},
		{sizeByCapSQL, append([]any{canonical.LargeCapUSD, canonical.MidCapUSD}, buckets...)},
		{sizeRanksSQL, append([]any{canonical.LargeCumPct, canonical.MidCumPct}, buckets...)},
		{sizeByRankSQL, nil},
		{sizeByLineSQL, nil},
		{sizeFillSQL, nil},
		{`DROP TABLE exposure_size_ranks`, nil},
	} {
		if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
			return err
		}
	}
	return nil
}
