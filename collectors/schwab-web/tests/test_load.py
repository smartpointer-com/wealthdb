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
from collectorkit import bronze  # noqa: E402

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
    sha, _ = bronze.sha256_file(path)
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

    def test_apply_migrations_reaches_head(self, conn):
        load.apply_migrations(conn, MIGRATIONS_DIR)
        # Head version = max migration file present; bumps when
        # a new migration lands.
        assert load._current_schema_version(conn) >= 2

    def test_apply_is_idempotent(self, conn):
        load.apply_migrations(conn, MIGRATIONS_DIR)
        load.apply_migrations(conn, MIGRATIONS_DIR)
        v = load._current_schema_version(conn)
        count = conn.execute(
            "SELECT COUNT(*) FROM schema_meta WHERE silver_schema_version = ?",
            (v,),
        ).fetchone()[0]
        # Each migration writes its row once; re-applying must
        # NOT insert duplicates.
        assert count == 1

    def test_expected_tables_created(self, migrated):
        rows = migrated.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
        ).fetchall()
        names = {r[0] for r in rows}
        assert {"schema_meta", "dump_runs", "accounts",
                "documents", "transactions",
                "historical_position_snapshots",
                "historical_cash_balances"} <= names

    def test_0004_backfills_logical_doc_key_and_collapses_churn(self, conn, tmp_path):
        # Apply only 0001..0003 so we can seed pre-0004 sha256-churn rows.
        early = tmp_path / "m"
        early.mkdir()
        for name in ("0001_initial.sql", "0002_historical_snapshots.sql",
                     "0003_account_registration.sql"):
            (early / name).write_text((MIGRATIONS_DIR / name).read_text())
        load.apply_migrations(conn, early)

        # Two PDFs = one logical statement re-downloaded (sha256 churn): same
        # account / doc_date / filename, different sha256. 'aaa' and 'bbb' are the
        # same logical transaction parsed from each copy (identical content,
        # distinct activity_id); 'ccc' is a genuinely-distinct transaction.
        conn.executescript(
            """
            INSERT INTO documents(sha256, snapshot_at, account_external_id, doc_date,
                                  doc_kind, file_format, filename, size_bytes, payload)
            VALUES
              ('shaA', 1000, 'NNN', 1709251200, 'brokerage', 'PDF', 'Stmt_2024-03.PDF', 10, '{}'),
              ('shaB', 1000, 'NNN', 1709251200, 'brokerage', 'PDF', 'Stmt_2024-03.PDF', 11, '{}');
            INSERT INTO transactions(activity_id, timestamp, account_external_id, kind,
                                     instrument_key, source, source_sha256, payload)
            VALUES
              ('aaa', 1709251200, 'NNN', 'Withdrawal', NULL, 'statement_pdf', 'shaA', '{"amount":-1000}'),
              ('bbb', 1709251200, 'NNN', 'Withdrawal', NULL, 'statement_pdf', 'shaB', '{"amount":-1000}'),
              ('ccc', 1709251200, 'NNN', 'Deposit',    NULL, 'statement_pdf', 'shaA', '{"amount":250}');
            """
        )
        assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 3

        # Apply the pending migration (0004 only).
        load.apply_migrations(conn, MIGRATIONS_DIR)

        rows = dict(conn.execute(
            "SELECT activity_id, logical_doc_key FROM transactions"
        ).fetchall())
        # Churn copies collapse to MIN(activity_id) = 'aaa'; the distinct row survives.
        assert set(rows) == {"aaa", "ccc"}, rows
        # logical_doc_key backfilled from the documents join (sha256-independent).
        assert rows["aaa"] == "NNN|1709251200|Stmt_2024-03.PDF"
        assert rows["ccc"] == "NNN|1709251200|Stmt_2024-03.PDF"
        assert load._current_schema_version(conn) == 4


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
        a = load._synthesize_activity_id("000", self.BASE_TX, 0)
        b = load._synthesize_activity_id("000", self.BASE_TX, 0)
        assert a == b
        assert len(a) == 32

    def test_different_index_differs(self):
        a = load._synthesize_activity_id("000", self.BASE_TX, 0)
        b = load._synthesize_activity_id("000", self.BASE_TX, 1)
        assert a != b

    def test_is_deterministic(self):
        """activity_id is a pure function of its inputs: the same
        logical row yields the same id on every download, so a
        re-fetched PDF cannot duplicate transactions. Guards against a
        clock/counter/uuid sneaking into the id."""
        a = load._synthesize_activity_id("000", self.BASE_TX, 0)
        b = load._synthesize_activity_id("000", self.BASE_TX, 0)
        assert a == b

    def test_different_account_differs(self):
        a = load._synthesize_activity_id("000", self.BASE_TX, 0)
        b = load._synthesize_activity_id("999", self.BASE_TX, 0)
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


def _set_manifest_status(run_dir: Path, status: str | None) -> None:
    """Rewrite run.json's `status` field (drop it when None) to
    exercise the load-time non-complete guard."""
    manifest = json.loads((run_dir / "run.json").read_text())
    if status is None:
        manifest.pop("status", None)
    else:
        manifest["status"] = status
    (run_dir / "run.json").write_text(json.dumps(manifest))


class TestStatusGuard:
    """download.walk() writes run.json incrementally with
    status="in-progress", flipping to "complete"/"dry-run" only at
    the end. load must skip non-complete dumps so a crashed walk or a
    --dry-run shell never leaks partial rows into silver."""

    def _docs_run(self, tmp_path, ts):
        return _make_bronze_run(tmp_path, ts, [
            {"suffix": "000", "label": "Demo …000",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": "Brokerage-Statement_2026-02-28_000.PDF"},
             ]},
        ])

    def test_in_progress_dump_skipped(self, migrated, tmp_path):
        run = self._docs_run(tmp_path, "20260520T120000Z")
        _set_manifest_status(run, "in-progress")
        stats = load.load_run(migrated, run)
        migrated.commit()
        assert stats["documents_new"] == 0
        sa = load.parse_snapshot_at("20260520T120000Z")
        assert load.already_loaded(migrated, sa) is False

    def test_dry_run_status_dump_skipped(self, migrated, tmp_path):
        run = self._docs_run(tmp_path, "20260520T120000Z")
        _set_manifest_status(run, "dry-run")
        stats = load.load_run(migrated, run)
        migrated.commit()
        assert stats["documents_new"] == 0
        sa = load.parse_snapshot_at("20260520T120000Z")
        assert load.already_loaded(migrated, sa) is False

    def test_complete_status_dump_loads(self, migrated, tmp_path):
        run = self._docs_run(tmp_path, "20260520T120000Z")
        _set_manifest_status(run, "complete")
        stats = load.load_run(migrated, run)
        migrated.commit()
        assert stats["documents_new"] == 1
        sa = load.parse_snapshot_at("20260520T120000Z")
        assert load.already_loaded(migrated, sa) is True

    def test_statusless_dump_still_loads(self, migrated, tmp_path):
        # Backward compat: pre-`status` manifests carry no status key
        # and must remain loadable.
        run = self._docs_run(tmp_path, "20260520T120000Z")
        _set_manifest_status(run, None)
        stats = load.load_run(migrated, run)
        migrated.commit()
        assert stats["documents_new"] == 1


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
    sha, _size = bronze.sha256_file(json_path)
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


