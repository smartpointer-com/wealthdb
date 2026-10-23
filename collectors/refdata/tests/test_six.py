"""six: a Swiss or Liechtenstein share's dividends from SIX's share page,
the pay days of SIX's ex-dividend notices, one search a run, and the
two joined by ex day, also across runs."""

import json
from datetime import date, timedelta
from pathlib import Path

import pytest

import download
import fetcher
import load
import wanted as wantedmod
from collectorkit import bronze
from providers import REGISTRY, six
from providers.base import Context, Provider, ProviderError, read_meta
from tests.helpers import TODAY, FakeClient, kept_run, make_run, rows, run_entries, wanted_doc

FIX = Path(__file__).parent / "fixtures"
CAL = "dividend_calendar"
KEY, LI_KEY = "isin:CH0000000001", "isin:LI0000000002"
DIVIDENDS = "/share_details/v3/CH0000000001CHF4/share/dividend.json"
# What a notice's details say of the issuer's contact: none of it may
# reach a file or the log.
CONTACT = ("Jane Example", "John Example", "+41 00", "+423 00", "Contact")


def fixture(name):
    return json.loads((FIX / name).read_text())


def share(key=KEY, isin=None, currency=None, wants=(CAL,), **kw):
    isin = isin or key.removeprefix("isin:")
    return wantedmod.Security(key=key, isin=isin, currency=currency, asset_class="public_equity", wants=wants,
                              **kw)


def routes(**over):
    """A SIX that lists the CH share's dividends, and finds six notices:
    one of each claimed share, one that names two ISINs, one of a share
    no wanted list names, one that is no ex-dividend notice and two that
    name no ISIN. It serves the details of those a run may ask for."""
    return {DIVIDENDS: fixture("six_dividend.json"), "/find.json": fixture("six_notices.json"),
            "/details/900101.json": fixture("six_notice_details.json"),
            "/details/900103.json": fixture("six_notice_details_html.json"),
            "/details/900105.json": fixture("six_notice_details_no_isin.json"), **over}


@pytest.fixture(autouse=True)
def today(monkeypatch):
    """The day a fetch states: TODAY, or the day a test sets."""
    day = [TODAY]
    monkeypatch.setattr(six, "today_utc", lambda: day[0])
    return day


def by_ex(events):
    return {e["ex_date"]: e for e in events}


def test_a_swiss_or_liechtenstein_share_held_directly_is_claimed_while_its_calendar_is_due():
    p = six.Six()
    assert p.claim_security(share(), None) == {"key": KEY, "isin": "CH0000000001", "currency": "CHF", "facts": [CAL]}
    assert p.claim_security(share(LI_KEY, currency="eur"), None)["currency"] == "EUR"
    assert p.claim_security(share("symbol:QSHR", isin="CH0000000001"), None)["isin"] == "CH0000000001"
    for other in (share(wants=()), share("isin:US0000000001"), share(receipt=True), share(scope="constituent"),
                  wantedmod.Security(key=KEY, isin="CH0000000001", asset_class="fund", wants=(CAL,))):
        assert p.claim_security(other, None) is None


def test_a_shares_dividends_are_one_event_per_ex_day_the_oldest_too(tmp_path):
    client = FakeClient(routes())
    p = six.Six()
    out = tmp_path / "out"
    p.fetch_security(p.claim_security(share(), None), out, Context(client=client, data_dir=tmp_path / "data"))
    assert client.urls() == ["https://www.six-group.com/sheldon" + DIVIDENDS]
    assert read_meta(out) == {"provider": "six", "key": KEY, "isin": "CH0000000001", "currency": "CHF",
                              "listed": True, "as_of": TODAY.isoformat()}
    events = by_ex(p.parse(out).dividend_events)
    assert sorted(events) == [date(2009, 4, 15), date(2024, 3, 12), date(2025, 3, 11), date(2026, 3, 10)]
    newest = events[date(2026, 3, 10)]
    assert (newest["ref"], newest["provider"], newest["event_id"], newest["as_of"], newest["kind"]) == \
        (KEY, "six", "dividend|2026-03-10||", TODAY, "dividend")
    assert (newest["amount"], newest["currency"], newest["record_date"], newest["pay_date"], newest["status"]) == \
        ("2.5", "CHF", None, None, None)
    # The source's other fields go to the payload; a split since has
    # adjusted the oldest.
    assert newest["payload"] == {"adjustedValue": 2.5}
    assert events[date(2009, 4, 15)]["payload"] == {"adjustedValue": 0.48}
    # Two rows of one ex day, a part of it a capital repayment, are one
    # payment.
    both = events[date(2024, 3, 12)]
    assert both["amount"] == "2.3" and [part.get("capitalRepayment") for part in both["payload"]["parts"]] == \
        [1.15, None]


