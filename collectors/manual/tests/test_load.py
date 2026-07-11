"""Smoke + validation tests for the manual collector's load.py.

Runs the real loader against the committed synthetic example CSVs, and
exercises the validation paths with tiny in-test CSVs. No real data —
the example CSVs use obvious placeholders ("Property A", "GmbH X", …).
"""
from __future__ import annotations

import sqlite3
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
EXAMPLES = COLLECTOR / "examples"
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = loader.open_db(tmp_path / "manual.db")
    loader.apply_migrations(conn)
    return conn


def _write(d: Path, name: str, text: str) -> None:
    (d / name).write_text(text, encoding="utf-8")


# ----------------------------------------------------------------------
# Happy path against the committed examples.
# ----------------------------------------------------------------------
def test_load_examples(tmp_path):
    conn = _fresh_db(tmp_path)
    counts = loader.load(conn, EXAMPLES)
    assert counts == {"positions": 6, "valuations": 13}

    # Every position kind in the examples is an accepted kind.
    kinds = {r[0] for r in conn.execute(
        "SELECT DISTINCT kind FROM positions").fetchall()}
    assert kinds <= loader.POSITION_KINDS
    assert {"real_estate", "convertible_note", "private_equity"} <= kinds

    # The converted note is closed; the equity it became is open and links back
    # (a conversion is recorded purely position-side — closed_at + back-ref).
    cla = conn.execute(
        "SELECT closed_at FROM positions WHERE id='cla-002'").fetchone()
    assert cla[0] is not None
    linked = conn.execute(
        "SELECT json_extract(payload, '$.converted_from_position_id') "
        "FROM positions WHERE id='gmbh-002'").fetchone()[0]
    assert linked == "cla-002"


def test_load_is_idempotent(tmp_path):
    """A second load fully rebuilds from the same CSVs — same row counts,
    no duplicates."""
    conn = _fresh_db(tmp_path)
    loader.load(conn, EXAMPLES)
    loader.load(conn, EXAMPLES)
    assert conn.execute("SELECT COUNT(*) FROM positions").fetchone()[0] == 6
    assert conn.execute("SELECT COUNT(*) FROM valuations").fetchone()[0] == 13
    # load_runs is an append-only audit log.
    assert conn.execute("SELECT COUNT(*) FROM load_runs").fetchone()[0] == 2


def test_as_of_query_drops_converted_note(tmp_path):
    """As of the conversion date the note has dropped out and the new equity
    position is present — the property gold relies on for as-of correctness."""
    conn = _fresh_db(tmp_path)
    loader.load(conn, EXAMPLES)
    # Dates are ISO-8601 TEXT, so lexicographic comparison is chronological.
    live = {r[0] for r in conn.execute(
        "SELECT id FROM positions "
        "WHERE closed_at IS NULL OR closed_at > '2024-12-31'"
    ).fetchall()}
    assert "cla-002" not in live
    assert "gmbh-002" in live


# ----------------------------------------------------------------------
# Validation paths — each must raise LoadError with file:row:column.
# ----------------------------------------------------------------------
def test_dangling_valuation_reference(tmp_path):
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at\n"
           "p-1,convertible_note,X,CHF,2020-01-01\n")
    _write(tmp_path, "valuations.csv",
           "position_id,as_of_date,value,currency\n"
           "nope-9,2024-01-01,100,CHF\n")
    conn = _fresh_db(tmp_path)
    with pytest.raises(loader.LoadError, match=r"valuations.csv:row 2:position_id"):
        loader.load(conn, tmp_path)


def test_unknown_position_kind(tmp_path):
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at\n"
           "p-1,spaceship,X,CHF,2020-01-01\n")
    conn = _fresh_db(tmp_path)
    with pytest.raises(loader.LoadError, match=r"positions.csv:row 2:kind"):
        loader.load(conn, tmp_path)


def test_bad_json_payload(tmp_path):
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at,payload\n"
           "p-1,convertible_note,X,CHF,2020-01-01,{not json}\n")
    conn = _fresh_db(tmp_path)
    with pytest.raises(loader.LoadError, match=r"positions.csv:row 2:payload"):
        loader.load(conn, tmp_path)