class TestPositionsAndCashLoad:
    """End-to-end loader tests for migration 0002 (positions +
    cash). Monkeypatches pp.parse_statement_pdf to return a known
    result, so we exercise the dispatcher + insert helpers + DB
    schema without depending on pdfplumber reading a real PDF.
    """

    PARSED_STATEMENT = {
        "path": "<patched>",
        "period_start": "2026-02-01",
        "period_end": "2026-02-28",
        "transactions": [
            {"date": "2026-02-05", "category": "Deposit",
             "action": "FundsReceived", "symbol": None,
             "description": "WIRE", "quantity": None, "price": None,
             "charges": None, "amount": 1000.0,
             "realized_gain_loss": None, "term": None,
             "raw_lines": ["02/05 Deposit FundsReceived WIRE 1,000.00"]},
        ],
        "positions": [
            {"instrument_key": "SYN1", "description": "Synthetic One",
             "quantity": 100.0, "market_price": 50.0,
             "market_value": 5000.0, "cost_basis": 4000.0,
             "unrealized_gain_loss": 1000.0,
             "accrued_interest": None, "est_yield": "N/A",
             "est_annual_income": None, "pct_of_acct": "1%",
             "section": "Equities",
             "raw_lines": ["SYN1 Synthetic One 100 50 5000 4000 1000 N/A N/A 1%"]},
            {"instrument_key": "SYN2", "description": "Synthetic Two",
             "quantity": 200.0, "market_price": 10.0,
             "market_value": 2000.0,
             # Missing cost basis — should land as NULL, NOT 0.0
             "cost_basis": None, "unrealized_gain_loss": None,
             "accrued_interest": None, "est_yield": None,
             "est_annual_income": None, "pct_of_acct": "<1%",
             "section": "Exchange Traded Funds",
             "raw_lines": ["SYN2 Synthetic Two 200 10 2000"]},
        ],
        "cash_summary": {
            "opening_balance": 100.0, "closing_balance": 250.0,
            "deposits": 1000.0, "withdrawals": -800.0,
            "purchases": -50.0, "sales_redemptions": 0.0,
            "dividends_interest": 0.0, "expenses": 0.0,
            "other_activity": 0.0,
            "total_credits": 1000.0, "total_debits": 850.0,
            "currency_iso": "USD",
            "raw_line": "$100.00 $1,000.00 ($800.00) ($50.00) $0.00 $0.00 $0.00 $250.00",
        },
        "account_registration": "Schwab One International® Account",
    }

    def _run(self, monkeypatch, migrated, tmp_path, *, run_ts="20260520T120000Z",
             filename="Brokerage-Statement_2026-02-28_000.PDF"):
        monkeypatch.setattr(
            load.pp, "parse_statement_pdf",
            lambda path, statement_year=None: dict(self.PARSED_STATEMENT),
        )
        run = _make_bronze_run(tmp_path, run_ts, [
            {"suffix": "NNN", "label": "Demo …NNN",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": filename},
             ]},
        ])
        # workers=1 keeps the parse on the main process so the
        # monkeypatch above is honoured. ProcessPoolExecutor
        # workers run in subprocesses that don't inherit the
        # monkeypatch.
        return load.load_run(migrated, run, workers=1)

    def test_positions_inserted_with_natural_pk(self, monkeypatch, migrated, tmp_path):
        stats = self._run(monkeypatch, migrated, tmp_path)
        migrated.commit()
        assert stats["positions_inserted"] == 2
        rows = migrated.execute(
            "SELECT instrument_key, quantity, market_value, cost_basis"
            " FROM historical_position_snapshots ORDER BY instrument_key"
        ).fetchall()
        assert rows == [
            ("SYN1", 100.0, 5000.0, 4000.0),
            ("SYN2", 200.0, 2000.0, None),  # cost_basis stayed NULL
        ]

    def test_cash_balance_inserted(self, monkeypatch, migrated, tmp_path):
        stats = self._run(monkeypatch, migrated, tmp_path)
        migrated.commit()
        assert stats["cash_balances_inserted"] == 1
        row = migrated.execute(
            "SELECT opening_balance, closing_balance, total_credits, total_debits,"
            " currency_iso FROM historical_cash_balances"
        ).fetchone()
        assert row == (100.0, 250.0, 1000.0, 850.0, "USD")

    def test_null_preservation_in_payload(self, monkeypatch, migrated, tmp_path):
        # Missing cost_basis arrives as None, not 0.0 — both at the
        # column level and (verify) by asserting the inserter didn't
        # coerce.
        self._run(monkeypatch, migrated, tmp_path)
        migrated.commit()
        row = migrated.execute(
            "SELECT cost_basis, unrealized_gain_loss FROM"
            " historical_position_snapshots WHERE instrument_key = 'SYN2'"
        ).fetchone()
        assert row == (None, None)

    def test_idempotent_reload_replaces_not_duplicates(
            self, monkeypatch, migrated, tmp_path):
        """Loading the same bronze run twice produces identical row
        counts (INSERT OR REPLACE keeps a single row per natural
        PK)."""
        self._run(monkeypatch, migrated, tmp_path)
        migrated.commit()
        n1 = migrated.execute(
            "SELECT COUNT(*) FROM historical_position_snapshots"
        ).fetchone()[0]
        # Second load with same run_ts triggers already_loaded path
        # — use a fresh run_ts so load_run actually re-iterates.
        self._run(monkeypatch, migrated, tmp_path,
                  run_ts="20260521T130000Z")
        migrated.commit()
        n2 = migrated.execute(
            "SELECT COUNT(*) FROM historical_position_snapshots"
        ).fetchone()[0]
        assert n1 == n2 == 2

    def test_sha256_churn_dedupes_logical_doc(
            self, monkeypatch, migrated, tmp_path):
        """Two PDFs with identical (account, doc_date, doc_kind,
        filename) but different sha256s (re-downloaded copy) must
        count as one logical statement: positions parsed once,
        the second occurrence increments statements_logical_deduped.
        """
        monkeypatch.setattr(
            load.pp, "parse_statement_pdf",
            lambda path, statement_year=None: dict(self.PARSED_STATEMENT),
        )
        # Build a bronze run where the same logical doc appears
        # under TWO different bytes-on-disk (Schwab regen). We
        # do this by emitting two doc entries pointing at the
        # same filename — _make_bronze_run rewrites the file
        # content per filename, but the second write doesn't
        # change the filename. To force two distinct sha256s,
        # we write them to separate run dirs.
        run1 = _make_bronze_run(tmp_path, "20260520T120000Z", [
            {"suffix": "NNN", "label": "L",
             "documents": [{"date": "02/28/2026", "type": "Statements",
                            "document": "Brokerage Statement",
                            "filename": "Brokerage-Statement_2026-02-28_NNN.PDF"}]},
        ])
        # Tweak the PDF bytes so its sha256 differs.
        p = run1 / "statements" / "NNN" / "Brokerage-Statement_2026-02-28_NNN.PDF"
        original_sha = bronze.sha256_file(p)[0]
        p.write_bytes(p.read_bytes() + b"%CHURNED-BYTES%")
        new_sha = bronze.sha256_file(p)[0]
        assert original_sha != new_sha
        # Update manifest to record the new sha256.
        manifest = json.loads((run1 / "run.json").read_text())
        manifest["statements"][0]["documents"][0]["sha256"] = new_sha
        (run1 / "run.json").write_text(json.dumps(manifest))

        stats1 = load.load_run(migrated, run1, workers=1)
        migrated.commit()
        # Same logical doc, different sha256, second run — the
        # dispatcher should detect existing positions (and now
        # transactions, since the gate is logical-doc-key-based)
        # and skip the parse entirely.
        run2 = _make_bronze_run(tmp_path, "20260521T130000Z", [
            {"suffix": "NNN", "label": "L",
             "documents": [{"date": "02/28/2026", "type": "Statements",
                            "document": "Brokerage Statement",
                            "filename": "Brokerage-Statement_2026-02-28_NNN.PDF"}]},
        ])
        load.load_run(migrated, run2, workers=1)
        migrated.commit()
        # Positions inserted once across both runs.
        n_pos = migrated.execute(
            "SELECT COUNT(*) FROM historical_position_snapshots"
        ).fetchone()[0]
        assert n_pos == 2  # the two SYN rows
        # First-run stats: 2 positions inserted; second run sees
        # already-populated table and inserts 0 (logical-dedup
        # gate catches it).
        assert stats1["positions_inserted"] == 2


