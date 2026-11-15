-- ============================================================
-- gold schema — the stated-rate keys on the rows of three shapes.
--
-- Three sources state an account's interest rate in a shape of their
-- own: UBS PSN's balance record (CdtIntrstRate1 to 4 and their limits,
-- the debit side alike), Raiffeisen's account details ("0,111% ab
-- 01.01.2024") and VIAC's strategy rate. Their adapters write the rate
-- under the stated-rate keys every source uses (canonical/statedrate.go),
-- and the yield pass reads those keys alone. This migration writes the
-- same keys onto the rows a load wrote before, so a gold keeps its rates
-- without a reload. Each value is read as the yield pass read it:
--
--   * a cash balance that states UBS's tiers: per side, each tier with a
--     rate under its column's number (a tier past the first only after a
--     column whose limit lies between 0.005 and 1e14, a limit outside
--     that none), the day the side holds from, the term's end before the open one (9999), and the
--     day count as canonical.DayCount names it. Keys of another shape on
--     such a balance give way to these, as the yield pass read the PSN
--     shape alone there;
--   * an account whose details state Raiffeisen's credit or debit rate:
--     the rate (its decimal a comma or a dot) and the day it holds from;
--   * an account whose strategy states VIAC's rate: a credit rate.
--
-- One side is the payload's own keys; a credit and a debit side are two
-- interest_conditions, the credit side first. A side of one tier, the
-- first, with no limit is a flat interest_rate. A rate that is no finite
-- number states nothing. The keys are appended to the payload's text, so
-- its other values keep their bytes; stated-rate keys of another shape
-- on a PSN balance are removed first.
-- ============================================================

UPDATE cash_balances
   SET payload = json_merge_patch(payload, '{"interest_rate": null, "interest_tiers": null,
       "interest_valid_from": null, "interest_side": null, "interest_term_end": null,
       "interest_day_count": null, "interest_conditions": null}')
 WHERE json_extract_string(payload, 'CdtIntrstRate1') IS NOT NULL
   AND list_has_any(json_keys(payload), ['interest_rate', 'interest_tiers', 'interest_valid_from',
       'interest_side', 'interest_term_end', 'interest_day_count', 'interest_conditions']);

UPDATE cash_balances cb
   SET payload = regexp_replace(cb.payload::VARCHAR, '\}\s*$', ',' || substr(k.keys::VARCHAR, 2))
  FROM (
    WITH ubs_balances AS (
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, payload
          FROM cash_balances
         WHERE json_extract_string(payload, 'CdtIntrstRate1') IS NOT NULL),
    tiers AS (
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'credit' AS side, 1 AS tier,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstRate1') AS DOUBLE) AS rate_pct,
               CASE WHEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt1') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt1') AS DOUBLE) END AS up_to,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstVldFrmDt') AS DATE) AS valid_from,
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END AS term_end,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END AS day_count
          FROM ubs_balances
        UNION ALL
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'credit', 2,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstRate2') AS DOUBLE),
               CASE WHEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt2') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt2') AS DOUBLE) END,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstVldFrmDt') AS DATE),
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END
          FROM ubs_balances
         WHERE TRY_CAST(json_extract_string(payload, 'CdtLmtAmt1') AS DOUBLE) BETWEEN 0.005 AND 1e14
        UNION ALL
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'credit', 3,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstRate3') AS DOUBLE),
               CASE WHEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt3') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt3') AS DOUBLE) END,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstVldFrmDt') AS DATE),
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END
          FROM ubs_balances
         WHERE TRY_CAST(json_extract_string(payload, 'CdtLmtAmt2') AS DOUBLE) BETWEEN 0.005 AND 1e14
        UNION ALL
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'credit', 4,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstRate4') AS DOUBLE),
               CASE WHEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt4') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'CdtLmtAmt4') AS DOUBLE) END,
               TRY_CAST(json_extract_string(payload, 'CdtIntrstVldFrmDt') AS DATE),
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END
          FROM ubs_balances
         WHERE TRY_CAST(json_extract_string(payload, 'CdtLmtAmt3') AS DOUBLE) BETWEEN 0.005 AND 1e14
        UNION ALL
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'debit', 1,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstRate1') AS DOUBLE),
               CASE WHEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt1') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt1') AS DOUBLE) END,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstVldFrmDt') AS DATE),
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END
          FROM ubs_balances
        UNION ALL
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'debit', 2,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstRate2') AS DOUBLE),
               CASE WHEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt2') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt2') AS DOUBLE) END,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstVldFrmDt') AS DATE),
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END
          FROM ubs_balances
         WHERE TRY_CAST(json_extract_string(payload, 'DbtLmtAmt1') AS DOUBLE) BETWEEN 0.005 AND 1e14
        UNION ALL
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'debit', 3,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstRate3') AS DOUBLE),
               CASE WHEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt3') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt3') AS DOUBLE) END,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstVldFrmDt') AS DATE),
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END
          FROM ubs_balances
         WHERE TRY_CAST(json_extract_string(payload, 'DbtLmtAmt2') AS DOUBLE) BETWEEN 0.005 AND 1e14
        UNION ALL
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, 'debit', 4,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstRate4') AS DOUBLE),
               CASE WHEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt4') AS DOUBLE) BETWEEN 0.005 AND 1e14
                    THEN TRY_CAST(json_extract_string(payload, 'DbtLmtAmt4') AS DOUBLE) END,
               TRY_CAST(json_extract_string(payload, 'DbtIntrstVldFrmDt') AS DATE),
               CASE WHEN year(TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE)) < 9000
                    THEN TRY_CAST(json_extract_string(payload, 'EndOfDurtn') AS DATE) END,
               CASE upper(replace(json_extract_string(payload, 'CalIntrstBasisDesc'), ' ', ''))
                    WHEN '30/360' THEN '30/360' WHEN '30E/360' THEN '30E/360' WHEN 'ACT/ACT' THEN 'ACT/ACT'
                    WHEN 'ACT/360' THEN 'ACT/360' WHEN 'ACT/365' THEN 'ACT/365' END
          FROM ubs_balances
         WHERE TRY_CAST(json_extract_string(payload, 'DbtLmtAmt3') AS DOUBLE) BETWEEN 0.005 AND 1e14),
    conds AS (
        SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind, side,
               json_merge_patch('{}', json_object(
                   'interest_side', side,
                   'interest_rate', CASE WHEN count(*) = 1 AND bool_and(up_to IS NULL) AND bool_and(tier = 1)
                                         THEN first(rate_pct) END,
                   'interest_tiers', CASE WHEN count(*) > 1 OR bool_or(up_to IS NOT NULL) OR bool_or(tier > 1)
                                          THEN to_json(list({'rate': rate_pct, 'up_to': up_to, 'tier': tier}
                                                            ORDER BY tier)) END,
                   'interest_valid_from', strftime(valid_from, '%Y-%m-%d'),
                   'interest_term_end', strftime(term_end, '%Y-%m-%d'),
                   'interest_day_count', day_count)) AS cond
          FROM tiers
         WHERE isfinite(rate_pct)
         GROUP BY silver_source_id, snapshot_at, account_external_id, currency, balance_kind, side, valid_from,
                  term_end, day_count)
    SELECT silver_source_id, snapshot_at, account_external_id, currency, balance_kind,
           CASE WHEN count(*) = 1 THEN first(cond)
                ELSE json_object('interest_conditions', list(cond ORDER BY side)) END AS keys
      FROM conds
     GROUP BY silver_source_id, snapshot_at, account_external_id, currency, balance_kind) k
 WHERE cb.silver_source_id = k.silver_source_id AND cb.snapshot_at = k.snapshot_at
   AND cb.account_external_id = k.account_external_id AND cb.currency = k.currency
   AND cb.balance_kind = k.balance_kind;

