"""Bronze→silver tests for the relevate collector's load.py.

Seeds a minimal synthetic Relevate bronze dump (investment-overview
+ per-portfolio modelportfolio) and runs the real loader, asserting
the projected silver rows. Synthetic ids / ISINs / amounts only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

RUN_SLUG = "20240101T000000Z"
ACC = "ACC1"


def _write_json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj), encoding="utf-8")


def _seed_bronze(root: Path) -> Path:
    run_dir = root / RUN_SLUG
    _write_json(run_dir / "run.json", {"utc": RUN_SLUG})
    _write_json(run_dir / "accounts" / "investment-overview.json", {
        "portfolios": [{
            "externalId": ACC,
            "id": 100,
            "product": {"key": "FZPF", "name": "PensFree", "productOfferId": 1},
            "currency": {"currencyCode": "CHF"},
            "name": "Strategy 50",
            "isActive": True,
            "cashBalance": 200.0,
            "securitiesBalance": 1000.0,
        }],
    })
    slug = loader.slug_for_account(ACC)
    _write_json(run_dir / "portfolios" / slug / "modelportfolio.json", {
        "positions": [{
            "security": {
                "id": 1,
                "isin": "CH0000000001",
                "name": "Equity Fund",
                "assetClass": {"name": "Stocks", "externalId": "EQ"},
                "country": {"countryCode": "CH"},
                "tradingPrice": 100.0,
                "tradingUnit": 1.0,
                "faceValue": 0.0,
            },
            "allocation": 0.6,
        }],
    })
    return run_dir


def _fresh_db(tmp_path: Path):
    conn = loader.open_db(tmp_path / "relevate.db")
    version = silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    return conn, version


def test_load_accounts_positions_cash(tmp_path):
    run_dir = _seed_bronze(tmp_path / "bronze")
    conn, version = _fresh_db(tmp_path)
    loader.load_one_dump(conn, run_dir, version)

    # Account row from investment-overview.
    acct = conn.execute(
        "SELECT account_external_id, product_key, product_name, "
        "currency_code, name FROM accounts").fetchall()
    assert len(acct) == 1
    assert acct[0]["account_external_id"] == ACC
    assert acct[0]["product_key"] == "FZPF"
    assert acct[0]["currency_code"] == "CHF"

    # Position: allocation (target fraction), keyed by internal id.
    pos = conn.execute(
        "SELECT instrument_external_id, isin, asset_class, allocation "
        "FROM positions").fetchall()
    assert len(pos) == 1
    assert pos[0]["instrument_external_id"] == "1"
    assert pos[0]["isin"] == "CH0000000001"
    assert pos[0]["asset_class"] == "Stocks"
    assert float(pos[0]["allocation"]) == 0.6

    # Instrument catalog row.
    instr = conn.execute("SELECT isin FROM instruments").fetchall()
    assert len(instr) == 1
    assert instr[0]["isin"] == "CH0000000001"

    # cash + securities balances both projected from the overview.
    bals = {r["balance_kind"]: float(r["amount"]) for r in conn.execute(
        "SELECT balance_kind, amount FROM cash_balances").fetchall()}
    assert bals["cash"] == 200.0
    assert bals["securities"] == 1000.0

    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1


# ============================================================
# list_pending_dumps: skip dumps the walk never finished
# ============================================================

def _dump_with_status(root: Path, slug: str, status) -> Path:
    """A minimal bronze run dir carrying a run.json with the given
    status (None = a statusless / pre-`status` manifest)."""
    d = root / slug
    manifest = {"tool": "relevate.download", "mode": "all", "dry_run": False}
    if status is not None:
        manifest["status"] = status
    _write_json(d / "run.json", manifest)
    _write_json(d / "accounts" / "investment-overview.json",
                {"portfolios": []})
    return d


def test_list_pending_skips_in_progress_and_dry_run(tmp_path):
    # A crashed walk (status="in-progress") and a --dry-run shell
    # (status="dry-run") must NOT be selected for load — run.json is
    # now present from run-dir creation, so its mere presence is not a
    # completeness signal.
    bronze = tmp_path / "bronze"
    _dump_with_status(bronze, "20240101T000000Z", "in-progress")
    _dump_with_status(bronze, "20240102T000000Z", "dry-run")
    complete = _dump_with_status(bronze, "20240103T000000Z", "complete")
    legacy = _dump_with_status(bronze, "20240104T000000Z", None)

    conn, _ = _fresh_db(tmp_path)
    pending = loader.list_pending_dumps(conn, bronze)

    names = {p.name for p in pending}
    # complete + statusless-legacy are loadable; the two unfinished
    # dumps are skipped.
    assert names == {complete.name, legacy.name}


def test_list_pending_skips_dir_without_run_json(tmp_path):
    bronze = tmp_path / "bronze"
    d = bronze / "20240105T000000Z"
    _write_json(d / "accounts" / "investment-overview.json",
                {"portfolios": []})
    conn, _ = _fresh_db(tmp_path)
    assert loader.list_pending_dumps(conn, bronze) == []


# ============================================================
# Parser generations — a re-parse replaces, it does not accumulate
# ============================================================

def _seed_pdf_pass_rows(conn):
    conn.execute(
        "INSERT OR REPLACE INTO historical_position_snapshots ("
        "snapshot_at, account_external_id, isin, security_name, currency, "
        "market_value, source_sha256, payload) "
        "VALUES (1700000000, '1000.100000.1', 'CH0000000001', "
        "'Example Fund', 'CHF', 1.0, 'sha0', '{}')")
    conn.execute(
        "INSERT OR REPLACE INTO historical_cash_balances ("
        "snapshot_at, account_external_id, currency, balance_kind, amount, "
        "source_sha256, payload) "
        "VALUES (1700000000, '1000.100000.1', 'CHF', 'cash', 1.0, "
        "'sha0', '{}')")


def test_the_purge_takes_everything_the_pdf_passes_wrote(tmp_path):
    conn, _ = _fresh_db(tmp_path)
    _seed_pdf_pass_rows(conn)

    assert loader._purge_stale_pdf_rows(conn) == 2
    assert loader._pdf_pass_row_count(conn) == 0
    conn.close()


def test_an_unmoved_report_parser_drops_nothing(tmp_path):
    conn, _ = _fresh_db(tmp_path)
    _seed_pdf_pass_rows(conn)
    silver.stamp_generation(conn, loader.PDF_GENERATION_SCOPE,
                            loader.PDF_GENERATION)

    assert loader._purge_stale_pdf_rows(conn) == 0
    assert loader._pdf_pass_row_count(conn) == 2
    conn.close()
