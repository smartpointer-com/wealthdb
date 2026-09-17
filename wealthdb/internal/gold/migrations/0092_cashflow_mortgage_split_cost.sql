-- ============================================================
-- gold schema, migration 0092 —
--   the mortgage split, at the cost the statement had before it.
--
-- 0091 shipped the split and made every cashflow query cost roughly
-- three times the memory it had. One query still fit; the dashboard
-- fires six at once against one shared pool, and the pool ran out. The
-- Metabase Cash Flow page failed on every card.
--
-- The cause was not the arithmetic, which touches a handful of rows.
-- It was the SHAPE. A table macro is inlined wherever it is named, so
-- `cashflow_txn_nodes` — the heaviest thing in the feature, a dozen
-- joins over every transaction gold holds — was planted in the plan
-- once per reference, and 0091 referenced it four times over: once for
-- the lines, once inside the helper macro that found the mortgage
-- payments, and once per arm of the UNION ALL that emitted the two
-- shares. Row counts stayed small the whole way, which is why nothing
-- in a result set showed it. The cost was in the plan, not the data.
--
-- Measured, as a floor on `SET memory_limit` for one full scan of
-- `web_cashflow`:
--
--     before the split      ~300 MB
--     0091 as shipped       ~900 MB      (fails at 600)
--     this migration        ~300 MB
--
-- Three changes of shape, no change of arithmetic:
--
--   * The payments are found from `transactions`, the spending overlay
--     and `accounts` directly rather than from the resolved lines. They
--     are the rows whose FAR ACCOUNT is a mortgage, which is a superset
--     of the rows the resolution will class as mortgage — and a
--     superset is safe, because the share is only ever READ on a line
--     the resolution classed that way. Small indexed tables, and the
--     node macro is no longer needed to find them.
--   * The two shares are emitted by joining each line against a
--     two-row VALUES list and keeping the arms that apply, instead of
--     by UNION ALL over the same source. One pass, one copy of the
--     plan.
--   * `cashflow_mortgage_principal` is dropped. Its only caller was
--     `cashflow_lines_base`, and a macro that cannot be called without
--     re-planting the node macro is a macro that should not exist.
--
-- The split itself is unchanged and so is every figure it produces:
-- the shares still sum to the row, and a line that is not a mortgage
-- instalment still passes through untouched.
-- ============================================================

DROP MACRO TABLE IF EXISTS cashflow_mortgage_principal;

CREATE OR REPLACE MACRO cashflow_lines_base(p_from, p_to) AS TABLE (
    WITH pay AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
               e.far_silver_source_id AS msrc, e.far_account_external_id AS macct,
               abs(t.net_amount) AS paid
          FROM transactions t
          JOIN spend_txn_enrichment e
            ON e.silver_source_id        = t.silver_source_id
           AND e.transaction_external_id = t.transaction_external_id
          JOIN accounts a
            ON a.silver_source_id    = e.far_silver_source_id
           AND a.account_external_id = e.far_account_external_id
         WHERE a.account_kind = 'mortgage'
           AND t.occurred_at BETWEEN p_from AND p_to
    ),
    obs AS (
        SELECT p.silver_source_id AS msrc, p.account_external_id AS macct,
               p.snapshot_at // 86400 AS from_day,
               COALESCE(LEAD(p.snapshot_at) OVER w // 86400, 99999999) AS to_day,
               p.market_value AS mv,
               LEAD(p.market_value) OVER w AS next_mv,
               MAX(p.snapshot_at) OVER (PARTITION BY p.silver_source_id) AS src_last
          FROM positions p
          JOIN accounts a ON a.silver_source_id    = p.silver_source_id
                         AND a.account_external_id = p.account_external_id
         WHERE a.account_kind = 'mortgage'
        WINDOW w AS (PARTITION BY p.silver_source_id, p.account_external_id
                     ORDER BY p.snapshot_at)
    ),
    retired AS (
        SELECT msrc, macct, from_day, to_day,
               CASE WHEN next_mv IS NOT NULL         THEN next_mv - mv
                    WHEN from_day * 86400 < src_last THEN -mv
                    ELSE 0 END AS retired
          FROM obs
    ),
    ranked AS (
        SELECT p.silver_source_id, p.transaction_external_id, p.paid,
               r.msrc, r.macct, r.from_day, r.retired,
               ROW_NUMBER() OVER (PARTITION BY r.msrc, r.macct, r.from_day
                                  ORDER BY p.paid DESC, p.transaction_external_id) AS rn
          FROM pay p
          JOIN retired r
            ON r.msrc = p.msrc AND r.macct = p.macct
           AND p.occurred_at // 86400 >= r.from_day
           AND p.occurred_at // 86400 <  r.to_day
    ),
    alloc AS (
        SELECT silver_source_id, transaction_external_id,
               CAST(greatest(0, least(paid, retired - COALESCE(
                   SUM(paid) OVER (PARTITION BY msrc, macct, from_day ORDER BY rn
                                   ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),
                   0))) AS DECIMAL(28,4)) AS principal
          FROM ranked
    ),
    line AS (
        SELECT n.*, CASE WHEN n.class = 'mortgage'
                         THEN COALESCE(a.principal, 0) ELSE 0 END AS principal
          FROM cashflow_txn_nodes(p_from, p_to) n
          JOIN cashflow_pool_accounts() p
                 ON p.silver_source_id    = n.silver_source_id
                AND p.account_external_id = n.account_external_id
          LEFT JOIN alloc a
                 ON a.silver_source_id        = n.silver_source_id
                AND a.transaction_external_id = n.transaction_external_id
         WHERE n.disposition = 'line'
    )
    SELECT l.* EXCLUDE (principal) REPLACE (
               CAST(CASE WHEN sh.share = 'principal'
                         THEN CASE WHEN l.net_amount < 0 THEN -l.principal ELSE l.principal END
                         ELSE l.net_amount + CASE WHEN l.net_amount < 0 THEN l.principal
                                                  ELSE -l.principal END
                    END AS DECIMAL(28,4)) AS net_amount,
               CASE WHEN l.class <> 'mortgage'         THEN l.grp
                    WHEN sh.share = 'principal'        THEN 'mortgage_amortization'
                    ELSE 'mortgage_interest' END AS grp,
               CASE WHEN l.class <> 'mortgage'         THEN l.group_label
                    WHEN sh.share = 'principal'        THEN 'Mortgage amortization'
                    ELSE 'Mortgage interest' END AS group_label)
      FROM line l
      JOIN (VALUES ('principal'), ('interest')) AS sh(share)
        ON (l.class <> 'mortgage' AND sh.share = 'interest')
        OR (l.class =  'mortgage' AND sh.share = 'principal'
            AND l.principal > 0)
        OR (l.class =  'mortgage' AND sh.share = 'interest'
            AND abs(l.net_amount) - l.principal > 0.005)
     ORDER BY l.occurred_at, l.silver_source_id, l.transaction_external_id, 
              CASE WHEN l.class = 'mortgage' AND sh.share = 'principal' THEN 0 ELSE 1 END
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (92, CAST(epoch(now()) AS BIGINT));
