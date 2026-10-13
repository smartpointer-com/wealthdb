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
    assert seen[0].get_header("User-agent") == "wealthdb-refdata/1 ops@example.org"


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
