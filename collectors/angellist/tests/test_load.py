"""Unit tests for load.py (event-sourced model, schema v7).

Synthetic bronze (captures.jsonl) + synthetic K-1 CSV → SQLite silver. Covers:
  * migrations apply (schema_meta v7)
  * GraphQL -> offerings (immutable identity) + vehicles + kind
  * position_snapshots: 'investment' (cost, at investment date) +
    'valuation' (FMV at the portfolio data date) events, with the
    collector-computed market_value_minor + valuation_basis
  * portfolio_summary + portfolio_timeseries + commitments
  * K-1 money parsing + CSV -> k1_capital_accounts / tax_documents
  * K-1 'statement' events fed into position_snapshots; SPV identity
    (fund_name / fund_tax_id) stamped onto the offering
  * idempotent reload (no duplicate snapshots)
"""
from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

INV_DATE = 1600000000   # investment date (2020)
DATA_DATE = 1700000000  # portfolio "as of" / valuation date (2023)


def m(frac, cur="USD"):
    return {"currency": cur, "fractional": frac}


def pos_node(pid, guid, *, name="Synthetic Co", inv_date=INV_DATE,
             status="live", contributed=50000, total=80000):
    return {
        "id": pid, "investableGuid": guid, "investableName": name,
        "investableAvatarUrl": "http://x/a.png",
        "commitmentAmount": m(100000), "contributedAmount": m(contributed),
        "investmentAmount": m(contributed), "realizedValue": m(0),
        "recycledValue": m(0),
        "unrealizedValue": m(total) if total is not None else None,
        "totalValue": m(total) if total is not None else None,
        "tvpi": 1.6, "investmentDate": inv_date, "status": status,
        "statusLabel": status.title(), "fcId": "fc-" + pid, "isOnline": True,
        "hasNonStandardReporting": False,
    }


def positions_capture(nodes, slug="acme"):
    return {"op": "PositionsTableQuery", "variables": {"investAccountSlug": slug},
            "data": {"invest": {"portfolio": {"positions": {
                "edges": [{"node": n} for n in nodes],
                "totalCount": len(nodes),
                "pageInfo": {"hasNextPage": False}}}}}}


def dashboard_capture(slug="acme", timeseries=None):
    return {"op": "PortfolioDashboardQuery", "variables": {"investAccountSlug": slug},
            "data": {"invest": {"portfolio": {"summary": {
                "totalInvestedAmount": m(100000), "totalRealizedValue": m(0),
                "totalUnrealizedValue": m(80000), "totalValue": m(80000),
                "totalCommittedAmount": None, "totalContributedAmount": None,
                "irr": 0.2, "tvpi": 1.6, "dpi": 0.0, "dataDate": DATA_DATE,
                "totalFundsCount": 1, "totalInvestmentsCount": 2, "totalStartupsCount": 1,
                "timeSeries": timeseries if timeseries is not None else [
                    {"year": 2024, "month": 1, "day": 1, "totalValue": m(50000),
                     "totalInvestedAmount": m(60000), "realizedValue": m(0),
                     "unrealizedValue": m(50000), "offlineValueChange": m(0),
                     "isApproximate": False}]}}}}}


def commitments_capture(slug="acme"):
    return {"op": "OpenInvestmentsQuery",
            "variables": {"investAccountId": "e1", "userId": "u1"},
            "data": {"invest": {"openInvestments": [{
                "id": "oi1", "state": "completed",
                "commitmentAmount": m(25000), "paymentAmount": m(25000),
                "remainingAmountNeededToFund": m(0),
                "needsToWire": False, "needsToSign": False,
                "opportunity": {"investableName": "NewCo", "investableSlug": "newco",
                                "type": "FundCampaign", "closeDate": 1700000000,
                                "fundingDeadlineDate": "2024-12-31",
                                "syndicate": {"name": "Synd", "slug": "synd"}}}]}}}


def write_run(dest, ts, captures):
    d = Path(dest) / ts
    d.mkdir(parents=True)
    (d / "captures.jsonl").write_text(
        "\n".join(json.dumps(c) for c in captures) + "\n")
    (d / "run.json").write_text(json.dumps({"source": "angellist"}))
    return d


