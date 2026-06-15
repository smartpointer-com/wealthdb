"""
Bronze->silver tests for the fred collector's load.py.

Seeds a synthetic FRED bronze run (run.json manifest + per-series
observation JSON, mirroring the real API shape) and runs the real loader,
asserting the projected silver `fx_rates` rows. No real data — round
synthetic rates only.
"""

from __future__ import annotations

import json
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

RUN_SLUG = "20240102T030405Z"


def _epoch(d: str) -> int:
    return int(datetime.strptime(d, "%Y-%m-%d")
               .replace(tzinfo=timezone.utc).timestamp())


def _obs_doc(rows):
    return {"observations": [{"date": d, "value": v} for d, v in rows]}


def _seed_bronze(root: Path) -> Path:
    """One bronze run: DEXSZUS (CHF/USD) + DEXUSEU (USD/EUR) in the
    canonical convention — (base, quote, mid) means '1 quote = mid base' —
    with a '.' holiday sentinel that must be skipped."""
    run = root / RUN_SLUG
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({
        "slug": RUN_SLUG,
        "since": "2020-01-01", "until": "2020-01-03",
        "series": {
            "DEXSZUS": {"base": "CHF", "quote": "USD", "rows": 2},
            "DEXUSEU": {"base": "USD", "quote": "EUR", "rows": 2},
        },
    }), encoding="utf-8")
    (run / "DEXSZUS.json").write_text(json.dumps(_obs_doc([
        ("2020-01-01", "."),          # New Year holiday -> skipped
        ("2020-01-02", "0.9000"),     # 1 USD = 0.90 CHF
        ("2020-01-03", "0.9100"),
    ])), encoding="utf-8")
    (run / "DEXUSEU.json").write_text(json.dumps(_obs_doc([
        ("2020-01-02", "1.1200"),     # 1 EUR = 1.12 USD
        ("2020-01-03", "1.1300"),
    ])), encoding="utf-8")
    return run


def _rows(db: Path):
    conn = sqlite3.connect(db)
    conn.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in conn.execute(
            "SELECT snapshot_at, base_currency_iso, quote_currency_iso, mid "
            "FROM fx_rates ORDER BY base_currency_iso, snapshot_at")]
    finally:
        conn.close()


def test_load_projects_fx_rates(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_bronze(bronze)
    db = tmp_path / "fred.db"
    assert loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze)]) == 0

    rows = _rows(db)
    # 2 dated DEXSZUS + 2 DEXUSEU; the '.' row is skipped.
    assert len(rows) == 4
    chf = [r for r in rows if r["base_currency_iso"] == "CHF"]
    assert {r["quote_currency_iso"] for r in chf} == {"USD"}
    assert chf[0]["snapshot_at"] == _epoch("2020-01-02")
    assert chf[0]["mid"] == "0.9000"
    eur = [r for r in rows if r["quote_currency_iso"] == "EUR"]
    assert eur[0]["base_currency_iso"] == "USD"
    assert eur[0]["mid"] == "1.1200"


def test_load_is_idempotent(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_bronze(bronze)
    db = tmp_path / "fred.db"
    loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze)])
    before = _rows(db)
    # Re-running skips the already-loaded run (dump_runs) — no duplication.
    loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze)])
    assert _rows(db) == before


def test_force_reloads_and_upserts(tmp_path):
    bronze = tmp_path / "bronze"
    run = _seed_bronze(bronze)
    db = tmp_path / "fred.db"
    loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze)])

    # FRED revises 2020-01-02 CHF; a re-fetch (same run dir here) + --force
    # must overwrite the existing (date, base, quote) row, not duplicate it.
    (run / "DEXSZUS.json").write_text(json.dumps(_obs_doc([
        ("2020-01-02", "0.9050"),
        ("2020-01-03", "0.9100"),
    ])), encoding="utf-8")
    loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze), "--force"])

    rows = _rows(db)
    assert len(rows) == 4  # still 4 — upsert, not append
    revised = [r for r in rows
               if r["base_currency_iso"] == "CHF"
               and r["snapshot_at"] == _epoch("2020-01-02")]
    assert revised[0]["mid"] == "0.9050"


def test_schema_meta_version(tmp_path):
    db = tmp_path / "fred.db"
    loader.main(["--silver-db", str(db), "--bronze-dir", str(tmp_path / "empty")])
    conn = silver.open_db(db)
    try:
        assert silver.current_schema_version(conn) == 1
    finally:
        conn.close()
