"""
Unit tests for load.py.

In-memory SQLite plus a tmp_path bronze-tree fixture. Exercises
the migration loop, the content-dedup contract on accounts, the
sha256-keyed documents table, the synthetic-activity_id contract
on transactions, and the per-run idempotency of load_run.
"""

from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


# ============================================================
# Fixtures
# ============================================================

@pytest.fixture
def conn():
    """Fresh in-memory SQLite per test."""
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    yield c
    c.close()


@pytest.fixture
def migrated(conn):
    """In-memory DB with all migrations applied."""
    load.apply_migrations(conn, MIGRATIONS_DIR)
    return conn


def _write_pdf(path: Path, content: bytes) -> str:
    """Write a stub PDF, return its sha256."""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(content)
    sha, _ = load.sha256_file(path)
    return sha


def _make_bronze_run(root: Path, run_ts: str, accounts: list[dict]) -> Path:
    """Build a bronze tree matching what download.walk() emits.

    `accounts` is a list of {"suffix", "label", "documents"}. Each
    document is {"date", "type", "document", "filename", "format"}.
    Each PDF body is constructed deterministically from the
    filename so different filenames produce different sha256s
    (otherwise the documents-table dedup absorbs everything
    after the first row).
    """
    run_dir = root / run_ts
    run_dir.mkdir(parents=True, exist_ok=True)
    manifest = {
        "run_ts": run_ts,
        "mode": "statements",
        "dry_run": False,
        "statements": [],
        "transactions": [],
    }
    for acct in accounts:
        stmts = []
        for doc in acct["documents"]:
            pdf_path = run_dir / "statements" / acct["suffix"] / doc["filename"]
            # Per-filename content keeps sha256s distinct.
            content = (
                f"%PDF-1.4 stub for {doc['filename']}\n".encode("utf-8")
            )
            sha = _write_pdf(pdf_path, content)
            stmts.append({
                "date": doc["date"],
                "type": doc["type"],
                "document": doc.get("document", ""),
                "filename": doc["filename"],
                "format": doc.get("format", "pdf"),
                "size": pdf_path.stat().st_size,
                "sha256": sha,
            })
        manifest["statements"].append({
            "suffix": acct["suffix"],
            "label": acct["label"],
            "documents": stmts,
        })
    (run_dir / "run.json").write_text(json.dumps(manifest))
    return run_dir


# ============================================================
# Snapshot-at + date parsing
# ============================================================

class TestParseSnapshotAt:
    @pytest.mark.parametrize("name,expected_iso", [
        ("20260101T000000Z", "2026-01-01T00:00:00+00:00"),
        ("20991231T235959Z", "2099-12-31T23:59:59+00:00"),
    ])
    def test_round_trip(self, name, expected_iso):
        from datetime import datetime
        got = load.parse_snapshot_at(name)
        want = int(datetime.fromisoformat(expected_iso).timestamp())
        assert got == want

    @pytest.mark.parametrize("bad", [
        "20260101T000000", "manual", "", "2026-01-01T00:00:00Z"
    ])
    def test_bad_input_raises(self, bad):
        with pytest.raises(ValueError):
            load.parse_snapshot_at(bad)


class TestParseDocDate:
    @pytest.mark.parametrize("input_,expected_iso", [
        ("02/28/2026", "2026-02-28T00:00:00+00:00"),
        ("1/5/2024",   "2024-01-05T00:00:00+00:00"),
    ])
    def test_parse(self, input_, expected_iso):
        from datetime import datetime
        got = load.parse_doc_date(input_)
        want = int(datetime.fromisoformat(expected_iso).timestamp())
        assert got == want

    @pytest.mark.parametrize("bad", ["", "not a date", "2026-02-28"])
    def test_unparseable_returns_none(self, bad):
        assert load.parse_doc_date(bad) is None


# ============================================================
# Schema migration
# ============================================================

class TestMigrations:
    def test_fresh_db_starts_at_version_zero(self, conn):
        assert load._current_schema_version(conn) == 0

    def test_apply_migrations_reaches_v1(self, conn):
        load.apply_migrations(conn, MIGRATIONS_DIR)
        assert load._current_schema_version(conn) == 1

    def test_apply_is_idempotent(self, conn):
        load.apply_migrations(conn, MIGRATIONS_DIR)
        load.apply_migrations(conn, MIGRATIONS_DIR)
        count = conn.execute("SELECT COUNT(*) FROM schema_meta").fetchone()[0]
        assert count == 1

    def test_expected_tables_created(self, migrated):
        rows = migrated.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
        ).fetchall()
        names = {r[0] for r in rows}
        assert {"schema_meta", "dump_runs", "accounts",
                "documents", "transactions"} <= names


