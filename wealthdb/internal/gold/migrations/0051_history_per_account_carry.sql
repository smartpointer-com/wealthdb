-- The daily-history macros resolve the active snapshot per (source,
-- ACCOUNT, day) — for cash, per (source, account, CURRENCY, day) —
-- instead of per (source, day).
--
-- Migration 0022 gave each source ONE active snapshot per day — its
-- latest snapshot_at at or before end-of-day — and every history macro
-- joined its lines to that timestamp by equality. That is right for a
-- source that writes every account in a single run, and wrong for one
-- that does not: a deposit run, a card run and a statement backfill each
-- land under their own snapshot_at, so the day resolves to whichever run
-- happened to be last and every account the other runs carried is not
-- carried forward — it is simply absent. The day's total is then that
-- one run's accounts alone, which for a card-only day is a negative
-- number, and a stacked daily series alternates between the real total
-- and a single account's balance.
--
-- The rules this migration issues instead:
--
--   * ACTIVE SNAPSHOT PER KEY. Each key is ASOF-joined onto the day
--     spine on its own snapshot series, so it contributes its own most
--     recent observation at or before the day and the day's total is
--     the sum across keys. One account's dump day can no longer hide
--     another's. The key is (source, account) for positions — the carry
--     unit there is the account's whole snapshot, so a holding absent
--     from that account's next snapshot is sold rather than carried —
--     and (source, account, CURRENCY) for cash, where every balance row
--     is its own observation and a run may report one leg of a
--     multi-currency account without touching the others. Cash grain is
--     what migration 0043 already uses for
--     `report_card_balances_history_multi`, generalised.
--
--   * ZERO IS AN OBSERVATION. The cash line bases dropped `amount <> 0`
--     rows as noise. Under one snapshot per source that was harmless;
--     under carry-forward it is a bug, because an account paid down to
--     zero would keep contributing its last non-zero balance for ever.
--     Zero rows are kept, so the zero supersedes the balance before it,
--     and a zero line is valued as zero in every reporting currency
--     rather than following the FX chain — no rate exists for a
--     currency nobody quotes, and a NULL there would poison the whole
--     entity-day's sum, not just that line. Positions need no filter of
--     their own: a holding absent from the account's next snapshot is
--     already gone, and an explicit $0 position row (the closure marker
--     the adapters emit) reads as zero. The per-POSITION series keeps the
--     plain FX chain, matching its point-in-time twin — a display row has
--     no sum for a NULL to poison.
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
--     on the very next dump, as the per-source rule did, whether one
--     account closed or several; a card-only or deposit-only run covers
--     none of the other run's keys and ends nothing. A key observed
--     alone has no company to re-cover and is never ended this way.
--
--   * THE SOURCE'S CLOCK BOUNDS THE REST. The backstop for a key
--     observed alone: it stops contributing once its source has
--     produced a snapshot more than `hist_carry_days()` days after that
--     key's last observation. The evidence is always the source's own
--     activity, never the calendar — a source that stops running
--     supersedes nothing, so its accounts keep their last values to the
--     end of the spine, exactly as under the per-source carry this
--     replaces.
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
--     from closure, and it is what the per-source rule this replaces
--     did with it too.
--
-- What follows for readers:
--
--   * `history@today` TOTALS equal `report_*(MAX)` totals for a source
--     that writes every account in one run — the two reconcile on
--     values, not on rows. The point-in-time reports carry a row for
--     every account in the dimension (0 when the source's latest
--     snapshot gave it no lines); the history emits rows only for
--     entity-days that have lines, which now includes a day whose only
--     line is a zero balance.
--   * For a source whose runs are partial the two differ by design: the
--     history carries every account, while the point-in-time reports
--     show only the accounts of the source's last run. `wealthdb
--     global` reads the point-in-time macros, so its headline can sit
--     below a chart built on the history for such a source (DESIGN.md
--     §10.7).
--   * `report_positions_history.snapshot_at` now varies per account
--     within a source-day: it is the account's own active snapshot, not
--     the source's.
--   * Cost: the day spine is crossed with (source, account) — for cash,
--     (source, account, currency) — keys rather than with sources, so
--     its cardinality grows by the accounts-per-source factor, and the
--     ending rules add one pass over each source's snapshot list.
--   * `hist_acct_lines(p_ccy)` (migration 0022) has been dead since
--     0024 replaced it with `hist_acct_lines_base()`; it is deliberately
--     NOT re-issued here, so the `amt <> 0` it still carries is not the
--     live rule and it must not be used for new history macros.
--   * `report_card_balances_history_multi` (0043) resolves its active
--     snapshot the same way but deliberately bounds nothing: a card its
--     source stops reporting keeps its last owed balance to the end of
--     that view's spine, where the macros below end it.
--
-- Every macro whose body reads hist_active_pos / hist_active_cash is
-- re-issued below, because the join gains the account (and, for cash,
-- the currency) key; bodies are otherwise copied verbatim from their
-- latest definition (0024 for the accounts and portfolios families,
-- 0025 for sources, 0022 for the single-currency positions history,
-- 0031 for its _multi). Column lists, types and NULL-vs-0 semantics are
-- unchanged, so every reader — the returns loader, the Metabase serving
-- views of 0032/0043/0049, the CLI scans — is untouched.
-- `report_global_history` / `_multi` sum the accounts history and bind
-- their macros lazily, so they need no re-issue.
--
-- CREATE OR REPLACE throughout keeps this replayable for the DDL-rerun
-- test (see gold.Migrate's REPLAY note).

-- ---- the carry horizon ------------------------------------------------

-- How many days a source may keep snapshotting past the last
-- observation of a key it never re-covers before that key leaves the
-- series. A year: long enough that the slowest clock inside a mixed
-- source survives its faster siblings, short enough that a closed
-- account's balance does not outlive it indefinitely.
CREATE OR REPLACE MACRO hist_carry_days() AS 365;

-- ---- active snapshot per key per day -----------------------------------

-- Positions and cash stay independent (a source's two series can
-- diverge), so each resolves on its own table, and each key carries its
-- own observation onto every day until one of the two endings above
-- applies to its LAST observation:
--
--   ends   the first later snapshot of the source that covers every
--          other key seen in the key's own last snapshot AND still live
--          at that snapshot: `covering` counts the peers a candidate
--          snapshot carries, `need` counts the peers still reporting at
--          that candidate (their own last observation is at or after
--          it), and the ending is the first candidate where the two
--          agree. A peer that departed in the same run as the key is
--          therefore not required — demanding it would make the count
--          unreachable for every key of a run in which two or more keys
--          leave together, and leave them all to the horizon below; and
--   QUALIFY the source's clock for the day — MAX(active snapshot) over
--          the day, which is the per-source active snapshot 0022
--          computed — running more than hist_carry_days() past the
--          key's own observation.
CREATE OR REPLACE MACRO hist_active_pos() AS TABLE (
    WITH obs AS (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, snapshot_at AS snap
          FROM positions),
    last_obs AS (SELECT src, acct, MAX(snap) AS snap FROM obs GROUP BY 1, 2),
    peers AS (
        SELECT l.src, l.acct, l.snap AS lsnap, o.acct AS peer, pl.snap AS peer_last
          FROM last_obs l
          JOIN obs o ON o.src = l.src AND o.snap = l.snap AND o.acct <> l.acct
          JOIN last_obs pl ON pl.src = l.src AND pl.acct = o.acct),
    covering AS (
        SELECT p.src, p.acct, o.snap AS cand, COUNT(*) AS covered
          FROM peers p JOIN obs o ON o.src = p.src AND o.acct = p.peer AND o.snap > p.lsnap
         GROUP BY 1, 2, 3),
    need AS (
        SELECT c.src, c.acct, c.cand, COUNT(*) AS n
          FROM covering c JOIN peers p
            ON p.src = c.src AND p.acct = c.acct AND p.peer_last >= c.cand
         GROUP BY 1, 2, 3),
    ends AS (
        SELECT c.src, c.acct, MIN(c.cand) // 86400 AS end_day
          FROM covering c JOIN need n
            ON n.src = c.src AND n.acct = c.acct AND n.cand = c.cand AND n.n = c.covered
         GROUP BY 1, 2),
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
    last_obs AS (SELECT src, acct, ccy, MAX(snap) AS snap FROM obs GROUP BY 1, 2, 3),
    peers AS (
        SELECT l.src, l.acct, l.ccy, l.snap AS lsnap, o.acct AS peer_acct, o.ccy AS peer_ccy,
               pl.snap AS peer_last
          FROM last_obs l
          JOIN obs o ON o.src = l.src AND o.snap = l.snap
           AND (o.acct <> l.acct OR o.ccy <> l.ccy)
          JOIN last_obs pl ON pl.src = l.src AND pl.acct = o.acct AND pl.ccy = o.ccy),
    covering AS (
        SELECT p.src, p.acct, p.ccy, o.snap AS cand, COUNT(*) AS covered
          FROM peers p JOIN obs o ON o.src = p.src AND o.acct = p.peer_acct
               AND o.ccy = p.peer_ccy AND o.snap > p.lsnap
         GROUP BY 1, 2, 3, 4),
    need AS (
        SELECT c.src, c.acct, c.ccy, c.cand, COUNT(*) AS n
          FROM covering c JOIN peers p
            ON p.src = c.src AND p.acct = c.acct AND p.ccy = c.ccy AND p.peer_last >= c.cand
         GROUP BY 1, 2, 3, 4),
    ends AS (
        SELECT c.src, c.acct, c.ccy, MIN(c.cand) // 86400 AS end_day
          FROM covering c JOIN need n
            ON n.src = c.src AND n.acct = c.acct AND n.ccy = c.ccy AND n.cand = c.cand
           AND n.n = c.covered
         GROUP BY 1, 2, 3),
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

-- ---- shared line base (0024), zero balances kept ----------------------

CREATE OR REPLACE MACRO hist_acct_lines_base() AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id AS acct,
               p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p
        UNION ALL
        SELECT src, snap, acct, ccy, amt, TRUE FROM cash_all)
    SELECT l.src, l.snap, l.acct, l.ccy, l.amt, l.is_cash, a.base_currency,
           CASE WHEN a.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = a.base_currency THEN l.amt::DOUBLE END,
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

-- ---- accounts family (0024) -------------------------------------------

CREATE OR REPLACE MACRO report_accounts_history(p_ccy) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val
          FROM hist_acct_lines_base() l
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_val, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_val, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, acct,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o,
               CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b,
               CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, a.silver_source_id, a.account_external_id, a.account_kind, a.display_name,
               a.base_currency, a.relationship_id, a.nickname, a.account_category,
               a.portfolio_external_id, a.tax_wrapper, a.management_style,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM agg g JOIN accounts a ON a.silver_source_id = g.src AND a.account_external_id = g.acct)
    SELECT day * 86400 AS as_of_day, silver_source_id, account_external_id, account_kind, display_name,
           base_currency, relationship_id, nickname, account_category, portfolio_external_id,
           tax_wrapper, management_style,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY as_of_day, silver_source_id, account_external_id
);

CREATE OR REPLACE MACRO report_accounts_history_multi() AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur
          FROM hist_acct_lines_base() l
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, acct,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, a.silver_source_id, a.account_external_id, a.account_kind, a.display_name,
               a.base_currency, a.relationship_id, a.nickname, a.account_category,
               a.portfolio_external_id,
               -- account-level display defaults (see report_accounts_multi).
               COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
               COALESCE(a.management_style, 'self_directed') AS management_style,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
          FROM agg g JOIN accounts a ON a.silver_source_id = g.src AND a.account_external_id = g.acct)
    SELECT day * 86400 AS as_of_day, silver_source_id, account_external_id, account_kind, display_name,
           base_currency, relationship_id, nickname, account_category, portfolio_external_id,
           tax_wrapper, management_style,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY as_of_day, silver_source_id, account_external_id
);

-- ---- positions family (0022 single-currency, 0031 multi) --------------

CREATE OR REPLACE MACRO report_positions_history(p_ccy) AS TABLE (
    WITH pv AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name, p.asset_class, p.currency,
               CAST(p.quantity AS VARCHAR) AS quantity, CAST(p.market_value AS VARCHAR) AS market_value,
               CAST(COALESCE(CASE WHEN p.currency = p_ccy THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * d.rate, p.market_value::DOUBLE * c1.rate * c2.rate,
                   p.market_value::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
          FROM positions p
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id
          ASOF LEFT JOIN fx_daily d  ON d.from_ccy = p.currency AND d.to_ccy = p_ccy AND d.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = p.currency AND c1.to_ccy = 'CHF' AND c1.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = p.currency AND u1.to_ccy = 'USD' AND u1.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (p.snapshot_at // 86400))
    SELECT ap.day * 86400 AS as_of_day, pv.src AS silver_source_id, pv.snap AS snapshot_at,
           pv.account_external_id, pv.display_name, pv.relationship_id, pv.nickname, pv.account_category,
           pv.position_key, pv.instrument_external_id, pv.symbol, pv.name, pv.asset_class, pv.currency,
           pv.quantity, pv.market_value, pv.value_outccy
      FROM pv JOIN hist_active_pos() ap
        ON ap.src = pv.src AND ap.acct = pv.account_external_id AND ap.snap = pv.snap
     ORDER BY as_of_day, pv.src, pv.account_external_id, pv.position_key
);

CREATE OR REPLACE MACRO report_positions_history_multi() AS TABLE (
    WITH pv AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name, p.asset_class, p.vehicle, p.currency,
               CAST(p.quantity AS DECIMAL(28,8)) AS quantity, CAST(p.market_value AS DECIMAL(28,4)) AS market_value,
               CAST(COALESCE(CASE WHEN p.currency = 'USD' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_usd.rate, p.market_value::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
               CAST(COALESCE(CASE WHEN p.currency = 'CHF' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_chf.rate, p.market_value::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
               CAST(COALESCE(CASE WHEN p.currency = 'EUR' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_eur.rate, p.market_value::DOUBLE * p_chf.rate * chf_eur.rate,
                   p.market_value::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
          FROM positions p
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = p.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = p.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = p.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (p.snapshot_at // 86400))
    SELECT ap.day * 86400 AS as_of_day, pv.src AS silver_source_id, pv.snap AS snapshot_at,
           pv.account_external_id, pv.display_name, pv.relationship_id, pv.nickname, pv.account_category,
           pv.position_key, pv.instrument_external_id, pv.symbol, pv.name, pv.asset_class, pv.vehicle, pv.currency,
           pv.quantity, pv.market_value, pv.value_usd, pv.value_chf, pv.value_eur
      FROM pv JOIN hist_active_pos() ap
        ON ap.src = pv.src AND ap.acct = pv.account_external_id AND ap.snap = pv.snap
     ORDER BY as_of_day, pv.src, pv.account_external_id, pv.position_key
);

-- ---- portfolios family (0024) -----------------------------------------

CREATE OR REPLACE MACRO report_portfolios_history(p_ccy) AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, ap.acct, ap.pid, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, ap.acct, ap.pid, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_port ap ON ap.src = cc.src AND ap.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.pid, l.is_cash, bk.base_currency, bk.tax_wrapper, bk.management_style,
               bk.display_name, bk.relationship_id, bk.nickname,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate, l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN portfolio_buckets() bk ON bk.src = l.src AND bk.pid = l.pid
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, pid,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o, CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, bk.src, bk.pid, bk.display_name, bk.base_currency, bk.relationship_id, bk.nickname,
               bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM agg g JOIN portfolio_buckets() bk ON bk.src = g.src AND bk.pid = g.pid)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, pid AS portfolio_external_id, display_name, base_currency,
           relationship_id, nickname, tax_wrapper, management_style,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY as_of_day, src, pid
);

CREATE OR REPLACE MACRO report_portfolios_history_multi() AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, ap.acct, ap.pid, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, ap.acct, ap.pid, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_port ap ON ap.src = cc.src AND ap.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.pid, l.is_cash, bk.base_currency,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN portfolio_buckets() bk ON bk.src = l.src AND bk.pid = l.pid
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, pid,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, bk.src, bk.pid, bk.display_name, bk.base_currency, bk.relationship_id, bk.nickname,
               bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
          FROM agg g JOIN portfolio_buckets() bk ON bk.src = g.src AND bk.pid = g.pid)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, pid AS portfolio_external_id, display_name, base_currency,
           relationship_id, nickname, tax_wrapper, management_style,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY as_of_day, src, pid
);

-- ---- sources family (0025) --------------------------------------------

CREATE OR REPLACE MACRO report_sources_history(p_ccy) AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    acct_src AS (SELECT silver_source_id AS src, account_external_id AS acct FROM accounts),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, a.acct, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_src a ON a.src = p.silver_source_id AND a.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, a.acct, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_src a ON a.src = cc.src AND a.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.is_cash, bk.base_currency,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate, l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN source_buckets() bk ON bk.src = l.src
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o, CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b
          FROM daily GROUP BY 1, 2),
    vals AS (
        SELECT g.day, bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM agg g JOIN source_buckets() bk ON bk.src = g.src)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, base_currency, tax_wrapper, management_style,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY as_of_day, src
);

CREATE OR REPLACE MACRO report_sources_history_multi() AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    acct_src AS (SELECT silver_source_id AS src, account_external_id AS acct FROM accounts),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, a.acct, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_src a ON a.src = p.silver_source_id AND a.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, a.acct, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_src a ON a.src = cc.src AND a.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.is_cash, bk.base_currency,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN source_buckets() bk ON bk.src = l.src
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e
          FROM daily GROUP BY 1, 2),
    vals AS (
        SELECT g.day, bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
          FROM agg g JOIN source_buckets() bk ON bk.src = g.src)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, base_currency, tax_wrapper, management_style,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY as_of_day, src
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (51, CAST(epoch(now()) AS BIGINT));
