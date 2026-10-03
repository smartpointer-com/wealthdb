"""Tests for plaidapi, the client: what it will ask Plaid for, what it
refuses to ask, and how it reads an answer. The transport is a recorder;
nothing reaches the network."""
from __future__ import annotations

import http.client
import io
import json
import re
import urllib.error
import urllib.request
from pathlib import Path

import pytest

import plaidapi

CLIENT_ID = "synthetic-client-id"
SECRET = "synthetic-secret-value"
ACCESS = "access-sandbox-00000000-0000-4000-8000-000000000001"


class Recorder:
    """A transport that answers from a script and keeps every request."""

    def __init__(self, *answers):
        self.answers = list(answers) or [(200, b"{}")]
        self.requests = []

    def __call__(self, url, headers, body, timeout):
        self.requests.append({"url": url, "headers": headers,
                              "body": json.loads(body), "timeout": timeout})
        answer = (self.answers.pop(0) if len(self.answers) > 1
                  else self.answers[0])
        if isinstance(answer, Exception):
            raise answer
        return answer


def client(recorder, environment="sandbox", sleeps=None):
    return plaidapi.Client(
        CLIENT_ID, SECRET, environment, transport=recorder,
        sleep=(sleeps.append if sleeps is not None else lambda s: None))


def ok(doc):
    return 200, json.dumps(doc).encode()


# ---- what may be asked -------------------------------------------------------

@pytest.mark.parametrize("path", [
    "/transfer/create",
    "/transfer/authorization/create",
    "/payment_initiation/payment/create",
    "/processor/token/create",
    "/item/access_token/invalidate",
    "/item/webhook/update",
    "/accounts/balance/get",
    "/transactions/refresh",
])
def test_a_route_outside_the_list_is_refused_before_any_request(path):
    recorder = Recorder()
    with pytest.raises(ValueError, match="not a route"):
        client(recorder).post(path, {})
    assert recorder.requests == []


def test_no_listed_route_moves_money_or_makes_a_payment():
    # The list is the collector's whole surface at Plaid. A route that
    # pays, transfers or hands access to another party has no place in
    # it, under any name.
    for path in (plaidapi.ENDPOINTS | plaidapi.SANDBOX_ENDPOINTS
                 | plaidapi.BILLED_ENDPOINTS):
        for word in ("transfer", "payment", "processor", "signal", "oauth/"):
            assert word not in path, path


def test_the_routes_are_exactly_the_ones_agents_md_lists():
    # A new route is an edit here and in AGENTS.md, never one alone.
    agents = (Path(plaidapi.__file__).parent / "AGENTS.md").read_text()
    section = agents.split("**Routes**", 1)[1].split("**Products**", 1)[0]
    listed = set(re.findall(r"`(/[a-z_/]+)`", section))
    assert (plaidapi.ENDPOINTS | plaidapi.SANDBOX_ENDPOINTS
            | plaidapi.BILLED_ENDPOINTS) == listed


# ---- the reads Plaid bills per call -------------------------------------------

REFRESH = "/investments/refresh"
PRODUCTION_ACCESS = "access-production-00000000-0000-4000-8000-000000000001"


def test_a_billed_read_is_refused_on_production_without_the_opt_in():
    recorder = Recorder()
    with pytest.raises(ValueError, match="opt-in in plaid.cfg"):
        client(recorder, "production").investments_refresh(PRODUCTION_ACCESS)
    assert recorder.requests == []


def test_a_billed_read_is_asked_on_production_with_the_opt_in():
    recorder = Recorder(ok({"request_id": "req-synthetic"}))
    production = client(recorder, "production")
    production.billed_reads = frozenset({REFRESH})
    assert production.investments_refresh(PRODUCTION_ACCESS) == {
        "request_id": "req-synthetic"}
    [request] = recorder.requests
    assert request["url"] == plaidapi.HOSTS["production"] + REFRESH
    assert request["body"] == {"access_token": PRODUCTION_ACCESS}
    # Plaid holds the request open while it fetches from the institution.
    assert request["timeout"] == plaidapi.SLOW_TIMEOUT


