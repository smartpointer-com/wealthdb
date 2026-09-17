-- ============================================================
-- gold schema, migration 0089 —
--   the coverage report: which accounts the statement can be trusted on.
--
-- The reconciliation memo (0083) answers one question per period for the
-- whole pool: does the computed cash section agree with the observed
-- balances? When it does not, it says so in a single number and stops.
-- Finding the account behind that number has been hand archaeology every
-- time, and the answer has never been in the statement — it has been in
-- a query somebody wrote from scratch.
--
-- This is that query, per account and per period, and it differs from a
-- naive version in the three ways a naive version is wrong.
--
--   1. IT DOES NOT CONVERT. A per-account gap belongs in the account's
--      own currency; carrying it to an output currency adds a rate to
--      every row and a second error term to every gap, and answers a
--      question nobody asked. The memo converts because it aggregates.
--      This does not aggregate.
--
--   2. IT NAMES ITS OWN BLIND SPOT. Gold pins no canonical sign for six
--      kinds — the FX family, corporate actions, and the two catch-alls
--      — so their amounts cannot be summed into a cash delta. Dropping
--      them silently would make an account that converts currencies
--      look like an account with a huge unexplained gap, which is
--      exactly the false positive that makes a diagnostic useless. So
--      the volume that had to be dropped is a COLUMN, and a gap no
--      larger than it reads `obscured`: not clean, not damning, simply
--      not answerable from what the adapter pinned a sign for.
--
--   3. IT REPORTS WHAT IT CANNOT MEASURE. An account with no balance
--      history at all never enters a join-based version's output — it
--      is absent rather than flagged, which reads as absent rather than
--      as unknown. Those rows are here with status `unmeasurable`, and
--      an account whose balance series BEGINS inside the period reads
--      `opening`, because the difference across that boundary is not a
--      delta.
--
-- Boundary balances are taken ASOF, per (account, currency), with the
-- same balance-kind precedence the memo pins, so a period boundary
-- falling between two snapshots reads the last one before it rather
-- than nothing. Balances are carried forward; they are not
-- interpolated.
--
-- The overlay accounts are excluded, as everywhere: they are a
-- per-portfolio synthetic and have no balances of their own.
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
    led AS (
        SELECT spend_period_bucket(p_period, t.occurred_at) AS period_start,
               t.silver_source_id, t.account_external_id, t.currency,
               SUM(CASE WHEN t.kind NOT IN ('fx','fx_forward','fx_swap',
                                            'corporate_action','journal','other')
                        THEN t.net_amount ELSE 0 END) AS ledger,
               SUM(CASE WHEN t.kind IN ('fx','fx_forward','fx_swap',
                                        'corporate_action','journal','other')
                        THEN abs(t.net_amount) ELSE 0 END) AS unsigned_volume,
               COUNT(*) AS txn_count
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to AND t.net_amount IS NOT NULL
         GROUP BY 1,2,3,4),
    grid AS (SELECT b.period_start, b.open_day, b.close_day, s.*
               FROM bday b CROSS JOIN series s),
    bal AS (
        SELECT g.period_start, g.silver_source_id, g.account_external_id, g.currency,
               o.amount AS open_amt, c.amount AS close_amt
          FROM grid g
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
                THEN b.close_amt - b.open_amt END AS DECIMAL(28,4)) AS VARCHAR) AS measured,
           CAST(CAST(CASE WHEN b.open_amt IS NOT NULL AND b.close_amt IS NOT NULL
                THEN COALESCE(l.ledger, 0) - (b.close_amt - b.open_amt)
                END AS DECIMAL(28,4)) AS VARCHAR) AS gap,
           CASE WHEN b.close_amt IS NULL THEN 'unmeasurable'
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
    VALUES (89, CAST(epoch(now()) AS BIGINT));