def test_a_bare_list_of_rows_reads_alike_and_one_without_an_ex_day_is_none():
    rows = [{"exDividendDate": "2026-03-10", "value": 1}, {"value": 2}]
    (event,) = six.dividend_events(six.records(rows, six.EX_KEY), [], KEY, "CHF", TODAY, provider="six")
    assert (event["ex_date"], event["amount"], event["currency"]) == (date(2026, 3, 10), "1", "CHF")


def test_a_notice_gives_the_row_of_its_ex_day_its_pay_day_and_fills_what_the_row_lacks():
    notices = [{"id": "900101", "isin": "CH0000000001",
                "fields": {"Ex Date": "10.03.2026", "Payment Date": "12.03.2026",
                           "Gross amount payable per security": "2.50", "Currency Code": "CHF"}},
               # A later notice of the same ex day wins.
               {"id": "900120", "isin": "CH0000000001", "fields": {"Ex Date": "10.03.2026",
                                                                  "Payment Date": "13.03.2026"}}]
    rows = [{"exDividendDate": 20260310}, {"exDividendDate": 20250311, "value": 1, "currency": "EUR"},
            {"exDividendDate": 20250311, "value": 2, "currency": "CHF"}]
    events = by_ex(six.dividend_events(rows, notices, KEY, "USD", TODAY, provider="six"))
    newest = events[date(2026, 3, 10)]
    assert (newest["pay_date"], newest["amount"], newest["currency"], newest["payload"]) == \
        (date(2026, 3, 13), None, "USD", {"notice": "900120"})
    # The first notice states the amount; the later one, which wins, none.
    assert six.dividend_events(rows[:1], notices[:1], KEY, "USD", TODAY, provider="six")[0]["amount"] == "2.5"
    # Parts in two currencies are not summed: the first one stands.
    older = events[date(2025, 3, 11)]
    assert (older["amount"], older["currency"], older["pay_date"]) == ("1", "EUR", None)


def test_a_share_six_does_not_list_answers_no_dividend(tmp_path, caplog):
    p = six.Six()
    out = tmp_path / "out"
    p.fetch_security(p.claim_security(share(), None), out, Context(client=FakeClient({}), data_dir=tmp_path))
    assert read_meta(out)["listed"] is False and not (out / six.DIVIDEND_DOC).exists()
    assert p.parse(out).dividend_events == [] and "SIX lists no share CH0000000001 in CHF" in caplog.text


def test_any_other_failure_fails_the_fetch(tmp_path):
    p = six.Six()
    with pytest.raises(fetcher.FetchError):
        p.fetch_security(p.claim_security(share(), None), tmp_path / "out",
                         Context(client=FakeClient({DIVIDENDS: 503}), data_dir=tmp_path))


def test_an_answer_in_no_known_layout_is_kept_and_counts_as_had(tmp_path, caplog):
    p = six.Six()
    out = tmp_path / "out"
    client = FakeClient({DIVIDENDS: {"rows": [{"exDate": 20260310}]}})
    p.fetch_security(p.claim_security(share(), None), out, Context(client=client, data_dir=tmp_path))
    assert (out / six.DIVIDEND_DOC).is_file() and "facts_missing" not in read_meta(out)
    assert p.parse(out).dividend_events == [] and "matched no known layout" in caplog.text
    # An answer that holds no list, or empty ones, states no dividend.
    assert six.records({}, six.EX_KEY) == [] and six.records({"dividends": []}, six.EX_KEY) == []


def test_one_search_a_run_reaches_back_a_year_then_from_the_last_search():
    p = six.Six()
    secs = [share(), share(LI_KEY), share("isin:CH0000000009", receipt=True), share("isin:US0000000001"),
            share("isin:CH0000000008", wants=())]
    (ref, claim), = p.plan_bulk(secs, None, TODAY, {}, 30)
    assert ref == six.NOTICES_REF == claim["ref"]
    assert claim == {"ref": ref, "first": (TODAY - timedelta(days=366)).isoformat(), "last": TODAY.isoformat(),
                     "isins": {"CH0000000001": [KEY], "LI0000000002": [LI_KEY]}, "keys": [KEY, LI_KEY],
                     "facts": [CAL]}
    # The last search's day again, as notices appear through a day.
    (_, claim), = p.plan_bulk(secs, None, TODAY, {ref: TODAY - timedelta(days=3)}, 30)
    assert claim["first"] == (TODAY - timedelta(days=3)).isoformat()
    # Searched today, or no share claimed: no search.
    assert p.plan_bulk(secs, None, TODAY, {ref: TODAY}, 30) == []
    assert p.plan_bulk(secs[2:], None, TODAY, {}, 30) == []