def test_the_sandbox_never_bills_and_needs_no_opt_in():
    recorder = Recorder()
    client(recorder).investments_refresh(ACCESS)
    assert [r["url"] for r in recorder.requests] == [
        plaidapi.HOSTS["sandbox"] + REFRESH]


@pytest.mark.parametrize("environment", ["sandbox", "production"])
@pytest.mark.parametrize("failure", [
    plaidapi.TransportError("TimeoutError: synthetic"),
    plaidapi.TransportError("IncompleteRead: synthetic")])
def test_a_billed_read_is_asked_once_whatever_happens(failure, environment):
    # Plaid may have fetched, and billed, before the answer was lost.
    sleeps = []
    recorder = Recorder(failure)
    billed = client(recorder, environment, sleeps=sleeps)
    billed.billed_reads = frozenset({REFRESH})
    with pytest.raises(plaidapi.TransportError):
        billed.investments_refresh(ACCESS)
    assert len(recorder.requests) == 1
    assert sleeps == []


def test_the_sandbox_route_is_refused_on_production():
    recorder = Recorder()
    with pytest.raises(ValueError, match="production"):
        client(recorder, "production").sandbox_public_token_create(
            "ins_000", ["transactions"])
    assert recorder.requests == []


def test_an_unknown_environment_is_refused():
    with pytest.raises(ValueError):
        plaidapi.Client(CLIENT_ID, SECRET, "development")


# ---- the link request ----------------------------------------------------------

def link_body(**kwargs):
    recorder = Recorder(ok({"link_token": "t", "hosted_link_url": "u"}))
    kwargs.setdefault("country_codes", ["US"])
    kwargs.setdefault("lifetime_seconds", 3600)
    client(recorder).link_token_for_new_item(**kwargs)
    (request,) = recorder.requests
    assert request["url"] == "https://sandbox.plaid.com/link/token/create"
    return request["body"]


def test_a_new_link_requires_one_product_and_offers_the_rest():
    body = link_body(required="investments")
    assert body["products"] == ["investments"]
    assert sorted(body["optional_products"]) == ["liabilities",
                                                 "transactions"]
    # Plaid refuses a product named in two of its lists.
    assert not set(body["products"]) & set(body["optional_products"])


def test_a_new_link_asks_for_data_products_only():
    for required in plaidapi.DATA_PRODUCTS:
        body = link_body(required=required)
        asked = set(body["products"]) | set(body["optional_products"])
        assert asked == {"transactions", "investments", "liabilities"}
        for key in ("required_if_supported_products",
                    "additional_consented_products", "payment_initiation",
                    "transfer"):
            assert key not in body


@pytest.mark.parametrize("product", [
    "transfer", "payment_initiation", "auth", "identity", "signal",
    "assets", "statements", "income_verification", "", "Transactions",
])
def test_a_product_outside_the_data_set_is_refused(product):
    recorder = Recorder()
    with pytest.raises(ValueError, match="not a product"):
        client(recorder).link_token_for_new_item(
            required=product, country_codes=["US"], lifetime_seconds=3600)
    assert recorder.requests == []


def test_a_new_link_asks_for_the_deepest_history_and_a_hosted_page():
    body = link_body(required="transactions", lifetime_seconds=7200,
                     country_codes=["US", "CA"])
    # The depth is fixed for the Item's life, so it is always the maximum.
    assert body["transactions"] == {"days_requested": 730}
    assert body["hosted_link"] == {"url_lifetime_seconds": 7200}
    assert body["country_codes"] == ["US", "CA"]
    assert body["client_name"] == "wealthdb"
    assert body["user"] == {"client_user_id": "wealthdb"}
    # Hosted Link on a desktop needs neither, and a webhook would need a
    # server this collector does not run.
    assert "redirect_uri" not in body and "webhook" not in body


