-- ============================================================
-- gold schema, migration 0182 — transfer_links: where each movement
-- went, kept.
--
-- The enrichment pass pairs the two legs of an own-account move, and
-- reads the far side of a one-legged one off the source or a rule. It
-- writes the far account onto the spending overlay, which keeps the
-- account and loses the partner leg and the evidence. This table keeps
-- all three, one row per leg whose other side is known:
--
--   silver_source_id,            the leg: the primary key.
--   transaction_external_id
--   far_silver_source_id,        the account on the other side. NULL
--   far_account_external_id      where the evidence names none, as the
--                                mortgage rule's class does.
--   far_transaction_external_id  the partner leg of a pair. NULL for a
--                                one-legged move.
--   far_class                    the class a rule stands for where it
--                                names no account (the mortgage rule).
--   evidence                     what says so: the phase of the matcher
--                                that formed a pair, or the road that
--                                named a one-legged row's far side. The
--                                words live in internal/spending
--                                (phaseEvidence), not in a CHECK.
--   measure                      cash: a movement of money.
--
-- The pass rewrites the table whole, in the transaction that writes the
-- overlays, and a row holds what the spending overlay's far columns
-- hold: the same rows, the same far account and class.
-- ============================================================

CREATE TABLE IF NOT EXISTS transfer_links (
    silver_source_id            TEXT NOT NULL,
    transaction_external_id     TEXT NOT NULL,
    far_silver_source_id        TEXT,
    far_account_external_id     TEXT,
    far_transaction_external_id TEXT,
    far_class                   TEXT,
    evidence                    TEXT NOT NULL,
    measure                     TEXT NOT NULL,
    PRIMARY KEY (silver_source_id, transaction_external_id)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (182, CAST(epoch(now()) AS BIGINT));
