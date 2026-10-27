"""Tests for the interest-condition capture and its load.

Three things carry weight. The **request guard**: the pass runs on a
fully privileged session, so it may GET its few routes, each with the
one query the pages give it, and nothing else. The **legacy chain**: the
entry the SPA frames, the contract list, each account's "Account
details" redirect and its page, with an account the account API covered
not fetched again. And the **reading**: tiers in source order, the
caption a nested table carries, and no row for an account that states
no condition.

Every value is invented. The IBANs are the IBAN specification's example
and placeholders of its shape.
"""
from __future__ import annotations

import json
import sqlite3

import pytest

import load
import rate_parsers
import rates
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


def _dispatcher(src):
    return (f'<html><body><div><iframe name="gj9MainFrame" '
            f'id="gj9MainFrame" src="{src}"></iframe></div></body></html>')


# --------------------------------------------------------------------
# The request guard
# --------------------------------------------------------------------

class _NO_REQUEST:
    """A context whose every request fails the test."""

    class request:
        @staticmethod
        def get(*_a, **_kw):  # pragma: no cover - must not run
            raise AssertionError("issued a refused request")


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


@pytest.mark.parametrize("token", ["..", ".", "...", "-._~", "=="])
def test_a_token_without_a_letter_or_digit_is_refused(token):
    client = rates.RatesClient(_NO_REQUEST(), "https://bank.test", "KEY", SPA)
    with pytest.raises(RuntimeError):
        client.account(token)


@pytest.mark.parametrize("method", ["POST", "PUT", "DELETE", "PATCH"])
def test_a_write_is_refused_even_on_an_admitted_route(method):
    assert rates.refuse_request(method, API_REF) is not None


def test_the_guard_runs_before_the_request_is_made():
    client = rates.RatesClient(_NO_REQUEST(), "https://bank.test", "KEY", SPA)
    with pytest.raises(RuntimeError, match="refusing to request"):
        client.page("/assetview/ebidispatcher.do", referer=SPA)
    with pytest.raises(RuntimeError, match="refusing to request"):
        client.page("/assetview/getdetailpositionlist.do", referer=SPA)
    with pytest.raises(RuntimeError, match="not in the admitted set"):
        client.account("acct/../payments")


# --------------------------------------------------------------------
# The capture, against a scripted session
# --------------------------------------------------------------------

class _Resp:
    def __init__(self, body="", status=200, location=None):
        self._body = body
        self.status = status
        self.ok = status < 400
        self.headers = {"location": location} if location else {}

    def json(self):
        return json.loads(self._body)

    def text(self):
        return self._body


class _Session:
    """Answers each admitted reference from a fixed map, recording it. A
    list answers its items in turn, the last one again after that."""

    def __init__(self, answers):
        self.answers = answers
        self.requested = []
        self.headers = []
        self.max_redirects = []
        session = self

        class _Request:
            @staticmethod
            def get(url, **kw):
                ref = url.removeprefix("https://bank.test")
                session.requested.append(ref)
                session.headers.append(kw.get("headers"))
                session.max_redirects.append(kw.get("max_redirects"))
                answer = session.answers.get(ref, _Resp("", 404))
                if isinstance(answer, list):
                    return answer.pop(0) if len(answer) > 1 else answer[0]
                return answer

        self.request = _Request


def _client(session, apikey="KEY"):
    return rates.RatesClient(session, "https://bank.test", apikey, SPA + "#/x")


def _detail_chain(link_id, token, page):
    """The answers that lead from one "Account details" link to a page."""
    dispatcher = ("/assetview/ebidispatcher.do?uri=getpositiondetail.do"
                  f"&parameters={token}")
    detail = f"/assetview/getpositiondetail.do?parameters={token}"
    return {
        f"/assetview/link.do?link={link_id}&":
            _Resp(status=302, location=dispatcher),
        dispatcher: _Resp(_dispatcher(detail)),
        detail: page,
    }


_ACCOUNTS = [{"account_id": "acct-opaque-1", "kind": "cash"}]


def _no_entry():
    raise RuntimeError("the menu did not open")


