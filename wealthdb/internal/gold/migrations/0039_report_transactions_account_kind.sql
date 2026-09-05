-- Re-issue the transaction report macros with the owning account's
-- `account_kind`, so consumers can fence a kind out of a chart.
--
-- Why now: credit-card accounts (migration 0037) book `interest` and
-- `fee` transactions of their own — a card issuer's finance charge and
-- annual fee. The web's "Income by month" and "Fees & taxes by month"
-- charts select purely by transaction kind, so without a dimension to
-- filter on, the first card load would show card charges as investment
-- income and portfolio costs. The column lands before any card data
-- does; web/provision.py fences both charts on it.
--
-- Nullable, like every other account-joined column here: the join is a
-- LEFT JOIN, so a transaction whose account is absent from `accounts`
-- reads NULL. A fence must therefore spell the null branch out and KEEP
-- those rows; a bare `<> 'card'` would silently drop them.
--
-- LOCKSTEP WARNING: gold.TransactionsBetween runs `SELECT *` over
-- report_transactions with a POSITIONAL rows.Scan, so every column added
-- to this macro must be matched by a field in gold.TransactionRow and a
-- scan target in the same position — otherwise `wealthdb transactions`
-- breaks at runtime, not at compile time. This change makes that edit;
-- migration 0042 re-issues these same two macros with the merchant /
-- spend-category columns and repeats it. (The returns flow loaders are
-- safe by construction: they project named columns.)
--
-- CREATE OR REPLACE on both macros, mirroring 0024's re-issue of 0021's
-- definitions. web_transactions (migration 0032) reads
-- report_transactions_multi, so it is dropped and re-created around the
-- macro swap — both to avoid a catalog dependency on the replaced macro
-- and because the privacy flow charts need the same fence. Its column
-- list is otherwise unchanged.

DROP VIEW IF EXISTS web_transactions;

-- report_transactions: every transaction in [p_from, p_to], net_amount
-- converted to p_ccy at occurred_at. Always ascending; the caller re-orders
-- for `-r`.
CREATE OR REPLACE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
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

-- report_transactions_multi: every transaction in [p_from, p_to] with net
-- amount in USD/CHF/EUR at occurred_at.
CREATE OR REPLACE MACRO report_transactions_multi(p_from, p_to) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS DECIMAL(28,4)) AS gross_amount, CAST(b.net_amount AS DECIMAL(28,4)) AS net_amount,
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.price AS DECIMAL(28,8)) AS price, b.description,
           CAST(COALESCE(CASE WHEN b.currency = 'USD' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_usd.rate, b.net_amount::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
           CAST(COALESCE(CASE WHEN b.currency = 'CHF' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_chf.rate, b.net_amount::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
           CAST(COALESCE(CASE WHEN b.currency = 'EUR' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_eur.rate, b.net_amount::DOUBLE * p_chf.rate * chf_eur.rate,
               b.net_amount::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
      FROM base b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

-- Per-transaction converted values (the income / cost flow charts).
-- All kinds and all account kinds; the cards filter to the kind families
-- they chart and fence out the account kinds that do not belong there.
CREATE OR REPLACE VIEW web_transactions AS
    SELECT CAST(to_timestamp(occurred_at) AS TIMESTAMP) AS occurred_at,
           kind, silver_source_id, account_kind,
           value_usd, value_chf, value_eur
      FROM report_transactions_multi(0, 9223372036854775807);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (39, CAST(epoch(now()) AS BIGINT));
