-- A line the merchant store placed reads provenance 'model'.
--
-- provenance names the tier that decided the category. Five of the six
-- tiers write their name into spend_txn_enrichment.provenance, and the
-- CHECK on that column (migration 0041) admits exactly those five:
-- matcher, rule, provider, manual, and signature-only for a row the
-- pass reached but could not place. The sixth tier — the model — does
-- not write to that table at all: it buys a verdict per merchant
-- signature and stores it in spend_merchant_categories, and the
-- resolution across the two scopes happens here, in the macro.
--
-- So a row the store answered stayed stamped 'signature-only', the
-- same value an uncategorised row carries, and the column could not
-- tell a model verdict from the backlog.
--
-- It is resolved where the resolution lives rather than in the table.
-- The overlay's value is correct at table level (the pass really could
-- not place the row), the CHECK stays closed — DuckDB cannot widen one
-- in place — and 'model' is derived exactly where the store's verdict
-- is: the resolved category comes from the store precisely when the
-- transaction scope is silent and the store is not.
--
-- One statement, same column shape as migration 0048's issue, one
-- expression changed. spending_lines_base, spending_lines_outccy /
-- _multi and report_spending_transactions(_multi) read provenance by
-- name, so they need no re-issue (0048's own note). The model tier's
-- backlog reads the resolved category the same way a report does, so
-- candidacy is unmoved.
--
-- CREATE OR REPLACE keeps this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note).
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN NULL
                ELSE m.merchant_name END AS merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed) AS spend_detailed,
           c.spend_primary,
           CASE WHEN e.spend_detailed IS NULL AND m.spend_detailed IS NOT NULL
                THEN 'model' ELSE e.provenance END AS provenance
      FROM spend_txn_enrichment e
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (50, CAST(epoch(now()) AS BIGINT));