class TestAccountRegistrationLoad:
    """Loader tests for migration 0003 — the
    `accounts.account_registration` column populated from the
    parsed statement header, with a tax-form filename fallback
    for accounts whose statements didn't surface a label."""

    PARSED_WITH_REG = {
        "path": "<patched>",
        "period_start": "2026-02-01",
        "period_end": "2026-02-28",
        "transactions": [],
        "positions": [],
        "cash_summary": None,
        "account_registration": "Contributory IRA",
    }

    PARSED_NO_REG = dict(PARSED_WITH_REG, account_registration=None)

    def test_registration_populated_from_statement(
            self, monkeypatch, migrated, tmp_path):
        monkeypatch.setattr(
            load.pp, "parse_statement_pdf",
            lambda path, statement_year=None: dict(self.PARSED_WITH_REG),
        )
        run = _make_bronze_run(tmp_path, "20260520T120000Z", [
            {"suffix": "NNN", "label": "Demo …NNN",
             "documents": [{"date": "02/28/2026", "type": "Statements",
                            "document": "Brokerage Statement",
                            "filename": "Brokerage-Statement_2026-02-28_NNN.PDF"}]},
        ])
        stats = load.load_run(migrated, run, workers=1)
        migrated.commit()
        assert stats["account_registration_updated"] == 1
        row = migrated.execute(
            "SELECT account_registration FROM accounts "
            "WHERE account_external_id = 'NNN'"
        ).fetchone()
        assert row == ("Contributory IRA",)

    def test_tax_form_fallback_5498_esa(
            self, monkeypatch, migrated, tmp_path):
        # Statement parser returns None — fallback should pick
        # up "Education Savings" from a 5498-ESA filename.
        monkeypatch.setattr(
            load.pp, "parse_statement_pdf",
            lambda path, statement_year=None: dict(self.PARSED_NO_REG),
        )
        run = _make_bronze_run(tmp_path, "20260520T120000Z", [
            {"suffix": "MMM", "label": "Demo …MMM",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": "Brokerage-Statement_2026-02-28_MMM.PDF"},
                 {"date": "05/01/2001", "type": "Tax Forms",
                  "document": "5498-ESA",
                  "filename": "5498-ESA---2000_2001-05-01_MMM.PDF"},
             ]},
        ])
        load.load_run(migrated, run, workers=1)
        migrated.commit()
        row = migrated.execute(
            "SELECT account_registration FROM accounts "
            "WHERE account_external_id = 'MMM'"
        ).fetchone()
        assert row == ("Education Savings",)

    def test_tax_form_fallback_5498_ira(
            self, monkeypatch, migrated, tmp_path):
        # Same as above but the tax form is a plain 5498 (IRA,
        # not ESA) — fallback maps to "Contributory IRA".
        monkeypatch.setattr(
            load.pp, "parse_statement_pdf",
            lambda path, statement_year=None: dict(self.PARSED_NO_REG),
        )
        run = _make_bronze_run(tmp_path, "20260520T120000Z", [
            {"suffix": "MMM", "label": "Demo …MMM",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": "Brokerage-Statement_2026-02-28_MMM.PDF"},
                 {"date": "05/01/2001", "type": "Tax Forms",
                  "document": "5498",
                  "filename": "5498---2000_2001-05-01_MMM.PDF"},
             ]},
        ])
        load.load_run(migrated, run, workers=1)
        migrated.commit()
        row = migrated.execute(
            "SELECT account_registration FROM accounts "
            "WHERE account_external_id = 'MMM'"
        ).fetchone()
        assert row == ("Contributory IRA",)

    def test_no_signal_leaves_column_null(
            self, monkeypatch, migrated, tmp_path):
        # Neither statement nor a 5498-style tax form — column
        # stays NULL and the gold adapter falls back to its
        # default at render time.
        monkeypatch.setattr(
            load.pp, "parse_statement_pdf",
            lambda path, statement_year=None: dict(self.PARSED_NO_REG),
        )
        run = _make_bronze_run(tmp_path, "20260520T120000Z", [
            {"suffix": "MMM", "label": "Demo …MMM",
             "documents": [
                 {"date": "02/28/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": "Brokerage-Statement_2026-02-28_MMM.PDF"},
                 {"date": "03/01/2001", "type": "Tax Forms",
                  "document": "1042-S",
                  "filename": "1042S---2000_2001-03-01_MMM.PDF"},
             ]},
        ])
        load.load_run(migrated, run, workers=1)
        migrated.commit()
        row = migrated.execute(
            "SELECT account_registration FROM accounts "
            "WHERE account_external_id = 'MMM'"
        ).fetchone()
        assert row == (None,)


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


