-- ============================================================
-- gold schema — transactions.action: what a row does to the lots of its
-- instrument, where its kind and the sign of its quantity do not say it
-- (canonical.TxAction). The adapter states it from the source's own
-- words, and the lot engine reads it. NULL where the kind says it all.
--
-- The rows a load wrote before this column are filled here, so the lot
-- engine reads them as it read them from their payload. Three kinds
-- state an action: cointracking, fidelity and schwab. Each of their rows
-- gets exactly the action its adapter writes for it, read from what the
-- row holds:
--
--   cointracking  the type, comment, currencies and fee the adapter
--                 keeps in the payload.
--   fidelity      the narrative.
--   schwab        the type ("type") and action ("Action", else
--                 "action") of the payload, and the narrative.
--
-- A row of the equity-transfer ledger is the loader's, not an adapter's,
-- and has none.
-- ============================================================

ALTER TABLE transactions ADD COLUMN IF NOT EXISTS action TEXT;

-- CoinTracking's type. A dust sweep is the comment "Dust Sweeping",
-- with its surrounding space trimmed and its case folded. A trade's fee
-- in neither of its currencies goes on the row that buys, or on the one
-- row of a trade with the base currency on one side: every row but the
-- sell leg (":s") of a trade split in two.
UPDATE transactions SET action = CASE json_extract_string(payload, '$.type')
        WHEN 'Deposit'               THEN 'in'
        WHEN 'Withdrawal'            THEN 'out'
        WHEN 'Expense (non taxable)' THEN 'out'
        WHEN 'Other Fee'             THEN 'fee'
        WHEN 'Staking'               THEN 'income'
        WHEN 'Reward / Bonus'        THEN 'income'
        WHEN 'Income'                THEN 'income'
        WHEN 'Airdrop'               THEN 'income'
        WHEN 'Airdrop (non taxable)' THEN 'income'
        WHEN 'Gift / Tip'            THEN 'income'
        WHEN 'Income (non taxable)'  THEN
             CASE WHEN regexp_matches(json_extract_string(payload, '$.comment'),
                      '^[\s\x{0B}\x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]*'
                      || '(?i:Dust Sweeping)'
                      || '[\s\x{0B}\x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]*$')
                  THEN 'acquired' ELSE 'in' END
        WHEN 'Other Expense'         THEN
             CASE WHEN regexp_matches(json_extract_string(payload, '$.comment'),
                      '^[\s\x{0B}\x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]*'
                      || '(?i:Dust Sweeping)'
                      || '[\s\x{0B}\x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]*$')
                  THEN 'converted' ELSE 'spend' END
        WHEN 'Spend'                 THEN 'spend'
        WHEN 'Lost'                  THEN 'lost'
        WHEN 'Stolen'                THEN 'lost'
        WHEN 'Gift'                  THEN 'gift'
        WHEN 'Donation'              THEN 'gift'
        WHEN 'Trade'                 THEN
             CASE WHEN COALESCE(TRY_CAST(json_extract_string(payload, '$.fee_amount') AS DOUBLE), 0) <> 0
                   AND upper(COALESCE(json_extract_string(payload, '$.fee_currency'), '')) NOT IN
                       ('', upper(COALESCE(json_extract_string(payload, '$.buy_currency'), '')),
                            upper(COALESCE(json_extract_string(payload, '$.sell_currency'), '')))
                   AND NOT ends_with(transaction_external_id, ':s')
                  THEN 'third_currency_fee' END
    END
 WHERE silver_source_id IN (SELECT silver_source_id FROM silver_sources WHERE silver_kind = 'cointracking')
   AND COALESCE(json_extract_string(payload, '$.equity_transfer_ledger'), '') <> 'true';

-- The narrative, upper-cased. An option leg ("PUT (XYZ)", "CALL (XYZ)")
-- moves no lot of the stock it is keyed on, whatever its kind. A
-- corporate action reads by the action its narrative opens with.
UPDATE transactions SET action = CASE
        WHEN regexp_matches(upper(description), '\b(PUT|CALL) \(')        THEN 'ignore'
        WHEN kind <> 'corporate_action'                                   THEN NULL
        WHEN starts_with(upper(description), 'RETURN OF CAPITAL')         THEN 'return_of_capital'
        WHEN starts_with(upper(description), 'IN LIEU OF')                THEN 'cash_in_lieu'
        WHEN starts_with(upper(description), 'EXPIRED')                   THEN 'expiry'
        ELSE 'reorganisation' END
 WHERE silver_source_id IN (SELECT silver_source_id FROM silver_sources WHERE silver_kind = 'fidelity')
   AND COALESCE(json_extract_string(payload, '$.equity_transfer_ledger'), '') <> 'true';

-- A TRADE moving no cash (an other) is no event. A corporate action
-- reads by its action, lower-cased, else its narrative, lower-cased.
UPDATE transactions SET action = CASE
        WHEN kind = 'other' THEN
             CASE WHEN json_extract_string(payload, '$.type') = 'TRADE' THEN 'ignore' END
        WHEN kind <> 'corporate_action' THEN NULL
        WHEN lower(COALESCE(json_extract_string(payload, '$.Action'), json_extract_string(payload, '$.action'), ''))
             = 'return of capital' THEN 'return_of_capital'
        WHEN lower(COALESCE(json_extract_string(payload, '$.Action'), json_extract_string(payload, '$.action'), ''))
             = 'cash in lieu'
          OR (json_extract_string(payload, '$.type') = 'JOURNAL'
              AND regexp_matches(lower(description), '(?i)\bIN LIEU\b')) THEN 'cash_in_lieu'
        WHEN lower(COALESCE(json_extract_string(payload, '$.Action'), json_extract_string(payload, '$.action'), ''))
             IN ('unissued rights redemption', 'litigation')
          OR starts_with(lower(description), 'exchange ') THEN 'cash_only'
        WHEN starts_with(lower(description), 'removed due to expiration') THEN 'expiry'
        ELSE 'reorganisation' END
 WHERE silver_source_id IN (SELECT silver_source_id FROM silver_sources WHERE silver_kind = 'schwab')
   AND COALESCE(json_extract_string(payload, '$.equity_transfer_ledger'), '') <> 'true';
