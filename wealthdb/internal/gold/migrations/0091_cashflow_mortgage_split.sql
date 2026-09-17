-- ============================================================
-- gold schema, migration 0091 —
--   a mortgage instalment is interest AND principal, and the two are
--   not the same kind of thing.
--
-- `Mortgage` was a sink: one class, one group of the same name, and a
-- Sankey leaf stage that would have drawn a node pointing at itself.
-- So interest and principal repayment summed into one edge, and the
-- statement said only that money went to the bank.
--
-- They differ in kind. Interest is CONSUMED — it buys the use of the
-- money and is gone. Principal is not spending at all: it moves value
-- from one side of the balance sheet to the other, and the household is
-- no poorer for it. A statement that adds them together overstates what
-- was consumed by whatever was repaid.
--
-- THE SPLIT IS DERIVED, and it has to be. The bank does not print the
-- share, and the narrative cannot be made to yield it:
--
--   * a tranche may bundle its scheduled amortisation into the same row
--     as its interest, printing one figure and one booking type;
--   * a closing books the principal and the final interest as two rows
--     identical in every narrative field.
--
-- So the rule reads neither. Whatever a period retired of the
-- mortgage's own outstanding balance was principal, and the rest of
-- what was paid into it that period was interest. The balance is
-- OBSERVED, not inferred — it is the lender's own figure, carried as
-- the mortgage account's position — which is what makes this an
-- estimator of the split and not of the debt.
--
-- Within a period the principal goes to the largest instalment first,
-- capped at its own face value. That is the order the two really occur
-- in on a closing, and the only order that leaves the small row as the
-- interest it is.
--
-- WHAT IT DOES NOT DO. A mortgage the product does not hold has no
-- balance to read, so its instalments keep their whole amount and draw
-- as interest. That is the conservative direction: it overstates what
-- was consumed rather than inventing a repayment that never happened.
--
-- WHERE IT LIVES, and why not as a column on `cashflow_txn_nodes`. A
-- column there would have been read by `cashflow_lines_base`, and
-- replaying any earlier migration that re-issues the node macro — 0085
-- and 0088 both do, and the replay bar says re-running one must leave
-- gold working — would take the column away and break the base. A macro
-- depends only on macros and on columns old enough that no replay
-- removes them.
--
-- The node macro is therefore NOT re-issued: it still emits one row per
-- transaction, and the reconciliation memo, which reads it directly, is
-- untouched. The split happens in `cashflow_lines_base`, whose
-- consumers aggregate, and the two shares sum to the row.
-- ============================================================

-- The principal share of every mortgage instalment in the window.
-- Absent from this macro's output means "no mortgage balance to read",
-- which the base treats as all interest.
CREATE OR REPLACE MACRO cashflow_mortgage_principal(p_from, p_to) AS TABLE (
    WITH pay AS (
        SELECT n.silver_source_id, n.transaction_external_id, n.occurred_at,
               n.far_silver_source_id AS msrc, n.far_account_external_id AS macct,
               abs(n.net_amount) AS paid
          FROM cashflow_txn_nodes(p_from, p_to) n
         WHERE n.class = 'mortgage'
           AND n.far_silver_source_id    IS NOT NULL
           AND n.far_account_external_id IS NOT NULL
    ),
    -- Every observation of a mortgage's outstanding balance, with the
    -- next one. A snapshot is the balance BEFORE that day's instalment,
    -- so an interval runs from one observation up to (not including)
    -- the next, and what the balance lost across it is what the
    -- instalments inside it retired.
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
    -- What each interval retired. The last interval of a mortgage that
    -- has STOPPED being observed retired the rest of it: the loan was
    -- closed out. One still observed at its source's latest snapshot
    -- retired nothing, because the debt is still standing.
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
    )
    -- Capped at the row's own amount and at what is left of the
    -- period's decline, so the shares can neither exceed the payment
    -- nor invent principal the balance never lost.
    SELECT silver_source_id, transaction_external_id,
           CAST(greatest(0, least(paid, retired - COALESCE(
               SUM(paid) OVER (PARTITION BY msrc, macct, from_day ORDER BY rn
                               ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),
               0))) AS DECIMAL(28,4)) AS principal
      FROM ranked
);

