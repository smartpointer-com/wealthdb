-- ============================================================
-- gold schema, migration 0140 — a snapshot speaks only for the
-- accounts it names, at every instant.
--
-- The daily history resolves the active snapshot per key (migration
-- 0051): (source, account) for positions, (source, account, currency)
-- for cash. The point-in-time readers took a source's newest snapshot
-- at or before the instant as the whole source. A source whose runs
-- cover only some of its accounts then lost the others at every instant
-- a partial run was the newest: a statement whose month-end snapshot
-- names some accounts monthly and others only at quarter ends, a bank
-- whose card run and custody feed land under their own snapshot_at.
-- `holdings -d`, `gains`, `exposure`, `yield` and the web's
-- point-in-time tables read such an account as holding nothing, while
-- the history carried it.
--
-- Every point-in-time reader takes 0051's rule instead, at its instant
-- t, per key:
--
--   * the key's candidate is its newest observation at or before t;
--   * a candidate that is not the key's last observation stands;
--   * the key's last observation stands while t is before its ending
--     snapshot, the first later snapshot of its source that re-covers
--     its company (pos_key_ends / cash_key_ends), and while the
--     source's clock at t, its newest snapshot in that table at or
--     before t, is at most hist_carry_days() days past the
--     observation's day.
--
-- So each observation holds over a span (pos_key_spans /
-- cash_key_spans): from its own instant to the key's next observation,
-- the last one to the earlier of its ending snapshot and the source's
-- first snapshot past the horizon, else for ever. pos_active_at and
-- cash_active_at give the observation each key holds at each instant.
-- At a day's last second the rule is 0051's day rule, so a read at a
-- day's end equals the history's day, in value, for every source whose
-- lines all have a rate to the currency.
--
-- What follows for readers:
--
--   * positions_at, cash_chosen, account_lines_base,
--     portfolio_lines_base, source_lines_base, exposure_holdings and
--     report_positions_multi pick per key; each is its latest text
--     (0021 to 0126) with only the pick changed. Every reader of them
--     follows: report_positions and report_cash, report_accounts,
--     report_portfolios, report_sources and report_global with their
--     _multi forms, the gains and lot reports, exposure and yield. The
--     Go readers that pick a snapshot themselves (yield's daily amounts
--     and account grid, the wanted list's still-held flag) read the
--     same macros.
--   * The tail keeps 0051's slow-sibling shape: an account whose last
--     observation shares a run with a faster sibling ends at that
--     sibling's next run. A quarterly account of a statement source so
--     ends at the month-end after its last quarter end, in both reads.
--   * report_gains_check compares the stated basis at each account's
--     active snapshot at the window's end, where that falls in the
--     window.
--   * A point-in-time row's snapshot_at is its account's own active
--     snapshot, as report_positions_history's is. A source's
--     snapshot_at stays the newest active snapshot among its accounts,
--     and report_global's min_snapshot_at is the oldest account's.
--   * Zero cash rows are observations: a zero supersedes the balance
--     before it. cash_chosen still leaves zero balances out of its
--     output, after the pick.
--   * hist_active_pos and hist_active_cash read the shared endings;
--     their output is 0051's.
--   * cashflow_lines_base keeps its own reading of a mortgage's end: it
--     measures the principal retired between a mortgage's own
--     observations, and reads one its source outlived as repaid.
--
-- CREATE OR REPLACE throughout keeps this replayable (see
-- gold.Migrate's REPLAY note).
-- ============================================================

-- ---- the ending of a key's last observation (0051) ---------------------

-- pos_key_ends / cash_key_ends: each key whose last observation a later
-- snapshot of its source ends, and that snapshot's instant: the first
-- that covers every other key seen in the key's own last snapshot AND
-- still live at that snapshot. `covering` counts the peers a candidate
-- snapshot carries, `need` counts the peers still reporting at that
-- candidate (their own last observation is at or after it), and the
-- ending is the first candidate where the two agree. A peer that left
-- in the same run as the key is therefore not required. A key observed
-- alone has no peers and no ending here.
CREATE OR REPLACE MACRO pos_key_ends() AS TABLE (
    WITH obs AS MATERIALIZED (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, snapshot_at AS snap
          FROM positions),
    last_obs AS MATERIALIZED (SELECT src, acct, MAX(snap) AS snap FROM obs GROUP BY 1, 2),
    peers AS MATERIALIZED (
        SELECT l.src, l.acct, l.snap AS lsnap, o.acct AS peer, pl.snap AS peer_last
          FROM last_obs l
          JOIN obs o ON o.src = l.src AND o.snap = l.snap AND o.acct <> l.acct
          JOIN last_obs pl ON pl.src = l.src AND pl.acct = o.acct),
    covering AS MATERIALIZED (
        SELECT p.src, p.acct, o.snap AS cand, COUNT(*) AS covered
          FROM peers p JOIN obs o ON o.src = p.src AND o.acct = p.peer AND o.snap > p.lsnap
         GROUP BY 1, 2, 3),
    need AS (
        SELECT c.src, c.acct, c.cand, COUNT(*) AS n
          FROM covering c JOIN peers p
            ON p.src = c.src AND p.acct = c.acct AND p.peer_last >= c.cand
         GROUP BY 1, 2, 3)
    SELECT c.src, c.acct, MIN(c.cand) AS end_snap
      FROM covering c JOIN need n
        ON n.src = c.src AND n.acct = c.acct AND n.cand = c.cand AND n.n = c.covered
     GROUP BY 1, 2
);
CREATE OR REPLACE MACRO cash_key_ends() AS TABLE (
    WITH obs AS MATERIALIZED (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct,
               currency AS ccy, snapshot_at AS snap
          FROM cash_balances),
    last_obs AS MATERIALIZED (SELECT src, acct, ccy, MAX(snap) AS snap FROM obs GROUP BY 1, 2, 3),
    peers AS MATERIALIZED (
        SELECT l.src, l.acct, l.ccy, l.snap AS lsnap, o.acct AS peer_acct, o.ccy AS peer_ccy,
               pl.snap AS peer_last
          FROM last_obs l
          JOIN obs o ON o.src = l.src AND o.snap = l.snap
           AND (o.acct <> l.acct OR o.ccy <> l.ccy)
          JOIN last_obs pl ON pl.src = l.src AND pl.acct = o.acct AND pl.ccy = o.ccy),
    covering AS MATERIALIZED (
        SELECT p.src, p.acct, p.ccy, o.snap AS cand, COUNT(*) AS covered
          FROM peers p JOIN obs o ON o.src = p.src AND o.acct = p.peer_acct
               AND o.ccy = p.peer_ccy AND o.snap > p.lsnap
         GROUP BY 1, 2, 3, 4),
    need AS (
        SELECT c.src, c.acct, c.ccy, c.cand, COUNT(*) AS n
          FROM covering c JOIN peers p
            ON p.src = c.src AND p.acct = c.acct AND p.ccy = c.ccy AND p.peer_last >= c.cand
         GROUP BY 1, 2, 3, 4)
    SELECT c.src, c.acct, c.ccy, MIN(c.cand) AS end_snap
      FROM covering c JOIN need n
        ON n.src = c.src AND n.acct = c.acct AND n.ccy = c.ccy AND n.cand = c.cand
       AND n.n = c.covered
     GROUP BY 1, 2, 3
);

-- ---- the daily history on the shared endings (0051) -------------------

-- hist_active_pos / hist_active_cash: 0051's, the ending read from
-- pos_key_ends / cash_key_ends. The source's clock for the day is
-- MAX(active snapshot) over the day, which is the newest snapshot of the
-- source in that table at or before the day's end.
CREATE OR REPLACE MACRO hist_active_pos() AS TABLE (
    WITH obs AS (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, snapshot_at AS snap
          FROM positions),
    ends AS (SELECT src, acct, end_snap // 86400 AS end_day FROM pos_key_ends()),
    live AS (
        SELECT s.day, k.src, k.acct, ps.snap
          FROM hist_days() s
          CROSS JOIN (SELECT DISTINCT src, acct FROM obs) k
          ASOF JOIN obs ps
            ON ps.src = k.src AND ps.acct = k.acct AND ps.snap <= s.day * 86400 + 86399)
    SELECT l.day, l.src, l.acct, l.snap
      FROM live l LEFT JOIN ends e ON e.src = l.src AND e.acct = l.acct
    QUALIFY l.snap < MAX(l.snap) OVER (PARTITION BY l.src, l.acct)
         OR (COALESCE(l.day < e.end_day, TRUE)
             AND (MAX(l.snap) OVER (PARTITION BY l.day, l.src)) // 86400 - l.snap // 86400 <= hist_carry_days())
);
CREATE OR REPLACE MACRO hist_active_cash() AS TABLE (
    WITH obs AS (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct,
               currency AS ccy, snapshot_at AS snap
          FROM cash_balances),
    ends AS (SELECT src, acct, ccy, end_snap // 86400 AS end_day FROM cash_key_ends()),
    live AS (
        SELECT s.day, k.src, k.acct, k.ccy, cs.snap
          FROM hist_days() s
          CROSS JOIN (SELECT DISTINCT src, acct, ccy FROM obs) k
          ASOF JOIN obs cs
            ON cs.src = k.src AND cs.acct = k.acct AND cs.ccy = k.ccy
           AND cs.snap <= s.day * 86400 + 86399)
    SELECT l.day, l.src, l.acct, l.ccy, l.snap
      FROM live l LEFT JOIN ends e ON e.src = l.src AND e.acct = l.acct AND e.ccy = l.ccy
    QUALIFY l.snap < MAX(l.snap) OVER (PARTITION BY l.src, l.acct, l.ccy)
         OR (COALESCE(l.day < e.end_day, TRUE)
             AND (MAX(l.snap) OVER (PARTITION BY l.day, l.src)) // 86400 - l.snap // 86400 <= hist_carry_days())
);

-- ---- the span each observation holds over ------------------------------

-- pos_key_spans / cash_key_spans: every observation of every key, and
-- the instant it stops holding (until, exclusive): the key's next
-- observation; for its last, the earlier of its ending snapshot and the
-- source's first snapshot in that table on a day more than
-- hist_carry_days() past the observation's own; NULL while neither
-- exists. The clock reads the source's own snapshots, never the
-- calendar, so a source that stops running ends nothing.
CREATE OR REPLACE MACRO pos_key_spans() AS TABLE (
    WITH obs AS MATERIALIZED (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, snapshot_at AS snap
          FROM positions),
    clock AS (SELECT DISTINCT src, snap FROM obs),
    seq AS (
        SELECT src, acct, snap, LEAD(snap) OVER (PARTITION BY src, acct ORDER BY snap) AS next_snap
          FROM obs)
    SELECT q.src, q.acct, q.snap,
           CASE WHEN q.next_snap IS NOT NULL THEN q.next_snap ELSE LEAST(e.end_snap, h.snap) END AS until
      FROM seq q
      LEFT JOIN pos_key_ends() e ON e.src = q.src AND e.acct = q.acct
      ASOF LEFT JOIN clock h ON h.src = q.src
           AND h.snap >= (q.snap // 86400 + hist_carry_days() + 1) * 86400
);
CREATE OR REPLACE MACRO cash_key_spans() AS TABLE (
    WITH obs AS MATERIALIZED (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct,
               currency AS ccy, snapshot_at AS snap
          FROM cash_balances),
    clock AS (SELECT DISTINCT src, snap FROM obs),
    seq AS (
        SELECT src, acct, ccy, snap,
               LEAD(snap) OVER (PARTITION BY src, acct, ccy ORDER BY snap) AS next_snap
          FROM obs)
    SELECT q.src, q.acct, q.ccy, q.snap,
           CASE WHEN q.next_snap IS NOT NULL THEN q.next_snap ELSE LEAST(e.end_snap, h.snap) END AS until
      FROM seq q
      LEFT JOIN cash_key_ends() e ON e.src = q.src AND e.acct = q.acct AND e.ccy = q.ccy
      ASOF LEFT JOIN clock h ON h.src = q.src
           AND h.snap >= (q.snap // 86400 + hist_carry_days() + 1) * 86400
);

-- ---- the active snapshot per key at an instant -------------------------

-- pos_active_at / cash_active_at: per instant of p_instants, each key's
-- newest observation at or before it, where that observation still
-- holds. A key no observation holds at the instant has no row.
CREATE OR REPLACE MACRO pos_active_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    s AS MATERIALIZED (SELECT * FROM pos_key_spans())
    SELECT b.instant, s.src, s.acct, s.snap
      FROM b CROSS JOIN (SELECT DISTINCT src, acct FROM s) k
      ASOF JOIN s ON s.src = k.src AND s.acct = k.acct AND s.snap <= b.instant
     WHERE s.until IS NULL OR b.instant < s.until
);
CREATE OR REPLACE MACRO cash_active_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    s AS MATERIALIZED (SELECT * FROM cash_key_spans())
    SELECT b.instant, s.src, s.acct, s.ccy, s.snap
      FROM b CROSS JOIN (SELECT DISTINCT src, acct, ccy FROM s) k
      ASOF JOIN s ON s.src = k.src AND s.acct = k.acct AND s.ccy = k.ccy AND s.snap <= b.instant
     WHERE s.until IS NULL OR b.instant < s.until
);

-- ---- the point-in-time line bases, picked per key ----------------------

-- positions_at: 0123's, each account at its own active snapshot.
CREATE OR REPLACE MACRO positions_at(p_instants) AS TABLE (
    WITH pick AS (SELECT instant, src, acct, snap FROM pos_active_at(p_instants))
    SELECT pk.instant, p.silver_source_id, p.snapshot_at, p.account_external_id,
           a.display_name, a.relationship_id, a.nickname, a.account_category,
           p.position_key, p.instrument_external_id, il.symbol, il.name,
           p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value,
           p.book_value, p.accrued_interest, p.acquisition_date,
           p.basis_origin, p.basis_method, p.basis_fees,
           p.book_value_known, p.quantity_without_basis, p.open_lots, p.valued_at
      FROM positions p
      JOIN pick pk ON pk.src = p.silver_source_id AND pk.acct = p.account_external_id
       AND pk.snap = p.snapshot_at
      LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id
      LEFT JOIN instrument_labels() il ON il.silver_source_id = p.silver_source_id
           AND il.instrument_external_id = p.instrument_external_id
);

-- cash_chosen: 0021's, each (account, currency) at its own active
-- snapshot, one row by balance_kind precedence (current > closing >
-- available > ...), non-zero.
CREATE OR REPLACE MACRO cash_chosen(p_asof) AS TABLE (
    WITH latest AS (SELECT src, acct, ccy, snap FROM cash_active_at([p_asof])),
    ranked AS (
        SELECT cb.silver_source_id, cb.account_external_id, cb.currency,
               cb.amount, cb.snapshot_at,
               ROW_NUMBER() OVER (
                   PARTITION BY cb.silver_source_id, cb.account_external_id, cb.currency
                   ORDER BY CASE cb.balance_kind
                       WHEN 'current' THEN 1 WHEN 'closing' THEN 2
                       WHEN 'available' THEN 3 WHEN 'aggregated' THEN 4
                       WHEN 'opening' THEN 5 WHEN 'initial' THEN 6
                       WHEN 'projected' THEN 7 ELSE 99 END, cb.balance_kind) AS rn
          FROM cash_balances cb
          JOIN latest l ON cb.silver_source_id = l.src AND cb.account_external_id = l.acct
           AND cb.currency = l.ccy AND cb.snapshot_at = l.snap)
    SELECT silver_source_id, account_external_id, currency, amount, snapshot_at
      FROM ranked WHERE rn = 1 AND amount <> 0
);

-- account_lines_base: 0024's, positions at each account's active
-- snapshot.
CREATE OR REPLACE MACRO account_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy,
               p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, currency, amount, snapshot_at, TRUE
          FROM cash_chosen(p_asof))
    SELECT l.src, l.acct, l.ccy, l.amt, l.snap, l.is_cash, a.base_currency,
           CASE WHEN a.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.ccy = a.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      LEFT JOIN accounts a ON l.src = a.silver_source_id AND l.acct = a.account_external_id
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = a.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = a.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = a.base_currency AND b4.day <= (l.snap // 86400)
);

-- portfolio_lines_base: 0024's, positions at each account's active
-- snapshot.
CREATE OR REPLACE MACRO portfolio_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, ap.pid, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
          JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.silver_source_id, ap.pid, cc.currency, cc.amount, cc.snapshot_at, TRUE
          FROM cash_chosen(p_asof) cc JOIN acct_port ap ON ap.src = cc.silver_source_id AND ap.acct = cc.account_external_id)
    SELECT l.src, l.pid, l.ccy, l.amt, l.snap, l.is_cash, bk.base_currency,
           CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      JOIN portfolio_buckets() bk ON bk.src = l.src AND bk.pid = l.pid
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)
);

-- source_lines_base: 0123's, positions at each account's active
-- snapshot.
CREATE OR REPLACE MACRO source_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap,
               COALESCE(p.valued_at, p.snapshot_at) AS valued, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, currency, amount, snapshot_at, snapshot_at, TRUE
          FROM cash_chosen(p_asof))
    SELECT l.src, l.ccy, l.amt, l.snap, l.valued, l.is_cash, bk.base_currency,
           CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      JOIN accounts a ON a.silver_source_id = l.src AND a.account_external_id = l.acct
      JOIN source_buckets() bk ON bk.src = l.src
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)
);

