-- ============================================================
-- chase silver schema, migration 0002 — credit cards.
--
-- Extends the deposit-only 0001 schema with the credit-card shape: a
-- product discriminator on the account roster, the card-only columns the
-- card export carries and the deposit export does not, and the per-period
-- statement balances the card statement pass records.
--
-- Both products share the `accounts` and `transactions` tables rather than
-- getting their own: the rows are the same kind of thing (an account
-- observation, a posted ledger event), the columns overlap almost entirely,
-- and every generic query (the export seam, the statement inventory, the
-- idempotence gate) then keeps working unchanged. `product` is what tells
-- them apart.
--
-- Sign convention, unchanged from 0001 and from bronze: amounts are stored
-- exactly as the provider reported them. On a card that means spend is
-- NEGATIVE and anything reducing the balance owed (a payment, a return, a
-- fee reversal) is POSITIVE, while the account's `balance` is the POSITIVE
-- amount owed. Silver never normalises a liability; negating it is the gold
-- adapter's job.
--
-- Identifier convention, replacing 0001's `fitid` note for BOTH products. An
-- export row's `fitid` is
-- `<product>:<account_external_id>:<content key>:<occurrence>`, where the
-- content key hashes the fields both of that product's exports state
-- identically and `occurrence` counts earlier rows of the same account
-- sharing that key. The key is (account, post date, amount) on a deposit row
-- and those plus the descriptor on a card row — the deposit exports state the
-- descriptor differently (QFX `<NAME>` + `<MEMO>` against one longer CSV
-- `Description`), so it cannot be keyed on there. Statement-sourced rows keep
-- their own `stmt_` ids, which the documents themselves determine.
--
-- The key is deliberately NOT the provider's OFX `<FITID>`, which is carried
-- in `payload.fitid` for traceability only. The FITID exists only in the QFX
-- export, and each format is fetched separately and can fail on its own, so
-- keying on it gives the same rows a different identity in a run that landed
-- only the CSV and permanently doubles that account's ledger. On a card there
-- is a second reason: the FITID is not unique even within one export — a
-- credit that offsets a charge is issued the same FITID as the charge it
-- reverses. A content key is stable across both failure modes, and the
-- occurrence index separates the genuine content collisions (same day, same
-- amount) that both ledgers really produce. Ids converge across re-loads but
-- are not durable external references: a re-priced row is new content and
-- gets a new id.
--
-- No schema change carries the deposit half of this: ids are loader output,
-- not schema. A silver DB built before this migration still holds its deposit
-- rows under the old FITID-derived ids, and this migration does not rewrite
-- them; an incremental `load` would insert every deposit row again under its
-- new id beside the old one (the ledger INSERT OR IGNOREs on `fitid`). Such a
-- DB must be rebuilt with `load --force` and gold reloaded from it, and
-- `load` enforces that rather than leaving it to a comment: it counts the
-- export rows whose ids carry neither product prefix and exits with that
-- instruction (`_refuse_pre_0002_ids`). The check lives in the loader, not
-- here, so it also catches a DB that has already absorbed one such load.
-- DESIGN.md ("Deposit ids are loader output, not schema") records the change
-- itself.
-- ============================================================

-- 'dda' (checking / savings) | 'card' (credit card). The default makes the
-- migration a no-op for the rows already in silver, which are deposits by
-- construction — silver carried no card before this migration.
ALTER TABLE accounts ADD COLUMN product TEXT NOT NULL DEFAULT 'dda';

-- Unposted card activity: already reflected in `balance`, absent from the
-- exports (which carry posted rows only). It is the whole gap between a
-- balance reconstructed from the ledger and the balance the card reports
-- live, so keeping it turns that gap into an identity. NULL on a deposit
-- account, and on a card whose detail block omitted it.
ALTER TABLE accounts ADD COLUMN pending_charges REAL;

-- A card row posts on a different day than it was transacted on
-- (`posted_at` is the post date, as on a deposit row). NULL on a deposit
-- row and on a card row the CSV export did not cover.
ALTER TABLE transactions ADD COLUMN txn_date INTEGER;
-- The card descriptor — the merchant, as the QFX `<NAME>` reports it (the
-- CSV `Description` is the same string with its commas replaced by spaces).
ALTER TABLE transactions ADD COLUMN merchant TEXT;
-- The provider's own `Category` for the row, stored verbatim: Chase
-- publishes more values than any one login produces, so this is free text,
-- not an enum. Empty on payments, which Chase leaves uncategorised.
ALTER TABLE transactions ADD COLUMN category TEXT;
-- Per-row currency. NOT dead: chase never populates it — neither export
-- format carries a currency or an original-amount field, and the
-- relationship is USD throughout — but a card ledger is the natural home
-- for a foreign-currency row, and this is the landing spot for a later
-- multi-currency card source. Leave it in place; NULL means "the account's
-- currency" (accounts.currency).
ALTER TABLE transactions ADD COLUMN currency TEXT;

-- ============================================================
-- EVENT TABLE — statement_balances
-- One row per account per statement period: the balance the statement
-- printed at the period's open and close. A card statement is the only
-- place a historic balance is stated (the card export carries no running
-- balance column), so this is what anchors a reconstructed card balance
-- series to something the provider actually asserted.
--
-- Keyed on (account, period_end) so re-parsing the same statement — or the
-- same period downloaded in two runs — converges: what a period records is
-- the copy of it that passes the most parse gates, and `snapshot_at` names
-- the bronze run that copy came from.
--
-- Written by the CARD statement pass only, and for every era — including
-- periods the export already covers, since those rows are exactly what
-- anchors the reconstructed per-transaction balance. The deposit statement
-- pass reconstructs its balances from the export's own running-balance
-- column and has no use for this table.
--
-- A period's balances say nothing about whether its TRANSACTIONS reached
-- silver; `0003` adds `transactions_covered` for that.
-- ============================================================
CREATE TABLE statement_balances (
    account_external_id TEXT    NOT NULL,
    period_start        INTEGER NOT NULL,          -- Unix seconds UTC at midnight
    period_end          INTEGER NOT NULL,          -- Unix seconds UTC at midnight
    opening             REAL,                      -- balance at period_start, as printed
    closing             REAL,                      -- balance at period_end, as printed
    snapshot_at         INTEGER NOT NULL,
    PRIMARY KEY (account_external_id, period_end)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s','now') AS INTEGER));
