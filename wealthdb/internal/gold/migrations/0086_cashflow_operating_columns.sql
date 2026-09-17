-- The summary's two operating columns get the vocabulary the rest of
-- the feature uses, and the memo gets the fourth term that makes its
-- printed row close.
--
-- 0083 is applied, so both macros are carried forward WHOLE and
-- re-issued here rather than edited in place.
--
--   1. `income` AND `spending` BECOME `operating_in` AND
--      `operating_out`. Those two words named the same numbers as the
--      income and spending features while differing from them by
--      construction (docs/CASHFLOW.md §2), and a column a reader
--      cannot safely compare to the report it is named after is a trap
--      on the command line, in `-C`, and in the JSON keys. `operating`
--      — their signed difference — keeps its name, and the flows view
--      already spells the two halves this way.
--
--   2. THE MEMO PUBLISHES ITS FLOW TERM, `cash_flow_measured`. The
--      memo subtracts the flow over the measured slice (each account
--      from its own first snapshot, pool-internal moves included), not
--      the statement's `net_cash_flow`, so a reader who did the
--      arithmetic across the four printed columns got a residual that
--      was neither zero nor `unexplained`. With the term printed the
--      row closes on its face:
--
--          unexplained = cash_measured - fx_effect - cash_flow_measured
--
--      It is appended in READING order, between `fx_effect` and
--      `unexplained`, in both macros. Both projections are positional
--      contracts — gold.CashflowSummaryRow scans the summary with a
--      positional list — so the struct, its scan and the column
--      registry move with this file.
--
-- IF NOT EXISTS / OR REPLACE throughout keep this replayable for the
-- DDL-rerun test.