# ============================================================
# SHA-256-churn idempotency for transactions (the duplication bug)
# ============================================================

class TestSha256ChurnTransactionIdempotency:
    """Core regression tests for the sha256-churn duplication bug.

    Schwab regenerates PDFs on every download (INTEROP.md §3). Before
    migration 0004, each re-download produced a fresh set of activity_ids
    (sha256 was baked into the hash) and the per-sha256 insert gate let
    them all in, causing multiplicative duplication.

    Invariants verified here:
      1. Loading the same logical statement under N distinct sha256s
         produces exactly ONE set of transactions.
      2. Genuinely-distinct same-day same-amount transactions (different
         ordinal position within the statement) are preserved as
         separate rows.
      3. The logical_doc_key column is populated on inserted rows.
    """

    # Minimal parsed statement with two transactions on the same day.
    PARSED_TWO_TX = {
        "path": "<patched>",
        "period_start": "2026-01-01",
        "period_end": "2026-01-31",
        "transactions": [
            {"date": "2026-01-15", "category": "CashDividend",
             "action": "DividendReinvestment", "symbol": "SYN",
             "description": "SYNTH CORP DIV", "quantity": None,
             "price": None, "charges": None, "amount": 42.00,
             "realized_gain_loss": None, "term": None,
             "raw_lines": ["01/15 CashDividend SYNTH CORP DIV 42.00"]},
            {"date": "2026-01-15", "category": "CashDividend",
             "action": "DividendReinvestment", "symbol": "SYN",
             "description": "SYNTH CORP DIV", "quantity": None,
             "price": None, "charges": None, "amount": 42.00,
             "realized_gain_loss": None, "term": None,
             "raw_lines": ["01/15 CashDividend SYNTH CORP DIV 42.00"]},
        ],
        "positions": [],
        "cash_summary": None,
        "account_registration": None,
    }

    def _run_with_patched_parser(self, monkeypatch, migrated, tmp_path,
                                 run_ts: str, filename: str,
                                 pdf_bytes_extra: bytes = b"") -> dict:
        """Build a bronze run with a (possibly byte-tweaked) PDF and
        load it. Returns stats. `pdf_bytes_extra` forces a different
        sha256 while the logical document (account + date + filename)
        stays the same."""
        monkeypatch.setattr(
            load.pp, "parse_statement_pdf",
            lambda path, statement_year=None: dict(self.PARSED_TWO_TX),
        )
        run = _make_bronze_run(tmp_path, run_ts, [
            {"suffix": "NNN", "label": "Synthetic …NNN",
             "documents": [
                 {"date": "01/31/2026", "type": "Statements",
                  "document": "Brokerage Statement",
                  "filename": filename},
             ]},
        ])
        if pdf_bytes_extra:
            p = run / "statements" / "NNN" / filename
            p.write_bytes(p.read_bytes() + pdf_bytes_extra)
            new_sha = bronze.sha256_file(p)[0]
            manifest = json.loads((run / "run.json").read_text())
            manifest["statements"][0]["documents"][0]["sha256"] = new_sha
            (run / "run.json").write_text(json.dumps(manifest))
        return load.load_run(migrated, run, workers=1)

    def test_two_downloads_yield_one_set_of_transactions(
            self, monkeypatch, migrated, tmp_path):
        """Loading the same statement under two distinct sha256s must
        produce exactly the same two transaction rows — not four."""
        filename = "Brokerage-Statement_2026-01-31_NNN.PDF"
        # First download (original sha256).
        self._run_with_patched_parser(
            monkeypatch, migrated, tmp_path,
            "20260201T080000Z", filename,
        )
        migrated.commit()

        # Second download — tweak bytes to produce a different sha256,
        # but same logical document (same account + date + filename).
        self._run_with_patched_parser(
            monkeypatch, migrated, tmp_path,
            "20260202T090000Z", filename,
            pdf_bytes_extra=b"%SCHWAB-REGEN-STAMP-2%",
        )
        migrated.commit()

        rows = migrated.execute(
            "SELECT COUNT(*) FROM transactions "
            "WHERE account_external_id = 'NNN'"
        ).fetchone()[0]
        # Exactly 2 rows (one per transaction in the statement),
        # NOT 4 (which would happen if sha256-churn caused duplication).
        assert rows == 2, (
            f"expected 2 transactions after two sha256-churned loads, "
            f"got {rows}"
        )

    def test_four_downloads_yield_one_set_of_transactions(
            self, monkeypatch, migrated, tmp_path):
        """Four downloads (4 distinct sha256s, same logical content)
        must still yield exactly 2 rows — matching the observed worst
        case of 4 sha256s per statement (INTEROP.md §3)."""
        filename = "Brokerage-Statement_2026-01-31_NNN.PDF"
        for i, extra in enumerate([b"", b"V2", b"V3", b"V4"], start=1):
            run_ts = f"2026020{i}T08000{i}Z"
            self._run_with_patched_parser(
                monkeypatch, migrated, tmp_path,
                run_ts, filename,
                pdf_bytes_extra=extra,
            )
            migrated.commit()
        rows = migrated.execute(
            "SELECT COUNT(*) FROM transactions "
            "WHERE account_external_id = 'NNN'"
        ).fetchone()[0]
        assert rows == 2, (
            f"expected 2 transactions after 4 sha256-churned loads, "
            f"got {rows}"
        )

    def test_distinct_same_day_same_amount_transactions_preserved(
            self, monkeypatch, migrated, tmp_path):
        """Two genuinely-distinct transactions with identical
        (date, amount, description, symbol) must survive as SEPARATE
        rows — the ordinal index disambiguates them."""
        filename = "Brokerage-Statement_2026-01-31_NNN.PDF"
        self._run_with_patched_parser(
            monkeypatch, migrated, tmp_path,
            "20260201T080000Z", filename,
        )
        migrated.commit()
        rows = migrated.execute(
            "SELECT activity_id FROM transactions "
            "WHERE account_external_id = 'NNN' "
            "ORDER BY activity_id"
        ).fetchall()
        # Both rows exist and have distinct activity_ids.
        assert len(rows) == 2
        assert rows[0][0] != rows[1][0]

    def test_logical_doc_key_column_populated(
            self, monkeypatch, migrated, tmp_path):
        """Every inserted transaction must carry a non-NULL
        logical_doc_key so the load gate and --reparse delete work."""
        filename = "Brokerage-Statement_2026-01-31_NNN.PDF"
        self._run_with_patched_parser(
            monkeypatch, migrated, tmp_path,
            "20260201T080000Z", filename,
        )
        migrated.commit()
        null_count = migrated.execute(
            "SELECT COUNT(*) FROM transactions "
            "WHERE logical_doc_key IS NULL"
        ).fetchone()[0]
        assert null_count == 0

    def test_idempotent_same_run_ts_is_noop(
            self, monkeypatch, migrated, tmp_path):
        """Re-running load_run with the exact same run dir (same
        run_ts, same sha256) must not duplicate transactions. The
        dump_run PK prevents re-entry at the run level; this test
        covers the load-gate path."""
        filename = "Brokerage-Statement_2026-01-31_NNN.PDF"
        self._run_with_patched_parser(
            monkeypatch, migrated, tmp_path,
            "20260201T080000Z", filename,
        )
        migrated.commit()
        n_before = migrated.execute(
            "SELECT COUNT(*) FROM transactions"
        ).fetchone()[0]
        # Manually re-invoke the inserter (bypassing the
        # already_loaded dump_run gate) via _insert_statement_transactions
        # directly to confirm INSERT OR IGNORE holds.
        ldk = load._logical_doc_key("NNN", load.parse_doc_date("01/31/2026"),
                                    filename)
        for idx, tx in enumerate(self.PARSED_TWO_TX["transactions"]):
            load._insert_statement_transactions(
                migrated, "NNN", [tx], "any-sha256", ldk,
            )
        migrated.commit()
        n_after = migrated.execute(
            "SELECT COUNT(*) FROM transactions"
        ).fetchone()[0]
        assert n_before == n_after