# ============================================================
# discover_bronze_runs
# ============================================================

class TestDiscoverBronzeRuns:
    def test_filters_non_ts_dirs(self, tmp_path):
        (tmp_path / "20260520T120000Z").mkdir()
        (tmp_path / "20260521T130000Z").mkdir()
        (tmp_path / "manual").mkdir()
        (tmp_path / "junk.txt").write_text("hello")
        runs = load.discover_bronze_runs(tmp_path)
        assert [p.name for p in runs] == [
            "20260520T120000Z", "20260521T130000Z",
        ]


# ============================================================
# Account upsert + content dedup
# ============================================================

class TestAccountUpsert:
    def test_first_upsert_inserts(self, migrated):
        snapshot_at = load.parse_snapshot_at("20260101T000000Z")
        inserted = load._upsert_account(
            migrated, snapshot_at, "000",
            {"suffix": "000", "label": "Synthetic", "nickname": "Synthetic"},
        )
        assert inserted is True
        row = migrated.execute(
            "SELECT account_external_id, nickname FROM accounts "
            "WHERE account_external_id = '000'"
        ).fetchone()
        assert row == ("000", "Synthetic")

    def test_same_payload_deduped(self, migrated):
        sa1 = load.parse_snapshot_at("20260101T000000Z")
        sa2 = load.parse_snapshot_at("20260102T000000Z")
        p = {"suffix": "000", "label": "Synthetic", "nickname": "Synthetic"}
        assert load._upsert_account(migrated, sa1, "000", p) is True
        assert load._upsert_account(migrated, sa2, "000", p) is False
        rows = migrated.execute(
            "SELECT snapshot_at FROM accounts WHERE account_external_id = '000'"
        ).fetchall()
        assert len(rows) == 1
        assert rows[0][0] == sa1

    def test_changed_payload_inserts_new_row(self, migrated):
        sa1 = load.parse_snapshot_at("20260101T000000Z")
        sa2 = load.parse_snapshot_at("20260102T000000Z")
        p1 = {"suffix": "000", "label": "Old", "nickname": "Old"}
        p2 = {"suffix": "000", "label": "New", "nickname": "New"}
        load._upsert_account(migrated, sa1, "000", p1)
        load._upsert_account(migrated, sa2, "000", p2)
        rows = migrated.execute(
            "SELECT snapshot_at, nickname FROM accounts "
            "WHERE account_external_id = '000' ORDER BY snapshot_at"
        ).fetchall()
        assert rows == [(sa1, "Old"), (sa2, "New")]


class TestAccountNickname:
    @pytest.mark.parametrize("label,suffix,expected", [
        # Dropdown-style label with U+2026 suffix marker. The
        # download.py inventory rendering doubles the nickname
        # (visible name + sr-only echo); we collapse that.
        ("Demo Account Demo Account …000 Account ending in 0 0 0",
         "000", "Demo Account"),
        # No ellipsis: pass-through.
        ("Just A Label", "000", "Just A Label"),
        # Empty label.
        ("", "000", None),
        (None, "000", None),
    ])
    def test_nickname_extraction(self, label, suffix, expected):
        assert load._account_nickname(label, suffix) == expected


# ============================================================
# Dump-run idempotency
# ============================================================

class TestDumpRunIdempotency:
    def test_already_loaded_initially_false(self, migrated):
        sa = load.parse_snapshot_at("20260520T120000Z")
        assert load.already_loaded(migrated, sa) is False

    def test_already_loaded_after_insert(self, migrated, tmp_path):
        sa = load.parse_snapshot_at("20260520T120000Z")
        load._insert_dump_run(migrated, sa, tmp_path)
        assert load.already_loaded(migrated, sa) is True
        other = load.parse_snapshot_at("20260521T130000Z")
        assert load.already_loaded(migrated, other) is False


# ============================================================
# Synthetic activity_id stability
# ============================================================

