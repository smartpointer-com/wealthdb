-- spend_enrichment_population gains the two columns a scoped
-- `spending.rules[].scope` narrows by: the account's portfolio, and the
-- account id itself. Both were reachable from the tables the macro
-- already joins; neither was projected, so a rule could only ever be
-- global. Nothing else about the population changes.
CREATE OR REPLACE MACRO spend_scoped_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category,
           a.portfolio_external_id
      FROM accounts a
      LEFT JOIN spend_account_scope s
             ON s.silver_source_id    = a.silver_source_id
            AND s.account_external_id = a.account_external_id
     WHERE COALESCE(s.mode,
                    CASE WHEN a.account_kind IN ('cash', 'card')
                         THEN 'include' ELSE 'exclude' END) = 'include'
);

CREATE OR REPLACE MACRO spend_enrichment_population(p_from, p_to) AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, sa.account_kind, sa.display_name,
           sa.nickname, sa.account_category, sa.portfolio_external_id,
           t.kind, t.currency, t.net_amount, t.description,
           t.counterparty, t.provider_category
      FROM transactions t
      JOIN spend_scoped_accounts() sa
             ON sa.silver_source_id    = t.silver_source_id
            AND sa.account_external_id = t.account_external_id
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND (t.kind IN ('purchase', 'refund', 'reward', 'withdrawal', 'fee', 'tax')
            OR (t.kind = 'interest' AND t.net_amount < 0))
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (55, CAST(epoch(now()) AS BIGINT));
