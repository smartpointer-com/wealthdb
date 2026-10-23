"""vanguard: the distributions of a US ETF from its profile page, on an
invented page in the site's layout. Every day and amount is invented."""

import html
from datetime import date
from pathlib import Path

import pytest

import config
from providers import REGISTRY
from providers.base import Context, ProviderError, read_meta, write_meta
from providers.vanguard import PAGE, Vanguard, distribution_rows, events
from tests.helpers import FakeClient, events_by_id, fund, plan, wanted_doc
from wanted import Fund

FIXTURES = Path(__file__).parent / "fixtures"
PAGE_HTML = (FIXTURES / "vanguard_us_page.html").read_text()
CAL = "dividend_calendar"
DAY = date(2026, 10, 21)
QVEX = Fund(ref="symbol:QVEX", symbol="QVEX", name="VANGUARD EXAMPLE FUNDS EQUITY ETF", wants=(CAL,))


def fetch(tmp_path, monkeypatch, page=PAGE_HTML, f=QVEX):
    monkeypatch.setattr("providers.vanguard.today_utc", lambda: DAY)
    client = FakeClient({"investor.vanguard.com": page})
    ctx = Context(client=client)
    v = Vanguard()
    claim = v.claim(f, ctx)
    assert v.fetch(claim, None, tmp_path, ctx) == DAY
    return client, v.parse(tmp_path)


EVENTS = {
    "dividend|2026-09-15|2026-09-15|2026-09-21": ("dividend", date(2026, 9, 15), date(2026, 9, 15),
                                                  date(2026, 9, 21), "0.214", "USD"),
    "dividend|2026-06-16|2026-06-16|2026-06-22": ("dividend", date(2026, 6, 16), date(2026, 6, 16),
                                                  date(2026, 6, 22), "0.1985", "USD"),
    "dividend|2025-12-15|2025-12-15|2025-12-19": ("dividend", date(2025, 12, 15), date(2025, 12, 15),
                                                  date(2025, 12, 19), "0.2602", "USD"),
    # The short- and long-term gains of one day are one capital gain.
    "capital_gain|2025-12-15|2025-12-15|2025-12-19": ("capital_gain", date(2025, 12, 15), date(2025, 12, 15),
                                                      date(2025, 12, 19), "0.075", "USD"),
    "return_of_capital|2025-03-10|2025-03-10|2025-03-14": ("return_of_capital", date(2025, 3, 10),
                                                           date(2025, 3, 10), date(2025, 3, 14), "0.012", "USD"),
}


def test_a_us_etf_states_its_distributions_from_its_profile_page(tmp_path, monkeypatch):
    client, parsed = fetch(tmp_path, monkeypatch)
    assert client.urls() == [PAGE.format(ticker="qvex")]
    assert (tmp_path / "page.html").read_text() == PAGE_HTML
    meta = read_meta(tmp_path)
    assert (meta["ref"], meta["symbol"], meta["as_of"]) == ("symbol:QVEX", "QVEX", "2026-10-21")
    # The reinvest day is the ex day; a row without a day is none.
    assert events_by_id(parsed.dividend_events) == EVENTS
    assert {(r["ref"], r["provider"], r["as_of"], r["status"]) for r in parsed.dividend_events} == {
        ("symbol:QVEX", "vanguard", DAY, None)}
    gain = next(r for r in parsed.dividend_events if r["kind"] == "capital_gain")
    assert gain["payload"] == {"parts": [
        {"amount": "0.03", "currency": "USD", "type": "Short-term capital gain", "reinvest_price": "47.65"},
        {"amount": "0.045", "currency": "USD", "type": "Long-term capital gain", "reinvest_price": "47.65"}]}
    one = parsed.dividend_events[0]
    assert set(one["payload"]) <= {"type", "reinvest_price"} and one["payload"]["type"]
    # The page lists every distribution of its months: load drops what a
    # later page no longer states.
    assert parsed.events_complete
    assert parsed.compositions == [] and parsed.lines == []


def test_a_state_written_html_escaped_reads_the_same(tmp_path, monkeypatch):
    state = PAGE_HTML.split('type="application/json">', 1)[1].split("</script>", 1)[0]
    page = f'<html><body><div id="app" data-state="{html.escape(state)}"></div></body></html>'
    assert events_by_id(fetch(tmp_path, monkeypatch, page=page)[1].dividend_events) == EVENTS


def test_a_fund_without_distributions_answers_its_calendar(tmp_path, monkeypatch):
    page = '<script type="application/json">{"distributions":{"incomeCapitalGains":[]}}</script>'
    assert fetch(tmp_path, monkeypatch, page=page)[1].dividend_events == []


