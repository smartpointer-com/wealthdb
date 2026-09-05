-- The spending reports: three views of the spending population, in the
-- shape the other report families already have — a single-currency
-- macro emitting VARCHAR money for the CLI, and a `_multi` sibling
-- emitting DECIMAL in USD/CHF/EUR for the web (migration 0024's
-- split).
--
--   report_spending_summary(f, t, ccy, period)        per period bucket
--   report_spending_categories(f, t, ccy, period, lvl) per bucket × category
--   report_spending_transactions(f, t, ccy)            per transaction
--
-- All three read `spending_lines_base` (migration 0041) and restate
-- none of its predicates: which accounts are in scope, which kinds are
-- spend, how a category resolves and that own-account moves are out
-- are all defined there, once.
--
-- Four things in here are decisions rather than mechanics:
--
-- * SIGN-SPLIT MAGNITUDES. `net_amount` is canonically signed — spend
--   negative, refunds and rewards positive — which makes a plain SUM
--   read as a small number that is neither a period's spending nor its
--   returns. The reports split the sign instead: `spend` and `refunds`
--   are both POSITIVE magnitudes and `net_spend = spend - refunds`, so
--   a month's gross outflow and what came back are separately legible
--   and their difference is the number a budget cares about.
--
-- * PERIOD BUCKETING THROUGH date_trunc. The bucket part is a macro
--   parameter, which DuckDB accepts (verified on 1.5.x, including as a
--   GROUP BY key). `total` is NOT a date_trunc part, and the engine
--   binds the call even on the branch a CASE would discard — a bare
--   `CASE WHEN p_period = 'total' THEN NULL ELSE date_trunc(p_period,
--   …) END` raises `extract specifier "total" not recognized`. So
--   `spend_period_bucket` substitutes a valid part INSIDE the
--   date_trunc call and nulls the result outside it. The timestamp
--   comes from `epoch_ms`, not `to_timestamp`, because the latter
--   yields TIMESTAMP WITH TIME ZONE and would truncate in the
--   session's zone rather than UTC.
--
-- * SHARE OVER MAGNITUDES. A category's share is |net_spend| over the
--   bucket's Σ|net_spend|. Signed shares would let a net-positive
--   category (a month whose refunds beat its purchases) shrink the
--   denominator and push the other categories past 100%; over
--   magnitudes the shares are non-negative and sum to exactly 1.
--
-- * `(uncategorized)` IS A LABEL, NOT A NULL. The backlog is a real
--   bucket — the model tier's work list, and the share of a period whose
--   category is simply unknown — so it is materialised BEFORE the
--   GROUP BY and grouped like any other category. Left as NULL it
--   would render as a blank row in every consumer and read as a
--   rendering fault rather than as the number it is.
--
-- The migration also re-issues `report_transactions` /
-- `report_transactions_multi` with the merchant name and the resolved
-- spend category, so `wealthdb transactions` and the Metabase
-- transaction model can show what a row was categorised as without
-- joining the overlay by hand. web_transactions (migration 0032, last
-- re-issued by 0039) reads the multi macro; it is dropped and
-- re-created around the swap on 0039's pattern, so the view is rebound
-- to the macro it now reads rather than left holding a stale
-- definition. Its own column list is unchanged.
--
-- LOCKSTEP: gold.TransactionsBetween runs `SELECT *` over
-- report_transactions with a POSITIONAL rows.Scan, so the three new
-- columns land in gold.TransactionRow and in that scan list in the
-- same change (0039 made the same edit for account_kind).
--
-- IF NOT EXISTS / OR REPLACE / DROP … IF EXISTS throughout: the
-- statements keep this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note).

DROP VIEW IF EXISTS web_transactions;

-- ============================================================
-- The category resolution, in one place
-- ============================================================

-- spend_txn_categories: the resolved category of every enriched
-- transaction, keyed like the enrichment table. COALESCE(transaction
-- scope, merchant scope) — a per-transaction verdict beats the
-- merchant-wide one — with the merchant store reached THROUGH the
-- enrichment row's signature and the primary read from the seeded
-- dimension rather than derived from the detailed value's spelling.
--
-- Extracted from spending_lines_base, which is re-issued below to read
-- it: the transaction reports need the same resolution over a
-- population the spending base deliberately excludes rows from (an
-- own-account move is not spend, but `wealthdb transactions` must
-- still say that is what it was), and two copies of a precedence rule
-- are two things to keep in step.
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature, m.merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed) AS spend_detailed,
           c.spend_primary, e.provenance
      FROM spend_txn_enrichment e
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed)
);

