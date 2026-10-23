"""The HTTP client: https only, a pace per host, a User-Agent that names
the tool, and redirects that keep the client's rules."""

import email.message
import http.client
import io
import json
import logging
import urllib.request

import pytest

import fetcher
from collectorkit import debugcap
from tests.helpers import FakeHttps

OK = (200, b'{"ok": true}', {"Content-Type": "application/json"})


def make_client(routes=None, default=OK, contact="ops@example.org", trace=None):
    now = [0.0]
    sleeps = []

    def sleep(s):
        sleeps.append(round(s, 3))
        now[0] += s

    https = FakeHttps(routes, default)
    c = fetcher.Client(contact, sleep=sleep, clock=lambda: now[0], https=https, trace=trace)
    return c, sleeps, https.seen


def test_one_request_a_second_per_host_and_eight_for_the_sec():
    c, sleeps, seen = make_client()
    c.get("https://example.org/a")
    c.get("https://example.org/b")
    c.get("https://www.sec.gov/x")
    c.get("https://www.sec.gov/y")
    assert sleeps == [1.0, 0.125]
    # The contact goes to the SEC alone; every other host gets the name.
    assert seen[0].get_header("User-agent") == "wealthdb-refdata/1"
    assert seen[2].get_header("User-agent") == "wealthdb-refdata/1 ops@example.org"


def test_the_sec_hosts_share_one_pace():
    c, sleeps, _ = make_client()
    c.get("https://www.sec.gov/x")
    c.get("https://data.sec.gov/y")
    c.get("https://www.sec.gov/z")
    assert sleeps == [0.125, 0.125]


def test_openfigi_and_gleif_keep_under_their_stated_limits_and_jpx_gets_five_seconds():
    c, sleeps, _ = make_client()
    for _ in range(2):
        c.post_json("https://api.openfigi.com/v3/mapping", [])
    for _ in range(2):
        c.get("https://api.gleif.org/api/v1/lei-records")
    c.get("https://www.jpx.co.jp/english/listing/others/ex-rights/index.html")
    c.get("https://www.jpx.co.jp/english/listing/others/ex-rights/20261023EN.xls")
    assert sleeps == [2.5, 1.2, 5.0]


def test_a_source_that_refuses_is_not_asked_again():
    c, _, seen = make_client(default=(403, b"", {}))
    for url in ("https://www.sec.gov/a", "https://data.sec.gov/b"):
        with pytest.raises(fetcher.FetchError):
            c.get(url)
    assert [r.full_url for r in seen] == ["https://www.sec.gov/a"]


def test_a_plain_403_elsewhere_is_one_page_only():
    c, _, seen = make_client(default=(403, b"", {}))
    for url in ("https://example.org/a", "https://example.org/b"):
        with pytest.raises(fetcher.FetchError):
            c.get(url)
    assert len(seen) == 2


def test_a_429_stops_any_source():
    c, _, seen = make_client(default=(429, b"", {}))
    for url in ("https://example.org/a", "https://example.org/b"):
        with pytest.raises(fetcher.FetchError):
            c.get(url)
    assert len(seen) == 1


def test_http_error_becomes_a_fetch_error():
    c, _, _ = make_client(default=(404, b"", {}))
    with pytest.raises(fetcher.FetchError, match="404") as e:
        c.get("https://example.org/missing")
    assert e.value.status == 404


class _CutShort(io.BytesIO):
    """An answer's body that ends before its length."""

    def read(self, *_):
        raise http.client.IncompleteRead(b"par", 10)


def test_an_error_whose_body_is_cut_short_is_a_fetch_error_of_its_status():
    c, _, _ = make_client({"https://example.org/busy": (503, _CutShort(), {})})
    with pytest.raises(fetcher.FetchError, match="503") as e:
        c.get("https://example.org/busy")
    assert e.value.status == 503 and c.received("example.org") == 0


def test_an_answer_whose_body_is_cut_short_is_a_fetch_error_without_a_status():
    c, _, _ = make_client({"https://example.org/part": (200, _CutShort(), {})})
    with pytest.raises(fetcher.FetchError) as e:
        c.get("https://example.org/part")
    assert e.value.status is None


def test_no_answer_becomes_a_fetch_error_without_a_status():
    c, _, _ = make_client({"https://example.org/down": OSError("unreachable")})
    with pytest.raises(fetcher.FetchError, match="no answer OSError: unreachable") as e:
        c.get("https://example.org/down")
    assert e.value.status is None


def test_only_https_is_sent():
    c, _, seen = make_client()
    for url in ("http://example.org/a", "file:///etc/hosts", "ftp://example.org/a"):
        with pytest.raises(fetcher.FetchError, match="https"):
            c.get(url)
    assert seen == []


