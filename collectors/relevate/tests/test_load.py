"""Bronze→silver tests for the relevate collector's load.py.

Seeds a minimal synthetic Relevate bronze dump (investment-overview
+ per-portfolio modelportfolio) and runs the real loader, asserting
the projected silver rows. Synthetic ids / ISINs / amounts only.
"""
from __future__ import annotations

import hashlib
import json
import logging
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import bronze, silver  # noqa: E402

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
    slug = bronze.short_token(ACC)
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
    conn = silver.open_db(tmp_path / "relevate.db")
    version = silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    return conn, version


def _load(tmp_path: Path, bronze_root: Path):
    """Run the loader over `bronze_root` into tmp_path/relevate.db and
    return a connection to the silver."""
    db = tmp_path / "relevate.db"
    assert loader.main(["--silver-db", str(db),
                        "--bronze-dir", str(bronze_root)]) == 0
    return silver.open_db(db)


def test_load_accounts_positions_cash(tmp_path):
    _seed_bronze(tmp_path / "bronze")
    conn = _load(tmp_path, tmp_path / "bronze")

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


def test_state_minted_at_keeps_the_time_of_day(tmp_path):
    # The login session's mint time is a full timestamp; the column holds
    # its Unix seconds, not the midnight of its day.
    run_dir = _seed_bronze(tmp_path / "bronze")
    _write_json(run_dir / "run.json", {
        "utc": RUN_SLUG, "state_minted_at": "2024-01-01T12:34:56+00:00"})
    conn = _load(tmp_path, tmp_path / "bronze")
    minted = conn.execute(
        "SELECT state_minted_at FROM dump_runs").fetchone()[0]
    assert minted == 1704112496


def test_a_dump_that_raises_mid_load_rolls_back_whole(tmp_path):
    # A non-SQL error must roll the dump's transaction back too: no
    # half-written rows and no dump_runs row, so the dump stays pending.
    run_dir = _seed_bronze(tmp_path / "bronze")
    slug = bronze.short_token(ACC)
    (run_dir / "portfolios" / slug / "performance.json").write_text(
        "{not json", encoding="utf-8")
    db = tmp_path / "relevate.db"
    with pytest.raises(json.JSONDecodeError):
        loader.main(["--silver-db", str(db),
                     "--bronze-dir", str(tmp_path / "bronze")])
    conn = silver.open_db(db)
    for table in ("accounts", "positions", "dump_runs"):
        assert conn.execute(
            f"SELECT COUNT(*) FROM {table}").fetchone()[0] == 0


# ============================================================
# Pending dumps: skip dumps the walk never finished
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


def test_list_pending_skips_unfinished_dumps(tmp_path):
    # A crashed walk (status="in-progress"), a --dry-run shell
    # (status="dry-run") and a walk that aborted before any work
    # (status="incomplete") must NOT be selected for load — run.json is
    # present from run-dir creation, so its mere presence is not a
    # completeness signal.
    bronze = tmp_path / "bronze"
    _dump_with_status(bronze, "20240101T000000Z", "in-progress")
    _dump_with_status(bronze, "20240102T000000Z", "dry-run")
    complete = _dump_with_status(bronze, "20240103T000000Z", "complete")
    legacy = _dump_with_status(bronze, "20240104T000000Z", None)
    _dump_with_status(bronze, "20240105T000000Z", "incomplete")

    conn, _ = _fresh_db(tmp_path)
    pending = loader.SPEC.pending(conn, bronze, log=logging.getLogger())

    names = {p.name for p in pending}
    # complete + statusless-legacy are loadable; the unfinished dumps
    # are skipped.
    assert names == {complete.name, legacy.name}


def test_list_pending_skips_dir_without_run_json(tmp_path):
    bronze = tmp_path / "bronze"
    d = bronze / "20240105T000000Z"
    _write_json(d / "accounts" / "investment-overview.json",
                {"portfolios": []})
    conn, _ = _fresh_db(tmp_path)
    assert loader.SPEC.pending(conn, bronze, log=logging.getLogger()) == []


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

    assert loader._purge_stale_pdf_rows(conn, True) == 2
    assert loader._pdf_pass_row_count(conn) == 0
    conn.close()


