-- ============================================================
-- gold schema, migration 0095 —
--   the residual is savings, not a fistful of notes.
--
-- The cash section is the statement's residual: the four sections
-- summed and negated, so cash the household kept is cash the pool
-- absorbed. Drawn as `Cash` it read as physical money — and the
-- diagram already has a node for that, `Cash withdrawal`, which is a
-- consumption leaf. Two nodes, one word, and the one that meant
-- "what was left over" lost.
--
-- `Cash savings` says what it is in both directions: the household put
-- money aside, or drew on what it had put aside. It sits beside
-- `Retirement savings` and `Education savings`, which 0087 named for
-- the same reason — a class is named for the ACT, not for the asset.
--
-- The class KEY is untouched: `cash` is what the section, the rank and
-- every arm of the resolution speak, and only the label a reader sees
-- changes. Carried forward whole from 0090; the one arm is the only
-- change.
-- ============================================================

CREATE OR REPLACE MACRO cashflow_class_label(p_class) AS (
    CASE p_class
        -- Operating in: money from labour, from assets, from
        -- entitlements, and everything else that arrived.
        WHEN 'earnings'       THEN 'Earnings'
        WHEN 'yield'          THEN 'Yield'
        WHEN 'benefits'       THEN 'Pensions & benefits'
        WHEN 'other_receipts' THEN 'Other receipts'
        -- Operating out, with the three classes every household-balance
        -- diagram lifts out of spending.
        WHEN 'consumption'    THEN 'Consumption'
        WHEN 'fees'           THEN 'Fees'
        WHEN 'taxes'          THEN 'Taxes'
        WHEN 'giving'         THEN 'Giving'
        -- The backlog is a node on each side, not a leaf hidden inside
        -- one: both families made it a visible label on purpose.
        WHEN '(uncategorized)' THEN 'Uncategorised'
        -- Investing: the whole under the default grain, the rows with
        -- no instrument, then the asset classes.
        WHEN 'investments'       THEN 'Investments'
        WHEN 'elsewhere'         THEN 'Untracked investments'
        WHEN 'public_equity'     THEN 'Public equity'
        WHEN 'private_equity'    THEN 'Private markets'
        WHEN 'fixed_income'      THEN 'Fixed income'
        WHEN 'private_debt'      THEN 'Private debt'
        WHEN 'real_estate'       THEN 'Real estate'
        WHEN 'infrastructure'    THEN 'Infrastructure'
        WHEN 'metal'             THEN 'Metals'
        WHEN 'crypto'            THEN 'Crypto'
        WHEN 'hedge_fund'        THEN 'Hedge funds'
        WHEN 'multi_asset'       THEN 'Multi-asset'
        WHEN 'foreign_exchange'  THEN 'Foreign exchange'
        WHEN 'other'             THEN 'Other'
        -- Financing, the vehicles, and the residual.
        WHEN 'mortgage'   THEN 'Mortgage'
        WHEN 'loans'      THEN 'Loans'
        WHEN 'retirement' THEN 'Retirement savings'
        WHEN 'education'  THEN 'Education savings'
        WHEN 'health'     THEN 'Health'
        WHEN 'trusts'     THEN 'Trusts'
        WHEN 'deposits'   THEN 'Bank deposits'
        WHEN 'untracked'  THEN 'Untracked accounts'
        WHEN 'cash'       THEN 'Cash savings'
        ELSE p_class
    END
);


-- THE LABEL WAS IN THREE PLACES, which is why one of them was going to
-- be wrong. `cashflow_class_label` is the macro that names a class, and
-- these two reports each carried their own copy of the residual's name
-- as a string literal — so re-issuing the label macro alone renamed the
-- node on the statement and left it unchanged on the diagram and the
-- flows. Both now ASK, and a fourth copy cannot be added by forgetting
-- one. Carried forward whole from 0091 and 0082; the label expression
-- is the only change in each.

CREATE OR REPLACE MACRO report_cashflow_sankey(p_from, p_to, p_ccy, p_level, p_investing) AS TABLE (
    WITH v AS (SELECT * FROM cashflow_nodes_outccy(p_from, p_to, p_ccy, p_investing)),
    -- The class column of every section, plus the residual.
    cls AS (
        SELECT section, node_class AS class, node_class_label AS label,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net
          FROM v GROUP BY 1, 2, 3
        UNION ALL
        SELECT 'cash', 'cash', cashflow_class_label('cash'),
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
CREATE OR REPLACE MACRO report_cashflow_flows(p_from, p_to, p_ccy, p_period, p_level, p_investing) AS TABLE (
    WITH v AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               section,
               CASE WHEN p_level = 'section' THEN NULL ELSE node_class END AS class,
               CASE WHEN p_level = 'section' THEN NULL ELSE node_class_label END AS class_label,
               CASE WHEN p_level = 'group' THEN node_group END AS grp,
               CASE WHEN p_level = 'group' THEN node_group_label END AS group_label,
               value_outccy
          FROM cashflow_nodes_outccy(p_from, p_to, p_ccy, p_investing)),
    agg AS (
        SELECT period_start, section, class, class_label, grp, group_label,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               CAST(SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS DECIMAL(28,4)) AS inflow_val,
               CAST(SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS DECIMAL(28,4)) AS outflow_val,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net_val
          FROM v
         GROUP BY 1, 2, 3, 4, 5, 6),
    -- One cash node per bucket, whatever the level: the residual has
    -- one class and one group as it has one node.
    cash AS (
        SELECT t.period_start, 'cash' AS section,
               CASE WHEN p_level = 'section' THEN NULL ELSE 'cash' END AS class,
               CASE WHEN p_level = 'section' THEN NULL
                    ELSE cashflow_class_label('cash') END AS class_label,
               CASE WHEN p_level = 'group' THEN 'cash' END AS grp,
               CASE WHEN p_level = 'group'
                    THEN cashflow_class_label('cash') END AS group_label,
               CAST(0 AS BIGINT) AS txn_count, CAST(1 AS BIGINT) AS n_converted,
               CAST(NULL AS DECIMAL(28,4)) AS inflow_val,
               CAST(NULL AS DECIMAL(28,4)) AS outflow_val,
               CAST(-t.total AS DECIMAL(28,4)) AS net_val
          FROM (SELECT period_start, SUM(net_val) AS total FROM agg GROUP BY 1) t),
    rows_all AS (SELECT * FROM agg UNION ALL SELECT * FROM cash)
    SELECT period_start, section, class, class_label, grp, group_label, txn_count,
           CAST(CASE WHEN n_converted = 0 THEN NULL ELSE inflow_val  END AS VARCHAR) AS inflow,
           CAST(CASE WHEN n_converted = 0 THEN NULL ELSE outflow_val END AS VARCHAR) AS outflow,
           CAST(CASE WHEN n_converted = 0 THEN NULL ELSE net_val     END AS VARCHAR) AS net,
           ABS(net_val)::DOUBLE
               / NULLIF(SUM(CASE WHEN net_val > 0 THEN net_val ELSE 0 END)
                            OVER (PARTITION BY period_start), 0) AS share
      FROM rows_all
     ORDER BY period_start,
              cashflow_section_rank(section),
              cashflow_class_rank(class), ABS(net_val) DESC, class, grp
);
INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (95, CAST(epoch(now()) AS BIGINT));
