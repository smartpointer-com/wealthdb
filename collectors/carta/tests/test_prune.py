"""
Unit tests for prune.py's carta configuration + the run.json status
lifecycle it keys on.

The shared engine's own suite (shared/collectorkit/tests/test_prune.py)
covers the generic cases: the in-flight age guard, unreadable manifests,
bad slugs, symlinks, --dry-run and target validation. These cover what
carta's PruneConfig decides, over tmp_path bronze trees in carta's layout
(synthetic data only, no real ids or holdings):
  * a complete dump keeps its load inputs and loses screenshots/, the one
    debug subdir `download --debug` writes
  * a statusless manifest counts as complete and is kept whole
  * `main()` wires that config: a dump with no run.json or a status other
    than complete is deleted, and the bronze-root siblings (silver DB,
    side-loaded override CSVs) are never touched

Plus the load-side half of the lifecycle (load.load_run):
  * a status="in-progress" dump or one with no run.json is NOT loaded (the
    guard keeps a crashed walk's partial capture out of silver)
  * a status="complete" dump and a statusless dump ARE loaded

And `load --force` rebuilds exactly the silver an incremental load builds.
"""

from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import load
import prune

OLD_TS = "20260101T010000Z"


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, entities: bool = True,
              documents: bool = True, screenshots: bool = True) -> Path:
    """Build a synthetic carta bronze run dir mirroring download.py's
    layout: bootstrap/ + entities/<slug>/ (meta + holdings + a security
    list) + documents/ (index + a PDF) + screenshots/ (the --debug
    landing capture) + run.json. ``status`` controls the run.json status
    field (``None`` → a statusless manifest)."""
    d = root / slug
    boot = d / "bootstrap"
    boot.mkdir(parents=True)
    (boot / "investments.json").write_text("[]")
    (boot / "navigation-config.json").write_text("{}")
    if entities:
        e = d / "entities" / "corp_100"
        (e / "vesting").mkdir(parents=True)
        (e / "meta.json").write_text(json.dumps(
            {"corporation_id": "100", "is_fund_investment": False}))
        (e / "holdings-dashboard.json").write_text(
            json.dumps({"held_since": "2025-01-15"}))
        (e / "shares.json").write_text(json.dumps({"rows": [], "totals": {}}))
    if documents:
        docs = d / "documents"
        docs.mkdir(parents=True)
        (docs / "index.json").write_text(
            json.dumps({"count": 0, "results": []}))
        (docs / "doc_100.pdf").write_bytes(b"%PDF-1.4 fake")
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir(parents=True)
        (shots / "10-landing.html").write_text("<html>landing</html>")
        (shots / "10-landing.png").write_bytes(b"\x89PNG fake")
    if run_json:
        manifest: dict = {"schema": 1, "dry_run": False,
                          "individual_id": "1", "firm_id": "9",
                          "entities": [], "documents": {"indexed": 0}}
        if status is not None:
            manifest["status"] = status
        (d / "run.json").write_text(json.dumps(manifest))
    return d


def assert_inputs_intact(d: Path) -> None:
    assert (d / "bootstrap" / "investments.json").exists()
    assert (d / "entities" / "corp_100" / "meta.json").exists()
    assert (d / "entities" / "corp_100" / "shares.json").exists()
    assert (d / "documents" / "index.json").exists()
    assert (d / "documents" / "doc_100.pdf").exists()
    assert (d / "run.json").exists()


# ============================================================
# carta's PruneConfig
# ============================================================

def test_complete_dump_screenshots_pruned_inputs_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert prune.main(["--bronze-dir", str(tmp_path)]) == 0
    assert not (d / "screenshots").exists()
    assert_inputs_intact(d)


def test_statusless_manifest_kept_as_complete(tmp_path):
    # A download that writes no status writes its manifest only once the
    # walk finished, so a statusless run.json means the dump is complete.
    d = make_dump(tmp_path, OLD_TS, status=None, screenshots=False)
    assert prune.main(["--bronze-dir", str(tmp_path),
                       "--min-age-hours", "0"]) == 0
    assert_inputs_intact(d)


def test_validate_target_accepts_screenshots_subdir(tmp_path):
    # The one subdir shape prune may target: the --debug capture dir.
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_main_deletes_unfinished_dumps_and_nothing_at_the_root(tmp_path):
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", status="in-progress")
    unmarked = make_dump(tmp_path, "20260103T010000Z", run_json=False)
    # The silver DB and the side-loaded override CSVs live at the bronze
    # root, beside the run dirs.
    root_files = [tmp_path / "carta.db", tmp_path / "100-valuations.csv",
                  tmp_path / "100-transactions.csv"]
    for f in root_files:
        f.write_text("synthetic\n")
    assert prune.main(["--bronze-dir", str(tmp_path),
                       "--min-age-hours", "0"]) == 0
    assert_inputs_intact(complete)
    assert not crashed.exists()
    assert not unmarked.exists()
    assert all(f.exists() for f in root_files)


# ============================================================
# Load-side of the lifecycle: load.load_run gating on status
# ============================================================

def _dump_runs_count(conn) -> int:
    return conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0]


def test_load_skips_in_progress_dump(tmp_path, migrated):
    # A crashed walk left status="in-progress"; load must not ingest its
    # partial capture as a silver snapshot.
    d = make_dump(tmp_path, OLD_TS, status="in-progress",
                  entities=False, documents=False)
    assert load.load_run(migrated, d) is False
    assert _dump_runs_count(migrated) == 0


