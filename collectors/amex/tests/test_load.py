"""Bronze → silver tests for the amex loader, on a synthetic bronze tree.

Every value here is invented from scratch: the account keys are obviously
fake hex, the ids are sequential, the merchants are placeholders and the
amounts are round. Nothing is derived from a real capture.
"""
from __future__ import annotations

import json
import logging
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from collectorkit import silver  # noqa: E402

import load  # noqa: E402

KEY_A = "0123456789ABCDEF0123456789ABCDEF"
KEY_B = "FEDCBA9876543210FEDCBA9876543210"
TOKEN_A = "AAAA1B2C3D4E5F6"


def day(y, m, d) -> int:
    return int(datetime(y, m, d, tzinfo=timezone.utc).timestamp())


# ============================================================
# Synthetic bronze
# ============================================================

def _account(key=KEY_A, token=TOKEN_A, amount="250.00", due="2026-04-05"):
    return {
        "account_key": key,
        "account_token": token,
        "display_account_number": "-01234",
        "product_display_name": "Example Card",
        "product_type": "AEXP_CARD_ACCOUNT",
        "sub_types": ["Example"],
        "line_of_business": "CONSUMER",
        "user_type": "ACCOUNT_HOLDER",
        "account_status": "Active",
        "is_partial": False,
        "balance": {"amount": amount, "currency": "USD",
                    "name": "total_balance_title"},
        "payment_due": {"date": due, "remaining_days": 12,
                        "title_key": "payment_due_title"},
    }


def _tx(n, *, amount="10.00", typ="DEBIT", status="posted", category="C1",
        post="2026-03-02", charge="2026-03-01", desc="EXAMPLE MERCHANT"):
    """One activity row, in Amex's own convention: a DEBIT is a POSITIVE
    amount charged."""
    ident = f"1000000000000000{n:02d}"
    return {
        "identifier": ident,
        "referenceNumber": ident,
        "categoryCode": category,
        "chargeDate": charge,
        "displayDate": post,
        "postDate": post,
        "statementEndDate": "2026-03-12",
        "displayDescription": desc,
        "transactionAmount": {"amount": amount, "currency": "USD"},
        "type": typ,
        "status": status,
    }


def _activity(rows, *, categories=None, balances=None,
              since="2026-02-01", until="2026-03-12"):
    return {
        "accountToken": TOKEN_A,
        "totalTransactionCount": len(rows),
        "since": since,
        "until": until,
        "categories": categories if categories is not None
                      else {"C1": "Merchandise & Supplies",
                            "C4": "Fees & Adjustments"},
        "balancesDetails": balances or {},
        "transactions": rows,
    }


def _balances(**amounts):
    standard = {k: {"amount": v, "currency": "USD"}
                for k, v in amounts.items()}
    return {"summary": {"standard": standard}}


def write_run(root: Path, slug: str, *, accounts=None, activity=None,
              statements=None, status="complete", until=None) -> Path:
    """Materialise one bronze run dir. `until` is the activity window's end,
    which is the key the run's own balance row is recorded under."""
    run = root / slug
    (run / "activity").mkdir(parents=True, exist_ok=True)
    (run / "raw").mkdir(exist_ok=True)
    accounts = accounts if accounts is not None else [_account()]
    (run / "accounts.json").write_text(json.dumps(accounts))
    for key, payload in (activity or {}).items():
        (run / "activity" / f"{key}.json").write_text(json.dumps(payload))
    for key, files in (statements or {}).items():
        d = run / "statements" / key
        d.mkdir(parents=True, exist_ok=True)
        for name, body in files.items():
            (d / name).write_bytes(body)
    manifest = {"source": "amex", "status": status}
    if until is not None:
        manifest["until"] = until
    (run / "run.json").write_text(json.dumps(manifest))
    return run


@pytest.fixture()
def db(tmp_path):
    conn = silver.open_db(tmp_path / "amex.db")
    load.apply_migrations(conn, load.MIGRATIONS_DIR)
    yield conn
    conn.close()


# ============================================================
# Parsing helpers
# ============================================================

@pytest.mark.parametrize("raw,expected", [
    ("2026-03-02", day(2026, 3, 2)),
    ("03/02/2026", day(2026, 3, 2)),
    ("2026-03-02T00:00:00Z", day(2026, 3, 2)),
    ("", None),
    (None, None),
    ("not a date", None),
])
def test_parse_date(raw, expected):
    assert load.parse_date(raw) == expected


@pytest.mark.parametrize("raw,expected", [
    ("10.00", 10.0), ("1,234.56", 1234.56), ("$12.00", 12.0),
    ("(5.00)", -5.0), (7, 7.0), ("", None), (None, None), ("x", None),
])
def test_parse_money(raw, expected):
    assert load.parse_money(raw) == expected


# ============================================================
# The sign flip — the one transformation that would be silently
# catastrophic to get wrong.
# ============================================================

def test_a_debit_becomes_negative():
    # Amex states a purchase as a POSITIVE amount charged; the fleet's silver
    # card convention is spend NEGATIVE.
    assert load.signed_amount(_tx(1, amount="10.00", typ="DEBIT")) == -10.0


