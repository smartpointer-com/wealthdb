"""Tests for the interest-condition capture and its load.

Three things carry weight. The **route guard**: the pass runs on a
fully privileged session, so while it runs the page may GET the legacy
pages and the account API only by their few routes, each with the one
query the pages give it, and never write to either. The **browser
reads**: the SPA's own account API response, and the legacy pages the
frames render, with an account the API covered not read again. And
the **reading**: tiers in source order, the caption a nested table
carries, and no row for an account that states no condition. A fake
browser stands in for Playwright; nothing reaches a network.

Every value is invented. The IBANs are the IBAN specification's example
and placeholders of its shape.
"""
from __future__ import annotations

import json
import sqlite3

import pytest

import landmarks as ubs
import load
import rate_parsers
import rates
from collectorkit import bronze
from silverdb import fresh_db

IBAN_API = "CH9300762011623852957"
IBAN_API_PRINTED = "CH93 0076 2011 6238 5295 7"
IBAN_LEGACY = "CH5600000000000000001"
IBAN_LEGACY_PRINTED = "CH56 0000 0000 0000 0000 1"
IBAN_OTHER = "CH3800000000000000002"
IBAN_OTHER_PRINTED = "CH38 0000 0000 0000 0000 2"

API_REF = "/api/v3/cash-accounts/acct-opaque-1?include=INTEREST_RATES"
ENTRY = ("/assetview/ebidispatcher.do?navitemid=AssetsPg"
         "&goToScreen=AO_Objects&tabControl=account_balance"
         "&assetId=PORTFOLIO-1")
LIST_REF = "/assetview/getvaluatedcontractlist.do?parameters=TElTVC0x%3D"
SPA = "https://bank.test/app/ebanking/spa.html"


def _api_payload(iban=IBAN_API_PRINTED, items=None):
    return {
        "accountId": "acct-opaque-1",
        "iban": iban,
        "currency": "CHF",
        "accountType": "EXAMPLE",
        "interestItems": items if items is not None else [
            {"conditionType": "CREDIT", "interestRate": "0.5",
             "upperLimit": "25000", "validFrom": "2026-01-01"},
            {"conditionType": "CREDIT", "interestRate": "0.25",
             "upperLimit": "100000.00", "validFrom": "2026-01-01"},
            {"conditionType": "CREDIT", "interestRate": "0",
             "upperLimit": "", "validFrom": "2026-01-01"},
            {"conditionType": "DEBIT", "interestRate": "9.75",
             "upperLimit": "", "validFrom": "2025-07-01"},
        ],
    }


def _rate_table(caption, rows):
    # The caption sits in a table of its own, in the first row.
    body = "".join(
        f"<tr><td><div>{d}</div></td><td><div>{r}</div></td>"
        f"<td><div>{lim}</div></td></tr>" for d, r, lim in rows)
    return ("<table><tr><td colspan='3'><div><table><tbody><tr>"
            f"<td><div>{caption}</div></td><td></td></tr></tbody></table>"
            "</div></td></tr>"
            "<tr class='uwrTableHeading'><td scope='col'><div>Valid from</div>"
            "</td><td scope='col'><div>Interest rate</div></td>"
            "<td scope='col'><div>Limit</div></td></tr>"
            f"{body}</table>")


def _chooser_row(iban_printed, label, *, selected=False):
    cls = "bzeSelectDialogRow selected" if selected else "bzeSelectDialogRow"
    return (f"<li><div class='{cls}'><table class='bzeSelectDialogTable'>"
            f"<tbody><tr><td>{iban_printed} / BANKCHZZXXX</td></tr>"
            f"<tr><td>CHF&nbsp;{label}<img src='x.gif' /></td></tr>"
            "</tbody></table></div></li>")


def _legacy_page(credit_rows, debit_rows, *, iban=IBAN_LEGACY_PRINTED,
                 repeated=True):
    tables = (_rate_table("Credit interest", credit_rows)
              + _rate_table("Debit interest", debit_rows))
    # A repeat of the tables with other figures; only the first is read.
    again = (_rate_table("Credit interest", [("01.01.2020", "9.999%", "")])
             + _rate_table("Debit interest", [("01.01.2020", "9.999%", "")])
             if repeated else "")
    # The account chooser lists the other accounts beside the page's
    # own, which is the selected row.
    return f"""<html><body>
<h1><em>Account</em> information</h1>
<div id="chooserDialog" class="bzeMainDialog"><ul>
{_chooser_row(IBAN_OTHER_PRINTED, "Another account")}
{_chooser_row(iban, "This account", selected=True)}
</ul></div>
<div>Account balance CHF 1.00</div>
{tables}
<div>{again}</div>
</body></html>"""


def _menu_script(keys=("KONTO", "KONTODETAILS", "PAY")):
    labels = {"KONTO": "Account transactions",
              "KONTODETAILS": "Account details", "PAY": "Payment"}
    items = ",".join(
        f"m().createItem1('i{k}','{labels[k]}', '',null,null, s(),"
        "new jamesPopupMenuLink1('javascript:window.open("
        f'\\"/assetview/link.do?link=\\" + getLinkId().{k} + \\"&\\",'
        "\\\"_parent\\\" );', ''))" for k in keys)
    return (f"<script>setTimeout(\"uwrCreateMenu1('null','m1',"
            f"'uwrActionIconMenu','250px',[{items}])\", 0);</script>")


