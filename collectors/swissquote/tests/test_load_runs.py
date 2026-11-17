"""
Tests for whole loads over bronze dumps: each dump through the load
frame, then the manual and Portfolio Performance passes.

Every fixture is synthetic: placeholder ISINs, an invented customer id.

Run from the repo root inside the container:
    python3 -m pytest tests
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from unittest import mock

import pytest

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import load  # noqa: E402
import pp_parser  # noqa: E402
from collectorkit import incremental  # noqa: E402
from tests.helpers import write_xls  # noqa: E402

ACCOUNT = "SYN-0001"
CSV_HEADER = ";".join(load.EXPECTED_CSV_HEADER)


def csv_row(date: str, kind: str, net: str, *, isin: str = "",
            symbol: str = "", quantity: str = "") -> str:
    """One transactions CSV line in the export's column order."""
    return ";".join([date, "00000000", kind, symbol, "", isin, quantity,
                     "", "", "", net, "0.0", "CHF"])


def statement_page(quantity: str, day: str = "2025-12-31") -> str:
    """A statement as of `day`: a first line with the day, then a
    layout-mode asset-allocation page with one fund."""
    return (
        f"{day}\n"
        "  Funds in          CHF\n"
        "          Quantity     Security                     ISIN"
        "              Average price      Market Price     Price date"
        "      Valuation in CHF       %\n"
        f"             {quantity}       Fictional Equity Fund        "
        "XX0000000003        95.000            150.000       31.12.2025"
        "         15'000.00      60.00\n")


def read_statement(pdf_path: Path) -> dict:
    """Stand-in for parse_portfolio_performance: the "PDF" holds the
    day of the statement and the layout text of its asset-allocation
    page."""
    day, page = Path(pdf_path).read_text(encoding="utf-8").split("\n", 1)
    return {"snapshot_date": day, "account_external_id": ACCOUNT,
            "positions": pp_parser._pp_parse_asset_allocation(page, Path(pdf_path))}


def write_dump(bronze: Path, slug: str, *, tx_rows: list[str],
               quantity: float = 10.0,
               statements: dict[str, str] | None = None) -> Path:
    """A complete dump as download.py lays it out: the roster, the
    list of assets, the positions export, one transactions CSV and the
    statements `{doc_id: page text}`."""
    run = bronze / slug
    run.mkdir(parents=True)
    (run / "accounts.json").write_text(json.dumps([
        {"account_external_id": ACCOUNT, "account_product": "Trading"}]))
    write_xls(run / "list_of_assets.xls", [
        load.LOA_EXPECTED_HEADER,
        ["CHF", 1.0, 500.0, 1000.0, 1500.0, 1500.0, 100.0],
        ["Total CHF", "", "", "", "", 1500.0, 100.0]])
    write_xls(run / "positions.xls", [
        load.POSITIONS_EXPECTED_HEADER, ["ETFs"],
        ["", "FAKEETF", quantity, 95.0, 1000.0, 1.0, 0.1, 100.0, "CHF",
         50.0, 5.0, 1000.0, 100.0]])
    (run / "transactions_001.csv").write_text(
        "\n".join([CSV_HEADER, *tx_rows]) + "\n", encoding="utf-8")
    documents = []
    for doc_id, text in (statements or {}).items():
        (run / "documents").mkdir(exist_ok=True)
        (run / "documents" / f"{doc_id}.pdf").write_text(text, encoding="utf-8")
        documents.append({"doc_id": doc_id, "doc_type": "Portfolio performance",
                          "date": "31.12.2025"})
    (run / "run.json").write_text(json.dumps({
        "status": "complete", "customer_id": ACCOUNT, "documents": documents,
        "transactions": [{"file": "transactions_001.csv",
                          "window_start": "2026-01-01",
                          "window_end": "2026-03-31"}]}))
    return run


def argv(db: Path, bronze: Path) -> list[str]:
    return ["--silver-db", str(db), "--bronze-dir", str(bronze)]


@pytest.fixture(autouse=True)
def _statements_read_as_text():
    with mock.patch.object(load, "parse_portfolio_performance", read_statement):
        yield


