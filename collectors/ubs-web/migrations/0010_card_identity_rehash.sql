-- The card ledger's identity carried the merchant text.
--
-- A card transaction's id was a hash over the card, the dates, the amounts
-- AND `merchant` — free text UBS re-labels between fetches. Nothing deletes
-- a card row (there is no DELETE FROM card_transactions anywhere in the
-- loader; every write is an upsert), so a re-label minted a second id and
-- left the first in place: one purchase, two rows, and gold counting the
-- spend twice. This is the defect fidelity-web's migration 0005 already
-- paid for on its own feed, where Fidelity's mutable description sat inside
-- the identity and a security re-label duplicated the transactions.
--
-- The key is now the card, the transaction and value dates, and the
-- amounts and currencies — no text. It collides ON PURPOSE where two
-- same-day, same-amount purchases differ only by merchant, and the
-- occurrence index separates those, exactly as chase's deposit content key
-- does. The merchant is still stored and still refreshed by the upsert; it
-- is simply no longer part of what makes a row that row. Do not put text
-- back in the key to resolve the collision.
--
-- Why this PURGES where chase's equivalent re-key REFUSED and asked for
-- `--force`: chase could tell the two eras apart in SQL (a bare FITID
-- against a `<product>:` prefix). Here both eras are `card:` followed by
-- sixteen hex characters — the prefix did not move, only what is hashed
-- under it — so no predicate can separate a pre-rehash row from a
-- post-rehash one, and a refusal check has nothing to test. Clearing is
-- the only thing that can be correct.
--
-- It is affordable for the reason 0008's clear was: the card surface has
-- no statement backfill (DESIGN §5.5), so the whole ledger is JSON that
-- the next load re-reads from bronze in full.
DELETE FROM card_transactions;

-- card_accounts and card_invoices are NOT cleared, unlike 0008: their ids
-- did not move. The next load re-observes them through their upserts.

-- And forget which dumps have been ingested, or the clear above is where
-- the card ledger ENDS: the loader skips a dump listed here, so the very
-- next `load` would report success over an empty table. Re-ingesting every
-- dump is safe — transactions upsert, snapshot tables dedup on their
-- primary key, documents key on the API's own token — so this costs the
-- time to re-read bronze and nothing else.
DELETE FROM dump_runs;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (10, CAST(strftime('%s','now') AS INTEGER));