-- spending_lines_base: unchanged in columns and semantics from
-- migration 0041 — the enrichment population with its category
-- resolved and its own-account moves removed — rebuilt on
-- spend_txn_categories so the resolution has one definition.
CREATE OR REPLACE MACRO spending_lines_base(p_from, p_to) AS TABLE (
    SELECT p.silver_source_id, p.transaction_external_id, p.occurred_at,
           p.account_external_id, p.account_kind, p.display_name,
           p.nickname, p.account_category,
           p.kind, p.currency, p.net_amount, p.description,
           p.counterparty, p.provider_category,
           c.merchant_signature, c.merchant_name, c.spend_detailed,
           c.spend_primary, c.provenance
      FROM spend_enrichment_population(p_from, p_to) p
      LEFT JOIN spend_txn_categories() c
             ON c.silver_source_id        = p.silver_source_id
            AND c.transaction_external_id = p.transaction_external_id
     WHERE c.spend_detailed IS DISTINCT FROM 'internal_transfer'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

-- ============================================================
-- Period bucketing and the valued line bases
-- ============================================================

-- spend_period_bucket: the UTC-midnight epoch second that opens the
-- bucket `p_at` falls in, or NULL for the single `total` bucket. See
-- the header for why `total` never reaches date_trunc and why the
-- timestamp comes from epoch_ms. Any other part is passed through to
-- date_trunc, so an unrecognised one raises rather than bucketing
-- wrongly; validating the choice belongs to the caller.
CREATE OR REPLACE MACRO spend_period_bucket(p_period, p_at) AS (
    CASE WHEN p_period = 'total' THEN NULL
         ELSE CAST(epoch(date_trunc(
                  CASE WHEN p_period = 'total' THEN 'month' ELSE p_period END,
                  epoch_ms(p_at * 1000))) AS BIGINT)
    END
);

-- spending_lines_outccy: the spending base with each line's native
-- net_amount converted to p_ccy at its own occurred_at day. Same
-- COALESCE/ASOF shape as every other single-currency report macro
-- (identity, direct, via-CHF, via-USD); NULL when no path resolves.
CREATE OR REPLACE MACRO spending_lines_outccy(p_from, p_to, p_ccy) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
      FROM spending_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = b.currency AND d.to_ccy  = p_ccy AND d.day  <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
);

-- spending_lines_multi: the same base valued in all three reporting
-- currencies at once, sharing the pivot legs (migration 0024's
-- factoring: ccy->{CHF,USD,EUR} plus the four crosses, rather than
-- three independent conversions).
CREATE OR REPLACE MACRO spending_lines_multi(p_from, p_to) AS TABLE (
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
      FROM spending_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF'      AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF'      AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD'      AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD'      AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.occurred_at // 86400)
);

-- ============================================================
-- report_spending_summary
-- ============================================================

-- One row per period bucket: how many spending lines it holds, the
-- gross outflow, what came back, and the difference.
--
-- NULL-vs-0 follows the other aggregate reports: a bucket only exists
-- because it has lines, so a bucket whose lines all failed to convert
-- reads NULL (no FX path), while a bucket that simply has no refunds
-- reads 0. An unconvertible line inside an otherwise convertible
-- bucket contributes nothing rather than voiding the bucket.
CREATE OR REPLACE MACRO report_spending_summary(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*)                AS txn_count,
               COUNT(value_outccy)     AS n_converted,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS spend_raw,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS refunds_raw
          FROM spending_lines_outccy(p_from, p_to, p_ccy)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE spend_raw   END AS DECIMAL(28,4)) AS spend_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE refunds_raw END AS DECIMAL(28,4)) AS refunds_val
          FROM agg)
    SELECT period_start, txn_count,
           CAST(spend_val   AS VARCHAR) AS spend,
           CAST(refunds_val AS VARCHAR) AS refunds,
           CAST(spend_val - refunds_val AS VARCHAR) AS net_spend
      FROM vals
     ORDER BY period_start
);