def test_runs_loaded_one_at_a_time_build_what_force_builds(tmp_path):
    # The later dump restates part of the earlier one's span and adds a
    # day, moves a position, holds the same statement again and a new one.
    # Between the two, the manual dir arrives: a file of its own, and a
    # copy of the later dump's new statement.
    fixture = tmp_path / "fixture"
    first = write_dump(fixture, "20260105T120000Z", tx_rows=[
        csv_row("02-01-2026 10:00:00", "Payment", "500.0"),
        csv_row("03-01-2026 10:00:00", "Credit", "1.0")],
        statements={"doc-0001": statement_page("100")})
    manual = fixture / "manual"
    manual.mkdir()
    (manual / "tax-statement.pdf").write_text("a manual file", encoding="utf-8")
    (manual / "statement-copy.pdf").write_text(
        statement_page("120", "2024-12-31"), encoding="utf-8")
    second = write_dump(fixture, "20260106T120000Z", quantity=12.0, tx_rows=[
        csv_row("03-01-2026 10:00:00", "Credit", "1.5"),
        csv_row("05-01-2026 10:00:00", "Custody Fees", "-5.0")],
        statements={"doc-0001": statement_page("100"),
                    "doc-0002": statement_page("120", "2024-12-31")})
    incremental.check(load.main, argv, [first, manual, second],
                      tmp_path / "work")
    rows = incremental.silver_rows(tmp_path / "work" / "at-once.db")
    assert len(rows["documents"]) == 3
    assert sum("pp:doc-0002" in r for r in rows["positions"]) == 1


def _statement_rows(db: Path) -> list[tuple]:
    conn = load.open_db(db)
    try:
        return [tuple(r) for r in conn.execute(
            "SELECT source, snapshot_at, symbol, payload FROM positions "
            "WHERE source LIKE 'pp:%' ORDER BY source")]
    finally:
        conn.close()


def _derived(db: Path) -> list[tuple]:
    conn = load.open_db(db)
    try:
        return [tuple(r) for r in conn.execute(
            "SELECT scope, generation, row_count FROM document_derivations "
            "ORDER BY content_sha256")]
    finally:
        conn.close()


def _two_statements(bronze: Path) -> None:
    write_dump(bronze, "20260105T120000Z", tx_rows=[], statements={
        "doc-0001": statement_page("100"),
        "doc-0002": statement_page("120", "2024-12-31")})


def test_a_second_load_parses_no_statement(tmp_path):
    bronze = tmp_path / "bronze"
    _two_statements(bronze)
    db = tmp_path / "swissquote.db"
    with mock.patch.object(load, "parse_portfolio_performance",
                           wraps=read_statement) as parse:
        assert load.main(argv(db, bronze)) == 0
        assert parse.call_count == 2
        assert load.main(argv(db, bronze)) == 0
        assert parse.call_count == 2
    assert _derived(db) == [(load.PP_SCOPE, load.PP_GENERATION, 1)] * 2


def test_a_new_generation_parses_every_statement_again(tmp_path):
    bronze = tmp_path / "bronze"
    _two_statements(bronze)
    db = tmp_path / "swissquote.db"
    assert load.main(argv(db, bronze)) == 0
    before = _statement_rows(db)
    with mock.patch.object(load, "PP_GENERATION", "another"), \
            mock.patch.object(load, "parse_portfolio_performance",
                              wraps=read_statement) as parse:
        assert load.main(argv(db, bronze)) == 0
    assert parse.call_count == 2
    assert _statement_rows(db) == before
    assert _derived(db) == [(load.PP_SCOPE, "another", 1)] * 2


def test_a_statement_that_fails_to_parse_waits_for_a_parser_change(tmp_path):
    # The parse is a function of the bytes and the generation: a failure
    # is recorded with no rows, and only a new generation reads it again.
    bronze = tmp_path / "bronze"
    _two_statements(bronze)
    db = tmp_path / "swissquote.db"

    def fail_one(path):
        if Path(path).stem == "doc-0002":
            raise SystemExit("layout shifted")
        return read_statement(path)

    with mock.patch.object(load, "parse_portfolio_performance", fail_one):
        assert load.main(argv(db, bronze)) == 0
    assert [r[0] for r in _statement_rows(db)] == ["pp:doc-0001"]
    assert sorted(r[2] for r in _derived(db)) == [0, 1]
    with mock.patch.object(load, "parse_portfolio_performance",
                           wraps=read_statement) as parse:
        assert load.main(argv(db, bronze)) == 0
        assert parse.call_count == 0
        with mock.patch.object(load, "PP_GENERATION", "another"):
            assert load.main(argv(db, bronze)) == 0
        assert parse.call_count == 2
    assert [r[0] for r in _statement_rows(db)] == ["pp:doc-0001", "pp:doc-0002"]