def test_load_basic(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    write_run(dest, "20240101T000000Z", [
        positions_capture([pos_node("p1", "acme-co-s"),
                           pos_node("p2", "acme-fund-f", total=None)]),
        dashboard_capture(), commitments_capture()])
    assert load.main(["--dest", str(dest), "--db", str(db)]) == 0
    c = sqlite3.connect(db)
    assert c.execute("SELECT MAX(silver_schema_version) FROM schema_meta").fetchone()[0] == 7

    # offerings: immutable identity, kind from the guid suffix
    kinds = dict(c.execute("SELECT position_external_id, kind FROM offerings").fetchall())
    assert kinds == {"p1": "spv", "p2": "fund"}
    assert c.execute("SELECT company_name FROM offerings WHERE position_external_id='p1'"
                     ).fetchone()[0] == "Synthetic Co"
    assert dict(c.execute("SELECT vehicle_external_id, kind FROM vehicles").fetchall()) == \
        {"acme-co-s": "spv", "acme-fund-f": "fund"}

    # position_snapshots: investment (at INV_DATE) + valuation (at DATA_DATE) per position
    assert c.execute("SELECT COUNT(*) FROM position_snapshots").fetchone()[0] == 4
    by = {(r[0], r[1]): r for r in c.execute(
        "SELECT position_external_id, event_type, as_of_date, market_value_minor, valuation_basis "
        "FROM position_snapshots")}
    # p1 has a current FMV; the valuation event carries it
    assert by[("p1", "valuation")][2] == DATA_DATE
    assert by[("p1", "valuation")][3] == 80000 and by[("p1", "valuation")][4] == "fmv"
    # investment event marks at cost
    assert by[("p1", "investment")][2] == INV_DATE
    assert by[("p1", "investment")][3] == 50000 and by[("p1", "investment")][4] == "cost"
    # p2 reports no current value -> valuation falls back to cost
    assert by[("p2", "valuation")][3] == 50000 and by[("p2", "valuation")][4] == "cost"

    assert c.execute("SELECT COUNT(*) FROM portfolio_timeseries").fetchone()[0] == 1
    assert c.execute("SELECT total_investments_count FROM portfolio_summary").fetchone()[0] == 2
    assert c.execute("SELECT commitment_minor FROM commitments "
                     "WHERE commitment_external_id='oi1'").fetchone()[0] == 25000
    c.close()


def test_parse_money_cents():
    assert load.parse_money_cents("$1,234.56") == 123456
    assert load.parse_money_cents("$-7,994") == -799400
    assert load.parse_money_cents("(1,234)") == -123400
    assert load.parse_money_cents("0") == 0
    assert load.parse_money_cents("") is None
    assert load.parse_money_cents("-") is None


def test_k1_documents(tmp_path):
    import csv as _csv
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    docs = tmp_path / "docs"
    docs.mkdir()
    # a position whose company matches the K-1 portfolio company
    write_run(dest, "20240101T000000Z",
              [positions_capture([pos_node("p1", "acme-co-s")]), dashboard_capture()])
    hdr = ["Portfolio Company", "Fund", "K-1 Status", "Beginning Capital",
           "Contributions", "Current Year Net Income (Loss)",
           "Other Increase (Decrease)", "Withdrawals & Distributions",
           "Line 19(a) - Cash Distributions", "Ending Capital",
           "Ending Capital %", "Final K-1?", "Fund Tax ID Number"]
    row = ["Synthetic Co", "ACME Fund I, a series of X, LP", "Issued", "$1,000",
           "$1,000", "$50", "", "", "$200", "$1,050", "5.0", "No", "12-3456789"]
    with open(docs / "Synthetic 2023 Consolidated Schedule K-1 Package.csv",
              "w", newline="") as f:
        _csv.writer(f).writerows([hdr, row])
    assert load.main(["--dest", str(dest), "--db", str(db),
                      "--documents-dir", str(docs)]) == 0
    c = sqlite3.connect(db)

    # parsed K-1 capital account
    k1 = c.execute(
        "SELECT tax_year, fund_tax_id, ending_capital_minor, fund_name "
        "FROM k1_capital_accounts").fetchall()
    assert k1 == [(2023, "12-3456789", 105000, "ACME Fund I, a series of X, LP")]
    td = c.execute("SELECT doc_type, tax_year, document_status FROM tax_documents").fetchone()
    assert td == ("k1_packet", 2023, "complete")

    # the SPV identity is stamped onto the immutable offering
    off = c.execute("SELECT fund_name, fund_tax_id FROM offerings "
                    "WHERE position_external_id='p1'").fetchone()
    assert off == ("ACME Fund I, a series of X, LP", "12-3456789")

    # a 'statement' event at the tax year-end carries the tax-basis ending capital
    st = c.execute(
        "SELECT market_value_minor, valuation_basis, contributed_minor, "
        "strftime('%Y-%m-%d', datetime(as_of_date,'unixepoch')) "
        "FROM position_snapshots WHERE position_external_id='p1' "
        "AND event_type='statement'").fetchone()
    assert st == (105000, "tax_basis", 100000, "2023-12-31")
    c.close()


def test_funding(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    cap = {"op": "InvestmentEntityQuery", "data": {"invest": {"investmentEntity": {
        "entityId": "e1", "slugName": "acct-individual", "legalName": "X",
        "balance": m(150000),
        "transactions": [
            {"id": "t1", "date": 100, "type": "deposit", "amount": m(500000),
             "balance": m(500000), "description": "Deposit", "syndicateName": None},
            {"id": "t2", "date": 200, "type": "investment", "amount": m(-300000),
             "balance": m(200000), "description": "Investment in X", "syndicateName": "X SPV"},
            {"id": "t3", "date": 300, "type": "withdrawal", "amount": m(-50000),
             "balance": m(150000), "description": "Withdrawal", "syndicateName": None},
        ]}}}}
    write_run(dest, "20240101T000000Z",
              [positions_capture([pos_node("p1", "acme-co-s")]), dashboard_capture(), cap])
    assert load.main(["--dest", str(dest), "--db", str(db)]) == 0
    c = sqlite3.connect(db)
    assert c.execute("SELECT COUNT(*) FROM funding_transactions").fetchone()[0] == 3
    assert c.execute("SELECT balance_minor FROM funding_accounts").fetchone()[0] == 150000
    amts = dict(c.execute(
        "SELECT transaction_external_id, amount_minor FROM funding_transactions").fetchall())
    assert amts == {"t1": 500000, "t2": -300000, "t3": -50000}   # signed, verbatim
    assert c.execute("SELECT syndicate_name FROM funding_transactions "
                     "WHERE transaction_external_id='t2'").fetchone()[0] == "X SPV"
    # ledger reconciles to the balance
    assert c.execute("SELECT SUM(amount_minor) FROM funding_transactions").fetchone()[0] == \
        c.execute("SELECT balance_minor FROM funding_accounts").fetchone()[0]
    c.close()


def test_funding_links(tmp_path):
    """Each funding tx links to the SPV/fund it concerns: single-SPV current
    company -> its position; multi-SPV company -> the position whose invest
    date is closest to the tx date; exited company -> a derived offering; bank
    deposit -> account-level (NULL)."""
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    D1, D2 = 1600000000, 1660000000   # two well-separated invest dates
    cap = {"op": "InvestmentEntityQuery", "data": {"invest": {"investmentEntity": {
        "entityId": "e1", "balance": m(0),
        "transactions": [
            {"id": "s1", "date": D1, "type": "investment", "amount": m(-1000),
             "balance": m(0), "description": "Investment in Single Co", "syndicateName": "a"},
            {"id": "ma", "date": D1 + 5000, "type": "investment", "amount": m(-2000),
             "balance": m(0), "description": "Investment in Multi Co", "syndicateName": "b"},
            {"id": "mb", "date": D2 + 5000, "type": "investment", "amount": m(-3000),
             "balance": m(0), "description": "Investment in Multi Co", "syndicateName": "b"},
            {"id": "x1", "date": D1, "type": "investment", "amount": m(-500),
             "balance": m(0), "description": "Investment in Exited Co", "syndicateName": "c"},
            {"id": "d1", "date": D1, "type": "deposit", "amount": m(6500),
             "balance": m(0), "description": "Deposit from bank", "syndicateName": None},
        ]}}}}
    write_run(dest, "20240101T000000Z", [
        positions_capture([
            pos_node("pS", "single-co-s", name="Single Co"),
            pos_node("pMa", "multi-co-a-s", name="Multi Co", inv_date=D1),
            pos_node("pMb", "multi-co-b-s", name="Multi Co", inv_date=D2)]),
        dashboard_capture(), cap])
    assert load.main(["--dest", str(dest), "--db", str(db)]) == 0
    c = sqlite3.connect(db)
    link = dict(c.execute(
        "SELECT transaction_external_id, position_external_id FROM funding_transactions").fetchall())
    assert link["s1"] == "pS"                  # single-SPV current company
    assert link["ma"] == "pMa"                 # multi-SPV: tx date near D1 -> pMa
    assert link["mb"] == "pMb"                 # multi-SPV: tx date near D2 -> pMb
    assert link["x1"] == "funding:exited-co"   # exited -> derived offering
    assert link["d1"] is None                  # external-bank deposit -> account-level
    # the exited investment got a thin offering derived from the ledger
    assert c.execute("SELECT kind, company_name FROM offerings "
                     "WHERE position_external_id='funding:exited-co'").fetchone() == ("spv", "Exited Co")
    c.close()


def test_idempotent_reload(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    write_run(dest, "20240101T000000Z",
              [positions_capture([pos_node("p1", "acme-co-s")]), dashboard_capture()])
    load.main(["--dest", str(dest), "--db", str(db)])
    n1 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM position_snapshots").fetchone()[0]
    load.main(["--dest", str(dest), "--db", str(db)])  # already loaded -> skip
    n2 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM position_snapshots").fetchone()[0]
    assert n1 == n2 == 2  # investment + valuation, no duplicates