UPDATE accounts a
   SET payload = regexp_replace(a.payload::VARCHAR, '\}\s*$', ',' || substr(k.keys::VARCHAR, 2))
  FROM (
    WITH conds AS (
        SELECT a.silver_source_id, a.account_external_id, s.side,
               json_merge_patch('{}', json_object(
                   'interest_side', s.side,
                   'interest_rate', TRY_CAST(replace(regexp_extract(json_extract_string(a.payload, '$.details.' || s.key),
                                                                    '(-?[0-9]+([.,][0-9]+)?)\s*%', 1), ',', '.') AS DOUBLE),
                   'interest_valid_from', strftime(TRY_STRPTIME(regexp_extract(json_extract_string(a.payload,
                                                                                                   '$.details.' || s.key),
                                                                               '([0-9]{2}\.[0-9]{2}\.[0-9]{4})', 1),
                                                                '%d.%m.%Y')::DATE, '%Y-%m-%d'))) AS cond
          FROM accounts a
         CROSS JOIN (VALUES ('credit', 'interest_credit'), ('debit', 'interest_debit')) s(side, key)
         WHERE regexp_matches(COALESCE(json_extract_string(a.payload, '$.details.' || s.key), ''), '[0-9]\s*%')
           AND isfinite(TRY_CAST(replace(regexp_extract(json_extract_string(a.payload, '$.details.' || s.key),
                                                        '(-?[0-9]+([.,][0-9]+)?)\s*%', 1), ',', '.') AS DOUBLE))
        UNION ALL
        SELECT a.silver_source_id, a.account_external_id, 'credit',
               json_object('interest_side', 'credit',
                           'interest_rate', TRY_CAST(json_extract_string(a.payload, '$.strategy.interestRate') AS DOUBLE))
          FROM accounts a
         WHERE isfinite(TRY_CAST(json_extract_string(a.payload, '$.strategy.interestRate') AS DOUBLE)))
    SELECT silver_source_id, account_external_id,
           CASE WHEN count(*) = 1 THEN first(cond)
                ELSE json_object('interest_conditions', list(cond ORDER BY side)) END AS keys
      FROM conds
     GROUP BY silver_source_id, account_external_id) k
 WHERE a.silver_source_id = k.silver_source_id AND a.account_external_id = k.account_external_id;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (189, CAST(epoch(now()) AS BIGINT));
