"""fidelity-529: a plan portfolio's schedule read from the plan's annual
report, each fund named by the ticker of its SEC series. Every portfolio,
fund, figure and identifier is invented."""

from datetime import date
from decimal import Decimal

import pytest

import config
import fetcher
from providers.base import Context, ProviderError, Unavailable, today_utc
from providers.fidelity529 import SERIES, Fidelity529, parse_schedule, schedule
from tests.helpers import pdf
from wanted import Fund

# Two portfolios' schedules, each on its page, the title in the footer.
# A name wraps; a money market fund has a footnote mark and a yield.
INDEX = ("Schedule of Investments September 30, 2025\nShowing Percentage of Net Assets\n"
         "Domestic Equity Funds – 60.0%\nShares Value ($)\nQumbra Series Total Market Index Fund\n"
         " (Cost $512,300) 41,200 600,000\nInternational Equity Funds – 39.0%\nShares Value ($)\n"
         "Qumbra Series Global ex U.S. Index\nFund 37,913 390,000\nMoney Market Funds – 1.0%\n"
         "Yield (%) Shares Value ($)\nQumbra Series Government Money Market Fund\n(a)\n"
         " (Cost $10,000) 4.10 10,000 10,000\nTOTAL INVESTMENT IN SECURITIES – 100.0%\n (Cost $912,300) 1,000,000\n"
         "NET OTHER ASSETS (LIABILITIES) – 0.0% (110)\nNET ASSETS – 100.0% 1,000,000\n"
         "See accompanying notes which are an integral part of the financial statements.\n"
         "QQ Qland Horizon Portfolio (Qumbra Index)\nSchedule of Investments \n75 Annual Report")
OTHER = INDEX.replace("QQ Qland Horizon Portfolio (Qumbra Index)", "QQ Qland Horizon Portfolio (Qumbra Funds)")
PAGES = ["Annual Report\nSeptember 30, 2025", OTHER, INDEX]
SERIES_ROWS = {
    "Qumbra Series Total Market Index Fund": [{"series": "S000000001", "tickers": ["QTMXX"]}],
    "Qumbra Series Global ex U.S. Index Fund": [{"series": "S000000002", "tickers": ["QGXUS"]}],
    # Two series of one name: no ticker is the right one.
    "Qumbra Series Government Money Market Fund": [{"series": "S000000003", "tickers": ["QGM1X"]},
                                                   {"series": "S000000004", "tickers": ["QGM2X"]}],
}


def test_a_portfolio_is_found_by_the_brokers_name_and_read_with_its_funds():
    day, holdings, net = schedule(PAGES, "QQ QLAND HORIZON PORT (QUMBRA INDX)")
    assert (day, net) == (date(2025, 9, 30), Decimal(1000000))
    assert [(h["name"], h["section"], h["value"]) for h in holdings] == [
        ("Qumbra Series Total Market Index Fund", "Domestic Equity Funds", 600000),
        ("Qumbra Series Global ex U.S. Index Fund", "International Equity Funds", 390000),
        ("Qumbra Series Government Money Market Fund", "Money Market Funds", 10000)]


def test_each_fund_takes_the_ticker_of_its_one_series():
    p = parse_schedule(PAGES, "QQ QLAND HORIZON PORT (QUMBRA INDX)", "symbol:QQQ000001", SERIES_ROWS,
                       provider="fidelity-529")
    (c,) = p.compositions
    assert (c["as_of"], c["line_count"], c["net_assets"], c["source_kind"]) == (date(2025, 9, 30), 3, "1000000", "report")
    assert [(ln["ticker"], ln["weight"], ln["payload"]) for ln in p.lines] == [
        ("QTMXX", "60", {"series": "S000000001"}), ("QGXUS", "39", {"series": "S000000002"}), (None, "1", {})]


def test_a_portfolio_the_report_does_not_hold_is_refused():
    with pytest.raises(ProviderError, match="no schedule"):
        schedule(PAGES, "QQ QLAND INCOME PORT (QUMBRA INDX)")


CSV = ("Reporting File Number,CIK Number,Entity Name,Entity Org Type,Series ID,Series Name,Class ID,Class Name,"
       "Class Ticker\n"
       "811-1,0000000001,Qumbra Trust,30,S000000001,Qumbra Series Total Market Index Fund,C1,Class,QTMXX\n"
       "811-1,0000000001,Qumbra Trust,30,S000000002,Qumbra Series Global ex U.S. Index Fund,C2,Class,QGXUS\n"
       "811-1,0000000001,Qumbra Trust,30,S000000002,Qumbra Series Global ex U.S. Index Fund,C3,Class B,\n")


REPORT = "https://www.example.com/qq-annual-report.pdf"
PLANS = config.Config(fidelity_529_plans=(("QQ", REPORT),))


class Site:
    def __init__(self, report, series=CSV):
        self.report, self.series, self.asked = report, series, []

    def get(self, url, params=None, headers=None):
        self.asked.append(url)
        if url == REPORT:
            return fetcher.Response(url, 200, self.report, "application/pdf")
        if url == SERIES.format(year=today_utc().year):
            raise fetcher.FetchError(url, 404)  # this year's list is not out yet
        return fetcher.Response(url, 200, self.series.encode(), "text/csv")