CREATE OR REPLACE MACRO report_spending_summary_multi(p_from, p_to, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*) AS txn_count,
               COUNT(value_usd) AS n_usd, COUNT(value_chf) AS n_chf, COUNT(value_eur) AS n_eur,
               SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS spend_usd_raw,
               SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS spend_chf_raw,
               SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS spend_eur_raw,
               SUM(CASE WHEN value_usd > 0 THEN  value_usd ELSE 0 END) AS refunds_usd_raw,
               SUM(CASE WHEN value_chf > 0 THEN  value_chf ELSE 0 END) AS refunds_chf_raw,
               SUM(CASE WHEN value_eur > 0 THEN  value_eur ELSE 0 END) AS refunds_eur_raw
          FROM spending_lines_multi(p_from, p_to)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE spend_usd_raw   END AS DECIMAL(28,4)) AS spend_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE spend_chf_raw   END AS DECIMAL(28,4)) AS spend_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE spend_eur_raw   END AS DECIMAL(28,4)) AS spend_eur,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE refunds_usd_raw END AS DECIMAL(28,4)) AS refunds_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE refunds_chf_raw END AS DECIMAL(28,4)) AS refunds_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE refunds_eur_raw END AS DECIMAL(28,4)) AS refunds_eur
          FROM agg)
    SELECT period_start, txn_count,
           spend_usd, spend_chf, spend_eur,
           refunds_usd, refunds_chf, refunds_eur,
           CAST(spend_usd - refunds_usd AS DECIMAL(28,4)) AS net_spend_usd,
           CAST(spend_chf - refunds_chf AS DECIMAL(28,4)) AS net_spend_chf,
           CAST(spend_eur - refunds_eur AS DECIMAL(28,4)) AS net_spend_eur
      FROM vals
     ORDER BY period_start
);

-- ============================================================
-- report_spending_categories
-- ============================================================

-- One row per (period bucket, category), plus each category's share of
-- the bucket. p_level picks the grouping vocabulary: 'primary' rolls
-- up to the coarse buckets (the deltas are primary-level, so they
-- group as themselves), anything else groups by the detailed value.
-- Both levels label an unresolved category `(uncategorized)` before
-- the GROUP BY, so the backlog is a bucket rather than a blank.
--
-- Σ over the categories of a bucket equals that bucket's summary row
-- by construction: both aggregate the same lines, split the same way.
CREATE OR REPLACE MACRO report_spending_categories(p_from, p_to, p_ccy, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary ELSE spend_detailed END,
                        '(uncategorized)') AS category,
               value_outccy
          FROM spending_lines_outccy(p_from, p_to, p_ccy)),
    agg AS (
        SELECT period_start, category,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS spend_raw,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS refunds_raw
          FROM labelled
         GROUP BY 1, 2),
    vals AS (
        SELECT period_start, category, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE spend_raw   END AS DECIMAL(28,4)) AS spend_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE refunds_raw END AS DECIMAL(28,4)) AS refunds_val
          FROM agg),
    net AS (
        SELECT period_start, category, txn_count, spend_val, refunds_val,
               CAST(spend_val - refunds_val AS DECIMAL(28,4)) AS net_val
          FROM vals)
    SELECT period_start, category, txn_count,
           CAST(spend_val   AS VARCHAR) AS spend,
           CAST(refunds_val AS VARCHAR) AS refunds,
           CAST(net_val     AS VARCHAR) AS net_spend,
           ABS(net_val)::DOUBLE
               / NULLIF(SUM(ABS(net_val)) OVER (PARTITION BY period_start), 0) AS share
      FROM net
     ORDER BY period_start, net_val DESC, category
);

