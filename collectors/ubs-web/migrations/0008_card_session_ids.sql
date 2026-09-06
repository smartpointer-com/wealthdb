-- The card surface was keyed on UBS's session-scoped handles.
--
-- Every id the card API hands out — an account's `id`, a card's `_id`, a
-- ledger row's `_id`, an invoice's `id` — is re-minted at login. Two
-- dumps taken a day apart shared NOT ONE ledger id, so the
-- ON CONFLICT that was meant to re-observe a row inserted a second copy
-- of it instead, and gold counted every card purchase twice.
--
-- The loader now mints these ids from row content and identifies an
-- account by its `accountNumber` (card_parsers.stable_account_ids), so
-- a re-download re-observes rather than re-inserts. The rows already
-- stored carry the old ids and cannot be re-keyed in SQL — the new id
-- is a hash the loader computes — so they are cleared here. The card
-- dump is a complete history every run, so the next `load` restores
-- them whole, deduplicated.
--
-- card_statements is left alone: it is keyed on the PDF's own
-- content_sha256, which never had this problem. Its invoice reference
-- is rewritten by the same load that repopulates the invoices.
DELETE FROM card_transactions;
DELETE FROM card_invoices;
DELETE FROM card_accounts;

-- And forget which dumps have been ingested, or the clear above is where
-- the card surface ENDS: the loader skips a dump listed here, so the very
-- next `load` would report success over three empty tables. Re-ingesting
-- every dump is safe — transactions upsert, snapshot tables dedup on their
-- primary key, documents key on the API's own token — so this costs the
-- time to re-read bronze and nothing else.
DELETE FROM dump_runs;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (8, CAST(strftime('%s','now') AS INTEGER));
