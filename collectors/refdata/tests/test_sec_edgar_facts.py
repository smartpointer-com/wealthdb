"""sec-edgar's reported figures: by concept for a security held directly,
by frame for the securities held through funds, on invented answers in
the layout of the SEC's XBRL API."""

import json
import re
from datetime import date
from pathlib import Path

import pytest

import fetcher
from providers.base import Context, ProviderError, Unavailable, read_meta
from providers.sec_edgar import FAMILIES, FIGURES, SecEdgar, figure_rows
from providers.sec_frames import due, last_due, periods, quarter
from tests.helpers import FakeClient, go_source
from wanted import Security

FIXTURES = Path(__file__).parent / "fixtures"
SUB = {"cik": "0000000007", "name": "EXAMPLE INDUSTRIES INC", "entityType": "operating", "sic": "3711",
       "tickers": ["EXMP"], "exchanges": ["NYSE"], "stateOfIncorporation": "DE",
       "addresses": {"business": {"stateOrCountry": "DE"}}}
TICKERS = {"0": {"cik_str": 7, "ticker": "EXMP", "title": "EXAMPLE INDUSTRIES INC"},
           "1": {"cik_str": 8, "ticker": "EXMR", "title": "EXEMPLAR HOLDINGS CORP"},
           "2": {"cik_str": 9, "ticker": "EXMS", "title": "QUILLMORE BIOLOGICS INC"},
           "3": {"cik_str": 12, "ticker": "EXMT", "title": "EXEMPLAR MARITIME SA"}}
TODAY = date(2026, 10, 20)


def fixture(name):
    return json.loads((FIXTURES / name).read_text())


def concept(tag, *facts, unit="USD", taxonomy="us-gaap"):
    """A companyconcept answer of one unit."""
    return {"cik": 7, "taxonomy": taxonomy, "tag": tag, "label": tag, "units": {unit: list(facts)}}


def fact(start, end, val, filed, *, form="10-K", fy=2025, fp="FY", accn="0000000007-26-000003"):
    return {"start": start, "end": end, "val": val, "accn": accn, "fy": fy, "fp": fp, "form": form, "filed": filed}


def by_period(rows, field):
    return {(r["period_start"].isoformat(), r["as_of"].isoformat()): r for r in rows if r["field"] == field}


# ---- by concept --------------------------------------------------------