CREATE OR REPLACE MACRO report_spending_categories_multi(p_from, p_to, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary ELSE spend_detailed END,
                        '(uncategorized)') AS category,
               value_usd, value_chf, value_eur
          FROM spending_lines_multi(p_from, p_to)),
    agg AS (
        SELECT period_start, category,
               COUNT(*) AS txn_count,
               COUNT(value_usd) AS n_usd, COUNT(value_chf) AS n_chf, COUNT(value_eur) AS n_eur,
               SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS spend_usd_raw,
               SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS spend_chf_raw,
               SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS spend_eur_raw,
               SUM(CASE WHEN value_usd > 0 THEN  value_usd ELSE 0 END) AS refunds_usd_raw,
               SUM(CASE WHEN value_chf > 0 THEN  value_chf ELSE 0 END) AS refunds_chf_raw,
               SUM(CASE WHEN value_eur > 0 THEN  value_eur ELSE 0 END) AS refunds_eur_raw
          FROM labelled
         GROUP BY 1, 2),
    vals AS (
        SELECT period_start, category, txn_count,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE spend_usd_raw   END AS DECIMAL(28,4)) AS spend_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE spend_chf_raw   END AS DECIMAL(28,4)) AS spend_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE spend_eur_raw   END AS DECIMAL(28,4)) AS spend_eur,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE refunds_usd_raw END AS DECIMAL(28,4)) AS refunds_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE refunds_chf_raw END AS DECIMAL(28,4)) AS refunds_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE refunds_eur_raw END AS DECIMAL(28,4)) AS refunds_eur
          FROM agg),
    net AS (
        SELECT *,
               CAST(spend_usd - refunds_usd AS DECIMAL(28,4)) AS net_spend_usd,
               CAST(spend_chf - refunds_chf AS DECIMAL(28,4)) AS net_spend_chf,
               CAST(spend_eur - refunds_eur AS DECIMAL(28,4)) AS net_spend_eur
          FROM vals)
    SELECT period_start, category, txn_count,
           spend_usd, spend_chf, spend_eur,
           refunds_usd, refunds_chf, refunds_eur,
           net_spend_usd, net_spend_chf, net_spend_eur,
           ABS(net_spend_usd)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_usd)) OVER (PARTITION BY period_start), 0) AS share_usd,
           ABS(net_spend_chf)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_chf)) OVER (PARTITION BY period_start), 0) AS share_chf,
           ABS(net_spend_eur)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_eur)) OVER (PARTITION BY period_start), 0) AS share_eur
      FROM net
     ORDER BY period_start, net_spend_usd DESC, category
);

-- ============================================================
-- report_spending_transactions
-- ============================================================

-- The spending population at transaction grain — what a summary or a
-- category row is made of, with the merchant, the resolved category
-- and the tier that decided it, so a surprising bucket can be opened.
-- Category columns stay NULL here rather than carrying the
-- `(uncategorized)` label: at transaction grain a NULL is not a
-- rendering problem, and the consumers that group (the categories
-- macro, web_spending) label it themselves.
CREATE OR REPLACE MACRO report_spending_transactions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, merchant_signature, merchant_name, spend_primary, spend_detailed,
           provenance, currency,
           CAST(net_amount AS VARCHAR) AS net_amount,
           description, counterparty,
           CAST(value_outccy AS VARCHAR) AS value_outccy
      FROM spending_lines_outccy(p_from, p_to, p_ccy)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE OR REPLACE MACRO report_spending_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, merchant_signature, merchant_name, spend_primary, spend_detailed,
           provenance, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur
      FROM spending_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

-- ============================================================
-- report_transactions / report_transactions_multi, re-issued
-- ============================================================

-- report_transactions: every transaction in [p_from, p_to], net_amount
-- converted to p_ccy at occurred_at, now carrying the merchant name and
-- the resolved spend category (NULL for everything the enrichment pass
-- does not reach — investment rows, and rows outside the spending
-- account scope). Always ascending; the caller re-orders for `-r`.
CREATE OR REPLACE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
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

-- report_transactions_multi: every transaction in [p_from, p_to] with net
-- amount in USD/CHF/EUR at occurred_at, plus the same merchant / category
-- columns.
CREATE OR REPLACE MACRO report_transactions_multi(p_from, p_to) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS DECIMAL(28,4)) AS gross_amount, CAST(b.net_amount AS DECIMAL(28,4)) AS net_amount,
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.price AS DECIMAL(28,8)) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           CAST(COALESCE(CASE WHEN b.currency = 'USD' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_usd.rate, b.net_amount::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
           CAST(COALESCE(CASE WHEN b.currency = 'CHF' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_chf.rate, b.net_amount::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
           CAST(COALESCE(CASE WHEN b.currency = 'EUR' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_eur.rate, b.net_amount::DOUBLE * p_chf.rate * chf_eur.rate,
               b.net_amount::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
      FROM base b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

-- Per-transaction converted values (the income / cost flow charts).
-- All kinds and all account kinds; the cards filter to the kind families
-- they chart and fence out the account kinds that do not belong there.
CREATE OR REPLACE VIEW web_transactions AS
    SELECT CAST(to_timestamp(occurred_at) AS TIMESTAMP) AS occurred_at,
           kind, silver_source_id, account_kind,
           value_usd, value_chf, value_eur
      FROM report_transactions_multi(0, 9223372036854775807);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (42, CAST(epoch(now()) AS BIGINT));
