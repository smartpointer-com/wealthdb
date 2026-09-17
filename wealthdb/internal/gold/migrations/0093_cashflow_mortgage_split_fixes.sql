-- ============================================================
-- gold schema, migration 0093 —
--   the mortgage split, made independent of the question asked.
--
-- The split shares an interval's retired principal among the payments
-- inside that interval. Five properties of that allocation are restored
-- here; `cashflow_lines_base` is carried forward whole.
--
-- 1. THE ALLOCATION IS A PROPERTY OF THE INTERVAL, NOT OF THE REPORT.
--    `pay` is read over ALL TIME and only the OUTPUT is windowed.
--    Clipped to the report range, an instalment competed only against
--    whichever of its interval's siblings the range happened to
--    include, so the same instalment split one way over a year and
--    another over the month containing it — the narrow window reporting
--    more principal than the interval ever retired.
--
-- 2. EXACTLY THE ROWS THAT CAN BE READ ARE THE ROWS THAT CAN SPEND.
--    The payments come from the resolved lines. A superset is not safe
--    merely because the share is never READ on a widened row: a widened
--    row still RANKS in the allocation and still consumes the interval's
--    decline, and the share it takes is then dropped — turning real
--    amortisation into interest with nothing to show for it.
--
-- 3. ONLY OUTFLOWS RETIRE DEBT, so only outflows compete and only
--    outflows split. A mortgage line runs in either direction
--    (docs/CASHFLOW.md §4), and ranking on `abs(...)` let money BORROWED
--    outrank the repayments, take the principal they had retired, and
--    draw itself as a positive interest edge on the wrong side of the
--    diagram. A drawdown keeps its whole amount and its class, which the
--    leaf rule then attaches to the hub.
--
-- 4. "STILL OBSERVED" ASKS WHETHER THE SOURCE WENT ON OBSERVING
--    ANYTHING after this mortgage stopped — not whether a newer
--    MORTGAGE snapshot exists, which is the same date as the mortgage's
--    own last observation wherever a source tracks a single one, leaving
--    the closed-mortgage arm unable to fire and the final repayment
--    drawn as interest.
--
-- 5. THE TWO SHARES SUM TO THE ROW. A residue under half a cent now
--    folds into the principal line; emitting no interest line for it
--    made a row's shares sum to the principal instead, quietly
--    contradicting the one invariant that makes the split safe to draw.
--
-- `obs` also aggregates per (source, account, snapshot) rather than
-- trusting one position row per mortgage per snapshot. Nothing in gold
-- holds two today; the window functions below would interleave them if
-- anything ever did.
-- ============================================================

CREATE OR REPLACE MACRO cashflow_lines_base(p_from, p_to) AS TABLE (
    -- Every mortgage instalment there has ever been, not merely the
    -- ones the window asks about: an interval's principal is shared out
    -- among the payments INSIDE THE INTERVAL, and which of those the
    -- reader happens to be looking at cannot change the answer.
    WITH pay AS (
        SELECT n.silver_source_id, n.transaction_external_id, n.occurred_at,
               n.far_silver_source_id AS msrc, n.far_account_external_id AS macct,
               -n.net_amount AS paid
          FROM cashflow_txn_nodes(0, 9223372036854775807) n
         WHERE n.class = 'mortgage'
           AND n.net_amount < 0
           AND n.far_silver_source_id    IS NOT NULL
           AND n.far_account_external_id IS NOT NULL
    ),
    bal AS (
        SELECT p.silver_source_id AS msrc, p.account_external_id AS macct,
               p.snapshot_at, SUM(p.market_value) AS mv
          FROM positions p
          JOIN accounts a ON a.silver_source_id    = p.silver_source_id
                         AND a.account_external_id = p.account_external_id
         WHERE a.account_kind = 'mortgage'
         GROUP BY 1, 2, 3
    ),
    -- What the source last saw of ANYTHING. A mortgage whose own
    -- observations stop before that was closed out; one observed right
    -- up to it is still standing.
    src AS (
        SELECT silver_source_id AS msrc, MAX(snapshot_at) AS src_last
          FROM positions GROUP BY 1
    ),
    obs AS (
        SELECT b.msrc, b.macct,
               b.snapshot_at // 86400 AS from_day,
               COALESCE(LEAD(b.snapshot_at) OVER w // 86400, 99999999) AS to_day,
               b.mv, LEAD(b.mv) OVER w AS next_mv, s.src_last
          FROM bal b JOIN src s ON s.msrc = b.msrc
        WINDOW w AS (PARTITION BY b.msrc, b.macct ORDER BY b.snapshot_at)
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
        SELECT n.*,
               CASE WHEN n.class = 'mortgage' AND n.net_amount < 0
                    THEN COALESCE(a.principal, 0) ELSE 0 END AS principal,
               -- A line splits only when it is a mortgage OUTFLOW that
               -- retired something and left something over.
               (n.class = 'mortgage' AND n.net_amount < 0) AS splits
          FROM cashflow_txn_nodes(p_from, p_to) n
          JOIN cashflow_pool_accounts() p
                 ON p.silver_source_id    = n.silver_source_id
                AND p.account_external_id = n.account_external_id
          LEFT JOIN alloc a
                 ON a.silver_source_id        = n.silver_source_id
                AND a.transaction_external_id = n.transaction_external_id
         WHERE n.disposition = 'line'
    )
    SELECT l.* EXCLUDE (principal, splits) REPLACE (
               CAST(CASE
                        WHEN NOT l.splits                                    THEN l.net_amount
                        -- The residue is under half a cent: one line
                        -- carrying the whole row, so the shares still
                        -- sum to it.
                        WHEN -l.net_amount - l.principal <= 0.005            THEN l.net_amount
                        WHEN sh.share = 'principal'                          THEN -l.principal
                        ELSE l.net_amount + l.principal
                    END AS DECIMAL(28,4)) AS net_amount,
               CASE WHEN NOT l.splits          THEN l.grp
                    WHEN sh.share = 'principal' THEN 'mortgage_amortization'
                    ELSE 'mortgage_interest' END AS grp,
               CASE WHEN NOT l.splits          THEN l.group_label
                    WHEN sh.share = 'principal' THEN 'Mortgage amortization'
                    ELSE 'Mortgage interest' END AS group_label)
      FROM line l
      JOIN (VALUES ('principal'), ('interest')) AS sh(share)
        ON (NOT l.splits AND sh.share = 'interest')
        OR (l.splits AND sh.share = 'principal' AND l.principal > 0)
        OR (l.splits AND sh.share = 'interest'
            AND -l.net_amount - l.principal > 0.005)
     ORDER BY l.occurred_at, l.silver_source_id, l.transaction_external_id,
              CASE WHEN l.splits AND sh.share = 'principal' THEN 0 ELSE 1 END
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (93, CAST(epoch(now()) AS BIGINT));