def test_a_security_held_directly_takes_every_period_of_its_figures(tmp_path):
    client = FakeClient({"submissions": SUB, "dei/EntityCommonStockSharesOutstanding": {"units": {"shares": []}},
                         "us-gaap/EarningsPerShareDiluted.json": fixture("edgar_concept_eps.json"),
                         "us-gaap/CashAndCashEquivalentsAtCarryingValue.json": fixture("edgar_concept_cash.json")})
    ctx = Context(client=client)
    e = SecEdgar()
    claim = e.claim_security(Security(key="symbol:EXMP", symbol="EXMP", missing=("sector", *FAMILIES)),
                             Context(client=FakeClient({"company_tickers.json": TICKERS})))
    assert claim["families"] == list(FAMILIES)
    e.fetch_security(claim, tmp_path, ctx)
    # One file per concept the company reports; the rest answered 404 and
    # the IFRS twins are not asked for.
    assert sorted(p.name for p in tmp_path.iterdir() if p.name.startswith("us-gaap")) == [
        "us-gaap_CashAndCashEquivalentsAtCarryingValue.json", "us-gaap_EarningsPerShareDiluted.json"]
    assert not any("ifrs-full" in u for u in client.urls())
    got = e.parse(tmp_path).period_facts
    eps = by_period(got, "eps_diluted")
    assert sorted(eps) == [("2024-01-01", "2024-12-31"), ("2025-01-01", "2025-12-31"), ("2025-04-01", "2025-06-30"),
                           ("2026-01-01", "2026-03-31"), ("2026-01-01", "2026-06-30"), ("2026-04-01", "2026-06-30")]
    q2 = eps[("2026-04-01", "2026-06-30")]
    assert (q2["value"], q2["scheme"], q2["unit"], q2["fiscal_year"], q2["fiscal_period"], q2["form"],
            q2["filed"]) == ("1.12", "us-gaap:EarningsPerShareDiluted", "USD/shares", 2026, "Q2", "10-Q",
                             date(2026, 8, 5))
    assert q2["payload"] == {"accn": "0000000007-26-000010", "frame": "CY2026Q2"}
    year = eps[("2025-01-01", "2025-12-31")]
    assert (year["value"], year["fiscal_period"], year["form"]) == ("4.1", "FY", "10-K")
    # A restated quarter takes the amendment's figure; a filing of another
    # form (8-K) does not count.
    restated = eps[("2026-01-01", "2026-03-31")]
    assert (restated["value"], restated["form"], restated["filed"]) == ("0.95", "10-Q/A", date(2026, 6, 15))
    # Filed the same day, the later accession wins. A comparative keeps
    # the fiscal year of the filing that first stated it.
    assert eps[("2024-01-01", "2024-12-31")]["value"] == "3.62"
    comparative = eps[("2025-04-01", "2025-06-30")]
    assert (comparative["fiscal_year"], comparative["filed"]) == (2025, date(2026, 8, 5))
    # A balance on a day starts and ends on that day.
    cash = by_period(got, "cash")
    assert sorted(cash) == [("2025-12-31", "2025-12-31"), ("2026-06-30", "2026-06-30")]
    assert (cash[("2026-06-30", "2026-06-30")]["value"], cash[("2026-06-30", "2026-06-30")]["unit"]) == (
        "5200000", "USD")
    assert {r["field"] for r in got} == {"eps_diluted", "cash"}


def test_a_security_held_directly_is_asked_only_the_families_it_misses(tmp_path):
    client = FakeClient({"company_tickers.json": TICKERS, "submissions": SUB, "us-gaap/EarningsPerShareBasicAndDiluted":
                         concept("EarningsPerShareBasicAndDiluted",
                                 fact("2025-01-01", "2025-12-31", 4.1, "2026-02-20"), unit="USD/shares")})
    ctx = Context(client=client)
    e = SecEdgar()

    def concepts_asked(sec, out):
        claim = e.claim_security(sec, ctx)
        client.asked.clear()
        e.fetch_security(claim, tmp_path / out, ctx)
        return [u.split("CIK0000000007/")[1] for u in client.urls() if "companyconcept" in u]

    # A version 1 list names no family: the lookup asks for no figure.
    assert concepts_asked(Security(key="symbol:EXMP", symbol="EXMP", missing=("sector",)), "v1") == [
        "dei/EntityCommonStockSharesOutstanding.json"]
    assert len(client.urls()) == 2 and client.urls()[0].endswith("/submissions/CIK0000000007.json")
    assert e.parse(tmp_path / "v1").period_facts == []
    # A stock missing its earnings is asked for their concepts only; a
    # later concept states a period the first does not.
    assert concepts_asked(Security(key="symbol:EXMP", symbol="EXMP", missing=("earnings",)), "v2") == [
        "dei/EntityCommonStockSharesOutstanding.json", "us-gaap/EarningsPerShareDiluted.json",
        "us-gaap/EarningsPerShareBasicAndDiluted.json"]
    (r,) = e.parse(tmp_path / "v2").period_facts
    assert (r["field"], r["scheme"], r["value"]) == ("eps_diluted", "us-gaap:EarningsPerShareBasicAndDiluted", "4.1")


def test_a_concept_the_company_does_not_report_leaves_the_figure_unknown(tmp_path):
    ctx = Context(client=FakeClient({"submissions": SUB}))
    e = SecEdgar()
    e.fetch_security({"key": "symbol:EXMP", "symbol": "EXMP", "cik": 7, "families": list(FAMILIES)}, tmp_path, ctx)
    got = e.parse(tmp_path)
    assert got.period_facts == [] and got.securities[0]["name"] == "EXAMPLE INDUSTRIES INC"


