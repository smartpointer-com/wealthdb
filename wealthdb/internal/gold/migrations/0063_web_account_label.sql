-- An account label that says whose account it is, and what kind.
--
-- The serving views labelled a row with the account's own name, which
-- is what the institution calls it and nothing more — a product
-- nickname, a masked card number, an IBAN. Read on a chart, a bar so
-- labelled does not say which institution it belongs to, nor whether
-- the money left a deposit account or a credit card; and product
-- nicknames are generic, so two institutions' accounts can wear the
-- same one. Migration 0061 fixed the half of this that lost money (the
-- unnamed accounts collapsing into one "null" bar); this fixes the
-- half that is merely unreadable.
--
-- Two scalar macros, so the rule has ONE home rather than a copy in
-- each view:
--
--   * account_display_name is 0061's fallback, lifted out of
--     web_spending's projection: an account with no name of its own is
--     labelled by its id, the way the CLI's `account` column does it.
--   * account_label annotates that with the source and the kind, as
--     `<name> (<source> <kind>)`.
--
-- Name FIRST. A row chart truncates a long label from the end, so
-- putting the identity in front means what gets clipped in a narrow
-- tile is the annotation, not the thing being labelled.
--
-- Both views keep `display_name` and `account_external_id` beside the
-- label, so a consumer that wants the institution's own name, or the
-- id, still reads it off its own column and nothing has to parse the
-- label back apart.
CREATE OR REPLACE MACRO account_display_name(nm, ext_id) AS
    COALESCE(NULLIF(TRIM(nm), ''), ext_id);

CREATE OR REPLACE MACRO account_label(nm, ext_id, src, kind) AS
    account_display_name(nm, ext_id)
        || ' (' || src || ' ' || COALESCE(kind, 'unknown') || ')';

-- The rest of each body is verbatim from the migration that last
-- defined it — a re-issue replaces the whole view, so what those added
-- has to be carried forward: 0061's id fallback and 0059's label and
-- issuer columns here, 0049's epoch_ms timestamps in both.
CREATE OR REPLACE VIEW web_spending AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind,
           merchant_name,
           COALESCE(spend_primary,  '(uncategorized)') AS spend_primary,
           COALESCE(spend_detailed, '(uncategorized)') AS spend_detailed,
           COALESCE(spend_primary_label, '(uncategorized)') AS spend_primary_label,
           COALESCE(spend_label,         '(uncategorized)') AS spend_label,
           provider_spend_label,
           value_usd, value_chf, value_eur
      FROM report_spending_transactions_multi(0, 9223372036854775807);

-- 'card' is a literal rather than a column because the macro this view
-- wraps selects on it: report_card_balances_history_multi joins
-- accounts and keeps `account_kind = 'card'` (migration 0043), so every
-- row here is a card by construction and the macro projects no kind.
CREATE OR REPLACE VIEW web_card_balances_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, 'card') AS account_label,
           currency,
           balance, balance_usd, balance_chf, balance_eur
      FROM report_card_balances_history_multi();

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (63, CAST(epoch(now()) AS BIGINT));
