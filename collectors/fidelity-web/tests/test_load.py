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
import re
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


def test_activity_dedups_across_description_relabels(migrated, tmp_path):
    """The same transaction whose security text Fidelity re-labelled
    between exports (Action/Description drift, structural columns
    unchanged) must still collapse onto one row. Regression guard
    for the payload-hash identity, which split on any text change."""
    dump = tmp_path / "20260101T120000Z"
    (dump / "activity").mkdir(parents=True)
    row_a = (
        f'06/01/2024,"Trust: Under Agreement","{ACCT_TRUST}",'
        f'"MERGER MER FROM 000000000#REOR R0000000000000 STUB CO '
        f'SPONSORED ADR ({SYM_TRUST}) (Cash)",'
        f'{SYM_TRUST},"STUB CO SPONSORED ADR",Cash,,25.000,,,,'
        f'1234.50,\n'
    )
    row_b = (
        f'06/01/2024,"Trust: Under Agreement","{ACCT_TRUST}",'
        f'"MERGER MER FROM 000000000#REOR R0000000000000 STUB CO '
        f'SPON ADS EACH REP 1 O ({SYM_TRUST}) (Cash)",'
        f'{SYM_TRUST},"STUB CO SPON ADS EACH REP 1 O",Cash,,25.000,,,,'
        f'1234.50,\n'
    )
    (dump / "activity" / "activity_20240401__20240630.csv").write_text(
        _activity_csv(row_a))
    (dump / "activity" / "activity_20240501__20240731.csv").write_text(
        _activity_csv(row_b))
    load._load_transactions(migrated, 1, dump)
    n = migrated.execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    assert n == 1


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
    # ETF — Fidelity groups ETFs with stocks, so the word "ETF" in
    # the description is the only signal
    assert load._classify_asset_class("SPY", "S&P 500 ETF", 0) == "etf"
    assert load._classify_asset_class(
        "ABCD", "ISHARES TR PLACEHOLDER ETF", 0,
    ) == "etf"
    # Word boundary: N-ETF-LIX must not match
    assert load._classify_asset_class("NFLX", "NETFLIX INC", 0) == "equity"
    # Name-shy exchange-traded products stay equity — that's what
    # the gold config's instrument_overrides are for
    assert load._classify_asset_class(
        "ABCD", "PLACEHOLDER TR METAL SHS", 0,
    ) == "equity"
    # Missing description never crashes the ETF check
    assert load._classify_asset_class("ABCD", None, 0) == "equity"


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


# ============================================================
# Bronze compression convergence
# ============================================================
#
# The hard invariant for zstd bronze compression: `load --force` on a
# compressed bronze tree must produce byte-identical silver to
# `load --force` on the same tree uncompressed. The loader keys the
# `documents` row on the DECOMPRESSED content (content_sha256 +
# size_bytes) and the LOGICAL name (`balances.html`, not
# `balances.html.zst`); the activity `source_sha256` is likewise the
# decompressed hash; positions/activity parse via a decompressing
# reader. Synthetic HTML/CSV bodies only — never real Fidelity data.

def _seed_convergence_dump(root: Path, ts: str) -> Path:
    """A full bronze dump plus balances/performance HTML and a
    documents/ statement-CSV companion, so every compressible path —
    positions/activity CSV, balances/performance HTML, and the
    documents/*.csv companion branch — is exercised."""
    dump = _write_dump(root, ts)  # positions + activity CSV + PDFs
    (dump / "balances").mkdir()
    (dump / "balances" / "balances.html").write_text(
        "<html><body><span data-testid='x-totalaccountvalue-label'>"
        "$1,234.56</span></body></html>",
        encoding="utf-8",
    )
    (dump / "performance").mkdir()
    (dump / "performance" / "performance.html").write_text(
        "<html><body><div>1yr +0.00%</div></body></html>",
        encoding="utf-8",
    )
    # documents/ statement-CSV companion (Fidelity saves a CSV twin of
    # some statement PDFs). Ingested as a document blob, not parsed —
    # synthetic bytes suffice. The documents/ dir already exists from
    # _write_dump.
    (dump / "documents" / "Statement3312026.csv").write_text(
        "Date,Description,Amount\n03/31/2026,PLACEHOLDER LINE,0.00\n",
        encoding="utf-8",
    )
    return dump


def _dump_tables(db_path: Path) -> dict:
    c = sqlite3.connect(str(db_path))
    try:
        return {
            t: c.execute(f"SELECT * FROM {t} ORDER BY 1, 2, 3").fetchall()
            for t in ("documents", "positions", "transactions",
                      "accounts", "portfolios",
                      "historical_position_snapshots")
        }
    finally:
        c.close()


# The compressible artefacts inside the seeded dump, relative to it.
_COMPRESSIBLE = (
    "positions/positions_summary.csv",
    "positions/positions_dividend.csv",
    "activity/activity_20240501__20240730.csv",
    "balances/balances.html",
    "performance/performance.html",
    "documents/Statement3312026.csv",
)


