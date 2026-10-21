"""invesco: the figures a fund page reads from Invesco's data API, on
invented answers in the API's layout. Every fund, identifier and figure is
invented."""

import json
import logging
from datetime import date
from functools import partial

import pytest

from providers.base import Context, read_meta, write_meta
from providers.invesco import API, Invesco, characteristics
from tests.helpers import FakeClient, characteristics_by_key
from wanted import Fund

DAY = date(2026, 10, 12)
US_ID, UCITS_ID = "000000AF8", "XX0000000940"
HOLDINGS = {"effectiveBusinessDate": "2026-10-12", "holdings": [
    {"issuerName": "EXAMPLE CORP 4% 2031", "cusip": "000000AG6", "percentageOfTotalNetAssets": 100.0,
     "securityTypeName": "Corporate Bond", "currency": "USD"}]}


def us_bond():
    return {
        "characteristics": {
            "cusip": US_ID, "effectiveDate": "2026-10-09", "currencyCode": "USD",
            "unauditedEffectiveDuration": 5.84, "unauditedEffectiveDurationEffectiveDate": None,
            "unauditedEffectiveDurationIsInlineDate": False,
            "unauditedModifiedDuration": 5.71, "unauditedModifiedDurationEffectiveDate": None,
            "unauditedModifiedDurationIsInlineDate": False,
            "unauditedYieldToMaturity": 4.512, "unauditedYieldToMaturityEffectiveDate": None,
            "unauditedYieldToMaturityISInlineDate": False,
            "unauditedYieldToWorst": 4.497, "unauditedYieldToWorstEffectiveDate": "2026-10-08",
            "unauditedYieldToWorstIsInlineDate": True,
            "unauditedYearsToMaturity": 7.25, "unauditedYearsToMaturityEffectiveDate": None,
            "unauditedYearsToMaturityIsInlineDate": False,
            "unauditedWeightedAverageLifeToMaturity": 7.1, "unauditedWeightedAverageLifeToMaturityEffectiveDate": None,
            "unauditedWeightedAverageLifeToMaturityIsInlineDate": False,
            "unauditedWeightedAverageCoupon": 3.875, "unauditedWeightedAverageCouponEffectiveDate": None,
            "unauditedWeightedAverageCouponIsInlineDate": False},
        "yields": {
            "cusip": US_ID, "effectiveDate": "2026-10-10",
            "secYield30Day": 4.25, "secYield30DayEffectiveDate": None,
            "distributionYield": 4.105, "distributionYieldEffectiveDate": None,
            "twelveMonthDistributionRate": 4.02, "twelveMonthDistributionRateEffectiveDate": None,
            "secUnsubsidizedYield30Day": None, "secUnsubsidizedYield30DayEffectiveDate": None},
        "credit_quality": {"effectiveDate": "2026-09-30", "cusip": US_ID, "holdingWeights": [
            {"name": "AA", "value": 20.5}, {"name": "A", "value": 45.25}, {"name": "BBB", "value": 34.25},
            {"name": "BB", "value": 0}]},
        "maturity_breakdown": {"effectiveDate": "2026-09-30", "cusip": US_ID, "holdingWeights": [
            {"name": "0-1 Year", "value": 4.5}, {"name": "5-10 Years", "value": 95.5}]},
    }


def us_equity():
    return {
        "characteristics": {
            "cusip": US_ID, "effectiveDate": "2026-09-30", "currencyCode": "USD",
            "priceToEarningsRatio": 19.874, "priceToEarningsRatioEffectiveDate": None,
            "priceToEarningsRatioIsInlineDate": False,
            "forwardPriceToEarningsRatio": 16.1, "forwardPriceToEarningsRatioEffectiveDate": None,
            "forwardPriceToEarningsRatioIsInlineDate": False,
            "priceToBookRatio": 2.75, "priceToBookRatioEffectiveDate": None, "priceToBookRatioIsInlineDate": False,
            "weightedAverageMarketCapatilization": 88000000000,
            "weightedAverageMarketCapatilizationEffectiveDate": None,
            "weightedAverageMarketCapatilizationIsInlineDate": False},
        "yields": {
            "cusip": US_ID, "effectiveDate": "2026-10-10",
            "secYield30Day": 1.17, "secYield30DayEffectiveDate": None,
            "distributionYield": 0, "distributionYieldEffectiveDate": None,
            "twelveMonthDistributionRate": 1.21, "twelveMonthDistributionRateEffectiveDate": None,
            "secUnsubsidizedYield30Day": None, "secUnsubsidizedYield30DayEffectiveDate": None},
    }