def _search(tmp_path, client, *, first=TODAY - timedelta(days=3)):
    p = six.Six()
    (_, claim), = p.plan_bulk([share(), share(LI_KEY)], None, TODAY, {six.NOTICES_REF: first}, 30)
    out = tmp_path / "notices"
    p.fetch_bulk(claim, out, Context(client=client))
    return p, out


def test_a_search_keeps_only_the_fields_read_of_the_claimed_shares_notices(tmp_path, caplog):
    caplog.set_level("DEBUG")
    client = FakeClient(routes())
    p, out = _search(tmp_path, client)
    assert client.asked[0] == ("GET", "https://www.six-group.com/sheldon/official_notices/v2/find.json",
                               {"firstDate": "20260712", "lastDate": "20260715", "showExDividend": "true",
                                "pageSize": 1000, "pageNumber": 0})
    # One detail per ex-dividend notice of a claimed share, one that names
    # two ISINs too; none of the share no list names, none of another
    # type, and none without an ISIN whose ex day no dividend answer
    # states.
    assert [u.rsplit("/", 1)[1] for u in client.urls()[1:]] == ["900101.json", "900103.json"]
    assert json.loads((out / six.NOTICES_DOC).read_text()) == [
        {"id": "900101", "isin": "CH0000000001",
         "fields": {"Ex Date": "10.03.2026", "Payment Date": "12.03.2026",
                    "Gross amount payable per security": "2.50", "Currency Code": "CHF"}},
        {"id": "900103", "isin": "LI0000000002", "fields": {"Ex Date": "2026-03-17", "Payment Date": "2026-03-19"}}]
    meta = read_meta(out)
    assert (meta["listed"], meta["answered"], meta["as_of"]) == (5, [KEY], TODAY.isoformat())
    written = b"".join(f.read_bytes() for f in out.iterdir()).decode()
    assert not [c for c in CONTACT if c in written or c in caplog.text]


def test_a_notice_that_states_its_four_fields_is_its_dividends_event(tmp_path):
    p, out = _search(tmp_path, FakeClient(routes()))
    # The Liechtenstein notice states no amount: it would replace its
    # share's dividend row with less, so it only joins it.
    (event,) = p.parse(out).dividend_events
    assert (event["ref"], event["event_id"], event["ex_date"], event["pay_date"], event["amount"],
            event["currency"], event["payload"]) == \
        (KEY, "dividend|2026-03-10||", date(2026, 3, 10), date(2026, 3, 12), "2.5", "CHF", {"notice": "900101"})


def test_a_detail_six_no_longer_serves_is_skipped_and_any_other_failure_fails_the_search(tmp_path, caplog):
    p, out = _search(tmp_path, FakeClient(routes(**{"/details/900103.json": 404})))
    assert [n["id"] for n in json.loads((out / six.NOTICES_DOC).read_text())] == ["900101"]
    assert "ex-dividend notice 900103: SIX no longer serves it" in caplog.text
    with pytest.raises(fetcher.FetchError):
        _search(tmp_path / "again", FakeClient(routes(**{"/details/900103.json": 500})))


def test_a_search_answer_in_no_known_layout_fails_the_search(tmp_path):
    with pytest.raises(ProviderError, match="no known layout"):
        _search(tmp_path, FakeClient(routes(**{"/find.json": {"errors": [{"code": "E1"}]}})))
    # No notice that day is no failure.
    p, out = _search(tmp_path / "quiet", FakeClient(routes(**{"/find.json": {"itemList": [], "itemCount": 0}})))
    assert json.loads((out / six.NOTICES_DOC).read_text()) == [] and p.parse(out).dividend_events == []


def test_a_notices_fields_are_read_wherever_its_text_writes_them():
    read = six.notice_fields({"notice": {"body": "Ex-Date: 20260310 Payment Date:\n2026-03-12\n"
                                                 "Gross amount payable per security: CHF 1'234.50\n"
                                                 "Currency Code: chf"}})
    # A currency code is upper case: "chf" is no code.
    assert read == {"Ex Date": "20260310", "Payment Date": "2026-03-12",
                    "Gross amount payable per security": "1'234.50"}
    assert six.notice_event({"id": 7, "fields": read}) == {"id": "7", "ex": date(2026, 3, 10),
                                                           "pay": date(2026, 3, 12), "amount": "1234.5",
                                                           "currency": None}
    assert six.notice_event({"id": 8, "fields": {"Payment Date": "12.03.2026"}}) == {}


