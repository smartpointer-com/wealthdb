"""invesco's dividend calendar: a US fund's distributions by CUSIP and a
UCITS fund's latest one by ISIN, on invented answers in the API's layout.
Every fund, identifier, day and amount is invented."""

import json
import logging
from datetime import date
from functools import partial

import pytest

from providers import REGISTRY
from providers.base import read_meta
from providers.invesco import API, distribution_events, distribution_rows
from tests import test_invesco_characteristics as chars
from tests.helpers import FakeClient, events_by_id, held, plan, wanted_doc
from tests.test_invesco_characteristics import DAY, HOLDINGS, UCITS_ID, US_ID

CAL = "dividend_calendar"


def dist(ex, record, pay, total, **parts):
    return {"exDate": ex, "recordDate": record, "payDate": pay, "distributionAmountPerUnit": total,
            "ordinaryIncomeDistribution": None, "shortTermCapitalGainsDistribution": None,
            "longTermCapitalGainsDistribution": None, "returnOfCapitalDistribution": None,
            "liquidationDistribution": None, **parts}


US_ANSWER = {"cusip": US_ID, "currencyCode": "USD", "distributions": [
    dist("2026-09-21", "2026-09-21", "2026-10-08", 0.75143, ordinaryIncomeDistribution=0.75143),
    # December pays income and both gains: one dividend, one capital gain.
    dist("2025-12-22T00:00:00", "2025-12-22T00:00:00", "2025-12-31T00:00:00", 1.1,
         ordinaryIncomeDistribution=0.8, shortTermCapitalGainsDistribution=0.1, longTermCapitalGainsDistribution=0.2),
    dist("2025-06-23", "2025-06-23", "2025-07-01", 0.5, ordinaryIncomeDistribution=0.4,
         returnOfCapitalDistribution=0.1, frequency="Quarterly"),
    dist("2024-03-18", "2024-03-19", "2024-03-28", 3.25, liquidationDistribution=3.25),
    # A distribution that states no part is one event of its amount.
    dist("2003-12-24", "2003-12-26", "2004-01-02", 0.02),
    dist(None, None, None, 0.01)]}
UCITS_ANSWER = {"effectiveDate": "2026-10-09", "announcementDate": "2026-09-04", "exDate": "2026-09-10",
                "recordDate": "2026-09-11", "paymentDate": "2026-09-17", "distributionYield": 0.3912,
                "currency": "USD"}


def api(distributions):
    """Invesco's API for one fund: its holdings, no index or swaps, and its
    distributions answer, or an HTTP error of that status."""
    return FakeClient({"/distribution": distributions, "/holdings/fund": HOLDINGS, "dng-api.invesco.com": 404})


fetch = partial(chars.fetch, wants=(CAL,), ref="symbol:XMPQQQ")


def test_a_fund_that_wants_its_calendar_alone_is_planned():
    inv = REGISTRY["invesco"]
    fund = {**held("symbol:XMPQQQ", "XMPQQQ", wants=[CAL], compositions=["2026-07-10"]),
            "name": "Invesco Example ETF", "cusip": US_ID}
    reqs, _, cover = plan(wanted_doc([fund], version=3), [inv])
    assert [(r.provider, r.ref, r.facts, r.compositions) for r in reqs] == [
        ("invesco", "symbol:XMPQQQ", (CAL,), False)]
    assert cover[CAL]["symbol:XMPQQQ"].requests == reqs


def test_a_us_fund_states_every_distribution_as_events_per_kind(tmp_path):
    client = api(US_ANSWER)
    parsed = fetch(tmp_path, client)
    ((_, url, params),) = [a for a in client.asked if a[1].endswith("/distribution")]
    assert url == API.format(locale="en_US", id=US_ID) + "distribution"
    assert params == {"idType": "cusip", "productType": "ETF"}
    assert "facts_missing" not in read_meta(tmp_path)
    assert json.loads((tmp_path / "distributions.json").read_text()) == US_ANSWER
    assert events_by_id(parsed.dividend_events, declared=True) == {
        "dividend|2026-09-21|2026-09-21|2026-10-08": ("dividend", date(2026, 9, 21), date(2026, 9, 21),
                                                      date(2026, 10, 8), None, "0.75143", "USD"),
        "dividend|2025-12-22|2025-12-22|2025-12-31": ("dividend", date(2025, 12, 22), date(2025, 12, 22),
                                                      date(2025, 12, 31), None, "0.8", "USD"),
        "capital_gain|2025-12-22|2025-12-22|2025-12-31": ("capital_gain", date(2025, 12, 22), date(2025, 12, 22),
                                                          date(2025, 12, 31), None, "0.3", "USD"),
        "dividend|2025-06-23|2025-06-23|2025-07-01": ("dividend", date(2025, 6, 23), date(2025, 6, 23),
                                                      date(2025, 7, 1), None, "0.4", "USD"),
        "return_of_capital|2025-06-23|2025-06-23|2025-07-01": ("return_of_capital", date(2025, 6, 23),
                                                               date(2025, 6, 23), date(2025, 7, 1), None, "0.1",
                                                               "USD"),
        "other|2024-03-18|2024-03-19|2024-03-28": ("other", date(2024, 3, 18), date(2024, 3, 19), date(2024, 3, 28),
                                                   None, "3.25", "USD"),
        "other|2003-12-24|2003-12-26|2004-01-02": ("other", date(2003, 12, 24), date(2003, 12, 26), date(2004, 1, 2),
                                                   None, "0.02", "USD")}
    assert {(r["ref"], r["provider"], r["as_of"], r["status"]) for r in parsed.dividend_events} == {
        ("symbol:XMPQQQ", "invesco", DAY, None)}
    gain = next(r for r in parsed.dividend_events if r["kind"] == "capital_gain")
    assert gain["payload"] == {"total": "1.1", "short_term": "0.1", "long_term": "0.2"}
    # The payload keeps the distribution's other fields.
    roc = next(r for r in parsed.dividend_events if r["kind"] == "return_of_capital")
    assert roc["payload"] == {"total": "0.5", "frequency": "Quarterly"}