def ucits_bond():
    return {
        "characteristics": {"effectiveDate": "2026-10-07", "effectiveMaturity": 9.12, "effectiveDuration": 7.03,
                            "averageQuality": "AA", "averageWeightedCoupon": 3.66},
        "yields": {"effectiveDate": "2026-10-07", "distributionYield": 3.91, "estimatedYieldToMaturity": 4.48},
        "credit_quality": {"isin": UCITS_ID, "effectiveDate": "2026-10-08", "holdingWeights": [
            {"name": "aA", "value": 100.6}, {"name": "cashand/orDerivatives", "value": -0.6}]},
        "maturity_breakdown": {"isin": UCITS_ID, "effectiveDate": "2026-10-08", "holdingWeights": [
            {"name": "5to10Years", "value": 100.6}, {"name": "cashand/orDerivatives", "value": -0.6}]},
    }


got = partial(characteristics_by_key, provider="invesco")


def test_a_us_bond_funds_answers():
    rows = characteristics(us_bond(), "symbol:XMPBND", DAY, provider="invesco")
    assert got(rows) == {
        # Dated by the characteristics answer's day, or a figure's own.
        ("effective_duration", ""): ("5.84", date(2026, 10, 9)),
        ("yield_to_maturity", ""): ("4.512", date(2026, 10, 9)),
        ("yield_to_worst", ""): ("4.497", date(2026, 10, 8)),
        ("weighted_average_maturity_years", ""): ("7.25", date(2026, 10, 9)),
        ("weighted_average_coupon", ""): ("3.875", date(2026, 10, 9)),
        # The yields answer: the distribution rate, and the 12-month rate
        # as the trailing yield; an unsubsidized yield of none is no row.
        ("sec_yield", ""): ("4.25", date(2026, 10, 10)),
        ("distribution_yield", ""): ("4.105", date(2026, 10, 10)),
        ("trailing_yield", ""): ("4.02", date(2026, 10, 10)),
        # Each breakdown by the page's labels; a part of no share is none.
        ("credit_quality", "AA"): ("20.5", date(2026, 9, 30)),
        ("credit_quality", "A"): ("45.25", date(2026, 9, 30)),
        ("credit_quality", "BBB"): ("34.25", date(2026, 9, 30)),
        ("maturity_breakdown", "0-1 Year"): ("4.5", date(2026, 9, 30)),
        ("maturity_breakdown", "5-10 Years"): ("95.5", date(2026, 9, 30)),
    }
    assert {r["payload"]["name"] for r in rows if r["field"] == "trailing_yield"} == {"twelveMonthDistributionRate"}


def test_a_us_equity_funds_answers_state_its_trailing_pe_and_yields():
    """The forward P/E is a forecast. A distribution rate of zero is none."""
    assert got(characteristics(us_equity(), "symbol:XMPEQF", DAY, provider="invesco")) == {
        ("price_earnings", ""): ("19.874", date(2026, 9, 30)),
        ("sec_yield", ""): ("1.17", date(2026, 10, 10)),
        ("trailing_yield", ""): ("1.21", date(2026, 10, 10))}


def test_a_ucits_bond_funds_answers_name_their_parts_by_key():
    assert got(characteristics(ucits_bond(), "isin:XX0000000940", DAY, provider="invesco")) == {
        ("effective_duration", ""): ("7.03", date(2026, 10, 7)),
        ("weighted_average_maturity_years", ""): ("9.12", date(2026, 10, 7)),
        ("weighted_average_coupon", ""): ("3.66", date(2026, 10, 7)),
        ("yield_to_maturity", ""): ("4.48", date(2026, 10, 7)),
        ("distribution_yield", ""): ("3.91", date(2026, 10, 7)),
        ("credit_quality", "aA"): ("100.6", date(2026, 10, 8)),
        ("credit_quality", "cashand/orDerivatives"): ("-0.6", date(2026, 10, 8)),
        ("maturity_breakdown", "5to10Years"): ("100.6", date(2026, 10, 8)),
        ("maturity_breakdown", "cashand/orDerivatives"): ("-0.6", date(2026, 10, 8)),
    }


