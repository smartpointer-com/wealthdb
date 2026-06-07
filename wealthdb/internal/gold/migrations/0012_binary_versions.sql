-- Track which wealthdb binaries have opened this database, so
-- gold.Open() can detect when a stale binary tries to read a
-- database that's already been written by a newer one (the
-- classic "I forgot to `go install` after pulling" failure mode
-- — manifests as obscure JSON-parse errors, missing fields, or
-- worse, silent misreads after a schema migration).
--
-- Append-only. RW opens insert one row per Open(); the staleness
-- check compares the binary's own VCS-derived commit_at against
-- MAX(binary_commit_at) here.
--
-- binary_commit_at is the source commit's Unix timestamp, read
-- by the binary at runtime from runtime/debug.ReadBuildInfo
-- (Go's automatic VCS stamping; populated when the binary is
-- built from a git checkout with the default -buildvcs=true).

CREATE TABLE binary_versions (
    opened_at        BIGINT  NOT NULL,
    binary_commit    VARCHAR NOT NULL,
    binary_commit_at BIGINT  NOT NULL,
    binary_version   VARCHAR
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (12, CAST(epoch(now()) AS BIGINT));
