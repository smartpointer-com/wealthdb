-- `web_income`: the serving view the Income dashboards read.
--
-- `web_spending`'s shape over the income lines, and for the same
-- reasons. One row per (line, reporting currency) so a currency picker
-- is a column choice rather than a re-query; timestamps as epoch_ms,
-- which is what Metabase's date machinery understands; the account's
-- display name and label resolved through the shared helpers, so a
-- card and a chequing account read the same way here as everywhere
-- else; and every nullable taxonomy column COALESCEd to
-- `(uncategorized)`.
--
-- That COALESCE is the dashboard's data-quality canary and not a
-- cosmetic default: a receipt no tier could place is a row of the
-- report with a name a reader can see and count, rather than a gap
-- that quietly shrinks a total. The uncategorised SHARE tile reads it.
--
-- payer_name is left NULL where the resolution left it NULL — a delta
-- line has no payer — because a ring of payers must not grow an
-- "(uncategorized)" slice for lines that by construction have nobody
-- behind them. The type columns and the payer column answer different
-- questions and take different treatments.
CREATE OR REPLACE VIEW web_income AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind,
           payer_name,
           COALESCE(income_primary,  '(uncategorized)') AS income_primary,
           COALESCE(income_detailed, '(uncategorized)') AS income_detailed,
           COALESCE(income_primary_label, '(uncategorized)') AS income_primary_label,
           COALESCE(income_label,         '(uncategorized)') AS income_label,
           provider_income_label,
           value_usd, value_chf, value_eur
      FROM report_income_transactions_multi(0, 9223372036854775807);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (72, CAST(epoch(now()) AS BIGINT));