def test_a_bond_funds_pe_is_none():
    """The answers name no asset class: a fund whose characteristics state
    a bond figure is a bond fund."""
    docs = us_bond()
    docs["characteristics"]["priceToEarningsRatio"] = 14.2
    assert ("price_earnings", "") not in got(characteristics(docs, "symbol:XMPBND", DAY, provider="invesco"))


def test_a_loan_funds_own_figures_and_the_order_of_keys():
    loans = {"effectiveDate": "2026-10-09", "bankLoanYieldToMaturity": 7.85, "bankLoanWeightedAverageMaturity": 4.6,
             "bankLoanWeightedAverageCoupon": 7.4, "bankLoanSpreadVsSOFR": 315}
    assert got(characteristics({"characteristics": loans}, "symbol:XMPBND", DAY, provider="invesco")) == {
        ("yield_to_maturity", ""): ("7.85", date(2026, 10, 9)),
        ("weighted_average_maturity_years", ""): ("4.6", date(2026, 10, 9)),
        ("weighted_average_coupon", ""): ("7.4", date(2026, 10, 9))}
    # A bond's figure comes before a loan's.
    both = {**loans, "unauditedYieldToMaturity": 5.1}
    rows = characteristics({"characteristics": both}, "symbol:XMPBND", DAY, provider="invesco")
    assert {r["value"] for r in rows if r["field"] == "yield_to_maturity"} == {"5.1"}


def test_answers_that_state_no_day_or_no_figure():
    docs = {"yields": {"secYield30Day": 1.2}, "characteristics": {"unauditedEffectiveDuration": "-"}}
    assert got(characteristics(docs, "symbol:XMPEQF", DAY, provider="invesco")) == {("sec_yield", ""): ("1.2", DAY)}
    assert characteristics({}, "symbol:XMPEQF", DAY, provider="invesco") == []
    assert characteristics({"yields": [], "characteristics": "busy"}, "symbol:XMPEQF", DAY, provider="invesco") == []


def api(answers, *, fails=()):
    """Invesco's API for one fund: the holdings, no index or swaps, and its
    page answers by name; a name in `fails` answers 500."""
    def answer(url, params):
        if "/holdings/" in url or url.endswith("/swaps"):
            return HOLDINGS if url.endswith("/holdings/fund") else 404
        name = {"creditRating": "credit_quality", "effectiveMaturity": "maturity_breakdown"}.get(
            params.get("breakdown"), {"fundCharacteristics": "characteristics", "portfolioInformation":
                                      "characteristics"}.get(params.get("variationType"), "yields"))
        return 500 if name in fails else answers.get(name, 404)
    return FakeClient({"dng-api.invesco.com": answer})


def fetch(tmp_path, client, *, wants=("characteristics",), ref="symbol:XMPBND", isin=None):
    ctx = Context(client=client)
    inv = Invesco()
    fund = Fund(ref=ref, isin=isin, cusip=None if isin else US_ID, name="Invesco Example ETF", wants=wants)
    claim = inv.claim(fund, ctx)
    assert inv.fetch(claim, None, tmp_path, ctx) == DAY
    return inv.parse(tmp_path)


def asked(client) -> list:
    """What each request asked for: the holdings, a variation or a
    breakdown."""
    return [(data or {}).get("breakdown") or (data or {}).get("variationType") or url.rsplit("/", 2)[-2]
            for _, url, data in client.asked]


def test_a_bond_funds_fetch_keeps_its_page_answers(tmp_path):
    client = api(us_bond())
    parsed = fetch(tmp_path, client)
    assert asked(client) == ["holdings", "fundCharacteristics", "yieldInformation", "creditRating",
                             "effectiveMaturity"]
    assert client.urls()[1] == API.format(locale="en_US", id=US_ID).rstrip("/")
    meta = read_meta(tmp_path)
    assert meta["pages"] == ["characteristics", "credit_quality", "maturity_breakdown", "yields"]
    assert "facts_missing" not in meta
    assert json.loads((tmp_path / "page-yields.json").read_text()) == us_bond()["yields"]
    assert len(parsed.lines) == 1
    assert got(parsed.characteristics) == got(characteristics(us_bond(), "symbol:XMPBND", DAY, provider="invesco"))


