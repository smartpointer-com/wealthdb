-- ============================================================
-- gold schema, migration 0145 — each key's observations, defined once.
--
-- pos_key_obs and cash_key_obs are each key's observations: the
-- DISTINCT over `positions` or `cash_balances` the endings, the spans
-- and the daily history each wrote out. key_grace is the grace both
-- endings give a key. pos_key_ends and cash_key_ends (0144),
-- pos_key_spans and cash_key_spans, hist_active_pos and hist_active_cash
-- (0140) are their latest text, each reading these macros (pos_key_ends'
-- own_cash too). yield_holdings (0136) carries each holding's kind in
-- its join keys. Every answer stays the same.
--
-- The materializers read the endings and the spans once a run: they pin
-- them in their transaction behind temporary macros of these names
-- (gold.beginPinned). A change to these macros' columns changes the
-- pinned tables too.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- ---- each key's observations -------------------------------------------

-- pos_key_obs / cash_key_obs: every observation of every key, once: a
-- positions key is (source, account), a cash key (source, account,
-- currency).
CREATE OR REPLACE MACRO pos_key_obs() AS TABLE (
    SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, snapshot_at AS snap
      FROM positions
);
CREATE OR REPLACE MACRO cash_key_obs() AS TABLE (
    SELECT DISTINCT silver_source_id AS src, account_external_id AS acct,
           currency AS ccy, snapshot_at AS snap
      FROM cash_balances
);

-- key_grace: how long a key's last observation stands past it, in
-- seconds: one and a half of its cadence, the median gap between its
-- observation days, or of a month where it has no gap.
CREATE OR REPLACE MACRO key_grace(median_gap_days) AS
    CAST(1.5 * COALESCE(median_gap_days, 30) * 86400 AS BIGINT);

-- ---- the endings (0144), the history and the spans (0140) -------------

-- pos_key_ends / cash_key_ends: 0144's.
CREATE OR REPLACE MACRO pos_key_ends() AS TABLE (
    WITH obs AS MATERIALIZED (SELECT * FROM pos_key_obs()),
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
         GROUP BY 1, 2, 3),
    recovered AS (
        SELECT c.src, c.acct, MIN(c.cand) AS snap
          FROM covering c JOIN need n
            ON n.src = c.src AND n.acct = c.acct AND n.cand = c.cand AND n.n = c.covered
         GROUP BY 1, 2),
    cadence AS (
        SELECT src, acct, key_grace(MEDIAN(next_day - day)) AS grace
          FROM (SELECT src, acct, day, LEAD(day) OVER (PARTITION BY src, acct ORDER BY day) AS next_day
                  FROM (SELECT DISTINCT src, acct, snap // 86400 AS day FROM obs))
         GROUP BY 1, 2),
    newest AS (SELECT src, MAX(snap) AS snap FROM obs GROUP BY 1),
    own_cash AS (SELECT DISTINCT src, acct, snap FROM cash_key_obs())
    SELECT r.src, r.acct, r.snap AS end_snap
      FROM recovered r
      JOIN last_obs l ON l.src = r.src AND l.acct = r.acct
      JOIN cadence c ON c.src = r.src AND c.acct = r.acct
      JOIN newest s ON s.src = r.src
      LEFT JOIN own_cash o ON o.src = r.src AND o.acct = r.acct AND o.snap = r.snap
     WHERE s.snap > l.snap + c.grace OR o.snap IS NOT NULL
);
CREATE OR REPLACE MACRO cash_key_ends() AS TABLE (
    WITH obs AS MATERIALIZED (SELECT * FROM cash_key_obs()),
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
         GROUP BY 1, 2, 3, 4),
    recovered AS (
        SELECT c.src, c.acct, c.ccy, MIN(c.cand) AS snap
          FROM covering c JOIN need n
            ON n.src = c.src AND n.acct = c.acct AND n.ccy = c.ccy AND n.cand = c.cand
           AND n.n = c.covered
         GROUP BY 1, 2, 3),
    cadence AS (
        SELECT src, acct, ccy, key_grace(MEDIAN(next_day - day)) AS grace
          FROM (SELECT src, acct, ccy, day,
                       LEAD(day) OVER (PARTITION BY src, acct, ccy ORDER BY day) AS next_day
                  FROM (SELECT DISTINCT src, acct, ccy, snap // 86400 AS day FROM obs))
         GROUP BY 1, 2, 3),
    newest AS (SELECT src, MAX(snap) AS snap FROM obs GROUP BY 1)
    SELECT r.src, r.acct, r.ccy, r.snap AS end_snap
      FROM recovered r
      JOIN last_obs l ON l.src = r.src AND l.acct = r.acct AND l.ccy = r.ccy
      JOIN cadence c ON c.src = r.src AND c.acct = r.acct AND c.ccy = r.ccy
      JOIN newest s ON s.src = r.src
     WHERE s.snap > l.snap + c.grace
);

-- hist_active_pos / hist_active_cash: 0140's.
CREATE OR REPLACE MACRO hist_active_pos() AS TABLE (
    WITH obs AS (SELECT * FROM pos_key_obs()),
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
    WITH obs AS (SELECT * FROM cash_key_obs()),
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

-- pos_key_spans / cash_key_spans: 0140's.
CREATE OR REPLACE MACRO pos_key_spans() AS TABLE (
    WITH obs AS MATERIALIZED (SELECT * FROM pos_key_obs()),
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
    WITH obs AS MATERIALIZED (SELECT * FROM cash_key_obs()),
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

-- ---- yield_holdings, hash-joined ----------------------------------------

-- yield_holdings: 0136's text with each holding's kind in the keys it
-- joins on. A cash holding joins positions on a NULL snapshot, and a
-- position joins the cash on a NULL key, so neither matches. In a LEFT
-- JOIN, DuckDB hashes only on equalities between both sides. A condition
-- on the holding alone would make each read loop over every position.
CREATE OR REPLACE MACRO yield_holdings(p_asof, p_ccy) AS TABLE (
    SELECT h.*, COALESCE(p.market_value, c.amount)::DOUBLE AS amount, p.quantity::DOUBLE AS quantity,
           p.accrued_interest::DOUBLE AS accrued, p.acquisition_date
      FROM exposure_holdings(p_asof, p_ccy) h
      LEFT JOIN positions p ON p.silver_source_id = h.src AND p.account_external_id = h.acct
           AND p.position_key = h.position_key AND p.snapshot_at = CASE WHEN NOT h.is_cash THEN h.snap END
      LEFT JOIN cash_chosen(p_asof) c ON c.silver_source_id = h.src AND c.account_external_id = h.acct
           AND 'cash:' || c.currency = CASE WHEN h.is_cash THEN h.position_key END
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (145, CAST(epoch(now()) AS BIGINT));
