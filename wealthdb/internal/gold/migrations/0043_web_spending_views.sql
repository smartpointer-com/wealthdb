-- Serving views for the web's spending surface, in the shape migration
-- 0032 established: a TIMESTAMP-cast reduction of a `_multi` report
-- macro to the grain and columns the cards read, so Metabase syncs it
-- like a table and its columns get field ids the dashboard pickers can
-- land on.
--
-- Two views, and the second needs a macro of its own.
--
-- web_spending is a thin wrapper: transaction grain over
-- report_spending_transactions_multi (migration 0042), with the
-- account's identity for the account picker and the two category
-- levels for the breakdown cards.
--
-- web_card_balances_history is NOT thin, and deliberately does NOT
-- come from the account-history macros. Those gate each day on ONE
-- active snapshot per SOURCE (`hist_active_cash`: the source's latest
-- snapshot_at at-or-before end-of-day, joined by equality), which is
-- right for a source that dumps every account together and wrong for a
-- source that does not. A card's balances arrive on the statement
-- clock — one closing per cycle — while the deposit accounts of the
-- same login are re-dumped on every run, so:
--
--   * on a day whose newest snapshot is a deposit move, the card's
--     statement closing is not the active snapshot and the card
--     vanishes from the series entirely; and
--   * on the rarer day whose newest snapshot IS the card closing, the
--     deposit rows vanish instead.
--
-- Both directions are reproducible against the shipped macros (a card
-- closing on day N and a deposit move on day N+1 yield exactly one
-- account per day, alternating). Carrying a balance forward per
-- account therefore needs each account's OWN series: the spine is
-- crossed with the (source, account, currency) keys and ASOF-joined
-- per key, so one account's dump day cannot hide another's.
--
-- Zero balances are KEPT here, unlike `cash_chosen` / the history line
-- bases which drop `amount <> 0` rows as noise. A paid-off card really
-- is at zero, and dropping the row would leave the carry-forward
-- showing the last balance it owed forever.
--
-- CREATE OR REPLACE throughout keeps this replayable for the DDL-rerun
-- test (see gold.Migrate's REPLAY note). A view binds its macros lazily on
-- this engine, so a later migration CAN replace one of these macros
-- underneath its view — but it must still drop and re-create the view
-- (0039's pattern) whenever the view's own projection changes with it,
-- since a re-created view is the only way its column list moves.

-- Every spending line with its converted values and the dimensions the
-- spending cards break down by. `(uncategorized)` is rendered HERE
-- rather than in the macro: at transaction grain the macro leaves the
-- category NULL (it is genuinely unknown), but a Metabase breakdown
-- would show that as an unlabelled slice, which reads as a rendering
-- fault instead of as the backlog it is.
CREATE OR REPLACE VIEW web_spending AS
    SELECT CAST(to_timestamp(occurred_at) AS TIMESTAMP) AS occurred_at,
           silver_source_id, account_external_id, display_name, account_kind,
           merchant_name,
           COALESCE(spend_primary,  '(uncategorized)') AS spend_primary,
           COALESCE(spend_detailed, '(uncategorized)') AS spend_detailed,
           value_usd, value_chf, value_eur
      FROM report_spending_transactions_multi(0, 9223372036854775807);

-- report_card_balances_history_multi: one row per (card account,
-- currency, UTC day) from that account's first balance to today, the
-- balance carried forward between its own snapshots and valued in
-- USD/CHF/EUR. Card balances are negative (owed money is a liability,
-- the margin-debit precedent), so the series runs below zero.
--
-- Each balance is valued ONCE at its own snapshot's FX day and then
-- expanded onto the daily spine, the same way the history macros do
-- it: today's row equals the latest balance, and per-day compute stays
-- cheap. The pivot legs are shared across the three currencies
-- (migration 0024's factoring).
--
-- The balance_kind ladder is the one `cash_chosen` uses — a card
-- publishes both a live `current` balance and per-cycle `closing`
-- ones, and on a day carrying both the live figure wins.
CREATE OR REPLACE MACRO report_card_balances_history_multi() AS TABLE (
    WITH card_rows AS (
        SELECT src, snap, acct, ccy, amt FROM (
            SELECT cb.silver_source_id AS src, cb.snapshot_at AS snap,
                   cb.account_external_id AS acct, cb.currency AS ccy, cb.amount AS amt,
                   ROW_NUMBER() OVER (
                       PARTITION BY cb.silver_source_id, cb.snapshot_at,
                                    cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind
                           WHEN 'current' THEN 1 WHEN 'closing' THEN 2
                           WHEN 'available' THEN 3 WHEN 'aggregated' THEN 4
                           WHEN 'opening' THEN 5 WHEN 'initial' THEN 6
                           WHEN 'projected' THEN 7 ELSE 99 END, cb.balance_kind) AS rn
              FROM cash_balances cb
              JOIN accounts a ON a.silver_source_id    = cb.silver_source_id
                             AND a.account_external_id = cb.account_external_id
             WHERE a.account_kind = 'card')
        WHERE rn = 1),
    valued AS (
        SELECT c.src, c.snap, c.acct, c.ccy, c.amt,
               CAST(COALESCE(CASE WHEN c.ccy = 'USD' THEN c.amt::DOUBLE END,
                   c.amt::DOUBLE * p_usd.rate,
                   c.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS bal_usd,
               CAST(COALESCE(CASE WHEN c.ccy = 'CHF' THEN c.amt::DOUBLE END,
                   c.amt::DOUBLE * p_chf.rate,
                   c.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS bal_chf,
               CAST(COALESCE(CASE WHEN c.ccy = 'EUR' THEN c.amt::DOUBLE END,
                   c.amt::DOUBLE * p_eur.rate,
                   c.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   c.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS bal_eur
          FROM card_rows c
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = c.ccy   AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (c.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = c.ccy   AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (c.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = c.ccy   AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (c.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF'   AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (c.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF'   AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (c.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD'   AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (c.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD'   AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (c.snap // 86400)),
    spine AS (
        SELECT d.day, k.src, k.acct, k.ccy
          FROM hist_days() d
          CROSS JOIN (SELECT DISTINCT src, acct, ccy FROM card_rows) k)
    SELECT s.day * 86400 AS as_of_day, v.src AS silver_source_id,
           v.acct AS account_external_id, a.display_name, v.ccy AS currency,
           CAST(v.amt AS DECIMAL(28,4)) AS balance,
           v.bal_usd AS balance_usd, v.bal_chf AS balance_chf, v.bal_eur AS balance_eur
      FROM spine s
      -- Per-key ASOF: each card carries ITS OWN last balance forward,
      -- so another account's dump day cannot displace it.
      ASOF JOIN valued v ON v.src = s.src AND v.acct = s.acct AND v.ccy = s.ccy
                        AND v.snap <= s.day * 86400 + 86399
      LEFT JOIN accounts a ON a.silver_source_id = v.src AND a.account_external_id = v.acct
     ORDER BY as_of_day, silver_source_id, account_external_id, currency
);

CREATE OR REPLACE VIEW web_card_balances_history AS
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP) AS as_of_day,
           silver_source_id, account_external_id, display_name, currency,
           balance, balance_usd, balance_chf, balance_eur
      FROM report_card_balances_history_multi();

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (43, CAST(epoch(now()) AS BIGINT));