def test_a_credit_becomes_positive():
    assert load.signed_amount(_tx(2, amount="-30.00", typ="CREDIT")) == 30.0


def test_the_type_wins_over_a_disagreeing_raw_sign():
    # The magnitude comes from the amount, the direction from `type`, so a
    # wire variant that drops the sign still lands the right way round.
    assert load.signed_amount(_tx(3, amount="-10.00", typ="DEBIT")) == -10.0
    assert load.signed_amount(_tx(4, amount="30.00", typ="CREDIT")) == 30.0


def test_without_a_type_the_raw_sign_is_negated():
    row = _tx(5, amount="10.00")
    del row["type"]
    assert load.signed_amount(row) == -10.0


def test_an_unparseable_amount_is_none():
    assert load.signed_amount({"transactionAmount": {"amount": "x"}}) is None


def test_the_provider_amount_is_kept_for_audit():
    rows = load.activity_rows(_activity([_tx(1, amount="10.00")]))
    assert rows[0]["amount"] == -10.0
    assert rows[0]["payload"]["provider_amount"] == "10.00"


# ============================================================
# activity_rows
# ============================================================

def test_rows_carry_both_dates_and_the_resolved_category():
    rows = load.activity_rows(_activity([_tx(1)]))
    assert len(rows) == 1
    row = rows[0]
    assert row["posted_at"] == day(2026, 3, 2)
    assert row["txn_date"] == day(2026, 3, 1)
    assert row["statement_end_at"] == day(2026, 3, 12)
    # Resolved through the payload's own map, so a category the provider adds
    # later lands as its label rather than an unmapped code.
    assert row["category"] == "Merchandise & Supplies"
    assert row["category_code"] == "C1"


def test_an_unmapped_category_code_keeps_the_code_and_no_label():
    rows = load.activity_rows(_activity([_tx(1, category="C9")]))
    assert rows[0]["category_code"] == "C9"
    assert rows[0]["category"] is None


def test_a_row_with_no_category_lands_blank():
    # A bill payment: Amex leaves it uncategorised, which is what the gold
    # adapter reads to tell a payment from a refund.
    rows = load.activity_rows(_activity([_tx(1, category="")]))
    assert rows[0]["category"] is None and rows[0]["category_code"] is None


def test_rows_without_a_date_or_amount_are_skipped():
    bad_date = _tx(1)
    bad_date["postDate"] = bad_date["displayDate"] = "nope"
    bad_amt = _tx(2)
    bad_amt["transactionAmount"] = {"amount": "x"}
    no_id = _tx(3)
    no_id["identifier"] = no_id["referenceNumber"] = ""
    rows = load.activity_rows(_activity([bad_date, bad_amt, no_id, _tx(4)]))
    assert [r["txn_id"] for r in rows] == ["100000000000000004"]


def test_pending_rows_are_flagged():
    rows = load.activity_rows(_activity([_tx(1), _tx(2, status="pending")]))
    assert [r["is_pending"] for r in rows] == [0, 1]


# ============================================================
# End-to-end load
# ============================================================

def test_a_run_loads_accounts_and_transactions(db, tmp_path):
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    activity={KEY_A: _activity([_tx(1), _tx(2)])})
    assert load.load_run(db, run) is True
    accounts = [tuple(r) for r in db.execute(
        "SELECT account_external_id, account_token, display_name, mask, "
        "currency, balance, payment_due_at FROM accounts")]
    assert accounts == [(KEY_A, TOKEN_A, "Example Card", "-01234", "USD",
                         250.0, day(2026, 4, 5))]
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 2


def test_reloading_the_same_run_is_a_no_op(db, tmp_path):
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    activity={KEY_A: _activity([_tx(1)])})
    assert load.load_run(db, run) is True
    assert load.load_run(db, run) is False
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 1


def test_a_second_run_adds_only_new_rows(db, tmp_path):
    root = tmp_path / "bronze"
    first = write_run(root, "20260312T000000Z",
                      activity={KEY_A: _activity([_tx(1), _tx(2)])})
    second = write_run(root, "20260413T000000Z",
                       activity={KEY_A: _activity([_tx(2), _tx(3)])})
    load.load_run(db, first)
    load.load_run(db, second)
    ids = [r[0] for r in db.execute(
        "SELECT txn_id FROM transactions ORDER BY txn_id")]
    assert ids == ["100000000000000001", "100000000000000002",
                   "100000000000000003"]


def test_an_incomplete_run_is_skipped(db, tmp_path):
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    activity={KEY_A: _activity([_tx(1)])},
                    status="in-progress")
    assert load.load_run(db, run) is False
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 0


def test_an_unchanged_roster_does_not_add_a_snapshot(db, tmp_path):
    root = tmp_path / "bronze"
    load.load_run(db, write_run(root, "20260312T000000Z",
                                activity={KEY_A: _activity([_tx(1)])}))
    load.load_run(db, write_run(root, "20260413T000000Z",
                                activity={KEY_A: _activity([_tx(1)])}))
    assert db.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 1