# ============================================================
# Cross-run parse-dedup (shared seen_logical_docs)
# ============================================================

class TestSeenLogicalDocsSharedAcrossRuns:
    """A statement that yields no transactions never closes its own
    transaction gate (there is no transaction row to detect on the next
    run), so without a shared parse-dedup set the same logical statement
    is re-parsed in every later bronze run. run_load shares the set
    across an invocation's runs via load_run's `seen_logical_docs`
    argument so each logical statement is parsed once. Silver is
    identical either way — the skipped parse would only re-produce rows
    already present.
    """

    # Zero transactions, but positions + cash present, so on a re-download
    # the positions and cash gates close on existing rows and the
    # transaction gate is the ONLY thing that would force a re-parse.
    PARSED_ZERO_TX = {
        "path": "<patched>",
        "period_start": "2026-02-01",
        "period_end": "2026-02-28",
        "transactions": [],
        "positions": [
            {"instrument_key": "SYN1", "description": "Synthetic One",
             "quantity": 100.0, "market_price": 50.0,
             "market_value": 5000.0, "cost_basis": 4000.0,
             "unrealized_gain_loss": 1000.0, "accrued_interest": None,
             "est_yield": None, "est_annual_income": None,
             "pct_of_acct": "1%", "section": "Equities", "raw_lines": []},
        ],
        "cash_summary": {
            "opening_balance": 100.0, "closing_balance": 100.0,
            "deposits": 0.0, "withdrawals": 0.0, "purchases": 0.0,
            "sales_redemptions": 0.0, "dividends_interest": 0.0,
            "expenses": 0.0, "other_activity": 0.0,
            "total_credits": 0.0, "total_debits": 0.0,
            "currency_iso": "USD", "raw_line": "",
        },
        "account_registration": None,
    }

    def _bronze(self, tmp_path, run_ts, extra=b""):
        """One bronze run holding a single logical statement. `extra`
        appended to the PDF bytes forces a fresh sha256 (Schwab regen)
        while the logical document — account + date + filename — is
        unchanged."""
        run = _make_bronze_run(tmp_path, run_ts, [
            {"suffix": "NNN", "label": "L",
             "documents": [{"date": "02/28/2026", "type": "Statements",
                            "document": "Brokerage Statement",
                            "filename": "Brokerage-Statement_2026-02-28_NNN.PDF"}]},
        ])
        if extra:
            p = run / "statements" / "NNN" / \
                "Brokerage-Statement_2026-02-28_NNN.PDF"
            p.write_bytes(p.read_bytes() + extra)
            new_sha = bronze.sha256_file(p)[0]
            m = json.loads((run / "run.json").read_text())
            m["statements"][0]["documents"][0]["sha256"] = new_sha
            (run / "run.json").write_text(json.dumps(m))
        return run

    def _count_calls(self, monkeypatch):
        calls: list[str] = []

        def _counting(path, statement_year=None):
            calls.append(path)
            return dict(self.PARSED_ZERO_TX)

        monkeypatch.setattr(load.pp, "parse_statement_pdf", _counting)
        return calls

    def test_shared_set_parses_zero_tx_statement_once(
            self, monkeypatch, migrated, tmp_path):
        calls = self._count_calls(monkeypatch)
        seen: set[tuple] = set()
        run1 = self._bronze(tmp_path, "20260301T000000Z")
        run2 = self._bronze(tmp_path, "20260302T000000Z",
                            extra=b"%SCHWAB-REGEN-2%")
        load.load_run(migrated, run1, workers=1, seen_logical_docs=seen)
        stats2 = load.load_run(migrated, run2, workers=1,
                               seen_logical_docs=seen)
        migrated.commit()
        # Parsed exactly once across both runs — the shared set caught the
        # zero-transaction re-download in run 2.
        assert len(calls) == 1
        assert stats2["statements_logical_deduped"] == 1
        # Positions written once; still no transactions — silver as if the
        # re-parse had run.
        assert migrated.execute(
            "SELECT COUNT(*) FROM historical_position_snapshots"
        ).fetchone()[0] == 1
        assert migrated.execute(
            "SELECT COUNT(*) FROM transactions"
        ).fetchone()[0] == 0

    def test_default_per_run_scope_reparses_zero_tx_statement(
            self, monkeypatch, migrated, tmp_path):
        # Without a shared set (the default), the zero-transaction
        # statement is re-parsed in the second run — the wasted work the
        # shared set removes — yet silver is unchanged (one position row,
        # no transactions).
        calls = self._count_calls(monkeypatch)
        run1 = self._bronze(tmp_path, "20260301T000000Z")
        run2 = self._bronze(tmp_path, "20260302T000000Z",
                            extra=b"%SCHWAB-REGEN-2%")
        load.load_run(migrated, run1, workers=1)
        load.load_run(migrated, run2, workers=1)
        migrated.commit()
        assert len(calls) == 2
        assert migrated.execute(
            "SELECT COUNT(*) FROM historical_position_snapshots"
        ).fetchone()[0] == 1
        assert migrated.execute(
            "SELECT COUNT(*) FROM transactions"
        ).fetchone()[0] == 0