def _execute(data, slug, reqs, client):
    """A complete run of reqs in data/slug, fetched with data as the data
    dir."""
    return make_run(data, slug, reqs, Context(client=client, data_dir=data))


def _plan(day, *, earlier=None, max_age=30, data_dir=None, calendar=None, keys=(KEY,), providers=None):
    """download.plan on `day` of a version 3 wanted list whose shares
    `keys`, held directly, want their calendar, gold's day for it
    `calendar`; asked of six alone or of `providers`."""
    doc = wanted_doc(securities=[{"key": key, "isin": key.removeprefix("isin:"), "currency": "CHF",
                                  "scope": "direct", "asset_class": "public_equity", "wants": [CAL],
                                  "calendar": calendar and calendar.isoformat()} for key in keys], version=3)
    return download.plan(wantedmod.parse(doc), providers or [REGISTRY["six"]],
                         Context(client=None, data_dir=data_dir), today=day, max_age=max_age, backfill=False,
                         earlier=earlier)


def _events(db):
    return {r[0]: r[1:] for r in rows(
        db, "SELECT ex_date, pay_date, amount, payload FROM dividend_events WHERE ref = ? AND provider = 'six'", KEY)}


def test_a_later_dividend_answer_keeps_the_pay_day_an_earlier_notice_stated(tmp_path, today):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    today[0] = date(2026, 3, 3)
    reqs, _, cover = _plan(today[0])
    assert [(r.kind, r.provider, r.ref, r.facts) for r in reqs] == [("security", "six", KEY, (CAL,)),
                                                                   ("bulk", "six", six.NOTICES_REF, (CAL,))]
    assert cover[CAL][KEY].requests == reqs
    run = _execute(data, "20260303T010000Z", reqs, FakeClient(routes()))
    assert [(e["status"], e["facts"]) for e in run_entries(run)] == [("ok", [CAL]), ("ok", [CAL])]
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    # The notice, fetched after the dividends, stands for its event.
    assert _events(db)["2026-03-10"] == ("2026-03-12", "2.5", '{"notice":"900101"}')
    # Within max_age and on the day of the search: nothing to ask.
    earlier = download.fetched_before(data)
    assert _plan(today[0], earlier=earlier)[0] == []
    # A month on, the dividends are due again; the search goes on from the
    # last one's day. It finds nothing new, and the answer joins the
    # notice an earlier run kept.
    today[0] = date(2026, 4, 15)
    reqs, _, _ = _plan(today[0], earlier=earlier)
    assert reqs[1].claim["first"] == "2026-03-03"
    _execute(data, "20260415T010000Z", reqs, FakeClient(routes(**{"/find.json": {"itemList": []}})))
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    events = _events(db)
    assert events["2026-03-10"] == ("2026-03-12", "2.5", '{"adjustedValue":2.5,"notice":"900101"}')
    # The notice that names no ISIN joined by its title's ex day; the
    # oldest dividend has no notice.
    assert events["2025-03-11"] == ("2025-03-13", "2.4", '{"adjustedValue":2.4,"notice":"900105"}')
    assert events["2009-04-15"][0] is None


def test_probe_asks_for_todays_notices():
    client = FakeClient({"/find.json": fixture("six_notices.json")})
    assert six.Six().probe(Context(client=client)) == "SIX notices: 6 for today, 5 ex-dividend"


def _due(day, earlier, max_age, calendar=None):
    reqs, _, cover = _plan(day, earlier=earlier, max_age=max_age, calendar=calendar)
    return [r.kind for r in reqs if r.kind == "security"], cover[CAL][KEY]


