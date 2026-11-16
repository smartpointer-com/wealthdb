-- ============================================================
-- gold schema — the mortgage rule places mortgage_transfer, and
-- transfer_links keeps no far class.
--
-- The built-in mortgage rule placed internal_transfer and linked the
-- row to the class `mortgage` where nothing named its far account. It
-- places mortgage_transfer, which reaches financing · Mortgage by
-- itself, so a link holds a far account or nothing.
--
-- The rows a pass placed so are moved here to what the pass places
-- now. They are the spending population's rows the rule tier placed
-- internal_transfer on whose narrative the mortgage rule matches: one
-- of its tokens (MORTGAGE, HYPOTHEK, HYPOTHEKARZINS) or its phrase
-- (HOME LOAN) as whole words in the signature, the counterparty or the
-- description before its memo, upper-cased and with accents folded.
-- Every row a link gave the class is one of them. A built-in rule outranks every
-- config rule, and no other built-in places internal_transfer, so the
-- match names the mortgage rule's rows. Their far account, where the
-- source named one, keeps its link.
-- ============================================================

UPDATE spend_txn_enrichment e SET spend_detailed = 'mortgage_transfer'
  FROM transactions t
  JOIN accounts a
    ON a.silver_source_id    = t.silver_source_id
   AND a.account_external_id = t.account_external_id
  LEFT JOIN spend_account_scope s
         ON s.silver_source_id    = t.silver_source_id
        AND s.account_external_id = t.account_external_id
 WHERE t.silver_source_id        = e.silver_source_id
   AND t.transaction_external_id = e.transaction_external_id
   AND e.provenance = 'rule' AND e.spend_detailed = 'internal_transfer'
   AND (EXISTS (SELECT 1 FROM transfer_links l
                 WHERE l.silver_source_id        = e.silver_source_id
                   AND l.transaction_external_id = e.transaction_external_id
                   AND l.far_class IS NOT NULL)
        OR (COALESCE(s.mode, 'include') = 'include'
            AND (t.kind IN ('purchase', 'refund', 'withdrawal', 'fee', 'tax')
                 OR (t.kind = 'interest' AND t.net_amount < 0))
            AND (regexp_matches(upper(strip_accents(COALESCE(e.merchant_signature, ''))), '(^|[^A-Z0-9])(MORTGAGE|HYPOTHEK|HYPOTHEKARZINS|HOME[^A-Z0-9]+LOAN)([^A-Z0-9]|$)')
                 OR regexp_matches(upper(strip_accents(COALESCE(t.counterparty, ''))), '(^|[^A-Z0-9])(MORTGAGE|HYPOTHEK|HYPOTHEKARZINS|HOME[^A-Z0-9]+LOAN)([^A-Z0-9]|$)')
                 OR regexp_matches(upper(strip_accents(
                        CASE WHEN starts_with(COALESCE(t.description, ''), '— ') THEN ''
                             ELSE split_part(COALESCE(t.description, ''), ' — ', 1) END)), '(^|[^A-Z0-9])(MORTGAGE|HYPOTHEK|HYPOTHEKARZINS|HOME[^A-Z0-9]+LOAN)([^A-Z0-9]|$)'))));

DELETE FROM transfer_links WHERE far_class IS NOT NULL;

ALTER TABLE transfer_links DROP COLUMN far_class;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (198, CAST(epoch(now()) AS BIGINT));