def test_an_unmoved_report_parser_drops_nothing(tmp_path):
    # The generation verdict is the caller's now; what this pins is that a
    # negative one is an early return rather than a no-op delete.
    conn, _ = _fresh_db(tmp_path)
    _seed_pdf_pass_rows(conn)

    assert loader._purge_stale_pdf_rows(conn, False) == 0
    assert loader._pdf_pass_row_count(conn) == 2
    conn.close()


# ============================================================
# Documents: a restated document supersedes, it does not collide
# ============================================================

DOC_ID = 4242
FIRST_RUN = "20240101T000000Z"
RESTATED_RUN = "20240401T000000Z"


def _seed_document_dump(root: Path, slug: str, body: bytes) -> Path:
    """A bronze run dir carrying one indexed document. The documents phase
    hashes the file and never parses it, so `body` stands in for the PDF."""
    d = root / slug
    _write_json(d / "run.json", {"tool": "relevate.download", "status": "complete"})
    _write_json(d / "accounts" / "investment-overview.json", {"portfolios": []})
    _write_json(d / "documents" / "index.json", {"documents": [
        {"id": DOC_ID, "fileName": "Quartalsbericht.pdf", "documentYear": 2024}]})
    (d / "documents" / f"{DOC_ID}.pdf").write_bytes(body)
    return d


def test_a_restated_document_supersedes_the_row_it_restates(tmp_path):
    # Relevate re-issues one document id with corrected content: the sha
    # lookup misses, and without superseding the row already holding that
    # id the insert violated UNIQUE(relevate_doc_id) — taking down the
    # whole dump, which then stayed pending forever.
    bronze_root = tmp_path / "bronze"
    _seed_document_dump(bronze_root, FIRST_RUN, b"%PDF-1.4 original")
    _seed_document_dump(bronze_root, RESTATED_RUN, b"%PDF-1.4 restated")
    conn = _load(tmp_path, bronze_root)

    rows = conn.execute(
        "SELECT content_sha256, relevate_doc_id, bronze_path FROM documents"
    ).fetchall()
    # One row per provider document id, holding the restated content.
    assert len(rows) == 1
    assert rows[0][1] == DOC_ID
    assert rows[0][0] == hashlib.sha256(b"%PDF-1.4 restated").hexdigest()
    assert rows[0][2] == f"{RESTATED_RUN}/documents/{DOC_ID}.pdf"
    # And the dump COMMITTED — the restatement did not abort the load.
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 2
    conn.close()


def test_unchanged_content_refreshes_rather_than_superseding(tmp_path):
    # The other half of the branch: identical bytes in a later dump keep
    # the one row and advance last_seen_at. Guards a supersede that fires
    # too eagerly and resets first_seen_at on every load.
    bronze_root = tmp_path / "bronze"
    _seed_document_dump(bronze_root, FIRST_RUN, b"%PDF-1.4 original")
    _seed_document_dump(bronze_root, RESTATED_RUN, b"%PDF-1.4 original")
    conn = _load(tmp_path, bronze_root)

    rows = conn.execute(
        "SELECT first_seen_at, last_seen_at FROM documents").fetchall()
    assert len(rows) == 1
    assert rows[0][0] == bronze.parse_run_ts(FIRST_RUN)
    assert rows[0][1] == bronze.parse_run_ts(RESTATED_RUN)
    conn.close()


# ============================================================
# Parse cache: a load parses a PDF once per parser generation
# ============================================================

def _counting_parsers(monkeypatch, calls: list):
    """Stand-ins for the two PDF parsers that record each call and
    return a synthetic result shaped like the real one."""
    def quarterly(pdf, sha):
        calls.append(("quarterly", pdf.name))
        return {"as_of_date": 1700000000, "account_external_id": "1000.100000.1",
                "currency": "CHF", "source_sha256": sha,
                "positions": [{"isin": "XX0000000001", "security_name": "Example Fund",
                               "currency": "CHF", "units": 1.5,
                               "allocation_pct": 0.25, "market_value": 10.0,
                               "asset_class": "Stocks"}],
                "cash": [{"balance_kind": "cash", "currency": "CHF", "amount": 2.0}]}

    def credit_note(pdf, sha):
        calls.append(("credit_note", pdf.name))
        return {"occurred_at": 1700000000, "account_external_id": "1000.100000.1",
                "currency": "CHF", "amount": 3.5, "kind": "contribution",
                "source_sha256": sha}

    monkeypatch.setattr(loader, "parse_quarterly_report", quarterly)
    monkeypatch.setattr(loader, "parse_credit_note", credit_note)