def test_a_changed_balance_adds_a_snapshot(db, tmp_path):
    root = tmp_path / "bronze"
    load.load_run(db, write_run(root, "20260312T000000Z",
                                accounts=[_account(amount="250.00")],
                                activity={KEY_A: _activity([_tx(1)])}))
    load.load_run(db, write_run(root, "20260413T000000Z",
                                accounts=[_account(amount="300.00")],
                                activity={KEY_A: _activity([_tx(1)])}))
    balances = [r[0] for r in db.execute(
        "SELECT balance FROM accounts ORDER BY snapshot_at")]
    assert balances == [250.0, 300.0]


def test_two_accounts_stay_separate(db, tmp_path):
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    accounts=[_account(), _account(key=KEY_B,
                                                   token="BBBB1B2C3D4E5F6")],
                    activity={KEY_A: _activity([_tx(1)]),
                              KEY_B: _activity([_tx(2)])})
    load.load_run(db, run)
    rows = dict(tuple(r) for r in
                db.execute("SELECT account_external_id, COUNT(*) "
                           "FROM transactions GROUP BY 1"))
    assert rows == {KEY_A: 1, KEY_B: 1}


# ============================================================
# Pending rows are replaced, never accumulated
# ============================================================

def test_a_pending_row_that_posts_leaves_no_twin(db, tmp_path):
    # The provisional pending id is replaced by a permanent one when the
    # charge posts; accumulating pending rows would leave a stale twin.
    root = tmp_path / "bronze"
    provisional = _tx(1, status="pending")
    provisional["identifier"] = provisional["referenceNumber"] = "P0001ABCDEF"
    load.load_run(db, write_run(root, "20260312T000000Z",
                                activity={KEY_A: _activity([provisional])}))
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 1
    load.load_run(db, write_run(root, "20260413T000000Z",
                                activity={KEY_A: _activity([_tx(1)])}))
    rows = [tuple(r) for r in
            db.execute("SELECT txn_id, is_pending FROM transactions")]
    assert rows == [("100000000000000001", 0)]


def test_a_still_pending_row_survives_the_replace(db, tmp_path):
    root = tmp_path / "bronze"
    pending = _tx(9, status="pending")
    load.load_run(db, write_run(root, "20260312T000000Z",
                                activity={KEY_A: _activity([pending])}))
    load.load_run(db, write_run(root, "20260413T000000Z",
                                activity={KEY_A: _activity([pending])}))
    assert db.execute("SELECT COUNT(*) FROM transactions "
                      "WHERE is_pending=1").fetchone()[0] == 1


def test_the_replace_is_scoped_to_one_account(db, tmp_path):
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    accounts=[_account(), _account(key=KEY_B,
                                                   token="BBBB1B2C3D4E5F6")],
                    activity={KEY_A: _activity([_tx(1, status="pending")]),
                              KEY_B: _activity([_tx(2, status="pending")])})
    load.load_run(db, run)
    assert db.execute("SELECT COUNT(*) FROM transactions "
                      "WHERE is_pending=1").fetchone()[0] == 2


# ============================================================
# Statement balances and the pending total
# ============================================================

def test_the_cycle_balance_block_lands(db, tmp_path):
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    activity={KEY_A: _activity(
                        [_tx(1)],
                        balances=_balances(previousBalance="100.00",
                                           newCharges="60.00",
                                           paymentsAndCredits="-10.00",
                                           fees="0.00",
                                           interestCharges="0.00",
                                           statementBalance="150.00"))})
    load.load_run(db, run)
    row = tuple(db.execute(
        "SELECT period_start, period_end, opening, closing, new_charges, "
        "payments_and_credits, source FROM statement_balances").fetchone())
    assert row == (day(2026, 2, 1), day(2026, 3, 12), 100.0, 150.0, 60.0,
                   -10.0, "activity")


def test_a_total_balance_stands_in_for_a_statement_balance(db, tmp_path):
    # Which figure the block states is view-dependent; a rolling view names
    # the total rather than the statement balance.
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    activity={KEY_A: _activity(
                        [_tx(1)], balances=_balances(totalBalance="175.00"))})
    load.load_run(db, run)
    assert db.execute(
        "SELECT closing FROM statement_balances").fetchone()[0] == 175.0


def test_no_balance_block_records_no_period(db, tmp_path):
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    activity={KEY_A: _activity([_tx(1)])})
    load.load_run(db, run)
    assert db.execute(
        "SELECT COUNT(*) FROM statement_balances").fetchone()[0] == 0


def test_the_pending_total_reaches_the_account_row(db, tmp_path):
    balances = {"pendingBalances": {"charges": {"amount": "42.00",
                                                "currency": "USD"}}}
    run = write_run(tmp_path / "bronze", "20260312T000000Z",
                    activity={KEY_A: _activity([_tx(1)], balances=balances)})
    load.load_run(db, run)
    assert db.execute(
        "SELECT pending_charges FROM accounts").fetchone()[0] == 42.0


# ============================================================
# Documents
# ============================================================