def test_compressed_bronze_converges_to_identical_silver(tmp_path):
    from collectorkit import compress

    bronze = tmp_path / "bronze"
    dump = _seed_convergence_dump(bronze, "20260101T120000Z")

    # 1) Load the plain tree.
    db_plain = tmp_path / "plain.db"
    load.main(["--silver-db", str(db_plain), "--bronze-dir", str(bronze)])
    plain = _dump_tables(db_plain)

    # 2) Compress every compressible artefact IN PLACE (same dir), so
    #    file_path converges too — PDFs and run.json stay untouched.
    for rel in _COMPRESSIBLE:
        f = dump / rel
        compress.compress_file(f)
        assert not f.exists()
        assert (f.with_name(f.name + ".zst")).is_file()

    # 3) Load the now-compressed tree into a fresh DB.
    db_zst = tmp_path / "zst.db"
    load.main(["--silver-db", str(db_zst), "--bronze-dir", str(bronze)])
    zst = _dump_tables(db_zst)

    # Every table identical — proves decompressed-hash + logical-name.
    assert zst == plain

    # Explicit: the balances documents row carries the LOGICAL name and
    # the DECOMPRESSED size, never the .zst name or the compressed size.
    c = sqlite3.connect(str(db_zst))
    try:
        name, size = c.execute(
            "SELECT file_name, size_bytes FROM documents "
            "WHERE doc_kind = 'balances_html'"
        ).fetchone()
    finally:
        c.close()
    zst_path = dump / "balances" / "balances.html.zst"
    _, decompressed_size = compress.decompressed_sha256(zst_path)
    assert name == "balances.html"                 # logical, not .zst
    assert size == decompressed_size               # decompressed length
    assert size != zst_path.stat().st_size         # NOT the compressed size

    # Same guarantee on the documents/*.csv companion branch (the one
    # compressible documents sub-path): logical name + decompressed size.
    c = sqlite3.connect(str(db_zst))
    try:
        csv_name, csv_size = c.execute(
            "SELECT file_name, size_bytes FROM documents "
            "WHERE doc_kind = 'statement' AND file_format = 'csv'"
        ).fetchone()
    finally:
        c.close()
    csv_zst = dump / "documents" / "Statement3312026.csv.zst"
    _, csv_decompressed = compress.decompressed_sha256(csv_zst)
    assert csv_name == "Statement3312026.csv"      # logical, not .zst
    assert csv_size == csv_decompressed


def test_activity_coexisting_plain_and_zst_ingests_once(migrated, tmp_path):
    """A recompress interrupted between verify and unlink leaves a plain
    activity CSV next to its .zst twin. The loader keys on the logical
    name and resolves one variant (plain wins), so the transactions are
    ingested exactly once — not doubled."""
    from collectorkit import compress

    dump = tmp_path / "20260101T120000Z"
    (dump / "activity").mkdir(parents=True)
    csv = dump / "activity" / "activity_20240401__20240630.csv"
    csv.write_text(_activity_csv(_div_row("06/01/2024", "12.50")))
    # Keep the original so plain + .zst coexist.
    compress.compress_file(csv, remove_original=False)
    assert csv.exists()
    assert csv.with_name(csv.name + ".zst").is_file()

    load._load_transactions(migrated, 1, dump)
    n = migrated.execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    assert n == 1


def test_logical_bronze_path_strips_compression_suffix():
    from pathlib import Path as _P
    assert load._logical_bronze_path(
        _P("a/balances.html.zst")) == _P("a/balances.html")
    assert load._logical_bronze_path(
        _P("a/x.csv.gz")) == _P("a/x.csv")
    # Plain paths (and PDFs) pass through unchanged.
    assert load._logical_bronze_path(
        _P("a/x.csv")) == _P("a/x.csv")
    assert load._logical_bronze_path(
        _P("a/Statement.pdf")) == _P("a/Statement.pdf")


# ============================================================
# Content-addressed parse cache + shared parse pool
# ============================================================
#
# The parse cache and coordinator replay a PDF's parsed dict on
# every later sighting so a `--force` rebuild / nightly reload
# re-parses nothing unchanged, while the inserts stay byte-identical
# (verified end-to-end by an order-independent silver diff of a
# cold vs warm rebuild). These unit tests cover the replay logic in
# isolation: synthetic parsed dicts, no real PDFs or values.

_PARSED = {
    "period_end": "2026-03-31",
    "accounts": [{
        "account_external_id": "100000001",
        "holdings": [{"description": "PLACEHOLDER FUND",
                      "quantity": 1.5, "price": None, "market_value": 3.0}],
    }],
}


def test_parse_cache_mem_and_sidecar_round_trip(tmp_path):
    cache = load.ParseCache(sidecar_dir=tmp_path / "pc")
    assert cache.get("abc", "v1") is None
    cache.put("abc", "v1", _PARSED)
    # Memory hit, and the sidecar is written under the versioned name.
    assert cache.get("abc", "v1") == _PARSED
    assert (tmp_path / "pc" / "abc.v1.json").is_file()
    # A fresh instance (mimicking a later process) reads the sidecar…
    reopened = load.ParseCache(sidecar_dir=tmp_path / "pc")
    assert reopened.get("abc", "v1") == _PARSED
    # …but a different version key is a miss (invalidation on version bump).
    assert reopened.get("abc", "v2") is None