def _account_row(label, iban_printed, ids):
    ids_js = ",".join(f"{k}:'{v}'" for k, v in ids.items())
    return (f"<tr><td><div>{label}</div></td><td><div>{iban_printed}</div>"
            "</td><td><div><button onclick=\"uwrActionIconInitMenu('m1');"
            f"setLinkId({{{ids_js}}});uwrToggleMenu1(this, 'm1');\" "
            "type='button' title='Other functions'><img src='x.gif'>"
            "</button></div></td></tr>")


def _contract_list(rows, *, script=None):
    body = "".join(_account_row(*r) for r in rows)
    return (f"<html><body>{_menu_script() if script is None else script}"
            f"<table>{body}</table></body></html>")


# --------------------------------------------------------------------
# The request guard
# --------------------------------------------------------------------

@pytest.mark.parametrize("ref", [
    API_REF,
    "/api/v3/cash-accounts/AbC_12-x.y~z==?include=INTEREST_RATES",
    ENTRY,
    LIST_REF,
    "/assetview/link.do?link=AV12&",
    "/assetview/link.do?link=AV12",
    "/assetview/ebidispatcher.do?uri=getpositiondetail.do"
    "&parameters=T1BBUVVF%2B1",
    "/assetview/getpositiondetail.do?parameters=T1BBUVVFLTE=",
])
def test_the_read_routes_are_admitted_with_their_queries(ref):
    assert rates.refuse_request("GET", ref) is None


@pytest.mark.parametrize("ref", [
    # The account API without its include query, or with another.
    "/api/v3/cash-accounts/acct-opaque-1",
    "/api/v3/cash-accounts/acct-opaque-1?include=TRANSACTIONS",
    "/api/v3/cash-accounts/acct-opaque-1?include=INTEREST_RATES&x=1",
    "/api/v3/cash-accounts/acct-opaque-1?expand=all",
    # The list endpoint and its neighbours are not this pass's business.
    "/api/v3/cash-accounts?include=INTEREST_RATES",
    "/api/v3/cash-accounts/acct-opaque-1/transactions"
    "?include=INTEREST_RATES",
    "/api/v1/payments/orders",
    # A bare legacy page, and the position list in any form.
    "/assetview/getvaluatedcontractlist.do",
    "/assetview/getdetailpositionlist.do",
    "/assetview/getdetailpositionlist.do?parameters=T1",
    "/assetview/getpositiondetail.do",
    "/assetview/getpositiondetail.do?parameters=T1&action=close",
    "/assetview/getpositiondetail.do?parameters=T1&parameters=T2",
    "/assetview/getpositiondetail.do?parameters=T1#top",
    "/assetview/link.do",
    "/assetview/link.do?link=PAY",
    # The dispatcher bare, to another page, with more, or another entry.
    "/assetview/ebidispatcher.do",
    "/assetview/ebidispatcher.do?uri=getcashtransactionlist.do"
    "&parameters=T1",
    "/assetview/ebidispatcher.do?uri=getpositiondetail.do&parameters=T1"
    "&hackModel.ST=2",
    "/assetview/ebidispatcher.do?navitemid=AssetsPg&goToScreen=AO_Objects"
    "&tabControl=extern&assetId=PORTFOLIO-1",
    "/assetview/ebidispatcher.do?navitemid=PayPg&goToScreen=AO_Objects"
    "&tabControl=account_balance&assetId=PORTFOLIO-1",
    "/assetview/other/getpositiondetail.do?parameters=T1",
    # Off-site references.
    "https://evil.test" + API_REF,
    "//evil.test" + LIST_REF,
])
def test_everything_else_is_refused(ref):
    assert rates.refuse_request("GET", ref) is not None


@pytest.mark.parametrize("ref", [
    "/api/v3/cash-accounts/..?include=INTEREST_RATES",
    "/api/v3/cash-accounts/.?include=INTEREST_RATES",
    "/api/v3/cash-accounts/...?include=INTEREST_RATES",
    "/api/v3/cash-accounts/../../v1/payments?include=INTEREST_RATES",
    "/assetview/../assetview/getvaluatedcontractlist.do?parameters=T1",
    "/assetview/./getpositiondetail.do?parameters=T1",
])
def test_a_dot_segment_is_refused(ref):
    # URL resolution would turn each of these into another path.
    assert rates.refuse_request("GET", ref) is not None


@pytest.mark.parametrize("method", ["POST", "PUT", "DELETE", "PATCH"])
def test_a_write_is_refused_even_on_an_admitted_route(method):
    assert rates.refuse_request(method, API_REF) is not None


# --------------------------------------------------------------------
# The browser the pass reads, faked
# --------------------------------------------------------------------

ORIGIN = "https://bank.test"
# A long account token with digits in it, as the homepage gives one;
# no error may print it whole.
ACCOUNT = "acctOpaqueToken0001AbCdEf"
API_REF_LONG = (f"/api/v3/cash-accounts/{ACCOUNT}"
                "?include=INTEREST_RATES")


class _Response:
    def __init__(self, url, body="", status=200):
        self.url = url
        self.status = status
        self.ok = status < 400
        self._body = body

    def json(self):
        return json.loads(self._body)

    def text(self):
        return self._body


class _Route:
    def __init__(self):
        self.result = None

    def continue_(self):
        self.result = "continued"

    def abort(self):
        self.result = "aborted"