-- cashflow_lines_base, carried forward from 0081. The one change is the
-- split: a mortgage instalment becomes up to two lines, and everything
-- else passes through as before.
--
-- A row whose mortgage retired no debt yields one line (all interest);
-- one that retired its whole face value yields one line (all
-- principal); only a genuinely mixed instalment yields two. The shares
-- sum to the row, so every total drawn from this macro is what it was.
CREATE OR REPLACE MACRO cashflow_lines_base(p_from, p_to) AS TABLE (
    WITH line AS (
        SELECT n.*, COALESCE(mp.principal, 0) AS principal
          FROM cashflow_txn_nodes(p_from, p_to) n
          JOIN cashflow_pool_accounts() p
                 ON p.silver_source_id    = n.silver_source_id
                AND p.account_external_id = n.account_external_id
          LEFT JOIN cashflow_mortgage_principal(p_from, p_to) mp
                 ON mp.silver_source_id        = n.silver_source_id
                AND mp.transaction_external_id = n.transaction_external_id
         WHERE n.disposition = 'line'
    ),
    split AS (
        SELECT * EXCLUDE (principal) FROM line WHERE class <> 'mortgage'
         UNION ALL
        -- The principal share: value moved across the balance sheet.
        SELECT * EXCLUDE (principal) REPLACE (
                   CAST(CASE WHEN net_amount < 0 THEN -principal ELSE principal END
                        AS DECIMAL(28,4)) AS net_amount,
                   'mortgage_amortization' AS grp,
                   'Mortgage amortization' AS group_label)
          FROM line
         WHERE class = 'mortgage' AND principal > 0
         UNION ALL
        -- The interest share: the price of the money, and consumed.
        SELECT * EXCLUDE (principal) REPLACE (
                   CAST(net_amount + CASE WHEN net_amount < 0 THEN principal ELSE -principal END
                        AS DECIMAL(28,4)) AS net_amount,
                   'mortgage_interest' AS grp,
                   'Mortgage interest' AS group_label)
          FROM line
         WHERE class = 'mortgage' AND abs(net_amount) - principal > 0.005
    )
    SELECT * FROM split
     ORDER BY occurred_at, silver_source_id, transaction_external_id, grp
);

