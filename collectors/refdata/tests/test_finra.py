"""finra: FINRA's OTC Daily List for the receipts held, on invented
answers in the API's layout, with an invented credential against fake
servers. Every ticker and rate is invented."""

import base64
import json
import logging
from datetime import date
from pathlib import Path

import pytest

import config
import download
import fetcher
import load
from collectorkit import bronze, debugcap
from providers import REGISTRY
from providers import finra as finra_mod
from providers.base import Context, Provider, ProviderError, read_meta
from providers.finra import (BUDGET, CAL, DATA_URL, HISTORY_START, REF, REQUEST_MAX, TALLY, TOKEN_KEY, TOKEN_URL,
                             Finra, covered, events)
from tests.helpers import TODAY, FakeClient, FakeHttps, kept_run, make_run, rows, wanted_doc
from wanted import Security

FIXTURES = Path(__file__).parent / "fixtures"
PAGE_BODY = (FIXTURES / "finra_otc_daily_list.json").read_bytes()
CID, SECRET, TOKEN = "example-client-id", "example-client-secret", "example-bearer-token"
BASIC = "Basic " + base64.b64encode(f"{CID}:{SECRET}".encode()).decode()
MONTH = TODAY.strftime("%Y-%m")


@pytest.fixture(autouse=True)
def today(monkeypatch):
    monkeypatch.setattr(finra_mod, "today_utc", lambda: TODAY)


def creds(cid=CID, secret=SECRET):
    return config.Config(providers=("finra",), finra_client_id=cid, finra_client_secret=secret)


def ctx(client, data_dir=None, cfg=None):
    return Context(client=client, data_dir=data_dir, config=cfg or creds(), providers=frozenset({"finra"}))


def finra_client(pages=None):
    """A FakeClient as FINRA's two hosts: a token for the credential, and
    the daily list, every page `pages` (one list or bytes each) in turn,
    then an empty answer."""
    pages = list(pages if pages is not None else [PAGE_BODY])

    def daily(url, payload):
        return pages.pop(0) if pages else b""
    return FakeClient({"oauth2/access_token": {"access_token": TOKEN, "token_type": "Bearer"},
                       "otcDailyList": daily})


def receipt(key, symbol=None, calendar=None, is_receipt=True, wants=(CAL,)):
    return Security(key=key, symbol=symbol, asset_class="public_equity", receipt=is_receipt, wants=tuple(wants),
                    calendar=calendar)


def plan(securities, data_dir=None, today=TODAY, cfg=None):
    return Finra().plan_bulk(securities, ctx(FakeClient(), data_dir, cfg), today, {}, 30)


def data_posts(client):
    return [(data, headers) for (m, url, data), headers in zip(client.asked, client.headers, strict=True)
            if url == DATA_URL]


def test_registered_and_off_until_refdata_cfg_names_it():
    assert isinstance(REGISTRY["finra"], Finra) and "finra" not in config.DEFAULT_PROVIDERS


def test_plan_asks_the_receipts_due_by_ticker_from_their_history_or_their_newest_day():
    secs = [receipt("symbol:QEXPY"),
            receipt("isin:XX0000000801", "QRCAY", calendar=date(2026, 5, 1)),
            receipt("isin:XX0000000802", "QRCBF", calendar=date(2026, 6, 1)),
            # Gold's calendar within max_age: not due.
            receipt("symbol:QRCCY", calendar=date(2026, 7, 10)),
            # No receipt, no ticker, no calendar wanted: never asked.
            receipt("symbol:QSHR", is_receipt=False),
            receipt("isin:XX0000000803"),
            receipt("symbol:QRCDY", wants=())]
    ((ref, claim),) = plan(secs)
    assert ref == REF == claim["ref"]
    assert claim == {"ref": REF, "facts": [CAL], "through": "2026-07-15",
                     "keys": {"QEXPY": ["symbol:QEXPY"], "QRCAY": ["isin:XX0000000801"],
                              "QRCBF": ["isin:XX0000000802"]},
                     "groups": [{"from": HISTORY_START.isoformat(), "symbols": ["QEXPY"]},
                                {"from": "2026-05-01", "symbols": ["QRCAY", "QRCBF"]}]}
    assert download.bulk_keys(claim) == {"symbol:QEXPY", "isin:XX0000000801", "isin:XX0000000802"}
    # Nothing due, nothing planned.
    assert plan(secs[3:]) == [] and plan([]) == []