def test_statement_pdfs_are_inventoried_and_deduped(db, tmp_path):
    root = tmp_path / "bronze"
    files = {"2026-03-12.pdf": b"%PDF-1.4 synthetic statement\n",
             "yes-2025.pdf": b"%PDF-1.4 synthetic summary\n"}
    load.load_run(db, write_run(root, "20260312T000000Z",
                                activity={KEY_A: _activity([_tx(1)])},
                                statements={KEY_A: files}))
    load.load_run(db, write_run(root, "20260413T000000Z",
                                activity={KEY_A: _activity([_tx(1)])},
                                statements={KEY_A: files}))
    rows = [tuple(r) for r in
            db.execute("SELECT doc_kind, doc_date, filename FROM documents "
                       "ORDER BY filename")]
    # The same bytes across two runs collapse to one row each.
    assert rows == [("statement", day(2026, 3, 12), "2026-03-12.pdf"),
                    ("year_end_summary", None, "yes-2025.pdf")]


@pytest.mark.parametrize("name,expected", [
    ("2026-03-12.pdf", day(2026, 3, 12)),
    ("yes-2025.pdf", None),          # a year, not a day — never fabricated
    ("statement.pdf", None),
])
def test_document_date(name, expected):
    assert load.document_date(name) == expected


# ============================================================
# --force and migrations
# ============================================================

def test_force_rebuilds_from_bronze(tmp_path):
    root = tmp_path / "bronze"
    write_run(root, "20260312T000000Z",
              activity={KEY_A: _activity([_tx(1)])})
    db_path = tmp_path / "amex.db"
    assert load.main(["--bronze-dir", str(root),
                      "--silver-db", str(db_path)]) == 0
    assert load.main(["--bronze-dir", str(root), "--silver-db", str(db_path),
                      "--force"]) == 0
    conn = silver.open_db(db_path)
    try:
        assert conn.execute(
            "SELECT COUNT(*) FROM transactions").fetchone()[0] == 1
        assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1
    finally:
        conn.close()


def test_migrations_are_idempotent(db):
    before = silver.current_schema_version(db)
    load.apply_migrations(db, load.MIGRATIONS_DIR)
    assert silver.current_schema_version(db) == before


# ============================================================
# The statement backfill — the deep era below the activity seam
# ============================================================

def _parsed(end, rows, *, previous="100.00", new="120.00"):
    """A ParsedCardStatement built directly, so the backfill tests exercise
    the loader rather than re-testing the PDF parser."""
    import statement_parser as sp
    from decimal import Decimal
    p = sp.ParsedCardStatement(
        period_end=end,
        previous_balance=Decimal(previous),
        payments_credits=Decimal("0.00"),
        new_charges=Decimal("0.00"),
        fees=Decimal("0.00"),
        interest_charged=Decimal("0.00"),
        new_balance=Decimal(new))
    for when, amount, kind in rows:
        p.transactions.append(sp.StatementTxn(
            when=when, description="EXAMPLE STORE", amount=Decimal(amount),
            kind=kind))
    return p


def _stub_statements(monkeypatch, by_name):
    """Make the loader's PDF parse return a scripted result per filename, so
    the tests need no PDFs."""
    import statement_parser as sp
    monkeypatch.setattr(sp, "parse_card_statement_pdf",
                        lambda path: by_name[Path(path).name])
    monkeypatch.setattr(sp, "rows_reconcile", lambda p: p.period_end is not None)


def load_all(db, root: Path):
    """Ingest every run under `root`, then rebuild the statement-derived rows
    — which is what `load` itself does, and what the seam depends on."""
    loaded = 0
    for run_dir in sorted(p for p in root.iterdir() if p.is_dir()):
        if load.load_run(db, run_dir):
            loaded += 1
    if loaded:
        load.rebuild_statements(db, root)
        db.commit()
    return loaded


def _write_pdfs(run: Path, key: str, names) -> None:
    d = run / "statements" / key
    d.mkdir(parents=True, exist_ok=True)
    for n in names:
        (d / n).write_bytes(b"%PDF-1.4 synthetic\n")


def test_statement_rows_flip_the_document_sign(monkeypatch):
    import datetime
    rows = load.statement_rows(KEY_A, _parsed(
        datetime.date(2026, 1, 12),
        [(datetime.date(2026, 1, 5), "60.00", "STMT_PURCHASE"),
         (datetime.date(2026, 1, 8), "-40.00", "STMT_PAYMENT")]))
    # The document signs a charge positive; silver's convention is the
    # inverse, and the provider's own figure is kept for audit.
    assert [r["amount"] for r in rows] == [-60.0, 40.0]
    assert rows[0]["payload"]["provider_amount"] == "60.00"
    assert rows[0]["source"] == "statement"


def test_statement_row_ids_are_deterministic():
    import datetime
    args = (KEY_A, _parsed(datetime.date(2026, 1, 12),
                           [(datetime.date(2026, 1, 5), "60.00",
                             "STMT_PURCHASE")]))
    first = load.statement_rows(*args)
    second = load.statement_rows(*args)
    assert first[0]["txn_id"] == second[0]["txn_id"]
    assert first[0]["txn_id"].startswith(f"stmt:{KEY_A}:2026-01-12:")