def test_params_are_encoded_into_the_url():
    c, _, seen = make_client()
    resp = c.get("https://example.org/find", params={"text": "a b", "q": "x&y"})
    assert seen[0].full_url == "https://example.org/find?text=a+b&q=x%26y"
    assert resp.json() == {"ok": True} and resp.status == 200


def test_a_lookup_is_posted_as_json():
    c, _, seen = make_client()
    c.post_json("https://example.org/map", [{"idType": "TICKER"}], headers={"Accept": "application/json"})
    req = seen[-1]
    assert (req.get_method(), json.loads(req.data), req.get_header("Content-type"), req.get_header("Accept")) == \
        ("POST", [{"idType": "TICKER"}], "application/json", "application/json")


def test_a_disclaimer_is_posted_as_a_form():
    c, _, seen = make_client()
    c.post_form("https://example.org/entry", {"_action_set": "I agree"})
    req = seen[-1]
    assert (req.get_method(), req.data, req.get_header("Content-type")) == \
        ("POST", b"_action_set=I+agree", "application/x-www-form-urlencoded")


def test_a_header_never_replaces_the_user_agent():
    c, _, seen = make_client()
    c.get("https://example.org/a", headers={"User-Agent": "Mozilla/5.0"})
    assert seen[0].get_header("User-agent") == "wealthdb-refdata/1"


def test_an_answer_that_is_not_json_fails_the_request():
    resp = fetcher.Response("https://example.org/a", 200, b"<html>busy</html>")
    with pytest.raises(fetcher.FetchError, match="not JSON"):
        resp.json()
    assert fetcher.Response("https://example.org/a", 200, b'\xef\xbb\xbf{"a": 1}').json() == {"a": 1}


def test_cookies_are_kept_for_a_session_host_only():
    class Resp:
        def __init__(self, headers):
            self._h = headers

        def info(self):
            return self._h

    c, _, _ = make_client()
    # A host's own cookie and one set for its whole domain are kept; a
    # cookie from any other host is not.
    for host, cookie in (("www.swissfunddata.ch", "JSESSIONID=x1; Path=/"),
                         ("www.swissfunddata.ch", "SCDID_S=x2; Path=/; Domain=.swissfunddata.ch"),
                         ("www.example.org", "JSESSIONID=x3; Path=/")):
        h = email.message.Message()
        h["Set-Cookie"] = cookie
        c.cookies.extract_cookies(Resp(h), urllib.request.Request(f"https://{host}/a"))
    assert sorted(k.value for k in c.cookies) == ["x1", "x2"]
    back = urllib.request.Request("https://www.swissfunddata.ch/b")
    c.cookies.add_cookie_header(back)
    assert "JSESSIONID=x1" in back.get_header("Cookie") and "SCDID_S=x2" in back.get_header("Cookie")
    other = urllib.request.Request("https://www.example.org/b")
    c.cookies.add_cookie_header(other)
    assert not other.has_header("Cookie")


ENTRY = "https://www.swissfunddata.ch/sfdpub/sfd-entry/private?url=%2Fen%2F"
MOVED = "https://www.swissfunddata.ch/sfdpub/en/"


def test_a_same_host_redirect_is_followed_at_the_pace_with_the_session():
    # The disclaimer's answer sets the session and redirects to a page
    # the site does not have.
    c, sleeps, seen = make_client({ENTRY: (302, b"", {"Location": MOVED, "Set-Cookie": "JSESSIONID=s1; Path=/"}),
                                   MOVED: (404, b"", {})})
    with pytest.raises(fetcher.FetchError, match="404") as e:
        c.post_form(ENTRY, {"_action_set": "I agree"})
    assert e.value.url == MOVED
    assert [(r.get_method(), r.full_url) for r in seen] == [("POST", ENTRY), ("GET", MOVED)]
    assert seen[1].get_header("Cookie") == "JSESSIONID=s1" and sleeps == [1.0]
    # A 404 is no refusal: the site is asked again.
    assert c.refused == {}


def test_a_redirect_to_another_host_drops_the_contact():
    moved = "https://files.example.org/tickers.json"
    c, sleeps, seen = make_client({"https://www.sec.gov/files/tickers.json": (301, b"", {"Location": moved})})
    assert c.get("https://www.sec.gov/files/tickers.json").url == moved
    assert seen[0].get_header("User-agent") == "wealthdb-refdata/1 ops@example.org"
    assert seen[1].get_header("User-agent") == "wealthdb-refdata/1"
    # Each host keeps its own pace: the first request to the new host
    # waits for nothing.
    assert sleeps == []


