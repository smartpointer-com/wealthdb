-- Observation spans: the positions and cash balances active at an
-- instant or on each day, and what each source has observed.

-- Daily-history variants of the report macros, for time-series
-- analytics in Metabase. Each report_*_history_multi() produces one row per entity per
-- UTC day, from the first snapshot day to today, with the value CARRIED FORWARD: on a
-- day with no new snapshot, the most recent snapshot's value is repeated (positions and
-- cash carried independently, since their snapshot series can diverge). Every line is
-- valued once at its own snapshot's FX day, then expanded onto the daily spine via an
-- at-or-before ASOF join — so history@today == the report_*(MAX) "latest" values, and
-- per-day compute stays cheap. Empty entity-days are omitted (they would be 0 and do not
-- affect any Σ). `as_of_day` is epoch-seconds at UTC midnight (rendered TIMESTAMP by the
-- Metabase models).

-- Daily spine: first snapshot day .. today (UTC). Empty gold -> no rows.
CREATE MACRO hist_days() AS TABLE (
    WITH b AS (
        SELECT MIN(d) AS lo, GREATEST(MAX(d), CAST(epoch(now()) AS BIGINT) // 86400) AS hi
          FROM (SELECT DISTINCT snapshot_at // 86400 AS d FROM positions
                UNION SELECT DISTINCT snapshot_at // 86400 FROM cash_balances))
    SELECT unnest(generate_series(b.lo, b.hi)) AS day FROM b WHERE b.lo IS NOT NULL
);

-- The daily-history macros resolve the active snapshot per (source,
-- ACCOUNT, day) — for cash, per (source, account, CURRENCY, day) —
-- not per (source, day).
--
-- One active snapshot per source and day is right for a source that
-- writes every account in a single run, and wrong for one that does
-- not: a deposit run, a card run and a statement backfill each land
-- under their own snapshot_at, so the day would resolve to whichever
-- run happened to be last, and every account the other runs carried
-- would be absent. The day's total would be that one run's accounts
-- alone, which for a card-only day is a negative number.
--
-- The rules:
--
--   * ACTIVE SNAPSHOT PER KEY. Each key is ASOF-joined onto the day
--     spine on its own snapshot series, so it contributes its own most
--     recent observation at or before the day and the day's total is
--     the sum across keys. One account's dump day cannot hide
--     another's. The key is (source, account) for positions — the carry
--     unit there is the account's whole snapshot, so a holding absent
--     from that account's next snapshot is sold rather than carried —
--     and (source, account, CURRENCY) for cash, where every balance row
--     is its own observation and a run may report one leg of a
--     multi-currency account without touching the others. Cash grain is
--     the grain `report_card_balances_history_multi` uses.
--
--   * ZERO IS AN OBSERVATION. Under carry-forward a dropped zero row
--     would be a bug: an account paid down to zero would keep
--     contributing its last non-zero balance for ever. Zero rows are
--     kept, so the zero supersedes the balance before it, and a zero
--     line is valued as zero in every reporting currency rather than
--     following the FX chain — no rate exists for a currency nobody
--     quotes, and a NULL there would poison the whole entity-day's sum,
--     not just that line. Positions need no filter of their own: a
--     holding absent from the account's next snapshot is already gone,
--     and an explicit $0 position row (the closure marker the adapters
--     emit) reads as zero. The per-POSITION series keeps the plain FX
--     chain, matching its point-in-time twin — a display row has no sum
--     for a NULL to poison.
--
--   * A LATER RUN THAT RE-COVERS THE KEY'S COMPANY ENDS IT. Absence is
--     evidence of closure exactly when the run that produced it was in
--     a position to report the key: a key leaves the series at the
--     first later snapshot of its source that covers every OTHER key
--     observed alongside it in its own last snapshot and still being
--     reported then. Company that left in the same run is not company
--     the source could re-cover, so it is not required — otherwise two
--     accounts closing in one nightly dump would each hold the other's
--     ending open. A nightly full dump therefore ends a closed account
--     on the very next dump, whether one account closed or several; a
--     card-only or deposit-only run covers none of the other run's keys
--     and ends nothing. A key observed alone has no company to re-cover
--     and is never ended this way.
--
--   * THE SOURCE'S CLOCK BOUNDS THE REST. The backstop for a key
--     observed alone: it stops contributing once its source has
--     produced a snapshot more than `hist_carry_days()` days after that
--     key's last observation. The evidence is always the source's own
--     activity, never the calendar — a source that stops running
--     supersedes nothing, so its accounts keep their last values to the
--     end of the spine.
--
--   * BOTH ENDINGS APPLY TO A KEY'S LAST OBSERVATION ONLY, so neither
--     can punch a hole in the middle of a series: a card reporting once
--     per statement cycle, a holding valued less often than yearly and
--     a deposit re-dumped nightly sit inside one source without
--     expiring each other for as long as their runs carry their own
--     snapshot_at, and a key that is quiet for years still carries
--     every day up to its next observation. Where a slow key's last
--     observation DOES share a run with a faster sibling, that
--     sibling's next run covers the whole peer set and ends the slow
--     key the next day: at this grain that shape is indistinguishable
--     from closure.
--
-- What follows for readers:
--
--   * `history@today` TOTALS equal `report_*(MAX)` totals for a source
--     that writes every account in one run — the two reconcile on
--     values, not on rows. The point-in-time reports carry a row for
--     every account in the dimension (0 when the source's latest
--     snapshot gave it no lines); the history emits rows only for
--     entity-days that have lines, including a day whose only line is
--     a zero balance.
--   * For a source whose runs are partial the two differ by design: the
--     history carries every account, while the point-in-time reports
--     show only the accounts of the source's last run. `wealthdb
--     global` reads the point-in-time macros, so its headline can sit
--     below a chart built on the history for such a source (DESIGN.md
--     §10.7).
--   * `report_positions_history_multi.snapshot_at` varies per account within
--     a source-day: it is the account's own active snapshot, not the
--     source's.
--   * Cost: the day spine is crossed with (source, account) — for cash,
--     (source, account, currency) — keys rather than with sources, so
--     its cardinality grows by the accounts-per-source factor, and the
--     ending rules add one pass over each source's snapshot list.
--   * `report_card_balances_history_multi` resolves its active snapshot
--     the same way but bounds nothing: a card its source stops
--     reporting keeps its last owed balance to the end of that view's
--     spine, where the macros below end it.
--
-- `report_global_history_multi` sums the accounts history.

-- ---- the carry horizon ------------------------------------------------

-- How many days a source may keep snapshotting past the last
-- observation of a key it never re-covers before that key leaves the
-- series. A year: long enough that the slowest clock inside a mixed
-- source survives its faster siblings, short enough that a closed
-- account's balance does not outlive it indefinitely.
CREATE MACRO hist_carry_days() AS 365;

-- source_observed: per source, the instants through which its holdings
-- and its ledger were observed, NULL where the silver does not say.
CREATE VIEW source_observed AS
    SELECT silver_source_id,
           CASE WHEN observed_snapshots_through > 0 THEN observed_snapshots_through END AS observed_snapshots_through,
           CASE WHEN observed_transactions_through > 0 THEN observed_transactions_through END AS observed_transactions_through
      FROM silver_sources;

-- ============================================================
-- Each key's observations, defined once.
--
-- pos_key_obs and cash_key_obs are each key's observations: the
-- DISTINCT over `positions` or `cash_balances` that the endings, the
-- spans and the daily history read. key_grace is the grace both
-- endings give a key. pos_key_ends and cash_key_ends, pos_key_spans
-- and cash_key_spans, hist_active_pos and hist_active_cash each read
-- these macros (pos_key_ends' own_cash too).
--
-- The materializers read the endings and the spans once a run: they pin
-- them in their transaction behind temporary macros of these names
-- (gold.beginPinned). A change to these macros' columns changes the
-- pinned tables too.
-- ============================================================

-- ---- each key's observations -------------------------------------------

-- pos_key_obs / cash_key_obs: every observation of every key, once: a
-- positions key is (source, account), a cash key (source, account,
-- currency).
CREATE MACRO pos_key_obs() AS TABLE (
    SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, snapshot_at AS snap
      FROM positions
);

CREATE MACRO cash_key_obs() AS TABLE (
    SELECT DISTINCT silver_source_id AS src, account_external_id AS acct,
           currency AS ccy, snapshot_at AS snap
      FROM cash_balances
);

-- key_grace: how long a key's last observation stands past it, in
-- seconds: one and a half of its cadence, the median gap between its
-- observation days, or of a month where it has no gap.
CREATE MACRO key_grace(median_gap_days) AS
    CAST(1.5 * COALESCE(median_gap_days, 30) * 86400 AS BIGINT);

-- ---- the endings, the history and the spans -----------------------------

-- pos_key_ends / cash_key_ends: the snapshot at which each key leaves
-- the series, by the endings above; a key that has not ended has no row.
CREATE MACRO pos_key_ends() AS TABLE (
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

CREATE MACRO cash_key_ends() AS TABLE (
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

-- hist_active_pos / hist_active_cash: per day of the spine, each key's
-- active snapshot, until the key ends or its source's clock bounds it.
CREATE MACRO hist_active_pos() AS TABLE (
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

CREATE MACRO hist_active_cash() AS TABLE (
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

-- pos_key_spans / cash_key_spans: each observation of each key and the
-- instant until which it holds: the key's next observation, else its
-- ending or the carry horizon.
CREATE MACRO pos_key_spans() AS TABLE (
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

-- ---- the active snapshot per key at an instant -------------------------

-- pos_active_at / cash_active_at: per instant of p_instants, each key's
-- newest observation at or before it, where that observation still
-- holds. A key no observation holds at the instant has no row.
CREATE MACRO pos_active_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    s AS MATERIALIZED (SELECT * FROM pos_key_spans())
    SELECT b.instant, s.src, s.acct, s.snap
      FROM b CROSS JOIN (SELECT DISTINCT src, acct FROM s) k
      ASOF JOIN s ON s.src = k.src AND s.acct = k.acct AND s.snap <= b.instant
     WHERE s.until IS NULL OR b.instant < s.until
);

CREATE MACRO cash_key_spans() AS TABLE (
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

CREATE MACRO cash_active_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    s AS MATERIALIZED (SELECT * FROM cash_key_spans())
    SELECT b.instant, s.src, s.acct, s.ccy, s.snap
      FROM b CROSS JOIN (SELECT DISTINCT src, acct, ccy FROM s) k
      ASOF JOIN s ON s.src = k.src AND s.acct = k.acct AND s.ccy = k.ccy AND s.snap <= b.instant
     WHERE s.until IS NULL OR b.instant < s.until
);