def test_a_share_is_asked_once_a_month_whatever_max_age_says_a_failed_request_too():
    assert six.Six.min_age_days == 30
    asked = date(2026, 7, 5)
    # The day's search is done: only the share's own request is in question.
    searched = {("six", six.NOTICES_REF): asked + timedelta(days=40)}
    served = download.Fetched.of(asked={("six", KEY): asked}, wants={("six", KEY, CAL): asked}, bulk=searched)
    failed = download.Fetched.of(asked={("six", KEY): asked}, bulk=searched)
    # Ten days on, with max_age_days at 7: neither is asked again.
    reqs, c = _due(asked + timedelta(days=10), served, 7)
    assert reqs == [] and c.had
    reqs, c = _due(asked + timedelta(days=10), failed, 7)
    assert reqs == [] and not c.had and c.reason == "six waits 30 days after a request"
    # A month on, both are.
    for earlier in (served, failed):
        assert _due(asked + timedelta(days=31), earlier, 7)[0] == ["security"]
    # A longer max_age_days holds as it says.
    assert _due(asked + timedelta(days=31), served, 60)[0] == []
    # Gold's calendar day counts for the month as well.
    gold = download.Fetched.of(bulk=searched)
    assert _due(asked + timedelta(days=10), gold, 7, calendar=asked)[0] == []
    assert _due(asked + timedelta(days=31), gold, 7, calendar=asked)[0] == ["security"]


class _Exchange(Provider):
    """A security provider that claims every share for its calendar."""
    name = "exchange"
    serves_securities = True
    answers = frozenset({CAL})

    def claim_security(self, sec, ctx):
        return {"key": sec.key, "facts": [CAL]}


def test_six_plans_every_share_whichever_provider_claimed_one_before():
    other = "isin:CH0000000003"
    reqs, _, cover = _plan(TODAY, keys=(KEY, other))
    assert [(r.kind, r.ref, r.facts) for r in reqs] == [("security", KEY, (CAL,)), ("security", other, (CAL,)),
                                                        ("bulk", six.NOTICES_REF, (CAL,))]
    assert all(cover[CAL][k].requests for k in (KEY, other))
    reqs, _, _ = _plan(TODAY, providers=[_Exchange(), REGISTRY["six"]])
    assert [(r.kind, r.provider, r.ref) for r in reqs] == [("security", "exchange", KEY), ("security", "six", KEY),
                                                           ("bulk", "six", six.NOTICES_REF)]


def test_a_failed_request_counts_as_asked_in_the_next_plan(tmp_path):
    data = tmp_path / "data"
    reqs, _, _ = _plan(TODAY)
    _execute(data, "20260715T010000Z", reqs[:1], FakeClient(routes(**{DIVIDENDS: 503})))
    earlier = download.fetched_before(data)
    assert earlier.asked == {("six", KEY): TODAY} and earlier.wants == {}
    assert _due(TODAY + timedelta(days=1), earlier, 1)[0] == []


def _paged(pages, count):
    """A search that answers `pages` (lists of notices) by its page number,
    from 0, each stating `count` notices in all."""
    def find(url, params):
        page = params["pageNumber"]
        return {"status": "OK", "totalCount": count, "itemList": pages[page] if page < len(pages) else []}
    return find


def test_the_search_asks_for_every_page_its_count_names(tmp_path):
    items = fixture("six_notices.json")["itemList"]
    client = FakeClient(routes(**{"/find.json": _paged([items[:2], items[2:]], 6)}))
    p, out = _search(tmp_path, client)
    searches = [params for _, url, params in client.asked if url.endswith("/find.json")]
    assert [(s["pageSize"], s["pageNumber"]) for s in searches] == [(1000, 0), (1000, 1)]
    assert read_meta(out)["listed"] == 5
    assert [n["id"] for n in json.loads((out / six.NOTICES_DOC).read_text())] == ["900101", "900103"]


def test_pages_that_stop_short_of_the_count_fail_the_search(tmp_path):
    items = fixture("six_notices.json")["itemList"]
    # A SIX that answers the first page whatever page is asked for.
    client = FakeClient(routes(**{"/find.json": {"status": "OK", "totalCount": 2500, "itemList": items}}))
    with pytest.raises(ProviderError, match="the search lists 2500 notices, and its pages hold 6"):
        _search(tmp_path, client)
    assert len(client.asked) == 2
    # An empty page before the count is reached.
    client = FakeClient(routes(**{"/find.json": _paged([items[:2]], 6)}))
    with pytest.raises(ProviderError, match="the search lists 6 notices, and its pages hold 2"):
        _search(tmp_path / "empty", client)
    assert len(client.asked) == 2


def test_a_search_the_cap_cuts_short_in_its_first_span_fails(tmp_path, monkeypatch):
    monkeypatch.setattr(six, "MAX_PAGES", 1)
    items = fixture("six_notices.json")["itemList"]
    client = FakeClient(routes(**{"/find.json": _paged([items[:2], items[2:]], 6)}))
    with pytest.raises(ProviderError, match="the search reached its cap of 1 pages a run"):
        _search(tmp_path, client)
    assert len(client.asked) == 1 and not (tmp_path / "notices").exists()


