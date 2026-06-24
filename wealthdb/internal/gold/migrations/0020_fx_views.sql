-- The SQL FX layer: replaces the per-row Go ConvertValue (an N+1 of 1-3
-- fx_rates lookups per position/transaction/cash line) with two views the
-- report macros (migration 0021) join against set-wise. Flat semantics:
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
CREATE OR REPLACE VIEW fx_norm AS
    SELECT r.base_currency  AS to_ccy,
           r.quote_currency AS from_ccy,
           r.snapshot_at,
           CAST(r.mid_rate AS DOUBLE) AS rate,
           COALESCE(s.fx_priority, 2147483647) AS pri
      FROM fx_rates r
      LEFT JOIN silver_sources s USING (silver_source_id)
     WHERE r.mid_rate <> 0
    UNION ALL
    SELECT r.quote_currency AS to_ccy,
           r.base_currency  AS from_ccy,
           r.snapshot_at,
           1.0 / CAST(r.mid_rate AS DOUBLE) AS rate,
           COALESCE(s.fx_priority, 2147483647) AS pri
      FROM fx_rates r
      LEFT JOIN silver_sources s USING (silver_source_id)
     WHERE r.mid_rate <> 0;

-- fx_daily: ONE rate per (from, to, UTC-day), chosen by priority then the
-- latest snapshot within the day. This collapses the multi-source tiebreak
-- to a per-day decision (matching the old "order by UTC day, then source
-- priority, then exact time"); the macros' ASOF joins then pick the nearest
-- day at-or-before each line. NULL priority (unranked) sorts last via the
-- COALESCE in fx_norm.
CREATE OR REPLACE VIEW fx_daily AS
    SELECT from_ccy, to_ccy, day, rate
      FROM (
        SELECT from_ccy, to_ccy,
               (snapshot_at // 86400) AS day,
               rate,
               ROW_NUMBER() OVER (
                   PARTITION BY from_ccy, to_ccy, (snapshot_at // 86400)
                   ORDER BY pri ASC, snapshot_at DESC
               ) AS rn
          FROM fx_norm
      )
     WHERE rn = 1;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (20, CAST(epoch(now()) AS BIGINT));