class TestSynthesizeActivityId:
    BASE_TX = {"date": "2026-02-02", "amount": -100.0,
               "description": "Sale", "symbol": "ABC"}

    def test_same_inputs_same_id(self):
        a = load._synthesize_activity_id("000", self.BASE_TX, 0, "sha-x")
        b = load._synthesize_activity_id("000", self.BASE_TX, 0, "sha-x")
        assert a == b
        assert len(a) == 32

    def test_different_index_differs(self):
        a = load._synthesize_activity_id("000", self.BASE_TX, 0, "sha-x")
        b = load._synthesize_activity_id("000", self.BASE_TX, 1, "sha-x")
        assert a != b

    def test_different_source_differs(self):
        a = load._synthesize_activity_id("000", self.BASE_TX, 0, "sha-x")
        b = load._synthesize_activity_id("000", self.BASE_TX, 0, "sha-y")
        assert a != b

    def test_different_account_differs(self):
        a = load._synthesize_activity_id("000", self.BASE_TX, 0, "sha-x")
        b = load._synthesize_activity_id("999", self.BASE_TX, 0, "sha-x")
        assert a != b


# ============================================================
# End-to-end load_run (no real PDF parse — uses stub PDFs that
# pdf_parsers will return zero transactions for)
# ============================================================

class TestLoadRun:
    def test_loads_documents_and_dedup_works(self, migrated, tmp_path):
        run = _make_bronze_run(tmp_path, "20260520T120000Z", [
            {"suffix": "000", "label": "Demo …000",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": "Brokerage-Statement_2026-02-28_000.PDF"},
                 {"date": "03/31/2026", "type": "Tax Forms",
                  "document": "1099 Composite",
                  "filename": "1099-Composite_2026-03-31_000.PDF"},
             ]},
        ])
        stats = load.load_run(migrated, run)
        migrated.commit()
        assert stats["documents_new"] == 2
        assert stats["documents_dup"] == 0

        # Same content under a different run → all dedup'd.
        run2 = _make_bronze_run(tmp_path, "20260521T130000Z", [
            {"suffix": "000", "label": "Demo …000",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": "Brokerage-Statement_2026-02-28_000.PDF"},
             ]},
        ])
        stats2 = load.load_run(migrated, run2)
        migrated.commit()
        assert stats2["documents_new"] == 0
        assert stats2["documents_dup"] == 1

    def test_doc_kind_mapping(self, migrated, tmp_path):
        run = _make_bronze_run(tmp_path, "20260520T120000Z", [
            {"suffix": "000", "label": "Demo …000",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "X", "filename": "s.pdf"},
                 {"date": "02/28/2026", "type": "Tax Forms",
                  "document": "X", "filename": "t.pdf"},
                 {"date": "02/28/2026", "type": "Letters",
                  "document": "X", "filename": "l.pdf"},
                 {"date": "02/28/2026", "type": "Reports & Plans",
                  "document": "X", "filename": "r.pdf"},
             ]},
        ])
        load.load_run(migrated, run)
        kinds = {r[0] for r in migrated.execute(
            "SELECT DISTINCT doc_kind FROM documents"
        ).fetchall()}
        assert kinds == {"statement", "tax_form", "letter", "report_or_plan"}

    def test_dump_run_recorded(self, migrated, tmp_path):
        run = _make_bronze_run(tmp_path, "20260520T120000Z", [])
        load.load_run(migrated, run)
        migrated.commit()
        sa = load.parse_snapshot_at("20260520T120000Z")
        assert load.already_loaded(migrated, sa) is True


# ============================================================
# Money parser
# ============================================================

class TestParseMoney:
    @pytest.mark.parametrize("s,expected", [
        ("$1,234.56",   1234.56),
        ("-$5.00",      -5.00),
        ("($1.00)",     -1.00),
        ("$0.00",       0.00),
        ("",            None),
        (None,          None),
        ("not a number", None),
    ])
    def test_parses(self, s, expected):
        assert load._parse_money(s) == expected


# ============================================================
# Tx-history JSON ingest
# ============================================================