def test_a_concept_that_fails_fails_the_fetch(tmp_path):
    ctx = Context(client=FakeClient({"submissions": SUB, "us-gaap/NetIncomeLoss.json": 503}))
    with pytest.raises(fetcher.FetchError, match="503"):
        SecEdgar().fetch_security({"key": "symbol:EXMP", "symbol": "EXMP", "cik": 7, "families": ["financials"]},
                                  tmp_path, ctx)
    assert not (tmp_path / "meta.json").exists()


def test_an_ifrs_filer_reports_under_the_twins_in_its_own_currency(tmp_path):
    client = FakeClient({"submissions": {**SUB, "name": "EXEMPLAR MARITIME SA"},
                         "ifrs-full/DilutedEarningsLossPerShare.json": fixture("edgar_concept_ifrs_eps.json")})
    e = SecEdgar()
    e.fetch_security({"key": "symbol:EXMT", "symbol": "EXMT", "cik": 12, "families": ["earnings", "dividends"]},
                     tmp_path, Context(client=client))
    # Only the families asked for: no us-gaap concept answered, so their
    # IFRS twins were asked.
    asked = [u.split("CIK0000000012/")[1] for u in client.urls() if "companyconcept" in u]
    assert asked == ["dei/EntityCommonStockSharesOutstanding.json", "us-gaap/EarningsPerShareDiluted.json",
                     "us-gaap/EarningsPerShareBasicAndDiluted.json",
                     "us-gaap/CommonStockDividendsPerShareDeclared.json",
                     "us-gaap/CommonStockDividendsPerShareCashPaid.json",
                     "ifrs-full/DilutedEarningsLossPerShare.json",
                     "ifrs-full/DividendsRecognisedAsDistributionsToOwnersPerShare.json"]
    eps = by_period(e.parse(tmp_path).period_facts, "eps_diluted")
    year = eps[("2025-01-01", "2025-12-31")]
    assert (year["value"], year["scheme"], year["unit"], year["form"]) == (
        "2.05", "ifrs-full:DilutedEarningsLossPerShare", "EUR/shares", "20-F")


def test_the_families_and_their_fields_are_those_gold_reads():
    """Gold's wanted list reads a family's newest period from these
    fields (periodFamilies): a figure renamed or moved on one side would
    leave its family wanted for ever."""
    block = re.search(r"var periodFamilies = .*?\n\}\n", go_source("gold/refdata_wanted.go"), re.S).group(0)
    gold = [(family, re.findall(r'"(\w+)"', fields))
            for family, fields in re.findall(r'\{"(\w+)",\s*\[\]string\{(.*?)\}\}', block, re.S)]
    assert gold == [(family, [f.field for f in FIGURES if f.family == family]) for family in FAMILIES]


def test_revenue_takes_the_first_concept_that_states_a_period():
    revenue = next(f for f in FIGURES if f.field == "revenue")
    answers = {
        ("us-gaap", "Revenues"): concept("Revenues", fact("2016-01-01", "2016-12-31", 900, "2017-02-20"),
                                         fact("2025-01-01", "2025-12-31", 1200, "2026-02-20")),
        ("us-gaap", "RevenueFromContractWithCustomerExcludingAssessedTax"): concept(
            "RevenueFromContractWithCustomerExcludingAssessedTax",
            fact("2025-01-01", "2025-12-31", 1100, "2026-03-01", form="10-K/A"),
            fact("2026-01-01", "2026-03-31", 310, "2026-05-06", form="10-Q", fy=2026, fp="Q1"))}
    got = by_period(figure_rows(revenue, answers, "symbol:EXMP", provider="sec-edgar"), "revenue")
    assert {p: (r["value"], r["scheme"]) for p, r in got.items()} == {
        ("2016-01-01", "2016-12-31"): ("900", "us-gaap:Revenues"),
        ("2025-01-01", "2025-12-31"): ("1200", "us-gaap:Revenues"),
        ("2026-01-01", "2026-03-31"): ("310", "us-gaap:RevenueFromContractWithCustomerExcludingAssessedTax")}


