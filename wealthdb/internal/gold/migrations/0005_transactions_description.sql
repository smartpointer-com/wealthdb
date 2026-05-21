-- ============================================================
-- gold schema, migration 0005 — transactions.description column.
--
-- Adds a free-text label column on `transactions` so an adapter
-- can carry per-row human-readable context that doesn't fit the
-- structured instrument_external_id join.
--
-- Motivating cases:
--
--   1. Schwab API dividends. The DIVIDEND_OR_INTEREST payload's
--      transferItems contains only a cash leg (assetType=
--      CURRENCY) — no CUSIP or symbol — but the top-level
--      `description` field carries the security name verbatim
--      ("VANGUARD TOTAL STOCK MKT ETF"). The adapter passes that
--      through here so the row is identifiable in the
--      `wealthdb transactions` table without needing a fuzzy
--      match back to the instruments table.
--
--   2. UBS PSN cash_movement events. The MT940 narrative is free-
--      form text; we extract a single-line summary into here as
--      a fallback for rows where the instrument isn't otherwise
--      resolvable (e.g. dividend credits whose ISIN doesn't
--      appear in the narrative).
--
-- The column is purely informational — gold rollups don't join
-- on it. The transactions CLI uses it as a fallback for the
-- `name` column when instruments.name is NULL.
-- ============================================================

ALTER TABLE transactions ADD COLUMN description TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (5, CAST(epoch(now()) AS BIGINT));