def test_an_equity_funds_fetch_asks_for_no_breakdown(tmp_path):
    client = api(us_equity())
    parsed = fetch(tmp_path, client)
    assert asked(client) == ["holdings", "fundCharacteristics", "yieldInformation"]
    assert read_meta(tmp_path)["pages"] == ["characteristics", "yields"]
    assert ("price_earnings", "") in got(parsed.characteristics)


def test_a_ucits_funds_fetch_asks_by_isin(tmp_path):
    client = api(ucits_bond())
    parsed = fetch(tmp_path, client, ref="isin:XX0000000940", isin=UCITS_ID)
    assert asked(client) == ["holdings", "holdings", "counterParties", "portfolioInformation", "yieldInformation",
                             "creditRating", "effectiveMaturity"]
    assert all((data or {}).get("idType", "isin") == "isin" for _, _, data in client.asked)
    assert ("credit_quality", "aA") in got(parsed.characteristics)


def test_a_fund_that_wants_no_characteristics_asks_for_no_page_answer(tmp_path):
    client = api(us_bond())
    parsed = fetch(tmp_path, client, wants=())
    assert asked(client) == ["holdings"]
    assert read_meta(tmp_path)["pages"] == [] and "facts_missing" not in read_meta(tmp_path)
    assert parsed.lines and parsed.characteristics == []


@pytest.mark.parametrize("answers,fails,pages,warning", [
    pytest.param(us_bond(), ("characteristics", "yields"), [], "did not come", id="none-came"),
    pytest.param({"characteristics": "<html>busy</html>"}, ("yields",), [], "did not come", id="not-json"),
    pytest.param({"characteristics": [], "yields": []}, (), ["characteristics", "yields"],
                 "its page's answers matched no known layout", id="no-object"),
])
def test_answers_that_do_not_come_or_are_no_fund_pages_leave_the_characteristics_due(tmp_path, caplog, answers,
                                                                                      fails, pages, warning):
    with caplog.at_level(logging.WARNING, logger="refdata.invesco"):
        parsed = fetch(tmp_path, api(answers, fails=fails))
    assert warning in caplog.text
    assert read_meta(tmp_path)["pages"] == pages
    assert read_meta(tmp_path)["facts_missing"] == ["characteristics"]
    assert parsed.lines and parsed.characteristics == []


def test_answers_that_state_no_figure_answer_the_characteristics(tmp_path, caplog):
    """An accumulating UCITS equity fund's answers: the fund has none of
    the figures, so none stay due."""
    with caplog.at_level(logging.WARNING, logger="refdata.invesco"):
        parsed = fetch(tmp_path, api({"characteristics": {"effectiveDate": "2026-10-09"}, "yields": {}}))
    assert not caplog.text
    assert read_meta(tmp_path)["pages"] == ["characteristics", "yields"] and "facts_missing" not in read_meta(tmp_path)
    assert parsed.lines and parsed.characteristics == []


def test_a_folder_fetched_before_page_answers_parses_its_holdings(tmp_path):
    """A meta.json that names no page answers, as an older fetch wrote it."""
    fetch(tmp_path, api(us_bond()), wants=())
    meta = read_meta(tmp_path)
    del meta["pages"]
    write_meta(tmp_path, **meta)
    parsed = Invesco().parse(tmp_path)
    assert len(parsed.lines) == 1 and parsed.characteristics == []


def test_a_breakdown_that_does_not_come_leaves_the_figures(tmp_path, caplog):
    with caplog.at_level(logging.WARNING, logger="refdata.invesco"):
        parsed = fetch(tmp_path, api(us_bond(), fails=("credit_quality",)))
    assert "its page's credit quality did not come" in caplog.text
    assert "facts_missing" not in read_meta(tmp_path)
    assert {f for f, _ in got(parsed.characteristics)} == {
        "effective_duration", "yield_to_maturity", "yield_to_worst", "weighted_average_maturity_years",
        "weighted_average_coupon", "sec_yield", "distribution_yield", "trailing_yield", "maturity_breakdown"}