def test_a_security_held_through_funds_is_looked_up_for_its_sector_or_country_only(tmp_path):
    e = SecEdgar()
    assert {"sector", "country", "earnings", "dividends", "financials"} <= e.answers
    only_figures = Security(key="symbol:EXMP", symbol="EXMP", scope="constituent", missing=("earnings",))
    sector = Security(key="symbol:EXMP", symbol="EXMP", scope="constituent", missing=("sector", "earnings"))
    assert not e.wants_constituent(only_figures) and e.wants_constituent(sector)
    client = FakeClient({"company_tickers.json": TICKERS, "submissions": SUB})
    ctx = Context(client=client)
    claim = e.claim_security(sector, ctx)
    assert claim["families"] == []
    e.fetch_security(claim, tmp_path, ctx)
    assert [u for u in client.urls() if "companyconcept" in u] == [
        "https://data.sec.gov/api/xbrl/companyconcept/CIK0000000007/dei/EntityCommonStockSharesOutstanding.json"]


# ---- by frame ----------------------------------------------------------


def constituent(key, symbol, missing, periods=None, **kw):
    return Security(key=key, symbol=symbol, scope="constituent", missing=tuple(missing), periods=periods or {}, **kw)


def plan(securities, today=TODAY, fetched=None, max_age=30):
    ctx = Context(client=FakeClient({"company_tickers.json": TICKERS}))
    return SecEdgar().plan_bulk(securities, ctx, today, fetched or {}, max_age)


def frames_of(planned, concept):
    return [ref.rsplit(":", 1)[1] for ref, _ in planned if f":{concept}:" in ref]


def test_the_quarter_arithmetic():
    assert quarter(date(2026, 6, 30)) == 2026 * 4 + 1
    # The quarter that ended 45 days before or more.
    assert last_due(date(2026, 11, 13)) == quarter(date(2026, 6, 30))
    assert last_due(date(2026, 11, 14)) == quarter(date(2026, 9, 30))
    q = quarter(date(2025, 12, 31))
    assert periods(q, q + 2, instant=True) == [("CY2025Q4I", date(2025, 12, 31)), ("CY2026Q1I", date(2026, 3, 31)),
                                               ("CY2026Q2I", date(2026, 6, 30))]
    assert [p for p, _ in periods(q, q + 2, instant=False)] == ["CY2025Q4", "CY2026Q1", "CY2026Q2", "CY2025",
                                                                 "CY2026"]
    # Fetched long after the period's end: final. Fetched soon after, and
    # longer ago than max_age: asked again.
    end = date(2026, 3, 31)
    assert due(None, end, TODAY, 30)
    assert not due(date(2026, 8, 1), end, TODAY, 30)
    assert due(date(2026, 5, 20), end, TODAY, 30) and not due(date(2026, 5, 20), end, date(2026, 6, 1), 30)


def test_plan_bulk_asks_from_the_least_current_securitys_newest_period():
    planned = plan([
        constituent("isin:US0000000701", "EXMP", ["earnings"], {"earnings": date(2025, 12, 31)}),
        constituent("isin:US0000000702", "EXMR", ["financials"], {"financials": date(2026, 3, 31)}),
        # Held directly, missing nothing of its figures, or no ticker the
        # SEC lists: no frame for it.
        Security(key="isin:US0000000703", symbol="EXMS", missing=("earnings",)),
        constituent("isin:US0000000704", "EXMS", ["sector"]),
        constituent("isin:US0000000705", "QNONE", ["dividends"]),
    ])
    # The last quarter due on 2026-10-20 is 2026Q2; the newest frames first.
    assert frames_of(planned, "EarningsPerShareDiluted") == ["CY2026Q2", "CY2026", "CY2026Q1", "CY2025Q4", "CY2025"]
    assert frames_of(planned, "CashAndCashEquivalentsAtCarryingValue") == ["CY2026Q2I", "CY2026Q1I"]
    assert frames_of(planned, "NetIncomeLoss") == ["CY2026Q2", "CY2026", "CY2026Q1"]
    assert not frames_of(planned, "CommonStockDividendsPerShareDeclared")
    assert planned[0][0] == "frame:us-gaap:EarningsPerShareDiluted:USD-per-shares:CY2026Q2"
    claim = dict(planned)["frame:us-gaap:Revenues:USD:CY2026Q1"]
    assert claim["concepts"] == ["Revenues", "RevenueFromContractWithCustomerExcludingAssessedTax"]
    assert (claim["field"], claim["families"], claim["keys"]) == ("revenue", ["financials"],
                                                                  {"8": ["isin:US0000000702"]})
    assert {c["families"][0] for _, c in planned} == {"earnings", "financials"}


