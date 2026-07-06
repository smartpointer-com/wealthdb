"""
Unit tests for load.py.

In-memory SQLite plus a tmp_path bronze tree. Covers:
  * migration apply
  * positions + dividend-view merge
  * activity CSV BOM + blank-line + header parsing
  * synthetic activity_id stability across reloads
  * documents content-sha dedup
  * portfolio classification (529 / trust_managed / other)
  * validation pass on a clean fixture
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
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    yield c
    c.close()


@pytest.fixture
def migrated(conn):
    load.apply_migrations(conn, MIGRATIONS_DIR)
    return conn


# Synthetic bronze dump: one 529 + one Authorized sleeve. Two activity
# rows, two positions (one in summary only, one in both views to
# exercise the merge), one statement PDF, one tax-form PDF.
# Identifiers are placeholders — no real Fidelity account numbers
# or instrument codes in tests.
ACCT_529 = "100000001"
ACCT_TRUST = "200000002"
SYM_529 = "PLAN00001"
SYM_TRUST = "TICK1"


def _hash16(aid: str) -> str:
    import hashlib
    return hashlib.sha256(aid.encode("ascii")).hexdigest()[:16]


def _write_dump(root: Path, ts: str) -> Path:
    """Build a bronze tree matching what download.walk() emits."""
    dump = root / ts
    dump.mkdir(parents=True, exist_ok=True)

    # run.json: account_dimensions + accounts lists.
    dims = {
        _hash16(ACCT_529): {
            "in_scope": True, "nickname": "Beneficiary",
            "portfolio": "Education",
        },
        _hash16(ACCT_TRUST): {
            "in_scope": True, "nickname": "Trust: Under Agreement",
            "portfolio": "Authorized",
        },
    }
    run = {
        "cli_config": {"mode": "all"},
        "accounts_enumerated": [ACCT_529, ACCT_TRUST],
        "accounts_in_scope":   [ACCT_529, ACCT_TRUST],
        "account_dimensions": dims,
        "activity_window": None,
    }
    (dump / "run.json").write_text(json.dumps(run))

    # positions/positions_summary.csv: BOM + header + two rows.
    pos_summary = (
        "﻿Account Number,Account Name,Symbol,Description,"
        "Quantity,Last Price,Last Price Change,Current Value,"
        "Today's Gain/Loss Dollar,Today's Gain/Loss Percent,"
        "Total Gain/Loss Dollar,Total Gain/Loss Percent,"
        "Percent Of Account,Cost Basis Total,Average Cost Basis,Type\n"
        f"{ACCT_529},Beneficiary,{SYM_529},STATE PLACEHOLDER FUND,"
        "100,$10.00,+$0.05,$1000.00,+$5,+0.5%,+$100,+11%,80%,"
        "$900,$9.00,Cash,\n"
        f"{ACCT_TRUST},Trust: Under Agreement,{SYM_TRUST},"
        "STUB TICKER ONE,50,$20.00,-$0.10,$1000.00,-$5,-0.5%,"
        "+$50,+5%,40%,$950,$19.00,Cash,\n"
    )
    (dump / "positions").mkdir()
    (dump / "positions" / "positions_summary.csv").write_text(pos_summary)

    # positions/positions_dividend.csv: same TICK1 row with
    # dividend-view columns; loader merges into one silver row.
    pos_div = (
        "﻿Account Number,Account Name,Symbol,Description,"
        "Quantity,Last Price,Last Price Change,Current Value,"
        "Percent Of Account,Ex-date,Amount per share,Pay date,"
        "Dist. yield,Distribution yield as of,SEC yield,"
        "SEC yield as of,Est. annual income,Type\n"
        f"{ACCT_TRUST},Trust: Under Agreement,{SYM_TRUST},"
        "STUB TICKER ONE,50,$20.00,-$0.10,$1000.00,40%,"
        "01/15/2026,$0.25,02/15/2026,5%,01/15/2026,4.8%,"
        "01/15/2026,$12.50,Cash,\n"
    )
    (dump / "positions" / "positions_dividend.csv").write_text(pos_div)

    # activity/activity_*.csv: BOM + blank line + header + two rows
    # (one DIVIDEND, one YOU BOUGHT — exercises the action classifier).
    activity = (
        "﻿\n"
        "\n"
        "Run Date,Account,Account Number,Action,Symbol,Description,"
        "Type,Price ($),Quantity,Commission ($),Fees ($),"
        "Accrued Interest ($),Amount ($),Settlement Date\n"
        f'06/01/2024,"Trust: Under Agreement","{ACCT_TRUST}",'
        f'"DIVIDEND RECEIVED STUB TICKER ONE ({SYM_TRUST}) (Cash)",'
        f'{SYM_TRUST},"STUB TICKER ONE",Cash,,0.000,,,,12.50,\n'
        f'06/02/2024,"Beneficiary","{ACCT_529}",'
        f'"YOU BOUGHT STATE PLACEHOLDER FUND ({SYM_529}) (Cash)",'
        f'{SYM_529},"STATE PLACEHOLDER FUND",Cash,10.00,5.000,,,,-50.00,'
        '06/03/2024\n'
    )
    (dump / "activity").mkdir()
    (dump / "activity" / "activity_20240501__20240730.csv").write_text(activity)

    # documents/: one statement PDF + one tax-form PDF.
    (dump / "documents").mkdir()
    (dump / "documents" / "Statement3312026.pdf").write_bytes(
        b"%PDF-1.4 stub statement\n"
    )
    (dump / "documents" /
     "2025-Example-0001-Consolidated-Form-1099.pdf"
     ).write_bytes(b"%PDF-1.4 stub 1099\n")

    return dump


# ============================================================
# Tests
# ============================================================

def test_apply_migrations_creates_schema(conn):
    load.apply_migrations(conn, MIGRATIONS_DIR)
    cur = conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
    )
    names = [r[0] for r in cur.fetchall()]
    assert names == [
        "accounts", "documents", "dump_runs",
        "historical_position_snapshots", "portfolios",
        "positions", "schema_meta", "transactions",
    ]


def test_load_dump_populates_all_tables(migrated, tmp_path):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 1)
    counts = {
        t: migrated.execute(f"SELECT COUNT(*) FROM {t}").fetchone()[0]
        for t in (
            "dump_runs", "portfolios", "accounts",
            "positions", "transactions", "documents",
        )
    }
    assert counts == {
        "dump_runs": 1, "portfolios": 2, "accounts": 2,
        "positions": 2, "transactions": 2, "documents": 2,
    }


def test_portfolios_classified(migrated, tmp_path):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 1)
    rows = migrated.execute(
        "SELECT portfolio_external_id, kind "
        "FROM portfolios ORDER BY portfolio_external_id"
    ).fetchall()
    assert rows == [("Authorized", "trust_managed"),
                    ("Education", "529")]


def test_positions_merge_summary_and_dividend(migrated, tmp_path):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 1)
    # The trust row has both summary and dividend views; merge
    # must populate ex_date + amount_per_share from dividend.
    row = migrated.execute(
        "SELECT instrument_key, cost_basis_total, ex_date, "
        "       amount_per_share, est_annual_income "
        "FROM positions WHERE account_external_id = ?",
        (ACCT_TRUST,),
    ).fetchone()
    instr, cost, ex_date, aps, eai = row
    assert instr == SYM_TRUST
    assert cost == 950.0           # from summary
    assert ex_date is not None     # from dividend
    assert aps == 0.25             # from dividend
    assert eai == 12.50            # from dividend


def test_action_classifier_buy_sell_dividend():
    assert load._classify_action(
        "YOU BOUGHT FOO (FOO) (Cash)") == "BUY"
    assert load._classify_action(
        "YOU SOLD FOO (FOO) (Cash)") == "SELL"
    assert load._classify_action(
        "DIVIDEND RECEIVED FOO (FOO) (Cash)") == "DIVIDEND"
    assert load._classify_action(
        "PURCHASE INTO CORE ACCOUNT FOO") == "CASH_SWEEP_IN"
    assert load._classify_action(
        "FOREIGN TAX WITHHELD FOO") == "TAX"
    assert load._classify_action(
        "SHORT-TERM CAP GAIN FOO") == "DISTRIBUTION"
    assert load._classify_action("") == "UNKNOWN"


def test_activity_id_stable_across_reload(migrated, tmp_path):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 1)
    first = set(
        r[0] for r in migrated.execute("SELECT activity_id FROM transactions")
    )
    # Re-load the same dump dir (second snapshot_at, same source bytes).
    _write_dump(tmp_path, "20260201T120000Z")
    load.load_dump(migrated, tmp_path / "20260201T120000Z", 1)
    second = set(
        r[0] for r in migrated.execute("SELECT activity_id FROM transactions")
    )
    # The synthetic ID is content-derived → same rows both times,
    # no growth in the transaction set.
    assert first == second


def _activity_csv(rows: str) -> str:
    """Wrap activity data rows in the BOM + blank-line + header
    envelope Fidelity emits."""
    return (
        "﻿\n\n"
        "Run Date,Account,Account Number,Action,Symbol,Description,"
        "Type,Price ($),Quantity,Commission ($),Fees ($),"
        "Accrued Interest ($),Amount ($),Settlement Date\n"
        + rows
    )


def _div_row(date: str, amount: str) -> str:
    return (
        f'{date},"Trust: Under Agreement","{ACCT_TRUST}",'
        f'"DIVIDEND RECEIVED STUB TICKER ONE ({SYM_TRUST}) (Cash)",'
        f'{SYM_TRUST},"STUB TICKER ONE",Cash,,0.000,,,,{amount},\n'
    )


def test_activity_dedups_across_overlapping_windows(migrated, tmp_path):
    """The same transaction re-downloaded in different window files
    (different bytes → different source_sha256) must collapse onto
    one row. This is the regression guard for the backfill-dup bug:
    the dedup key must be file-independent."""
    dump = tmp_path / "20260101T120000Z"
    (dump / "activity").mkdir(parents=True)
    # Two overlapping windows both containing the 06/01 dividend;
    # window B additionally carries a 06/15 dividend. Distinct
    # filenames + distinct bytes → distinct source_sha256.
    win_a = _activity_csv(_div_row("06/01/2024", "12.50"))
    win_b = _activity_csv(
        _div_row("06/01/2024", "12.50") + _div_row("06/15/2024", "9.00")
    )
    (dump / "activity" / "activity_20240401__20240630.csv").write_text(win_a)
    (dump / "activity" / "activity_20240501__20240731.csv").write_text(win_b)
    load._load_transactions(migrated, 1, dump)
    n = migrated.execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    # 2 distinct transactions (06/01 + 06/15), not 3.
    assert n == 2


def test_activity_preserves_genuine_same_day_duplicates(migrated, tmp_path):
    """Two byte-identical rows within a single export are two real
    transactions (e.g. two same-day, same-amount fills) and must be
    preserved — the per-file occurrence index keeps them distinct,
    and they still collapse correctly across a re-download."""
    dump = tmp_path / "20260101T120000Z"
    (dump / "activity").mkdir(parents=True)
    twice = _activity_csv(
        _div_row("06/01/2024", "12.50") + _div_row("06/01/2024", "12.50")
    )
    (dump / "activity" / "activity_20240401__20240630.csv").write_text(twice)
    # A second window file with the same pair of identical rows.
    (dump / "activity" / "activity_20240501__20240731.csv").write_text(twice)
    load._load_transactions(migrated, 1, dump)
    n = migrated.execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    # Both genuine copies survive (2), but the re-download doesn't
    # inflate them to 4.
    assert n == 2


def test_read_signature_sidecar_first_real_line(tmp_path):
    # First non-empty, non-comment line wins; the registration (PII)
    # lives in the data dir, never in argv or a committed script.
    (tmp_path / "signature.txt").write_text(
        "# guard substring\n\nPLACEHOLDER HOLDER\nIGNORED SECOND LINE\n"
    )
    assert load._read_signature_sidecar(tmp_path) == "PLACEHOLDER HOLDER"


def test_read_signature_sidecar_absent_returns_none(tmp_path):
    assert load._read_signature_sidecar(tmp_path) is None


def test_trust_statements_default_dir_missing_is_noop(migrated, tmp_path):
    # The bronze-resident default (<bronze>/supplied-statements) simply
    # not existing must be a clean no-op — deployments without trust
    # accounts never create it.
    missing = tmp_path / "supplied-statements"
    # schema_version 4 so the historical table exists; the guard is
    # the directory check, not the schema.
    load._load_trust_statements_oneshot(migrated, missing, 4)
    n = migrated.execute(
        "SELECT COUNT(*) FROM historical_position_snapshots"
    ).fetchone()[0]
    assert n == 0


def test_documents_dedup_on_content_sha(migrated, tmp_path):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 1)
    # Build a second dump with byte-identical documents.
    _write_dump(tmp_path, "20260201T120000Z")
    load.load_dump(migrated, tmp_path / "20260201T120000Z", 1)
    count = migrated.execute(
        "SELECT COUNT(*) FROM documents"
    ).fetchone()[0]
    # 2 PDFs, deduped on content_sha256 across two dumps.
    assert count == 2


def test_validation_finds_no_unexpected_null_instrument_keys(
    migrated, tmp_path, caplog,
):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 1)
    with caplog.at_level("WARNING"):
        load.validate(migrated)
    assert not any(
        "fell outside the CASH_ONLY_ACTIONS allowlist" in r.message
        for r in caplog.records
    )


def test_parse_decimal_handles_fidelity_formats():
    assert load.parse_decimal("$1,234.56") == 1234.56
    assert load.parse_decimal("+$5.00") == 5.0
    assert load.parse_decimal("-$50") == -50.0
    assert load.parse_decimal("+0.5%") == pytest.approx(0.005)
    assert load.parse_decimal("--") is None
    assert load.parse_decimal("") is None
    assert load.parse_decimal(None) is None


# ============================================================
# Migration-0002 columns: currency, asset_class, is_core_position
# ============================================================

def test_currency_defaults_to_usd(migrated, tmp_path):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 2)
    pos_currencies = {
        r[0] for r in migrated.execute(
            "SELECT DISTINCT currency FROM positions"
        )
    }
    txn_currencies = {
        r[0] for r in migrated.execute(
            "SELECT DISTINCT currency FROM transactions"
        )
    }
    assert pos_currencies == {"USD"}
    assert txn_currencies == {"USD"}


def test_money_market_suffix_stripped_and_flagged(migrated, tmp_path):
    """A position row whose Symbol ends in '**' (Fidelity's core
    money-market channel signal) lands in silver with the asterisks
    stripped from instrument_key and is_core_position = 1."""
    dump = tmp_path / "20260101T120000Z"
    _write_dump(tmp_path, "20260101T120000Z")
    # Patch in a core-position row.
    csv = dump / "positions" / "positions_summary.csv"
    text = csv.read_text()
    text += (
        f"{ACCT_TRUST},Trust: Under Agreement,CORE_X**,"
        "FIDELITY GOVERNMENT CASH RESERVES,1000,$1.00,+$0.00,$1000.00,"
        "$0,0.0%,+$0,0.0%,5%,$1000.00,$1.00,Cash,\n"
    )
    csv.write_text(text)
    load.load_dump(migrated, dump, 2)
    row = migrated.execute(
        "SELECT instrument_key, is_core_position, asset_class "
        "FROM positions WHERE account_external_id = ? "
        "AND description LIKE 'FIDELITY GOVERNMENT%'",
        (ACCT_TRUST,),
    ).fetchone()
    assert row == ("CORE_X", 1, "money_market")


def test_asset_class_classifier_covers_known_shapes():
    # Money-market via is_core_position=1
    assert load._classify_asset_class("CORE_X", "anything", 1) == "money_market"
    # CUSIP-shaped 9-char ticker → bond
    assert load._classify_asset_class(
        "000000AA1", "PLACEHOLDER MUNI BOND", 0,
    ) == "bond"
    # 3-letter + 6-digit Fidelity 529 plan-fund code
    assert load._classify_asset_class(
        "ABC123456", "STATE PLAN PORTFOLIO 2030", 0,
    ) == "plan_fund"
    # Industry mutual-fund convention: 5 chars ending in X
    assert load._classify_asset_class(
        "FXAIX", "FIDELITY 500 INDEX FUND", 0,
    ) == "mutual_fund"
    # Common stock — fall through to equity
    assert load._classify_asset_class("AAPL", "APPLE INC", 0) == "equity"
    # ADR — 5-char ending in Y, not X → equity
    assert load._classify_asset_class(
        "ABCDY", "PLACEHOLDER ADR", 0,
    ) == "equity"
    # ETF — 3-char alpha → equity (gold disambiguates further)
    assert load._classify_asset_class("SPY", "S&P 500 ETF", 0) == "equity"


def test_dump_runs_no_balances_or_performance_columns(migrated, tmp_path):
    """Migration 0002 dropped the *_present flags for balances /
    performance — the documents table is the source of truth."""
    cols = {
        r[1] for r in migrated.execute(
            "PRAGMA table_info(dump_runs)"
        )
    }
    assert "balances_present" not in cols
    assert "performance_present" not in cols
    assert "positions_present" in cols  # the data-bearing phases stay


# ============================================================
# Migration-0003 column: management_style on accounts
# ============================================================

def test_management_style_derived_from_portfolio_kind(migrated, tmp_path):
    _write_dump(tmp_path, "20260101T120000Z")
    load.load_dump(migrated, tmp_path / "20260101T120000Z", 3)
    rows = migrated.execute(
        "SELECT a.portfolio_external_id, p.kind, a.management_style "
        "FROM accounts a "
        "JOIN portfolios p "
        "  ON p.snapshot_at = a.snapshot_at "
        " AND p.portfolio_external_id = a.portfolio_external_id "
        "ORDER BY a.account_external_id"
    ).fetchall()
    by_kind = {kind: style for _, kind, style in rows}
    assert by_kind["529"] == "self_directed"
    assert by_kind["trust_managed"] == "discretionary"


def test_management_style_other_kind_is_null(migrated):
    """Anything that doesn't match the 529 / trust_managed kinds
    in PORTFOLIO_KIND falls through to NULL management_style — the
    silver loader doesn't guess for unknown group labels."""
    assert load.MANAGEMENT_STYLE_BY_KIND.get("other") is None
    assert load.MANAGEMENT_STYLE_BY_KIND.get(None) is None
