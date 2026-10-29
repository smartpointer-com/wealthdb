-- ============================================================
-- gold schema, migration 0170 — a gains row with no asset class reads
-- `unknown` (web/DESIGN.md §6).
--
-- report_gains leaves a position's asset_class NULL where no snapshot
-- names it at either end of the month: a sale within the month of a
-- position no snapshot shows at its start or its end. The Gains
-- dashboard's Asset class picker can pick no NULL, so its classes would
-- not sum to the whole. web_gains reads a missing class as
-- `unknown`, as the exposure and yield views do. report_gains itself is
-- unchanged.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE OR REPLACE VIEW web_gains AS
    SELECT g.currency, g.missing_basis, date_trunc('month', epoch_ms(g.period_start * 1000)) AS period,
           g.silver_source_id, g.account_external_id,
           account_label(a.display_name, g.account_external_id,
                         g.silver_source_id, a.account_kind) AS account,
           a.account_kind, COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           g.k AS instrument_key, g.symbol, g.name, COALESCE(g.asset_class, 'unknown') AS asset_class, g.vehicle,
           g.book_x AS cost_basis, g.value_x AS value,
           g.unrealized_start_x AS unrealized_start, g.unrealized_end_x AS unrealized_end,
           g.unrealized_change_x AS unrealized_change,
           g.realized_x AS realized, g.realized_short_x AS realized_short,
           g.realized_long_x AS realized_long, g.realized_other_x AS realized_other,
           g.realized_book_x AS realized_cost_basis,
           g.proceeds_x AS proceeds, g.wash_x AS wash_disallowed,
           g.at_end, g.end_applies, g.end_without_basis, g.end_unpriced,
           g.basis_changed, g.paid_in, g.account_unobserved, g.onboarded_source,
           g.n_lots AS realized_lots, g.n_without_gain AS lots_without_gain,
           g.n_undated AS undated_lots, g.n_sells AS sells,
           g.n_undocumented AS sells_without_documents, g.n_in_kind AS in_kind_moves,
           g.n_corporate AS corporate_actions, g.n_fx_missing AS fx_missing,
           g.n_rebuilt AS sells_rebuilt, g.n_seed AS seed_lots, g.n_implied AS implied_disposals,
           g.n_blip AS snapshot_blips, g.n_wash AS wash_sales_not_applied, g.n_fee_unvalued AS fee_unvalued,
           g.n_assumed_zero AS basis_assumed_zero, g.spliced, g.pooled, g.settlement AS dated_by_settlement,
           g.held_change_x AS held_change, g.sold_gain_x AS sold_gain
      FROM report_gains g
      LEFT JOIN accounts a ON a.silver_source_id = g.silver_source_id
           AND a.account_external_id = g.account_external_id;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (170, CAST(epoch(now()) AS BIGINT));