def test_plan_bulk_looks_back_eight_quarters_where_gold_holds_none_and_never_further():
    none_held = plan([constituent("isin:US0000000711", "EXMP", ["dividends"]),
                      constituent("isin:US0000000712", "EXMR", ["dividends"], {"dividends": date(2026, 3, 31)})])
    assert frames_of(none_held, "CommonStockDividendsPerShareDeclared") == [
        "CY2026Q2", "CY2026", "CY2026Q1", "CY2025Q4", "CY2025", "CY2025Q3", "CY2025Q2", "CY2025Q1", "CY2024Q4",
        "CY2024", "CY2024Q3"]
    long_ago = plan([constituent("isin:US0000000713", "EXMP", ["dividends"], {"dividends": date(2019, 6, 30)})])
    assert frames_of(long_ago, "CommonStockDividendsPerShareDeclared") == frames_of(
        none_held, "CommonStockDividendsPerShareDeclared")


def test_plan_bulk_skips_a_frame_fetched_unless_late_filers_may_have_come():
    sec = constituent("isin:US0000000721", "EXMP", ["earnings"], {"earnings": date(2025, 12, 31)})
    ref = "frame:us-gaap:EarningsPerShareDiluted:USD-per-shares:{}".format
    fetched = {ref("CY2025Q4"): date(2026, 6, 1),     # 152 days after its end: final
               ref("CY2026Q1"): date(2026, 5, 20),    # soon after its end, and 153 days ago: again
               ref("CY2026Q2"): date(2026, 10, 1),    # soon after its end, 19 days ago: not yet
               ref("CY2026"): date(2026, 9, 1)}       # before its end, 49 days ago: again
    assert frames_of(plan([sec], fetched=fetched), "EarningsPerShareDiluted") == ["CY2026", "CY2026Q1", "CY2025"]
    assert frames_of(plan([sec], fetched=fetched, max_age=60), "EarningsPerShareDiluted") == ["CY2026Q1", "CY2025"]


