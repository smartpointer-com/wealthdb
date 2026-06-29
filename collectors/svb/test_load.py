"""Tests for load.py — the svb builder's carry-forward (skip-empty), $0-closure,
and master-synthesis logic. The PDF parser is mocked, so no real statements are
needed and all data is synthetic."""
import sqlite3
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import load as B  # noqa: E402

MIGRATIONS = Path(__file__).parent / "migrations"

# Canned parser output keyed by filename stem. SVM-000000 plays an account
# held early with a later empty unwind statement (must carry forward, never
# closed). SVM-000001 plays an account still held at the latest real date
# (closed at the synthetic closure date).
_CANNED = {
    "2021-12-31_a": {
        "period_end": "2021-12-31",
        "accounts": [{"account_external_id": "SVM-000000", "holdings": [
            {"description": "AAAA EQUITY", "instrument_key": "AAAA",
             "quantity": 10.0, "price": 5.0, "market_value": 50.0}]}],
    },
    "2022-06-30_a": {  # unwind: no holdings → skipped, account carries forward
        "period_end": "2022-06-30",
        "accounts": [{"account_external_id": "SVM-000000", "holdings": []}],
    },
    "2022-12-30_b": {
        "period_end": "2022-12-31",
        "accounts": [{"account_external_id": "SVM-000001", "holdings": [
            {"description": "BBBB FUND", "instrument_key": "BBBB",
             "quantity": 20.0, "price": 10.0, "market_value": 200.0}]}],
    },
}


def _build(tmp_path, monkeypatch):
    bronze = tmp_path / "bronze"
    bronze.mkdir()
    for stem in _CANNED:
        (bronze / f"{stem}.pdf").write_bytes(b"%PDF-fake\n")
    monkeypatch.setattr(
        B.pdf_parsers_svbwa, "parse_svbwa_statement_pdf",
        lambda path, expected_signature=None: dict(_CANNED[Path(path).stem]))
    db = tmp_path / "svb.db"
    B.build(db, bronze, signature=None, closure_date="2023-09-30",
            migrations_dir=MIGRATIONS)
    return sqlite3.connect(str(db))


def test_migrations_lockstep_with_fidelity_web():
    """svb.db is read by the shared Fidelity gold adapter (kind:"fidelity"), so
    these migrations MUST stay byte-identical to fidelity-web's silver schema.
    This guard fails if either side drifts (see CLAUDE.md / DESIGN.md)."""
    fidelity = MIGRATIONS.parent.parent / "fidelity-web" / "migrations"
    for mig in sorted(MIGRATIONS.glob("*.sql")):
        twin = fidelity / mig.name
        assert twin.is_file(), f"{mig.name} has no fidelity-web counterpart"
        assert mig.read_bytes() == twin.read_bytes(), (
            f"{mig.name} drifted from fidelity-web/migrations — re-sync them")


def test_skip_empty_carries_forward(tmp_path, monkeypatch):
    conn = _build(tmp_path, monkeypatch)
    rows = conn.execute(
        "SELECT as_of_date, market_value FROM historical_position_snapshots "
        "WHERE account_external_id='SVM-000000' ORDER BY as_of_date").fetchall()
    # Only the 2021-12-31 holding row — the empty 2022-06 unwind inserted
    # nothing, so the account carries its $50 forward rather than zeroing.
    assert len(rows) == 1
    assert rows[0][0] == B.ts_from_iso("2021-12-31")
    assert rows[0][1] == 50.0
    # SVM-000000 is NOT closed (it was already superseded before the latest
    # real statement), so it gets no synthetic-closure row.
    closed = conn.execute(
        "SELECT COUNT(*) FROM historical_position_snapshots "
        "WHERE account_external_id='SVM-000000' AND source_sha256=?",
        (B._CLOSURE_SHA,)).fetchone()[0]
    assert closed == 0


def test_closure_injected_for_held_sleeve(tmp_path, monkeypatch):
    conn = _build(tmp_path, monkeypatch)
    rows = conn.execute(
        "SELECT as_of_date, market_value, source_sha256 FROM "
        "historical_position_snapshots WHERE account_external_id='SVM-000001' "
        "ORDER BY as_of_date").fetchall()
    assert len(rows) == 2
    # real holding at 2022-12-31, then a $0 closure at the 2023-09-30 handoff.
    assert rows[0] == (B.ts_from_iso("2022-12-31"), 200.0, _real_sha(conn))
    assert rows[1][0] == B.ts_from_iso("2023-09-30")
    assert rows[1][1] == 0.0
    assert rows[1][2] == B._CLOSURE_SHA


def test_masters_synthesised_neutral(tmp_path, monkeypatch):
    conn = _build(tmp_path, monkeypatch)
    accts = dict(conn.execute(
        "SELECT account_external_id, portfolio_external_id FROM accounts"))
    assert set(accts) == {"SVM-000000", "SVM-000001"}
    assert all(p == B._SYNTHETIC_PORTFOLIO for p in accts.values())
    kinds = [r[0] for r in conn.execute("SELECT kind FROM portfolios")]
    assert kinds and all(k == "other" for k in kinds)  # neutral → taxable default
    # the sleeve's master sits at its latest as_of (the closure date), so the
    # gold adapter projects it across the account's whole history.
    latest = conn.execute(
        "SELECT snapshot_at FROM accounts WHERE account_external_id='SVM-000001'"
    ).fetchone()[0]
    assert latest == B.ts_from_iso("2023-09-30")


def test_duplicate_option_descriptions_all_survive(tmp_path, monkeypatch):
    # Two short option legs share a description (strike is off-description) but
    # differ by OCC instrument_key; both must persist, so the negative legs
    # aren't collapsed and the signed total stays correct.
    canned = {"s": {"period_end": "2021-12-31", "accounts": [
        {"account_external_id": "SVM-000099", "holdings": [
            {"description": "CALL (XYZ) WIDGET JAN 18 30",
             "instrument_key": "XYZ300118C100", "quantity": -10.0,
             "price": 5.0, "market_value": -50.0},
            {"description": "CALL (XYZ) WIDGET JAN 18 30",
             "instrument_key": "XYZ300118C110", "quantity": -20.0,
             "price": 3.0, "market_value": -60.0},
            {"description": "WIDGET INC", "instrument_key": "XYZ",
             "quantity": 100.0, "price": 10.0, "market_value": 1000.0}]}]}}
    bronze = tmp_path / "bronze"
    bronze.mkdir()
    (bronze / "s.pdf").write_bytes(b"%PDF-fake\n")
    monkeypatch.setattr(
        B.pdf_parsers_svbwa, "parse_svbwa_statement_pdf",
        lambda path, expected_signature=None: dict(canned["s"]))
    db = tmp_path / "svb.db"
    B.build(db, bronze, signature=None, closure_date="2023-09-30",
            migrations_dir=MIGRATIONS)
    conn = sqlite3.connect(str(db))
    rows = conn.execute(
        "SELECT market_value FROM historical_position_snapshots "
        "WHERE account_external_id='SVM-000099' AND source_sha256<>? "
        "ORDER BY market_value", (B._CLOSURE_SHA,)).fetchall()
    assert len(rows) == 3, "all three legs must survive (no PK collapse)"
    assert sum(r[0] for r in rows) == 1000.0 - 50.0 - 60.0 == 890.0


def _real_sha(conn):
    return conn.execute(
        "SELECT source_sha256 FROM historical_position_snapshots "
        "WHERE account_external_id='SVM-000001' AND source_sha256<>?",
        (B._CLOSURE_SHA,)).fetchone()[0]
