"""
Plaid REST client: the one module of this collector that talks to Plaid.

Every request is a JSON POST. The app keys travel in the PLAID-CLIENT-ID
and PLAID-SECRET headers, so a request body holds parameters only. An
access token is a parameter and does travel in the body; no body is ever
logged or written.

Two kinds of list limit what the collector may ask of Plaid:

* ENDPOINTS, SANDBOX_ENDPOINTS and BILLED_ENDPOINTS are every route the
  client will call. A route outside them is refused before any request
  is built, so adding one is a reviewed edit here and in AGENTS.md, never
  a string at a call site.
* DATA_PRODUCTS is every product a link may request. None of these can
  move money.

A read can still change what an Item is billed for. AGENTS.md lists how.
"""

from __future__ import annotations

import http.client
import json
import logging
import re
import time
import urllib.error
import urllib.request
from datetime import datetime

log = logging.getLogger("plaid.api")

# Sent on every request. A Plaid account carries its own default API
# version, so without the pin two deployments would see different response
# shapes from the same code.
API_VERSION = "2020-09-14"

HOSTS = {
    "production": "https://production.plaid.com",
    "sandbox": "https://sandbox.plaid.com",
}

# The products a link may request: account data, nothing that pays.
DATA_PRODUCTS = ("transactions", "investments", "liabilities")

# Plaid fixes an Item's transaction history depth when the Item is made
# and offers no way to deepen it later, so every link asks for the most.
MAX_DAYS_REQUESTED = 730

# The longest Plaid lets a Hosted Link page live, in seconds.
MAX_LINK_LIFETIME = 21 * 24 * 3600

# What the institution's consent screen calls the application, and the
# opaque handle Plaid files the links under. One deployment is one user.
CLIENT_NAME = "wealthdb"
CLIENT_USER_ID = "wealthdb"

ENDPOINTS = frozenset({
    "/accounts/get",
    "/institutions/get",
    "/institutions/get_by_id",
    "/investments/holdings/get",
    "/investments/transactions/get",
    "/item/get",
    "/item/public_token/exchange",
    "/item/remove",
    "/liabilities/get",
    "/link/token/create",
    "/link/token/get",
    "/transactions/get",
    "/transactions/sync",
})

# Routes that exist on the Sandbox host only. They make test Items.
SANDBOX_ENDPOINTS = frozenset({
    "/sandbox/public_token/create",
})

# Reads that fetch live from the institution, which Plaid bills per
# successful call on a paid plan. Production calls one only when the
# client's `billed_reads` names it. download fills that from plaid.cfg.
# The Sandbox never bills. Each is asked once, never again on its own:
# a second try could be a second bill.
BILLED_ENDPOINTS = frozenset({
    "/investments/refresh",
})

# The most rows Plaid returns for one page of a paged read.
PAGE_SIZE = 500

# How long a request waits for Plaid's answer, in seconds.
TIMEOUT = 60.0

# For the two reads Plaid holds open while it works. A first read of
# investment transactions waits until Plaid has assembled their history:
# one to two minutes, Plaid says. A refresh waits while Plaid fetches from
# the institution: over a minute at some.
SLOW_TIMEOUT = 300.0

_ATTEMPTS = 3
_BACKOFF_SECONDS = 1.5


class TransportError(Exception):
    """Plaid gave no answer: DNS, connect, TLS or timeout."""


class PlaidError(Exception):
    """Plaid answered with an error.

    Carries Plaid's own fields. `str()` is Plaid's wording, unedited, with
    the code and the request id, and never any part of the request.
    """

    def __init__(self, path: str, status: int, error: dict):
        self.path = path
        self.status = status
        self.error_type = error.get("error_type")
        self.error_code = error.get("error_code")
        self.error_message = error.get("error_message")
        self.request_id = error.get("request_id")
        super().__init__(self._describe())

    @property
    def transient(self) -> bool:
        """Whether the same request can succeed later: a fault on Plaid's
        side or a rate limit, as opposed to a refusal of the request."""
        return self.status >= 500 or self.status == 429

    def _describe(self) -> str:
        code = self.error_code or f"HTTP {self.status}"
        text = self.error_message or "no error message in the answer"
        tail = f" (request id {self.request_id})" if self.request_id else ""
        return f"{self.path}: {code}: {text}{tail}"