class _Request:
    def __init__(self, method, url):
        self.method = method
        self.url = url


class _Element:
    def __init__(self, frame):
        self._frame = frame

    def content_frame(self):
        return self._frame


class _Frame:
    """A frame of the fake browser: its URL, the page it frames in its
    main frame, and the HTML it loaded."""

    def __init__(self, browser, url):
        self.browser = browser
        self.url = url
        self.child = None
        self.html = ""

    def goto(self, url, referer=None, **_kw):
        self.browser.opened.append(url.removeprefix(ORIGIN))
        self.browser.referers.append(referer)
        return self.browser.load(self, url)

    def wait_for_selector(self, selector, **_kw):
        if self.child is None:
            raise TimeoutError(f"no {selector}")
        return _Element(self.child)

    def wait_for_load_state(self, *_a, **_kw):
        pass

    def content(self):
        return self.html


class _Waiter:
    def __init__(self, browser, predicate):
        self.browser = browser
        self.predicate = predicate
        self.seen = []

    def __enter__(self):
        self.browser.waiters.append(self)
        return self

    def __exit__(self, *exc):
        self.browser.waiters.remove(self)
        return False

    @property
    def value(self):
        if not self.seen:
            raise TimeoutError("Timeout 30000ms exceeded")
        return self.seen[0]


class _Browser:
    """The page: its route handlers, its response listeners, the SPA's
    own calls per route, and a site of legacy pages.

    `spa` maps a route fragment to what the SPA does there: a response
    to a GET it makes, or a (method, ref) request it makes. `site` maps
    a reference to (status, html, redirect target, framed reference).
    """

    def __init__(self, spa=None, site=None):
        self.url = SPA + "#/home"
        self.spa = spa or {}
        self.site = site or {}
        self.routes = []
        self.listeners = []
        self.waiters = []
        self.went = []
        self.opened = []
        self.referers = []
        self.entries = 0
        self.requests = []

    # The page API the pass uses.
    def route(self, pattern, handler):
        self.routes.append((pattern, handler))

    def unroute(self, pattern, handler):
        self.routes.remove((pattern, handler))

    def on(self, event, handler):
        assert event == "response"
        self.listeners.append(handler)

    def remove_listener(self, event, handler):
        self.listeners.remove(handler)

    def expect_response(self, predicate, **_kw):
        return _Waiter(self, predicate)

    def goto(self, url, **_kw):
        self.url = url
        fragment = url.split("#", 1)[1]
        self.went.append(fragment)
        for call in self.spa.get(fragment, []):
            if isinstance(call, tuple):
                self.request(*call)
            elif self.request("GET", call.url.removeprefix(ORIGIN)):
                self.emit(call)

    # The browser's own doing.
    def request(self, method, ref):
        """A request the browser makes, through the route handlers;
        whether it went out."""
        url = ORIGIN + ref
        self.requests.append((method, ref))
        for pattern, handler in self.routes:
            if pattern.match(url):
                route = _Route()
                handler(route, _Request(method, url))
                return route.result == "continued"
        return True

    def emit(self, response):
        for listener in list(self.listeners):
            listener(response)
        for waiter in self.waiters:
            if waiter.predicate(response):
                waiter.seen.append(response)

    def load(self, frame, url, *, framed=False):
        """Load `url` in `frame` as a browser does: the route guard,
        then the redirect, then the page it frames. An aborted load
        fails a navigation, and leaves a framed page empty."""
        ref = url.removeprefix(ORIGIN)
        if not self.request("GET", ref):
            if framed:
                frame.url, frame.child = url, None
                return None
            raise RuntimeError(f"net::ERR_FAILED at {url}")
        status, html, redirect, framed = self.site.get(ref,
                                                       (404, "", None, None))
        if redirect:
            return self.load(frame, ORIGIN + redirect)
        response = _Response(url, html, status)
        self.emit(response)
        frame.url, frame.html, frame.child = url, html, None
        if framed:
            frame.child = _Frame(self, ORIGIN + framed)
            self.load(frame.child, ORIGIN + framed, framed=True)
        return response

    def open_entry(self):
        """The SPA's "Account balances" view: its frame shows the entry."""
        self.entries += 1
        frame = _Frame(self, ORIGIN + ENTRY)
        self.load(frame, ORIGIN + ENTRY)
        return frame


def _surface(browser, open_legacy=None, shots=None):
    return rates.Surface(
        page=browser, origin=ORIGIN,
        spa_url=lambda route: SPA + route,
        open_legacy=open_legacy or browser.open_entry,
        shot=(shots.append if shots is not None else lambda _n: None))


def _info(account_id):
    return ubs.account_information_route(account_id).removeprefix("#")


def _site(*chains, list_html=None):
    """A site with the entry, its contract list, and per account the
    "Account details" link's redirect, its dispatcher and its page."""
    site = {ENTRY: (200, "<html>entry</html>", None, LIST_REF),
            LIST_REF: (200, list_html or "", None, None)}
    for link_id, token, status, html in chains:
        dispatcher = ("/assetview/ebidispatcher.do?uri=getpositiondetail.do"
                      f"&parameters={token}")
        detail = f"/assetview/getpositiondetail.do?parameters={token}"
        site[f"/assetview/link.do?link={link_id}&"] = (302, "", dispatcher,
                                                       None)
        site[dispatcher] = (200, "<html>dispatcher</html>", None, detail)
        site[detail] = (status, html, None, None)
    return site


