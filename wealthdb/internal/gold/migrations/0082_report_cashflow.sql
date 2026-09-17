-- The four cash flow reports, and the three cashflow columns on
-- `wealthdb transactions`.
--
-- Migration 0042 and 0071 are the two this diffs against: same FX pair,
-- same period bucketing, same n_converted guard, same
-- positive-magnitude idiom. What is new is the netting — a node is a
-- net, the level decides what nets, and the sign decides which side of
-- the diagram it is drawn on — and the edge list that netting makes
-- possible.
--
-- The FX blocks are duplicated rather than shared because a DuckDB
-- table macro cannot take a table as a parameter, which is the same
-- reason 0042 duplicates them across its own pair.
--
-- THE SIGN RULE, once, for every report below: positive is cash
-- arriving in the pool and negative is cash leaving it. Magnitude
-- columns (`income`, `spending`, `inflow`, `outflow`) are positive, as
-- the families' spend and refund columns are; `net` and the section
-- columns are signed.
--
-- IF NOT EXISTS / OR REPLACE throughout keep this replayable for the
-- DDL-rerun test.

-- ============================================================
-- The FX pair: the base with its amounts converted.
-- ============================================================

CREATE OR REPLACE MACRO cashflow_lines_outccy(p_from, p_to, p_ccy) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
      FROM cashflow_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = b.currency AND d.to_ccy  = p_ccy AND d.day  <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
);