def _seed_pdf_dump(root: Path, slug: str) -> Path:
    d = root / slug
    _write_json(d / "run.json", {"tool": "relevate.download", "status": "complete"})
    _write_json(d / "accounts" / "investment-overview.json", {"portfolios": []})
    _write_json(d / "documents" / "index.json", {"documents": [
        {"id": 1, "fileName": "Quartalsbericht.pdf"},
        {"id": 2, "fileName": "Gutschriftsanzeige.pdf"}]})
    (d / "documents" / "1.pdf").write_bytes(b"%PDF-1.4 report")
    (d / "documents" / "2.pdf").write_bytes(b"%PDF-1.4 credit note")
    return d


def test_a_second_load_calls_no_parser(tmp_path, monkeypatch):
    calls: list = []
    _counting_parsers(monkeypatch, calls)
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)

    _load(tmp_path, bronze_root).close()
    assert sorted(calls) == [("credit_note", "2.pdf"), ("quarterly", "1.pdf")]
    calls.clear()
    conn = _load(tmp_path, bronze_root)
    assert calls == []
    assert loader._pdf_pass_row_count(conn) == 3
    conn.close()


def test_a_rebuild_reads_the_parse_cache(tmp_path, monkeypatch):
    calls: list = []
    _counting_parsers(monkeypatch, calls)
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    _load(tmp_path, bronze_root).close()
    calls.clear()

    db = tmp_path / "relevate.db"
    assert loader.main(["--silver-db", str(db), "--bronze-dir",
                        str(bronze_root), "--force"]) == 0
    assert calls == []


# ============================================================
# Derivations: a document is derived once per parser generation
# ============================================================

SECOND_RUN = "20240201T000000Z"


def test_a_second_load_derives_nothing_even_without_the_cache(
        tmp_path, monkeypatch):
    calls: list = []
    _counting_parsers(monkeypatch, calls)
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    _load(tmp_path, bronze_root).close()
    calls.clear()

    monkeypatch.setenv("WEALTHDB_CACHE_DIR", str(tmp_path / "other-cache"))
    _load(tmp_path, bronze_root).close()
    assert calls == []


def test_a_new_document_is_the_only_one_parsed(tmp_path, monkeypatch):
    calls: list = []
    _counting_parsers(monkeypatch, calls)
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    _load(tmp_path, bronze_root).close()
    calls.clear()

    d = _seed_pdf_dump(bronze_root, SECOND_RUN)
    _write_json(d / "documents" / "index.json", {"documents": [
        {"id": 1, "fileName": "Quartalsbericht.pdf"},
        {"id": 2, "fileName": "Gutschriftsanzeige.pdf"},
        {"id": 3, "fileName": "Quartalsbericht.pdf"}]})
    (d / "documents" / "3.pdf").write_bytes(b"%PDF-1.4 next report")
    _load(tmp_path, bronze_root).close()
    assert calls == [("quarterly", "3.pdf")]


def test_a_credit_note_takes_the_newest_dump_at_every_load(
        tmp_path, monkeypatch):
    _counting_parsers(monkeypatch, [])
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    _load(tmp_path, bronze_root).close()

    d = bronze_root / SECOND_RUN
    _write_json(d / "run.json", {"tool": "relevate.download", "status": "complete"})
    _write_json(d / "accounts" / "investment-overview.json", {"portfolios": []})
    conn = _load(tmp_path, bronze_root)
    assert conn.execute(
        "SELECT snapshot_at FROM transactions WHERE source = 'credit_note_pdf'"
    ).fetchone()[0] == bronze.parse_run_ts(SECOND_RUN)
    conn.close()