def test_a_notice_two_spans_list_is_asked_for_once(tmp_path):
    p = six.Six()
    claim = {"ref": six.NOTICES_REF, "first": "2026-06-01", "last": "2026-07-15", "isins": {"CH0000000001": [KEY]},
             "keys": [KEY], "facts": [CAL]}
    found = {"status": "OK", "totalCount": 1, "itemList": fixture("six_notices.json")["itemList"][:1]}
    client = FakeClient(routes(**{"/find.json": found}))
    p.fetch_bulk(claim, tmp_path / "out", Context(client=client))
    assert len([u for u in client.urls() if u.endswith("/find.json")]) == 2
    assert [u for u in client.urls() if "/details/" in u] == [six.DETAILS.format(id="900101")]
    assert [n["id"] for n in json.loads((tmp_path / "out" / six.NOTICES_DOC).read_text())] == ["900101"]


def test_details_that_state_no_ex_day_are_kept_and_state_no_event(tmp_path, caplog):
    client = FakeClient(routes(**{"/details/900101.json": {"noticeId": 900101, "text": "Payment Date 12.03.2026"}}))
    p, out = _search(tmp_path, client)
    assert "ex-dividend notice 900101: its details state no ex day" in caplog.text
    kept = {n["id"]: n for n in json.loads((out / six.NOTICES_DOC).read_text())}
    assert kept["900101"]["fields"] == {"Payment Date": "12.03.2026"}
    assert KEY not in read_meta(out)["answered"]
    assert not [e for e in p.parse(out).dividend_events if e["ref"] == KEY]


def test_a_shares_dividend_answer_is_complete_with_the_notices_it_carries(tmp_path):
    p = six.Six()
    out = tmp_path / "out"
    data = tmp_path / "data"
    # An earlier search kept a notice of an ex day the list does not hold
    # yet.
    folder = kept_run(data, "20260714T010000Z", "bulk", "six", six.NOTICES_REF)
    bronze.atomic_write_json(folder / six.NOTICES_DOC, [
        {"id": "900130", "isin": "CH0000000001", "fields": {"Ex Date": "09.09.2026", "Payment Date": "11.09.2026",
                                                            "Gross amount payable per security": "0.40",
                                                            "Currency Code": "CHF"}}])
    p.fetch_security(p.claim_security(share(), None), out, Context(client=FakeClient(routes()), data_dir=data))
    parsed = p.parse(out)
    assert parsed.events_complete
    events = by_ex(parsed.dividend_events)
    assert (events[date(2026, 9, 9)]["pay_date"], events[date(2026, 9, 9)]["amount"],
            events[date(2026, 9, 9)]["payload"]) == (date(2026, 9, 11), "0.4", {"notice": "900130"})
    assert len(events) == 5
    # The notices a search finds add to what silver holds; a share SIX
    # does not list states nothing complete.
    _, notices = _search(tmp_path / "s", FakeClient(routes()))
    assert not p.parse(notices).events_complete
    p.fetch_security(p.claim_security(share(), None), tmp_path / "gone", Context(client=FakeClient({}), data_dir=data))
    assert not p.parse(tmp_path / "gone").events_complete


@pytest.mark.parametrize("named,isins", [
    ("CH0000000001", {"CH0000000001"}), ("CH0000000098 / LI0000000002", {"CH0000000098", "LI0000000002"}),
    ("CH0000000098, LI0000000002", {"CH0000000098", "LI0000000002"}), ("NONE", set()), (None, set()), ("", set())])
def test_a_notice_names_one_isin_several_or_none(named, isins):
    assert six.notice_isins({"isin": named}) == isins


def test_a_notice_without_an_isin_is_asked_for_where_its_ex_day_is_a_claimed_shares(tmp_path, caplog):
    caplog.set_level("DEBUG")
    client = FakeClient(routes())
    ctx = Context(client=client)
    p = six.Six()
    # This run's dividend answer states 2025-03-11, the ex day the title
    # of 900105 writes.
    p.fetch_security(p.claim_security(share(), None), tmp_path / "share", ctx)
    (_, claim), = p.plan_bulk([share(), share(LI_KEY)], None, TODAY, {six.NOTICES_REF: TODAY - timedelta(days=3)},
                              30)
    p.fetch_bulk(claim, tmp_path / "notices", ctx)
    # 900106 names none and its ex day is no claimed share's: not asked.
    assert [u.rsplit("/", 1)[1] for u in client.urls() if "/details/" in u] == ["900101.json", "900103.json",
                                                                               "900105.json"]
    kept = {n["id"]: n for n in json.loads((tmp_path / "notices" / six.NOTICES_DOC).read_text())}
    # Its details name the share.
    assert kept["900105"] == {"id": "900105", "isin": "CH0000000001",
                              "fields": {"Ex Date": "11.03.2025", "Payment Date": "13.03.2025",
                                         "Gross amount payable per security": "2.40", "Currency Code": "CHF"}}
    events = by_ex(p.parse(tmp_path / "notices").dividend_events)
    assert (events[date(2025, 3, 11)]["ref"], events[date(2025, 3, 11)]["pay_date"]) == (KEY, date(2025, 3, 13))
    # No contact reaches a file or the log.
    written = b"".join(f.read_bytes() for f in (tmp_path / "notices").iterdir()).decode()
    assert not [c for c in CONTACT if c in written or c in caplog.text]