def test_vehicle_defaults_from_kind(tmp_path):
    """When the CSV omits `vehicle` (or the whole column), each kind gets its
    default wrapper; an explicit vehicle overrides — notably escrow/loan for
    the kind=other catch-all."""
    _write(tmp_path, "positions.csv",
           "id,kind,vehicle,display_name,currency,acquired_at\n"
           "re,real_estate,,Prop,CHF,2020-01-01\n"        # default -> physical
           "pe,private_equity,,Co,CHF,2020-01-01\n"       # default -> stock
           "esc,other,escrow,Escrow,USD,2020-01-01\n"     # explicit
           "ln,other,loan,Loan,CHF,2020-01-01\n")         # explicit
    conn = _fresh_db(tmp_path)
    loader.load(conn, tmp_path)
    got = dict(conn.execute("SELECT id, vehicle FROM positions").fetchall())
    assert got == {"re": "physical", "pe": "stock", "esc": "escrow", "ln": "loan"}

    # A whole-column omission still defaults (back-compat with pre-0002 CSVs).
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at\n"
           "sp,spv,Deal,USD,2020-01-01\n")
    conn = _fresh_db(tmp_path)
    loader.load(conn, tmp_path)
    assert conn.execute("SELECT vehicle FROM positions WHERE id='sp'").fetchone()[0] == "spv"


def test_unknown_vehicle(tmp_path):
    _write(tmp_path, "positions.csv",
           "id,kind,vehicle,display_name,currency,acquired_at\n"
           "p-1,real_estate,spaceship,X,CHF,2020-01-01\n")
    conn = _fresh_db(tmp_path)
    with pytest.raises(loader.LoadError, match=r"positions.csv:row 2:vehicle"):
        loader.load(conn, tmp_path)


def test_converted_from_dangling_reference(tmp_path):
    """A position that back-references a converted_from_position_id which
    isn't in positions.csv fails loudly (the conversion-link integrity check
    that replaces the old conversion transaction)."""
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at,payload\n"
           'p-1,private_equity,X,CHF,2022-01-01,"{""converted_from_position_id"": ""nope-9""}"\n')
    conn = _fresh_db(tmp_path)
    with pytest.raises(loader.LoadError, match=r"converted_from_position_id"):
        loader.load(conn, tmp_path)


def test_valuation_currency_must_match_position(tmp_path):
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at\n"
           "p-1,real_estate,X,CHF,2020-01-01\n")
    _write(tmp_path, "valuations.csv",
           "position_id,as_of_date,value,currency\n"
           "p-1,2020-01-01,100,USD\n")
    conn = _fresh_db(tmp_path)
    with pytest.raises(loader.LoadError, match=r"currency USD != position"):
        loader.load(conn, tmp_path)


def test_unknown_column_is_rejected(tmp_path):
    """A typo'd column name fails loudly rather than being silently ignored."""
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at,oops\n"
           "p-1,convertible_note,X,CHF,2020-01-01,bad\n")
    conn = _fresh_db(tmp_path)
    with pytest.raises(loader.LoadError, match=r"unexpected column"):
        loader.load(conn, tmp_path)


def test_resolve_paths_precedence(tmp_path, monkeypatch):
    """flag > env var > default; the silver DB follows the resolved bronze
    dir unless overridden itself."""
    monkeypatch.delenv(loader.ENV_BRONZE_DIR, raising=False)
    monkeypatch.delenv(loader.ENV_SILVER_DB, raising=False)

    # default
    b, s = loader.resolve_paths(loader.parse_args([]))
    assert b == loader.DEFAULT_BRONZE_DIR
    assert s == loader.DEFAULT_BRONZE_DIR / loader.SILVER_DB_NAME

    # env var — silver follows the env-set bronze dir
    monkeypatch.setenv(loader.ENV_BRONZE_DIR, str(tmp_path / "data"))
    b, s = loader.resolve_paths(loader.parse_args([]))
    assert b == tmp_path / "data"
    assert s == tmp_path / "data" / loader.SILVER_DB_NAME

    # flag beats env var (for both paths)
    monkeypatch.setenv(loader.ENV_SILVER_DB, str(tmp_path / "env.db"))
    b, s = loader.resolve_paths(loader.parse_args(
        ["--bronze-dir", str(tmp_path / "flag"),
         "--silver-db", str(tmp_path / "flag.db")]))
    assert b == tmp_path / "flag"
    assert s == tmp_path / "flag.db"


def test_failed_load_leaves_silver_untouched(tmp_path):
    """A good load, then a bad one — the bad load rolls back, so the good
    data survives."""
    conn = _fresh_db(tmp_path)
    loader.load(conn, EXAMPLES)
    before = conn.execute("SELECT COUNT(*) FROM positions").fetchone()[0]
    _write(tmp_path, "positions.csv",
           "id,kind,display_name,currency,acquired_at\n"
           "p-1,spaceship,X,CHF,2020-01-01\n")
    with pytest.raises(loader.LoadError):
        loader.load(conn, tmp_path)
    after = conn.execute("SELECT COUNT(*) FROM positions").fetchone()[0]
    assert after == before
