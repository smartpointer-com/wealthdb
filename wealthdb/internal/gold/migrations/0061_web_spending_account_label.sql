-- An account with no name is still an account.
--
-- Several adapters leave `display_name` NULL on purpose. UBS is the
-- clearest: its cash accounts carry an AcctTpDesc like "Private" or
-- "Custody", which names the PRODUCT rather than the account, so the
-- adapter forwards it as `account_category` and leaves the name unset —
-- the IBAN is already human-readable and is the better label (see
-- appendCashAccounts in internal/silver/ubs/snapshots.go).
--
-- Every consumer is expected to fall back to the id, and the CLI's
-- `account` column does. This view did not, so on a dashboard grouping
-- by the label the unnamed accounts did not appear as themselves: they
-- collapsed into ONE "null" bar, and the Account
-- picker offered a single "null" entry standing for all of them. The
-- money was right and unattributable, which is the worst way for a
-- report to be wrong.
--
-- So the view labels a row the way the CLI does. `account_external_id`
-- is projected beside it either way, so nothing that needs the id has
-- to read the label. TRIM/NULLIF because an adapter that writes an
-- empty string means the same thing as one that writes NULL.
--
-- The rest of the body is migration 0059's verbatim: a re-issue
-- replaces the whole view, so the label and issuer columns it added
-- have to be carried forward.
CREATE OR REPLACE VIEW web_spending AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           COALESCE(NULLIF(TRIM(display_name), ''), account_external_id) AS display_name,
           account_kind,
           merchant_name,
           COALESCE(spend_primary,  '(uncategorized)') AS spend_primary,
           COALESCE(spend_detailed, '(uncategorized)') AS spend_detailed,
           COALESCE(spend_primary_label, '(uncategorized)') AS spend_primary_label,
           COALESCE(spend_label,         '(uncategorized)') AS spend_label,
           provider_spend_label,
           value_usd, value_chf, value_eur
      FROM report_spending_transactions_multi(0, 9223372036854775807);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (61, CAST(epoch(now()) AS BIGINT));