@pytest.mark.parametrize("seconds", [0, -5, 21 * 24 * 3600 + 1])
def test_a_page_lifetime_plaid_would_refuse_is_caught_first(seconds):
    recorder = Recorder()
    with pytest.raises(ValueError, match="lives between"):
        client(recorder).link_token_for_new_item(
            required="transactions", country_codes=["US"],
            lifetime_seconds=seconds)
    assert recorder.requests == []


def test_an_update_link_names_the_item_and_no_product():
    recorder = Recorder(ok({"link_token": "t", "hosted_link_url": "u"}))
    client(recorder).link_token_for_update(
        access_token=ACCESS, country_codes=["US"], lifetime_seconds=3600)
    body = recorder.requests[0]["body"]
    assert body["access_token"] == ACCESS
    assert body["update"] == {"account_selection_enabled": True}
    # Update mode inherits the Item's products; naming one is an error
    # at Plaid.
    for key in ("products", "optional_products", "transactions"):
        assert key not in body
    assert body["hosted_link"] == {"url_lifetime_seconds": 3600}


def test_a_sandbox_item_takes_data_products_only():
    recorder = Recorder(ok({"public_token": "p"}))
    client(recorder).sandbox_public_token_create(
        "ins_000", ["investments", "transactions"])
    body = recorder.requests[0]["body"]
    assert body["initial_products"] == ["investments", "transactions"]
    assert body["options"] == {"transactions": {"days_requested": 730}}
    for bad in (["transfer"], [], ["transactions", "auth"]):
        with pytest.raises(ValueError):
            client(Recorder()).sandbox_public_token_create("ins_000", bad)


def test_a_sandbox_item_without_transactions_sends_no_options():
    recorder = Recorder(ok({"public_token": "p"}))
    client(recorder).sandbox_public_token_create("ins_000", ["investments"])
    assert "options" not in recorder.requests[0]["body"]


# ---- how a request is sent -------------------------------------------------

def test_the_app_keys_travel_in_headers_and_never_in_the_body():
    recorder = Recorder(ok({"item": {}}))
    client(recorder).item_get(ACCESS)
    (request,) = recorder.requests
    assert request["headers"]["PLAID-CLIENT-ID"] == CLIENT_ID
    assert request["headers"]["PLAID-SECRET"] == SECRET
    assert request["headers"]["Plaid-Version"] == plaidapi.API_VERSION
    assert request["body"] == {"access_token": ACCESS}
    assert SECRET not in json.dumps(request["body"])


def test_each_environment_has_its_own_host():
    for environment, host in (("sandbox", "https://sandbox.plaid.com"),
                              ("production", "https://production.plaid.com")):
        recorder = Recorder(ok({}))
        client(recorder, environment).institutions_first(["US"])
        assert recorder.requests[0]["url"] == host + "/institutions/get"
        assert recorder.requests[0]["body"] == {
            "count": 1, "offset": 0, "country_codes": ["US"]}


def test_the_client_does_not_print_its_secret():
    c = client(Recorder())
    assert SECRET not in repr(c) and SECRET not in str(c)
    assert CLIENT_ID not in repr(c)


# ---- how an answer is read -------------------------------------------------

def test_an_error_answer_carries_plaids_own_fields():
    recorder = Recorder((400, json.dumps({
        "error_type": "ITEM_ERROR", "error_code": "ITEM_LOGIN_REQUIRED",
        "error_message": "the login details of this item have changed",
        "display_message": None, "request_id": "req-1"}).encode()))
    with pytest.raises(plaidapi.PlaidError) as caught:
        client(recorder).item_get(ACCESS)
    e = caught.value
    assert (e.status, e.error_type, e.error_code, e.request_id) == (
        400, "ITEM_ERROR", "ITEM_LOGIN_REQUIRED", "req-1")
    text = str(e)
    # Plaid's wording, unedited, and nothing of the request.
    assert "the login details of this item have changed" in text
    assert "ITEM_LOGIN_REQUIRED" in text and "req-1" in text
    assert ACCESS not in text and SECRET not in text


