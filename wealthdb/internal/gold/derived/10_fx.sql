-- Foreign exchange: one rate a day per pair, and the paths that
-- convert an amount from one currency into another.

-- The SQL FX layer: two views the report macros join against set-wise,
-- so no line looks its rate up on its own. Flat semantics:
-- nearest rate at-or-before the line's snapshot (NO interpolation), with
-- source priority and CHF/USD triangulation. See docs/DESIGN.md and
-- web/DESIGN.md.
--
-- Rate direction convention (fx_rates): a row (base=X, quote=Y, mid=R)
-- means "1 Y = R X". So Y->X multiplies by R; X->Y divides by R.

-- fx_norm: every stored rate normalised to a from->to multiply-rate,
-- carrying the source's persisted priority rank. Both directions are
-- emitted (direct = stored row; reciprocal = 1/mid_rate), so the macros
-- never branch on direct-vs-reciprocal — a single nearest lookup per leg.
-- recip marks the reciprocal edge, and the source id rides along, so
-- fx_daily can break every tie the same way.
CREATE VIEW fx_norm AS
    SELECT r.base_currency  AS to_ccy,
           r.quote_currency AS from_ccy,
           r.snapshot_at,
           CAST(r.mid_rate AS DOUBLE) AS rate,
           COALESCE(s.fx_priority, 2147483647) AS pri,
           FALSE AS recip,
           r.silver_source_id
      FROM fx_rates r
      LEFT JOIN silver_sources s USING (silver_source_id)
     WHERE r.mid_rate <> 0
    UNION ALL
    SELECT r.quote_currency AS to_ccy,
           r.base_currency  AS from_ccy,
           r.snapshot_at,
           1.0 / CAST(r.mid_rate AS DOUBLE) AS rate,
           COALESCE(s.fx_priority, 2147483647) AS pri,
           TRUE AS recip,
           r.silver_source_id
      FROM fx_rates r
      LEFT JOIN silver_sources s USING (silver_source_id)
     WHERE r.mid_rate <> 0;

-- fx_daily: one rate a day per pair, by source priority, then newest,
-- then a stored rate before a reciprocal, then the source id. The last
-- two keys only break ties, so the winner never follows the plan.
-- Each pair's EARLIEST rate also reaches back to day 0 through a floor
-- row, so a line whose snapshot predates its currency's FX history (a
-- holding older than the first rate of its pair) clamps to the earliest
-- available rate in the macros' at-or-before ASOF instead of failing to
-- convert and dropping out of the rollups. A real day always out-ranks
-- the day-0 floor in the ASOF, so rates within the history are the
-- stored ones.