def test_an_earlier_runs_dividend_answer_gives_its_ex_days(tmp_path):
    data = tmp_path / "data"
    reqs, _, _ = _plan(TODAY)
    _execute(data, "20260714T010000Z", reqs[:1], FakeClient(routes()))
    days = six.dividend_days(Context(client=None, data_dir=data))
    assert days == {"CH0000000001": {date(2026, 3, 10), date(2025, 3, 11), date(2024, 3, 12), date(2009, 4, 15)}}


def test_each_request_is_kept_in_the_ask_record_a_failed_one_too(tmp_path, today):
    data = tmp_path / "data"
    data.mkdir()
    p = six.Six()
    ctx = Context(client=FakeClient(routes(**{DIVIDENDS: 503})), data_dir=data)
    with pytest.raises(fetcher.FetchError):
        p.fetch_security(p.claim_security(share(), None), tmp_path / "out", ctx)
    path = data / six.ASKED
    assert json.loads(path.read_text()) == {KEY: TODAY.isoformat()} and path.stat().st_mode & 0o777 == 0o600
    today[0] = TODAY + timedelta(days=1)
    p.fetch_security(p.claim_security(share(LI_KEY), None), tmp_path / "li", ctx)
    assert p.asked(ctx) == {KEY: TODAY, LI_KEY: TODAY + timedelta(days=1)}


def test_a_run_that_died_after_its_request_still_waits_the_month(tmp_path):
    data = tmp_path / "data"
    data.mkdir()
    # No manifest counts the request; the ask record does.
    bronze.atomic_write_json(data / six.ASKED, {KEY: "2026-07-05"})

    def plan(day):
        reqs, _, cover = _plan(day, earlier=download.Fetched.of(bulk={("six", six.NOTICES_REF): day}), max_age=7,
                               data_dir=data)
        return [r.kind for r in reqs], cover[CAL][KEY]
    reqs, c = plan(date(2026, 7, 15))
    assert reqs == [] and c.reason == "six waits 30 days after a request"
    assert plan(date(2026, 8, 5))[0] == ["security"]


@pytest.mark.parametrize("doc", ["{", "[]", '{"isin:CH0000000001": "soon"}', '{"isin:CH0000000001": 20260705}'])
def test_an_ask_record_that_does_not_read_is_refused(tmp_path, doc):
    (tmp_path / six.ASKED).write_text(doc)
    with pytest.raises(ProviderError, match=r"does not read \(.+\); SIX's monthly ask cannot be kept without it"):
        six.read_asked(tmp_path / six.ASKED)


def test_an_ask_record_that_does_not_read_asks_six_nothing(tmp_path, caplog):
    data = tmp_path / "data"
    data.mkdir()
    (data / six.ASKED).write_text("{")
    client = FakeClient(routes())
    p = six.Six()
    with pytest.raises(ProviderError, match="monthly ask cannot be kept"):
        p.fetch_security(p.claim_security(share(), None), tmp_path / "out", Context(client=client, data_dir=data))
    assert client.asked == []
    reqs, _, _ = _plan(TODAY, earlier=download.Fetched.of(bulk={("six", six.NOTICES_REF): TODAY}), data_dir=data)
    assert reqs == [] and "could not read the days of its requests" in caplog.text


def test_the_days_go_in_spans_of_about_a_month():
    got = six.spans(TODAY - timedelta(days=six.LOOKBACK_DAYS), TODAY)
    assert len(got) == 12 and got[0] == (date(2025, 7, 14), date(2025, 8, 13))
    assert got[-1][1] == TODAY and all(b == a + timedelta(days=1) for (_, a), (b, _) in zip(got, got[1:], strict=False))
    assert six.spans(TODAY, TODAY) == [(TODAY, TODAY)]