def test_the_api_is_asked_for_the_conditions(tmp_path):
    session = _Session({API_REF: _Resp(json.dumps(_api_payload()))})
    meta = rates.capture(_client(session), tmp_path, _ACCOUNTS, _no_entry)
    assert session.requested == [API_REF]
    assert session.headers[0] == {"accept": "application/json",
                                  "referer": SPA, "apikey": "KEY",
                                  "x-requested-with": "XMLHttpRequest"}
    assert meta["api"]["answered"][0]["states_conditions"] is True


def test_the_legacy_chain_skips_what_the_api_covered(tmp_path, caplog):
    caplog.set_level("INFO", logger="ubs-web.rates")
    render_1 = _contract_list([
        ("Personal account", IBAN_API_PRINTED,
         {"KONTO": "AV11", "KONTODETAILS": "AV12", "PAY": "AV13"}),
        ("Mandate cash account", IBAN_LEGACY_PRINTED,
         {"KONTO": "AV21", "KONTODETAILS": "AV22", "PAY": "AV23"}),
        ("Another cash account", IBAN_OTHER_PRINTED,
         {"KONTO": "AV31", "KONTODETAILS": "AV32", "PAY": "AV33"}),
    ])
    # A fresh render numbers its links anew; the account is found again
    # by its IBAN.
    render_2 = _contract_list([
        ("Personal account", IBAN_API_PRINTED, {"KONTODETAILS": "AV42"}),
        ("Mandate cash account", IBAN_LEGACY_PRINTED,
         {"KONTODETAILS": "AV52"}),
        ("Another cash account", IBAN_OTHER_PRINTED,
         {"KONTODETAILS": "AV62"}),
    ])
    answers = {
        API_REF: _Resp(json.dumps(_api_payload())),
        ENTRY: _Resp(_dispatcher(LIST_REF)),
        LIST_REF: [_Resp(render_1), _Resp(render_2)],
        **_detail_chain("AV22", "TOKEN-B", _Resp(
            _legacy_page([("01.01.2026", "0.100%", "")], []))),
        **_detail_chain("AV62", "TOKEN-C", _Resp("", 500)),
    }
    session = _Session(answers)
    meta = rates.capture(_client(session), tmp_path, _ACCOUNTS,
                         lambda: ENTRY)

    disp_b = ("/assetview/ebidispatcher.do?uri=getpositiondetail.do"
              "&parameters=TOKEN-B")
    disp_c = disp_b.replace("TOKEN-B", "TOKEN-C")
    assert session.requested == [
        API_REF,
        ENTRY, LIST_REF,
        "/assetview/link.do?link=AV22&", disp_b,
        "/assetview/getpositiondetail.do?parameters=TOKEN-B",
        ENTRY, LIST_REF,
        "/assetview/link.do?link=AV62&", disp_c,
        "/assetview/getpositiondetail.do?parameters=TOKEN-C",
    ]
    # No redirect is ever followed by the client, and each page goes
    # with the referer of the page that gave its link.
    assert set(session.max_redirects) == {0}
    referers = [h["referer"] for h in session.headers]
    assert referers[1:6] == [
        SPA, "https://bank.test" + ENTRY, "https://bank.test" + LIST_REF,
        "https://bank.test" + LIST_REF, "https://bank.test" + disp_b]
    assert all("apikey" not in h for h in session.headers[1:])
    files = sorted(p.name for p in (tmp_path / "rates").iterdir())
    assert files == sorted(["acct-opaque-1.json",
                            rates.legacy_file_name(IBAN_LEGACY)])
    assert meta["legacy"]["listed"] == 3
    assert len(meta["legacy"]["skipped_covered"]) == 1
    assert len(meta["legacy"]["attempted"]) == 2
    assert len(meta["legacy"]["answered"]) == 1
    assert [e["stage"] for e in meta["errors"]] == ["legacy"]
    assert "500" in meta["errors"][0]["error"]
    # The one line a run ends on.
    assert ("rates: api 1 of 1 answered, 1 stating conditions; legacy 1 "
            "of 2 answered, 1 covered by the api; 1 error(s)") in caplog.text


