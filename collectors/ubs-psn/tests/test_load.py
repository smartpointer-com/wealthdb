"""Bronze→silver tests for the ubs-psn collector's load.py.

ubs-psn's bronze is SWIFT MT messages (and PSN XML) inside Z*.zip
containers. These tests exercise the MT parsing → silver path
directly with synthetic SWIFT text: an MT535 holdings message into
the `holdings` table, plus the balance-line parser. Synthetic
safekeeping ids / ISINs / amounts only.
"""
from __future__ import annotations

import sqlite3
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

REL = "SFTPCH99"

# Minimal synthetic MT535 Statement of Holdings: one safekeeping
# account (:97A::SAFE//) with one FIN holding block carrying an ISIN.
MT535 = (
    "{1:F01TESTXXXXAXXX0000000000}{2:I535TESTXXXXXXXXN}{4:\n"
    ":16R:GENL\n"
    ":97A::SAFE//SK123\n"
    ":16S:GENL\n"
    ":16R:FIN\n"
    ":35B:ISIN CH0000000001\n"
    "EXAMPLE FUND\n"
    ":93B::AGGR//FAMT/100,\n"
    ":16S:FIN\n"
    "-}"
)


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = loader.open_db(tmp_path / "ubs-psn.db")
    conn.row_factory = sqlite3.Row
    silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    return conn


def test_load_mt535_holdings(tmp_path):
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader.load_mt535(conn, 1700000000, REL, MT535)
    assert n == 1

    rows = conn.execute(
        "SELECT relationship_id, safekeeping_external_id, isin "
        "FROM holdings").fetchall()
    assert len(rows) == 1
    assert rows[0]["relationship_id"] == REL
    assert rows[0]["safekeeping_external_id"] == "SK123"
    assert rows[0]["isin"] == "CH0000000001"


def test_load_mt535_without_safe_is_noop(tmp_path):
    conn = _fresh_db(tmp_path)
    bad = "{4:\n:16R:FIN\n:35B:ISIN CH0000000001\n:16S:FIN\n-}"
    with conn:
        n = loader.load_mt535(conn, 1700000000, REL, bad)
    assert n == 0
    assert conn.execute("SELECT COUNT(*) FROM holdings").fetchone()[0] == 0


def test_parse_mt_balance():
    # :62F: closing-balance line, form <C|D>YYMMDD<CCY><amount>.
    bal = loader.parse_mt_balance("C231231CHF1234,56")
    assert bal is not None
    assert bal["currency_iso"] == "CHF"
    assert bal["credit_debit"] == "C"
    # SWIFT comma decimal is normalised to a dot.
    assert float(bal["amount"]) == 1234.56
