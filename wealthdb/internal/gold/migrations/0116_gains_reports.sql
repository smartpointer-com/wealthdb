-- ============================================================
-- gold schema, migration 0116 — the gains reports, and the cost basis
-- on the positions report.
--
-- No table changes: every object here is a macro over what 0115
-- loads. docs/GAINS.md defines each figure; DESIGN.md §10.13 lists
-- the macros.
--
-- Shared pieces, used by every report below:
--
--   fx_rates_to(ccy)            the rate into ccy per currency and day
--   fx_amount(amt, ccy, tgt, r) one conversion at such a rate
--   positions_at(instants)      the position lines as of each instant,
--                               under the point-in-time rule of
--                               `wealthdb holdings` (§10.1)
--   gains_position_lines        positions_at with clean value,
--                               unrealized gain and FX
--   realized_lots_in            the realized lots of a window, with the
--                               one realized-gain formula and FX
--   gains_events                the window's sells, each marked whether
--                               a document covers it, and the in-kind
--                               moves and corporate actions the identity
--                               of docs/GAINS.md §2 cannot see through
--
-- report_positions and report_cash are re-issued over the first three,
-- with the cost basis columns; their earlier columns keep their names,
-- order and values.
--
-- Money columns come out as VARCHAR decimals like the other report
-- macros; ratios as DOUBLE. CREATE OR REPLACE keeps this replayable
-- (see gold.Migrate's REPLAY note).
-- ============================================================

-- fx_rates_to: the rate from every currency fx_daily knows into p_ccy,
-- on every day fx_daily holds any rate: direct, else through CHF, else
-- through USD, the order every single-currency report applies. Because
-- the grid holds every day on which any rate changes, an at-or-before
-- join on (currency, day) finds the rates the per-leg joins find.
CREATE OR REPLACE MACRO fx_rates_to(p_ccy) AS TABLE (
    WITH grid AS (
        SELECT c.from_ccy, d.day
          FROM (SELECT DISTINCT from_ccy FROM fx_daily) c,
               (SELECT DISTINCT day FROM fx_daily) d)
    SELECT g.from_ccy, g.day, COALESCE(x.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
      FROM grid g
      ASOF LEFT JOIN fx_daily x  ON x.from_ccy  = g.from_ccy AND x.to_ccy  = p_ccy AND x.day  <= g.day
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.from_ccy AND c1.to_ccy = 'CHF' AND c1.day <= g.day
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= g.day
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.from_ccy AND u1.to_ccy = 'USD' AND u1.day <= g.day
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= g.day
);

-- fx_amount: p_amt in p_ccy converted into p_tgt at p_rate, a rate
-- from fx_rates_to(p_tgt); the identity when the currencies agree.
CREATE OR REPLACE MACRO fx_amount(p_amt, p_ccy, p_tgt, p_rate) AS
    CASE WHEN p_ccy = p_tgt THEN p_amt::DOUBLE ELSE p_amt::DOUBLE * p_rate END;

-- positions_at: the position lines as of each instant in p_instants, one
-- row per (instant, line). Each source contributes its latest snapshot
-- at or before the instant. The account, its portfolio bucket (''
-- outside any registered portfolio, as portfolio_acct_map has it) and
-- the instrument's symbol come along.
CREATE OR REPLACE MACRO positions_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    snaps AS (SELECT DISTINCT silver_source_id, snapshot_at FROM positions),
    pick AS (
        SELECT b.instant, s.silver_source_id, MAX(s.snapshot_at) AS snapshot_at
          FROM b JOIN snaps s ON s.snapshot_at <= b.instant
         GROUP BY b.instant, s.silver_source_id)
    SELECT pk.instant, p.silver_source_id, p.snapshot_at, p.account_external_id,
           a.display_name, a.relationship_id, a.nickname, a.account_category,
           a.account_kind, a.tax_wrapper, m.pid AS portfolio_bucket,
           p.position_key, p.instrument_external_id,
           COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
           p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value,
           p.book_value, p.accrued_interest, p.acquisition_date,
           p.basis_origin, p.basis_method, p.basis_fees
      FROM positions p
      JOIN pick pk ON pk.silver_source_id = p.silver_source_id AND pk.snapshot_at = p.snapshot_at
      LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id
      LEFT JOIN portfolio_acct_map() m ON m.src = p.silver_source_id AND m.acct = p.account_external_id
      LEFT JOIN instruments i ON i.silver_source_id = p.silver_source_id AND i.instrument_external_id = p.instrument_external_id
      LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
           AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id
);

-- gains_position_lines: positions_at with the figures every gains
-- reader takes from a line. The clean value leaves out the accrued
-- income market_value carries (DESIGN.md §7.1); the unrealized gain is
-- the clean value less the cost basis, NULL where the source states
-- none or where no cost basis describes the line (below). Every amount
-- converts at the snapshot day's rate, so an
-- unrealized gain in the output currency carries no FX of its own.
-- `stamp` is the basis stamp as one token, origin/method/fees.
-- `basis_applies` is false for a line no cost basis describes: cash
-- held as a position, a mortgage, and an FX forward, whose value is
-- itself the gain. Such a line has no unrealized gain, even where the
-- source states a book value (a mortgage's principal), and coverage
-- counts it neither way.
CREATE OR REPLACE MACRO gains_position_lines(p_instants, p_ccy) AS TABLE (
    WITH a AS (
        SELECT p.*,
               p.asset_class <> 'cash' AND COALESCE(p.vehicle, '') NOT IN ('mortgage', 'forward') AS basis_applies
          FROM positions_at(p_instants) p),
    l AS (
        SELECT a.*,
               a.market_value - COALESCE(a.accrued_interest, 0) AS clean_value,
               CASE WHEN a.basis_applies
                    THEN a.market_value - COALESCE(a.accrued_interest, 0) - a.book_value END AS unrealized,
               a.basis_origin || '/' || a.basis_method || '/' || a.basis_fees AS stamp
          FROM a)
    SELECT l.*,
           fx_amount(l.market_value, l.currency, p_ccy, fx.rate) AS value_x,
           fx_amount(l.book_value, l.currency, p_ccy, fx.rate) AS book_x,
           fx_amount(l.unrealized, l.currency, p_ccy, fx.rate) AS unrealized_x,
           l.currency <> p_ccy AND fx.rate IS NULL AS fx_missing
      FROM l
      ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = l.currency AND fx.day <= (l.snapshot_at // 86400)
);

-- report_positions: every position line as of p_asof, value in p_ccy,
-- with its cost basis, clean value and unrealized gain.
CREATE OR REPLACE MACRO report_positions(p_asof, p_ccy) AS TABLE (
    SELECT silver_source_id, snapshot_at, account_external_id, display_name,
           relationship_id, nickname, account_category, position_key,
           instrument_external_id, symbol, name, asset_class, vehicle, currency,
           CAST(quantity AS VARCHAR) AS quantity, CAST(market_value AS VARCHAR) AS market_value,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(book_value AS VARCHAR) AS book_value,
           CAST(book_x::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(accrued_interest AS VARCHAR) AS accrued_interest,
           CAST(clean_value AS VARCHAR) AS clean_value,
           CAST(unrealized AS VARCHAR) AS unrealized_gain,
           CAST(unrealized_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_outccy,
           unrealized::DOUBLE / NULLIF(book_value::DOUBLE, 0) AS unrealized_ratio,
           stamp AS basis_stamp,
           CAST(acquisition_date AS VARCHAR) AS acquisition_date
      FROM gains_position_lines([p_asof], p_ccy)
     ORDER BY silver_source_id, account_external_id, position_key
);

-- report_cash: one line per account and currency with a non-zero cash
-- balance, in report_positions' shape; cash has no cost basis, so those
-- columns are NULL.
CREATE OR REPLACE MACRO report_cash(p_asof, p_ccy) AS TABLE (
    SELECT cc.silver_source_id, cc.snapshot_at, cc.account_external_id, a.display_name,
           a.relationship_id, a.nickname, a.account_category,
           'cash:' || cc.currency AS position_key, CAST(NULL AS VARCHAR) AS instrument_external_id,
           cc.currency AS symbol, 'Cash ' || cc.currency AS name,
           'cash' AS asset_class, 'demand_deposit' AS vehicle, cc.currency,
           CAST(NULL AS VARCHAR) AS quantity, CAST(cc.amount AS VARCHAR) AS market_value,
           CAST(fx_amount(cc.amount, cc.currency, p_ccy, fx.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(NULL AS VARCHAR) AS book_value, CAST(NULL AS VARCHAR) AS book_value_outccy,
           CAST(NULL AS VARCHAR) AS accrued_interest, CAST(NULL AS VARCHAR) AS clean_value,
           CAST(NULL AS VARCHAR) AS unrealized_gain, CAST(NULL AS VARCHAR) AS unrealized_outccy,
           CAST(NULL AS DOUBLE) AS unrealized_ratio, CAST(NULL AS VARCHAR) AS basis_stamp,
           CAST(NULL AS VARCHAR) AS acquisition_date
      FROM cash_chosen(p_asof) cc
      LEFT JOIN accounts a ON cc.silver_source_id = a.silver_source_id AND cc.account_external_id = a.account_external_id
      ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = cc.currency AND fx.day <= (cc.snapshot_at // 86400)
     ORDER BY cc.silver_source_id, cc.account_external_id, cc.currency
);

-- realized_lots_in: the realized lots whose effective date falls in
-- [p_from, p_to]: the primary set, or every copy with p_all. The
-- effective date is the disposal date, else the settlement date, else
-- the last day of the tax year (`undated`). The gain is the document's,
-- else proceeds − cost basis + wash sale disallowed, else NULL; this is
-- the one place that formula lives. Amounts convert at the effective
-- date's rate.
CREATE OR REPLACE MACRO realized_lots_in(p_from, p_to, p_ccy, p_all) AS TABLE (
    WITH r AS (
        SELECT r.* EXCLUDE (payload),
               COALESCE(r.disposal_date, r.settlement_date, make_date(r.tax_year, 12, 31)) AS effective_date,
               r.disposal_date IS NULL AND r.settlement_date IS NULL AS undated,
               COALESCE(r.realized_gain_loss,
                        r.proceeds - r.book_value + COALESCE(r.wash_sale_disallowed, 0)) AS gain,
               CASE WHEN r.realized_gain_loss IS NOT NULL THEN 'stated'
                    WHEN r.proceeds IS NOT NULL AND r.book_value IS NOT NULL THEN 'derived'
                    ELSE 'unknown' END AS gain_origin
          FROM realized_lots r
         WHERE p_all OR r.is_primary),
    w AS (SELECT *, CAST(epoch(effective_date) AS BIGINT) AS effective_at FROM r)
    SELECT w.*, m.pid AS portfolio_bucket,
           fx_amount(w.proceeds, w.currency, p_ccy, fx.rate) AS proceeds_x,
           fx_amount(w.book_value, w.currency, p_ccy, fx.rate) AS book_x,
           fx_amount(w.gain, w.currency, p_ccy, fx.rate) AS gain_x,
           fx_amount(w.wash_sale_disallowed, w.currency, p_ccy, fx.rate) AS wash_x,
           w.currency <> p_ccy AND fx.rate IS NULL AS fx_missing
      FROM w
      LEFT JOIN portfolio_acct_map() m ON m.src = w.silver_source_id AND m.acct = w.account_external_id
      ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = w.currency AND fx.day <= (w.effective_at // 86400)
     WHERE w.effective_at BETWEEN p_from AND p_to
);

-- gains_events: the transactions in [p_from, p_to] that move a holding
-- other than by a purchase, one row each, `event` saying which:
--   sell              a sale. It is documented when a primary realized
--                     lot of its calendar year sits on its account or on
--                     another account of the same registered portfolio:
--                     a bank can book the sale on the portfolio's cash
--                     account and print the lots against its custody
--                     account.
--   in_kind           a security moved in or out (a transfer or journal
--                     naming an instrument and a quantity): the holding
--                     arrives or leaves with its whole unrealized gain.
--   corporate_action  a merger, split or spin-off: one holding becomes
--                     another with no realized lot.
CREATE OR REPLACE MACRO gains_events(p_from, p_to) AS TABLE (
    WITH m AS (SELECT * FROM portfolio_acct_map()),
    docs AS (
        SELECT DISTINCT r.silver_source_id AS src, r.account_external_id AS acct, rm.pid, r.tax_year
          FROM realized_lots r LEFT JOIN m rm ON rm.src = r.silver_source_id AND rm.acct = r.account_external_id
         WHERE r.is_primary)
    SELECT t.silver_source_id, t.account_external_id, t.instrument_external_id, t.occurred_at,
           tm.pid AS portfolio_bucket,
           CASE t.kind WHEN 'sell' THEN 'sell' WHEN 'corporate_action' THEN 'corporate_action'
                ELSE 'in_kind' END AS event,
           t.kind = 'sell' AND EXISTS (
               SELECT 1 FROM docs d
                WHERE d.src = t.silver_source_id
                  AND d.tax_year = year(epoch_ms(t.occurred_at * 1000))
                  AND (d.acct = t.account_external_id OR (tm.pid <> '' AND d.pid = tm.pid))) AS documented
      FROM transactions t
      LEFT JOIN m tm ON tm.src = t.silver_source_id AND tm.acct = t.account_external_id
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND (t.kind = 'sell'
            OR (t.kind = 'corporate_action' AND t.instrument_external_id IS NOT NULL)
            OR (t.kind IN ('transfer_in', 'transfer_out', 'journal')
                AND t.instrument_external_id IS NOT NULL AND t.quantity IS NOT NULL))
);

-- report_gains_realized: the realized lots of the window, one row per
-- lot, oldest first.
CREATE OR REPLACE MACRO report_gains_realized(p_from, p_to, p_ccy, p_all) AS TABLE (
    SELECT r.silver_source_id, r.account_external_id, a.display_name, a.nickname,
           CAST(r.effective_date AS VARCHAR) AS effective_date, r.undated,
           r.instrument_external_id, r.instrument_hint,
           COALESCE(i.symbol, sri.symbol) AS symbol, r.description,
           CAST(r.quantity AS VARCHAR) AS quantity,
           CAST(r.acquisition_date AS VARCHAR) AS acquisition_date, r.acquired_various,
           date_diff('day', r.acquisition_date, r.effective_date) AS held_days,
           r.term, r.covered, r.form_8949_box, r.currency,
           CAST(r.proceeds AS VARCHAR) AS proceeds, CAST(r.book_value AS VARCHAR) AS book_value,
           CAST(r.gain AS VARCHAR) AS gain,
           CAST(r.wash_sale_disallowed AS VARCHAR) AS wash_sale_disallowed,
           CAST(r.accrued_market_discount AS VARCHAR) AS accrued_market_discount,
           r.gain_origin,
           CAST(r.proceeds_x::DECIMAL(28,4) AS VARCHAR) AS proceeds_outccy,
           CAST(r.book_x::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(r.gain_x::DECIMAL(28,4) AS VARCHAR) AS gain_outccy,
           r.document_kind, r.tax_year, r.is_primary,
           r.basis_origin || '/' || r.basis_method || '/' || r.basis_fees AS basis_stamp,
           r.realized_lot_external_id, r.source_document
      FROM realized_lots_in(p_from, p_to, p_ccy, p_all) r
      LEFT JOIN accounts a ON a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id
      LEFT JOIN instruments i ON i.silver_source_id = r.silver_source_id AND i.instrument_external_id = r.instrument_external_id
      LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = r.silver_source_id
           AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = r.instrument_external_id
     ORDER BY r.effective_at, r.silver_source_id, r.account_external_id, r.realized_lot_external_id
);

-- report_lots: the open lots of the positions held at p_asof. A lot's
-- value is the one its source states, else its position's value pro
-- rata by quantity; a pro-rata value carries its share of the
-- position's accrued income, which the unrealized gain leaves out.
CREATE OR REPLACE MACRO report_lots(p_asof, p_ccy) AS TABLE (
    WITH v AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id, p.display_name, p.nickname,
               p.position_key, p.symbol, p.name, p.currency, l.lot_key,
               l.acquisition_date, l.term, l.covered, l.quantity, l.book_value, l.basis_origin,
               l.source_document,
               CASE WHEN l.market_value IS NOT NULL THEN 'stated'
                    WHEN p.market_value IS NOT NULL AND p.quantity <> 0 THEN 'pro_rata' END AS value_origin,
               COALESCE(l.market_value::DOUBLE,
                        p.market_value::DOUBLE * l.quantity::DOUBLE / NULLIF(p.quantity::DOUBLE, 0)) AS lot_value,
               CASE WHEN l.market_value IS NOT NULL THEN l.market_value::DOUBLE
                    ELSE (p.market_value::DOUBLE - COALESCE(p.accrued_interest::DOUBLE, 0))
                         * l.quantity::DOUBLE / NULLIF(p.quantity::DOUBLE, 0) END AS lot_clean,
               fx.rate
          FROM position_lots l
          JOIN positions_at([p_asof]) p
            ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snapshot_at
           AND p.account_external_id = l.account_external_id AND p.position_key = l.position_key
          ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = p.currency AND fx.day <= (p.snapshot_at // 86400))
    SELECT silver_source_id, snapshot_at, account_external_id, display_name, nickname,
           position_key, symbol, name, lot_key,
           CAST(acquisition_date AS VARCHAR) AS acquisition_date,
           date_diff('day', acquisition_date, CAST(epoch_ms(CAST(p_asof AS BIGINT) * 1000) AS DATE)) AS held_days,
           term, covered, CAST(quantity AS VARCHAR) AS quantity, currency,
           CAST(book_value AS VARCHAR) AS book_value,
           CAST(lot_value::DECIMAL(28,4) AS VARCHAR) AS market_value, value_origin,
           CAST((lot_clean - book_value::DOUBLE)::DECIMAL(28,4) AS VARCHAR) AS unrealized_gain,
           (lot_clean - book_value::DOUBLE) / NULLIF(book_value::DOUBLE, 0) AS unrealized_ratio,
           CAST(fx_amount(book_value, currency, p_ccy, rate)::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(fx_amount(lot_value, currency, p_ccy, rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(fx_amount(lot_clean - book_value::DOUBLE, currency, p_ccy, rate)::DECIMAL(28,4) AS VARCHAR) AS unrealized_outccy,
           basis_origin, source_document
      FROM v
     ORDER BY silver_source_id, account_external_id, position_key, acquisition_date, lot_key
);

-- report_gains_buckets: realized and unrealized gains per period bucket
-- of [p_from, p_to], at one grain: 'all' (one row per bucket),
-- 'sources', 'portfolios' (the holdings buckets, '' for a source's
-- accounts outside any registered portfolio) or 'accounts'. Every grain
-- is the same sums grouped differently, so the grains reconcile.
--
-- A bucket's start value is the holdings as of the second before it
-- opens, its end value the holdings as of its last second. The window
-- opens no earlier than the first data gold holds, so an open start
-- does not produce empty buckets back to 1970.
CREATE OR REPLACE MACRO report_gains_buckets(p_from, p_to, p_ccy, p_period, p_grain) AS TABLE (
    WITH first_data AS (
        SELECT LEAST((SELECT MIN(snapshot_at) FROM positions),
                     (SELECT MIN(CAST(epoch(COALESCE(disposal_date, settlement_date,
                                                     make_date(tax_year, 12, 31))) AS BIGINT))
                        FROM realized_lots)) AS f),
    w AS (SELECT CAST(GREATEST(p_from, COALESCE(f, p_from)) AS BIGINT) AS f0,
                 CASE WHEN p_period = 'total' THEN 'year' ELSE p_period END AS unit
            FROM first_data),
    starts AS (
        SELECT f0 AS b_from FROM w
        UNION
        SELECT CAST(epoch(x) AS BIGINT)
          FROM (SELECT f0, unnest(generate_series(date_trunc(unit, epoch_ms(f0 * 1000)),
                                                   epoch_ms(CAST(p_to AS BIGINT) * 1000),
                                                   CAST('1 ' || unit AS INTERVAL))) AS x FROM w)
         WHERE p_period <> 'total' AND CAST(epoch(x) AS BIGINT) > f0),
    bk AS (
        SELECT b_from, COALESCE(LEAD(b_from) OVER (ORDER BY b_from) - 1, p_to) AS b_to
          FROM starts WHERE b_from <= p_to),
    instants AS (SELECT list(DISTINCT t) AS l FROM (SELECT b_from - 1 AS t FROM bk UNION SELECT b_to FROM bk)),
    lines AS (SELECT * FROM gains_position_lines((SELECT l FROM instants), p_ccy)),
    src_first AS (SELECT silver_source_id, MIN(snapshot_at) AS first_at FROM positions GROUP BY 1),
    -- The grain key: constant '' where the grain does not split.
    keyed_lines AS (
        SELECT l.*, bk.b_from, bk.b_to, l.instant = bk.b_to AS at_end,
               CASE WHEN p_grain = 'all' THEN '' ELSE l.silver_source_id END AS g_src,
               CASE WHEN p_grain = 'portfolios' THEN l.portfolio_bucket ELSE '' END AS g_pid,
               CASE WHEN p_grain = 'accounts' THEN l.account_external_id ELSE '' END AS g_acct,
               sf.first_at
          FROM lines l
          JOIN bk ON l.instant = bk.b_to OR l.instant = bk.b_from - 1
          JOIN src_first sf ON sf.silver_source_id = l.silver_source_id),
    ends AS (
        SELECT b_from, g_src, g_pid, g_acct,
               COUNT(*) AS n_positions,
               COUNT(*) FILTER (WHERE basis_applies AND book_value IS NULL) AS n_without_basis,
               SUM(abs(value_x)) FILTER (WHERE basis_applies) AS abs_value_x,
               SUM(abs(value_x)) FILTER (WHERE basis_applies AND book_value IS NOT NULL) AS abs_basis_value_x,
               SUM(unrealized_x) AS unrealized_end_x,
               COUNT(*) FILTER (WHERE fx_missing) AS n_fx_missing,
               bool_or(basis_method = 'paid_in') AS paid_in,
               list_sort(list_distinct(list(silver_source_id) FILTER (WHERE first_at BETWEEN b_from AND b_to))) AS onboarded
          FROM keyed_lines WHERE at_end GROUP BY ALL),
    starts_agg AS (
        SELECT b_from, g_src, g_pid, g_acct,
               SUM(unrealized_x) AS unrealized_start_x,
               COUNT(*) FILTER (WHERE fx_missing) AS n_fx_missing
          FROM keyed_lines WHERE NOT at_end GROUP BY ALL),
    lots AS (
        SELECT bk.b_from,
               CASE WHEN p_grain = 'all' THEN '' ELSE r.silver_source_id END AS g_src,
               CASE WHEN p_grain = 'portfolios' THEN COALESCE(r.portfolio_bucket, '') ELSE '' END AS g_pid,
               CASE WHEN p_grain = 'accounts' THEN r.account_external_id ELSE '' END AS g_acct,
               SUM(r.gain_x) AS realized_x,
               SUM(r.gain_x) FILTER (WHERE r.term = 'short') AS realized_short_x,
               SUM(r.gain_x) FILTER (WHERE r.term = 'long') AS realized_long_x,
               SUM(r.gain_x) FILTER (WHERE r.term IS NULL) AS realized_other_x,
               SUM(r.proceeds_x) AS proceeds_x,
               SUM(r.wash_x) AS wash_x,
               COUNT(*) AS n_lots,
               COUNT(*) FILTER (WHERE r.gain IS NULL) AS n_without_gain,
               COUNT(*) FILTER (WHERE r.undated) AS n_undated,
               COUNT(*) FILTER (WHERE r.fx_missing) AS n_fx_missing
          FROM realized_lots_in((SELECT MIN(b_from) FROM bk), p_to, p_ccy, FALSE) r
          JOIN bk ON r.effective_at BETWEEN bk.b_from AND bk.b_to
         GROUP BY ALL),
    sells AS (
        SELECT bk.b_from,
               CASE WHEN p_grain = 'all' THEN '' ELSE s.silver_source_id END AS g_src,
               CASE WHEN p_grain = 'portfolios' THEN COALESCE(s.portfolio_bucket, '') ELSE '' END AS g_pid,
               CASE WHEN p_grain = 'accounts' THEN s.account_external_id ELSE '' END AS g_acct,
               COUNT(*) FILTER (WHERE s.event = 'sell') AS n_sells,
               COUNT(*) FILTER (WHERE s.event = 'sell' AND NOT s.documented) AS n_undocumented,
               COUNT(*) FILTER (WHERE s.event = 'in_kind') AS n_in_kind,
               COUNT(*) FILTER (WHERE s.event = 'corporate_action') AS n_corporate
          FROM gains_events((SELECT MIN(b_from) FROM bk), p_to) s
          JOIN bk ON s.occurred_at BETWEEN bk.b_from AND bk.b_to
         GROUP BY ALL),
    keys AS (
        SELECT b_from, g_src, g_pid, g_acct FROM ends
        UNION SELECT b_from, g_src, g_pid, g_acct FROM starts_agg
        UNION SELECT b_from, g_src, g_pid, g_acct FROM lots
        UNION SELECT b_from, g_src, g_pid, g_acct FROM sells),
    m AS (
        SELECT b_from, b_to, g_src, g_pid, g_acct,
               e.n_positions, e.n_without_basis, e.abs_value_x, e.abs_basis_value_x, e.unrealized_end_x,
               e.paid_in, e.onboarded,
               s.unrealized_start_x,
               l.realized_x, l.realized_short_x, l.realized_long_x, l.realized_other_x,
               l.proceeds_x, l.wash_x, l.n_lots, l.n_without_gain, l.n_undated,
               se.n_sells, se.n_undocumented, se.n_in_kind, se.n_corporate,
               COALESCE(e.n_fx_missing, 0) + COALESCE(s.n_fx_missing, 0) + COALESCE(l.n_fx_missing, 0) AS n_fx_missing,
               CASE WHEN s.unrealized_start_x IS NULL AND e.unrealized_end_x IS NULL THEN NULL
                    ELSE COALESCE(e.unrealized_end_x, 0) - COALESCE(s.unrealized_start_x, 0) END AS unrealized_change_x
          FROM keys k
          LEFT JOIN ends e USING (b_from, g_src, g_pid, g_acct)
          LEFT JOIN starts_agg s USING (b_from, g_src, g_pid, g_acct)
          LEFT JOIN lots l USING (b_from, g_src, g_pid, g_acct)
          LEFT JOIN sells se USING (b_from, g_src, g_pid, g_acct)
          JOIN bk USING (b_from))
    SELECT CASE WHEN p_period = 'total' THEN NULL ELSE m.b_from END AS period_start,
           NULLIF(m.g_src, '') AS silver_source_id,
           CASE WHEN p_grain = 'portfolios' THEN m.g_pid END AS portfolio_external_id,
           pb.display_name AS portfolio_display_name,
           NULLIF(m.g_acct, '') AS account_external_id,
           a.display_name, a.nickname, a.account_kind, a.tax_wrapper, a.account_category, a.relationship_id,
           CAST(m.realized_x::DECIMAL(28,4) AS VARCHAR) AS realized,
           CAST(m.realized_short_x::DECIMAL(28,4) AS VARCHAR) AS realized_short,
           CAST(m.realized_long_x::DECIMAL(28,4) AS VARCHAR) AS realized_long,
           CAST(m.realized_other_x::DECIMAL(28,4) AS VARCHAR) AS realized_other,
           CAST(m.unrealized_start_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_start,
           CAST(m.unrealized_end_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_end,
           CAST(m.unrealized_change_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_change,
           CAST(CASE WHEN m.realized_x IS NULL AND m.unrealized_change_x IS NULL THEN NULL
                     ELSE COALESCE(m.realized_x, 0) + COALESCE(m.unrealized_change_x, 0) END::DECIMAL(28,4) AS VARCHAR) AS gain,
           CAST(m.proceeds_x::DECIMAL(28,4) AS VARCHAR) AS proceeds,
           CAST(m.wash_x::DECIMAL(28,4) AS VARCHAR) AS wash_disallowed,
           COALESCE(m.n_lots, 0) AS realized_lots,
           COALESCE(m.n_sells, 0) AS sells,
           COALESCE(m.n_positions, 0) AS positions,
           COALESCE(m.n_without_basis, 0) AS positions_without_basis,
           m.abs_basis_value_x / NULLIF(m.abs_value_x, 0) AS basis_coverage,
           concat_ws(';',
               CASE WHEN m.n_undocumented > 0 THEN 'sells_without_documents=' || m.n_undocumented END,
               CASE WHEN m.n_without_gain > 0 THEN 'lots_without_gain=' || m.n_without_gain END,
               CASE WHEN m.n_undated > 0 THEN 'undated_lots=' || m.n_undated END,
               CASE WHEN m.n_in_kind > 0 THEN 'in_kind_moves=' || m.n_in_kind END,
               CASE WHEN m.n_corporate > 0 THEN 'corporate_actions=' || m.n_corporate END,
               CASE WHEN m.paid_in THEN 'paid_in_basis' END,
               CASE WHEN len(m.onboarded) > 0 THEN 'onboarded_in_window=' || array_to_string(m.onboarded, ',') END,
               CASE WHEN m.n_fx_missing > 0 THEN 'fx_missing=' || m.n_fx_missing END) AS quality
      FROM m
      LEFT JOIN portfolio_buckets() pb ON p_grain = 'portfolios' AND pb.src = m.g_src AND pb.pid = m.g_pid
      LEFT JOIN accounts a ON p_grain = 'accounts' AND a.silver_source_id = m.g_src AND a.account_external_id = m.g_acct
     ORDER BY m.b_from, m.g_src, m.g_pid, m.g_acct
);

-- report_gains_positions: per account and instrument, the window's
-- realized and unrealized gains. A position line keys on its
-- instrument (else its position key) and a realized lot on its
-- instrument (else the instrument hint, else the description), so a
-- lot meets its position where the two name the same instrument.
-- `unmatched_lots` counts lots whose instrument the account never
-- held in any snapshot, the sign of an identity the two documents do
-- not share; a round trip inside the window is not one.
CREATE OR REPLACE MACRO report_gains_positions(p_from, p_to, p_ccy) AS TABLE (
    WITH lines AS (SELECT * FROM gains_position_lines([p_from - 1, p_to], p_ccy)),
    open_lots AS (
        SELECT silver_source_id, snapshot_at, account_external_id, position_key, COUNT(*) AS n
          FROM position_lots GROUP BY ALL),
    side AS (
        SELECT l.silver_source_id, l.account_external_id,
               COALESCE(l.instrument_external_id, l.position_key) AS k,
               l.instant = p_to AS at_end,
               min(l.position_key) AS position_key, any_value(l.symbol) AS symbol, any_value(l.name) AS name,
               any_value(l.asset_class) AS asset_class, any_value(l.vehicle) AS vehicle,
               any_value(l.currency) AS currency, any_value(l.display_name) AS display_name,
               any_value(l.nickname) AS nickname,
               SUM(l.quantity) AS quantity, SUM(l.market_value) AS market_value, SUM(l.value_x) AS value_x,
               CASE WHEN COUNT(l.book_value) = COUNT(*) THEN SUM(l.book_value) END AS book_value,
               CASE WHEN COUNT(l.book_value) = COUNT(*) THEN SUM(l.book_x) END AS book_x,
               CASE WHEN COUNT(l.book_value) = COUNT(*) THEN SUM(l.unrealized) END AS unrealized,
               CASE WHEN COUNT(l.book_value) = COUNT(*) THEN SUM(l.unrealized_x) END AS unrealized_x,
               string_agg(DISTINCT l.stamp, ',' ORDER BY l.stamp) AS stamp,
               min(l.acquisition_date) AS acquisition_date,
               bool_or(l.basis_method = 'paid_in') AS paid_in,
               COUNT(*) FILTER (WHERE l.fx_missing) AS n_fx_missing,
               COALESCE(SUM(ol.n), 0) AS open_lots
          FROM lines l
          LEFT JOIN open_lots ol ON ol.silver_source_id = l.silver_source_id AND ol.snapshot_at = l.snapshot_at
               AND ol.account_external_id = l.account_external_id AND ol.position_key = l.position_key
         GROUP BY ALL),
    held AS (SELECT DISTINCT silver_source_id, account_external_id, instrument_external_id FROM positions),
    lots AS (
        SELECT r.silver_source_id, r.account_external_id,
               COALESCE(r.instrument_external_id, r.instrument_hint, r.description) AS k,
               any_value(COALESCE(i.symbol, sri.symbol)) AS symbol, any_value(r.description) AS description,
               any_value(r.currency) AS currency,
               SUM(r.gain) AS realized, SUM(r.gain_x) AS realized_x,
               COUNT(*) FILTER (WHERE r.gain IS NULL) AS n_without_gain,
               COUNT(*) FILTER (WHERE r.undated) AS n_undated,
               COUNT(*) FILTER (WHERE h.instrument_external_id IS NULL) AS n_unmatched,
               COUNT(*) FILTER (WHERE r.fx_missing) AS n_fx_missing
          FROM realized_lots_in(p_from, p_to, p_ccy, FALSE) r
          LEFT JOIN held h ON h.silver_source_id = r.silver_source_id
               AND h.account_external_id = r.account_external_id AND h.instrument_external_id = r.instrument_external_id
          LEFT JOIN instruments i ON i.silver_source_id = r.silver_source_id AND i.instrument_external_id = r.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = r.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = r.instrument_external_id
         GROUP BY ALL),
    sells AS (
        SELECT silver_source_id, account_external_id, instrument_external_id AS k,
               COUNT(*) FILTER (WHERE event = 'sell' AND NOT documented) AS n_undocumented,
               COUNT(*) FILTER (WHERE event = 'in_kind') AS n_in_kind,
               COUNT(*) FILTER (WHERE event = 'corporate_action') AS n_corporate
          FROM gains_events(p_from, p_to) WHERE instrument_external_id IS NOT NULL GROUP BY ALL),
    keys AS (
        SELECT silver_source_id, account_external_id, k FROM side
        UNION SELECT silver_source_id, account_external_id, k FROM lots),
    src_first AS (SELECT silver_source_id, MIN(snapshot_at) AS first_at FROM positions GROUP BY 1),
    m AS (
        SELECT k.silver_source_id, k.account_external_id, k.k,
               e.position_key AS end_key, s.position_key AS start_key,
               COALESCE(e.symbol, s.symbol, l.symbol) AS symbol,
               COALESCE(e.name, s.name, l.description) AS name,
               COALESCE(e.asset_class, s.asset_class) AS asset_class,
               COALESCE(e.vehicle, s.vehicle) AS vehicle,
               COALESCE(e.currency, s.currency, l.currency) AS currency,
               COALESCE(e.display_name, s.display_name, a.display_name) AS display_name,
               COALESCE(e.nickname, s.nickname, a.nickname) AS nickname,
               s.quantity AS quantity_start, e.quantity AS quantity_end,
               e.book_value, e.book_x, e.market_value, e.value_x, e.unrealized,
               s.unrealized_x AS unrealized_start_x, e.unrealized_x AS unrealized_end_x,
               CASE WHEN s.unrealized_x IS NULL AND e.unrealized_x IS NULL THEN NULL
                    ELSE COALESCE(e.unrealized_x, 0) - COALESCE(s.unrealized_x, 0) END AS unrealized_change_x,
               l.realized, l.realized_x,
               e.stamp, COALESCE(e.acquisition_date, s.acquisition_date) AS acquisition_date,
               COALESCE(e.open_lots, 0) AS open_lots,
               COALESCE(e.paid_in, FALSE) OR COALESCE(s.paid_in, FALSE) AS paid_in,
               se.n_undocumented, se.n_in_kind, se.n_corporate, l.n_without_gain, l.n_undated, l.n_unmatched,
               COALESCE(e.n_fx_missing, 0) + COALESCE(s.n_fx_missing, 0) + COALESCE(l.n_fx_missing, 0) AS n_fx_missing,
               sf.first_at
          FROM keys k
          LEFT JOIN side e ON e.at_end AND e.silver_source_id = k.silver_source_id
               AND e.account_external_id = k.account_external_id AND e.k = k.k
          LEFT JOIN side s ON NOT s.at_end AND s.silver_source_id = k.silver_source_id
               AND s.account_external_id = k.account_external_id AND s.k = k.k
          LEFT JOIN lots l ON l.silver_source_id = k.silver_source_id
               AND l.account_external_id = k.account_external_id AND l.k = k.k
          LEFT JOIN sells se ON se.silver_source_id = k.silver_source_id
               AND se.account_external_id = k.account_external_id AND se.k = k.k
          LEFT JOIN accounts a ON a.silver_source_id = k.silver_source_id AND a.account_external_id = k.account_external_id
          LEFT JOIN src_first sf ON sf.silver_source_id = k.silver_source_id)
    SELECT silver_source_id, account_external_id, display_name, nickname,
           COALESCE(end_key, start_key) AS position_key,
           CASE WHEN end_key IS NULL AND start_key IS NULL THEN k END AS lot_key,
           symbol, name, asset_class, vehicle, currency,
           CAST(quantity_start AS VARCHAR) AS quantity_start, CAST(quantity_end AS VARCHAR) AS quantity_end,
           CAST(book_value AS VARCHAR) AS book_value,
           CAST(book_x::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(market_value AS VARCHAR) AS market_value,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(unrealized AS VARCHAR) AS unrealized_gain,
           CAST(unrealized_start_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_start,
           CAST(unrealized_end_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_end,
           CAST(unrealized_change_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_change,
           CAST(realized AS VARCHAR) AS realized_gain,
           CAST(realized_x::DECIMAL(28,4) AS VARCHAR) AS realized,
           CAST(CASE WHEN realized_x IS NULL AND unrealized_change_x IS NULL THEN NULL
                     ELSE COALESCE(realized_x, 0) + COALESCE(unrealized_change_x, 0) END::DECIMAL(28,4) AS VARCHAR) AS gain,
           unrealized::DOUBLE / NULLIF(book_value::DOUBLE, 0) AS unrealized_ratio,
           stamp AS basis_stamp,
           CAST(acquisition_date AS VARCHAR) AS acquisition_date,
           open_lots,
           concat_ws(';',
               CASE WHEN n_undocumented > 0 THEN 'sells_without_documents=' || n_undocumented END,
               CASE WHEN n_without_gain > 0 THEN 'lots_without_gain=' || n_without_gain END,
               CASE WHEN n_undated > 0 THEN 'undated_lots=' || n_undated END,
               CASE WHEN n_unmatched > 0 THEN 'unmatched_lots=' || n_unmatched END,
               CASE WHEN n_in_kind > 0 THEN 'in_kind_moves=' || n_in_kind END,
               CASE WHEN n_corporate > 0 THEN 'corporate_actions=' || n_corporate END,
               CASE WHEN paid_in THEN 'paid_in_basis' END,
               CASE WHEN first_at BETWEEN p_from AND p_to THEN 'onboarded_in_window=' || silver_source_id END,
               CASE WHEN n_fx_missing > 0 THEN 'fx_missing=' || n_fx_missing END) AS quality
      FROM m
     ORDER BY silver_source_id, account_external_id, COALESCE(end_key, start_key, k)
);

-- report_gains_coverage: per account, how far the window's gains can be
-- trusted. One row per account holding positions a cost basis can
-- describe at the window's end (gains_position_lines' basis_applies),
-- or with sells or primary realized lots inside it. The verdict reads:
-- `no_basis` an account holding positions, none with a cost basis;
-- `no_realized` sells no document covers, on an account whose
-- positions are covered; `ok` positions at least 99% covered by value
-- and every sell documented; `partial` anything between.
CREATE OR REPLACE MACRO report_gains_coverage(p_from, p_to, p_ccy) AS TABLE (
    WITH lines AS (SELECT * FROM gains_position_lines([p_to], p_ccy) WHERE basis_applies),
    pos AS (
        SELECT l.silver_source_id, l.account_external_id,
               COUNT(*) AS n_positions,
               COUNT(l.book_value) AS n_with_basis,
               SUM(l.value_x) AS value_x,
               SUM(l.value_x) FILTER (WHERE l.book_value IS NOT NULL) AS value_with_basis_x,
               SUM(abs(l.value_x)) AS abs_value_x,
               SUM(abs(l.value_x)) FILTER (WHERE l.book_value IS NOT NULL) AS abs_basis_value_x,
               string_agg(DISTINCT l.stamp, ',' ORDER BY l.stamp) AS stamps,
               COALESCE(SUM(ol.n), 0) AS open_lots
          FROM lines l
          LEFT JOIN (SELECT silver_source_id, snapshot_at, account_external_id, position_key, COUNT(*) AS n
                       FROM position_lots GROUP BY ALL) ol
            ON ol.silver_source_id = l.silver_source_id AND ol.snapshot_at = l.snapshot_at
           AND ol.account_external_id = l.account_external_id AND ol.position_key = l.position_key
         GROUP BY ALL),
    sells AS (
        SELECT silver_source_id, account_external_id,
               COUNT(*) FILTER (WHERE event = 'sell') AS n_sells,
               COUNT(*) FILTER (WHERE event = 'sell' AND NOT documented) AS n_undocumented
          FROM gains_events(p_from, p_to) WHERE event = 'sell' GROUP BY ALL),
    lots AS (
        SELECT silver_source_id, account_external_id, COUNT(*) AS n_lots
          FROM realized_lots_in(p_from, p_to, p_ccy, FALSE) GROUP BY ALL),
    docs AS (
        SELECT silver_source_id, account_external_id,
               string_agg(DISTINCT document_kind, ',' ORDER BY document_kind) AS documents
          FROM realized_lots
         WHERE tax_year BETWEEN year(epoch_ms(CAST(p_from AS BIGINT) * 1000)) AND year(epoch_ms(CAST(p_to AS BIGINT) * 1000))
         GROUP BY ALL),
    keys AS (
        SELECT silver_source_id, account_external_id FROM pos
        UNION SELECT silver_source_id, account_external_id FROM sells
        UNION SELECT silver_source_id, account_external_id FROM lots),
    m AS (
        SELECT k.silver_source_id, k.account_external_id, a.display_name, a.nickname, a.tax_wrapper,
               p.n_positions, p.n_with_basis, p.value_x, p.value_with_basis_x,
               p.abs_basis_value_x / NULLIF(p.abs_value_x, 0) AS coverage,
               p.stamps, COALESCE(p.open_lots, 0) AS open_lots,
               COALESCE(s.n_sells, 0) AS sells, COALESCE(s.n_undocumented, 0) AS n_undocumented,
               COALESCE(l.n_lots, 0) AS realized_lots, d.documents
          FROM keys k
          LEFT JOIN pos p USING (silver_source_id, account_external_id)
          LEFT JOIN sells s USING (silver_source_id, account_external_id)
          LEFT JOIN lots l USING (silver_source_id, account_external_id)
          LEFT JOIN docs d USING (silver_source_id, account_external_id)
          LEFT JOIN accounts a USING (silver_source_id, account_external_id))
    SELECT silver_source_id, account_external_id, display_name, nickname, tax_wrapper,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value,
           CAST(value_with_basis_x::DECIMAL(28,4) AS VARCHAR) AS value_with_basis,
           coverage AS basis_coverage, stamps AS basis_stamps, open_lots,
           sells, realized_lots, documents,
           CASE WHEN COALESCE(n_positions, 0) > 0 AND n_with_basis = 0 THEN 'no_basis'
                WHEN n_undocumented > 0 AND (COALESCE(n_positions, 0) = 0 OR coverage >= 0.99) THEN 'no_realized'
                WHEN n_undocumented = 0 AND (COALESCE(n_positions, 0) = 0 OR coverage >= 0.99) THEN 'ok'
                ELSE 'partial' END AS verdict
      FROM m
     ORDER BY silver_source_id, account_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (116, CAST(epoch(now()) AS BIGINT));
