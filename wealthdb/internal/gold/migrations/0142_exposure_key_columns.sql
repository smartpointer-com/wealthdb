-- ============================================================
-- gold schema, migration 0142 — exposure_key binds its argument to the
-- caller, wherever the caller passes it.
--
-- exposure_key(p_key) (0126) expands to a subquery on security_aliases.
-- A caller that passes an unqualified column named `security_key` or
-- `alias` in a WHERE clause or a JOIN condition had it bind to
-- security_aliases' own column inside that subquery, so the alias never
-- resolved; in a select list the same call bound to the caller. The
-- subquery reads security_aliases through a derived table whose name and
-- column names no caller uses, so the argument binds to the caller in
-- every clause. The answers are the same.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE OR REPLACE MACRO exposure_key(p_key) AS
    COALESCE((SELECT exposure_key_alias.exposure_key_to
                FROM (SELECT alias AS exposure_key_from, security_key AS exposure_key_to
                        FROM security_aliases) exposure_key_alias
               WHERE exposure_key_alias.exposure_key_from = p_key), p_key);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (142, CAST(epoch(now()) AS BIGINT));