def test_two_keys_of_one_ticker_are_asked_once_from_the_older_day():
    ((_, claim),) = plan([receipt("symbol:QRCAY", calendar=date(2026, 5, 1)),
                          receipt("isin:XX0000000801", "QRCAY", calendar=date(2026, 4, 1))])
    assert claim["keys"] == {"QRCAY": ["symbol:QRCAY", "isin:XX0000000801"]}
    assert claim["groups"] == [{"from": "2026-04-01", "symbols": ["QRCAY"]}]


def _bronze_run(data, slug, answered, through, status="complete"):
    """A run in data whose finra answer covers the keys `answered` through
    the day `through`."""
    folder = kept_run(data, slug, "bulk", "finra", REF, status=status)
    bronze.atomic_write_json(folder / "meta.json", {"through": through, "answered": answered})


def test_plan_starts_where_an_earlier_answer_left_off(tmp_path):
    data = tmp_path / "data"
    _bronze_run(data, "20260601T010000Z", ["symbol:QEXPY", "symbol:QRCAY"], "2026-06-01")
    _bronze_run(data, "20260710T010000Z", ["symbol:QEXPY"], "2026-07-10")
    # A run that did not complete covers nothing, nor does an answer that
    # states no day it covered or whose meta.json does not read.
    _bronze_run(data, "20260714T010000Z", ["symbol:QRCAY"], "2026-07-14", status="in-progress")
    _bronze_run(data, "20260711T010000Z", ["symbol:QRCBF"], "x")
    (kept_run(data, "20260712T010000Z", "bulk", "finra", REF) / "meta.json").write_text("{")
    assert covered(data) == {"symbol:QEXPY": date(2026, 7, 10), "symbol:QRCAY": date(2026, 6, 1)}
    secs = [receipt("symbol:QEXPY"), receipt("symbol:QRCAY"), receipt("symbol:QRCBF")]
    ((_, claim),) = plan(secs, data)
    # QEXPY's answer is within max_age; QRCAY asks from the day it was
    # covered through, the newer of that and gold's day.
    assert claim["groups"] == [{"from": HISTORY_START.isoformat(), "symbols": ["QRCBF"]},
                               {"from": "2026-06-01", "symbols": ["QRCAY"]}]
    ((_, claim),) = plan([receipt("symbol:QRCAY", calendar=date(2026, 6, 10))], data)
    assert claim["groups"] == [{"from": "2026-06-10", "symbols": ["QRCAY"]}]
    ((_, claim),) = plan(secs[:1], data, today=date(2026, 8, 15))
    assert claim["groups"] == [{"from": "2026-07-10", "symbols": ["QEXPY"]}]


@pytest.mark.parametrize("cid,secret,refusal", [
    ("", SECRET, "FINRA_CLIENT_ID is not set"),
    (CID, "", "FINRA_CLIENT_SECRET is not set"),
    ("", "", "FINRA_CLIENT_ID and FINRA_CLIENT_SECRET are not set"),
    (CID, SECRET + "\r", "FINRA_CLIENT_SECRET holds a control character"),
    (CID + "\n", SECRET, "FINRA_CLIENT_ID holds a control character"),
    ("\x1bexample", "example\x7f", "FINRA_CLIENT_ID and FINRA_CLIENT_SECRET hold a control character")])