def test_a_moved_generation_derives_every_document_again(
        tmp_path, monkeypatch):
    calls: list = []
    _counting_parsers(monkeypatch, calls)
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    _load(tmp_path, bronze_root).close()
    calls.clear()

    _move_generation(monkeypatch)
    conn = _load(tmp_path, bronze_root)
    assert sorted(calls) == [("credit_note", "2.pdf"), ("quarterly", "1.pdf")]
    assert loader._pdf_pass_row_count(conn) == 3
    assert {r[0] for r in conn.execute(
        "SELECT generation FROM document_derivations")} == {"g2"}
    conn.close()


def _move_generation(monkeypatch):
    monkeypatch.setattr(loader, "PDF_GENERATION", "g2")
    monkeypatch.setattr(loader, "QUARTERLY_NAMESPACE", "quarterly-g2")
    monkeypatch.setattr(loader, "CREDIT_NOTE_NAMESPACE", "credit-note-g2")


def test_a_short_re_derivation_is_not_recorded(tmp_path, monkeypatch):
    # Fewer rows than before: the rows are replaced and the new generation
    # is not recorded, so the next load drops and derives them once more.
    # That load compares against the short count, so it records the
    # generation.
    calls: list = []
    _counting_parsers(monkeypatch, calls)
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    conn = _load(tmp_path, bronze_root)
    # A row the old parser wrote under a key the new one no longer reads.
    conn.execute(
        "INSERT INTO historical_cash_balances (snapshot_at, "
        "account_external_id, currency, balance_kind, amount, source_sha256, "
        "payload) VALUES (1600000000, '1000.100000.1', 'CHF', 'cash', 1.0, "
        "'sha0', '{}')")
    conn.close()

    _move_generation(monkeypatch)
    for recorded in (False, True):
        conn = _load(tmp_path, bronze_root)
        assert loader._pdf_pass_row_count(conn) == 3
        assert ("g2" in {r[0] for r in conn.execute(
            "SELECT generation FROM document_derivations")}) == recorded
        conn.close()


def test_a_pdf_that_does_not_parse_is_parsed_once(tmp_path, monkeypatch):
    # The parse is a function of the bytes and the generation, so a
    # failure is recorded with no rows and a second load reads nothing.
    calls: list = []
    _counting_parsers(monkeypatch, calls)

    def broken(pdf, sha):
        calls.append(("quarterly", pdf.name))
        raise ValueError("no valuation date")

    monkeypatch.setattr(loader, "parse_quarterly_report", broken)
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    conn = _load(tmp_path, bronze_root)
    assert sorted(calls) == [("credit_note", "2.pdf"), ("quarterly", "1.pdf")]
    assert sorted(r[0] for r in conn.execute(
        "SELECT row_count FROM document_derivations")) == [0, 1]
    conn.close()
    calls.clear()

    _load(tmp_path, bronze_root).close()
    assert calls == []


def test_a_pdf_missing_on_disk_is_tried_at_the_next_load(
        tmp_path, monkeypatch):
    calls: list = []
    _counting_parsers(monkeypatch, calls)
    bronze_root = tmp_path / "bronze"
    d = _seed_pdf_dump(bronze_root, FIRST_RUN)
    conn = _load(tmp_path, bronze_root)
    # The report is pending again, and its PDF is gone.
    conn.execute("DELETE FROM document_derivations WHERE content_sha256 = ?",
                 (hashlib.sha256(b"%PDF-1.4 report").hexdigest(),))
    conn.close()
    pdf = d / "documents" / "1.pdf"
    pdf.unlink()
    calls.clear()

    conn = _load(tmp_path, bronze_root)
    assert calls == []
    assert conn.execute(
        "SELECT COUNT(*) FROM document_derivations").fetchone()[0] == 1
    conn.close()

    # Restored, it is derived (from the parse cache) and recorded.
    pdf.write_bytes(b"%PDF-1.4 report")
    conn = _load(tmp_path, bronze_root)
    assert conn.execute(
        "SELECT COUNT(*) FROM document_derivations").fetchone()[0] == 2
    assert loader._pdf_pass_row_count(conn) == 3
    conn.close()


