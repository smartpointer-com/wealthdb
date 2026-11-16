"""
The standing test of an incremental load: carta's fixture runs loaded one
at a time give the silver that one `load --force` of all of them gives
(collectorkit.incremental). Three small runs with real one-page PDFs: a
cap-table entity and a fund, its statements, notices and K-1, a re-issued
K-1, and a run without documents. Synthetic data only.
"""
from __future__ import annotations

import json
from pathlib import Path

import load
from collectorkit import incremental
from conftest import pdf_bytes, text_page


def _json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj))


def _statement(contributed: str, distributed: str, ending: str) -> str:
    return (f"Capital contributions     10,371     {contributed}\n"
            f"Capital distributions     —     {distributed}\n"
            f"Management fees     (1,047)     (6,139)\n"
            f"Ending balance     $     {ending}     $     {ending}\n")


_CALL = ("Capital Call Notice\n"
         "Date of notice          May 20, 2098\n"
         "Due date          June 15, 2098\n"
         "Contribution          $61,250.00\n"
         "Called capital (post call)          $217,500.00\n")
_DIST = ("Distribution Notice\n"
         "Date of notice          January 15, 2099\n"
         "Distribution date          January 15, 2099\n"
         "Distribution          $1,234.56\n")


def _k1(beginning: str) -> list[tuple[float, float, str]]:
    """A federal face page: the heading, the two column anchors and item L."""
    return [(36, 66, "Schedule K-1"),
            (231, 90, "For calendar year 2098, or tax year"),
            (337, 106, "1 Ordinary business income (loss)"),
            (454, 106, "14 Self-employment earnings (loss)"),
            (337, 370, "8 Net short-term capital gain (loss)"),
            (405, 378, "(1,247.)"),
            (112, 622, "Partner's Capital Account Analysis"),
            (54, 634, "Beginning capital account ~~~ $"), (266, 630, beginning),
            (54, 682, "Withdrawals and distributions ~~ $ ("),
            (280, 678, "3,618."), (321, 682, ")"),
            (54, 694, "Ending capital account ~~ $"), (266, 690, "148,915.")]


def _entities(run: Path) -> None:
    corp = run / "entities" / "corp_7"
    _json(corp / "meta.json", {"corporation_id": 7, "is_fund_investment": False})
    _json(corp / "holdings-dashboard.json", {"held_since": "2098-01-01"})
    _json(corp / "options.json", {"rows": [
        {"id": 11, "label": "ES-1", "exercise_price": 0.5, "exercised": 1000,
         "quantity": 4000}]})
    _json(corp / "shares.json", {"rows": [
        {"id": 21, "label": "CS-1", "exercise_from": "ES-1", "quantity": 1000,
         "cost": 500.0, "issue_date": "03/04/2098"}]})
    _json(corp / "vesting" / "grant_11.json", {
        "label": "ES-1", "canceled_date": "2099-06-30",
        "vesting_event_data": [{"date": "2099-01-01", "amount": 1000}]})
    fund = run / "entities" / "fund_9"
    _json(fund / "meta.json", {"corporation_id": 9, "is_fund_investment": True})
    _json(fund / "fund-admin" / "partner-metrics.json", [{
        "partner": {"fund_id": "f-1", "fund_currency": "USD"},
        "metrics": {"commitment": "625000.00", "net_asset_value": "104286.37"},
        "sharing_date": "12/31/2098"}])
    _json(fund / "fund-cap-calls.json", [
        {"id": 501, "due_date": "2098-06-15", "amount": "61250.00"}])


def _run(root: Path, name: str, docs: list[tuple[dict, list]]) -> Path:
    run = root / name
    _entities(run)
    if docs:
        (run / "documents").mkdir(parents=True)
        for row, pages in docs:
            (run / "documents" / f"doc_{row['id']}.pdf").write_bytes(
                pdf_bytes(pages))
        _json(run / "documents" / "index.json", {"results": [
            {"document_name": f"document {row['id']}", "fund_id": "9", **row}
            for row, _ in docs]})
    _json(run / "run.json", {"status": "complete", "individual_id": "ind-1",
                             "entities": [7, 9]})
    return run


def test_runs_loaded_one_at_a_time_build_the_silver_of_a_force_load(tmp_path):
    statement = ({"id": "s1", "document_type": "Capital account statements",
                  "document_date": "12/31/2098"},
                 [text_page(_statement("217,500", "—", "224,931"))])
    call = ({"id": "n1", "document_type": "Capital calls"}, [text_page(_CALL)])
    runs = [
        _run(tmp_path / "runs", "20990101T000000Z", [
            statement, call,
            ({"id": "k1", "document_type": "Tax - Schedule K-1"},
             [_k1("104,286.")]),
            ({"id": "x1", "document_type": "Tax"},
             [text_page("Form 1042-S")])]),
        _run(tmp_path / "runs", "20990301T000000Z", [
            statement, call,
            ({"id": "s2", "document_type": "Capital account statements",
              "document_date": "03/31/2099"},
             [text_page(_statement("278,750", "(1,234.56)", "281,004"))]),
            ({"id": "d1", "document_type": "Distributions"},
             [text_page(_DIST)]),
            ({"id": "k1", "document_type": "Tax - Schedule K-1"},
             [_k1("104,290.")])]),
        _run(tmp_path / "runs", "20990501T000000Z", []),
    ]
    incremental.check(
        load.main,
        lambda db, bronze: ["--bronze-dir", str(bronze), "--silver-db", str(db)],
        runs, tmp_path / "work")
