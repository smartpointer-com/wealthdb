"""Unit tests for the Donor-Advised Fund silver loader (load._load_daf).

Synthetic bronze only — no real Fidelity charitable account numbers,
pool ids, charities, or amounts (root AGENTS.md §4). Builds a
<dump>/daf/ tree matching what download.scrape_daf emits and asserts the
rows land in the shared silver tables (portfolios kind='daf', accounts
management_style='automated', positions from pools, transactions from
grants/contributions, documents from PDFs).
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

# Synthetic ids — obviously fake, never a real DAF account/pool/grant.
DAF_ACCT = "9990001"
POOL_ID = "POOLX1"
GRANT_ID = "G1000001"
CONTRIB_ID = "C2000002"


@pytest.fixture
def migrated():
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    load.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


def _pdf(tag: str) -> bytes:
    # Distinct bytes per file so the content-sha dedup keeps them all.
    return b"%PDF-1.4\n" + tag.encode() + b"\n%%EOF\n"


def _write_daf_dump(root: Path, ts: str, *, with_docs: bool = True) -> Path:
    dump = root / ts
    acct = dump / "daf" / "acct0"
    (acct / "exports").mkdir(parents=True, exist_ok=True)
    (dump / "daf" / "accounts.json").write_text(json.dumps(
        [{"accountNbr": DAF_ACCT, "gaName": "Example Giving Account"}]))
    (acct / "account.json").write_text(json.dumps({
        "accountNbr": DAF_ACCT, "gaName": "Example Giving Account",
        "establishDate": "2014-03-07", "gaBalance": 100000.0,
    }))
    (acct / "pool_balances.json").write_text(json.dumps([{
        "poolPriceDate": "2026-09-01", "totalMarketValue": 100000.0,
        "poolInfoList": [{
            "poolId": POOL_ID, "poolName": "Example Growth Pool",
            "poolCategory": "EQUITY", "unitQuantity": 1000.0,
            "poolUnitPrice": 100.0, "marketValue": 100000.0,
            "percentage": 100.0,
        }],
    }]))
    (acct / "grants.json").write_text(json.dumps([{
        "creationDate": "2001-01-02", "status": "COMPLETE", "type": "GRANT",
        "grant": {"grantId": GRANT_ID, "amount": 20000.0,
                  "approvalDate": "2001-01-03", "charityName": "Example Charity",
                  "taxId": "00-0000000"},
    }]))
    (acct / "contributions.json").write_text(json.dumps([{
        "contributionId": CONTRIB_ID, "netProceeds": 50000.0,
        "receivedDate": "2000-12-01", "currentStatusCode": "COMPLETE",
    }]))
    # Empty event files (the common case for a fund with no such events).
    for name in ("gifts.json", "pool_exchanges.json", "adjustments.json"):
        (acct / name).write_text("[]")
    (acct / "documents_index.json").write_text("[]")
    if with_docs:
        docs = acct / "documents"
        docs.mkdir(exist_ok=True)
        (docs / "STATEMENT_2001_03_31.pdf").write_bytes(_pdf("stmt"))
        (docs / "FORM_8283_2000_12_31.pdf").write_bytes(_pdf("form8283"))
        (docs / "GRANT_2001_01_03.pdf").write_bytes(_pdf("grantconf"))
        (docs / "CONTRIBUTION_2000_12_01.pdf").write_bytes(_pdf("contribconf"))
    # A minimal run.json so load_dump's dump-run insert has a manifest.
    (dump / "run.json").write_text(json.dumps(
        {"status": "complete", "cli_config": {"mode": "all"},
     "daf_results": {"status": "complete"}}))
    return dump


def _load(migrated, dump, schema_version=7):
    load.load_dump(migrated, dump, schema_version)


def test_daf_portfolio_and_account(migrated, tmp_path):
    _load(migrated, _write_daf_dump(tmp_path, "20260101T120000Z"))
    kind = migrated.execute(
        "SELECT kind FROM portfolios WHERE portfolio_external_id = ?",
        (load.DAF_PORTFOLIO_LABEL,)).fetchone()
    assert kind == ("daf",)
    row = migrated.execute(
        "SELECT nickname, management_style, portfolio_external_id "
        "FROM accounts WHERE account_external_id = ?", (DAF_ACCT,)).fetchone()
    assert row == ("Example Giving Account", "automated",
                   load.DAF_PORTFOLIO_LABEL)


def test_daf_pools_become_positions(migrated, tmp_path):
    _load(migrated, _write_daf_dump(tmp_path, "20260101T120000Z"))
    row = migrated.execute(
        "SELECT instrument_key, description, quantity, current_value, "
        "asset_class, currency FROM positions WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchone()
    assert row == (POOL_ID, "Example Growth Pool", 1000.0, 100000.0,
                   "daf_pool", "USD")


def test_daf_grant_and_contribution_signs(migrated, tmp_path):
    _load(migrated, _write_daf_dump(tmp_path, "20260101T120000Z"))
    txns = dict(migrated.execute(
        "SELECT kind, amount FROM transactions WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchall())
    assert txns["GRANT"] == -20000.0        # outflow, signed negative
    assert txns["CONTRIBUTION"] == 50000.0  # inflow, signed positive


def test_daf_transactions_stable_across_reload(migrated, tmp_path):
    _load(migrated, _write_daf_dump(tmp_path, "20260101T120000Z"))
    ids1 = {r[0] for r in migrated.execute(
        "SELECT activity_id FROM transactions WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchall()}
    # A second dump with the same events must collapse onto the same PKs.
    _load(migrated, _write_daf_dump(tmp_path, "20260201T120000Z"))
    ids2 = {r[0] for r in migrated.execute(
        "SELECT activity_id FROM transactions WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchall()}
    assert ids1 == ids2 and len(ids1) == 2


def test_daf_documents_classified(migrated, tmp_path):
    _load(migrated, _write_daf_dump(tmp_path, "20260101T120000Z"))
    kinds = dict(migrated.execute(
        "SELECT doc_kind, COUNT(*) FROM documents "
        "WHERE account_external_id = ? GROUP BY doc_kind", (DAF_ACCT,)
    ).fetchall())
    assert kinds == {
        "daf_statement": 1, "daf_tax_form": 1,
        "daf_grant_confirmation": 1, "daf_contribution_confirmation": 1,
    }
    tax_year = migrated.execute(
        "SELECT tax_year FROM documents WHERE doc_kind = 'daf_tax_form'"
    ).fetchone()
    assert tax_year == (2000,)


def test_daf_absent_is_noop(migrated, tmp_path):
    # A dump with no daf/ dir loads cleanly and creates no DAF rows.
    dump = tmp_path / "20260101T120000Z"
    dump.mkdir()
    (dump / "run.json").write_text(json.dumps({"status": "complete"}))
    _load(migrated, dump)
    assert migrated.execute(
        "SELECT COUNT(*) FROM portfolios WHERE kind = 'daf'").fetchone() == (0,)


def test_daf_mode_dump_writes_no_retail_master(migrated, tmp_path):
    # A mode='daf' dump enumerates the retail selector but runs no
    # retail phase — its account_dimensions must NOT create retail
    # master rows (partial coverage would read downstream as the
    # accounts having emptied). The DAF's own master rows still land.
    import hashlib as _h
    retail = "100000001"
    dump = _write_daf_dump(tmp_path, "20260101T120000Z")
    run = json.loads((dump / "run.json").read_text())
    run["cli_config"] = {"mode": "daf"}
    run["accounts_enumerated"] = [retail, DAF_ACCT]
    run["accounts_in_scope"] = [retail]
    run["account_dimensions"] = {
        _h.sha256(retail.encode()).hexdigest()[:16]: {
            "in_scope": True, "nickname": "Beneficiary",
            "portfolio": "Education",
        },
    }
    (dump / "run.json").write_text(json.dumps(run))
    _load(migrated, dump)
    assert migrated.execute(
        "SELECT COUNT(*) FROM accounts WHERE account_external_id = ?",
        (retail,)).fetchone() == (0,)
    assert migrated.execute(
        "SELECT COUNT(*) FROM portfolios WHERE portfolio_external_id = "
        "'Education'").fetchone() == (0,)
    # The DAF master rows are unaffected...
    assert migrated.execute(
        "SELECT COUNT(*) FROM accounts WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchone() == (1,)
    # ...but the completeness gate drops the pool positions: a legacy
    # mode='daf' dump is partial by construction, and loading its pool
    # row would make it the source's positions anchor and zero retail.
    assert migrated.execute(
        "SELECT COUNT(*) FROM positions WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchone() == (0,)
    # Events and documents still load (keyed rows, not snapshots).
    assert migrated.execute(
        "SELECT COUNT(*) FROM transactions WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchone() == (2,)


def test_daf_skipped_below_schema_v7(migrated, tmp_path):
    # The gate keeps the DAF load off on a pre-v7 schema even if the
    # tables exist (they do in the migrated fixture).
    _load(migrated, _write_daf_dump(tmp_path, "20260101T120000Z"),
          schema_version=6)
    assert migrated.execute(
        "SELECT COUNT(*) FROM accounts WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchone() == (0,)


def _write_retail_positions(dump: Path, acct: str) -> None:
    (dump / "positions").mkdir(exist_ok=True)
    (dump / "positions" / "positions_summary.csv").write_text(
        "Account number,Account name,Symbol,Description,Quantity,"
        "Last price,Current value,Type\n"
        f"{acct},Nick,TICK1,STUB TICKER ONE,10,$10.00,$100.00,Cash\n"
    )


def test_gate_daf_failure_skips_all_positions(migrated, tmp_path):
    # Retail positions landed but the DAF phase errored (SSO bounce
    # with a DAF enumerated): loading retail alone would zero the pool
    # at this snapshot, so the gate drops ALL positions from the dump.
    retail = "100000001"
    dump = tmp_path / "20260101T120000Z"
    dump.mkdir()
    _write_retail_positions(dump, retail)
    (dump / "run.json").write_text(json.dumps({
        "status": "complete", "cli_config": {"mode": "all"},
        "daf_results": {"status": "error", "error": "SSO bounced"},
    }))
    load.load_dump(migrated, dump, 7)
    assert migrated.execute(
        "SELECT COUNT(*) FROM positions").fetchone() == (0,)


def test_gate_retail_zero_with_pool_skips_all_positions(migrated, tmp_path):
    # The inverse: the DAF pool landed but the retail export parsed to
    # zero rows while retail accounts were in scope (the 2026-07
    # header-drift failure shape). Loading the pool alone would make
    # it the source's positions anchor and zero every retail holding.
    retail = "100000001"
    dump = _write_daf_dump(tmp_path, "20260101T120000Z")
    run = json.loads((dump / "run.json").read_text())
    run["accounts_in_scope"] = [retail]
    (dump / "run.json").write_text(json.dumps(run))
    # No positions/ dir at all — the retail export produced nothing.
    load.load_dump(migrated, dump, 7)
    assert migrated.execute(
        "SELECT COUNT(*) FROM positions").fetchone() == (0,)
    # Events / documents / master still load.
    assert migrated.execute(
        "SELECT COUNT(*) FROM transactions WHERE account_external_id = ?",
        (DAF_ACCT,)).fetchone() == (2,)


def test_gate_complete_dump_loads_both_sides(migrated, tmp_path):
    # A complete observation (retail rows + pool rows in one dump)
    # loads both — the non-vacuity check for the two gate tests above.
    retail = "100000001"
    dump = _write_daf_dump(tmp_path, "20260101T120000Z")
    run = json.loads((dump / "run.json").read_text())
    run["accounts_in_scope"] = [retail]
    (dump / "run.json").write_text(json.dumps(run))
    _write_retail_positions(dump, retail)
    load.load_dump(migrated, dump, 7)
    accts = {r[0] for r in migrated.execute(
        "SELECT DISTINCT account_external_id FROM positions").fetchall()}
    assert accts == {retail, DAF_ACCT}


def test_daf_historical_from_statements(migrated, tmp_path, monkeypatch):
    # _load_daf_historical parses each STATEMENT_*.pdf, enforces the
    # reconciliation gate, and inserts period-end pool rows keyed
    # (as_of, account, description). The PDF worker is stubbed with
    # parsed dicts (the text-level parsing has its own tests); one
    # statement reconciles and loads, the other fails the gate and is
    # skipped.
    dump = _write_daf_dump(tmp_path, "20260101T120000Z")
    docs = dump / "daf" / "acct0" / "documents"
    docs.mkdir(exist_ok=True)
    (docs / "STATEMENT_2001_03_31.pdf").write_bytes(_pdf("q1"))
    (docs / "STATEMENT_2001_06_30.pdf").write_bytes(_pdf("q2"))

    parsed = {
        "STATEMENT_2001_03_31.pdf": {
            "as_of_date": "2001-03-31", "ending_value": 50000.0,
            "reconciled": True, "reconcile_error": None,
            "pools": [{"description": "Example Growth Pool",
                       "quantity": 500.0, "price": 100.0,
                       "market_value": 50000.0}],
        },
        "STATEMENT_2001_06_30.pdf": {
            "as_of_date": "2001-06-30", "ending_value": 60000.0,
            "reconciled": False, "reconcile_error": "pool sum mismatch",
            "pools": [{"description": "Example Growth Pool",
                       "quantity": 500.0, "price": 120.0,
                       "market_value": 60000.0}],
        },
    }
    monkeypatch.setattr(
        load, "_parse_daf_statement_pdf_worker",
        lambda path: parsed[Path(path).name])

    load.load_dump(migrated, dump, 7)

    rows = migrated.execute(
        "SELECT as_of_date, description, quantity, market_value, "
        "instrument_key FROM historical_position_snapshots "
        "WHERE account_external_id = ?", (DAF_ACCT,)).fetchall()
    # Only the reconciled statement loads.
    assert len(rows) == 1
    as_of, desc, qty, mv, ikey = rows[0]
    assert desc == "Example Growth Pool" and qty == 500.0 and mv == 50000.0
    # Cross-walked to the live pool's instrument key via the matching
    # positions.description written by the same dump's pool load.
    assert ikey == POOL_ID