def test_a_ucits_fund_states_its_latest_distribution(tmp_path):
    client = api(UCITS_ANSWER)
    parsed = fetch(tmp_path, client, isin=UCITS_ID)
    ((_, url, params),) = [a for a in client.asked if a[1].endswith("/distribution")]
    # No product type: the API refuses one for a UCITS fund.
    assert (url, params) == (API.format(locale="en_GB", id=UCITS_ID) + "distribution", {"idType": "isin"})
    assert "facts_missing" not in read_meta(tmp_path)
    assert events_by_id(parsed.dividend_events, declared=True) == {
        "dividend|2026-09-10|2026-09-11|2026-09-17": ("dividend", date(2026, 9, 10), date(2026, 9, 11),
                                                      date(2026, 9, 17), date(2026, 9, 4), "0.3912", "USD")}
    assert parsed.dividend_events[0]["payload"] == {"effectiveDate": "2026-10-09"}
    # The latest only: what silver holds stands beside it.
    assert not parsed.events_complete


@pytest.mark.parametrize("answer", [{}, {"effectiveDate": "2026-10-09", "distributionYield": None}])
def test_a_ucits_answer_that_states_no_day_states_no_distribution(tmp_path, answer):
    parsed = fetch(tmp_path, api(answer), isin=UCITS_ID)
    assert "facts_missing" not in read_meta(tmp_path) and parsed.dividend_events == []


def test_a_us_fund_without_distributions_has_answered(tmp_path):
    parsed = fetch(tmp_path, api({"cusip": US_ID, "currencyCode": "USD", "distributions": []}))
    assert "facts_missing" not in read_meta(tmp_path) and parsed.dividend_events == []
    # A US answer lists the fund's whole history.
    assert parsed.events_complete


@pytest.mark.parametrize("answer,isin,warning", [
    (500, None, "its distributions did not come"),
    ("<html>busy</html>", None, "its distributions did not come"),
    ([], None, "its distributions matched no known layout"),
    ({"cusip": US_ID, "currencyCode": "USD"}, None, "its distributions matched no known layout"),
    ({"distributions": "none"}, None, "its distributions matched no known layout"),
    # A UCITS answer that holds none of its fields does not say it paid nothing.
    ({"error": "busy"}, UCITS_ID, "its distributions matched no known layout"),
    ([UCITS_ANSWER], UCITS_ID, "its distributions matched no known layout"),
])
def test_distributions_that_do_not_come_leave_the_calendar_due_and_the_holdings_stand(tmp_path, caplog, answer,
                                                                                     isin, warning):
    with caplog.at_level(logging.WARNING, logger="refdata.invesco"):
        parsed = fetch(tmp_path, api(answer), isin=isin)
    assert warning in caplog.text
    assert read_meta(tmp_path)["facts_missing"] == [CAL]
    assert parsed.dividend_events == [] and len(parsed.lines) == 1 and not parsed.events_complete


def test_a_fund_that_wants_no_calendar_asks_for_no_distributions(tmp_path):
    client = api(US_ANSWER)
    parsed = fetch(tmp_path, client, wants=())
    assert not [u for u in client.urls() if u.endswith("/distribution")]
    assert not (tmp_path / "distributions.json").exists() and parsed.dividend_events == []


def test_both_facts_missing_are_named(tmp_path):
    fetch(tmp_path, api(500), wants=("characteristics", CAL))
    assert read_meta(tmp_path)["facts_missing"] == ["characteristics", CAL]


def test_a_day_not_in_iso_form_reads_as_an_issuer_writes_it():
    doc = {"distributions": [dist("09/21/2026", "21-Sep-2026", "Oct 08 2026", 0.25, ordinaryIncomeDistribution=0.25)]}
    (e,) = distribution_events(doc, "us", "symbol:XMPQQQ", DAY, provider="invesco")
    assert (e["ex_date"], e["record_date"], e["pay_date"]) == (date(2026, 9, 21), date(2026, 9, 21), date(2026, 10, 8))


def test_an_answer_in_no_layout_has_no_rows():
    assert distribution_rows(None, "us") is distribution_rows([UCITS_ANSWER], "ucits") is None
    assert distribution_rows({"distributions": None}, "us") == []
    # The answer's currency, else US dollars.
    doc = {"distributions": [dist("2026-09-21", "2026-09-21", "2026-10-08", 0.25, ordinaryIncomeDistribution=0.25)]}
    assert {r["currency"] for r in distribution_events(doc, "us", "symbol:XMPQQQ", DAY, provider="invesco")} == {
        "USD"}