class TestRollbackDoesNotPoisonSeenSet:
    """A bronze run that rolls back must not leave its logical docs marked
    seen in the invocation-scoped set. load_run marks a doc seen during
    the manifest walk, before the run commits; run_load snapshots the set
    before each run and restores it on failure. Without that restore, a
    doc first seen in a run that later fails would be skipped by every
    subsequent run, silently dropping its rows."""

    PARSED = {
        "path": "<patched>",
        "period_start": "2026-02-01",
        "period_end": "2026-02-28",
        "transactions": [],
        "positions": [
            {"instrument_key": "SYN1", "description": "Synthetic One",
             "quantity": 100.0, "market_price": 50.0,
             "market_value": 5000.0, "cost_basis": 4000.0,
             "unrealized_gain_loss": 1000.0, "accrued_interest": None,
             "est_yield": None, "est_annual_income": None,
             "pct_of_acct": "1%", "section": "Equities", "raw_lines": []},
        ],
        "cash_summary": None,
        "account_registration": None,
    }

    def test_failing_run_does_not_skip_doc_in_later_run(
            self, monkeypatch, tmp_path):
        import argparse

        bronze = tmp_path / "bronze"
        filename = "Brokerage-Statement_2026-02-28_NNN.PDF"
        # Two runs carrying the same logical statement.
        for ts in ("20260301T000000Z", "20260302T000000Z"):
            _make_bronze_run(bronze, ts, [
                {"suffix": "NNN", "label": "L",
                 "documents": [{"date": "02/28/2026", "type": "Statements",
                                "document": "Brokerage Statement",
                                "filename": filename}]},
            ])

        parse_calls: list[str] = []

        def _counting(path, statement_year=None):
            parse_calls.append(path)
            return dict(self.PARSED)

        monkeypatch.setattr(load.pp, "parse_statement_pdf", _counting)

        # Fail the position insert on its first call (the first bronze
        # run) so that run rolls back; succeed on every later call.
        orig_insert = load._insert_position_snapshots
        insert_calls = {"n": 0}

        def _flaky_insert(*a, **k):
            insert_calls["n"] += 1
            if insert_calls["n"] == 1:
                raise RuntimeError("synthetic insert failure")
            return orig_insert(*a, **k)

        monkeypatch.setattr(load, "_insert_position_snapshots", _flaky_insert)

        db = tmp_path / "silver.db"
        args = argparse.Namespace(
            silver_db=db, bronze_dir=bronze, migrations_dir=MIGRATIONS_DIR,
            reparse=False, workers=1, verbose=False, force=False,
        )
        assert load.run_load(args) == 0

        conn = sqlite3.connect(str(db))
        try:
            n_pos = conn.execute(
                "SELECT COUNT(*) FROM historical_position_snapshots"
            ).fetchone()[0]
        finally:
            conn.close()
        # Run 1 marked the doc seen then rolled back; run 2 must re-parse
        # and insert it — two parses, one surviving position row. With a
        # poisoned seen-set run 2 would skip the doc and leave zero rows.
        assert len(parse_calls) == 2
        assert n_pos == 1


# ============================================================
# 1099-B + 3rd-Party-Distribution load integration
# ============================================================

def _make_doc_bronze(root: Path, run_ts: str, suffix: str,
                     docs: list[dict]) -> Path:
    """Bronze run whose statements area holds arbitrary documents.

    Each doc: {date, type, document, filename, content}. `content` is
    written verbatim (real synthetic XML/CSV, or a stub PDF body), so
    the real parsers run against it where applicable. format is
    inferred from the extension."""
    run_dir = root / run_ts
    run_dir.mkdir(parents=True, exist_ok=True)
    stmts = []
    for d in docs:
        p = run_dir / "statements" / suffix / d["filename"]
        p.parent.mkdir(parents=True, exist_ok=True)
        content = d["content"]
        p.write_bytes(content.encode("utf-8") if isinstance(content, str)
                      else content)
        sha, _ = bronze.sha256_file(p)
        stmts.append({
            "date": d["date"], "type": d["type"],
            "document": d.get("document", ""), "filename": d["filename"],
            "format": d.get("format")
            or d["filename"].rsplit(".", 1)[-1].lower(),
            "size": p.stat().st_size, "sha256": sha,
        })
    manifest = {
        "run_ts": run_ts, "mode": "statements", "dry_run": False,
        "statements": [{"suffix": suffix, "label": f"Demo …{suffix}",
                        "documents": stmts}],
        "transactions": [],
    }
    (run_dir / "run.json").write_text(json.dumps(manifest))
    return run_dir