def test_a_statement_row_dates_itself_by_transaction_date():
    import datetime
    rows = load.statement_rows(KEY_A, _parsed(
        datetime.date(2026, 1, 12),
        [(datetime.date(2026, 1, 5), "60.00", "STMT_PURCHASE")]))
    # The document states no post date at all, and the payload says so.
    assert rows[0]["posted_at"] == rows[0]["txn_date"] == day(2026, 1, 5)
    assert rows[0]["payload"]["posted_at_basis"] == "transaction_date"


def test_only_periods_below_the_seam_are_imported(db, tmp_path, monkeypatch):
    import datetime
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity(
                        [_tx(1, post="2026-02-10", charge="2026-02-09")])})
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf", "2026-02-12.pdf"])
    _stub_statements(monkeypatch, {
        # Wholly below the seam (2026-02-10) — imported.
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12),
                                  [(datetime.date(2025, 12, 3), "30.00",
                                    "STMT_PURCHASE")]),
        # Ends after the seam — its rows are given up rather than
        # double-counted against the activity's copy of them.
        "2026-02-12.pdf": _parsed(datetime.date(2026, 2, 12),
                                  [(datetime.date(2026, 2, 9), "60.00",
                                    "STMT_PURCHASE")]),
    })
    load_all(db, root)

    sources = dict(tuple(r) for r in db.execute(
        "SELECT source, COUNT(*) FROM transactions GROUP BY 1"))
    assert sources == {"activity": 1, "statement": 1}
    covered = dict(tuple(r) for r in db.execute(
        "SELECT period_end, transactions_covered FROM statement_balances"))
    assert covered[day(2025, 12, 12)] == 1
    # The straddling period keeps its anchors and says its rows are absent.
    assert covered[day(2026, 2, 12)] == 0


def test_a_period_inside_the_activity_reach_is_flagged_covered(db, tmp_path,
                                                               monkeypatch):
    # Nothing is imported from either period — both end after the seam — but
    # the activity channel carries every row of the one that lies wholly
    # INSIDE its reach, so its anchors are not left looking rowless.
    import datetime
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity(
                        [_tx(1, post="2025-11-01", charge="2025-11-01"),
                         _tx(2, post="2026-03-01", charge="2026-03-01")])})
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf", "2026-02-12.pdf"])
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12),
                                  [(datetime.date(2025, 12, 3), "30.00",
                                    "STMT_PURCHASE")]),
        "2026-02-12.pdf": _parsed(datetime.date(2026, 2, 12),
                                  [(datetime.date(2026, 1, 9), "60.00",
                                    "STMT_PURCHASE")]),
    })
    load_all(db, root)

    covered = dict(tuple(r) for r in db.execute(
        "SELECT period_end, transactions_covered FROM statement_balances"))
    assert covered[day(2026, 2, 12)] == 1
    # The oldest period has no predecessor to chain a start from, so it
    # cannot be shown to lie above the seam and stays uncovered.
    assert covered[day(2025, 12, 12)] == 0
    assert not db.execute(
        "SELECT 1 FROM transactions WHERE source='statement'").fetchall()


def test_a_period_past_the_activity_reach_is_not_flagged_covered(
        db, tmp_path, monkeypatch):
    # No activity row posts at or after the period end, so the activity
    # cannot be shown to carry it. It is above the seam but nothing holds
    # its rows, and claiming coverage would hide a real gap between two
    # anchors.
    import datetime
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity(
                        [_tx(1, post="2025-11-01", charge="2025-11-01"),
                         _tx(2, post="2025-12-20", charge="2025-12-20")])})
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf", "2026-02-12.pdf"])
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12),
                                  [(datetime.date(2025, 12, 3), "30.00",
                                    "STMT_PURCHASE")]),
        "2026-02-12.pdf": _parsed(datetime.date(2026, 2, 12),
                                  [(datetime.date(2026, 1, 9), "60.00",
                                    "STMT_PURCHASE")]),
    })
    load_all(db, root)

    covered = dict(tuple(r) for r in db.execute(
        "SELECT period_end, transactions_covered FROM statement_balances"))
    assert covered[day(2026, 2, 12)] == 0


def test_balances_are_recorded_for_every_reconciling_period(db, tmp_path,
                                                            monkeypatch):
    import datetime
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf", "2026-02-12.pdf"])
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12), [],
                                  previous="80.00", new="100.00"),
        "2026-02-12.pdf": _parsed(datetime.date(2026, 2, 12), [],
                                  previous="100.00", new="160.00"),
    })
    load_all(db, root)
    rows = [tuple(r) for r in db.execute(
        "SELECT period_start, period_end, opening, closing, source "
        "FROM statement_balances ORDER BY period_end")]
    # Even the period the activity already covers keeps its anchors: they are
    # the only historic balance a card has.
    assert rows == [
        (day(2025, 12, 12), day(2025, 12, 12), 80.0, 100.0, "statement"),
        (day(2025, 12, 13), day(2026, 2, 12), 100.0, 160.0, "statement"),
    ]


