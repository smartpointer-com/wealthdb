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
  * silver-DB + documents-dir defaults derived from --bronze-dir
  * --force rebuild reproduces the incremental load, modulo the
    VOLATILE_COLUMNS wall-clock stamps (allowlist completeness guarded)
"""
from __future__ import annotations

import csv
import json
import re
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


K1_HDR = ["Portfolio Company", "Fund", "K-1 Status", "Beginning Capital",
          "Contributions", "Current Year Net Income (Loss)",
          "Other Increase (Decrease)", "Withdrawals & Distributions",
          "Line 19(a) - Cash Distributions", "Ending Capital",
          "Ending Capital %", "Final K-1?", "Fund Tax ID Number"]
K1_ROW = ["Synthetic Co", "ACME Fund I, a series of X, LP", "Issued", "$1,000",
          "$1,000", "$50", "", "", "$200", "$1,050", "5.0", "No", "12-3456789"]


def write_k1_csv(docs_dir):
    """One-row 2023 K-1 package CSV for the 'Synthetic Co' position."""
    docs_dir = Path(docs_dir)
    docs_dir.mkdir(parents=True, exist_ok=True)
    with open(docs_dir / "Synthetic 2023 Consolidated Schedule K-1 Package.csv",
              "w", newline="") as f:
        csv.writer(f).writerows([K1_HDR, K1_ROW])


def test_load_basic(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    write_run(dest, "20240101T000000Z", [
        positions_capture([pos_node("p1", "acme-co-s"),
                           pos_node("p2", "acme-fund-f", total=None)]),
        dashboard_capture(), commitments_capture()])
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db)]) == 0
    c = sqlite3.connect(db)
    assert c.execute("SELECT MAX(silver_schema_version) FROM schema_meta").fetchone()[0] == 7

    # offerings: immutable identity, kind from the guid suffix
    kinds = dict(c.execute("SELECT position_external_id, kind FROM offerings").fetchall())
    assert kinds == {"p1": "spv", "p2": "fund"}
    assert c.execute("SELECT company_name FROM offerings WHERE position_external_id='p1'"
                     ).fetchone()[0] == "Synthetic Co"
    assert dict(c.execute("SELECT vehicle_external_id, kind FROM vehicles").fetchall()) == \
        {"acme-co-s": "spv", "acme-fund-f": "fund"}

    # position_snapshots: investment (at INV_DATE) per position + valuation
    # (at DATA_DATE) where the portal states a value
    assert c.execute("SELECT COUNT(*) FROM position_snapshots").fetchone()[0] == 3
    by = {(r[0], r[1]): r for r in c.execute(
        "SELECT position_external_id, event_type, as_of_date, market_value_minor, valuation_basis "
        "FROM position_snapshots")}
    # p1 has a current FMV; the valuation event carries it
    assert by[("p1", "valuation")][2] == DATA_DATE
    assert by[("p1", "valuation")][3] == 80000 and by[("p1", "valuation")][4] == "fmv"
    # investment event marks at cost
    assert by[("p1", "investment")][2] == INV_DATE
    assert by[("p1", "investment")][3] == 50000 and by[("p1", "investment")][4] == "cost"
    # p2 reports no current value -> no valuation event; its investment cost
    # carries until a statement marks it (a re-mark to cost at the data date
    # would override a later, lower statement)
    assert ("p2", "valuation") not in by
    assert by[("p2", "investment")][3] == 50000

    assert c.execute("SELECT COUNT(*) FROM portfolio_timeseries").fetchone()[0] == 1
    assert c.execute("SELECT total_investments_count FROM portfolio_summary").fetchone()[0] == 2
    assert c.execute("SELECT commitment_minor FROM commitments "
                     "WHERE commitment_external_id='oi1'").fetchone()[0] == 25000
    c.close()


def test_realized_position_marks_zero_and_closes(tmp_path):
    # A Realized position's totalValue is its realized value — what came out,
    # not what is held — so it marks 0 and closes at the data date. A
    # written-off one (realized 0) closes the same way.
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    gone = pos_node("p3", "gone-co-s", name="Gone Co", status="closed", total=7000)
    gone["realizedValue"], gone["unrealizedValue"] = m(7000), m(0)
    lost = pos_node("p4", "lost-co-s", name="Lost Co", status="closed", total=0)
    write_run(dest, "20240101T000000Z", [
        positions_capture([gone, lost]), dashboard_capture(), commitments_capture()])
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db)]) == 0
    c = sqlite3.connect(db)
    rows = {r[0]: r[1:] for r in c.execute(
        "SELECT position_external_id, market_value_minor, is_open, status, distributions_minor "
        "FROM position_snapshots WHERE event_type='valuation'")}
    assert rows == {"p3": (0, 0, "closed", 7000), "p4": (0, 0, "closed", 0)}
    c.close()


def test_a_position_with_no_dated_mark_takes_cost(tmp_path):
    # No investment date and no portal value: cost at the data date is the
    # only value the position can show, so it still gets one.
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    write_run(dest, "20240101T000000Z", [
        positions_capture([pos_node("p5", "new-co-s", inv_date=None, total=None)]),
        dashboard_capture(), commitments_capture()])
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db)]) == 0
    c = sqlite3.connect(db)
    assert c.execute("SELECT event_type, as_of_date, market_value_minor, valuation_basis "
                     "FROM position_snapshots").fetchall() == [("valuation", DATA_DATE, 50000, "cost")]
    c.close()


def test_parse_money_cents():
    assert load.parse_money_cents("$1,234.56") == 123456
    assert load.parse_money_cents("$-7,994") == -799400
    assert load.parse_money_cents("(1,234)") == -123400
    assert load.parse_money_cents("0") == 0
    assert load.parse_money_cents("") is None
    assert load.parse_money_cents("-") is None


def test_k1_documents(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    # a position whose company matches the K-1 portfolio company
    write_run(dest, "20240101T000000Z",
              [positions_capture([pos_node("p1", "acme-co-s")]), dashboard_capture()])
    write_k1_csv(tmp_path / "docs")
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db),
                      "--documents-dir", str(tmp_path / "docs")]) == 0
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


def test_default_paths_derive_from_bronze_dir(tmp_path):
    """With only --bronze-dir given, the silver DB lands at
    <bronze-dir>/angellist.db and documents are parsed from the
    <bronze-dir>/angellist-documents sibling — the DESIGN.md bronze-root
    layout — so one flag scopes the whole load (a temp tree stays a temp
    tree, no absolute /data reads)."""
    dest = tmp_path / "bronze"
    write_run(dest, "20240101T000000Z",
              [positions_capture([pos_node("p1", "acme-co-s")]), dashboard_capture()])
    write_k1_csv(dest / "angellist-documents")
    assert load.main(["--bronze-dir", str(dest)]) == 0
    db = dest / "angellist.db"
    assert db.is_file()
    c = sqlite3.connect(db)
    assert c.execute("SELECT COUNT(*) FROM offerings").fetchone()[0] == 1
    # the sibling documents dir was picked up without --documents-dir
    assert c.execute("SELECT doc_type, tax_year FROM tax_documents").fetchall() == \
        [("k1_packet", 2023)]
    assert c.execute("SELECT COUNT(*) FROM k1_capital_accounts").fetchone()[0] == 1
    c.close()
    # an explicit flag wins over the derivation
    assert load.parse_args(["--documents-dir", "/x"]).documents_dir == Path("/x")
    assert load.parse_args(["--silver-db", "/y.db"]).silver_db == Path("/y.db")
    assert load.parse_args([]).documents_dir is None
    assert load.parse_args([]).silver_db is None


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
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db)]) == 0
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
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db)]) == 0
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


def write_rename_k1_csvs(docs_dir):
    """Two K-1 package CSVs for one fund whose portfolio company renamed:
    the 2023 rows carry the old label, the 2024 rows the new one."""
    docs_dir = Path(docs_dir)
    docs_dir.mkdir(parents=True, exist_ok=True)
    fund = "NEW Fund I, a series of X, LP"
    with open(docs_dir / "Synthetic 2023 Consolidated Schedule K-1 Package.csv",
              "w", newline="") as f:
        csv.writer(f).writerows([K1_HDR, [
            "Old Name Co", fund, "Issued", "$0", "$1,000", "$0", "", "", "",
            "$1,000", "5.0", "No", "12-3456789"]])
    with open(docs_dir / "Synthetic 2024 Consolidated Schedule K-1 Package.csv",
              "w", newline="") as f:
        csv.writer(f).writerows([K1_HDR, [
            "New Name Co", fund, "Issued", "$1,000", "$0", "$500", "", "", "",
            "$1,500", "5.0", "No", "12-3456789"]])


def test_rename_pairs_fund_to_one_position(tmp_path):
    """A renamed portfolio company must not split its fund across two
    instruments: every K-1 year lands on the one real position (matched
    through any label the fund ever carried), and a funding tx written
    under the old label resolves through the K-1 alias map instead of
    minting a phantom offering."""
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    cap = {"op": "InvestmentEntityQuery", "data": {"invest": {"investmentEntity": {
        "entityId": "e1", "balance": m(0),
        "transactions": [
            {"id": "t1", "date": INV_DATE, "type": "investment", "amount": m(-100000),
             "balance": m(0), "description": "Investment in Old Name Co",
             "syndicateName": "x"}]}}}}
    # The portal shows only the new name; contributed matches the K-1 total.
    write_run(dest, "20240101T000000Z", [
        positions_capture([pos_node("pN", "new-name-co-s", name="New Name Co",
                                    contributed=100000)]),
        dashboard_capture(), cap])
    write_rename_k1_csvs(tmp_path / "docs")
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db),
                      "--documents-dir", str(tmp_path / "docs")]) == 0
    c = sqlite3.connect(db)
    # both tax years' statements sit on the real position
    st = c.execute(
        "SELECT market_value_minor FROM position_snapshots "
        "WHERE position_external_id='pN' AND event_type='statement' "
        "ORDER BY as_of_date").fetchall()
    assert st == [(100000,), (150000,)]
    # the old-label funding tx linked to the real position; no phantom minted
    assert c.execute("SELECT position_external_id FROM funding_transactions "
                     "WHERE transaction_external_id='t1'").fetchone()[0] == "pN"
    assert c.execute("SELECT COUNT(*) FROM offerings "
                     "WHERE position_external_id LIKE 'funding:%'").fetchone()[0] == 0
    c.close()


def test_rename_heals_existing_phantom(tmp_path):
    """A silver DB that already carries a rename phantom (a funding:*
    offering with statement marks stamped while the pairing was unstable)
    self-heals on the next load: statements are rebuilt onto the real
    position only, the funding tx relinks, and the phantom plus its
    snapshots are dropped."""
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    cap = {"op": "InvestmentEntityQuery", "data": {"invest": {"investmentEntity": {
        "entityId": "e1", "balance": m(0),
        "transactions": [
            {"id": "t1", "date": INV_DATE, "type": "investment", "amount": m(-100000),
             "balance": m(0), "description": "Investment in Old Name Co",
             "syndicateName": "x"}]}}}}
    write_run(dest, "20240101T000000Z", [
        positions_capture([pos_node("pN", "new-name-co-s", name="New Name Co",
                                    contributed=100000)]),
        dashboard_capture(), cap])
    write_rename_k1_csvs(tmp_path / "docs")
    args = ["--bronze-dir", str(dest), "--silver-db", str(db),
            "--documents-dir", str(tmp_path / "docs")]
    assert load.main(args) == 0
    # Simulate the pre-fix state: a phantom under the old label, carrying a
    # duplicate statement mark and the funding-tx link.
    c = sqlite3.connect(db)
    c.execute("INSERT INTO offerings (position_external_id, kind, company_name, currency) "
              "VALUES ('funding:old-name-co', 'spv', 'Old Name Co', 'USD')")
    c.execute("INSERT INTO position_snapshots (position_external_id, as_of_date, "
              "event_type, is_open, currency, market_value_minor, valuation_basis, snapshot_at) "
              "VALUES ('funding:old-name-co', 1735603200, 'statement', 1, 'USD', "
              "150000, 'tax_basis', 1)")
    c.execute("UPDATE funding_transactions SET position_external_id='funding:old-name-co' "
              "WHERE transaction_external_id='t1'")
    c.commit()
    c.close()
    assert load.main(args) == 0   # bronze already loaded; doc+pairing passes rerun
    c = sqlite3.connect(db)
    assert c.execute("SELECT COUNT(*) FROM offerings "
                     "WHERE position_external_id LIKE 'funding:%'").fetchone()[0] == 0
    assert c.execute("SELECT COUNT(*) FROM position_snapshots "
                     "WHERE position_external_id LIKE 'funding:%'").fetchone()[0] == 0
    assert c.execute("SELECT position_external_id FROM funding_transactions "
                     "WHERE transaction_external_id='t1'").fetchone()[0] == "pN"
    # statements exist exactly once, on the real position
    assert c.execute("SELECT COUNT(*) FROM position_snapshots "
                     "WHERE event_type='statement'").fetchone()[0] == 2
    c.close()


FUND_STMT_TEXT = """\
              Partner's Capital Statement (Unaudited)
