-- ============================================================
-- gold schema — the far side of a move lives in transfer_links alone.
--
-- The spending overlay carried the far side of each move it placed in
-- three columns, and transfer_links (0182) carried the same rows. The
-- flow macros read the far side off the links, so the columns go.
--
-- A gold whose links a pass wrote already holds every far side the
-- columns hold: the pass writes both in one transaction, from the same
-- rows. A gold migrated past 0182 that no pass has run on since holds
-- none, so each overlay row with a far side and no link gets one here,
-- with the same far account and class. Its partner leg is not known,
-- so it carries none, and its evidence is the road a one-legged move
-- names: a rule where a rule's class or a declared account says so,
-- else the stated account. The next load's pass writes every link
-- whole again.
-- ============================================================

INSERT INTO transfer_links (silver_source_id, transaction_external_id,
                            far_silver_source_id, far_account_external_id,
                            far_transaction_external_id, far_class, evidence, measure)
SELECT e.silver_source_id, e.transaction_external_id,
       e.far_silver_source_id, e.far_account_external_id, NULL, e.far_class,
       CASE WHEN e.far_class IS NOT NULL OR e.far_silver_source_id = 'declared'
            THEN 'rule' ELSE 'stated-account' END,
       'cash'
  FROM spend_txn_enrichment e
 WHERE (e.far_account_external_id IS NOT NULL OR e.far_class IS NOT NULL)
   AND NOT EXISTS (SELECT 1 FROM transfer_links l
                    WHERE l.silver_source_id        = e.silver_source_id
                      AND l.transaction_external_id = e.transaction_external_id);

-- DuckDB drops no column of a table an index depends on, so the
-- signature index (0041) is dropped around the columns and made again.
DROP INDEX IF EXISTS ix_spend_txn_enrichment_signature;
ALTER TABLE spend_txn_enrichment DROP COLUMN IF EXISTS far_silver_source_id;
ALTER TABLE spend_txn_enrichment DROP COLUMN IF EXISTS far_account_external_id;
ALTER TABLE spend_txn_enrichment DROP COLUMN IF EXISTS far_class;
CREATE INDEX IF NOT EXISTS ix_spend_txn_enrichment_signature
    ON spend_txn_enrichment(merchant_signature);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (196, CAST(epoch(now()) AS BIGINT));
