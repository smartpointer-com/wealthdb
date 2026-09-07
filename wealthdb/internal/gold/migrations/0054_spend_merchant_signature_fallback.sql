-- A line with no verdict still names its merchant.
--
-- `merchant_name` resolved to the merchant STORE's name for the line's
-- signature, and the store is written by the model tier alone. The
-- model tier is the weakest scope in the lattice (docs/SPENDING.md §3),
-- so every line a stronger tier placed — a card row the provider filed
-- under its merchant category, a row a rule or a pin named, a row
-- nothing placed at all — is never a model candidate, never acquires a
-- store row, and showed a blank merchant even though the enrichment
-- pass recorded a perfectly good signature for it. The signature IS a
-- merchant identity: it is the fold the store would have been keyed by.
--
-- One statement, and nothing else changes:
--
--   * spend_txn_categories is re-issued with the column list, the types
--     and every other expression byte-identical to migration 0052's
--     issue. merchant_name resolves in three steps: the enrichment's
--     merchant_label when the resolved category is a delta (0048 +
--     0052, unchanged — a delta line is not a merchant transaction, and
--     a card bill names the issuer it was paid to), otherwise the
--     store's name, otherwise the line's own merchant_signature.
--     NULLIF over TRIM keeps an empty — or blank — signature from
--     rendering as an empty merchant, and a line with no signature
--     still shows nothing. A signature carries no outer space
--     (spending.Normalize joins tokens), so the TRIM changes nothing
--     reachable and closes the case if the normaliser ever widens.
--
-- The fallback is deliberately NOT restricted to a resolved category. A
-- line no scope placed has no dimension row to test, falls to the ELSE
-- branch and takes the store's name — NULL by construction, a store row
-- being exactly what would have resolved it — so it now shows its
-- signature too, which is what the backlog is made of.
--
-- It is not restricted by CANDIDACY either, and that is a widening.
-- spending.Uninformative and spending.FilingOnly fence a fold that is
-- nothing but a booking code, or nothing but the provider's own filing
-- of the row, out of the MODEL — a verdict bought at such a key would
-- cover every row the bank filed that way — but spending.Normalize
-- deliberately stores that fold rather than an empty signature, and
-- those gates guard the store, not this column. So a line whose whole
-- narrative was the bank's tag surfaces, and ranks, under that tag.
-- The alternative is a blank, which says less and hides the line from
-- the reader who could recognise it (docs/SPENDING.md §7).
--
-- VERBATIM, not cased for display. The cell is then the fold itself:
-- the exact value of the merchant_signature column beside it, where
-- casing would print a string that exists nowhere else in gold. The
-- upper-case fold also labels itself — a reader tells a model-written
-- name (prose) from a derived fallback without reading the provenance
-- column — and identity is stable by construction, so one signature
-- always renders one way and a report groups it as one row. A display
-- caser would additionally risk folding two distinct signatures, or a
-- signature and a store name, onto one label, merging rows the
-- resolution holds apart. (`initcap` does not exist in this DuckDB in
-- any case; a hand-rolled list_transform caser inside a macro every
-- report reads would be cost and risk for a worse answer on acronyms
-- and brand casing.)
--
-- Nothing downstream is re-issued: spending_lines_base, the three
-- spending reports and their _multi siblings, report_transactions,
-- report_transactions_multi, the web views and both CLI views read
-- merchant_name from this macro by name, a view binds its macros
-- lazily, and no projection moves (0045's precedent, 0048's and 0052's
-- notes).
--
-- The dashboard's two merchant rankings are the consumers that see the
-- most change, and they need none: both already exclude the delta
-- CATEGORIES rather than reading a blank merchant as "not a delta"
-- (0052), so the lines this migration names rank as the merchants they
-- are, and an unresolved line — whose two category columns are equal at
-- '(uncategorized)' in web_spending, which is what the MBQL model reads
-- — stays outside the ranking as before. The ranked population widens
-- from the lines the store named to every non-delta line carrying a
-- signature, so the ranks, and rank 1's share, move with it.
--
-- PRIVACY is unmoved: merchant_name is PrivacyFreeText on every
-- surface, which masks the cell whole. The column is free text rather
-- than a name precisely for this — a signature the transfer fence
-- refused a store verdict, a person-to-person payment whose narrative
-- carries a payee's name where a merchant would be, now surfaces that
-- fold here on the non-private view, exactly as `merchant_signature`
-- and `counterparty` already do.
--
-- CREATE OR REPLACE keeps this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note).
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN e.merchant_label
                ELSE COALESCE(m.merchant_name, NULLIF(TRIM(e.merchant_signature), ''))
           END AS merchant_name,
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
    VALUES (54, CAST(epoch(now()) AS BIGINT));