def test_a_credential_unset_or_with_a_control_character_is_refused_before_any_request(tmp_path, cid, secret,
                                                                                      refusal):
    client = finra_client()
    c = ctx(client, tmp_path, creds(cid, secret))
    p = Finra()
    for call in (lambda: p.plan_bulk([receipt("symbol:QEXPY")], c, TODAY, {}, 30),
                 lambda: p.fetch_bulk(plan([receipt("symbol:QEXPY")])[0][1], tmp_path / "out", c),
                 lambda: p.probe(c)):
        with pytest.raises(ProviderError, match=refusal) as e:
            call()
        # Every value here holds "example"; the refusal names none.
        assert "example" not in str(e.value)
    assert client.asked == [] and not (tmp_path / TALLY).exists()


def test_fetch_takes_a_token_and_asks_only_the_fields_tickers_and_days(tmp_path):
    client = finra_client()
    ((_, claim),) = plan([receipt("symbol:QEXPY"), receipt("isin:XX0000000801", "QRCAY", calendar=date(2026, 5, 1))])
    out = tmp_path / "out"
    Finra().fetch_bulk(claim, out, ctx(client, tmp_path))
    # One token, then a page per group: each holds less than a full page.
    assert client.urls() == [TOKEN_URL, DATA_URL, DATA_URL]
    assert client.asked[0] == ("POST", TOKEN_URL, {}) and client.headers[0] == {"Authorization": BASIC}
    posts = data_posts(client)
    assert [h for _, h in posts] == [{"Authorization": f"Bearer {TOKEN}", "Accept": "application/json"}] * 2
    assert posts[0][0] == {"limit": finra_mod.PAGE, "offset": 0, "fields": list(finra_mod.FIELDS),
                           "domainFilters": [{"fieldName": "newSymbolCode", "values": ["QEXPY"]}],
                           "dateRangeFilters": [{"fieldName": "calendarDay", "startDate": "2015-01-01",
                                                 "endDate": "2026-07-15"}]}
    assert posts[1][0]["dateRangeFilters"][0]["startDate"] == "2026-05-01"
    meta = read_meta(out)
    assert meta["files"] == ["page-001.json", "page-002.json"] and (out / "page-001.json").read_bytes() == PAGE_BODY
    assert meta["answered"] == ["isin:XX0000000801", "symbol:QEXPY"] and meta["as_of"] == "2026-07-15"


def test_a_full_page_asks_for_the_next_and_tickers_go_in_chunks(tmp_path, monkeypatch):
    monkeypatch.setattr(finra_mod, "PAGE", 2)
    monkeypatch.setattr(finra_mod, "CHUNK", 2)
    rows = json.loads(PAGE_BODY)
    client = finra_client([rows[:2], rows[2:4], rows[4:5]])
    ((_, claim),) = plan([receipt("symbol:QEXPY"), receipt("symbol:QRCAY"), receipt("symbol:QRCBF")])
    Finra().fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    posts = [d for d, _ in data_posts(client)]
    assert [(d["domainFilters"][0]["values"], d["offset"]) for d in posts] == [
        (["QEXPY", "QRCAY"], 0), (["QEXPY", "QRCAY"], 2), (["QEXPY", "QRCAY"], 4), (["QRCBF"], 0)]
    assert len(read_meta(tmp_path / "out")["files"]) == 4


def test_a_token_serves_30_minutes_then_a_new_one_is_taken(tmp_path, monkeypatch):
    monkeypatch.setattr(finra_mod, "PAGE", 1)
    now = [0.0]
    rows = json.loads(PAGE_BODY)

    def daily(url, payload):
        now[0] += 10 * 60
        return [rows[0]] if payload["offset"] < 4 else []
    client = FakeClient({"oauth2/access_token": {"access_token": TOKEN}, "otcDailyList": daily})
    p = Finra()
    p.clock = lambda: now[0]
    ((_, claim),) = plan([receipt("symbol:QEXPY")])
    p.fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    # Pages at 0, 10, 20 minutes on one token; at 30 a new one.
    assert [u == TOKEN_URL for u in client.urls()] == [True, False, False, False, True, False, False]