def test_a_statement_that_does_not_reconcile_is_skipped(db, tmp_path,
                                                        monkeypatch):
    import datetime
    import statement_parser as sp
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf"])
    monkeypatch.setattr(sp, "parse_card_statement_pdf",
                        lambda path: _parsed(datetime.date(2025, 12, 12),
                                             [(datetime.date(2025, 12, 3),
                                               "30.00", "STMT_PURCHASE")]))
    monkeypatch.setattr(sp, "rows_reconcile", lambda p: False)
    load_all(db, root)
    # Not its rows, and not even its balances.
    assert db.execute("SELECT COUNT(*) FROM transactions "
                      "WHERE source='statement'").fetchone()[0] == 0
    assert db.execute(
        "SELECT COUNT(*) FROM statement_balances").fetchone()[0] == 0


def test_with_no_activity_loaded_the_rows_are_deferred(db, tmp_path,
                                                       monkeypatch):
    import datetime
    root = tmp_path / "bronze"
    # A run with statements but no activity at all: there is no seam to bound
    # against, so importing would put a statement copy of every row beside the
    # copy the first activity fetch lands.
    run = write_run(root, "20260315T120000Z")
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf"])
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12),
                                  [(datetime.date(2025, 12, 3), "30.00",
                                    "STMT_PURCHASE")]),
    })
    load_all(db, root)
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 0
    # The anchors still land.
    assert db.execute(
        "SELECT COUNT(*) FROM statement_balances").fetchone()[0] == 1


def test_the_seam_is_anchored_to_activity_rows_only(db, tmp_path):
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity(
                        [_tx(1, post="2026-02-10")])})
    load.load_run(db, run)
    assert load.export_seam(db, KEY_A) == day(2026, 2, 10)
    # A statement-sourced row must not move the seam, or it would drift as
    # statements are added and the two channels would stop being disjoint.
    db.execute("INSERT INTO transactions (txn_id, posted_at, "
               "account_external_id, amount, source, payload) "
               "VALUES ('stmt:x', ?, ?, -1.0, 'statement', '{}')",
               (day(2020, 1, 1), KEY_A))
    assert load.export_seam(db, KEY_A) == day(2026, 2, 10)


def test_a_deeper_window_withdraws_rows_the_activity_now_covers(db, tmp_path,
                                                                monkeypatch):
    """The regression the first live load found (DESIGN.md §M).

    The seam MOVES: a later run with a wider --lookback reaches further back,
    so a period that was below the seam is covered by the activity next time.
    An incremental import would leave both copies; the rebuild withdraws the
    statement's."""
    import datetime
    root = tmp_path / "bronze"
    # Run 1: a narrow window. Only 2026-02 activity, so the statement period
    # ending 2025-12-12 is below the seam and its rows belong in silver.
    run1 = write_run(root, "20260301T000000Z",
                     activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _write_pdfs(run1, KEY_A, ["2025-12-12.pdf"])
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12),
                                  [(datetime.date(2025, 12, 3), "30.00",
                                    "STMT_PURCHASE")]),
    })
    load_all(db, root)
    assert db.execute("SELECT COUNT(*) FROM transactions "
                      "WHERE source='statement'").fetchone()[0] == 1

    # Run 2: a wider window whose activity now reaches back past that period.
    run2 = write_run(root, "20260401T000000Z",
                     activity={KEY_A: _activity([_tx(2, post="2025-11-01")])})
    _write_pdfs(run2, KEY_A, ["2025-12-12.pdf"])
    load_all(db, root)
    seam = load.export_seam(db, KEY_A)
    assert seam == day(2025, 11, 1)
    overlapping = db.execute(
        "SELECT COUNT(*) FROM transactions WHERE source='statement' "
        "AND posted_at >= ?", (seam,)).fetchone()[0]
    assert overlapping == 0, "a statement row survived into the activity era"
    # And the period now says its rows are not carried here.
    assert db.execute("SELECT transactions_covered FROM statement_balances "
                      "WHERE period_end=?", (day(2025, 12, 12),)
                      ).fetchone()[0] == 0


def test_a_period_is_parsed_once_however_many_runs_hold_it(db, tmp_path,
                                                           monkeypatch):
    # Every run re-fetches the same statements, so a tree with N runs holds N
    # copies of each period — and each parse costs a pdftotext subprocess.
    import datetime
    import statement_parser as sp
    root = tmp_path / "bronze"
    parses = []
    for slug in ("20260301T000000Z", "20260401T000000Z", "20260501T000000Z"):
        run = write_run(root, slug,
                        activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
        _write_pdfs(run, KEY_A, ["2025-12-12.pdf"])
    monkeypatch.setattr(sp, "rows_reconcile", lambda p: True)
    monkeypatch.setattr(sp, "parse_card_statement_pdf",
                        lambda path: (parses.append(path),
                                      _parsed(datetime.date(2025, 12, 12),
                                              []))[1])
    load_all(db, root)
    assert len(parses) == 1, f"parsed {len(parses)} copies of one period"
    # ...and it is the newest run's copy, not whichever came first.
    assert Path(parses[0]).parents[2].name == "20260501T000000Z"


def test_the_rebuild_is_idempotent(db, tmp_path, monkeypatch):
    import datetime
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf"])
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12),
                                  [(datetime.date(2025, 12, 3), "30.00",
                                    "STMT_PURCHASE")]),
    })
    load_all(db, root)
    before = db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    load.rebuild_statements(db, root)
    db.commit()
    assert db.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == before