@pytest.mark.parametrize("items,followed", [
    (None, True),     # no interestItems: the API said nothing
    ([], False),      # an empty list: the account has no conditions
])
def test_only_a_stated_condition_list_covers_an_account(tmp_path, items,
                                                        followed):
    payload = _api_payload(iban=IBAN_LEGACY_PRINTED, items=[])
    if items is None:
        del payload["interestItems"]
    answers = {
        API_REF: _Resp(json.dumps(payload)),
        ENTRY: _Resp(_dispatcher(LIST_REF)),
        LIST_REF: _Resp(_contract_list([
            ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"}),
        ])),
        **_detail_chain("AV22", "TOKEN-B", _Resp(
            _legacy_page([("01.01.2026", "0.100%", "")], []))),
    }
    session = _Session(answers)
    meta = rates.capture(_client(session), tmp_path, _ACCOUNTS,
                         lambda: ENTRY)
    assert meta["api"]["answered"][0]["states_conditions"] is not followed
    assert ("/assetview/link.do?link=AV22&" in session.requested) is followed
    assert (len(meta["legacy"]["answered"]) == 1) is followed


def test_a_missed_entry_is_recorded_and_the_api_files_stand(tmp_path):
    session = _Session({API_REF: _Resp(json.dumps(_api_payload()))})
    meta = rates.capture(_client(session), tmp_path, _ACCOUNTS, _no_entry)
    assert session.requested == [API_REF]
    assert meta["errors"] == [{"stage": "legacy-entry",
                               "error": "RuntimeError: the menu did not open"}]
    assert [p.name for p in (tmp_path / "rates").iterdir()] == [
        "acct-opaque-1.json"]


_DISPATCH_B = ("/assetview/ebidispatcher.do?uri=getpositiondetail.do"
               "&parameters=TOKEN-B")


@pytest.mark.parametrize("link_answer,framed,error", [
    (_Resp("<html></html>"), None, "not the redirect"),
    (_Resp(status=302, location="https://evil.test" + LIST_REF), None,
     "off site"),
    (_Resp(status=302, location="/assetview/ebidispatcher.do"
           "?uri=getcashtransactionlist.do&parameters=T1"), None,
     "refusing to request"),
    (_Resp(status=302, location=LIST_REF), None, "redirects to"),
    (_Resp(status=302, location=_DISPATCH_B), LIST_REF,
     "not an account page"),
])
def test_an_unexpected_details_link_answer_fetches_nothing(tmp_path,
                                                           link_answer,
                                                           framed, error):
    answers = {
        ENTRY: _Resp(_dispatcher(LIST_REF)),
        LIST_REF: _Resp(_contract_list([
            ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"}),
        ])),
        "/assetview/link.do?link=AV22&": link_answer,
        _DISPATCH_B: _Resp(_dispatcher(framed or "")),
    }
    session = _Session(answers)
    meta = rates.capture(_client(session), tmp_path, [], lambda: ENTRY)
    assert session.requested[:3] == [ENTRY, LIST_REF,
                                     "/assetview/link.do?link=AV22&"]
    assert "/assetview/getpositiondetail.do" not in " ".join(
        session.requested)
    assert [e["stage"] for e in meta["errors"]] == ["legacy"]
    assert error in meta["errors"][0]["error"]
    assert list((tmp_path / "rates").iterdir()) == []


def test_a_page_of_another_account_is_recorded_and_not_kept(tmp_path):
    answers = {
        ENTRY: _Resp(_dispatcher(LIST_REF)),
        LIST_REF: _Resp(_contract_list([
            ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"}),
        ])),
        **_detail_chain("AV22", "TOKEN-B", _Resp(
            _legacy_page([("01.01.2026", "0.100%", "")], [],
                         iban=IBAN_OTHER_PRINTED))),
    }
    meta = rates.capture(_client(_Session(answers)), tmp_path, [],
                         lambda: ENTRY)
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
    answers = {
        ENTRY: _Resp(_dispatcher(LIST_REF)),
        LIST_REF: [_Resp(first), _Resp(second)],
        **_detail_chain("AV22", "TOKEN-B", _Resp(
            _legacy_page([("01.01.2026", "0.100%", "")], []))),
    }
    meta = rates.capture(_client(_Session(answers)), tmp_path, [],
                         lambda: ENTRY)
    # The page already written stays.
    assert [p.name for p in (tmp_path / "rates").iterdir()] == [
        rates.legacy_file_name(IBAN_LEGACY)]
    assert [e["error"] for e in meta["errors"]] == [
        "an account left the list"]