def _no_entry():
    raise RuntimeError("the menu did not open")


_ACCOUNTS = [{"account_id": "acct-opaque-1", "kind": "cash"}]


# --------------------------------------------------------------------
# The account API, as the SPA calls it
# --------------------------------------------------------------------

def test_the_spas_own_response_is_kept_and_another_ignored(tmp_path):
    browser = _Browser(spa={_info("acct-opaque-1"): [
        # The SPA's other calls on the route are not the pass's.
        _Response(ORIGIN + "/api/v3/cash-accounts/acct-opaque-1",
                  json.dumps({"iban": IBAN_OTHER_PRINTED})),
        _Response(ORIGIN + "/api/v3/cash-accounts?pageSize=20", "{}"),
        _Response("https://elsewhere.test" + API_REF,
                  json.dumps(_api_payload(iban=IBAN_OTHER_PRINTED))),
        _Response(ORIGIN + API_REF, json.dumps(_api_payload())),
    ]})
    meta = rates.capture(_surface(browser, _no_entry), tmp_path, _ACCOUNTS)
    assert browser.went == [_info("acct-opaque-1")]
    saved = json.loads((tmp_path / "rates" / "acct-opaque-1.json")
                       .read_text())
    assert rate_parsers.api_iban(saved) == IBAN_API
    assert meta["api"]["answered"][0]["states_conditions"] is True
    # The pass itself requested nothing: every request was the SPA's.
    assert browser.opened == []


def test_no_response_within_the_timeout_is_an_error(tmp_path):
    shots = []
    browser = _Browser()
    meta = rates.capture(_surface(browser, _no_entry, shots), tmp_path,
                         [{"account_id": ACCOUNT, "kind": "cash"}])
    api = [e for e in meta["errors"] if e["stage"] == "api"]
    assert len(api) == 1
    assert "no account API response" in api[0]["error"]
    assert ACCOUNT not in json.dumps(meta)
    assert any(s.startswith("rates-api-") for s in shots)


def test_a_failed_spa_call_records_its_status_and_body(tmp_path, caplog):
    body = json.dumps({"type": "about:blank", "title": "Bad Request",
                       "detail": f"invalid account {ACCOUNT}"})
    browser = _Browser(spa={_info(ACCOUNT): [
        _Response(ORIGIN + API_REF_LONG, body, 400)]})
    meta = rates.capture(_surface(browser, _no_entry), tmp_path,
                         [{"account_id": ACCOUNT, "kind": "cash"}])
    error = next(e for e in meta["errors"] if e["stage"] == "api")
    assert error["http_status"] == 400
    assert "Bad Request" in error["body"]
    assert ACCOUNT not in json.dumps(meta)
    assert ACCOUNT not in caplog.text
    assert list((tmp_path / "rates").iterdir()) == []


@pytest.mark.parametrize("items,followed", [
    (None, True),     # no interestItems: the API said nothing
    ([], False),      # an empty list: the account has no conditions
])
def test_only_a_stated_condition_list_covers_an_account(tmp_path, items,
                                                        followed):
    payload = _api_payload(iban=IBAN_LEGACY_PRINTED, items=[])
    if items is None:
        del payload["interestItems"]
    browser = _Browser(
        spa={_info("acct-opaque-1"): [
            _Response(ORIGIN + API_REF, json.dumps(payload))]},
        site=_site(("AV22", "TOKEN-B", 200,
                    _legacy_page([("01.01.2026", "0.100%", "")], [])),
                   list_html=_contract_list([
                       ("Cash account", IBAN_LEGACY_PRINTED,
                        {"KONTODETAILS": "AV22"})])))
    meta = rates.capture(_surface(browser), tmp_path, _ACCOUNTS)
    assert meta["api"]["answered"][0]["states_conditions"] is not followed
    assert ("/assetview/link.do?link=AV22&" in browser.opened) is followed
    assert (len(meta["legacy"]["answered"]) == 1) is followed


@pytest.mark.parametrize("token", ["..", ".", "...", "-._~", "=="])
def test_a_token_without_a_letter_or_digit_is_not_opened(tmp_path, token):
    browser = _Browser()
    meta = rates.capture(_surface(browser, _no_entry), tmp_path,
                         [{"account_id": token, "kind": "cash"}])
    assert browser.went == []
    assert "not in the admitted set" in meta["errors"][0]["error"]


# --------------------------------------------------------------------
# The legacy view, as the browser renders it
# --------------------------------------------------------------------