CREATE OR REPLACE MACRO cashflow_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = 'USD' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_usd.rate,
               b.net_amount::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
           CAST(COALESCE(CASE WHEN b.currency = 'CHF' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_chf.rate,
               b.net_amount::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
           CAST(COALESCE(CASE WHEN b.currency = 'EUR' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_eur.rate,
               b.net_amount::DOUBLE * p_chf.rate * chf_eur.rate,
               b.net_amount::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
      FROM cashflow_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF'      AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF'      AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD'      AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD'      AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.occurred_at // 86400)
);

-- ============================================================
-- cashflow_class_rank: the order a section's classes are printed in.
--
-- The vocabulary's own order, not the data's. A reader comparing two
-- windows should find Taxes in the same place in both, and a reader
-- learning the statement should meet the classes in the order the
-- documentation introduces them. Only the asset classes fall back to
-- size, because the investing section's vocabulary IS the instrument
-- taxonomy and has no narrative order to borrow.
-- ============================================================
CREATE OR REPLACE MACRO cashflow_class_rank(p_class) AS (
    CASE p_class
        WHEN 'earnings'        THEN 1
        WHEN 'yield'           THEN 2
        WHEN 'benefits'        THEN 3
        WHEN 'other_receipts'  THEN 4
        WHEN 'consumption'     THEN 1
        WHEN 'fees'            THEN 2
        WHEN 'taxes'           THEN 3
        WHEN 'giving'          THEN 4
        WHEN '(uncategorized)' THEN 9
        WHEN 'investments'     THEN 1
        WHEN 'elsewhere'       THEN 8
        WHEN 'mortgage'        THEN 1
        WHEN 'loans'           THEN 2
        WHEN 'retirement'      THEN 1
        WHEN 'education'       THEN 2
        WHEN 'health'          THEN 3
        WHEN 'trusts'          THEN 4
        WHEN 'untracked'       THEN 5
        WHEN 'cash'            THEN 1
        ELSE 5  -- an asset class: ordered by size within the section
    END
);

CREATE OR REPLACE MACRO cashflow_section_rank(p_section) AS (
    CASE p_section
        WHEN 'operating_in'  THEN 1
        WHEN 'operating_out' THEN 2
        WHEN 'investing'     THEN 3
        WHEN 'financing'     THEN 4
        WHEN 'vehicles'      THEN 5
        WHEN 'cash'          THEN 6
    END
);

-- ============================================================
-- cashflow_nodes_outccy: the lines, converted, with the investing
-- grain applied.
--
-- The one place `--investing` is honoured. Under `whole`, the default,
-- every asset class folds into one node and the groups fold with it:
-- a class on the other side cannot be drawn as a child of a node on
-- this one, so "as a whole" means as a whole. Under `class` the fold is
-- a no-op and the section opens into one node per asset class, which is
-- the step in — it shows the reallocation between classes that the
-- whole nets away.
--
-- Shared by the flows view and the diagram, which differ in how they
-- aggregate it and not in what they aggregate.
-- ============================================================
CREATE OR REPLACE MACRO cashflow_nodes_outccy(p_from, p_to, p_ccy, p_investing) AS TABLE (
    WITH folded AS (
        SELECT b.*,
               CASE WHEN b.section = 'investing' AND p_investing = 'whole'
                    THEN 'investments' ELSE b.class END AS node_class,
               CASE WHEN b.section = 'investing' AND p_investing = 'whole'
                    THEN 'investments' ELSE b.grp END AS node_group
          FROM cashflow_lines_outccy(p_from, p_to, p_ccy) b
    )
    SELECT f.* EXCLUDE (class_label, group_label),
           cashflow_class_label(f.node_class) AS node_class_label,
           CASE WHEN f.node_group = f.grp THEN f.group_label
                ELSE cashflow_class_label(f.node_group) END AS node_group_label
      FROM folded f
);

-- ============================================================
-- report_cashflow_summary: the statement, one row per bucket.
--
-- `income` and `spending` are magnitudes — the two halves of operating,
-- each summed over its own section — and `operating` is their signed
-- difference. `investing`, `financing` and `vehicles` are signed nets,
-- and `net_cash_flow` is the four summed, which is also the sum of
-- every line in the bucket. That identity is structural and therefore a
-- guard against arithmetic alone; the reconciliation that can catch a
-- wrong POPULATION is the memo trio.
--
-- A bucket where nothing converted reports NULL rather than zero — the
-- same n_converted guard the two families' summaries use, so a missing
-- FX rate reads as "not known" instead of "no cash moved".
--
-- The memo columns behind -C are the three lifted outflow classes and
-- the yield class, each a magnitude, plus the savings rate. `yield` is
-- what the household's assets produced without its labour.
-- ============================================================
CREATE OR REPLACE MACRO report_cashflow_summary(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN section = 'operating_in'  THEN value_outccy ELSE 0 END) AS income_raw,
               SUM(CASE WHEN section = 'operating_out' THEN value_outccy ELSE 0 END) AS spending_raw,
               SUM(CASE WHEN section = 'investing'     THEN value_outccy ELSE 0 END) AS investing_raw,
               SUM(CASE WHEN section = 'financing'     THEN value_outccy ELSE 0 END) AS financing_raw,
               SUM(CASE WHEN section = 'vehicles'      THEN value_outccy ELSE 0 END) AS vehicles_raw,
               SUM(CASE WHEN class   = 'yield'  THEN value_outccy ELSE 0 END) AS yield_raw,
               SUM(CASE WHEN class   = 'taxes'  THEN value_outccy ELSE 0 END) AS taxes_raw,
               SUM(CASE WHEN class   = 'fees'   THEN value_outccy ELSE 0 END) AS fees_raw,
               SUM(CASE WHEN class   = 'giving' THEN value_outccy ELSE 0 END) AS giving_raw
          FROM cashflow_lines_outccy(p_from, p_to, p_ccy)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  income_raw    END AS DECIMAL(28,4)) AS income_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -spending_raw  END AS DECIMAL(28,4)) AS spending_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  investing_raw END AS DECIMAL(28,4)) AS investing_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  financing_raw END AS DECIMAL(28,4)) AS financing_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  vehicles_raw  END AS DECIMAL(28,4)) AS vehicles_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  yield_raw     END AS DECIMAL(28,4)) AS yield_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -taxes_raw     END AS DECIMAL(28,4)) AS taxes_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -fees_raw      END AS DECIMAL(28,4)) AS fees_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -giving_raw    END AS DECIMAL(28,4)) AS giving_val
          FROM agg)
    SELECT period_start, txn_count,
           CAST(income_val    AS VARCHAR) AS income,
           CAST(spending_val  AS VARCHAR) AS spending,
           CAST(income_val - spending_val AS VARCHAR) AS operating,
           CAST(investing_val AS VARCHAR) AS investing,
           CAST(financing_val AS VARCHAR) AS financing,
           CAST(vehicles_val  AS VARCHAR) AS vehicles,
           CAST(income_val - spending_val + investing_val + financing_val + vehicles_val
                AS VARCHAR) AS net_cash_flow,
           CAST(yield_val  AS VARCHAR) AS yield,
           CAST(taxes_val  AS VARCHAR) AS taxes,
           CAST(fees_val   AS VARCHAR) AS fees,
           CAST(giving_val AS VARCHAR) AS giving,
           -- Blank rather than infinite where nothing came in: a rate
           -- over zero income says nothing a reader can act on.
           (income_val - spending_val)::DOUBLE / NULLIF(income_val::DOUBLE, 0) AS savings_rate
      FROM vals
     ORDER BY 1
);

-- ============================================================
-- report_cashflow_flows: one row per (bucket, node) at --level.
--
-- The row's `net` is netted AT THAT LEVEL, which is the whole argument
-- of the design's netting section: a class with seven hundred thousand
-- of gross activity and one net figure is one node, not two gross
-- bands. `inflow` and `outflow` are the line-level gross magnitudes
-- under the node at every level and grain, so the churn the net hides
-- is one column away.
--
-- `share` is |net| over the bucket's HUB at the level drawn — the sum
-- of the positive nets there, which equals the sum of the negative ones
-- once cash is a node. A finer level can therefore have a larger hub
-- than a coarser one: two leaves of opposite sign inside one class net
-- away at the class level and both appear at the group level. That is
-- not an inconsistency, it is what netting means.
--
-- THE CASH ROW is synthesised, not summed: it is the residual seen from
-- the pool's side, so its net is the negative of net_cash_flow. Cash
-- the household kept is cash the pool absorbed, which is a use — and
-- that is what makes a bucket's rows sum to zero, which is the cash
-- flow statement closing.
-- ============================================================
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
               CASE WHEN p_level = 'section' THEN NULL ELSE 'Cash' END AS class_label,
               CASE WHEN p_level = 'group' THEN 'cash' END AS grp,
               CASE WHEN p_level = 'group' THEN 'Cash' END AS group_label,
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