# A 2-lot synthetic 1099-B OFX-2.x XML (covered + Various lots).
_MINI_1099B_XML = (
    '<?xml version="1.0"?>\n<?OFX OFXHEADER="200" VERSION="200" ?>\n'
    "<OFX><TAX1099MSGSRSV1><TAX1099TRNRS><TAX1099RS>"
    "<TAX1099B_V100><TAXYEAR>2021</TAXYEAR><EXTDBINFO_V100>"
    "<PROCDET_V100><FORM8949CODE>D</FORM8949CODE><DTSALE>20211001</DTSALE>"
    "<SECNAME>SYNTH ALPHA CORP</SECNAME>"
    "<SALEDESCRIPTION>100.00 SYNTH ALPHA CORP</SALEDESCRIPTION>"
    "<NUMSHRS>100.000000</NUMSHRS><COSTBASIS>100.00</COSTBASIS>"
    "<SALESPR>3000.00</SALESPR><LONGSHORT>LONG</LONGSHORT>"
    "<DTAQD>20100104</DTAQD><NONCOVEREDSECURITY>N</NONCOVEREDSECURITY>"
    "<BASISNOTSHOWN>N</BASISNOTSHOWN></PROCDET_V100>"
    "<PROCDET_V100><FORM8949CODE>B</FORM8949CODE><DTSALE>20210615</DTSALE>"
    "<SECNAME>SYNTH BETA INC</SECNAME>"
    "<SALEDESCRIPTION>50.00 SYNTH BETA INC</SALEDESCRIPTION>"
    "<NUMSHRS>50.000000</NUMSHRS><COSTBASIS>500.00</COSTBASIS>"
    "<SALESPR>1000.00</SALESPR><LONGSHORT>SHORT</LONGSHORT><DTVAR>Y</DTVAR>"
    "<NONCOVEREDSECURITY>N</NONCOVEREDSECURITY>"
    "<BASISNOTSHOWN>N</BASISNOTSHOWN></PROCDET_V100>"
    "</EXTDBINFO_V100></TAX1099B_V100></TAX1099RS></TAX1099TRNRS>"
    "</TAX1099MSGSRSV1></OFX>"
)

# A 1-lot CSV twin whose security name is a sentinel — if it ever shows
# up in silver, the CSV was parsed when the XML twin should have won.
_MINI_1099B_CSV = (
    ":Account,XXXX-X999\r\n"
    "Form 1099 B\r\n"
    "1a,1b,1c,1d,1e,2,12\r\n"
    "Description of property,Date acquired,Date sold,Proceeds,"
    "Cost or other basis,Term,Basis reported\r\n"
    "9.00 CSV SENTINEL CORP,01/02/2020,03/04/2021,99.00,9.00,Long Term,X\r\n"
)


class TestForm1099bLoad:
    def test_xml_loads_sale_rows(self, migrated, tmp_path):
        run = _make_doc_bronze(tmp_path, "20260520T120000Z", "999", [
            {"date": "02/15/2022", "type": "Tax Forms",
             "document": "1099 Composite and Year-End Summary - 2021",
             "filename": "XXXX-X999.XML", "content": _MINI_1099B_XML},
        ])
        stats = load.load_run(migrated, run, workers=1)
        migrated.commit()
        assert stats["form_1099b_transactions_inserted"] == 2
        rows = migrated.execute(
            "SELECT kind, instrument_key, source,"
            " json_extract(payload,'$.security_name'),"
            " json_extract(payload,'$.cost_basis'),"
            " json_extract(payload,'$.tax_year')"
            " FROM transactions WHERE source='form_1099b' ORDER BY timestamp"
        ).fetchall()
        assert rows == [
            ("Sale", None, "form_1099b", "SYNTH BETA INC", 500.0, 2021),
            ("Sale", None, "form_1099b", "SYNTH ALPHA CORP", 100.0, 2021),
        ]

    def test_xml_preferred_over_csv_twin(self, migrated, tmp_path):
        # XML + CSV share the base filename "XXXX-X999"; the loader must
        # parse exactly one (XML) — 2 rows, never the CSV sentinel.
        run = _make_doc_bronze(tmp_path, "20260520T120000Z", "999", [
            {"date": "02/15/2022", "type": "Tax Forms",
             "document": "1099 Composite and Year-End Summary - 2021",
             "filename": "XXXX-X999.XML", "content": _MINI_1099B_XML},
            {"date": "02/15/2022", "type": "Tax Forms",
             "document": "1099 Composite and Year-End Summary - 2021",
             "filename": "XXXX-X999.CSV", "content": _MINI_1099B_CSV},
        ])
        stats = load.load_run(migrated, run, workers=1)
        migrated.commit()
        assert stats["form_1099b_transactions_inserted"] == 2
        n = migrated.execute(
            "SELECT COUNT(*) FROM transactions WHERE source='form_1099b'"
        ).fetchone()[0]
        assert n == 2
        sentinel = migrated.execute(
            "SELECT COUNT(*) FROM transactions WHERE"
            " json_extract(payload,'$.security_name') = 'CSV SENTINEL CORP'"
        ).fetchone()[0]
        assert sentinel == 0

    def test_csv_fallback_when_no_xml(self, migrated, tmp_path):
        run = _make_doc_bronze(tmp_path, "20260520T120000Z", "999", [
            {"date": "02/15/2022", "type": "Tax Forms",
             "document": "1099 Composite and Year-End Summary - 2021",
             "filename": "XXXX-X999.CSV", "content": _MINI_1099B_CSV},
        ])
        stats = load.load_run(migrated, run, workers=1)
        migrated.commit()
        assert stats["form_1099b_transactions_inserted"] == 1
        name = migrated.execute(
            "SELECT json_extract(payload,'$.security_name')"
            " FROM transactions WHERE source='form_1099b'"
        ).fetchone()[0]
        assert name == "CSV SENTINEL CORP"

    def test_reload_is_idempotent(self, migrated, tmp_path):
        docs = [{"date": "02/15/2022", "type": "Tax Forms",
                 "document": "1099 Composite and Year-End Summary - 2021",
                 "filename": "XXXX-X999.XML", "content": _MINI_1099B_XML}]
        load.load_run(migrated, _make_doc_bronze(
            tmp_path, "20260520T120000Z", "999", docs), workers=1)
        migrated.commit()
        # Re-download (new run_ts, same logical form). The base-filename
        # logical_doc_key gate must skip re-insertion.
        stats2 = load.load_run(migrated, _make_doc_bronze(
            tmp_path, "20260521T120000Z", "999", docs), workers=1)
        migrated.commit()
        assert stats2["form_1099b_transactions_inserted"] == 0
        n = migrated.execute(
            "SELECT COUNT(*) FROM transactions WHERE source='form_1099b'"
        ).fetchone()[0]
        assert n == 2

    def test_reparse_parse_failure_preserves_prior_rows(
            self, monkeypatch, migrated, tmp_path):
        """A parse that fails during --reparse must NOT drop the prior
        rows (delete happens only after a successful parse)."""
        docs = [{"date": "02/15/2022", "type": "Tax Forms",
                 "document": "1099 Composite and Year-End Summary - 2021",
                 "filename": "XXXX-X999.XML", "content": _MINI_1099B_XML}]
        load.load_run(migrated, _make_doc_bronze(
            tmp_path, "20260520T120000Z", "999", docs), workers=1)
        migrated.commit()
        n0 = migrated.execute(
            "SELECT COUNT(*) FROM transactions WHERE source='form_1099b'"
        ).fetchone()[0]
        assert n0 == 2

        def _boom(path, fmt=None):
            raise RuntimeError("synthetic parse failure")
        monkeypatch.setattr(load.tf, "parse_1099b", _boom)
        stats = load.load_run(migrated, _make_doc_bronze(
            tmp_path, "20260521T120000Z", "999", docs),
            reparse=True, workers=1)
        migrated.commit()
        assert stats["form_1099b_parse_errors"] == 1
        assert stats["transactions_reparsed"] == 0   # delete not performed
        n1 = migrated.execute(
            "SELECT COUNT(*) FROM transactions WHERE source='form_1099b'"
        ).fetchone()[0]
        assert n1 == n0 == 2                          # prior rows preserved

    def test_pdf_only_form_is_coverage_gap(self, migrated, tmp_path):
        run = _make_doc_bronze(tmp_path, "20260520T120000Z", "999", [
            {"date": "02/15/2019", "type": "Tax Forms",
             "document": "1099 Composite and Year-End Summary - 2018",
             "filename": "XXXX-X999.PDF", "content": "%PDF-1.4 stub\n"},
        ])
        stats = load.load_run(migrated, run, workers=1)
        migrated.commit()
        assert stats["form_1099b_transactions_inserted"] == 0
        assert stats["form_1099b_pdf_only"] == 1


