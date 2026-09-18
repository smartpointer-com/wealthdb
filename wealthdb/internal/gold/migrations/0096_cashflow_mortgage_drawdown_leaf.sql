-- ============================================================
-- gold schema, migration 0096 —
--   money borrowed against a mortgage is a leaf of its own.
--
-- The Sankey's leaf stage is the self-edge rule — a group that repeats
-- its class has nothing finer to say, so it attaches to the hub rather
-- than drawing below it. The mortgage split relabels the group on a
-- mortgage OUTFLOW only, so a drawdown kept `mortgage` for its group
-- and fell out of the leaves; and because its class then HAD leaves,
-- the class was out of the atoms in its own right too, its hub edge
-- being the sum of those leaves. The drawdown was in neither.
--
-- What that draws is not a gap but a wrong answer. The residual cash
-- node is the other sections summed and negated over every line, the
-- drawdown included, so the money went on being counted there: a
-- household that borrowed and repaid read as a household that saved
-- the difference, and the hub stopped balancing.
--
-- `mortgage_drawdown` is the seventh leaf of cashflow's own, minted
-- here rather than in the resolution for the reason the other two are:
-- no family value names half a row, or names money borrowed against an
-- account the product holds. It is keyed off direction alone, so
-- financing's one class whose group could repeat it no longer can —
-- the leaf rule is structural again rather than true by arithmetic.
--
-- Where it draws follows from the existing `stays` rule with nothing
-- added: a leaf running against its class's net detaches and attaches
-- to the hub in its own right, which is what a drawdown is beside a
-- year of repayments. A class's hub edge stays the sum of the leaves
-- drawn through it.
--
-- A second, smaller violation of the same invariant goes with it. The
-- macro's promise is that a row's shares sum to the row, and a
-- mortgage outflow that retired no principal AND is itself under half
-- a cent satisfied neither share's admission test, so it was emitted
-- as no line at all. The interest share now admits a row no principal
-- line covers, whatever its size. Both defects are latent on a ledger
-- of ordinary repayments and neither changes a figure there.
--
-- Carried forward whole from 0093; the group and label arms and that
-- one admission test are the only changes.
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
               (n.class = 'mortgage' AND n.net_amount < 0) AS splits,
               -- And a mortgage line that does NOT split is money
               -- borrowed. Sign alone, so the class has no row left
               -- whose group repeats it: a zero-amount mortgage row
               -- draws no edge either way, and naming it here costs
               -- nothing and keeps the leaf rule structural.
               (n.class = 'mortgage' AND n.net_amount >= 0) AS borrowed
          FROM cashflow_txn_nodes(p_from, p_to) n
          JOIN cashflow_pool_accounts() p
                 ON p.silver_source_id    = n.silver_source_id
                AND p.account_external_id = n.account_external_id
          LEFT JOIN alloc a
                 ON a.silver_source_id        = n.silver_source_id
                AND a.transaction_external_id = n.transaction_external_id
         WHERE n.disposition = 'line'
    )
    SELECT l.* EXCLUDE (principal, splits, borrowed) REPLACE (
               CAST(CASE
                        WHEN NOT l.splits                                    THEN l.net_amount
                        -- The residue is under half a cent: one line
                        -- carrying the whole row, so the shares still
                        -- sum to it.
                        WHEN -l.net_amount - l.principal <= 0.005            THEN l.net_amount
                        WHEN sh.share = 'principal'                          THEN -l.principal
                        ELSE l.net_amount + l.principal
                    END AS DECIMAL(28,4)) AS net_amount,
               CASE WHEN l.borrowed            THEN 'mortgage_drawdown'
                    WHEN NOT l.splits          THEN l.grp
                    WHEN sh.share = 'principal' THEN 'mortgage_amortization'
                    ELSE 'mortgage_interest' END AS grp,
               CASE WHEN l.borrowed            THEN 'Mortgage drawdown'
                    WHEN NOT l.splits          THEN l.group_label
                    WHEN sh.share = 'principal' THEN 'Mortgage amortization'
                    ELSE 'Mortgage interest' END AS group_label)
      FROM line l
      JOIN (VALUES ('principal'), ('interest')) AS sh(share)
        ON (NOT l.splits AND sh.share = 'interest')
        OR (l.splits AND sh.share = 'principal' AND l.principal > 0)
        -- The remainder, or the whole row where no principal line
        -- covers it: a sub-cent outflow that retired nothing is still
        -- a line, and dropping it is the one way these shares could
        -- fail to sum to their row.
        OR (l.splits AND sh.share = 'interest'
            AND (-l.net_amount - l.principal > 0.005 OR l.principal <= 0))
     ORDER BY l.occurred_at, l.silver_source_id, l.transaction_external_id,
              CASE WHEN l.splits AND sh.share = 'principal' THEN 0 ELSE 1 END
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (96, CAST(epoch(now()) AS BIGINT));
