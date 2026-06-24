-- Persist each silver source's FX precedence rank into gold so the SQL FX
-- layer (migration 0020) and the report macros (0021) can honour source
-- priority without a runtime config injection. The rank is the 0-based
-- index in config.FxSourceOrder() (0 = highest priority); NULL = unranked,
-- sorted last. The loader re-stamps every source on each `wealthdb load`
-- (see gold.SetFxPriorities + cmd_load), so editing the config's
-- fx_priority and re-loading any source refreshes the column.
--
-- Plain ADD COLUMN — no CHECK on this column, so no rename-recreate dance.

ALTER TABLE silver_sources ADD COLUMN fx_priority INTEGER;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (19, CAST(epoch(now()) AS BIGINT));