CREATE OR REPLACE MACRO report_cashflow_sankey(p_from, p_to, p_ccy, p_level, p_investing) AS TABLE (
    WITH v AS (SELECT * FROM cashflow_nodes_outccy(p_from, p_to, p_ccy, p_investing)),
    -- The class column of every section, plus the residual.
    cls AS (
        SELECT section, node_class AS class, node_class_label AS label,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net
          FROM v GROUP BY 1, 2, 3
        UNION ALL
        SELECT 'cash', 'cash', 'Cash',
               CAST(-COALESCE(SUM(value_outccy), 0) AS DECIMAL(28,4)) FROM v),
    -- The self-edge rule is stated as itself now (`node_group <>
    -- node_class`) rather than left to a list of sections to imply. The
    -- list had been doing two jobs and the second was silent: cash and
    -- the vehicles have nothing finer to say, so excluding them BY
    -- SECTION looked equivalent to excluding them for repeating their
    -- class — but financing was excluded by that same list even once it
    -- HAS something finer to say, so no amount of splitting a mortgage
    -- would ever have drawn. The guards are separate now, and a section
    -- gains its leaf stage the day its groups stop repeating its
    -- classes.
    --
    -- Investing stays out deliberately. Its groups do not repeat its
    -- classes, so the self-edge rule alone would give it a leaf stage —
    -- but what that stage would say (a trade against a capital call) is
    -- a different question from this one.
    --
    -- The leaves: the operating sections and financing, less the backlog. The
    -- backlog class IS its own leaf — nothing placed those rows, so
    -- there is nothing finer to say about them — and a class drawn into
    -- a leaf of the same name is a self-edge, which a Sankey renders as
    -- a node pointing at itself. It attaches to the hub directly
    -- instead, the way cash and the vehicles do.
    leaf AS (
        SELECT section, node_class AS class, node_group AS grp, node_group_label AS label,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net
          FROM v
         WHERE section IN ('operating_in', 'operating_out', 'financing')
           AND node_class <> '(uncategorized)'
           AND node_group <> node_class
         GROUP BY 1, 2, 3, 4),
    -- A node's printable name, disambiguated where the same value
    -- reaches both operating sections.
    named_cls AS (
        SELECT c.*, CASE
                   WHEN c.section = 'operating_in'  AND (c.class = '(uncategorized)' OR sc.family = 'both')
                        THEN c.label || ' in'
                   WHEN c.section = 'operating_out' AND (c.class = '(uncategorized)' OR sc.family = 'both')
                        THEN c.label || ' out'
                   ELSE c.label END AS node_name,
               c.section || '.' || c.class AS node_id
          FROM cls c LEFT JOIN spend_categories sc ON sc.spend_detailed = c.class),
    named_leaf AS (
        SELECT l.*, CASE
                   WHEN l.section = 'operating_in'  AND (l.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN l.label || ' in'
                   WHEN l.section = 'operating_out' AND (l.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN l.label || ' out'
                   ELSE l.label END AS node_name,
               l.section || '.' || l.class || '.' || l.grp AS node_id
          FROM leaf l LEFT JOIN spend_categories sg ON sg.spend_detailed = l.grp),
    -- A leaf stays with its class when the two run the same way.
    attached AS (
        SELECT l.*, c.net AS class_net, c.node_name AS class_name, c.node_id AS class_id,
               ((l.net > 0 AND c.net > 0) OR (l.net < 0 AND c.net < 0)) AS stays
          FROM named_leaf l
          JOIN named_cls c ON c.section = l.section AND c.class = l.class),
    -- The atomic nodes: what the hub is summed over, and what attaches
    -- to it. At class grain every class is atomic; at group grain the
    -- operating classes give way to their leaves.
    atoms AS (
        -- A class enters the hub in its own right exactly when it has
        -- no leaves to enter through. That used to be spelled as a
        -- section list too, and the list stopped agreeing with the leaf
        -- rule the moment financing grew leaves: the class then entered
        -- BOTH here and as the sum of its leaves below, and the diagram
        -- drew the same edge twice. Asking `attached` directly cannot
        -- drift from it.
        SELECT c.node_id, c.node_name, c.section, c.net FROM named_cls c
         WHERE p_level = 'class'
            OR NOT EXISTS (SELECT 1 FROM attached a
                            WHERE a.section = c.section AND a.class = c.class)
        UNION ALL
        SELECT node_id, node_name, section,
               CASE WHEN stays THEN CAST(0 AS DECIMAL(28,4)) ELSE net END
          FROM attached WHERE p_level = 'group'
        UNION ALL
        -- A class whose leaves stayed with it enters the hub as the sum
        -- of those leaves, which is its net widened by whatever left.
        SELECT class_id, class_name, section,
               CAST(SUM(CASE WHEN stays THEN net ELSE 0 END) AS DECIMAL(28,4))
          FROM attached WHERE p_level = 'group'
         GROUP BY 1, 2, 3),
    hub AS (SELECT SUM(CASE WHEN net > 0 THEN net ELSE 0 END) AS total FROM atoms),
    edges AS (
        -- Stages 2 and 3: every atomic node, to or from the household.
        SELECT CASE WHEN a.net > 0 THEN 2 ELSE 3 END AS stage,
               CASE WHEN a.net > 0 THEN a.node_name ELSE 'Household' END AS source,
               CASE WHEN a.net > 0 THEN 'Household' ELSE a.node_name END AS target,
               CASE WHEN a.net > 0 THEN a.node_id ELSE 'household' END AS source_id,
               CASE WHEN a.net > 0 THEN 'household' ELSE a.node_id END AS target_id,
               a.section, ABS(a.net) AS value
          FROM atoms a WHERE a.net <> 0
        UNION ALL
        -- Stages 1 and 4: the operating leaves that stayed with their
        -- class, drawn outside it on whichever side the class sits.
        SELECT CASE WHEN a.class_net > 0 THEN 1 ELSE 4 END,
               CASE WHEN a.class_net > 0 THEN a.node_name ELSE a.class_name END,
               CASE WHEN a.class_net > 0 THEN a.class_name ELSE a.node_name END,
               CASE WHEN a.class_net > 0 THEN a.node_id ELSE a.class_id END,
               CASE WHEN a.class_net > 0 THEN a.class_id ELSE a.node_id END,
               a.section, ABS(a.net)
          FROM attached a WHERE p_level = 'group' AND a.stays AND a.net <> 0)
    SELECT e.stage, e.source, e.target, e.source_id, e.target_id, e.section,
           CAST(CAST(e.value AS DECIMAL(28,4)) AS VARCHAR) AS value,
           e.value::DOUBLE / NULLIF(h.total::DOUBLE, 0) AS share
      FROM edges e CROSS JOIN hub h
     ORDER BY e.stage, e.value DESC, e.source, e.target
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (91, CAST(epoch(now()) AS BIGINT));