class TestDistributionLoad:
    _SYNTH_ROWS = [{
        "date": "2024-01-23", "direction": "out", "method": "schwab_third_party",
        "counterparty": "SYNTH FAMILY TRUST", "counterparty_bank": None,
        "counterparty_account_suffix": "321", "kind": "Transfer Out",
        "amount": 1250.0, "symbol": "VTI", "instrument_key": "VTI",
        "description": "Securities transfer out to SYNTH FAMILY TRUST",
        "transfer_kind": "securities", "security_symbol": "VTI",
        "quantity": 12.5, "market_value": 1250.0,
    }]

    def _bronze(self, tmp_path, run_ts="20260520T120000Z",
                filename="3rd-Party-Distribution_2024-01-23_999.PDF"):
        return _make_doc_bronze(tmp_path, run_ts, "999", [
            {"date": "01/23/2024", "type": "Letters",
             "document": "3rd Party Distribution",
             "filename": filename, "content": f"%PDF-1.4 stub {filename}\n"},
        ])

    def test_distribution_letter_loads(self, monkeypatch, migrated, tmp_path):
        monkeypatch.setattr(load.pp, "parse_distribution_pdf",
                            lambda path: [dict(r) for r in self._SYNTH_ROWS])
        stats = load.load_run(migrated, self._bronze(tmp_path), workers=1)
        migrated.commit()
        assert stats["distribution_transactions_inserted"] == 1
        row = migrated.execute(
            "SELECT kind, instrument_key, source,"
            " json_extract(payload,'$.transfer_kind'),"
            " json_extract(payload,'$.counterparty'),"
            " json_extract(payload,'$.market_value')"
            " FROM transactions WHERE source='third_party_distribution'"
        ).fetchone()
        assert row == ("Transfer Out", "VTI", "third_party_distribution",
                       "securities", "SYNTH FAMILY TRUST", 1250.0)

    def test_distribution_reload_is_idempotent(
            self, monkeypatch, migrated, tmp_path):
        monkeypatch.setattr(load.pp, "parse_distribution_pdf",
                            lambda path: [dict(r) for r in self._SYNTH_ROWS])
        load.load_run(migrated, self._bronze(tmp_path), workers=1)
        migrated.commit()
        stats2 = load.load_run(
            migrated, self._bronze(tmp_path, run_ts="20260521T120000Z"),
            workers=1)
        migrated.commit()
        assert stats2["distribution_transactions_inserted"] == 0
        n = migrated.execute(
            "SELECT COUNT(*) FROM transactions"
            " WHERE source='third_party_distribution'"
        ).fetchone()[0]
        assert n == 1

    def test_reparse_parse_failure_preserves_prior_rows(
            self, monkeypatch, migrated, tmp_path):
        monkeypatch.setattr(load.pp, "parse_distribution_pdf",
                            lambda path: [dict(r) for r in self._SYNTH_ROWS])
        load.load_run(migrated, self._bronze(tmp_path), workers=1)
        migrated.commit()

        def _boom(path):
            raise RuntimeError("synthetic parse failure")
        monkeypatch.setattr(load.pp, "parse_distribution_pdf", _boom)
        stats = load.load_run(
            migrated, self._bronze(tmp_path, run_ts="20260521T120000Z"),
            reparse=True, workers=1)
        migrated.commit()
        assert stats["distribution_parse_errors"] == 1
        assert stats["transactions_reparsed"] == 0
        n = migrated.execute(
            "SELECT COUNT(*) FROM transactions"
            " WHERE source='third_party_distribution'"
        ).fetchone()[0]
        assert n == 1                                # prior row preserved