Capital account balance at December 31, 2025      $    233,300 $   233,300
Paid in capital, since inception                       202,000
Distributions, since inception                            (990)
"""


def test_financial_statement_fmv_overrides_k1(tmp_path, monkeypatch):
    """A fund financial report's per-LP capital statement lands as a
    basis='fmv' statement event at the period end, replacing a same-dated
    K-1 tax-basis mark; the fund is matched by name from the filename and
    SPVs never match. Quarter-end reports add their own dated events."""
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    docs = tmp_path / "docs"
    write_run(dest, "20240101T000000Z", [
        positions_capture([
            pos_node("pF", "example-fund-of-funds-f", name="Example Fund-of-Funds",
                     contributed=100000),
            pos_node("pS", "example-co-s", name="Example Co")]),
        dashboard_capture()])
    docs.mkdir(parents=True)
    # 2025 K-1 for the fund -> a Dec-31 tax-basis statement first.
    fund = "Example Fund of Funds, LP"
    with open(docs / "Synthetic 2025 Consolidated Schedule K-1 Package.csv",
              "w", newline="") as f:
        csv.writer(f).writerows([K1_HDR, [
            "Example Fund-of-Funds", fund, "Issued", "$0", "$1,000", "$0", "",
            "", "", "$460", "5.0", "No", "12-3456789"]])
    # Two financial reports: a quarter end and the year end (same date as
    # the K-1 statement). Content comes from the monkeypatched extractor.
    (docs / "Example Fund of Funds, LP-2025-09-30 (X).pdf").write_bytes(b"%PDF")
    (docs / "Example Fund of Funds, LP-2025-12-31 (X).pdf").write_bytes(b"%PDF")
    q3 = FUND_STMT_TEXT.replace("December 31", "September 30").replace("233,300", "88,800")
    monkeypatch.setattr(load.statements, "pdf_text",
                        lambda p: q3 if "09-30" in p.name else FUND_STMT_TEXT)
    assert load.main(["--bronze-dir", str(dest), "--silver-db", str(db),
                      "--documents-dir", str(docs)]) == 0
    c = sqlite3.connect(db)
    rows = c.execute(
        "SELECT strftime('%Y-%m-%d', datetime(as_of_date,'unixepoch')), "
        "market_value_minor, valuation_basis FROM position_snapshots "
        "WHERE position_external_id='pF' AND event_type='statement' "
        "ORDER BY as_of_date").fetchall()
    # Q3 fair value + Dec-31 fair value (which REPLACED the K-1 tax mark)
    assert rows == [("2025-09-30", 8880000, "fmv"),
                    ("2025-12-31", 23330000, "fmv")]
    # the SPV got nothing from the reports
    assert c.execute("SELECT COUNT(*) FROM position_snapshots "
                     "WHERE position_external_id='pS' AND event_type='statement'"
                     ).fetchone()[0] == 0
    c.close()


def test_idempotent_reload(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    write_run(dest, "20240101T000000Z",
              [positions_capture([pos_node("p1", "acme-co-s")]), dashboard_capture()])
    load.main(["--bronze-dir", str(dest), "--silver-db", str(db)])
    n1 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM position_snapshots").fetchone()[0]
    load.main(["--bronze-dir", str(dest), "--silver-db", str(db)])  # already loaded -> skip
    n2 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM position_snapshots").fetchone()[0]
    assert n1 == n2 == 2  # investment + valuation, no duplicates


# Wall-clock ingest stamps: (table, column) pairs that record WHEN a load
# ran, not bronze-derived data, so two otherwise-identical silver builds
# legitimately differ there. _dump_silver drops exactly these columns —
# whether the stamp comes from a column DEFAULT or from the INSERT itself.
# test_volatile_allowlist_is_complete cross-checks the set against the
# migrations and the loader SQL, so a new stamp fails loudly until listed.
VOLATILE_COLUMNS = {
    ("schema_meta", "applied_at"),      # DEFAULT (datetime('now'))
    ("dump_runs", "loaded_at"),         # DEFAULT (datetime('now'))
    ("tax_documents", "retrieved_at"),  # INSERT stamps strftime('%s','now')
}

# SQLite's spellings of "current wall-clock time": the quoted 'now'
# timestring (datetime('now'), strftime('%s','now'), ...), the
# CURRENT_TIMESTAMP / CURRENT_TIME / CURRENT_DATE keywords, and the
# no-timestring forms that default to 'now' — zero-argument date(),
# time(), datetime(), julianday(), unixepoch(), and format-only
# strftime('...'). The last alternations also catch a Python-side
# time.time() / datetime() stamp bound into an INSERT, which no
# DEFAULT scan could see.
_WALL_CLOCK_SQL = re.compile(
    r"(?i)'now'|current_(?:time|date)"
    r"|\b(?:date|time|datetime|julianday|unixepoch)\s*\(\s*\)"
    r"|\bstrftime\s*\(\s*'[^']*'\s*\)")


def _dump_silver(db):
    """Full snapshot of every user table's rows, for asserting two silver
    builds carry identical data. Tables are discovered via sqlite_master (a
    newly added one is picked up automatically); the VOLATILE_COLUMNS
    wall-clock stamps are dropped — they form no part of "silver is
    reproducible from bronze". Each table's rows are ordered by their repr,
    a stable total order independent of insertion sequence and of NULL/int
    column mixing."""
    c = sqlite3.connect(db)
    try:
        tables = [r[0] for r in c.execute(
            "SELECT name FROM sqlite_master WHERE type='table' "
            "AND name NOT LIKE 'sqlite_%' ORDER BY name")]
        state = {}
        for t in tables:
            keep = [name for _cid, name, _ty, _nn, _dflt, _pk
                    in c.execute(f'PRAGMA table_info("{t}")')
                    if (t, name) not in VOLATILE_COLUMNS]
            sel = ", ".join(f'"{col}"' for col in keep)
            state[t] = sorted(c.execute(f'SELECT {sel} FROM "{t}"'), key=repr)
        return state
    finally:
        c.close()


def test_volatile_allowlist_is_complete():
    """VOLATILE_COLUMNS must track the schema and the loader exactly.

    Completeness: every wall-clock stamp in the SQL — a column DEFAULT in
    migrations/*.sql or a stamp inside one of load.py's statements (the
    kind a DEFAULT scan cannot see) — must target an allowlisted column;
    otherwise _dump_silver keeps it and any two-build comparison flakes
    once the builds straddle a second boundary. Precision: every
    allowlisted pair must be a real, provably stamped column, so a stale
    entry can't silently drop genuine data from the comparison."""
    stamped = set()
    for src in sorted(load.MIGRATIONS.glob("*.sql")) + [Path(load.__file__)]:
        lines = src.read_text().splitlines()
        for i, line in enumerate(lines):
            if not _WALL_CLOCK_SQL.search(line):
                continue
            # The statement's target table + column list sit within a few
            # lines above the stamp (the loader's SQL literals are
            # line-broken); the nearest preceding target wins.
            ctx = "\n".join(lines[max(0, i - 12):i + 1])
            targets = re.findall(
                r"(?i)(?:INTO|UPDATE|CREATE TABLE(?:\s+IF\s+NOT\s+EXISTS)?)\s+(\w+)",
                ctx)
            assert targets, \
                f"{src.name}:{i + 1}: wall-clock stamp with no target table in context"
            table = targets[-1]
            cols = [c for t, c in VOLATILE_COLUMNS
                    if t == table and re.search(rf"\b{c}\b", ctx)]
            assert cols, (
                f"{src.name}:{i + 1}: stamps the wall clock into {table!r}, but no "
                f"matching (table, column) is in VOLATILE_COLUMNS — add it there so "
                f"_dump_silver keeps excluding it from the two-build comparison")
            stamped.update((table, c) for c in cols)

    # Both directions at once: nothing stamped is missing from the
    # allowlist, and no allowlisted entry has gone stale.
    assert stamped == VOLATILE_COLUMNS

    # Every allowlisted pair is a real column of the migrated schema, and
    # every DEFAULT-side stamp the schema itself declares is allowlisted
    # (authoritative for DEFAULTs, independent of the source scan above).
    conn = sqlite3.connect(":memory:")
    try:
        for sql in sorted(load.MIGRATIONS.glob("*.sql")):
            conn.executescript(sql.read_text())
        schema_cols, default_stamped = set(), set()
        for table, in conn.execute(
                "SELECT name FROM sqlite_master WHERE type='table' "
                "AND name NOT LIKE 'sqlite_%'").fetchall():
            for _cid, name, _ty, _nn, dflt, _pk in conn.execute(
                    f'PRAGMA table_info("{table}")'):
                schema_cols.add((table, name))
                if dflt and _WALL_CLOCK_SQL.search(dflt):
                    default_stamped.add((table, name))
        assert VOLATILE_COLUMNS <= schema_cols
        assert default_stamped <= VOLATILE_COLUMNS
    finally:
        conn.close()


def test_force_rebuild_equals_incremental(tmp_path):
    """`load --force` deletes the silver DB and rebuilds it from all bronze;
    for UNCHANGED bronze that reproduces the plain incremental load exactly
    (silver is a pure function of bronze). Guards the silver.reset wiring in
    main(): a forced rebuild must land on identical data, never drift or
    duplicate."""
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    # One complete run spanning every populated table: two positions (an SPV
    # and a fund), the dashboard summary + its NAV time series, an open
    # commitment, and the funding cash ledger — an account-level deposit, an
    # investment matched to a current position, and one into an exited company
    # that derives a thin offering from the ledger.
    funding = {"op": "InvestmentEntityQuery", "data": {"invest": {"investmentEntity": {
        "entityId": "e1", "slugName": "acct-individual", "legalName": "X",
        "balance": m(120000),
        "transactions": [
            {"id": "t1", "date": 100, "type": "deposit", "amount": m(200000),
             "balance": m(200000), "description": "Deposit from bank", "syndicateName": None},
            {"id": "t2", "date": 200, "type": "investment", "amount": m(-50000),
             "balance": m(150000), "description": "Investment in Synthetic Co",
             "syndicateName": "Synth SPV"},
            {"id": "t3", "date": 300, "type": "investment", "amount": m(-30000),
             "balance": m(120000), "description": "Investment in Exited Co",
             "syndicateName": "Exit SPV"},
        ]}}}}
    write_run(dest, "20240101T000000Z", [
        positions_capture([pos_node("p1", "acme-co-s", name="Synthetic Co"),
                           pos_node("p2", "acme-fund-f", name="Fund One", total=None)]),
        dashboard_capture(), commitments_capture(), funding])

    # Documents at the derived <bronze-dir>/angellist-documents sibling, so
    # the rebuild also spans tax_documents + k1_capital_accounts — and the
    # re-stamped tax_documents.retrieved_at (strftime('%s','now') inside the
    # INSERT, invisible to a DEFAULT scan) is exactly what VOLATILE_COLUMNS
    # must absorb for the equality to hold across a second boundary.
    write_k1_csv(dest / "angellist-documents")
    argv = ["--bronze-dir", str(dest), "--silver-db", str(db)]

    # Plain incremental load, then snapshot the whole silver DB.
    assert load.main(argv) == 0
    before = _dump_silver(db)
    # Sanity: the load actually populated the core tables, so the equality
    # below can't pass vacuously on two empty builds.
    assert before["offerings"] and before["position_snapshots"] \
        and before["funding_transactions"] and before["tax_documents"] \
        and before["k1_capital_accounts"]

    # Force = reset + full rebuild from the same, unchanged bronze.
    assert load.main(argv + ["--force"]) == 0
    after = _dump_silver(db)

    assert after == before