def test_a_year_end_summary_is_never_parsed_as_a_statement(db, tmp_path,
                                                           monkeypatch):
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _write_pdfs(run, KEY_A, ["yes-2025.pdf"])
    import statement_parser as sp
    monkeypatch.setattr(sp, "parse_card_statement_pdf",
                        lambda path: pytest.fail("a summary was parsed"))
    load_all(db, root)
    # It is still inventoried as a document.
    assert db.execute("SELECT doc_kind FROM documents").fetchone()[0] == \
        "year_end_summary"


def test_the_loader_stem_helpers_match_the_download_side_originals():
    # load.py copies them rather than importing download.py (which would drag
    # Playwright into a loader that never opens a browser), so nothing but
    # this pins the two to the same answer.
    import download
    for raw in ["0123ABC", "2026-03-12", "../../etc/passwd", "a/b",
                ".hidden", "", "///", "0123456789ABCDEF0123456789ABCDEF"]:
        assert load._safe_stem(raw) == download.safe_stem(raw), raw
        assert load._log_id(raw) == download.log_id(raw), raw


# ============================================================
# One bad re-render must not erase a period an older run read cleanly
# ============================================================

def _pdf_run(root: Path, slug: str, name: str, body: bytes) -> Path:
    """A complete run holding one statement PDF with the given bytes."""
    run = write_run(root, slug,
                    activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    d = run / "statements" / KEY_A
    d.mkdir(parents=True, exist_ok=True)
    (d / name).write_bytes(body)
    return run


def test_an_older_copy_stands_in_when_the_newest_one_is_refused(
        db, tmp_path, monkeypatch, caplog):
    # Every run re-fetches the same statements, and one render the gates
    # refuse (the accessible-PDF variant carries no readable summary) would
    # otherwise take the period's rows and its closing anchor with it.
    import datetime
    import statement_parser as sp
    root = tmp_path / "bronze"
    _pdf_run(root, "20260301T000000Z", "2025-12-12.pdf", b"%PDF good\n")
    _pdf_run(root, "20260401T000000Z", "2025-12-12.pdf", b"%PDF unreadable\n")

    good = _parsed(datetime.date(2025, 12, 12),
                   [(datetime.date(2025, 12, 3), "30.00", "STMT_PURCHASE")])
    refused = _parsed(datetime.date(2025, 12, 12), [])
    monkeypatch.setattr(
        sp, "parse_card_statement_pdf",
        lambda path: good if b"good" in Path(path).read_bytes() else refused)
    monkeypatch.setattr(sp, "rows_reconcile", lambda p: p is good)

    with caplog.at_level(logging.WARNING, logger="amex.load"):
        load_all(db, root)
    assert db.execute("SELECT COUNT(*) FROM statement_balances "
                      "WHERE source='statement'").fetchone()[0] == 1
    assert db.execute("SELECT COUNT(*) FROM transactions "
                      "WHERE source='statement'").fetchone()[0] == 1
    assert "stood in" in caplog.text


def test_a_statement_anchor_is_stamped_with_the_run_that_holds_it(
        db, tmp_path, monkeypatch):
    # snapshot_at names the bronze run the surviving copy came from, which is
    # not the newest run loaded — a routine 90-day run holds no deep-era PDF.
    import datetime
    root = tmp_path / "bronze"
    _pdf_run(root, "20260301T000000Z", "2025-12-12.pdf",
             b"%PDF-1.4 synthetic\n")
    write_run(root, "20260501T000000Z",
              activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(datetime.date(2025, 12, 12), []),
    })
    load_all(db, root)
    stamped = db.execute("SELECT snapshot_at FROM statement_balances "
                         "WHERE source='statement'").fetchone()[0]
    assert stamped == load.bronze.parse_run_ts("20260301T000000Z")


# ============================================================
# A shared period_end: the statement wins the day, and gives it back
# ============================================================

def test_a_refused_reparse_restores_the_activity_row_it_shadowed(
        db, tmp_path, monkeypatch):
    # `statement_balances` is keyed (account, period_end), so a statement
    # closing on the same day a run's own activity window ended replaces
    # that run's row. The rebuild deletes the replacement on every load and
    # the run is never replayed, so without a re-derive the day would end
    # up with no mark at all.
    import datetime
    import statement_parser as sp
    root = tmp_path / "bronze"
    run = write_run(root, "20260212T120000Z", until="2026-02-12",
                    activity={KEY_A: _activity(
                        [_tx(1, post="2026-02-10")],
                        since="2026-01-01", until="2026-02-12",
                        balances=_balances(totalBalance="150.00"))})
    _write_pdfs(run, KEY_A, ["2026-02-12.pdf"])
    monkeypatch.setattr(sp, "parse_card_statement_pdf",
                        lambda path: _parsed(datetime.date(2026, 2, 12), [],
                                             previous="100.00", new="160.00"))
    monkeypatch.setattr(sp, "rows_reconcile", lambda p: True)
    load_all(db, root)
    assert [tuple(r) for r in db.execute(
        "SELECT source, closing FROM statement_balances")] == [
            ("statement", 160.0)]

    # A later rebuild that refuses the same period gives the day back to the
    # channel that had it.
    monkeypatch.setattr(sp, "rows_reconcile", lambda p: False)
    load.rebuild_statements(db, root)
    assert [tuple(r) for r in db.execute(
        "SELECT period_end, closing, source FROM statement_balances")] == [
            (day(2026, 2, 12), 150.0, "activity")]