@pytest.mark.parametrize("status,raw", [
    (502, b"<html>bad gateway</html>"),
    (200, b"not json"),
    (200, b"[1, 2]"),
])
def test_an_answer_that_is_not_a_json_object_is_an_error(status, raw):
    with pytest.raises(plaidapi.PlaidError) as caught:
        client(Recorder((status, raw))).item_get(ACCESS)
    assert caught.value.status == status
    assert "not a JSON object" in str(caught.value)


@pytest.mark.parametrize("status,transient", [
    (400, False), (401, False), (404, False), (429, True), (500, True),
    (503, True)])
def test_an_error_says_whether_asking_again_can_help(status, transient):
    assert plaidapi.PlaidError("/item/get", status, {}).transient is transient


def test_an_error_status_is_not_retried():
    recorder = Recorder((500, json.dumps({"error_code": "INTERNAL_SERVER_ERROR",
                                          "error_message": "x"}).encode()))
    with pytest.raises(plaidapi.PlaidError):
        client(recorder).item_get(ACCESS)
    assert len(recorder.requests) == 1


def test_no_answer_is_retried_and_then_given_up_on():
    sleeps = []
    recorder = Recorder(plaidapi.TransportError("TimeoutError: timed out"))
    with pytest.raises(plaidapi.TransportError):
        client(recorder, sleeps=sleeps).item_get(ACCESS)
    assert len(recorder.requests) == 3
    assert len(sleeps) == 2 and sleeps[0] < sleeps[1]


class _Answer:
    """What urlopen hands back: a context manager with a status."""

    def __init__(self, status, read):
        self.status = status
        self.read = read

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _cut(*args):
    raise http.client.IncompleteRead(b"{\"par", 100)


def test_the_transport_returns_an_error_answer_like_any_other(monkeypatch):
    def urlopen(request, timeout):
        raise urllib.error.HTTPError(request.full_url, 400, "Bad Request",
                                     {}, io.BytesIO(b'{"error_code": "X"}'))

    monkeypatch.setattr(urllib.request, "urlopen", urlopen)
    assert plaidapi._urllib_transport(
        "https://plaid.invalid/item/get", {}, b"{}", 1.0) == (
        400, b'{"error_code": "X"}')


@pytest.mark.parametrize("urlopen", [
    lambda request, timeout: _Answer(200, _cut),
    lambda request, timeout: (_ for _ in ()).throw(urllib.error.HTTPError(
        request.full_url, 500, "Server Error", {},
        type("Cut", (io.BytesIO,), {"read": _cut})())),
    lambda request, timeout: (_ for _ in ()).throw(
        http.client.BadStatusLine("")),
], ids=["answer-cut-off", "error-answer-cut-off", "no-status-line"])
def test_an_answer_that_breaks_off_is_no_answer(monkeypatch, urlopen):
    # The client asks again after no answer; any other exception would
    # end the whole run.
    monkeypatch.setattr(urllib.request, "urlopen", urlopen)
    with pytest.raises(plaidapi.TransportError):
        plaidapi._urllib_transport(
            "https://plaid.invalid/item/get", {}, b"{}", 1.0)


def test_one_lost_answer_does_not_fail_the_call():
    recorder = Recorder(plaidapi.TransportError("URLError: reset"),
                        ok({"item": {"item_id": "i"}}))
    assert client(recorder).item_get(ACCESS) == {"item": {"item_id": "i"}}
    assert len(recorder.requests) == 2


# ---- Plaid's times -----------------------------------------------------------

JAN_2 = 1_767_312_000            # 2026-01-02T00:00:00Z


@pytest.mark.parametrize("value,seconds", [
    ("2026-01-02T00:00:00Z", JAN_2),
    ("2026-01-02T00:00:00.123456789Z", JAN_2),
    ("2026-01-02T01:00:00+01:00", JAN_2),
    ("2026-01-02T00:00:00", JAN_2),
    (None, None), ("", None)])
def test_a_time_is_read_with_any_fraction_and_zone(value, seconds):
    assert plaidapi.instant(value) == seconds


def test_a_time_plaid_does_not_write_is_an_error():
    with pytest.raises(ValueError):
        plaidapi.instant("02/01/2026")
