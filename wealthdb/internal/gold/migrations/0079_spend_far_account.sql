-- The other end of an own-account move, recorded on the row.
--
-- The internal-transfer matcher has SEEN both legs of every pair it
-- forms, and until now it wrote down only that a leg was matched. That
-- is everything the two families need: an own-account move is not
-- spending and not income whichever account it went to. It is not
-- enough for a cash flow statement, which has to ask one more
-- question — whether the money stayed inside the household's cash pool
-- — and can only answer it by looking at the account on the other side.
--
-- Three columns, and the pass fills them on every run:
--
--   far_silver_source_id     the partner leg's account, written by the
--   far_account_external_id  matcher. Together they key `accounts`, so
--                            the resolution reads the far account's tax
--                            wrapper and kind off the dimension rather
--                            than storing a verdict that would go stale
--                            when a wrapper override lands.
--   far_class                the cashflow class a RULE stands for where
--                            it places an own-account move without
--                            pairing anything. One rule writes it
--                            today: the built-in mortgage rule, which
--                            matches a narrative rather than an account
--                            and therefore fires whether or not the
--                            lender is tracked.
--
-- They go on the SPENDING overlay because it is the one that writes a
-- row for every matched leg whether or not the leg is in its own
-- population (internal/spending/family.go, emitOutsidePopulation). The
-- income overlay holds only its own population plus pins, so a card
-- bill's own leg — a pair's far side as often as not — has no row
-- there to carry the fact.
--
-- far_class is NOT the card rule's `merchant_label`. That column names
-- the issuer a card bill was paid to and every spending surface prints
-- it as the line's merchant; a class name in it would read as a
-- merchant on every card bill. Two rules, two facts, two columns.
--
-- Nothing reads these yet. spend_txn_categories() is deliberately not
-- re-issued: it resolves the spending VOCABULARY — the merchant, the
-- category, the tier that decided it — and the far account is a raw
-- fact about a pairing rather than part of that resolution. The
-- cashflow resolution joins this table directly.
--
-- IF NOT EXISTS keeps the migration replayable for the DDL-rerun test.
ALTER TABLE spend_txn_enrichment ADD COLUMN IF NOT EXISTS far_silver_source_id    TEXT;
ALTER TABLE spend_txn_enrichment ADD COLUMN IF NOT EXISTS far_account_external_id TEXT;
ALTER TABLE spend_txn_enrichment ADD COLUMN IF NOT EXISTS far_class               TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (79, CAST(epoch(now()) AS BIGINT));