def _spanned(fail_from=None):
    """A search that answers each span with its own notices: 900101 for
    the span that begins on 2026-06-01, none for another; from the span
    that begins on `fail_from` on, 500."""
    def find(url, params):
        first = date(int(params["firstDate"][:4]), int(params["firstDate"][4:6]), int(params["firstDate"][6:]))
        if fail_from is not None and first >= fail_from:
            return 500
        items = fixture("six_notices.json")["itemList"][:1] if first == date(2026, 6, 1) else []
        return {"status": "OK", "totalCount": len(items), "itemList": items}
    return find


def test_a_span_that_does_not_finish_ends_the_search_and_the_next_run_goes_on_from_the_last_one(tmp_path, caplog):
    p = six.Six()
    claim = {"ref": six.NOTICES_REF, "first": "2026-06-01", "last": "2026-07-15", "isins": {"CH0000000001": [KEY]},
             "keys": [KEY], "facts": [CAL]}
    out = kept_run(tmp_path / "data", "20260715T010000Z", "bulk", "six", six.NOTICES_REF)
    p.fetch_bulk(claim, out, Context(client=FakeClient(routes(**{"/find.json": _spanned(date(2026, 7, 2))}))))
    meta = read_meta(out)
    assert (meta["through"], meta["listed"]) == ("2026-07-01", 1)
    assert [n["id"] for n in json.loads((out / six.NOTICES_DOC).read_text())] == ["900101"]
    assert "the search of 2026-07-02 to 2026-07-15 did not finish" in caplog.text
    assert six.searched_through(tmp_path / "data") == date(2026, 7, 1)
    # The next run resumes there, though the run that searched was later.
    (_, again), = p.plan_bulk([share()], Context(client=None, data_dir=tmp_path / "data"), TODAY,
                              {six.NOTICES_REF: TODAY}, 30)
    assert again["first"] == "2026-07-01"
    # Where no span finishes, the search fails.
    with pytest.raises(fetcher.FetchError):
        p.fetch_bulk(claim, tmp_path / "none", Context(client=FakeClient(routes(**{"/find.json": _spanned(
            date(2026, 6, 1))}))))


def test_a_run_asks_for_at_most_its_cap_of_pages(tmp_path, monkeypatch):
    monkeypatch.setattr(six, "MAX_PAGES", 2)
    p = six.Six()
    claim = {"ref": six.NOTICES_REF, "first": "2026-05-01", "last": "2026-07-15", "isins": {"CH0000000001": [KEY]},
             "keys": [KEY], "facts": [CAL]}
    client = FakeClient(routes(**{"/find.json": _spanned()}))
    p.fetch_bulk(claim, tmp_path / "out", Context(client=client))
    # Three spans, two pages: the third waits for the next run.
    assert len([u for u in client.urls() if u.endswith("/find.json")]) == 2
    assert read_meta(tmp_path / "out")["through"] == "2026-07-01"


# An id a details URL cannot name is none: it never reaches the URL's
# path.
@pytest.mark.parametrize("nid", [None, "", "../900101", "9001 01"])
def test_a_search_answer_notice_without_an_id_counts_but_is_never_read(tmp_path, nid):
    items = fixture("six_notices.json")["itemList"]
    anonymous = {"noticeId": nid, "noticeType": "EX", "contact": "Jane Example", "title": "Ex 10.03.2026",
                 "isin": "CH0000000001"}
    client = FakeClient(routes(**{"/find.json": _paged([[anonymous, *items[:1]]], 2)}))
    p, out = _search(tmp_path, client)
    assert [n["id"] for n in json.loads((out / six.NOTICES_DOC).read_text())] == ["900101"]
    assert len([u for u in client.urls() if u.endswith("/find.json")]) == 1
    assert [u for u in client.urls() if "/details/" in u] == [six.DETAILS.format(id="900101")]


def test_a_later_notice_of_one_share_and_ex_day_stands_for_the_event():
    def notice(nid, pay):
        return {"id": nid, "isin": "CH0000000001", "fields": {"Ex Date": "10.03.2026", "Payment Date": pay,
                                                             "Gross amount payable per security": "2.50",
                                                             "Currency Code": "CHF"}}
    (event,) = six.notice_events([notice("900120", "13.03.2026"), notice("900101", "12.03.2026")],
                                 {"CH0000000001": [KEY]}, TODAY, provider="six")
    assert (event["pay_date"], event["payload"]) == (date(2026, 3, 13), {"notice": "900120"})