def test_a_token_answer_without_a_token_asks_no_data(tmp_path):
    client = FakeClient({"oauth2/access_token": {"error": "invalid_client"}})
    ((_, claim),) = plan([receipt("symbol:QEXPY")])
    with pytest.raises(ProviderError, match="without an access_token"):
        Finra().fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    client = FakeClient({"oauth2/access_token": 401})
    with pytest.raises(fetcher.FetchError):
        Finra().fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    assert client.urls() == [TOKEN_URL]


def test_the_month_tally_counts_every_answer_and_stops_before_the_budget(tmp_path):
    client = finra_client()
    ((_, claim),) = plan([receipt("symbol:QEXPY")])
    (tmp_path / TALLY).write_text(json.dumps({"2026-06": BUDGET}))
    Finra().fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    assert json.loads((tmp_path / TALLY).read_text()) == {"2026-06": BUDGET, MONTH: len(PAGE_BODY)}
    assert (tmp_path / TALLY).stat().st_mode & 0o777 == 0o600
    # One request more could pass the budget: none is sent, not even for
    # a token.
    bronze.atomic_write_json(tmp_path / TALLY, {MONTH: BUDGET - REQUEST_MAX + 1})
    client = finra_client()
    with pytest.raises(ProviderError, match=r"stopped: .* past this month's budget of 8.00 GB"):
        Finra().fetch_bulk(claim, tmp_path / "out2", ctx(client, tmp_path))
    assert client.asked == []
    with pytest.raises(ProviderError, match="data dir"):
        Finra().fetch_bulk(claim, tmp_path / "out3", ctx(client))
    assert client.asked == []


@pytest.mark.parametrize("tally", ["{", "[]", '{"2026-07": "1"}', '{"2026-07": true}'])
def test_a_tally_that_does_not_read_is_not_guessed_at(tmp_path, tally):
    (tmp_path / TALLY).write_text(tally)
    ((_, claim),) = plan([receipt("symbol:QEXPY")])
    client = finra_client()
    with pytest.raises(ProviderError, match=r"does not read \(.+\); FINRA's budget cannot be kept without it"):
        Finra().fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    assert client.asked == []


def test_a_budget_stop_keeps_the_pages_had_and_answers_no_key(tmp_path, monkeypatch):
    rows = json.loads(PAGE_BODY)
    first = json.dumps(rows[:2]).encode()
    # The first page passes the budget; one more could not.
    monkeypatch.setattr(finra_mod, "PAGE", 2)
    monkeypatch.setattr(finra_mod, "REQUEST_MAX", 1000)
    monkeypatch.setattr(finra_mod, "BUDGET", len(first) + 999)
    client = finra_client([first, json.dumps(rows[2:4]).encode()])
    ((_, claim),) = plan([receipt("symbol:QEXPY")])
    out = tmp_path / "out"
    with pytest.raises(ProviderError, match="stopped: "):
        Finra().fetch_bulk(claim, out, ctx(client, tmp_path))
    assert (out / "page-001.json").read_bytes() == first and not (out / "meta.json").exists()
    assert client.urls() == [TOKEN_URL, DATA_URL]
    assert json.loads((tmp_path / TALLY).read_text()) == {MONTH: len(first)}


def test_an_answer_that_is_no_list_fails_and_its_bytes_count(tmp_path):
    client = FakeClient({"oauth2/access_token": {"access_token": TOKEN}, "otcDailyList": {"error": "x"}})
    ((_, claim),) = plan([receipt("symbol:QEXPY")])
    with pytest.raises(ProviderError, match="FINRA's answer is no list of rows"):
        Finra().fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    assert json.loads((tmp_path / TALLY).read_text()) == {MONTH: len(json.dumps({"error": "x"}))}


