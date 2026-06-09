"""Unit tests for load.py.

Synthetic bronze (captures.jsonl) → SQLite silver. Covers:
  * migrations apply (schema_meta v3)
  * vehicles + kind derivation ('-f' fund, else spv)
  * positions parse + null totalValue preserved
  * portfolio_summary + portfolio_timeseries + commitments
  * change-based delta: unchanged positions don't write a new row;
    a value change does
  * idempotent reload (already-loaded snapshot skipped)
"""
from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402


def m(frac, cur="USD"):
    return {"currency": cur, "fractional": frac}


def pos_node(pid, guid, *, status="live", contributed=50000, total=80000,
             realized=0, tvpi=1.6):
    return {
        "id": pid, "investableGuid": guid, "investableName": "Synthetic Co",
        "investableAvatarUrl": "http://x/a.png",
        "commitmentAmount": m(100000), "contributedAmount": m(contributed),
        "investmentAmount": m(contributed), "realizedValue": m(realized),
        "recycledValue": m(0),
        "unrealizedValue": m(total) if total is not None else None,
        "totalValue": m(total) if total is not None else None,
        "tvpi": tvpi, "investmentDate": 1700000000, "status": status,
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
                "irr": 0.2, "tvpi": 1.6, "dpi": 0.0, "dataDate": 1700000000,
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
    assert c.execute("SELECT silver_schema_version FROM schema_meta "
                     "ORDER BY silver_schema_version DESC LIMIT 1").fetchone()[0] == 4
    kinds = dict(c.execute("SELECT vehicle_external_id, kind FROM vehicles").fetchall())
    assert kinds == {"acme-co-s": "spv", "acme-fund-f": "fund"}
    assert c.execute("SELECT COUNT(*) FROM positions").fetchone()[0] == 2
    tv = dict(c.execute("SELECT position_external_id, total_value_minor FROM positions").fetchall())
    assert tv["p1"] == 80000 and tv["p2"] is None          # null totalValue preserved
    assert c.execute("SELECT contributed_minor FROM positions "
                     "WHERE position_external_id='p1'").fetchone()[0] == 50000
    assert c.execute("SELECT COUNT(*) FROM portfolio_timeseries").fetchone()[0] == 1
    assert c.execute("SELECT total_investments_count FROM portfolio_summary").fetchone()[0] == 2
    assert c.execute("SELECT commitment_minor FROM commitments "
                     "WHERE commitment_external_id='oi1'").fetchone()[0] == 25000
    c.close()


def test_delta_dedup(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    write_run(dest, "20240101T000000Z", [positions_capture([
        pos_node("p1", "acme-co-s", realized=0),
        pos_node("p2", "acme-fund-f", realized=0)]), dashboard_capture()])
    # later snapshot: p1 unchanged, p2's realized changes 0 -> 10000
    write_run(dest, "20240201T000000Z", [positions_capture([
        pos_node("p1", "acme-co-s", realized=0),
        pos_node("p2", "acme-fund-f", realized=10000)]), dashboard_capture()])
    assert load.main(["--dest", str(dest), "--db", str(db)]) == 0
    c = sqlite3.connect(db)
    assert c.execute("SELECT COUNT(*) FROM positions WHERE position_external_id='p1'").fetchone()[0] == 1
    assert c.execute("SELECT COUNT(*) FROM positions WHERE position_external_id='p2'").fetchone()[0] == 2
    assert c.execute("SELECT COUNT(*) FROM positions").fetchone()[0] == 3
    latest_p2 = c.execute("SELECT realized_minor FROM positions WHERE position_external_id='p2' "
                          "ORDER BY snapshot_at DESC LIMIT 1").fetchone()[0]
    assert latest_p2 == 10000
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
    # a position so a vehicle named "Synthetic Co" exists for K-1 linkage
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
    r = c.execute(
        "SELECT tax_year, fund_tax_id, beginning_capital_minor, contributions_minor, "
        "net_income_minor, cash_distributions_minor, ending_capital_minor, "
        "ending_capital_pct, final_k1, vehicle_external_id, fund_name "
        "FROM k1_capital_accounts").fetchall()
    assert len(r) == 1
    (year, ein, beg, contrib, ni, cashd, end, pct, final, vid, fund) = r[0]
    assert year == 2023                         # parsed from filename
    assert ein == "12-3456789"
    assert (beg, contrib, ni, cashd, end) == (100000, 100000, 5000, 20000, 105000)
    assert abs(pct - 5.0) < 1e-9
    assert final == 0
    assert vid == "acme-co-s"                    # linked to the company's vehicle
    assert "a series of" in fund                 # SPV legal name kept
    # tax_documents provenance + status from filename
    td = c.execute("SELECT doc_type, tax_year, document_status FROM tax_documents").fetchone()
    assert td == ("k1_packet", 2023, "complete")
    # ending capital fed into positions history as a Dec-31 tax-basis snapshot
    tb = c.execute(
        "SELECT tax_basis_capital_minor, total_value_minor, "
        "strftime('%Y-%m-%d', datetime(snapshot_at,'unixepoch')) "
        "FROM positions WHERE position_external_id='p1' "
        "AND tax_basis_capital_minor IS NOT NULL").fetchone()
    assert tb == (105000, None, "2023-12-31")   # $1,050 ending cap, FMV untouched, dated at year-end
    # idempotent re-load (same content sha) does not duplicate
    load.main(["--dest", str(dest), "--db", str(db), "--documents-dir", str(docs)])
    assert c.execute("SELECT COUNT(*) FROM k1_capital_accounts").fetchone()[0] == 1
    c.close()


def test_idempotent_reload(tmp_path):
    dest, db = tmp_path / "bronze", tmp_path / "angellist.db"
    write_run(dest, "20240101T000000Z",
              [positions_capture([pos_node("p1", "acme-co-s")]), dashboard_capture()])
    load.main(["--dest", str(dest), "--db", str(db)])
    n1 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM positions").fetchone()[0]
    load.main(["--dest", str(dest), "--db", str(db)])  # already loaded -> skip
    n2 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM positions").fetchone()[0]
    assert n1 == n2 == 1
