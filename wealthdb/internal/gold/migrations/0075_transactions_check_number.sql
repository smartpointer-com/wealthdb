-- A cheque number, captured once and in one place.
--
-- The number written on a paper cheque drawn on the holder's own
-- account, kept so a row can be correlated against the holder's own
-- paper records. Nullable, and null nearly everywhere: one source can
-- produce one today.
--
-- THE RULE IS STRUCTURAL, not a per-adapter judgement: the column is
-- set only where money LEFT the account. It exists to name an outgoing
-- payment, and the guard is what makes the field safe to widen to other
-- banks later. Two observed shapes prove the guard earns its place. A
-- bank may put its OWN instrument reference in the same field on an
-- incoming credit — an interest payment identified by a reference
-- number is not a cheque the holder wrote. And a deposit export may
-- carry a column headed "Check or Slip #", where a slip number rides in
-- on an inflow. An outflow gate drops both without either adapter
-- needing to know it is being corrected.
--
-- Deliberately NOT a new transaction kind. A cheque is a payment
-- INSTRUMENT, not a distinct economic event: the money left the
-- account exactly as any withdrawal does, and a new kind would
-- fragment the matcher pool and the spending population for nothing.
--
-- Deliberately NOT in `payload` either, though the column is sparse to
-- the point of rounding error. `payload` holds what could not be
-- canonicalised (DESIGN.md: an unmapped kind or asset_class keeps its
-- raw value there); this is the opposite — a value being canonicalised
-- so two banks spell it one way. The payload already carries cheque
-- numbers under two rival spellings, `check_number` from one silver and
-- `checkNumber` from another's raw blob, which is the drift this
-- column removes. Sparsity is not the criterion elsewhere either:
-- `price` is set on under a quarter of rows and `counterparty` on under
-- a third, and both are columns.
--
-- IF NOT EXISTS is load-bearing, not decorative: gold.Migrate replays
-- migration bodies for the DDL-rerun test.
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS check_number TEXT;

-- ============================================================
-- report_transactions gains the column.
--
-- 0071's body carried forward whole — the spending join, the income
-- join, the symbol resolutions, the FX block — with `check_number`
-- added as the last NAMED column, after the income trio and before the
-- value_outccy CAST.
--
-- The position is a contract. gold.TransactionRow scans this macro
-- POSITIONALLY, so the row struct, the scan list and this projection
-- move together in one change, and a new column goes at the end of the
-- named block so the FX tail stays where the scan expects it.
-- ============================================================
CREATE OR REPLACE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed,
               ic.payer_name, ic.income_primary, ic.income_detailed,
               t.check_number
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
          LEFT JOIN income_txn_categories() ic ON ic.silver_source_id = t.silver_source_id
               AND ic.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           b.payer_name, b.income_primary, b.income_detailed,
           b.check_number,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = p_ccy AND d.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);
INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (75, CAST(epoch(now()) AS BIGINT));