def test_the_legacy_chain_reads_the_rendered_pages(tmp_path, caplog):
    caplog.set_level("INFO", logger="ubs-web.rates")
    listing = _contract_list([
        ("Personal account", IBAN_API_PRINTED,
         {"KONTO": "AV11", "KONTODETAILS": "AV12", "PAY": "AV13"}),
        ("Mandate cash account", IBAN_LEGACY_PRINTED,
         {"KONTO": "AV21", "KONTODETAILS": "AV22", "PAY": "AV23"}),
        ("Another cash account", IBAN_OTHER_PRINTED,
         {"KONTO": "AV31", "KONTODETAILS": "AV32", "PAY": "AV33"}),
    ])
    browser = _Browser(
        spa={_info("acct-opaque-1"): [
            _Response(ORIGIN + API_REF, json.dumps(_api_payload()))]},
        site=_site(("AV22", "TOKEN-B", 200,
                    _legacy_page([("01.01.2026", "0.100%", "")], [])),
                   ("AV32", "TOKEN-C", 500, "<html>error</html>"),
                   list_html=listing))
    meta = rates.capture(_surface(browser), tmp_path, _ACCOUNTS)

    # Opened in the entry's frame, as the item opens its link in the
    # list's parent, from the list: the details link, then, after
    # "Account balances" is opened again for a fresh render, the next
    # account's link. The covered account (AV12) and every payment id
    # are never opened.
    assert browser.opened == ["/assetview/link.do?link=AV22&",
                              "/assetview/link.do?link=AV32&"]
    assert browser.referers == [ORIGIN + LIST_REF] * 2
    assert browser.entries == 2
    files = sorted(p.name for p in (tmp_path / "rates").iterdir())
    assert files == sorted(["acct-opaque-1.json",
                            rates.legacy_file_name(IBAN_LEGACY)])
    assert meta["legacy"]["listed"] == 3
    assert len(meta["legacy"]["skipped_covered"]) == 1
    assert len(meta["legacy"]["attempted"]) == 2
    assert len(meta["legacy"]["answered"]) == 1
    [error] = meta["errors"]
    assert error["stage"] == "legacy"
    assert error["http_status"] == 500
    assert ("rates: api 1 of 1 answered, 1 stating conditions; legacy 1 "
            "of 2 answered, 1 covered by the api; 1 error(s)") in caplog.text
    # The guard is gone once the pass ends.
    assert browser.routes == [] and browser.listeners == []


def test_a_list_that_answers_an_error_is_recorded_with_its_status(tmp_path):
    shots = []
    site = _site()
    site[LIST_REF] = (500, "<html>Internal error</html>", None, None)
    browser = _Browser(site=site)
    meta = rates.capture(_surface(browser, shots=shots), tmp_path, [])
    [error] = meta["errors"]
    assert error["stage"] == "legacy-list"
    assert error["http_status"] == 500
    assert "Internal error" in error["body"]
    assert "rates-legacy-list-failed" in shots
    assert browser.opened == []


def test_a_frame_that_shows_another_page_is_a_failed_list(tmp_path):
    site = _site()
    site[ENTRY] = (200, "", None, "/assetview/getcashtransactionlist.do"
                   "?parameters=T1")
    shots = []
    browser = _Browser(site=site)
    meta = rates.capture(_surface(browser, shots=shots), tmp_path, [])
    stages = [e["stage"] for e in meta["errors"]]
    # The guard aborts the frame's load of a page outside the
    # allow-list; the list is then missing from the frame.
    assert stages == ["legacy-list", "guard"]
    assert "rates-legacy-list-failed" in shots


def test_a_missed_entry_is_recorded_and_the_api_files_stand(tmp_path):
    browser = _Browser(spa={_info("acct-opaque-1"): [
        _Response(ORIGIN + API_REF, json.dumps(_api_payload()))]})
    meta = rates.capture(_surface(browser, _no_entry), tmp_path, _ACCOUNTS)
    assert meta["errors"] == [{"stage": "legacy-entry",
                               "error": "RuntimeError: the menu did not open"}]
    assert [p.name for p in (tmp_path / "rates").iterdir()] == [
        "acct-opaque-1.json"]