def test_a_dump_without_a_customer_id_rolls_back(tmp_path):
    bronze = tmp_path / "bronze"
    run = write_dump(bronze, "20260105T120000Z", tx_rows=[
        csv_row("02-01-2026 10:00:00", "Payment", "500.0")])
    meta = json.loads((run / "run.json").read_text())
    del meta["customer_id"]
    (run / "run.json").write_text(json.dumps(meta))
    db = tmp_path / "swissquote.db"
    with pytest.raises(SystemExit) as stop:
        load.main(argv(db, bronze))
    assert stop.value.code not in (0, None)
    conn = load.open_db(db)
    try:
        for table in ("dump_runs", "accounts", "transactions"):
            assert conn.execute(
                f"SELECT COUNT(*) FROM {table}").fetchone()[0] == 0
    finally:
        conn.close()


def test_a_missing_bronze_dir_creates_no_silver(tmp_path):
    db = tmp_path / "swissquote.db"
    with pytest.raises(SystemExit) as stop:
        load.main(argv(db, tmp_path / "absent"))
    assert stop.value.code not in (0, None)
    assert not db.exists()


def test_statement_rows_of_a_silver_before_derivations_are_parsed_once(tmp_path):
    # A silver at schema 7 holds statement rows and no record of the
    # parser that wrote them: the next load parses each statement once,
    # to the same rows.
    bronze = tmp_path / "bronze"
    _two_statements(bronze)
    db = tmp_path / "swissquote.db"
    assert load.main(argv(db, bronze)) == 0
    before = _statement_rows(db)
    conn = load.open_db(db)
    conn.executescript(
        "DROP TABLE document_derivations;"
        "DROP INDEX transactions_run_snapshot_at;"
        "ALTER TABLE transactions DROP COLUMN run_snapshot_at;"
        "DELETE FROM schema_meta WHERE silver_schema_version >= 8;")
    conn.close()
    with mock.patch.object(load, "parse_portfolio_performance",
                           wraps=read_statement) as parse:
        assert load.main(argv(db, bronze)) == 0
        assert load.main(argv(db, bronze)) == 0
    assert parse.call_count == 2
    assert _statement_rows(db) == before


def test_a_document_downloaded_again_with_other_bytes_keeps_the_first(tmp_path, caplog):
    # A later dump holds a document under an id silver already indexed,
    # rendered anew. The first copy stays indexed and parsed; the later
    # one is skipped with a warning, and the load goes on.
    bronze = tmp_path / "bronze"
    write_dump(bronze, "20260105T120000Z", tx_rows=[],
               statements={"doc-0001": statement_page("100")})
    write_dump(bronze, "20260106T120000Z", tx_rows=[],
               statements={"doc-0001": statement_page("100") + "\n",
                           "doc-0002": statement_page("120", "2024-12-31")})
    db = tmp_path / "swissquote.db"
    with caplog.at_level("WARNING", logger=load.log.name):
        assert load.main(argv(db, bronze)) == 0
    assert "doc-0001.pdf" in caplog.text
    conn = load.open_db(db)
    try:
        assert [tuple(r) for r in conn.execute(
            "SELECT swissquote_doc_id, first_seen_at FROM documents "
            "ORDER BY swissquote_doc_id")] == [
            ("doc-0001", load.bronze.parse_run_ts("20260105T120000Z")),
            ("doc-0002", load.bronze.parse_run_ts("20260106T120000Z"))]
        assert len(load.silver.loaded_snapshots(conn)) == 2
    finally:
        conn.close()
    assert [r[0] for r in _statement_rows(db)] == ["pp:doc-0001", "pp:doc-0002"]
