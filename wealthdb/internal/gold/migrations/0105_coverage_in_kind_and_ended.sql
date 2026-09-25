-- ============================================================
-- gold schema, migration 0105 —
--   coverage reads an in-kind transfer as unsigned, and a series that
--   stopped as ended.
--
-- Two ways the coverage report (0089) named a gap that was not one.
--
--   1. An in-kind transfer — securities delivered into or out of an
--      account, `transfer_in` / `transfer_out` carrying an instrument —
--      is valued at the securities' worth and moves no cash. Summed into
--      the ledger it read as a cash flow the balances never saw, so an
--      account that received a portfolio in kind showed a gap the size
--      of the portfolio. It now joins the unsigned volume, beside the
--      kinds that pin no sign. A transfer without an instrument is cash
--      and stays in the ledger.
--
--   2. A balance series that stopped before a period carried its last
--      balance across the whole period, so the measured delta was zero
--      and every transaction read as a gap. Such a row now reads
--      `ended`, with neither a measured delta nor a gap, as `opening`
--      marks a series that began inside the period.
--
-- The macro is carried forward whole from 0089 with those two changes;
-- it is OR REPLACE, so the migration re-runs cleanly.
-- ============================================================

CREATE OR REPLACE MACRO report_cashflow_coverage(p_from, p_to, p_period) AS TABLE (
    WITH pool AS (
        SELECT silver_source_id, account_external_id, account_kind,
               COALESCE(nickname, display_name, account_external_id) AS account
          FROM cashflow_pool_accounts()
         WHERE account_kind IS DISTINCT FROM 'overlay'),
    chosen AS (
        SELECT silver_source_id, account_external_id, currency, snapshot_at, amount
          FROM (SELECT cb.silver_source_id, cb.account_external_id, cb.currency,
                       cb.snapshot_at, cb.amount,
                       ROW_NUMBER() OVER (
                           PARTITION BY cb.silver_source_id, cb.snapshot_at,
                                        cb.account_external_id, cb.currency
                           ORDER BY CASE cb.balance_kind
                               WHEN 'current' THEN 1 WHEN 'closing' THEN 2
                               WHEN 'available' THEN 3 WHEN 'aggregated' THEN 4
                               WHEN 'opening' THEN 5 WHEN 'initial' THEN 6
                               WHEN 'projected' THEN 7 ELSE 99 END, cb.balance_kind) AS rn
                  FROM cash_balances cb
                  JOIN pool p ON p.silver_source_id    = cb.silver_source_id
                             AND p.account_external_id = cb.account_external_id)
         WHERE rn = 1),
    buckets AS (
        SELECT DISTINCT spend_period_bucket(p_period, t.occurred_at) AS period_start
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to),
    bday AS (
        SELECT period_start,
               (GREATEST(COALESCE(period_start, p_from), p_from) - 1) // 86400 AS open_day,
               cashflow_period_end(p_period, period_start, p_to) // 86400 AS close_day
          FROM buckets),
    series AS (
        SELECT DISTINCT silver_source_id, account_external_id, currency FROM chosen
        UNION
        SELECT DISTINCT t.silver_source_id, t.account_external_id, t.currency
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to AND t.net_amount IS NOT NULL),
    -- A row moves no cash when its kind pins no sign, or when it is an
    -- in-kind transfer: securities delivered in or out, valued but paid
    -- for by nobody.
    unsigned AS (
        SELECT t.*,
               t.kind IN ('fx','fx_forward','fx_swap','corporate_action','journal','other')
               OR (t.kind IN ('transfer_in','transfer_out')
                   AND t.instrument_external_id IS NOT NULL) AS no_cash
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to AND t.net_amount IS NOT NULL),
    led AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               silver_source_id, account_external_id, currency,
               SUM(CASE WHEN no_cash THEN 0 ELSE net_amount END) AS ledger,
               SUM(CASE WHEN no_cash THEN abs(net_amount) ELSE 0 END) AS unsigned_volume,
               COUNT(*) AS txn_count
          FROM unsigned
         GROUP BY 1,2,3,4),
    -- Where each balance series stops. A series that stopped before a
    -- period carries its last balance across it, which measures nothing.
    last_seen AS (
        SELECT silver_source_id, account_external_id, currency,
               MAX(snapshot_at) AS last_at
          FROM chosen
         GROUP BY 1,2,3),
    grid AS (SELECT b.period_start, b.open_day, b.close_day, s.*
               FROM bday b CROSS JOIN series s),
    bal AS (
        SELECT g.period_start, g.silver_source_id, g.account_external_id, g.currency,
               o.amount AS open_amt, c.amount AS close_amt,
               ls.last_at <= g.open_day * 86400 + 86399 AS ended
          FROM grid g
          LEFT JOIN last_seen ls
                 ON ls.silver_source_id    = g.silver_source_id
                AND ls.account_external_id = g.account_external_id
                AND ls.currency            = g.currency
          ASOF LEFT JOIN chosen o
                ON o.silver_source_id    = g.silver_source_id
               AND o.account_external_id = g.account_external_id
               AND o.currency            = g.currency
               AND o.snapshot_at        <= g.open_day * 86400 + 86399
          ASOF LEFT JOIN chosen c
                ON c.silver_source_id    = g.silver_source_id
               AND c.account_external_id = g.account_external_id
               AND c.currency            = g.currency
               AND c.snapshot_at        <= g.close_day * 86400 + 86399)
    SELECT b.period_start, p.silver_source_id, p.account, p.account_kind, b.currency,
           COALESCE(l.txn_count, 0) AS txn_count,
           CAST(CAST(l.ledger          AS DECIMAL(28,4)) AS VARCHAR) AS ledger,
           CAST(CAST(l.unsigned_volume AS DECIMAL(28,4)) AS VARCHAR) AS unsigned_volume,
           CAST(CAST(CASE WHEN b.open_amt IS NOT NULL AND b.close_amt IS NOT NULL
                               AND NOT b.ended
                THEN b.close_amt - b.open_amt END AS DECIMAL(28,4)) AS VARCHAR) AS measured,
           CAST(CAST(CASE WHEN b.open_amt IS NOT NULL AND b.close_amt IS NOT NULL
                               AND NOT b.ended
                THEN COALESCE(l.ledger, 0) - (b.close_amt - b.open_amt)
                END AS DECIMAL(28,4)) AS VARCHAR) AS gap,
           CASE WHEN b.close_amt IS NULL THEN 'unmeasurable'
                WHEN b.ended            THEN 'ended'
                WHEN b.open_amt  IS NULL THEN 'opening'
                WHEN abs(COALESCE(l.ledger, 0) - (b.close_amt - b.open_amt))
                     <= COALESCE(l.unsigned_volume, 0) THEN 'obscured'
                ELSE 'measured' END AS status
      FROM bal b
      JOIN pool p ON p.silver_source_id    = b.silver_source_id
                 AND p.account_external_id = b.account_external_id
      LEFT JOIN led l ON l.period_start IS NOT DISTINCT FROM b.period_start
                     AND l.silver_source_id    = b.silver_source_id
                     AND l.account_external_id = b.account_external_id
                     AND l.currency            = b.currency
     WHERE COALESCE(l.txn_count, 0) > 0 OR b.close_amt IS NOT NULL
     ORDER BY b.period_start, abs(COALESCE(COALESCE(l.ledger,0) - (b.close_amt - b.open_amt), 0)) DESC
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (105, CAST(epoch(now()) AS BIGINT));
