"""
Plaid REST client: the one module of this collector that talks to Plaid.

Every request is a JSON POST. The app keys travel in the PLAID-CLIENT-ID
and PLAID-SECRET headers, so a request body holds parameters only. An
access token is a parameter and does travel in the body; no body is ever
logged or written.

Two lists make the collector read-only by construction:

* ENDPOINTS is every route the client will call. A route outside it is
  refused before any request is built, so adding one is a reviewed edit
  here and in AGENTS.md, never a string at a call site.
* DATA_PRODUCTS is every product a link may request. Plaid grants an
  Item exactly the products its link asked for, and none of these can
  move money.
"""

from __future__ import annotations

import json
import logging
import time
import urllib.error
import urllib.request

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
    "/institutions/get",
    "/institutions/get_by_id",
    "/item/get",
    "/item/public_token/exchange",
    "/item/remove",
    "/link/token/create",
    "/link/token/get",
})

# Routes that exist on the Sandbox host only. They make test Items.
SANDBOX_ENDPOINTS = frozenset({
    "/sandbox/public_token/create",
})

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
        self.display_message = error.get("display_message")
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


def _urllib_transport(url: str, headers: dict, body: bytes, timeout: float):
    """POST `body` and return (status, raw answer). An HTTP error status is
    an answer like any other; only the absence of one raises."""
    request = urllib.request.Request(url, data=body, headers=headers,
                                     method="POST")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except OSError as e:
        # URLError, TimeoutError and the socket and TLS errors.
        raise TransportError(f"{type(e).__name__}: {e}") from e


class Client:
    """The app keys of one environment, and the calls made with them."""

    def __init__(self, client_id: str, secret: str, environment: str, *,
                 transport=_urllib_transport, sleep=time.sleep,
                 timeout: float = 60.0):
        if environment not in HOSTS:
            raise ValueError(f"unknown Plaid environment: {environment!r}")
        self.environment = environment
        self._client_id = client_id
        self._secret = secret
        self._transport = transport
        self._sleep = sleep
        self._timeout = timeout

    def __repr__(self) -> str:
        return f"Client(environment={self.environment!r})"

    def post(self, path: str, payload: dict) -> dict:
        """Call one allowed route and return the decoded answer."""
        if path not in ENDPOINTS and not (
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
        for attempt in range(1, _ATTEMPTS + 1):
            try:
                status, raw = self._transport(url, headers, body,
                                              self._timeout)
                break
            except TransportError as e:
                if attempt == _ATTEMPTS:
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
        it fits. The other data products ride along as optional, so the
        user consents to them and Plaid fetches them where it can, and
        neither their absence nor a failed first fetch stops the Item from
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