def test_parse_cache_memory_only_when_no_sidecar():
    cache = load.ParseCache(sidecar_dir=None)
    cache.put("s", "v", {"ok": 1})
    assert cache.get("s", "v") == {"ok": 1}


def test_coordinator_resolve_parses_inline_then_caches(tmp_path):
    """Without enqueue/dispatch (a transient coordinator, as tests
    and tiny corpora use), resolve parses inline the first time and
    serves the cache thereafter — the worker runs exactly once."""
    calls = []

    def worker(arg):
        calls.append(arg)
        return {"value": arg}

    coord = load.PdfParseCoordinator(
        load.ParseCache(sidecar_dir=tmp_path / "pc"), max_workers=1)
    r1 = coord.resolve("sha1", "v1", worker, "A")
    r2 = coord.resolve("sha1", "v1", worker, "A")
    assert r1 == r2 == {"value": "A"}
    assert calls == ["A"]


def test_coordinator_never_caches_error_results(tmp_path):
    """A parse failure / signature mismatch must be re-evaluated
    every run, never pinned in the cache."""
    calls = []

    def worker(arg):
        calls.append(arg)
        return {"_error": "boom"}

    coord = load.PdfParseCoordinator(
        load.ParseCache(sidecar_dir=tmp_path / "pc"), max_workers=1)
    coord.resolve("sha1", "v1", worker, "A")
    coord.resolve("sha1", "v1", worker, "A")
    assert calls == ["A", "A"]
    assert not (tmp_path / "pc" / "sha1.v1.json").exists()


def test_coordinator_sha_for_memoizes(tmp_path, monkeypatch):
    pdf = tmp_path / "x.pdf"
    pdf.write_bytes(b"%PDF-1.4 placeholder\n")
    coord = load.PdfParseCoordinator(load.ParseCache(None), max_workers=1)
    real = load.bronze.sha256_file
    calls = []

    def counting(path, *a, **k):
        calls.append(str(path))
        return real(path, *a, **k)

    monkeypatch.setattr(load.bronze, "sha256_file", counting)
    assert coord.sha_for(pdf) == coord.sha_for(pdf)
    assert len(calls) == 1  # hashed once, memoized by path


def test_trust_parser_version_depends_on_signature_without_leaking_it():
    v_none = load._trust_parser_version(None)
    v_a = load._trust_parser_version("PLACEHOLDER HOLDER")
    v_b = load._trust_parser_version("OTHER TRUST")
    assert v_a == load._trust_parser_version("PLACEHOLDER HOLDER")  # stable
    assert len({v_none, v_a, v_b}) == 3                            # distinct
    assert "PLACEHOLDER HOLDER" not in v_a                          # no PII


def test_parser_logic_fingerprint_in_cache_namespaces():
    """Both parse-cache namespaces embed a hex parser-logic fingerprint
    (srcfp.parser_fingerprint over the parser's import closure plus the
    pdfplumber / pdfminer.six versions), so a parser edit, an imported-helper
    edit, or a library upgrade invalidates the cache. The namespaces are clean
    filename components, and the statement vs trust parsers get distinct
    fingerprints. What moves the fingerprint is covered by test_srcfp."""
    ok = set("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-")
    stmt = load._STATEMENT_PARSER_VERSION
    trust = load._TRUST_PARSER_FINGERPRINT
    assert stmt.startswith("stmt529.v") and set(stmt) <= ok
    assert trust.startswith("trust.v") and set(trust) <= ok
    assert re.search(r"\.[0-9a-f]{32}$", stmt), "statement ns ends in a hex fingerprint"
    assert re.search(r"\.[0-9a-f]{32}$", trust), "trust ns ends in a hex fingerprint"
    # distinct parser modules -> distinct fingerprints
    assert stmt.rsplit(".", 1)[-1] != trust.rsplit(".", 1)[-1]
    # the trust key folds the signature hash on top of the trust namespace
    assert load._trust_parser_version("PLACEHOLDER HOLDER").startswith(trust + ".")


def test_changed_parser_version_misses_cache(tmp_path):
    """An entry written under one parser-version namespace is not served under
    another — a parser or library change forces a re-parse rather than replaying
    stale text."""
    cache = load.ParseCache(sidecar_dir=tmp_path / "pc")
    v_old = "stmt529.v1." + "a" * 32
    v_new = "stmt529.v1." + "b" * 32
    cache.put("sha", v_old, {"ok": 1})
    assert cache.get("sha", v_old) == {"ok": 1}
    assert cache.get("sha", v_new) is None
    reopened = load.ParseCache(sidecar_dir=tmp_path / "pc")
    assert reopened.get("sha", v_new) is None
