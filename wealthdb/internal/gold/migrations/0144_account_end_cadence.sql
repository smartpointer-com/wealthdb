-- ============================================================
-- gold schema, migration 0144 — an account's last observation ends on
-- its own cadence too.
--
-- A key's last observation ends at the first later snapshot of its
-- source that re-covers its company (migrations 0051 and 0140). A slow
-- key whose last observation shares a run with faster siblings so ends
-- at their next run: a quarterly account of a monthly statement drops
-- out from the month-end after its last quarter end until its next one.
--
-- The ending takes the key's cadence as a second condition. The key's
-- last observation ends at its re-cover snapshot only where both hold:
--
--   * a later run of its source re-covers its company without it, as
--     before; and
--   * the source's newest snapshot in that table lies more than one and
--     a half cadences past the observation.
--
-- A key's cadence is the median gap between its own observation days,
-- so a second reading of one day does not shrink it and one long gap
-- among several does not stretch it. A key observed on one day only
-- takes a month, a statement's. Instants are compared, not days.
-- hist_carry_days() stays the backstop for a key no re-cover has ended.
--
-- So a recent last observation stands while its source has not yet run
-- one and a half cadences past it: a quarterly account through the
-- month ends after its quarter end. A new quarterly account, seen at one
-- quarter end only, takes a month: it drops out from its second month
-- end until its second quarter end. Once the source has run past the
-- grace, the ending is the re-cover snapshot, at every instant: a closed
-- account reads as closed from the run that left it out, as under 0140.
-- So a read of a past instant can change when a later run passes a
-- key's grace.
--
-- A positions key needs no grace where its re-cover run states the
-- account's cash: that run reported the account, and its holdings were
-- gone, as after a sale of every one. Without it the holdings would
-- stand beside the proceeds.
--
-- The cadence alone ends nothing. Some sources report a few accounts
-- only in bursts, beside a nightly run of others. A cadence rule alone
-- would drop them between bursts. The companions test keeps them while
-- each burst is a run of its own: no nightly run covers their company.
-- So a key can only stand longer than under 0140, never shorter.
--
-- The clock is the key's own table: a run that writes only cash moves
-- no grace of a positions key, as it moves no horizon.
--
-- What follows for readers:
--
--   * pos_key_ends and cash_key_ends give the combined ending. The
--     history (hist_active_pos, hist_active_cash) and the point-in-time
--     spans (pos_key_spans, cash_key_spans) read them, so every report
--     and the Go readers of 0140 follow, and a read at a day's end still
--     equals the history's day.
--   * cashflow_lines_base reads a mortgage's end from the shared spans:
--     its last observation retires what was left where that
--     observation ends, and nothing while it stands. 0143's own cadence
--     test is gone; the shared ending adds the companions test and the
--     horizon to it. A mortgage reported in a run of its own has no
--     company: only the horizon ends it.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- ---- the ending of a key's last observation ----------------------------

-- pos_key_ends / cash_key_ends: each key whose last observation a later
-- snapshot of its source ends, and that snapshot's instant: the
-- re-cover snapshot (`recovered`, 0140's), where the source's newest
-- snapshot lies more than one and a half cadences (`grace`) past the
-- observation. The re-cover snapshot is the first that covers every
-- other key seen in the key's own last snapshot AND still live at that
-- snapshot. `covering` counts the peers a candidate snapshot carries,
-- `need` counts the peers still reporting at that candidate (their own
-- last observation is at or after it), and the re-cover is the first
-- candidate where the two agree. A peer that left in the same run as
-- the key is therefore not required. A key observed alone has no peers
-- and no ending here. A positions key whose re-cover run states the
-- account's cash (`own_cash`) needs no grace: that run reported the
-- account, and its holdings were gone, as after a sale of every one.
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
         GROUP BY 1, 2, 3),
    recovered AS (
        SELECT c.src, c.acct, MIN(c.cand) AS snap
          FROM covering c JOIN need n
            ON n.src = c.src AND n.acct = c.acct AND n.cand = c.cand AND n.n = c.covered
         GROUP BY 1, 2),
    cadence AS (
        SELECT src, acct, CAST(1.5 * COALESCE(MEDIAN(next_day - day), 30) * 86400 AS BIGINT) AS grace
          FROM (SELECT src, acct, day, LEAD(day) OVER (PARTITION BY src, acct ORDER BY day) AS next_day
                  FROM (SELECT DISTINCT src, acct, snap // 86400 AS day FROM obs))
         GROUP BY 1, 2),
    newest AS (SELECT src, MAX(snap) AS snap FROM obs GROUP BY 1),
    own_cash AS (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, snapshot_at AS snap
          FROM cash_balances)
    SELECT r.src, r.acct, r.snap AS end_snap
      FROM recovered r
      JOIN last_obs l ON l.src = r.src AND l.acct = r.acct
      JOIN cadence c ON c.src = r.src AND c.acct = r.acct
      JOIN newest s ON s.src = r.src
      LEFT JOIN own_cash o ON o.src = r.src AND o.acct = r.acct AND o.snap = r.snap
     WHERE s.snap > l.snap + c.grace OR o.snap IS NOT NULL
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
         GROUP BY 1, 2, 3, 4),
    recovered AS (
        SELECT c.src, c.acct, c.ccy, MIN(c.cand) AS snap
          FROM covering c JOIN need n
            ON n.src = c.src AND n.acct = c.acct AND n.ccy = c.ccy AND n.cand = c.cand
           AND n.n = c.covered
         GROUP BY 1, 2, 3),
    cadence AS (
        SELECT src, acct, ccy, CAST(1.5 * COALESCE(MEDIAN(next_day - day), 30) * 86400 AS BIGINT) AS grace
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

-- ---- a mortgage's end in the cash flow ---------------------------------

-- cashflow_lines_base: 0143's, a mortgage's end read from the shared
-- spans. The allocation among an interval's payments, the leaves and
-- the shares stand.
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
    obs AS (
        SELECT b.msrc, b.macct,
               b.snapshot_at // 86400 AS from_day,
               COALESCE(LEAD(b.snapshot_at) OVER w // 86400, 99999999) AS to_day,
               b.mv, LEAD(b.mv) OVER w AS next_mv, s.until
          FROM bal b
          JOIN pos_key_spans() s ON s.src = b.msrc AND s.acct = b.macct AND s.snap = b.snapshot_at
        WINDOW w AS (PARTITION BY b.msrc, b.macct ORDER BY b.snapshot_at)
    ),
    -- A mortgage's last observation retires what was left where it
    -- ends, by the ending every reader takes (pos_key_spans): a later
    -- run re-covers its company and its source runs more than one and a
    -- half cadences past it, or the source's clock passes the horizon.
    -- Short of that it stands, and its last interval retires nothing.
    retired AS (
        SELECT msrc, macct, from_day, to_day,
               CASE WHEN next_mv IS NOT NULL THEN next_mv - mv
                    WHEN until IS NOT NULL THEN -mv
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
    VALUES (144, CAST(epoch(now()) AS BIGINT));
