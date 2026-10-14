"""The HTTP client: a pace per host and a User-Agent that names the tool."""

import io

import pytest

import fetcher


class FakeResp(io.BytesIO):
    status = 200
    headers = {"Content-Type": "application/json"}

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def make_client(contact="ops@example.org"):
    now = [0.0]
    sleeps, seen = [], []

    def opener(req, timeout):
        seen.append(req)
        return FakeResp(b'{"ok": true}')

    def sleep(s):
        sleeps.append(round(s, 3))
        now[0] += s

    c = fetcher.Client(contact, sleep=sleep, clock=lambda: now[0], opener=opener)
    return c, sleeps, seen


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


def _refusing(code):
    import urllib.error
    calls = []

    def opener(req, timeout):
        calls.append(req.full_url)
        raise urllib.error.HTTPError(req.full_url, code, "Refused", {}, None)

    return fetcher.Client(opener=opener, sleep=lambda s: None), calls


def test_a_source_that_refuses_is_not_asked_again():
    c, calls = _refusing(403)
    for url in ("https://www.sec.gov/a", "https://data.sec.gov/b"):
        with pytest.raises(fetcher.FetchError):
            c.get(url)
    assert calls == ["https://www.sec.gov/a"]


def test_a_plain_403_elsewhere_is_one_page_only():
    c, calls = _refusing(403)
    for url in ("https://example.org/a", "https://example.org/b"):
        with pytest.raises(fetcher.FetchError):
            c.get(url)
    assert len(calls) == 2


def test_a_429_stops_any_source():
    c, calls = _refusing(429)
    for url in ("https://example.org/a", "https://example.org/b"):
        with pytest.raises(fetcher.FetchError):
            c.get(url)
    assert len(calls) == 1


def test_http_error_becomes_a_fetch_error():
    import urllib.error

    def opener(req, timeout):
        raise urllib.error.HTTPError(req.full_url, 404, "Not Found", {}, None)

    c = fetcher.Client(opener=opener, sleep=lambda s: None)
    with pytest.raises(fetcher.FetchError, match="404"):
        c.get("https://example.org/missing")


def test_a_disclaimer_is_posted_as_a_form():
    c, _, seen = make_client()
    c.post_form("https://example.org/entry", {"_action_set": "I agree"})
    req = seen[-1]
    assert (req.get_method(), req.data, req.get_header("Content-type")) == \
        ("POST", b"_action_set=I+agree", "application/x-www-form-urlencoded")


def test_cookies_are_kept_for_a_session_host_only():
    import email.message
    import urllib.request

    class Resp:
        def __init__(self, headers):
            self._h = headers

        def info(self):
            return self._h

    c = fetcher.Client(opener=lambda req, timeout: None)
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
