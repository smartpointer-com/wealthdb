-- ============================================================
-- gold schema, migration 0006 — symbol_resolutions lookup table.
--
-- A side table populated by `wealthdb resolve-symbols`. Stores
-- ticker symbols inferred by a local LLM for rows that the silver
-- adapters couldn't resolve themselves: ETFs whose descriptions
-- don't carry a parenthesised ticker (UBS Xtrackers, UBS Core,
-- SPDR, ...), Schwab API dividend descriptions that don't match
-- any silver position by normalised name (FLOATNGRATE BD, TRSURYBOND,
-- BLMBRG BRCLY HGH YDBND, ...), and similar gaps.
--
-- The base tables (instruments, transactions) are NOT touched —
-- the LLM is fallible and a wrong mutation there would be hard to
-- back out. Resolutions live only in this table; the read path
-- (gold/positions.go, gold/transactions.go) does a LEFT JOIN with
-- COALESCE(instruments.symbol, sr_by_id.symbol, sr_by_name.symbol).
-- The model_name + resolved_at columns make it easy to bulk-delete
-- entries from an outdated model run.
--
-- lookup_kind discriminates which source column the lookup_value
-- corresponds to:
--   'instrument_external_id'  → join against {instruments,positions,
--                                transactions}.instrument_external_id.
--   'name'                    → join against transactions.description
--                                (positions has no equivalent column).
-- ============================================================

CREATE TABLE symbol_resolutions (
    silver_source_id TEXT    NOT NULL,
    lookup_kind      TEXT    NOT NULL CHECK (lookup_kind IN ('instrument_external_id', 'name')),
    lookup_value     TEXT    NOT NULL,
    symbol           TEXT    NOT NULL,
    resolved_at      BIGINT  NOT NULL,    -- Unix seconds UTC
    model_name       TEXT    NOT NULL,    -- copy from cfg.model.name at resolution time
    PRIMARY KEY (silver_source_id, lookup_kind, lookup_value)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (6, CAST(epoch(now()) AS BIGINT));
