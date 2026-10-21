-- ============================================================
-- gold schema, migration 0136 — the holdings the yield report and the
-- income projection read (docs/YIELD.md, gold.YieldInputs).
--
-- yield_holdings is exposure_holdings with each line's own amount, its
-- quantity, the interest accrued on it and its acquisition day.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- yield_holdings: exposure_holdings, each line with its own amount (in
-- ccy, its currency), its quantity, the interest accrued on it and its
-- acquisition day, where the source states them. A cash line's quantity
-- and accrued interest are NULL. Its lines and values are exposure's,
-- so its total is `holdings global`.
CREATE OR REPLACE MACRO yield_holdings(p_asof, p_ccy) AS TABLE (
    SELECT h.*, COALESCE(p.market_value, c.amount)::DOUBLE AS amount, p.quantity::DOUBLE AS quantity,
           p.accrued_interest::DOUBLE AS accrued, p.acquisition_date
      FROM exposure_holdings(p_asof, p_ccy) h
      LEFT JOIN positions p ON NOT h.is_cash AND p.silver_source_id = h.src AND p.account_external_id = h.acct
           AND p.position_key = h.position_key AND p.snapshot_at = h.snap
      LEFT JOIN cash_chosen(p_asof) c ON h.is_cash AND c.silver_source_id = h.src
           AND c.account_external_id = h.acct AND 'cash:' || c.currency = h.position_key
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (136, CAST(epoch(now()) AS BIGINT));