-- report_positions_multi: 0114's, each account at its own active
-- snapshot.
CREATE OR REPLACE MACRO report_positions_multi(p_asof) AS TABLE (
    WITH latest AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.src AND p.account_external_id = l.acct
           AND p.snapshot_at = l.snap
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id)
    SELECT b.silver_source_id, b.snapshot_at, b.account_external_id, b.display_name,
           b.relationship_id, b.nickname, b.account_category, b.position_key,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.vehicle, b.currency,
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.market_value AS DECIMAL(28,4)) AS market_value,
           CAST(fx_reporting_value(b.market_value, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.market_value, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.market_value, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.market_value, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM base b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.snapshot_at // 86400)
     ORDER BY b.silver_source_id, b.account_external_id, b.position_key
);

-- exposure_holdings: 0126's, positions at each account's active
-- snapshot.
CREATE OR REPLACE MACRO exposure_holdings(p_asof, p_ccy) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.position_key,
               p.instrument_external_id AS iid, p.asset_class, p.vehicle, p.currency AS ccy,
               p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, 'cash:' || currency, NULL, 'cash', 'demand_deposit',
               currency, amount, snapshot_at, TRUE
          FROM cash_chosen(p_asof)),
    valued AS (
        SELECT l.*,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS value
          FROM lines l
          JOIN accounts a ON a.silver_source_id = l.src AND a.account_external_id = l.acct
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    dropped AS (
        SELECT src, acct FROM valued GROUP BY 1, 2
        HAVING (COUNT(*) FILTER (WHERE NOT is_cash) > 0 AND COUNT(value) FILTER (WHERE NOT is_cash) = 0)
            OR (COUNT(*) FILTER (WHERE is_cash) > 0 AND COUNT(value) FILTER (WHERE is_cash) = 0)),
    keyed AS (
        SELECT v.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
          FROM valued v
          ANTI JOIN dropped d ON d.src = v.src AND d.acct = v.acct
          LEFT JOIN holding_aliases ha ON ha.silver_source_id = v.src AND ha.account_external_id = v.acct
               AND ha.from_key = holding_key(v.iid, v.position_key)
          LEFT JOIN holding_aliases hd ON hd.silver_source_id = v.src AND hd.account_external_id = ''
               AND hd.from_key = holding_key(v.iid, v.position_key)
          LEFT JOIN instrument_aliases() ia ON ia.src = v.src AND ia.alias = v.iid)
    SELECT k.src, k.acct, k.position_key, k.iid, k.asset_class, k.vehicle, k.ccy, k.snap, k.is_cash,
           k.value::DOUBLE AS value, il.symbol, il.name,
           exposure_key(COALESCE(ik1.security_key, ik2.security_key)) AS key
      FROM keyed k
      LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = k.src AND ik1.instrument_external_id = k.akey
      LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = k.src AND ik2.instrument_external_id = k.iid
      LEFT JOIN instrument_labels() il ON il.silver_source_id = k.src AND il.instrument_external_id = k.iid
);