def _make_tx_history_bronze(root: Path, run_ts: str, suffix: str,
                            transactions: list[dict],
                            more_details: list[dict] | None = None,
                            ) -> Path:
    """Build a bronze run with a tx-history JSON export (and
    optional more-details.json sidecar) for `suffix`.
    """
    import json as _json
    run_dir = root / run_ts
    run_dir.mkdir(parents=True, exist_ok=True)
    tx_dir = run_dir / "transactions" / suffix
    tx_dir.mkdir(parents=True, exist_ok=True)
    # JSON export
    json_path = tx_dir / f"Demo_XXX{suffix}_Transactions.json"
    payload = {
        "FromDate": "01/01/2024",
        "ToDate": "12/31/2024",
        "TotalTransactionsAmount": "$0.00",
        "TotalFeesAndCommAmount": "$0.00",
        "BrokerageTransactions": transactions,
    }
    json_path.write_text(_json.dumps(payload), encoding="utf-8")
    sha, _size = load.sha256_file(json_path)
    if more_details is not None:
        (tx_dir / "more-details.json").write_text(
            _json.dumps(more_details), encoding="utf-8",
        )
    manifest = {
        "run_ts": run_ts,
        "mode": "transactions",
        "dry_run": False,
        "statements": [],
        "transactions": [{
            "suffix": suffix,
            "label": f"Demo …{suffix}",
            "exports": [{
                "format": "json",
                "filename": json_path.name,
                "size": json_path.stat().st_size,
                "sha256": sha,
            }],
        }],
    }
    (run_dir / "run.json").write_text(_json.dumps(manifest))
    return run_dir


class TestTxHistoryIngest:
    def test_loads_json_into_transactions(self, migrated, tmp_path):
        txs = [
            {"Date": "05/12/2024", "Action": "Buy", "Symbol": "ABC",
             "Description": "ACME CORP", "Quantity": "10",
             "Price": "$10.00", "Fees & Comm": "$0.01",
             "Amount": "-$100.01", "AcctgRuleCd": "2"},
            {"Date": "05/13/2024", "Action": "Cash Dividend",
             "Symbol": "ABC", "Description": "ACME CORP DIV",
             "Quantity": "", "Price": "", "Fees & Comm": "",
             "Amount": "$0.50", "AcctgRuleCd": "10"},
        ]
        run = _make_tx_history_bronze(tmp_path, "20260520T120000Z",
                                      "000", txs)
        stats = load.load_run(migrated, run)
        migrated.commit()
        assert stats["transactions_inserted"] == 2
        rows = migrated.execute(
            "SELECT kind, instrument_key, source FROM transactions "
            "ORDER BY timestamp"
        ).fetchall()
        assert rows == [
            ("Buy", "ABC", "tx_history_json"),
            ("Cash Dividend", "ABC", "tx_history_json"),
        ]

    def test_more_detail_sidecar_merged_into_payload(
            self, migrated, tmp_path):
        import json as _json
        # One transaction whose row_key matches a sidecar record.
        tx = {"Date": "05/12/2024", "Action": "Buy", "Symbol": "ABC",
              "Description": "ACME CORP", "Quantity": "10",
              "Price": "$10.00", "Fees & Comm": "$0.01",
              "Amount": "-$100.01"}
        row_key = load._tx_history_row_key(tx)
        sidecar = [{
            "row_key": row_key,
            "row_cells": ["05/12/2024", "-$100.01", "ACME CORP",
                          "ABC", "Buy"],
            "fields": {
                "Settle Date": "05/14/2024",
                "CUSIP #": "000000000",
                "Principal": "$100.00",
                "Commission": "$0.00",
                "Industry Fee": "$0.01",
            },
            "raw_text": "Trade Date  05/12/2024\nSettle Date  05/14/2024",
        }]
        run = _make_tx_history_bronze(
            tmp_path, "20260520T120000Z", "000",
            transactions=[tx], more_details=sidecar,
        )
        load.load_run(migrated, run)
        migrated.commit()
        row = migrated.execute(
            "SELECT payload FROM transactions"
        ).fetchone()
        payload = _json.loads(row[0])
        assert payload.get("_more", {}).get("Settle Date") == "05/14/2024"
        assert payload.get("_more", {}).get("Principal") == "$100.00"


class TestTxHistoryRowKey:
    def test_stable_for_identical_inputs(self):
        tx = {"Date": "01/02/2024", "Amount": "$1.00",
              "Description": "X", "Symbol": "ABC", "Action": "Buy"}
        assert load._tx_history_row_key(tx) == load._tx_history_row_key(tx)

    def test_differs_for_different_inputs(self):
        a = {"Date": "01/02/2024", "Amount": "$1.00",
             "Description": "X", "Symbol": "ABC", "Action": "Buy"}
        b = dict(a, Amount="$2.00")
        assert load._tx_history_row_key(a) != load._tx_history_row_key(b)