def _restating_parser(monkeypatch):
    """A stand-in report parser that reads the valuation date off the
    bytes, and fails on bytes that say so."""
    def quarterly(pdf, sha):
        body = pdf.read_bytes()
        if b"broken" in body:
            raise ValueError("no valuation date")
        return {"as_of_date": 1800000000 if b"restated" in body else 1700000000,
                "account_external_id": "1000.100000.1", "currency": "CHF",
                "source_sha256": sha,
                "positions": [{"isin": "XX0000000001",
                               "security_name": "Example Fund",
                               "currency": "CHF", "market_value": 10.0}],
                "cash": [{"balance_kind": "cash", "currency": "CHF",
                          "amount": 2.0}]}

    monkeypatch.setattr(loader, "parse_quarterly_report", quarterly)


@pytest.mark.parametrize("restated, dates", [
    (b"%PDF-1.4 restated", [1800000000]),
    (b"%PDF-1.4 broken", []),
])
def test_a_restatement_leaves_no_row_of_the_older_content(
        tmp_path, monkeypatch, restated, dates):
    # The restatement moves the valuation date, or does not parse: either
    # way the older content's rows go, as they would under --force.
    _counting_parsers(monkeypatch, [])
    _restating_parser(monkeypatch)
    bronze_root = tmp_path / "bronze"
    _seed_document_dump(bronze_root, FIRST_RUN, b"%PDF-1.4 original")
    _load(tmp_path, bronze_root).close()
    _seed_document_dump(bronze_root, RESTATED_RUN, restated)
    conn = _load(tmp_path, bronze_root)
    for table in ("historical_position_snapshots", "historical_cash_balances"):
        assert [r[0] for r in conn.execute(
            f"SELECT snapshot_at FROM {table}")] == dates
    conn.close()


def _version_3_silver(tmp_path: Path, stamp: str | None) -> Path:
    """A silver migrated to version 3, holding one derived report."""
    db = tmp_path / "relevate.db"
    conn = silver.open_db(db)
    conn.executescript("\n".join(
        (loader.MIGRATIONS_DIR / name).read_text(encoding="utf-8")
        for name in ("0001_initial.sql", "0002_historical_snapshots.sql",
                     "0003_parser_generations.sql")))
    conn.execute(
        "INSERT INTO documents (content_sha256, relevate_doc_id, file_name, "
        "file_size, doc_kind, bronze_path, first_seen_at, last_seen_at, "
        "payload) VALUES ('sha1', 1, 'Quartalsbericht.pdf', 1, "
        "'quarterly_report', 'x/documents/1.pdf', 1, 1, '{}')")
    _seed_pdf_pass_rows(conn)
    conn.execute(
        "UPDATE historical_position_snapshots SET source_sha256 = 'sha1'")
    if stamp is not None:
        conn.execute(
            "INSERT INTO parser_generations VALUES ('pdf_reports', ?, 1)",
            (stamp,))
    conn.close()
    return db


@pytest.mark.parametrize("stamp, generation", [("g1", "g1"), (None, "")])
def test_the_migration_seeds_the_stamped_generation(
        tmp_path, stamp, generation):
    db = _version_3_silver(tmp_path, stamp)
    conn = silver.open_db(db)
    silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    assert [tuple(r) for r in conn.execute(
        "SELECT content_sha256, scope, generation, row_count "
        "FROM document_derivations")] == [("sha1", "pdf_reports", generation, 1)]
    assert conn.execute(
        "SELECT 1 FROM sqlite_master WHERE name = 'parser_generations'"
    ).fetchone() is None
    conn.close()


def test_a_credit_note_names_the_dump_that_brought_it(tmp_path, monkeypatch):
    # The PDF pass runs after the dumps; the stamp is the dump that first
    # indexed the document, so later dumps leave it as it is.
    _counting_parsers(monkeypatch, [])
    bronze_root = tmp_path / "bronze"
    _seed_pdf_dump(bronze_root, FIRST_RUN)
    _seed_pdf_dump(bronze_root, SECOND_RUN)
    conn = _load(tmp_path, bronze_root)
    assert conn.execute(
        "SELECT run_snapshot_at FROM transactions "
        "WHERE source = 'credit_note_pdf'").fetchone()[0] == \
        bronze.parse_run_ts(FIRST_RUN)
    conn.close()