-- ============================================================
-- report_cashflow_sankey: the window's diagram as an edge list.
--
-- Period-less by construction: a diagram is a window, not a series, and
-- the whole window is one bucket here. Four stages at --level group,
-- the two inner ones at --level class.
--
-- WHICH NODES GET A LEAF STAGE. The operating sections alone. The
-- design's diagram attaches investing, financing, the vehicles and cash
-- to the hub directly, at class grain, and `--investing` rather than
-- `--level` is what opens the investing section — so a level of `group`
-- opens the two operating sections and changes nothing else.
--
-- THE SIDE OF A NODE is the sign of its net, universally: a category
-- whose refunds exceeded its purchases really did supply cash that
-- period, and the diagram says so. One consequence: a leaf whose net
-- runs opposite to its class is attached to the HUB directly rather
-- than drawn as a child of a node on the other side, and its class's
-- edge carries only the leaves that stayed with it. Each stage still
-- conserves flow, and no node appears twice.
--
-- NODE NAMES must be unique across the edge list, because a Sankey
-- visualisation keys a node by the string it is called. Three values of
-- the shared vocabulary reach both operating sections — a gift, an
-- other, and an unplaced row — so a node whose key is one of them takes
-- its direction as a suffix. The test is read off the dimension's
-- `family` column rather than from a list here, so a delta added as
-- family 'both' is disambiguated the day it lands.
-- ============================================================
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
    -- The leaves, operating only.
    leaf AS (
        SELECT section, node_class AS class, node_group AS grp, node_group_label AS label,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net
          FROM v
         WHERE section IN ('operating_in', 'operating_out')
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
        SELECT node_id, node_name, section, net FROM named_cls
         WHERE p_level = 'class' OR section NOT IN ('operating_in', 'operating_out')
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

-- ============================================================
-- report_cashflow_transactions: a cashflow line, oldest first.
--
-- The family transactions views' shape with the node resolved. `name`
-- is the instrument on an investing line and the payer or merchant on
-- an operating line; a financing, vehicle or cash line names nothing,
-- because its counterparty is an ACCOUNT and no surface of this feature
-- prints an account as a name.
--
-- The projection order is a contract: gold.CashflowTransactionRow scans
-- this macro POSITIONALLY, so the macro, the row struct and the scan
-- list move together in one commit.
-- ============================================================
CREATE OR REPLACE MACRO report_cashflow_transactions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, section, class, class_label, grp, group_label, name,
           verdict, spend_detailed, income_detailed, provenance, currency,
           CAST(net_amount AS VARCHAR) AS net_amount,
           description, counterparty,
           CAST(value_outccy AS VARCHAR) AS value_outccy
      FROM cashflow_lines_outccy(p_from, p_to, p_ccy)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE OR REPLACE MACRO report_cashflow_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, section, class, class_label, grp, group_label, name,
           verdict, spend_detailed, income_detailed, provenance, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur
      FROM cashflow_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

-- ============================================================
-- report_transactions gains the cashflow trio.
--
-- 0075's body carried forward whole — the spending join, the income
-- join, the symbol resolutions, the check number, the FX block — with
-- cashflow_txn_nodes() joined beside the two overlays and its three
-- columns projected after `check_number`. The one surface that shows a
-- row from every side keeps doing so.
--
-- The node is read from the RESOLUTION rather than from the base, so a
-- row this feature declines or calls pool-internal still shows what it
-- was resolved as; a row on no account at all shows nothing, as it does
-- for the two families.
--
-- The projection order is a contract. gold.TransactionRow scans this
-- macro POSITIONALLY, so the new columns go at the end of the named
-- block and the row struct, the scan list and this macro move together.
-- ============================================================
CREATE OR REPLACE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed,
               ic.payer_name, ic.income_primary, ic.income_detailed,
               t.check_number,
               cf.section AS cashflow_section, cf.class AS cashflow_class, cf.grp AS cashflow_group
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
          LEFT JOIN income_txn_categories() ic ON ic.silver_source_id = t.silver_source_id
               AND ic.transaction_external_id = t.transaction_external_id
          LEFT JOIN cashflow_txn_nodes(p_from, p_to) cf ON cf.silver_source_id = t.silver_source_id
               AND cf.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           b.payer_name, b.income_primary, b.income_detailed,
           b.check_number,
           b.cashflow_section, b.cashflow_class, b.cashflow_group,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = p_ccy AND d.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (82, CAST(epoch(now()) AS BIGINT));
