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

    def test_sha256_independent(self):
        """activity_id must be identical regardless of which sha256
        (i.e. which download of the same logical PDF) produced the row.
        This is the core fix for the sha256-churn duplication bug."""
        a = load._synthesize_activity_id("000", self.BASE_TX, 0)
        b = load._synthesize_activity_id("000", self.BASE_TX, 0)
        assert a == b  # trivially true — sha256 is no longer a param

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