def test_an_empty_answer_states_no_event_and_answers_every_key(tmp_path):
    client = finra_client([b""])
    ((_, claim),) = plan([receipt("symbol:QRCEY")])
    Finra().fetch_bulk(claim, tmp_path / "out", ctx(client, tmp_path))
    assert read_meta(tmp_path / "out")["answered"] == ["symbol:QRCEY"]
    # The days asked for only: what silver holds stands beside it.
    parsed = Finra().parse(tmp_path / "out")
    assert parsed.dividend_events == [] and not parsed.events_complete
    assert not (tmp_path / TALLY).exists()


def test_parse_keeps_each_events_latest_entry():
    rows = json.loads(PAGE_BODY)
    keys = {"QEXPY": ["symbol:QEXPY", "isin:XX0000000804"], "QRCAY": ["isin:XX0000000801"]}
    got = {(r["ref"], r["event_id"]): r for r in events(rows, keys, TODAY, provider="finra")}
    # QOTHY was not asked; QEXPY's entry without a distribution day is
    # none.
    assert sorted(got) == [("isin:XX0000000801", "900002"), ("isin:XX0000000801", "900003"),
                           ("isin:XX0000000804", "900001"), ("symbol:QEXPY", "900001")]
    final = got[("symbol:QEXPY", "900001")]
    assert final == {**got[("isin:XX0000000804", "900001")], "ref": "symbol:QEXPY"}
    # The estimate is revised to the final rate and the pay day.
    assert {k: final[k] for k in ("as_of", "kind", "ex_date", "record_date", "pay_date", "declared_date", "amount",
                                  "currency", "status")} == {
        "as_of": date(2026, 4, 27), "kind": "dividend", "ex_date": date(2026, 4, 7),
        "record_date": date(2026, 4, 7), "pay_date": date(2026, 5, 8), "declared_date": None,
        "amount": "1.5", "currency": "USD", "status": "final"}
    assert final["payload"] == {"symbol": "QEXPY", "dividendTypeCode": "CD", "commentText": "FNL",
                                "dailyListDatetime": "2026-04-27 08:00:00", "ADRNetRate": "0.925",
                                "ADRFeeAmount": "0.05"}
    with_stock = got[("isin:XX0000000801", "900002")]
    assert (with_stock["kind"], with_stock["declared_date"], with_stock["amount"],
            with_stock["payload"]["ADRTaxReliefAmount"]) == ("dividend", date(2025, 10, 28), "0.4125", "0.06")
    # An unknown code is `other`; no rate, no amount and no currency.
    old = got[("isin:XX0000000801", "900003")]
    assert (old["kind"], old["amount"], old["currency"], old["as_of"]) == ("other", None, None, date(2016, 3, 1))


def test_parse_reads_an_estimate_and_names_an_event_without_an_id():
    rows = [{"newSymbolCode": "qrcay", "dailyListDatetime": "2026-07-01 08:00:00", "dividendTypeCode": "CD",
             "commentText": "APX", "exDate": "2026-07-08", "recordDate": "2026-07-08", "ADRGrossRate": "0.5"},
            # A row that is no entry is none; an entry that states no day
            # of its list is dated by the answer's.
            "x", None, {"newSymbolCode": "QRCBF", "exDate": "2026-07-09", "dividendTypeCode": "CD"}]
    e, undated = events(rows, {"QRCAY": ["symbol:QRCAY"], "QRCBF": ["symbol:QRCBF"]}, TODAY, provider="finra")
    assert (e["event_id"], e["status"], e["pay_date"], e["as_of"]) == (
        "dividend|2026-07-08|2026-07-08|", "estimated", None, date(2026, 7, 1))
    assert (undated["ref"], undated["as_of"], undated["status"]) == ("symbol:QRCBF", TODAY, None)


def test_probe_takes_a_token_asks_no_data_and_reports_the_month(tmp_path):
    bronze.atomic_write_json(tmp_path / TALLY, {MONTH: 1_234_567, "2026-06": 9 * 10**9})
    client = finra_client()
    assert Finra().probe(ctx(client, tmp_path)) == \
        "a token taken; 2026-07: FINRA's answers brought 1.2 MB of the 8.00 GB budget"
    assert client.urls() == [TOKEN_URL]
    assert json.loads((tmp_path / TALLY).read_text()) == {MONTH: 1_234_567, "2026-06": 9 * 10**9}
    assert Finra().probe(ctx(finra_client())) == "a token taken; no data dir, so no tally"