def test_the_plan_is_claimed_by_name_and_fetched_with_the_series_its_funds_belong_to(tmp_path):
    # The SEC spells one name otherwise: its words still find it.
    site = Site(pdf([p.splitlines() for p in PAGES]), CSV.replace("Total Market", "Total-Market"))
    ctx = Context(client=site, config=PLANS)
    f = Fidelity529()
    claim = f.claim(Fund(ref="symbol:QQQ000001", symbol="QQQ000001", name="QQ QLAND HORIZON PORT (QUMBRA INDX)"), ctx)
    assert claim == {"ref": "symbol:QQQ000001", "plan": "QQ", "report": REPORT,
                     "name": "QQ QLAND HORIZON PORT (QUMBRA INDX)"}
    # Another plan's portfolio, or a fund that is no portfolio, is not claimed.
    assert f.claim(Fund(ref="symbol:QX", symbol="QX", name="ZZ QLAND HORIZON PORT"), ctx) is None
    assert f.claim(Fund(ref="symbol:QY", symbol="QY", name="QQ QLAND BOND FUND"), ctx) is None
    # A prefix configured in another case claims alike.
    lower = Context(client=site, config=config.Config(fidelity_529_plans=(("qq", REPORT),)))
    assert f.claim(Fund(ref="symbol:QQQ000001", name="QQ QLAND HORIZON PORT"), lower)["plan"] == "qq"
    # With no plan configured, nothing is claimed.
    assert f.claim(Fund(ref="symbol:QQQ000001", name="QQ QLAND HORIZON PORT"), Context(client=site)) is None
    assert site.asked == []
    assert f.fetch(claim, None, tmp_path / "f", ctx) == date(2025, 9, 30)
    p = f.parse(tmp_path / "f")
    assert [ln["ticker"] for ln in p.lines] == ["QTMXX", "QGXUS", None]
    with pytest.raises(Unavailable):
        f.fetch(claim, date(2025, 9, 30), tmp_path / "g", ctx)


def test_two_schedules_on_a_page_and_one_over_two_pages_read_apart():
    # Both schedules on one page: each is read up to its own footer.
    page = OTHER.replace("75 Annual Report", "") + "\n" + INDEX
    for name in ("QQ QLAND HORIZON PORT (QUMBRA INDX)", "QQ QLAND HORIZON PORT (QUMBRA FUNDS)"):
        _, held, _ = schedule(["Annual Report", page], name)
        assert len(held) == 3, name
    # A schedule whose last lines run onto the next page.
    first, last = INDEX.split("Money Market Funds – 1.0%\n")
    pages = ["Annual Report", first + "QQ Qland Horizon Portfolio (Qumbra Index)\nSchedule of Investments \n",
             "Money Market Funds – 1.0%\n" + last]
    _, held, _ = schedule(pages, "QQ QLAND HORIZON PORT (QUMBRA INDX)")
    assert [h["name"] for h in held][-1] == "Qumbra Series Government Money Market Fund"


def test_holdings_that_miss_the_printed_total_are_refused():
    # A wrapped name that ends in two numbers reads as a holding: the
    # total gives it away.
    page = INDEX.replace("Qumbra Series Total Market Index Fund\n", "Qumbra Target 2025 2030\nFund\n")
    with pytest.raises(ProviderError, match="add up to"):
        schedule(["Annual Report", page], "QQ QLAND HORIZON PORT (QUMBRA INDX)")


@pytest.mark.parametrize("dash", ["—", "-"])
def test_dashes_and_a_dollar_sign_read_alike(dash):
    page = INDEX.replace("Funds – ", f"Funds {dash} ").replace("NET ASSETS – 100.0% 1,000,000",
                                                              f"NET ASSETS {dash} 100.0% $ 1,000,000")
    _, held, net = schedule(["Annual Report", page], "QQ QLAND HORIZON PORT (QUMBRA INDX)")
    assert (len(held), net) == (3, Decimal(1000000))


def test_the_total_may_end_its_own_line():
    page = INDEX.replace("TOTAL INVESTMENT IN SECURITIES – 100.0%\n (Cost $912,300) 1,000,000",
                         "TOTAL INVESTMENT IN SECURITIES – 100.0% (Cost $912,300) $ 1,000,000")
    assert len(schedule(["Annual Report", page], "QQ QLAND HORIZON PORT (QUMBRA INDX)")[1]) == 3
    with pytest.raises(ProviderError, match="add up to"):
        schedule(["Annual Report", page.replace("$ 1,000,000", "$ 990,000")], "QQ QLAND HORIZON PORT (QUMBRA INDX)")


def test_the_sec_list_falls_back_a_year_only_while_this_years_is_not_out(tmp_path):
    from providers.fidelity529 import _series

    class Sec:
        def __init__(self, status):
            self.status = status

        def get(self, url, params=None, headers=None):
            if url == SERIES.format(year=today_utc().year):
                raise fetcher.FetchError(url, self.status)
            return fetcher.Response(url, 200, CSV.encode(), "text/csv")
    exact, _ = _series(Context(client=Sec(404)))
    assert exact["qumbra series total market index fund"] == [{"series": "S000000001", "tickers": ["QTMXX"]}]
    with pytest.raises(fetcher.FetchError):
        _series(Context(client=Sec(503)))

    class Empty:
        def get(self, url, params=None, headers=None):
            return fetcher.Response(url, 200, b"Series Name,Series ID\n", "text/csv")
    with pytest.raises(ProviderError, match="no fund series"):
        _series(Context(client=Empty()))


def test_the_probe_asks_for_a_configured_report():
    site = Site(pdf([p.splitlines() for p in PAGES]))
    assert Fidelity529().probe(Context(client=site, config=PLANS)) == "the QQ plan's report answered 200 with a PDF"
    assert site.asked == [REPORT]
    with pytest.raises(ProviderError, match="no plan"):
        Fidelity529().probe(Context(client=site))