def test_a_frame_states_a_figure_for_each_key_of_a_company_it_lists(tmp_path):
    planned = plan([
        constituent("isin:US0000000731", "EXMP", ["earnings"]),
        constituent("cusip:000000731", "EXMP", ["earnings"]),
        constituent("isin:US0000000732", "EXMR", ["earnings"]),
        # A bank abroad whose local ticker is a US company's: not matched.
        constituent("isin:XA0000000733", "EXMS", ["earnings"], name="Quarzberg Bank AG"),
    ])
    ref = "frame:us-gaap:EarningsPerShareDiluted:USD-per-shares:CY2026Q2"
    claim = dict(planned)[ref]
    assert claim["keys"] == {"7": ["isin:US0000000731", "cusip:000000731"], "8": ["isin:US0000000732"]}
    client = FakeClient({"frames/us-gaap/EarningsPerShareDiluted/USD-per-shares/CY2026Q2.json":
                         fixture("edgar_frame_eps.json")})
    e = SecEdgar()
    e.fetch_bulk(claim, tmp_path, Context(client=client))
    # The figure's later concept has no frame for the quarter: no error.
    assert client.urls() == [f"https://data.sec.gov/api/xbrl/frames/us-gaap/{c}/USD-per-shares/CY2026Q2.json"
                             for c in ("EarningsPerShareDiluted", "EarningsPerShareBasicAndDiluted")]
    meta = read_meta(tmp_path)
    assert (meta["families"], meta["concepts"]) == (["earnings"], ["EarningsPerShareDiluted"])
    assert meta["answered"] == ["isin:US0000000731", "cusip:000000731", "isin:US0000000732"]
    rows = e.parse(tmp_path).period_facts
    got = {r["security_key"]: (r["as_of"], r["period_start"], r["value"]) for r in rows}
    assert got == {"isin:US0000000731": (date(2026, 6, 30), date(2026, 4, 1), "1.12"),
                   "cusip:000000731": (date(2026, 6, 30), date(2026, 4, 1), "1.12"),
                   "isin:US0000000732": (date(2026, 6, 27), date(2026, 3, 29), "-0.4")}
    r = rows[0]
    assert (r["field"], r["scheme"], r["unit"], r["provider"]) == (
        "eps_diluted", "us-gaap:EarningsPerShareDiluted", "USD/shares", "sec-edgar")
    assert (r["fiscal_year"], r["fiscal_period"], r["form"], r["filed"]) == (None, None, None, None)
    assert r["payload"] == {"accn": "0000000007-26-000010", "frame": "CY2026Q2"}
    # A folder that lacks a frame its meta.json names does not parse.
    for f in tmp_path.glob("us-gaap_*"):
        f.unlink()
    with pytest.raises(ProviderError, match="no us-gaap_EarningsPerShareDiluted.json"):
        e.parse(tmp_path)


def test_a_revenue_frame_takes_the_first_concept_that_lists_a_company(tmp_path):
    def frame(tag, *rows):
        return {"taxonomy": "us-gaap", "tag": tag, "ccp": "CY2025", "uom": "USD", "pts": len(rows),
                "data": [{"accn": f"000000000{c}-26-000001", "cik": c, "entityName": "EXAMPLE CO", "loc": "US-DE",
                          "start": "2025-01-01", "end": "2025-12-31", "val": v} for c, v in rows]}
    claim = {"ref": "frame:us-gaap:Revenues:USD:CY2025", "field": "revenue", "families": ["financials"],
             "taxonomy": "us-gaap",
             "concepts": ["Revenues", "RevenueFromContractWithCustomerExcludingAssessedTax"], "unit": "USD",
             "frame": "CY2025", "keys": {"7": ["symbol:EXMP"], "8": ["symbol:EXMR"]}}
    client = FakeClient({"/Revenues/USD/CY2025.json": frame("Revenues", (7, 1200)),
                         "/RevenueFromContractWithCustomerExcludingAssessedTax/USD/CY2025.json":
                             frame("RevenueFromContractWithCustomerExcludingAssessedTax", (7, 1100), (8, 800))})
    e = SecEdgar()
    e.fetch_bulk(claim, tmp_path, Context(client=client))
    got = {r["security_key"]: (r["value"], r["scheme"]) for r in e.parse(tmp_path).period_facts}
    assert got == {"symbol:EXMP": ("1200", "us-gaap:Revenues"),
                   "symbol:EXMR": ("800", "us-gaap:RevenueFromContractWithCustomerExcludingAssessedTax")}
    assert read_meta(tmp_path)["answered"] == ["symbol:EXMP", "symbol:EXMR"]


def test_a_frame_that_does_not_exist_is_unavailable(tmp_path):
    claim = {"ref": "frame:us-gaap:LongTermDebt:USD:CY2026Q2I", "field": "long_term_debt",
             "families": ["financials"], "taxonomy": "us-gaap", "concepts": ["LongTermDebt", "LongTermDebtNoncurrent"],
             "unit": "USD", "frame": "CY2026Q2I", "keys": {"7": ["symbol:EXMP"]}}
    with pytest.raises(Unavailable, match="no frame"):
        SecEdgar().fetch_bulk(claim, tmp_path, Context(client=FakeClient()))
    assert not (tmp_path / "meta.json").exists()