def test_the_credential_and_token_are_written_nowhere(tmp_path, caplog):
    """Through the real client, its trace on: no file of the run, no log
    line, no trace line holds the secret, the Basic header or the token."""
    caplog.set_level(logging.DEBUG)
    rows = json.dumps(json.loads(PAGE_BODY)).encode()
    https = FakeHttps({TOKEN_URL: (200, json.dumps({"access_token": TOKEN}).encode(), {}),
                       DATA_URL: (200, rows, {"Content-Type": "application/json"})})
    run = tmp_path / "data" / "20260715T010000Z"
    client = fetcher.Client(https=https, sleep=lambda s: None,
                            trace=debugcap.HttpTrace(run, log=logging.getLogger("t"), enabled=True))
    c = ctx(client, tmp_path / "data")
    p = Finra()
    ((_, claim),) = p.plan_bulk([receipt("symbol:QEXPY")], c, TODAY, {}, 30)
    p.fetch_bulk(claim, run / "bulk" / "finra" / REF, c)
    p.probe(c)
    sent = {r.full_url: r.get_header("Authorization") for r in https.seen}
    assert sent == {TOKEN_URL: BASIC, DATA_URL: f"Bearer {TOKEN}"}
    assert client.received("api.finra.org") == len(rows)
    written = [f for f in tmp_path.rglob("*") if f.is_file()]
    assert any(f.name == debugcap.HttpTrace.FILENAME for f in written)
    for secret in (SECRET, BASIC.split()[1], TOKEN):
        assert not [f for f in written if secret.encode() in f.read_bytes()]
        assert secret not in caplog.text
    assert SECRET not in repr(c.config) and CID not in repr(c.config)
    # The run's context holds the token, and no repr of it shows it.
    assert c.cache[finra_mod.TOKEN_KEY].value == TOKEN
    for shown in (repr(c), repr(c.cache), str(c.cache)):
        assert TOKEN not in shown and SECRET not in shown


def test_a_failed_pages_bytes_count_in_the_tally(tmp_path):
    """Through the real client: FINRA's error answer brings bytes, and
    they count against the month as a page's do."""
    body = b'{"error": "' + b"x" * 1000 + b'"}'
    https = FakeHttps({TOKEN_URL: (200, json.dumps({"access_token": TOKEN}).encode(), {}),
                       DATA_URL: (500, body, {"Content-Type": "application/json"})})
    c = ctx(fetcher.Client(https=https, sleep=lambda s: None), tmp_path)
    ((_, claim),) = Finra().plan_bulk([receipt("symbol:QEXPY")], c, TODAY, {}, 30)
    with pytest.raises(fetcher.FetchError):
        Finra().fetch_bulk(claim, tmp_path / "out", c)
    assert json.loads((tmp_path / TALLY).read_text()) == {MONTH: len(body)}


@pytest.mark.parametrize("token", ["example-token\r\n", "example token", "example-\x00token", 12345])
def test_a_token_no_header_can_carry_is_refused_unquoted(tmp_path, token):
    client = FakeClient({"oauth2/access_token": {"access_token": token}, "otcDailyList": PAGE_BODY})
    c = ctx(client, tmp_path)
    ((ref, claim),) = Finra().plan_bulk([receipt("symbol:QEXPY")], c, TODAY, {}, 30)
    (entry,) = download.execute([download.Request("bulk", "finra", ref, None, claim, facts=(CAL,))],
                                tmp_path / "run", c)
    assert entry["status"] == "error" and "not one word of printable ASCII" in entry["error"]
    assert str(token).strip() not in entry["error"] and client.urls() == [TOKEN_URL]
    assert TOKEN_KEY not in c.cache