def test_a_details_link_that_leads_elsewhere_reads_nothing(tmp_path):
    site = _site(list_html=_contract_list([
        ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"})]))
    site["/assetview/link.do?link=AV22&"] = (302, "", LIST_REF, None)
    browser = _Browser(site=site)
    meta = rates.capture(_surface(browser), tmp_path, [])
    [error] = meta["errors"]
    assert "led to getvaluatedcontractlist.do" in error["error"]
    assert list((tmp_path / "rates").iterdir()) == []


def test_a_page_of_another_account_is_recorded_and_not_kept(tmp_path):
    browser = _Browser(site=_site(
        ("AV22", "TOKEN-B", 200,
         _legacy_page([("01.01.2026", "0.100%", "")], [],
                      iban=IBAN_OTHER_PRINTED)),
        list_html=_contract_list([
            ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"})])))
    meta = rates.capture(_surface(browser), tmp_path, [])
    assert [e["error"] for e in meta["errors"]] == [
        "the page names another account than its row"]
    assert list((tmp_path / "rates").iterdir()) == []


def test_an_account_gone_from_a_fresh_render_is_recorded(tmp_path):
    first = _contract_list([
        ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"}),
        ("Another", IBAN_OTHER_PRINTED, {"KONTODETAILS": "AV32"}),
    ])
    second = _contract_list([
        ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV42"}),
    ])
    site = _site(("AV22", "TOKEN-B", 200,
                  _legacy_page([("01.01.2026", "0.100%", "")], [])),
                 list_html=first)
    browser = _Browser(site=site)
    surface = _surface(browser)

    def entry_then_second_render():
        frame = browser.open_entry()
        site[LIST_REF] = (200, second, None, None)  # the next render
        return frame

    meta = rates.capture(rates.Surface(**{**surface.__dict__,
                                          "open_legacy":
                                          entry_then_second_render}),
                         tmp_path, [])
    assert [p.name for p in (tmp_path / "rates").iterdir()] == [
        rates.legacy_file_name(IBAN_LEGACY)]
    assert [e["error"] for e in meta["errors"]] == [
        "an account left the list"]


def test_an_unnamed_row_or_page_is_recorded_and_not_kept(tmp_path):
    browser = _Browser(site=_site(
        ("AV22", "TOKEN-B", 200,
         "<html><body><p>Account information</p></body></html>"),
        list_html=_contract_list([
            ("IBAN not printed", "", {"KONTODETAILS": "AV12"}),
            ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"}),
        ])))
    meta = rates.capture(_surface(browser), tmp_path, [])
    assert "/assetview/link.do?link=AV12&" not in browser.opened
    assert [e["error"] for e in meta["errors"]] == [
        "the row prints no IBAN", "the page states no IBAN"]
    assert list((tmp_path / "rates").iterdir()) == []


# --------------------------------------------------------------------
# The route guard
# --------------------------------------------------------------------

def test_the_guard_aborts_a_write_and_a_refused_read(tmp_path):
    browser = _Browser(spa={_info("acct-opaque-1"): [
        ("POST", "/assetview/ebidispatcher.do"),
        ("GET", "/assetview/getcashtransactionlist.do?parameters=T1"),
        ("PUT", f"/api/v3/cash-accounts/{ACCOUNT}"),
        ("GET", f"/api/v3/cash-accounts/{ACCOUNT}/transactions"),
        # The SPA's own reads and searches elsewhere pass untouched.
        ("POST", "/api/v1/payments/orders/search"),
        ("GET", "/assetview/UWR/1.0/js/app.js"),
        ("GET", "/api/v3/cash-accounts?pageSize=20"),
        _Response(ORIGIN + API_REF, json.dumps(_api_payload())),
    ]})
    meta = rates.capture(_surface(browser, _no_entry), tmp_path, _ACCOUNTS)
    aborted = [e for e in meta["errors"] if e["stage"] == "guard"]
    assert [(e["method"], e["ref"].split("?")[0]) for e in aborted] == [
        ("POST", "/assetview/ebidispatcher.do"),
        ("GET", "/assetview/getcashtransactionlist.do"),
        ("PUT", "/api/v3/cash-accounts/" + bronze.short_token(ACCOUNT)),
        ("GET", "/api/v3/cash-accounts/" + bronze.short_token(ACCOUNT)
         + "/transactions"),
    ]
    assert ACCOUNT not in json.dumps(meta)
    # The SPA's own call for the conditions went through.
    assert len(meta["api"]["answered"]) == 1


def test_the_guard_is_removed_when_the_pass_fails(tmp_path, monkeypatch):
    def broken(*_a, **_kw):
        raise RuntimeError("synthetic")

    monkeypatch.setattr(rates.rate_parsers, "api_states_conditions", broken)
    browser = _Browser(spa={_info("acct-opaque-1"): [
        _Response(ORIGIN + API_REF, json.dumps(_api_payload()))]})
    with pytest.raises(RuntimeError, match="synthetic"):
        rates.capture(_surface(browser), tmp_path, _ACCOUNTS)
    assert browser.routes == [] and browser.listeners == []


@pytest.mark.parametrize("method,ref,aborted", [
    ("GET", ENTRY, False),
    ("GET", LIST_REF, False),
    ("GET", "/assetview/link.do?link=AV12&", False),
    ("GET", "/assetview/UWR/1.0/app.css", False),
    ("GET", API_REF, False),
    ("GET", "/api/v3/cash-accounts?pageSize=50", False),
    ("POST", "/api/v1/marketing/campaigns/search", False),
    ("GET", "/assetview/ebidispatcher.do", True),
    ("GET", "/assetview/getdetailpositionlist.do?parameters=T1", True),
    ("GET", "/api/v3/cash-accounts/acct-opaque-1", True),
    ("POST", "/assetview/ebidispatcher.do", True),
    ("POST", "/api/v3/cash-accounts", True),
    ("DELETE", API_REF, True),
    ("POST", "/api/v3/cash-accounts-search", False),
])
def test_what_the_guard_aborts(method, ref, aborted):
    assert (rates.RouteGuard.refusal(method, ORIGIN + ref)
            is not None) is aborted


def test_a_reference_and_an_error_print_short_tokens_only():
    ref = f"/api/v3/cash-accounts/{ACCOUNT}?include=INTEREST_RATES"
    assert rates.redact(ref) == ("/api/v3/cash-accounts/"
                                 f"{bronze.short_token(ACCOUNT)}?include=…")
    text = rates.scrub(f"Timeout at spa.html#/accounts?accountId={ACCOUNT}")
    assert ACCOUNT not in text and bronze.short_token(ACCOUNT) in text
    assert "getvaluatedcontractlist" in rates.scrub(
        "HTTP 500 for getvaluatedcontractlist.do")
    assert rates.excerpt(f"x{ACCOUNT}y") == "<token>"


# --------------------------------------------------------------------
# Reading the two surfaces
# --------------------------------------------------------------------

def test_api_tiers_are_numbered_per_side_in_source_order():
    rows = rate_parsers.api_conditions(_api_payload())
    got = [(r["side"], r["tier"], r["rate_pct"], r["upper_limit"],
            r["valid_from"]) for r in rows]
    assert got == [
        ("credit", 1, 0.5, 25000.0, "2026-01-01"),
        ("credit", 2, 0.25, 100000.0, "2026-01-01"),
        ("credit", 3, 0.0, None, "2026-01-01"),
        ("debit", 1, 9.75, None, "2025-07-01"),
    ]
    assert rate_parsers.api_iban(_api_payload()) == IBAN_API


def test_an_unreadable_api_item_states_nothing():
    rows = rate_parsers.api_conditions(_api_payload(items=[
        {"conditionType": "CREDIT", "interestRate": "n/a", "upperLimit": ""},
        {"conditionType": "BONUS", "interestRate": "1", "upperLimit": ""},
        {"conditionType": "CREDIT", "interestRate": "1", "upperLimit": "lots"},
    ]))
    assert rows == []


def test_the_legacy_page_reads_both_tables_from_the_first_copy():
    html = _legacy_page(
        [("01.01.2026", "0.100%", "50'000.00"), ("01.01.2026", "0.050%", "")],
        [("01.07.2025", "7.500%", "")])
    got = [(r["side"], r["tier"], r["rate_pct"], r["upper_limit"],
            r["valid_from"]) for r in rate_parsers.legacy_conditions(html)]
    assert got == [
        ("credit", 1, 0.1, 50000.0, "2026-01-01"),
        ("credit", 2, 0.05, None, "2026-01-01"),
        ("debit", 1, 7.5, None, "2025-07-01"),
    ]
    # The chooser's selected row, not its other account.
    assert rate_parsers.legacy_iban(html) == IBAN_LEGACY


def test_a_legacy_page_with_headers_and_no_rows_states_nothing():
    html = _legacy_page([], [], repeated=False)
    assert rate_parsers.legacy_conditions(html) == []


def test_a_caption_row_inside_the_table_names_its_side():
    # The caption row is nearer the header row than the heading above
    # the table, so it wins.
    html = """<html><body><h3>Credit interest</h3><table>
<tr><td>Debit interest</td></tr>
<tr><th>Valid from</th><th>Interest rate</th><th>Limit</th></tr>
<tr><td>01.07.2025</td><td>7.500%</td><td></td></tr>
</table></body></html>"""
    got = [(r["side"], r["rate_pct"])
           for r in rate_parsers.legacy_conditions(html)]
    assert got == [("debit", 7.5)]


def test_the_contract_list_gives_each_rows_details_link_once():
    listing = _contract_list([
        ("Cash account", IBAN_LEGACY_PRINTED,
         {"KONTO": "AV11", "KONTODETAILS": "AV12", "PAY": "AV13"}),
        ("The same link again", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV12"}),
        ("No details item", IBAN_OTHER_PRINTED, {"PAY": "AV23"}),
        ("IBAN not printed", "", {"KONTODETAILS": "AV32"}),
    ])
    # Only the details id is read: the payment and transfer ids beside
    # it in the row's map are never turned into a reference.
    assert rate_parsers.legacy_account_rows(listing) == [
        {"iban": IBAN_LEGACY, "ref": "/assetview/link.do?link=AV12&"},
        {"iban": None, "ref": "/assetview/link.do?link=AV32&"},
    ]


def test_a_list_without_the_details_item_gives_no_link():
    listing = _contract_list(
        [("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV12"})],
        script=_menu_script(keys=("KONTO", "PAY")))
    assert rate_parsers.legacy_account_rows(listing) == []


def test_the_page_names_its_account_only_by_the_selected_row():
    page = _legacy_page([], [], iban=IBAN_LEGACY_PRINTED)
    assert rate_parsers.legacy_iban(page) == IBAN_LEGACY
    unselected = page.replace("bzeSelectDialogRow selected",
                              "bzeSelectDialogRow")
    assert rate_parsers.legacy_iban(unselected) is None


def test_a_tag_left_open_in_the_selected_row_ends_with_the_row():
    page = (f"<ul><li><div class='bzeSelectDialogRow selected'><p>Own "
            "account</div></li><li><div class='bzeSelectDialogRow'>"
            f"{IBAN_OTHER_PRINTED}</div></li></ul>")
    assert rate_parsers.legacy_iban(page) is None


@pytest.mark.parametrize("payload,want", [
    ({"iban": IBAN_API_PRINTED, "interestItems": []}, True),
    ({"iban": IBAN_API_PRINTED}, False),
    ({"iban": IBAN_API_PRINTED, "interestItems": None}, False),
    ([], False),
])
def test_only_a_condition_list_states_conditions(payload, want):
    assert rate_parsers.api_states_conditions(payload) is want


@pytest.mark.parametrize("payload", [
    {"account": {"iban": IBAN_API_PRINTED}},
    {"iban": {"formatted": IBAN_API_PRINTED, "country": "CH"}},
])
def test_api_iban_reads_a_wrapper_and_a_structured_field(payload):
    assert rate_parsers.api_iban(payload) == IBAN_API


def test_api_iban_reads_only_a_field_named_iban():
    assert rate_parsers.api_iban({"counterpartyAccount": IBAN_API}) is None


@pytest.mark.parametrize("text,want", [
    ("CHF 100'000.00", 100000.0),
    ("1’000.50", 1000.5),
    ("1\u00a0000", 1000.0),
    ("100,000.00", 100000.0),
    ("", None),
    ("lots", None),
])
def test_parse_amount_drops_the_currency_and_the_separators(text, want):
    assert rate_parsers.parse_amount(text) == want


@pytest.mark.parametrize("text,want", [
    ("01.03.2026", "2026-03-01"),
    ("2026-03-01T00:00:00", "2026-03-01"),
    ("31.02.2026", None),
    ("2026-02-31", None),
    ("March 2026", None),
])
def test_parse_date_reads_both_forms_and_rejects_an_impossible_day(text, want):
    assert rate_parsers.parse_date(text) == want


# --------------------------------------------------------------------
# The load
# --------------------------------------------------------------------

def _dump(tmp_path, name="20260601T080000Z"):
    dump = tmp_path / "bronze" / name
    (dump / "rates").mkdir(parents=True)
    (dump / "run.json").write_text('{"status": "partial", "partial_pass": "rates"}')
    return dump


def _rows(conn: sqlite3.Connection):
    return [tuple(r) for r in conn.execute(
        "SELECT account_external_id, side, tier, rate_pct, upper_limit, "
        "valid_from, surface FROM account_rates "
        "ORDER BY account_external_id, side, tier")]


def test_load_reads_both_surfaces_onto_their_accounts(tmp_path):
    dump = _dump(tmp_path)
    (dump / "rates" / "acct-opaque-1.json").write_text(
        json.dumps(_api_payload()))
    (dump / "rates" / rates.legacy_file_name(IBAN_LEGACY)).write_text(
        _legacy_page([("01.01.2026", "0.100%", "")],
                     [("01.07.2025", "7.500%", "")]))
    # A legacy page for an account the API answered for is not read.
    (dump / "rates" / rates.legacy_file_name(IBAN_API)).write_text(
        _legacy_page([("01.01.2026", "5.000%", "")], [],
                     iban=IBAN_API_PRINTED))
    conn = fresh_db(tmp_path)
    load.load_dump(conn, dump, 16, {}, {})
    assert _rows(conn) == [
        (IBAN_LEGACY, "credit", 1, 0.1, None, "2026-01-01", "legacy"),
        (IBAN_LEGACY, "debit", 1, 7.5, None, "2025-07-01", "legacy"),
        (IBAN_API, "credit", 1, 0.5, 25000.0, "2026-01-01", "api"),
        (IBAN_API, "credit", 2, 0.25, 100000.0, "2026-01-01", "api"),
        (IBAN_API, "credit", 3, 0.0, None, "2026-01-01", "api"),
        (IBAN_API, "debit", 1, 9.75, None, "2025-07-01", "api"),
    ]
    snap = conn.execute("SELECT DISTINCT snapshot_at FROM account_rates").fetchall()
    assert [s[0] for s in snap] == [load.ts_from_dir(dump.name)]


def test_an_account_with_no_conditions_writes_no_row(tmp_path):
    dump = _dump(tmp_path)
    (dump / "rates" / "acct-opaque-1.json").write_text(
        json.dumps(_api_payload(items=[])))
    (dump / "rates" / rates.legacy_file_name(IBAN_LEGACY)).write_text(
        _legacy_page([], [], repeated=False))
    conn = fresh_db(tmp_path)
    load.load_dump(conn, dump, 16, {}, {})
    assert _rows(conn) == []


def test_an_api_file_stating_no_conditions_leaves_the_legacy_page(tmp_path):
    dump = _dump(tmp_path)
    payload = _api_payload(iban=IBAN_LEGACY_PRINTED)
    del payload["interestItems"]
    (dump / "rates" / "acct-opaque-1.json").write_text(json.dumps(payload))
    (dump / "rates" / rates.legacy_file_name(IBAN_LEGACY)).write_text(
        _legacy_page([("01.01.2026", "0.100%", "")], []))
    conn = fresh_db(tmp_path)
    load.load_dump(conn, dump, 16, {}, {})
    assert _rows(conn) == [
        (IBAN_LEGACY, "credit", 1, 0.1, None, "2026-01-01", "legacy")]


def test_a_dump_without_rates_loads_as_zero(tmp_path):
    dump = tmp_path / "bronze" / "20260601T080000Z"
    dump.mkdir(parents=True)
    (dump / "run.json").write_text('{"status": "complete"}')
    conn = fresh_db(tmp_path)
    assert load._load_rates(conn, 0, dump) == 0


def test_a_reread_with_fewer_tiers_leaves_no_old_row(tmp_path):
    dump = _dump(tmp_path)
    path = dump / "rates" / "acct-opaque-1.json"
    path.write_text(json.dumps(_api_payload()))
    conn = fresh_db(tmp_path)
    snapshot_at = load.ts_from_dir(dump.name)
    assert load._load_rates(conn, snapshot_at, dump) == 4
    # Another dump's rows are not this dump's to drop.
    conn.execute(
        "INSERT INTO account_rates (snapshot_at, account_external_id, side, "
        "tier, rate_pct, upper_limit, valid_from, surface, payload) "
        "VALUES (?, ?, 'credit', 1, 0.3, NULL, '2025-01-01', 'api', '{}')",
        (snapshot_at - 86400, IBAN_API))
    path.write_text(json.dumps(_api_payload(items=[
        {"conditionType": "CREDIT", "interestRate": "0.4",
         "upperLimit": "", "validFrom": "2026-01-01"}])))
    assert load._load_rates(conn, snapshot_at, dump) == 1
    assert _rows(conn) == [
        (IBAN_API, "credit", 1, 0.3, None, "2025-01-01", "api"),
        (IBAN_API, "credit", 1, 0.4, None, "2026-01-01", "api"),
    ]