_INSTANT_RE = re.compile(
    r"^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})(?:\.\d+)?"
    r"(Z|[+-]\d{2}:?\d{2})?$")


def instant(value) -> int | None:
    """An ISO 8601 time Plaid states, in whole Unix seconds. Plaid writes
    up to nine fractional digits, which the standard parser of the oldest
    supported Python refuses, so the fraction is dropped first."""
    if not value:
        return None
    m = _INSTANT_RE.match(value)
    if not m:
        raise ValueError(f"not a time Plaid writes: {value!r}")
    zone = m.group(3) or "Z"
    zone = "+00:00" if zone == "Z" else zone[:3] + ":" + zone[-2:]
    return int(datetime.fromisoformat(f"{m.group(1)}T{m.group(2)}{zone}")
               .timestamp())


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Plaid's API never redirects. Followed, a redirect would send the app
    keys to its target, so it is an answer like any other status."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def _urllib_transport(url: str, headers: dict, body: bytes, timeout: float):
    """POST `body` and return (status, raw answer). An HTTP error status is
    an answer like any other; only the absence of one raises."""
    request = urllib.request.Request(url, data=body, headers=headers,
                                     method="POST")
    try:
        with _OPENER.open(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as e:
        try:
            return e.code, e.read()
        except (OSError, http.client.HTTPException) as cut:
            raise TransportError(f"{type(cut).__name__}: {cut}") from cut
    except (OSError, http.client.HTTPException) as e:
        # URLError, TimeoutError, the socket and TLS errors, and an answer
        # cut off part-way (IncompleteRead) or never begun (BadStatusLine).
        raise TransportError(f"{type(e).__name__}: {e}") from e


class Client:
    """The app keys of one environment, and the calls made with them.

    `on_exchange`, when set, is called once per HTTP attempt with the URL,
    the status (None when no answer came), the time taken in milliseconds,
    the size of the answer and the transport error, if any. It sees
    nothing of the request, so it cannot leak a key or a token.
    """

    def __init__(self, client_id: str, secret: str, environment: str, *,
                 transport=_urllib_transport, sleep=time.sleep):
        if environment not in HOSTS:
            raise ValueError(f"unknown Plaid environment: {environment!r}")
        self.environment = environment
        self.on_exchange = None
        # The billed routes this deployment opted in to, for Production.
        self.billed_reads: frozenset = frozenset()
        self._client_id = client_id
        self._secret = secret
        self._transport = transport
        self._sleep = sleep

    def __repr__(self) -> str:
        return f"Client(environment={self.environment!r})"

    def post(self, path: str, payload: dict, *,
             timeout: float = TIMEOUT) -> dict:
        """Call one allowed route and return the decoded answer."""
        billed = path in BILLED_ENDPOINTS
        if billed:
            if self.environment == "production" and \
                    path not in self.billed_reads:
                raise ValueError(
                    f"{path} is billed per call on a paid plan; production "
                    f"calls it only with the opt-in in plaid.cfg")
        elif path not in ENDPOINTS and not (
                self.environment == "sandbox" and path in SANDBOX_ENDPOINTS):
            raise ValueError(
                f"{path} is not a route this collector calls on "
                f"{self.environment}")
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json",
            "User-Agent": "wealthdb-plaid/1",
            "Plaid-Version": API_VERSION,
            "PLAID-CLIENT-ID": self._client_id,
            "PLAID-SECRET": self._secret,
        }
        body = json.dumps(payload).encode("utf-8")
        url = HOSTS[self.environment] + path
        attempts = 1 if billed else _ATTEMPTS
        for attempt in range(1, attempts + 1):
            started = time.monotonic()
            try:
                status, raw = self._transport(url, headers, body, timeout)
                self._observe(url, started, status=status, size=len(raw))
                break
            except TransportError as e:
                self._observe(url, started, error=str(e))
                if attempt == attempts:
                    raise
                log.warning("%s: no answer (%s); trying again", path, e)
                self._sleep(_BACKOFF_SECONDS * attempt)
        try:
            doc = json.loads(raw)
        except ValueError:
            doc = None
        if not isinstance(doc, dict):
            raise PlaidError(path, status, {
                "error_message": "the answer was not a JSON object"})
        if not 200 <= status < 300:
            raise PlaidError(path, status, doc)
        return doc

    def _observe(self, url: str, started: float, *, status=None, size=None,
                 error=None) -> None:
        if self.on_exchange is not None:
            self.on_exchange(url, status, (time.monotonic() - started) * 1000,
                             size, error)

    # ---- links -----------------------------------------------------------

    def _link_token(self, country_codes, lifetime_seconds: int,
                    extra: dict) -> dict:
        if not 0 < lifetime_seconds <= MAX_LINK_LIFETIME:
            raise ValueError(
                f"a Hosted Link page lives between 1 and "
                f"{MAX_LINK_LIFETIME} seconds, not {lifetime_seconds}")
        return self.post("/link/token/create", {
            "client_name": CLIENT_NAME,
            "language": "en",
            "country_codes": list(country_codes),
            "user": {"client_user_id": CLIENT_USER_ID},
            # Present, even empty, this is what makes Plaid host the page
            # and answer with its URL.
            "hosted_link": {"url_lifetime_seconds": lifetime_seconds},
            **extra,
        })

    def link_token_for_new_item(self, *, required: str, country_codes,
                                lifetime_seconds: int) -> dict:
        """Start a Hosted Link session that makes a new Item.

        `required` is the one product the login must support: Plaid offers
        only institutions that have it, and refuses a login with no account
        it fits. The other data products ride along as optional. The
        consent screen covers them, and Plaid fetches them where it can.
        Neither their absence nor a failed first fetch stops the Item from
        being made.
        """
        if required not in DATA_PRODUCTS:
            raise ValueError(
                f"{required!r} is not a product this collector requests "
                f"(allowed: {', '.join(DATA_PRODUCTS)})")
        return self._link_token(country_codes, lifetime_seconds, {
            "products": [required],
            "optional_products": [p for p in DATA_PRODUCTS if p != required],
            "transactions": {"days_requested": MAX_DAYS_REQUESTED},
        })

    def link_token_for_update(self, *, access_token: str, country_codes,
                              lifetime_seconds: int) -> dict:
        """Start a Hosted Link session on an Item that exists: update mode.

        The Item, its id and its access token stay the same. Account
        selection is on, so an account opened since the link can be added.
        """
        return self._link_token(country_codes, lifetime_seconds, {
            "access_token": access_token,
            "update": {"account_selection_enabled": True},
        })

    def link_token_get(self, link_token: str) -> dict:
        return self.post("/link/token/get", {"link_token": link_token})

    # ---- Items -----------------------------------------------------------

    def exchange_public_token(self, public_token: str) -> dict:
        return self.post("/item/public_token/exchange",
                         {"public_token": public_token})

    def item_get(self, access_token: str) -> dict:
        return self.post("/item/get", {"access_token": access_token})

    def item_remove(self, access_token: str) -> dict:
        """Revoke an Item's access token at Plaid. Never frees a Trial
        slot; the one use is an Item whose token could not be stored."""
        return self.post("/item/remove", {"access_token": access_token})

    # ---- data --------------------------------------------------------------
    #
    # Each read below returns the copy Plaid holds for the Item, which Plaid
    # refreshes on its own schedule. Only `investments_refresh` asks Plaid
    # to refresh it now.

    def accounts_get(self, access_token: str) -> dict:
        """The Item's accounts, with the balances of Plaid's last update."""
        return self.post("/accounts/get", {"access_token": access_token})

    def holdings_get(self, access_token: str) -> dict:
        """Holdings and securities of the Item's investment accounts."""
        return self.post("/investments/holdings/get",
                         {"access_token": access_token})

    def investment_transactions_get(self, access_token: str, *, start, end,
                                    offset: int) -> dict:
        """One page of investment transactions dated `start` to `end`."""
        return self.post("/investments/transactions/get", {
            "access_token": access_token,
            "start_date": start.isoformat(),
            "end_date": end.isoformat(),
            "options": {"count": PAGE_SIZE, "offset": offset},
        }, timeout=SLOW_TIMEOUT)

    def transactions_get(self, access_token: str, *, start, end,
                         offset: int) -> dict:
        """One page of the bank and card ledger dated `start` to `end`.

        Asks for the institution's own description beside Plaid's cleaned
        name, and for version 2 of Plaid's category taxonomy, so every Item
        files its rows in one vocabulary whatever the account's default.
        """
        return self.post("/transactions/get", {
            "access_token": access_token,
            "start_date": start.isoformat(),
            "end_date": end.isoformat(),
            "options": {
                "count": PAGE_SIZE,
                "offset": offset,
                "include_original_description": True,
                "personal_finance_category_version": "v2",
            },
        })

    def liabilities_get(self, access_token: str) -> dict:
        """Card, mortgage and student-loan terms of the Item's accounts."""
        return self.post("/liabilities/get", {"access_token": access_token})

    def investments_refresh(self, access_token: str) -> dict:
        """Ask Plaid to fetch the Item's holdings and investment
        transactions from the institution now. Plaid holds the request
        open while it fetches: a few seconds in the Sandbox, over a minute
        at some institutions. The answer carries only a request id. A
        billed read: see BILLED_ENDPOINTS."""
        return self.post("/investments/refresh",
                         {"access_token": access_token},
                         timeout=SLOW_TIMEOUT)

    def ledger_history_status(self, access_token: str) -> str | None:
        """How far Plaid has got assembling the Item's bank and card
        ledger: NOT_READY, INITIAL_UPDATE_COMPLETE (the newest 30 days),
        HISTORICAL_UPDATE_COMPLETE, or TRANSACTIONS_UPDATE_STATUS_UNKNOWN.
        Plaid states it only on its sync route. This asks that route for
        one row, with no cursor, and keeps only the status."""
        doc = self.post("/transactions/sync", {
            "access_token": access_token,
            "count": 1,
            "options": {"personal_finance_category_version": "v2"},
        })
        return doc.get("transactions_update_status")

    # ---- institutions ----------------------------------------------------

    def institutions_first(self, country_codes) -> dict:
        """The smallest read the app keys alone can make: one institution
        off the top of Plaid's list. Free, and it touches no Item."""
        return self.post("/institutions/get", {
            "count": 1, "offset": 0, "country_codes": list(country_codes)})

    def institution_get_by_id(self, institution_id: str,
                              country_codes) -> dict:
        return self.post("/institutions/get_by_id", {
            "institution_id": institution_id,
            "country_codes": list(country_codes)})

    # ---- Sandbox ---------------------------------------------------------

    def sandbox_public_token_create(self, institution_id: str,
                                    products) -> dict:
        """Make a test Item with no browser step. Sandbox host only."""
        products = list(products)
        unknown = [p for p in products if p not in DATA_PRODUCTS]
        if unknown or not products:
            raise ValueError(
                f"a test Item takes one or more of "
                f"{', '.join(DATA_PRODUCTS)}, not {products}")
        payload = {"institution_id": institution_id,
                   "initial_products": products}
        if "transactions" in products:
            payload["options"] = {
                "transactions": {"days_requested": MAX_DAYS_REQUESTED}}
        return self.post("/sandbox/public_token/create", payload)
