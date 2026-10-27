-- ============================================================
-- gold schema, migration 0163 — the memo sees a deposit stated as a
-- holding.
--
-- The reconciliation memo and the coverage report measure an account
-- by its cash balances, so an account whose source states none is
-- invisible to both. A deposit family read from statements states the
-- account's balance as a holding: a `cash` position of vehicle
-- `demand_deposit`, which net worth already counts. Restating it as a
-- cash balance would count it twice.
--
-- cashflow_cash_balances() is cash_balances plus, for an account with
-- no cash balance at all, its demand-deposit holdings, summed per
-- snapshot and currency, as its `current` balance. The guard is
-- account-wide on purpose: an account with any cash balance states its
-- cash there, and a deposit sleeve beside it is a holding. A snapshot
-- with no deposit holding carries the last balance forward, as a
-- statement that lost its table does. Positions and cash_balances are
-- unchanged; the three balance readers of cash flow read the macro
-- instead of the table: cashflow_balance_pool (from 0083),
-- report_cashflow_reconciliation (from 0086) and
-- report_cashflow_coverage (from 0161), each carried forward whole with
-- only that read changed.
--
-- Replayable: OR REPLACE throughout.
-- ============================================================

CREATE OR REPLACE MACRO cashflow_cash_balances() AS TABLE (
    SELECT silver_source_id, snapshot_at, account_external_id, currency,
           balance_kind, amount
      FROM cash_balances
    UNION ALL
    SELECT p.silver_source_id, p.snapshot_at, p.account_external_id, p.currency,
           'current' AS balance_kind, SUM(p.market_value) AS amount
      FROM positions p
     WHERE p.asset_class = 'cash' AND p.vehicle = 'demand_deposit'
       AND p.market_value IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM cash_balances c
                        WHERE c.silver_source_id    = p.silver_source_id
                          AND c.account_external_id = p.account_external_id)
     GROUP BY 1, 2, 3, 4
);

CREATE OR REPLACE MACRO cashflow_balance_pool() AS TABLE (
    SELECT p.silver_source_id, p.account_external_id
      FROM cashflow_pool_accounts() p
     WHERE p.account_kind IS DISTINCT FROM 'overlay'
       AND EXISTS (SELECT 1 FROM transactions t
                    WHERE t.silver_source_id    = p.silver_source_id
                      AND t.account_external_id = p.account_external_id)
       AND EXISTS (SELECT 1 FROM cashflow_cash_balances() c
                    WHERE c.silver_source_id    = p.silver_source_id
                      AND c.account_external_id = p.account_external_id)
);

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
                  FROM cashflow_cash_balances() cb
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

CREATE OR REPLACE MACRO report_cashflow_coverage(p_from, p_to, p_period) AS TABLE (
    WITH pool AS (
        SELECT silver_source_id, account_external_id, account_kind, nickname,
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
                  FROM cashflow_cash_balances() cb
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
    -- for by nobody. A cash leg its source states signed
    -- (cash_signed_kinds, 0161) moves cash and is in the ledger.
    unsigned AS (
        SELECT t.*,
               (t.kind IN ('fx','fx_forward','fx_swap','corporate_action','journal','other')
                AND NOT (ck.kind IS NOT NULL AND COALESCE(t.quantity, 0) = 0
                         AND t.net_amount <> 0))
               OR (t.kind IN ('transfer_in','transfer_out')
                   AND t.instrument_external_id IS NOT NULL) AS no_cash
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
          LEFT JOIN silver_sources ss ON ss.silver_source_id = t.silver_source_id
          LEFT JOIN cash_signed_kinds ck
                 ON ck.silver_kind = ss.silver_kind AND ck.kind = t.kind
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
    SELECT b.period_start, p.silver_source_id, p.account, p.nickname, p.account_kind, b.currency,
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
                WHEN COALESCE(l.unsigned_volume, 0) > 0
                     AND abs(COALESCE(l.ledger, 0) - (b.close_amt - b.open_amt))
                         <= l.unsigned_volume THEN 'obscured'
                ELSE 'measured' END AS status
      FROM bal b
      JOIN pool p ON p.silver_source_id    = b.silver_source_id
                 AND p.account_external_id = b.account_external_id
      LEFT JOIN led l ON l.period_start IS NOT DISTINCT FROM b.period_start
                     AND l.silver_source_id    = b.silver_source_id
                     AND l.account_external_id = b.account_external_id
                     AND l.currency            = b.currency
     WHERE COALESCE(l.txn_count, 0) > 0 OR b.close_amt IS NOT NULL
     ORDER BY b.period_start, abs(COALESCE(COALESCE(l.ledger,0) - (b.close_amt - b.open_amt), 0)) DESC,
              b.silver_source_id, p.account, b.account_external_id, b.currency
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (163, CAST(epoch(now()) AS BIGINT));