# ============================================================
# A parse-tooling fault is not a rebuild
# ============================================================

def test_the_loader_refuses_to_run_without_pdftotext(tmp_path, monkeypatch):
    # The rebuild deletes the deep era and re-imports what parses, so a
    # missing parser would empty it rather than skip it. Nothing is written,
    # nothing is recorded, and the next invocation retries cleanly.
    root = tmp_path / "bronze"
    write_run(root, "20260315T120000Z",
              activity={KEY_A: _activity([_tx(1)])})
    monkeypatch.setattr(load.shutil, "which", lambda name: None)
    db_path = tmp_path / "amex.db"
    assert load.main(["--bronze-dir", str(root),
                      "--silver-db", str(db_path)]) == 1
    assert not db_path.exists()


def test_a_parser_that_fails_on_everything_leaves_the_deep_era_standing(
        db, tmp_path, monkeypatch):
    # pdftotext present but broken: every document raises and none parses.
    # That shape is a tooling fault, not bronze losing its statements, and
    # rebuilding on it would drop years of rows and anchors for nothing.
    import datetime
    import statement_parser as sp
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _write_pdfs(run, KEY_A, ["2025-12-12.pdf"])
    _stub_statements(monkeypatch, {
        "2025-12-12.pdf": _parsed(
            datetime.date(2025, 12, 12),
            [(datetime.date(2025, 12, 3), "30.00", "STMT_PURCHASE")]),
    })
    load_all(db, root)
    counted = "SELECT (SELECT COUNT(*) FROM transactions WHERE source=?), " \
              "(SELECT COUNT(*) FROM statement_balances WHERE source=?)"
    before = tuple(db.execute(counted, ("statement", "statement")).fetchone())
    assert before == (1, 1)

    def _explode(path):
        raise OSError("pdftotext: cannot execute")
    monkeypatch.setattr(sp, "parse_card_statement_pdf", _explode)
    with pytest.raises(RuntimeError):
        load.rebuild_statements(db, root)
    assert tuple(db.execute(counted,
                            ("statement", "statement")).fetchone()) == before


def test_one_unreadable_document_still_lets_the_others_land(db, tmp_path,
                                                            monkeypatch):
    # A single bad PDF stays a per-document skip; only a total failure is
    # read as a tooling fault.
    import datetime
    import statement_parser as sp
    root = tmp_path / "bronze"
    run = write_run(root, "20260315T120000Z",
                    activity={KEY_A: _activity([_tx(1, post="2026-02-10")])})
    _write_pdfs(run, KEY_A, ["2025-10-12.pdf", "2025-11-12.pdf",
                             "2025-12-12.pdf"])
    ok = {name: _parsed(datetime.date(2025, month, 12), [])
          for name, month in (("2025-10-12.pdf", 10), ("2025-12-12.pdf", 12))}

    def _parse(path):
        name = Path(path).name
        if name == "2025-11-12.pdf":
            raise subprocess.CalledProcessError(1, "pdftotext")
        return ok[name]
    monkeypatch.setattr(sp, "parse_card_statement_pdf", _parse)
    monkeypatch.setattr(sp, "rows_reconcile", lambda p: True)
    load_all(db, root)
    assert [r[0] for r in db.execute(
        "SELECT period_end FROM statement_balances WHERE source='statement' "
        "ORDER BY period_end")] == [day(2025, 10, 12), day(2025, 12, 12)]


# ============================================================
# A window the source honoured only in part
# ============================================================

def test_a_short_activity_payload_is_flagged_at_load_time(db, tmp_path,
                                                          caplog):
    # Covers runs already on disk, whose manifests predate the download
    # side's own coverage block.
    root = tmp_path / "bronze"
    payload = _activity([_tx(1)])
    payload["totalTransactionCount"] = 500
    write_run(root, "20260315T120000Z", activity={KEY_A: payload})
    with caplog.at_level(logging.WARNING, logger="amex.load"):
        load_all(db, root)
    assert "came back short" in caplog.text
    # Still ingested: what landed is real, and silver stays additive.
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 1


def test_a_row_the_ledger_cannot_key_is_not_a_short_fetch(db, tmp_path,
                                                          caplog):
    # The projection also drops a row with no identifier, no amount or no
    # post date. Counting those against the source's own total would report
    # a pagination gap that never happened.
    root = tmp_path / "bronze"
    unkeyed = _tx(2)
    unkeyed["identifier"] = unkeyed["referenceNumber"] = ""
    write_run(root, "20260315T120000Z",
              activity={KEY_A: _activity([_tx(1), unkeyed])})
    with caplog.at_level(logging.WARNING, logger="amex.load"):
        load_all(db, root)
    assert "came back short" not in caplog.text
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 1