-- ============================================================
-- report_cashflow_reconciliation: the memo trio, per bucket.
--
-- EACH ACCOUNT IS MEASURED FROM ITS OWN FIRST OBSERVATION, and that is
-- where this departs from a simpler reading of the rule above. A
-- deployment's accounts do not all start together: a collector begins,
-- back-loads years of transaction history, and has balances only from
-- the day it first ran. A bucket blanked because ANY pooled account was
-- unobserved at its boundary is a bucket blanked for every year before
-- the last collector was added — which is every year a household would
-- want to read.
--
-- So an account joins the memo at its first snapshot and not before:
-- its opening boundary is the later of the bucket's and its own, and
-- the lines it contributed before that are outside the comparison. What
-- is left is a like-for-like subtraction over the slice that can
-- actually be measured, which is the whole point of the memo. A bucket
-- in which NOTHING is measurable reports blank.
-- ============================================================
CREATE OR REPLACE MACRO report_cashflow_reconciliation(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH pool AS (SELECT * FROM cashflow_balance_pool()),
    -- The pinned balance, per (account, currency, snapshot). No
    -- non-zero filter: a balance that went to zero is a fact.
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
    first_obs AS (
        SELECT silver_source_id, account_external_id,
               MIN(snapshot_at) // 86400 AS first_day
          FROM chosen GROUP BY 1, 2),
    -- WHAT MOVES THE SPINE. The flow side is every row on a measured
    -- account that the resolution did NOT decline — its lines, and the
    -- movements it calls pool-internal.
    --
    -- The internal ones belong here even though the statement never
    -- draws them, and leaving them out is what a first reading gets
    -- wrong. The spine is narrower than the household: a wire to a
    -- pooled account whose balances are not collected leaves the
    -- MEASURED pool while staying inside the household's, and a
    -- money-market fund bought with idle cash leaves the cash balances
    -- for a position. Both move the spine and neither is a line. Where
    -- both ends of a move are measured, both legs are here and they net
    -- to zero, which is what makes one rule serve every case.
    --
    -- What is deliberately NOT here is the rows the resolution
    -- DECLINED: the FX family, corporate actions, an unpaired card bill
    -- or in-kind leg, the catch-all kinds. Each moves the spine with
    -- nothing to explain it, and showing up in `unexplained` is exactly
    -- what the memo is for.
    --
    -- The flow side is also restricted to the same accounts and the
    -- same slice as the balance side. A diagnostic whose two halves
    -- covered different populations would carry a structural term for
    -- every account it cannot measure, and would stop saying anything
    -- about whether the statement's account of the cash is complete.
    raw_lines AS (
        SELECT spend_period_bucket(p_period, n.occurred_at) AS period_start,
               n.silver_source_id, n.account_external_id,
               n.occurred_at // 86400 AS day, n.currency, n.net_amount
          FROM cashflow_txn_nodes(p_from, p_to) n
          JOIN pool p ON p.silver_source_id    = n.silver_source_id
                     AND p.account_external_id = n.account_external_id
         WHERE n.disposition IN ('line', 'internal')),
    lines AS (
        SELECT r.*,
               CAST(COALESCE(CASE WHEN r.currency = p_ccy THEN r.net_amount::DOUBLE END,
                   r.net_amount::DOUBLE * d0.rate, r.net_amount::DOUBLE * c1.rate * c2.rate,
                   r.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
          FROM raw_lines r
          ASOF LEFT JOIN fx_daily d0 ON d0.from_ccy = r.currency AND d0.to_ccy = p_ccy AND d0.day <= r.day
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = r.currency AND c1.to_ccy = 'CHF' AND c1.day <= r.day
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= r.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = r.currency AND u1.to_ccy = 'USD' AND u1.day <= r.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= r.day),
    -- The buckets the STATEMENT reports, so the memo sits beside it
    -- rather than inventing a bucket of its own.
    buckets AS (
        SELECT DISTINCT spend_period_bucket(p_period, occurred_at) AS period_start
          FROM cashflow_lines_base(p_from, p_to)),
    bday AS (
        SELECT period_start,
               (GREATEST(COALESCE(period_start, p_from), p_from) - 1) // 86400 AS open_day,
               cashflow_period_end(p_period, period_start, p_to) // 86400 AS close_day
          FROM buckets),
    -- An account's own opening boundary: the later of the bucket's and
    -- its first observation. An account whose balances have not started
    -- by the bucket's end is not in it at all.
    aopen AS (
        SELECT b.period_start, f.silver_source_id, f.account_external_id,
               GREATEST(b.open_day, f.first_day) AS open_day, b.close_day
          FROM bday b JOIN first_obs f ON f.first_day <= b.close_day),
    acur AS (SELECT DISTINCT silver_source_id, account_external_id, currency FROM chosen),
    edges AS (
        SELECT a.period_start, 'open' AS side, a.open_day AS day,
               a.silver_source_id, a.account_external_id
          FROM aopen a
        UNION ALL
        SELECT a.period_start, 'close', a.close_day,
               a.silver_source_id, a.account_external_id
          FROM aopen a),
    -- One balance and one rate per (bucket, boundary, account,
    -- currency). The rate is triangulated the way every other report
    -- triangulates it.
    valued AS (
        SELECT g.period_start, g.side, g.silver_source_id, g.account_external_id, g.currency,
               COALESCE(c.amount, 0) AS amount,
               COALESCE(CASE WHEN g.currency = p_ccy THEN 1.0 END,
                        d0.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
          FROM (SELECT e.period_start, e.side, e.day,
                       e.silver_source_id, e.account_external_id, u.currency
                  FROM edges e
                  JOIN acur u ON u.silver_source_id    = e.silver_source_id
                             AND u.account_external_id = e.account_external_id) g
          ASOF LEFT JOIN chosen c
                 ON c.silver_source_id    = g.silver_source_id
                AND c.account_external_id = g.account_external_id
                AND c.currency            = g.currency
                AND c.snapshot_at        <= g.day * 86400 + 86399
          ASOF LEFT JOIN fx_daily d0 ON d0.from_ccy = g.currency AND d0.to_ccy = p_ccy AND d0.day <= g.day
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.currency AND c1.to_ccy = 'CHF' AND c1.day <= g.day
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= g.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.currency AND u1.to_ccy = 'USD' AND u1.day <= g.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= g.day),
    paired AS (
        SELECT period_start, silver_source_id, account_external_id, currency,
               MAX(CASE WHEN side = 'open'  THEN amount END) AS open_amount,
               MAX(CASE WHEN side = 'close' THEN amount END) AS close_amount,
               MAX(CASE WHEN side = 'open'  THEN rate   END) AS open_rate,
               MAX(CASE WHEN side = 'close' THEN rate   END) AS close_rate
          FROM valued GROUP BY 1, 2, 3, 4),
    balance_terms AS (
        SELECT period_start,
               SUM(close_amount::DOUBLE * close_rate) - SUM(open_amount::DOUBLE * open_rate) AS cash_measured,
               SUM(open_amount::DOUBLE * (close_rate - open_rate)) AS fx_opening,
               COUNT(*) FILTER (WHERE open_rate IS NULL OR close_rate IS NULL) AS unrated
          FROM paired GROUP BY 1),
    -- The bucket's closing rate per currency: the day is the bucket's
    -- for every account, so one rate serves them all.
    close_rates AS (
        SELECT g.period_start, g.currency,
               COALESCE(CASE WHEN g.currency = p_ccy THEN 1.0 END,
                        d0.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
          FROM (SELECT b.period_start, b.close_day AS day, c.currency
                  FROM bday b
                  CROSS JOIN (SELECT DISTINCT currency FROM lines
                              UNION SELECT DISTINCT currency FROM chosen) c) g
          ASOF LEFT JOIN fx_daily d0 ON d0.from_ccy = g.currency AND d0.to_ccy = p_ccy AND d0.day <= g.day
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.currency AND c1.to_ccy = 'CHF' AND c1.day <= g.day
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= g.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.currency AND u1.to_ccy = 'USD' AND u1.day <= g.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= g.day),
    -- The lines inside each account's own measured slice, revalued from
    -- their own day's rate to the bucket's closing one.
    flow_terms AS (
        SELECT l.period_start,
               SUM(l.net_amount::DOUBLE * r.rate) - SUM(l.value_outccy::DOUBLE) AS fx_lines,
               SUM(l.value_outccy::DOUBLE) AS net_flow,
               COUNT(*) FILTER (WHERE l.value_outccy IS NULL OR r.rate IS NULL) AS unrated
          FROM lines l
          JOIN aopen a ON a.period_start IS NOT DISTINCT FROM l.period_start
                      AND a.silver_source_id    = l.silver_source_id
                      AND a.account_external_id = l.account_external_id
                      AND l.day > a.open_day
          JOIN close_rates r ON r.period_start IS NOT DISTINCT FROM l.period_start
                            AND r.currency = l.currency
         GROUP BY 1),
    memo AS (
        SELECT b.period_start,
               b.cash_measured,
               b.fx_opening + COALESCE(f.fx_lines, 0) AS fx_effect,
               -- The flow term the residual is taken against, published
               -- rather than left implicit: it is the flow over the
               -- MEASURED slice, internal moves included, and so is not
               -- the statement's net_cash_flow. Without it the four
               -- printed columns do not close and a reader doing the
               -- arithmetic finds a residual that is neither zero nor
               -- `unexplained` (docs/CASHFLOW.md §8).
               COALESCE(f.net_flow, 0) AS cash_flow_measured,
               b.cash_measured - (b.fx_opening + COALESCE(f.fx_lines, 0))
                   - COALESCE(f.net_flow, 0) AS unexplained,
               -- Blank rather than print a figure no rate could value.
               (b.unrated = 0 AND COALESCE(f.unrated, 0) = 0) AS usable
          FROM balance_terms b
          LEFT JOIN flow_terms f ON f.period_start IS NOT DISTINCT FROM b.period_start)
    SELECT period_start,
           CAST(CASE WHEN usable THEN CAST(cash_measured      AS DECIMAL(28,4)) END AS VARCHAR) AS cash_measured,
           CAST(CASE WHEN usable THEN CAST(fx_effect          AS DECIMAL(28,4)) END AS VARCHAR) AS fx_effect,
           CAST(CASE WHEN usable THEN CAST(cash_flow_measured AS DECIMAL(28,4)) END AS VARCHAR) AS cash_flow_measured,
           CAST(CASE WHEN usable THEN CAST(unexplained        AS DECIMAL(28,4)) END AS VARCHAR) AS unexplained
      FROM memo
     ORDER BY 1
);

-- ============================================================
-- report_cashflow_summary gains the memo trio.
--
-- 0082's body carried forward whole, with the three columns joined on
-- the bucket and appended after `savings_rate`. The projection order is
-- a contract: gold.CashflowSummaryRow scans this macro POSITIONALLY.
--
-- A LEFT join, not a FULL one, and the difference from income's
-- withholding memo is the point: the memo is computed FOR the buckets
-- the statement reports and has no population of its own, so there is
-- no bucket it could contribute that the statement does not already
-- have. IS NOT DISTINCT FROM, not `=`, because `--period total`
-- collapses the window into one bucket whose period_start is NULL on
-- both sides.
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
    SELECT v.period_start, v.txn_count,
           CAST(v.income_val    AS VARCHAR) AS operating_in,
           CAST(v.spending_val  AS VARCHAR) AS operating_out,
           CAST(v.income_val - v.spending_val AS VARCHAR) AS operating,
           CAST(v.investing_val AS VARCHAR) AS investing,
           CAST(v.financing_val AS VARCHAR) AS financing,
           CAST(v.vehicles_val  AS VARCHAR) AS vehicles,
           CAST(v.income_val - v.spending_val + v.investing_val + v.financing_val + v.vehicles_val
                AS VARCHAR) AS net_cash_flow,
           CAST(v.yield_val  AS VARCHAR) AS yield,
           CAST(v.taxes_val  AS VARCHAR) AS taxes,
           CAST(v.fees_val   AS VARCHAR) AS fees,
           CAST(v.giving_val AS VARCHAR) AS giving,
           -- Blank rather than infinite where nothing came in: a rate
           -- over zero income says nothing a reader can act on.
           (v.income_val - v.spending_val)::DOUBLE / NULLIF(v.income_val::DOUBLE, 0) AS savings_rate,
           m.cash_measured, m.fx_effect, m.cash_flow_measured, m.unexplained
      FROM vals v
      LEFT JOIN report_cashflow_reconciliation(p_from, p_to, p_ccy, p_period) m
             ON m.period_start IS NOT DISTINCT FROM v.period_start
     ORDER BY 1
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (86, CAST(epoch(now()) AS BIGINT));