def test_a_failed_rerender_keeps_the_pages_written(tmp_path):
    answers = {
        ENTRY: [_Resp(_dispatcher(LIST_REF)), _Resp("", 500)],
        LIST_REF: _Resp(_contract_list([
            ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"}),
            ("Another", IBAN_OTHER_PRINTED, {"KONTODETAILS": "AV32"}),
        ])),
        **_detail_chain("AV22", "TOKEN-B", _Resp(
            _legacy_page([("01.01.2026", "0.100%", "")], []))),
    }
    meta = rates.capture(_client(_Session(answers)), tmp_path, [],
                         lambda: ENTRY)
    assert [p.name for p in (tmp_path / "rates").iterdir()] == [
        rates.legacy_file_name(IBAN_LEGACY)]
    assert [e["stage"] for e in meta["errors"]] == ["legacy-list"]


def test_only_a_details_link_of_the_render_is_followed():
    client = _client(_NO_REQUEST())
    with pytest.raises(RuntimeError, match="not an account details link"):
        rates._account_page(client, "/assetview/link.do?link=AV13&",
                            LIST_REF, {"/assetview/link.do?link=AV12&"})


def test_a_redirect_without_a_location_is_not_followed():
    session = _Session({"/assetview/link.do?link=AV12&": _Resp(status=302)})
    with pytest.raises(RuntimeError, match="redirects nowhere"):
        _client(session).redirect_target("/assetview/link.do?link=AV12&",
                                         referer=SPA)


def test_an_unnamed_row_or_page_is_recorded_and_not_kept(tmp_path):
    answers = {
        ENTRY: _Resp(_dispatcher(LIST_REF)),
        LIST_REF: _Resp(_contract_list([
            ("IBAN not printed", "", {"KONTODETAILS": "AV12"}),
            ("Cash account", IBAN_LEGACY_PRINTED, {"KONTODETAILS": "AV22"}),
        ])),
        **_detail_chain("AV22", "TOKEN-B", _Resp(
            "<html><body><p>Account information</p></body></html>")),
    }
    session = _Session(answers)
    meta = rates.capture(_client(session), tmp_path, [], lambda: ENTRY)
    assert "/assetview/link.do?link=AV12&" not in session.requested
    assert [e["error"] for e in meta["errors"]] == [
        "the row prints no IBAN", "the page states no IBAN"]
    assert list((tmp_path / "rates").iterdir()) == []


def test_without_an_apikey_the_requests_carry_no_api_headers(tmp_path):
    session = _Session({})
    rates.capture(_client(session, apikey=None), tmp_path, _ACCOUNTS,
                  lambda: ENTRY)
    assert session.headers == [
        {"accept": "application/json", "referer": SPA},
        {"accept": "text/html", "referer": SPA}]


def test_a_redirect_is_not_followed_and_is_recorded(tmp_path):
    # A 3xx names a target the allow-list never saw: the request asks
    # Playwright not to follow it, and the response counts as a failure.
    answers = {API_REF: _Resp(status=302, location="/elsewhere"),
               ENTRY: _Resp(status=302, location="/elsewhere")}
    session = _Session(answers)
    meta = rates.capture(_client(session), tmp_path, _ACCOUNTS,
                         lambda: ENTRY)
    assert session.max_redirects == [0, 0]
    assert session.requested == [API_REF, ENTRY]
    assert [e["stage"] for e in meta["errors"]] == ["api", "legacy-list"]
    assert all("302" in e["error"] for e in meta["errors"])
    assert meta["api"]["answered"] == []
    assert list((tmp_path / "rates").iterdir()) == []


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


def test_the_dispatcher_gives_the_page_it_frames():
    assert rate_parsers.legacy_frame_src(_dispatcher(LIST_REF)) == LIST_REF
    assert rate_parsers.legacy_frame_src(
        '<iframe name="other" src="/x.do"></iframe>') is None


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