def test_a_redirect_the_client_would_not_send_is_refused():
    c, _, seen = make_client({"https://example.org/plain": (302, b"", {"Location": "http://example.org/plain"}),
                              "https://example.org/sec": (302, b"", {"Location": "https://data.sec.gov/x"}),
                              "https://data.sec.gov/y": (403, b"", {})})
    with pytest.raises(fetcher.FetchError, match="https"):
        c.get("https://example.org/plain")
    with pytest.raises(fetcher.FetchError):
        c.get("https://data.sec.gov/y")
    # The SEC refused: a redirect does not lead back to it.
    with pytest.raises(fetcher.FetchError, match="refused an earlier request"):
        c.get("https://example.org/sec")
    assert [r.full_url for r in seen] == ["https://example.org/plain", "https://data.sec.gov/y",
                                          "https://example.org/sec"]


def test_a_refusal_after_a_redirect_counts_against_the_host_that_refused():
    c, _, _ = make_client({"https://example.org/a": (302, b"", {"Location": "https://www.sec.gov/a"}),
                           "https://www.sec.gov/a": (403, b"", {})})
    with pytest.raises(fetcher.FetchError):
        c.get("https://example.org/a")
    assert set(c.refused) == {"sec"}


def test_every_exchange_is_traced(tmp_path):
    trace = debugcap.HttpTrace(tmp_path, log=logging.getLogger("test"), enabled=True)
    c, _, _ = make_client({"https://example.org/old": (302, b"", {"Location": "https://example.org/new"}),
                           "https://example.org/down": OSError("unreachable")}, trace=trace)
    c.get("https://example.org/old")
    with pytest.raises(fetcher.FetchError):
        c.get("https://example.org/down")
    lines = [json.loads(ln) for ln in (tmp_path / debugcap.SCREENSHOTS_DIR / "http-trace.jsonl").read_text().splitlines()]
    assert [(e["url"], e.get("status")) for e in lines] == [
        ("https://example.org/old", 302), ("https://example.org/new", 200), ("https://example.org/down", None)]
    assert lines[1]["bytes"] == len(OK[1]) and "unreachable" in lines[2]["error"]


def test_a_transport_error_is_masked(tmp_path):
    """A transport error's text reaches the trace and the FetchError
    through debugcap.safe_error, so a credential a URL in it carries is
    masked in both."""
    trace = debugcap.HttpTrace(tmp_path, log=logging.getLogger("test"), enabled=True)
    leak = OSError("cannot reach https://example.org/x?api_key=SECRET0000&q=1")
    c, _, _ = make_client({"https://example.org/down": leak}, trace=trace)
    with pytest.raises(fetcher.FetchError) as e:
        c.get("https://example.org/down")
    assert "SECRET0000" not in str(e.value) and "q=1" in str(e.value)
    text = (tmp_path / debugcap.SCREENSHOTS_DIR / "http-trace.jsonl").read_text()
    assert "SECRET0000" not in text and "cannot reach" in text


TOKEN = "Bearer EXAMPLE-TOKEN-0000"


def test_an_authorization_header_goes_to_its_host_only_and_never_to_the_trace(tmp_path, caplog):
    trace = debugcap.HttpTrace(tmp_path, log=logging.getLogger("test"), enabled=True)
    first, same, other = "https://api.example.org/a", "https://api.example.org/b", "https://files.example.org/c"
    c, _, seen = make_client({first: (302, b"", {"Location": same}), same: (302, b"", {"Location": other}),
                              other: OK, "https://api.example.org/busy": (429, b"slow down", {})}, trace=trace)
    with caplog.at_level(logging.DEBUG):
        c.get(first, headers={"Authorization": TOKEN})
        with pytest.raises(fetcher.FetchError) as e:
            c.get("https://api.example.org/busy", headers={"Authorization": TOKEN})
    # A redirect on the host keeps the header; one to another host drops it.
    assert [r.get_header("Authorization") for r in seen] == [TOKEN, TOKEN, None, TOKEN]
    text = (tmp_path / debugcap.SCREENSHOTS_DIR / "http-trace.jsonl").read_text()
    assert len(text.splitlines()) == 4
    assert "EXAMPLE-TOKEN" not in text + caplog.text + str(e.value)


def test_the_bytes_of_every_answer_are_counted_per_host():
    c, _, _ = make_client({"https://api.example.org/a": (200, b"12345", {}),
                           "https://api.example.org/gone": (404, b"gone", {}),
                           "https://www.example.org/old": (301, b"", {"Location": "https://api.example.org/a"})})
    c.get("https://api.example.org/a")
    with pytest.raises(fetcher.FetchError):
        c.get("https://api.example.org/gone")
    # A redirect's answer counts at the host that answers it.
    c.get("https://www.example.org/old")
    assert (c.received("api.example.org"), c.received("API.Example.org"), c.received("www.example.org"),
            c.received("other.example.org")) == (14, 14, 0, 0)
