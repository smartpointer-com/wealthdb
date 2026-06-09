"""
Unit tests for load.py's side-loaded valuation override.

In-memory SQLite + a tmp_path bronze edir. Exercises the valuation-CSV
parsing, the carry-forward FMV lookup, and the core contract: a position's
share count AND per-share price both move correctly over time
(quantity-held-as-of x FMV-as-of), with exit on cancellation. Synthetic
data only — no real holdings, ids, or valuations.
"""
from __future__ import annotations

import datetime as dt
import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


def _ts(iso: str) -> int:
    return int(dt.datetime.fromisoformat(iso)
               .replace(tzinfo=dt.timezone.utc).timestamp())


@pytest.fixture
def migrated():
    """Fresh in-memory silver with all migrations applied."""
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    load.silver.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


# ---- CSV parsing + carry-forward -------------------------------------------

def test_read_valuation_csv_parses_sorts_and_skips_comments(tmp_path):
    p = tmp_path / "ACME.csv"
    p.write_text("# header comment\n"
                 "# date,fmv_per_share_usd\n"
                 "\n"
                 "2019-06-30,0.50\n"
                 "2018-01-01,0.10\n"   # deliberately out of order
                 "2021-12-31,3.00\n")
    assert load._read_valuation_csv(p) == [
        (_ts("2018-01-01"), 0.10),
        (_ts("2019-06-30"), 0.50),
        (_ts("2021-12-31"), 3.00),
    ]


def test_read_valuation_csv_absent(tmp_path):
    assert load._read_valuation_csv(tmp_path / "missing.csv") == []


def test_fmv_as_of_carries_forward():
    tl = [(_ts("2018-01-01"), 0.10), (_ts("2021-12-31"), 3.00)]
    assert load._fmv_as_of(tl, _ts("2017-06-01")) is None    # before first row
    assert load._fmv_as_of(tl, _ts("2018-01-01")) == 0.10    # exactly on it
    assert load._fmv_as_of(tl, _ts("2020-06-30")) == 0.10    # carried forward
    assert load._fmv_as_of(tl, _ts("2022-06-30")) == 3.00    # latest


# ---- load_securities_valued: count + price move over time ------------------

def _synthetic_edir(tmp_path: Path) -> Path:
    """Two share certs issued on different dates + one option grant."""
    edir = tmp_path / "entities" / "corp_42"
    edir.mkdir(parents=True)
    (edir / "shares.json").write_text(json.dumps({"rows": [
        {"id": 1, "label": "CS-1", "issue_date": "01/01/2021",
         "quantity": 1000, "currency": "$", "is_canceled": True},
        {"id": 2, "label": "CS-2", "issue_date": "01/01/2023",
         "quantity": 500, "currency": "$", "is_canceled": True},
    ]}))
    (edir / "options.json").write_text(json.dumps({"rows": [
        {"id": 3, "label": "ES-1", "issue_date": "01/01/2021",
         "quantity": 200, "exercise_price": 0.5, "is_canceled": True},
    ]}))
    return edir


def _holdings_as_of(conn, asof_ts):
    """Mirror the gold read: each position's latest delta <= D, drop exited.
    Returns (shares_held, total_market_value)."""
    rows = conn.execute(
        "WITH l AS (SELECT security_type, security_external_id, "
        "                  MAX(snapshot_at) s FROM securities "
        "            WHERE snapshot_at<=? GROUP BY 1,2) "
        "SELECT s.security_type, COALESCE(s.quantity,0), "
        "       COALESCE(s.market_value,0) "
        "  FROM securities s JOIN l ON s.security_type=l.security_type "
        "   AND s.security_external_id=l.security_external_id "
        "   AND s.snapshot_at=l.s "
        " WHERE s.position_status='held'", (asof_ts,)).fetchall()
    shares = sum(q for t, q, _ in rows if t == "share")
    value = sum(v for _, _, v in rows)
    return shares, value


def test_valued_count_and_price_over_time(migrated, tmp_path):
    edir = _synthetic_edir(tmp_path)
    fmv = [(_ts("2021-01-01"), 1.0), (_ts("2022-01-01"), 2.0),
           (_ts("2024-01-01"), 5.0)]
    cancel = _ts("2025-01-01")
    load.load_securities_valued(migrated, 42, edir,
                                fmv_timeline=fmv, cancel_ts=cancel)

    # only cert-1 held (cert-2 issued 2023), FMV 1.0
    assert _holdings_as_of(migrated, _ts("2021-06-01")) == (1000, 1000.0)
    # FMV stepped to 2.0
    assert _holdings_as_of(migrated, _ts("2022-06-01")) == (1000, 2000.0)
    # cert-2 now held too; both carry FMV 2.0
    assert _holdings_as_of(migrated, _ts("2023-06-01")) == (1500, 3000.0)
    # FMV stepped to 5.0
    assert _holdings_as_of(migrated, _ts("2024-06-01")) == (1500, 7500.0)
    # after cancellation everything is exited
    assert _holdings_as_of(migrated, _ts("2025-06-01")) == (0, 0.0)


def test_options_carry_zero_value(migrated, tmp_path):
    edir = _synthetic_edir(tmp_path)
    load.load_securities_valued(migrated, 42, edir,
                                fmv_timeline=[(_ts("2021-01-01"), 1.0)],
                                cancel_ts=None)
    vals = migrated.execute("SELECT DISTINCT market_value FROM securities "
                            "WHERE security_type='option'").fetchall()
    assert vals == [(0.0,)]