@pytest.mark.parametrize("page", ["<html>busy</html>", '<script>{"distributions":{"incomeCapitalGains":null}}</script>',
                                  '<script>{"incomeCapitalGains": [{"type": </script>'])
def test_a_page_without_distributions_is_refused_and_not_stored(tmp_path, page):
    v = Vanguard()
    ctx = Context(client=FakeClient({"investor.vanguard.com": page}))
    with pytest.raises(ProviderError, match="states no distributions"):
        v.fetch({"ref": "symbol:QVEX", "symbol": "QVEX"}, None, tmp_path / "f", ctx)
    assert not (tmp_path / "f").exists()


def test_a_kept_page_without_distributions_does_not_parse(tmp_path):
    write_meta(tmp_path, provider="vanguard", ref="symbol:QVEX", symbol="QVEX", as_of=DAY.isoformat())
    (tmp_path / "page.html").write_text("<html>busy</html>")
    with pytest.raises(ProviderError, match="the page states no distributions"):
        Vanguard().parse(tmp_path)


@pytest.mark.parametrize("f,claimed", [
    (QVEX, True),
    (Fund(ref="symbol:QVGR", symbol="QVGR", name="Vanguard Example Growth ETF"), True),
    # A mutual fund, another issuer's ETF, a fund without a US ticker.
    (Fund(ref="symbol:VQXAX", symbol="VQXAX", name="Vanguard Example Index Fund Admiral Shares"), False),
    (Fund(ref="symbol:SPY", symbol="SPY", name="SPDR Example ETF Trust"), False),
    (Fund(ref="isin:XX0000000990", isin="XX0000000990", symbol="QVUS", name="Vanguard Example UCITS ETF"), False),
    (Fund(ref="symbol:QVET", symbol="QVET"), False),
])
def test_a_claim_is_a_us_etf_whose_name_names_vanguard(f, claimed):
    client = FakeClient()
    claim = Vanguard().claim(f, Context(client=client))
    assert (claim is not None) == claimed and client.asked == []


def test_rows_of_one_kind_and_day_sum_and_others_read_as_written():
    rows = [{"type": "Income dividend", "share": 0.25, "reInvestDate": "2026-09-15", "recordDate": "2026-09-15",
             "payableDate": "2026-09-21T00:00:00"},
            {"type": "Special", "share": "$1.00", "reInvestDate": "09/15/2026"},
            # A special dividend is special, though its type names a
            # dividend too.
            {"type": "Special dividend", "share": "$0.50", "reInvestDate": "09/15/2026"},
            {"type": "Supplemental", "share": "$0.00", "reInvestDate": "09/15/2026"}]
    assert events_by_id(events(rows, "symbol:QVEX", DAY, provider="vanguard")) == {
        "dividend|2026-09-15|2026-09-15|2026-09-21": ("dividend", date(2026, 9, 15), date(2026, 9, 15),
                                                      date(2026, 9, 21), "0.25", "USD"),
        "special|2026-09-15||": ("special", date(2026, 9, 15), None, None, "1.5", "USD"),
        "other|2026-09-15||": ("other", date(2026, 9, 15), None, None, None, None)}
    assert distribution_rows("") is None
    # A short- and a long-term gain of one amount are two parts, not one
    # row stated twice.
    gains = [{"type": f"{term}-term capital gain", "share": "0.01", "reInvestDate": "12/15/2025"}
             for term in ("Short", "Long")]
    (gain,) = events(gains, "symbol:QVEX", DAY, provider="vanguard")
    assert gain["amount"] == "0.02" and len(gain["payload"]["parts"]) == 2


def test_vanguard_answers_the_calendar_alone_and_is_off_until_named():
    v = REGISTRY["vanguard"]
    assert (v.facts, v.serves_compositions, v.serves_securities) == (frozenset({CAL}), False, False)
    assert "vanguard" not in config.DEFAULT_PROVIDERS
    held = {**fund("symbol:QVEX", "QVEX", compositions=["2026-07-10"]), "name": "Vanguard Example Equity ETF",
            "wants": [CAL], "facts": {}}
    # A request for the calendar alone.
    reqs, _, cover = plan(wanted_doc([held], version=3), [v])
    assert [(r.kind, r.provider, r.ref, r.as_of, r.facts, r.compositions) for r in reqs] == [
        ("fund", "vanguard", "symbol:QVEX", None, (CAL,), False)]
    assert cover[CAL]["symbol:QVEX"].requests == reqs
    # A fund that wants its composition is not offered it.
    reqs, unclaimed, _ = plan(wanted_doc([{**held, "compositions": []}], version=3), [v])
    assert unclaimed == ["symbol:QVEX"] and [r.compositions for r in reqs] == [False]