-- ---- fx_daily with the earliest-rate floor (clamp) -------------------------
CREATE VIEW fx_daily AS
    WITH per_day AS (
        SELECT from_ccy, to_ccy, day, rate
          FROM (
            SELECT from_ccy, to_ccy, (snapshot_at // 86400) AS day, rate,
                   ROW_NUMBER() OVER (
                       PARTITION BY from_ccy, to_ccy, (snapshot_at // 86400)
                       ORDER BY pri ASC, snapshot_at DESC, recip ASC, silver_source_id ASC
                   ) AS rn
              FROM fx_norm
          )
         WHERE rn = 1)
    SELECT from_ccy, to_ccy, day, rate FROM per_day
    UNION ALL
    SELECT from_ccy, to_ccy, 0 AS day, arg_min(rate, day) AS rate
      FROM per_day GROUP BY from_ccy, to_ccy;

-- ============================================================
-- One conversion helper for the reporting currencies: USD, CHF, EUR
-- and GBP.
--
-- The `_multi` report macros value every line in each reporting
-- currency. They share the conversion:
--
--   fx_reporting_legs     a currency's rate to each reporting
--                         currency, per day a rate is quoted;
--   fx_reporting_bridges  the CHF and USD rates to each reporting
--                         currency, per day a rate is quoted;
--   fx_reporting_value    one value in one reporting currency.
--
-- A macro joins the two views once each and calls the scalar macro
-- per currency. The conversion order is the identity, the direct rate,
-- then through CHF, then through USD — the order the single-currency
-- macros use, so a `_multi` column equals the matching `-x` report.
-- Each view is laid out on a grid of every day one of its rates is
-- quoted, so an ASOF lookup on the grid finds the same rate an ASOF
-- lookup on fx_daily does. The products keep their left-to-right
-- order, so the values are the same to the bit.
--
-- A conversion to a base currency keeps its own joins: the base is a
-- per-row currency rather than a reporting one.
-- ============================================================

CREATE VIEW fx_reporting_legs AS
    WITH d AS MATERIALIZED (
        SELECT from_ccy, to_ccy, day, rate FROM fx_daily
         WHERE to_ccy IN ('USD', 'CHF', 'EUR', 'GBP')),
    grid AS (SELECT DISTINCT from_ccy, day FROM d)
    SELECT g.from_ccy, g.day,
           l_usd.rate AS to_usd, l_chf.rate AS to_chf,
           l_eur.rate AS to_eur, l_gbp.rate AS to_gbp
      FROM grid g
      ASOF LEFT JOIN d l_usd ON l_usd.from_ccy = g.from_ccy AND l_usd.to_ccy = 'USD' AND l_usd.day <= g.day
      ASOF LEFT JOIN d l_chf ON l_chf.from_ccy = g.from_ccy AND l_chf.to_ccy = 'CHF' AND l_chf.day <= g.day
      ASOF LEFT JOIN d l_eur ON l_eur.from_ccy = g.from_ccy AND l_eur.to_ccy = 'EUR' AND l_eur.day <= g.day
      ASOF LEFT JOIN d l_gbp ON l_gbp.from_ccy = g.from_ccy AND l_gbp.to_ccy = 'GBP' AND l_gbp.day <= g.day;

CREATE VIEW fx_reporting_bridges AS
    WITH d AS MATERIALIZED (
        SELECT from_ccy, to_ccy, day, rate FROM fx_daily
         WHERE from_ccy IN ('CHF', 'USD') AND to_ccy IN ('USD', 'CHF', 'EUR', 'GBP')),
    grid AS (SELECT DISTINCT day FROM d)
    SELECT g.day,
           chf_usd.rate AS chf_usd, chf_eur.rate AS chf_eur, chf_gbp.rate AS chf_gbp,
           usd_chf.rate AS usd_chf, usd_eur.rate AS usd_eur, usd_gbp.rate AS usd_gbp
      FROM grid g
      ASOF LEFT JOIN d chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= g.day
      ASOF LEFT JOIN d chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= g.day
      ASOF LEFT JOIN d chf_gbp ON chf_gbp.from_ccy = 'CHF' AND chf_gbp.to_ccy = 'GBP' AND chf_gbp.day <= g.day
      ASOF LEFT JOIN d usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= g.day
      ASOF LEFT JOIN d usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= g.day
      ASOF LEFT JOIN d usd_gbp ON usd_gbp.from_ccy = 'USD' AND usd_gbp.to_ccy = 'GBP' AND usd_gbp.day <= g.day;

-- `p_amt` in reporting currency `p_tgt`, for a line in currency `p_ccy`.
-- `p_legs` and `p_bridges` are the line's rows of the two views above,
-- passed whole. A target has no bridge through itself, so its CASE arm
-- is NULL and that step drops out of the COALESCE.
CREATE MACRO fx_reporting_value(p_amt, p_ccy, p_tgt, p_legs, p_bridges) AS
    COALESCE(CASE WHEN p_ccy = p_tgt THEN p_amt::DOUBLE END,
             p_amt::DOUBLE * CASE p_tgt WHEN 'USD' THEN p_legs.to_usd WHEN 'CHF' THEN p_legs.to_chf
                                        WHEN 'EUR' THEN p_legs.to_eur WHEN 'GBP' THEN p_legs.to_gbp END,
             p_amt::DOUBLE * p_legs.to_chf
                 * CASE p_tgt WHEN 'USD' THEN p_bridges.chf_usd WHEN 'EUR' THEN p_bridges.chf_eur
                              WHEN 'GBP' THEN p_bridges.chf_gbp END,
             p_amt::DOUBLE * p_legs.to_usd
                 * CASE p_tgt WHEN 'CHF' THEN p_bridges.usd_chf WHEN 'EUR' THEN p_bridges.usd_eur
                              WHEN 'GBP' THEN p_bridges.usd_gbp END);

-- fx_amount: p_amt in p_ccy converted into p_tgt at p_rate, a rate
-- from fx_rates_to(p_tgt); the identity when the currencies agree.
CREATE MACRO fx_amount(p_amt, p_ccy, p_tgt, p_rate) AS
    CASE WHEN p_ccy = p_tgt THEN p_amt::DOUBLE ELSE p_amt::DOUBLE * p_rate END;

-- ============================================================
-- The gains readers in one evaluation for several output currencies
-- and readings of a missing cost basis.
--
-- MaterializeGains writes report_gains for every reporting currency
-- under both readings. Run per currency and reading, the readers would
-- build the buckets, read the positions and the realized lots, and
-- match the events and findings eight times over. The _all macros take
-- lists of currencies and readings and do that work once:
-- each line and lot repeats under every currency and reading, carries
-- out_currency and missing_basis, and converts at its own rate. The
-- single-currency macros are those over one-element lists, so every
-- reader keeps one definition.
-- ============================================================

-- fx_rates_into: fx_rates_to for each currency in p_ccys: the
-- rate from every currency gold's figures are in into to_ccy, on every
-- day fx_daily holds any rate. Direct, else through CHF, else through
-- USD.
CREATE MACRO fx_rates_into(p_ccys) AS TABLE (
    WITH ccys AS (
        SELECT currency AS from_ccy FROM positions
        UNION SELECT currency FROM cash_balances
        UNION SELECT currency FROM realized_lots
        UNION SELECT currency FROM lot_realized
        UNION SELECT currency FROM lots
        UNION SELECT currency FROM lot_anchors),
    grid AS (
        SELECT t.to_ccy, c.from_ccy, d.day
          FROM (SELECT unnest(p_ccys) AS to_ccy) t, ccys c, (SELECT DISTINCT day FROM fx_daily) d)
    SELECT g.to_ccy, g.from_ccy, g.day, COALESCE(x.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
      FROM grid g
      ASOF LEFT JOIN fx_daily x  ON x.from_ccy  = g.from_ccy AND x.to_ccy  = g.to_ccy AND x.day  <= g.day
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.from_ccy AND c1.to_ccy = 'CHF'    AND c1.day <= g.day
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = g.to_ccy AND c2.day <= g.day
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.from_ccy AND u1.to_ccy = 'USD'    AND u1.day <= g.day
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = g.to_ccy AND u2.day <= g.day
);

CREATE MACRO fx_rates_to(p_ccy) AS TABLE (
    SELECT from_ccy, day, rate FROM fx_rates_into([p_ccy])
);
