-- ============================================================
-- gold schema — UBS's grade under the rating key.
--
-- The UBS adapter folds the web feed's position row into PSN's position
-- under the `web` key, and the row's Rating column is one more grade of
-- the bond. The adapter states that grade under rating_sp, a key of the
-- bond terms every source writes (canonical/payloadterms.go), and the
-- yield pass reads those keys alone. This migration writes the key onto
-- the positions a load wrote before, so a gold keeps its grades without
-- a reload: the Rating a position's web row states, as it is written,
-- where the position states no rating_sp of its own. The key is appended
-- to the payload's text, so its other values keep their bytes.
-- ============================================================

UPDATE positions
   SET payload = regexp_replace(payload::VARCHAR, '\}\s*$',
                                ',"rating_sp":' || json_extract(payload, '$.web.Rating')::VARCHAR || '}')
 WHERE json_extract_string(payload, '$.web.Rating') IS NOT NULL
   AND NOT json_exists(payload, '$.rating_sp');

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (190, CAST(epoch(now()) AS BIGINT));