def test_load_skips_dump_without_run_json(tmp_path, migrated):
    # The walk crashed before writing run.json, or is still writing; prune
    # reclaims the dir, and load neither loads nor records it.
    d = make_dump(tmp_path, OLD_TS, run_json=False,
                  entities=False, documents=False)
    assert load.load_run(migrated, d) is False
    assert _dump_runs_count(migrated) == 0


def test_load_loads_complete_dump(tmp_path, migrated):
    d = make_dump(tmp_path, OLD_TS, status="complete",
                  entities=False, documents=False)
    assert load.load_run(migrated, d) is True
    assert _dump_runs_count(migrated) == 1


def test_load_loads_statusless_dump(tmp_path, migrated):
    # A statusless manifest is loadable: a download that writes no status
    # writes its manifest only at the end of a successful walk.
    d = make_dump(tmp_path, OLD_TS, status=None,
                  entities=False, documents=False)
    assert load.load_run(migrated, d) is True
    assert _dump_runs_count(migrated) == 1


# ============================================================
# Force-rebuild equivalence: `load --force` reproduces the incremental load
# ============================================================

# schema_meta is excluded from the row-level equivalence check: it is the
# migration-version ledger, not silver data, and each row's `applied_at`
# records the wall-clock second the migration ran (strftime('%s','now')). A
# from-scratch rebuild re-applies the migrations later than the incremental
# load did, so that column differs by construction — comparing it would make
# the test flaky (pass only when both loads land in the same second). Every
# `*_at` column in the DATA tables derives from bronze content / run slugs, so
# those stay identical across a rebuild.
_NON_DATA_TABLES = {"schema_meta"}


def _dump_silver(db_path: Path) -> dict[str, list[tuple]]:
    """Snapshot every silver DATA table for an order-independent equivalence
    check: {table -> its rows as sorted plain tuples}. Introspects
    sqlite_master for the table set (skipping sqlite internals and the
    bookkeeping tables above), reads `SELECT *` from each, and sorts the rows
    — the order bronze runs are ingested in must not affect the final state,
    so the comparison is over row multisets, not insertion order. The sort key
    stringifies each column and ranks NULLs first, giving a total order across
    the mixed (text / int / real / NULL) column types without a None-vs-str
    comparison error."""
    conn = sqlite3.connect(str(db_path))
    try:
        names = [r[0] for r in conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' "
            "AND name NOT LIKE 'sqlite_%' ORDER BY name").fetchall()]
        return {
            t: sorted(conn.execute(f'SELECT * FROM "{t}"').fetchall(),
                      key=lambda row: [(v is None, str(v)) for v in row])
            for t in names if t not in _NON_DATA_TABLES
        }
    finally:
        conn.close()


def _seed_holdings(run_dir: Path) -> Path:
    """Enrich a reused make_dump run with a couple of synthetic share
    certificates so the load populates the value-bearing tables (securities +
    the cash_flows exercise ledger), not just entities / dump_runs — that
    cross-run cash_flows overwrite path is the part most sensitive to a
    rebuild diverging from an incremental load. Placeholder ids and round
    figures only, no real holdings. Returns the run dir."""
    (run_dir / "entities" / "corp_100" / "shares.json").write_text(
        json.dumps({"rows": [
            {"id": 1, "label": "CS-1", "issue_date": "01/15/2025",
             "quantity": 1000, "cost": 500.0, "currency": "$"},
            {"id": 2, "label": "CS-2", "issue_date": "06/01/2025",
             "quantity": 500, "cost": 1000.0, "currency": "$"},
        ], "totals": {}}))
    return run_dir


def test_force_rebuild_equals_incremental(tmp_path):
    # The --force contract (load.main): --force DELETEs the silver DB and
    # rebuilds it from ALL bronze. For UNCHANGED bronze that rebuild must
    # land exactly where a plain incremental load did — a faithful replay,
    # table for table, not a subtly different result.
    bronze_root = tmp_path / "bronze"
    bronze_root.mkdir()
    db = tmp_path / "carta.db"

    # Two complete runs at distinct timestamps: --force rebuilds BOTH (each is
    # its own dump_runs row), exercising the multi-run ingest + the cross-run
    # cash_flows overwrite where a divergent rebuild would surface.
    for slug in ("20260101T010000Z", "20260401T010000Z"):
        _seed_holdings(make_dump(bronze_root, slug))

    # 1) Plain incremental load into a fresh DB → ingests every run.
    assert load.main(["--bronze-dir", str(bronze_root),
                      "--silver-db", str(db)]) == 0
    incremental = _dump_silver(db)
    # Guard against a vacuous pass: the load must actually have populated the
    # value-bearing tables, else "identical" below proves nothing.
    assert (incremental["dump_runs"] and incremental["securities"]
            and incremental["cash_flows"])

    # 2) --force over the SAME unchanged bronze: wipe the DB, rebuild from all
    # bronze.
    assert load.main(["--bronze-dir", str(bronze_root),
                      "--silver-db", str(db), "--force"]) == 0
    rebuilt = _dump_silver(db)

    # The rebuild reproduces the incremental state exactly, every data table.
    assert rebuilt == incremental