def test_a_run_plans_fetches_and_loads_the_receipts_calendar(tmp_path, monkeypatch):
    """Through download and load: the receipts' events reach silver, and a
    second plan within max_age asks nothing."""
    data = tmp_path / "data"
    want = download.wantedmod.parse(wanted_doc(securities=[
        {"key": "symbol:QEXPY", "scope": "direct", "asset_class": "public_equity", "receipt": True,
         "wants": [CAL], "calendar": None},
        {"key": "isin:XX0000000801", "symbol": "QRCAY", "scope": "direct", "asset_class": "public_equity",
         "receipt": True, "wants": [CAL], "calendar": None},
        {"key": "symbol:QSHR", "scope": "direct", "asset_class": "public_equity", "wants": [CAL]}], version=3))
    p = REGISTRY["finra"]
    c = ctx(finra_client(), data)
    reqs, _, cover = download.plan(want, [p], c, today=TODAY, max_age=30, backfill=False)
    assert [(r.kind, r.provider, r.ref, r.facts) for r in reqs] == [("bulk", "finra", REF, (CAL,))]
    assert download.tally(cover)[0][1:4] == (3, 2, 0)
    make_run(data, "20260715T010000Z", reqs, c)
    db = tmp_path / "refdata.db"
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    assert rows(db, "SELECT ref, event_id, status, pay_date, amount FROM dividend_events ORDER BY 1, 2") == [
        ("isin:XX0000000801", "900002", "final", "2025-12-01", "0.4125"),
        ("isin:XX0000000801", "900003", "final", "2016-04-01", None),
        ("symbol:QEXPY", "900001", "final", "2026-05-08", "1.5")]
    earlier = download.fetched_before(data)
    assert set(earlier.wants) == {("finra", "symbol:QEXPY", CAL), ("finra", "isin:XX0000000801", CAL)}
    reqs, _, cover = download.plan(want, [p], ctx(FakeClient(), data), today=TODAY, max_age=30, backfill=False,
                                   earlier=earlier)
    assert reqs == [] and download.tally(cover)[0][1:4] == (3, 0, 2)


class _Seen(Provider):
    """A probe that records the credential the run gives it."""
    name = "seen"

    def probe(self, ctx):
        self.got = (ctx.config.finra_client_id, ctx.config.finra_client_secret)
        return "seen"


@pytest.mark.parametrize("argv,want", [([], ("env-id", "env-secret")),
                                       (["--client-id", "flag-id"], ("flag-id", "env-secret"))])
def test_download_hands_the_credential_to_the_providers(monkeypatch, argv, want):
    seen = _Seen()
    monkeypatch.setitem(download.REGISTRY, "seen", seen)
    monkeypatch.setattr(download.config, "load", lambda known: config.Config(providers=("seen",)))
    monkeypatch.setenv("FINRA_CLIENT_ID", "env-id")
    monkeypatch.setenv("FINRA_CLIENT_SECRET", "env-secret")
    assert download.main(["--check", *argv]) == 0
    assert seen.got == want


def test_the_daily_lists_codes_comments_and_unknown_day_read_as_finra_writes_them():
    # A cash dividend is CD, or CSD beside a stock dividend; a split or a
    # stock dividend is no cash event. The comment's word gives the status
    # in any of its forms, and 9999-09-09 is a day FINRA does not know.
    assert [finra_mod.kind_of(c) for c in ("CD", "csd", "SD", "FS", "RS", "SO", None)] == \
        ["dividend", "dividend", "other", "other", "other", "other", "other"]
    assert [finra_mod.status_of(c) for c in ("FNL", "(Final) Will not trade ex", "Fnl", "FINAL - x", "(FNL)",
                                         "APX", "(Approximate)", "Approx.", None, "Will not trade ex")] == \
        ["final"] * 5 + ["estimated"] * 3 + [None, None]
    assert finra_mod.day("9999-09-09") is None and finra_mod.day("2026-05-28") == date(2026, 5, 28)