-- report_gains_check: 0118's, the positions check at each account's
-- active snapshot at the window's end, where that falls in the window.
CREATE OR REPLACE MACRO report_gains_check(p_from, p_to, p_ccy) AS TABLE (
    WITH r AS (
        SELECT x.*, lot_key(x.instrument_external_id, x.instrument_hint, x.description) AS k
          FROM realized_lots_in(p_from, p_to, p_ccy, TRUE) x),
    eng AS (
        SELECT silver_source_id, account_external_id, k, tax_year,
               COUNT(*) AS n, SUM(quantity) AS qty, SUM(book_x) AS cost_x, SUM(gain_x) AS gain_x,
               COUNT(*) FILTER (WHERE book_x IS NULL) AS n_uncosted
          FROM r WHERE document_kind = 'engine' AND NOT is_primary GROUP BY ALL),
    sta AS (
        SELECT silver_source_id, account_external_id, k, tax_year,
               SUM(quantity) AS qty, SUM(book_x) AS cost_x, SUM(gain_x) AS gain_x
          FROM r WHERE document_kind <> 'engine' AND is_primary GROUP BY ALL),
    realized AS (
        SELECT 'realized' AS check_kind, e.silver_source_id, e.account_external_id,
               CAST(e.tax_year AS VARCHAR) AS period,
               SUM(e.n) AS items, SUM(e.qty) AS quantity,
               SUM(e.cost_x) AS engine_x, SUM(s.cost_x) AS stated_x,
               SUM(e.gain_x) AS engine_gain_x, SUM(s.gain_x) AS stated_gain_x
          FROM eng e JOIN sta s ON s.silver_source_id = e.silver_source_id
           AND s.account_external_id = e.account_external_id AND s.k = e.k AND s.tax_year = e.tax_year
         WHERE e.n_uncosted = 0 AND abs(e.qty - s.qty) <= 0.005 * abs(s.qty)
         GROUP BY ALL),
    -- A pooled source states no basis of a wallet's own, so only
    -- account keys are compared.
    last_snap AS (
        SELECT a.src AS silver_source_id, a.acct AS account_external_id, a.snap AS snapshot_at,
               lot_obs_at(a.snap) AS instant
          FROM pos_active_at([p_to]) a JOIN lot_sources() s ON s.silver_source_id = a.src AND NOT s.pooled
         WHERE a.snap >= p_from),
    stated_pos AS (
        SELECT p.silver_source_id, p.account_external_id, holding_key(p.instrument_external_id, p.position_key) AS k,
               any_value(p.currency) AS currency, ls.snapshot_at, ls.instant,
               SUM(p.quantity) AS qty, SUM(p.book_value) AS cost
          FROM positions p JOIN last_snap ls ON ls.silver_source_id = p.silver_source_id
           AND ls.account_external_id = p.account_external_id AND ls.snapshot_at = p.snapshot_at
         WHERE stated_basis(p.book_value, p.basis_origin) AND basis_applies(p.asset_class, p.vehicle)
         GROUP BY ALL),
    open_book AS (
        SELECT instant, silver_source_id, account_external_id, instrument_external_id AS k,
               SUM(quantity) AS qty, SUM(cost) AS cost, bool_and(cost IS NOT NULL) AS costed
          FROM lots_at((SELECT list(DISTINCT instant) FROM last_snap)) GROUP BY ALL),
    positions_check AS (
        SELECT 'positions' AS check_kind, s.silver_source_id, s.account_external_id, NULL AS period,
               COUNT(*) AS items, SUM(s.qty) AS quantity,
               SUM(fx_amount(b.cost, s.currency, p_ccy, fx.rate)) AS engine_x,
               SUM(fx_amount(s.cost, s.currency, p_ccy, fx.rate)) AS stated_x,
               CAST(NULL AS DOUBLE) AS engine_gain_x, CAST(NULL AS DOUBLE) AS stated_gain_x
          FROM stated_pos s
          JOIN open_book b ON b.instant = s.instant AND b.silver_source_id = s.silver_source_id
           AND b.account_external_id = s.account_external_id AND b.k = s.k
          ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = s.currency AND fx.day <= (s.snapshot_at // 86400)
         WHERE b.costed AND abs(b.qty - s.qty) <= 0.005 * abs(s.qty)
         GROUP BY ALL),
    anchors AS (
        SELECT 'anchors' AS check_kind, a.silver_source_id, a.account_external_id,
               NULL AS period, COUNT(*) AS items, SUM(a.matched_quantity) AS quantity,
               SUM(fx_amount(a.book_cost, a.currency, p_ccy, fx.rate)) AS engine_x,
               SUM(fx_amount(a.stated_cost, a.currency, p_ccy, fx.rate)) AS stated_x,
               CAST(NULL AS DOUBLE) AS engine_gain_x, CAST(NULL AS DOUBLE) AS stated_gain_x
          FROM lot_anchors a
          ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = a.currency AND fx.day <= (a.anchored_at // 86400)
         WHERE a.anchored_at BETWEEN p_from AND p_to AND NOT a.kept
         GROUP BY ALL),
    -- What each move carried, from its own disposals: the receiving
    -- lots of one move can be many (one per lot, or an average pool
    -- reopened per merge), so they only say where it went.
    moves AS (
        SELECT d.disposal_transaction_external_id AS txn, g.silver_source_id AS from_src,
               g.instrument_external_id AS inst, MIN(d.disposed_at) AS at,
               SUM(d.quantity) AS qty, SUM(d.cost) AS cost, bool_and(d.cost IS NOT NULL) AS costed
          FROM lot_disposals d JOIN lots g ON g.lot_id = d.lot_id
         WHERE d.kind = 'move' AND d.disposed_at BETWEEN p_from AND p_to
         GROUP BY ALL),
    moved AS (
        SELECT n.silver_source_id, n.account_external_id, n.instrument_external_id, n.currency,
               MIN(mv.at) AS at, SUM(mv.qty) AS qty, SUM(mv.cost) AS cost, bool_and(mv.costed) AS costed
          FROM moves mv
          JOIN (SELECT DISTINCT silver_source_id, account_external_id, instrument_external_id, currency,
                       origin_transaction_external_id
                  FROM lots WHERE origin = 'transfer_in') n
            ON n.origin_transaction_external_id = mv.txn AND n.silver_source_id <> mv.from_src
           AND n.instrument_external_id = mv.inst
         GROUP BY ALL),
    first_stated AS (
        SELECT mv.silver_source_id, mv.account_external_id, mv.instrument_external_id,
               arg_min(p.book_value::DOUBLE / NULLIF(p.quantity::DOUBLE, 0), p.snapshot_at) AS unit_cost
          FROM moved mv
          JOIN positions p ON p.silver_source_id = mv.silver_source_id AND p.account_external_id = mv.account_external_id
           AND p.instrument_external_id = mv.instrument_external_id AND p.snapshot_at >= mv.at
           AND stated_basis(p.book_value, p.basis_origin)
         GROUP BY ALL),
    handover AS (
        SELECT 'handover' AS check_kind, mv.silver_source_id, mv.account_external_id, NULL AS period,
               COUNT(*) AS items, SUM(mv.qty) AS quantity,
               SUM(fx_amount(mv.cost, mv.currency, p_ccy, fx.rate)) FILTER (WHERE mv.costed AND fs.unit_cost IS NOT NULL) AS engine_x,
               SUM(fx_amount(fs.unit_cost * mv.qty, mv.currency, p_ccy, fx.rate)) FILTER (WHERE mv.costed AND fs.unit_cost IS NOT NULL) AS stated_x,
               CAST(NULL AS DOUBLE) AS engine_gain_x, CAST(NULL AS DOUBLE) AS stated_gain_x
          FROM moved mv
          LEFT JOIN first_stated fs USING (silver_source_id, account_external_id, instrument_external_id)
          ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = mv.currency AND fx.day <= (mv.at // 86400)
         GROUP BY ALL),
    allc AS (
        SELECT * FROM realized UNION ALL BY NAME SELECT * FROM positions_check
        UNION ALL BY NAME SELECT * FROM anchors UNION ALL BY NAME SELECT * FROM handover)
    SELECT c.check_kind AS "check", c.silver_source_id, c.account_external_id, a.display_name, a.nickname,
           c.period, CAST(c.items AS BIGINT) AS items, CAST(c.quantity AS VARCHAR) AS quantity,
           CAST(c.engine_x::DECIMAL(28,4) AS VARCHAR) AS engine_cost,
           CAST(c.stated_x::DECIMAL(28,4) AS VARCHAR) AS stated_cost,
           CAST((c.engine_x - c.stated_x)::DECIMAL(28,4) AS VARCHAR) AS cost_delta,
           CAST(c.engine_gain_x::DECIMAL(28,4) AS VARCHAR) AS engine_gain,
           CAST(c.stated_gain_x::DECIMAL(28,4) AS VARCHAR) AS stated_gain,
           CAST((c.engine_gain_x - c.stated_gain_x)::DECIMAL(28,4) AS VARCHAR) AS gain_delta
      FROM allc c
      LEFT JOIN accounts a ON a.silver_source_id = c.silver_source_id AND a.account_external_id = c.account_external_id
     ORDER BY CASE c.check_kind WHEN 'realized' THEN 1 WHEN 'positions' THEN 2 WHEN 'anchors' THEN 3 ELSE 4 END,
              c.silver_source_id, c.account_external_id, c.period
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (140, CAST(epoch(now()) AS BIGINT));
