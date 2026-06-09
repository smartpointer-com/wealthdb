-- angellist silver v7 — link funding transactions to the SPV/fund position.
--
-- A contribution / distribution concerns a specific investment (the SPV/fund
-- the capital went to or came from). The funding ledger names it only in the
-- transaction `description` ("Investment in <company>", "Disbursement -
-- <company> - …", "Refund for <company> …"), so load.py resolves the company
-- and records the position id here:
--   * single current position for the company -> that position;
--   * several positions (a multi-SPV company)  -> the position whose invest
--     date is closest to the transaction date (the description can't say which
--     SPV — heuristic);
--   * no current position (an EXITED investment) -> a thin offering derived
--     from the ledger (so the instrument exists), linked here.
-- External-bank deposits / withdrawals name no company and stay NULL.

ALTER TABLE funding_transactions ADD COLUMN position_external_id TEXT;

INSERT INTO schema_meta (silver_schema_version) VALUES (7);
