#!/usr/bin/env python3
"""
Interest-condition capture for ubs-web bronze.

Two surfaces state a cash account's credit and debit interest
(DESIGN.md §3.11):

* **The modern account API**,
  ``GET /api/v3/cash-accounts/<accountId>?include=INTEREST_RATES``, one
  JSON response per account, with its ``interestItems``. The
  ``accountId`` is the opaque token the homepage anchors carry, the one
  `download.enumerate_accounts` already extracts. Without the
  ``include`` query the response leaves the conditions out.
* **The legacy asset view**, entered the way the SPA enters it. The
  SPA's "Account balances" view frames the entry
  ``ebidispatcher.do?navitemid=AssetsPg&...&assetId=<token>``; the
  dispatcher page frames the contract list
  ``getvaluatedcontractlist.do?parameters=<token>``. Each account row's
  "Account details" item opens ``link.do?link=<id>&``, which the server
  answers with a redirect to
  ``ebidispatcher.do?uri=getpositiondetail.do&parameters=<token>``; that
  page frames the account's "Account information" page
  ``getpositiondetail.do?parameters=<token>``, with a "Credit interest"
  and a "Debit interest" table. It reaches the accounts the homepage
  does not tile. Every token is taken from the page, the frame or the
  redirect that gives it, never built.

The modern pass runs first. The legacy pass then fetches only the
accounts the modern pass did not cover, matched by IBAN. An API
response covers its account only when it states ``interestItems``.

Read-only by construction, as `cards.py` is: :func:`refuse_request`
admits GET on the routes below, each with exactly its query, and every
request passes it, a redirect's target included. A redirect is never
followed by the request client: `link.do`'s is read, checked and then
requested. AGENTS.md §1 states the surface; this is where it is held.
"""

from __future__ import annotations

import hashlib
import logging
import re
from collections.abc import Callable
from pathlib import Path
from urllib.parse import urljoin, urlsplit

from collectorkit import bronze, debugcap

import landmarks as ubs  # local module
import rate_parsers  # local module

log = logging.getLogger("ubs-web.rates")

# The account token as the homepage gives it. A token outside this set
# is not requested: it could not be put in a path verbatim. It holds a
# letter or a digit, so a token of dots alone (which a URL resolves to
# another path) never reads as one.
_TOKEN = r"(?=[A-Za-z0-9._~=-]*[A-Za-z0-9])[A-Za-z0-9._~=-]+"
_TOKEN_RE = re.compile(rf"^{_TOKEN}$")
# A query value as a page gives it: percent-encoded, never decoded here.
_QUERY_TOKEN_RE = re.compile(r"^[A-Za-z0-9%._~+=-]+$")
_LINK_ID_RE = re.compile(r"^AV\d+$")
# A path segment of dots alone. The anchored allow-list already admits
# none; this refusal is a backstop that names the reason.
_DOT_SEGMENT_RE = re.compile(r"^\.+$")
_API_PATH_RE = re.compile(
    rf"^{re.escape(ubs.RATES_API_ACCOUNT_PREFIX)}{_TOKEN}$")

# Each legacy route, with the one query shape it is requested with: the
# query's names, each with its value or a check on it.
_TOKENED = {"parameters": _QUERY_TOKEN_RE}
_LEGACY_QUERIES = {
    ubs.LEGACY_CONTRACT_LIST_PATH: [_TOKENED],
    ubs.LEGACY_POSITION_DETAIL_PATH: [_TOKENED],
    ubs.LEGACY_LINK_PATH: [{"link": _LINK_ID_RE}],
    ubs.LEGACY_DISPATCHER_PATH: [
        {**ubs.LEGACY_ENTRY_QUERY,
         ubs.LEGACY_ENTRY_TOKEN_NAME: _QUERY_TOKEN_RE},
        {"uri": ubs.LEGACY_POSITION_DETAIL_PATH.rsplit("/", 1)[-1],
         **_TOKENED},
    ],
}

REQUEST_TIMEOUT_MS = 60_000


def _query_fields(query: str) -> dict[str, str] | None:
    """The query's fields by name, undecoded, or None when a name
    repeats or a field has no `=`. An empty field (a trailing `&`, as
    the pages give `link.do`) is no field."""
    out: dict[str, str] = {}
    for field in query.split("&"):
        if not field:
            continue
        name, eq, value = field.partition("=")
        if not eq or name in out:
            return None
        out[name] = value
    return out


def _query_matches(fields: dict[str, str], shape: dict) -> bool:
    if fields.keys() != shape.keys():
        return False
    return all(want.match(fields[k]) if isinstance(want, re.Pattern)
               else fields[k] == want for k, want in shape.items())


def refuse_request(method: str, path_and_query: str) -> str | None:
    """Why this request may not be made, or None when it may.

    An allow-list of GETs: the account API with its include query, and
    the legacy entry, contract list, `link.do`, dispatcher and account
    page, each with exactly the query the pages give it. A path with a
    segment of dots alone is refused: URL resolution would turn it into
    another path.
    """
    if method.upper() != "GET":
        return f"{method} is not a read"
    split = urlsplit(path_and_query)
    if split.scheme or split.netloc or not split.path.startswith("/"):
        return f"not a site-relative reference: {path_and_query!r}"
    if split.fragment:
        return f"a fragment in {path_and_query!r}"
    if any(_DOT_SEGMENT_RE.match(seg) for seg in split.path.split("/")):
        return f"a dot segment in {split.path!r}"
    if _API_PATH_RE.match(split.path):
        if split.query != ubs.RATES_API_INCLUDE_QUERY:
            return (f"the account API takes "
                    f"{ubs.RATES_API_INCLUDE_QUERY!r} and nothing else")
        return None
    shapes = _LEGACY_QUERIES.get(split.path)
    if shapes is None:
        return f"{split.path!r} is not a rate read endpoint"
    fields = _query_fields(split.query)
    if fields is None or not any(_query_matches(fields, s) for s in shapes):
        return f"{split.path!r} is not requested with this query"
    return None


class RatesClient:
    """Read-only client for the rate surfaces, on the browser session.

    Requests go through the Playwright context, so the session cookie
    rides along. Each carries the referer the browser sends: the SPA for
    the account API and the legacy entry, else the page that gave the
    link. Only the account API takes the SPA's `apikey` header, the one
    the card pass uses (`cards.pick_apikey`). A redirect is not
    followed: its target was never checked against the allow-list, so a
    3xx is an error, except where :meth:`redirect_target` asks for one.
    """

    def __init__(self, context, origin: str, apikey: str | None,
                 spa_url: str) -> None:
        self._context = context
        self._origin = origin.rstrip("/")
        self._apikey = apikey
        self.spa_url = spa_url.split("#", 1)[0]

    def url(self, path_and_query: str) -> str:
        return self._origin + path_and_query

    def _get(self, path_and_query: str, *, accept: str, referer: str,
             api: bool = False, redirect: bool = False):
        refusal = refuse_request("GET", path_and_query)
        if refusal is not None:
            raise RuntimeError(f"refusing to request: {refusal}")
        headers = {"accept": accept, "referer": referer}
        if api and self._apikey:
            headers["apikey"] = self._apikey
            headers["x-requested-with"] = "XMLHttpRequest"
        resp = self._context.request.get(self.url(path_and_query),
                                         headers=headers,
                                         max_redirects=0,
                                         timeout=REQUEST_TIMEOUT_MS)
        path = urlsplit(path_and_query).path
        if 300 <= resp.status < 400 and not redirect:
            raise RuntimeError(
                f"refusing to follow HTTP {resp.status} redirect for {path}")
        if redirect and not 300 <= resp.status < 400:
            raise RuntimeError(f"HTTP {resp.status} for {path}, "
                               "not the redirect its link gives")
        if not redirect and not resp.ok:
            raise RuntimeError(f"HTTP {resp.status} for {path}")
        return resp

    def account(self, account_id: str) -> dict:
        """One cash account's JSON, interest conditions included."""
        if not _TOKEN_RE.match(account_id or ""):
            raise RuntimeError("account token is not in the admitted set")
        return self._get(
            f"{ubs.RATES_API_ACCOUNT_PREFIX}{account_id}"
            f"?{ubs.RATES_API_INCLUDE_QUERY}",
            accept="application/json", referer=self.spa_url,
            api=True).json()

    def page(self, ref: str, referer: str) -> str:
        """One legacy page, as HTML, by the reference a page gave."""
        return self._get(ref, accept="text/html", referer=referer).text()

    def redirect_target(self, ref: str, referer: str) -> str:
        """The site-relative reference `ref` redirects to, unrequested.

        It goes through the allow-list when it is requested; an off-site
        target is refused here.
        """
        resp = self._get(ref, accept="text/html", referer=referer,
                         redirect=True)
        location = resp.headers.get("location")
        if not location:
            raise RuntimeError(f"{urlsplit(ref).path} redirects nowhere")
        target = urlsplit(urljoin(self.url(ref), location))
        if f"{target.scheme}://{target.netloc}" != self._origin:
            raise RuntimeError(f"{urlsplit(ref).path} redirects off site")
        return target.path + (f"?{target.query}" if target.query else "")


def legacy_file_name(iban: str) -> str:
    """The bronze name of a legacy page: its account's IBAN, hashed."""
    return f"legacy-{hashlib.sha256(iban.encode('ascii')).hexdigest()}.html"


def capture(client: RatesClient, run_dir: Path, accounts: list[dict],
            legacy_entry: Callable[[], str]) -> dict:
    """Capture both surfaces into ``<run_dir>/rates/``.

    Returns the manifest block for ``run.json``. Each account stands
    alone: one that fails is recorded as an error and the rest still
    land. ``legacy_entry`` gives the legacy entry as the SPA frames it;
    when it fails, the miss is recorded and the API files stand. The
    legacy pass runs even when the modern one covered nobody.
    """
    rates_dir = run_dir / "rates"
    rates_dir.mkdir(parents=True, exist_ok=True)
    result: dict = {"api": {"attempted": [], "answered": []},
                    "legacy": {"listed": 0, "attempted": [], "answered": [],
                               "skipped_covered": []},
                    "errors": []}

    covered: set[str] = set()
    for account in accounts:
        account_id = account["account_id"]
        short = bronze.short_token(account_id)
        result["api"]["attempted"].append(short)
        try:
            payload = client.account(account_id)
        except Exception as e:  # noqa: BLE001 — one account, not the pass
            log.warning("rates for account %s… failed: %s",
                        short[:8], debugcap.safe_error(e))
            result["errors"].append({"account_short_id": short,
                                     "stage": "api",
                                     "error": debugcap.safe_error(e)})
            continue
        name = f"{account_id}.json"
        bronze.atomic_write_json(rates_dir / name, payload)
        states = rate_parsers.api_states_conditions(payload)
        result["api"]["answered"].append({"account_short_id": short,
                                          "file": name,
                                          "states_conditions": states})
        if states and (iban := rate_parsers.api_iban(payload)):
            covered.add(iban)

    try:
        entry = legacy_entry()
    except Exception as e:  # noqa: BLE001 — the modern files stay
        log.warning("legacy rate entry not found: %s",
                    debugcap.safe_error(e))
        result["errors"].append({"stage": "legacy-entry",
                                 "error": debugcap.safe_error(e)})
    else:
        try:
            _capture_legacy(client, rates_dir, covered, result, entry)
        except Exception as e:  # noqa: BLE001 — the modern files stay
            log.warning("legacy rate pass failed: %s",
                        debugcap.safe_error(e))
            result["errors"].append({"stage": "legacy-list",
                                     "error": debugcap.safe_error(e)})

    log.info("rates: api %d of %d answered, %d stating conditions; "
             "legacy %d of %d answered, %d covered by the api; %d error(s)",
             len(result["api"]["answered"]), len(result["api"]["attempted"]),
             sum(a["states_conditions"] for a in result["api"]["answered"]),
             len(result["legacy"]["answered"]),
             len(result["legacy"]["attempted"]),
             len(result["legacy"]["skipped_covered"]),
             len(result["errors"]))
    return result


def _contract_list(client: RatesClient, entry: str) -> tuple[str, str]:
    """The contract list the entry frames: its reference and its HTML."""
    dispatcher = client.page(entry, referer=client.spa_url)
    ref = rate_parsers.legacy_frame_src(dispatcher)
    if not ref:
        raise RuntimeError("the entry frames no page")
    ref = urljoin(entry, ref)
    return ref, client.page(ref, referer=client.url(entry))


def _account_page(client: RatesClient, link: str, list_ref: str,
                  details: set[str]) -> str:
    """One account's page, by its "Account details" link: the link's
    redirect, then the page that redirect's dispatcher frames. The link
    must be one of the render's ``details`` links: the allow-list admits
    any `link.do` id, the row's payment and transfer ids among them."""
    if link not in details:
        raise RuntimeError("not an account details link of this render")
    dispatcher_ref = client.redirect_target(link, referer=client.url(list_ref))
    if urlsplit(dispatcher_ref).path != ubs.LEGACY_DISPATCHER_PATH:
        raise RuntimeError(f"the details link redirects to "
                           f"{urlsplit(dispatcher_ref).path}")
    dispatcher = client.page(dispatcher_ref, referer=client.url(list_ref))
    ref = rate_parsers.legacy_frame_src(dispatcher)
    if not ref:
        raise RuntimeError("the dispatcher frames no page")
    ref = urljoin(dispatcher_ref, ref)
    if urlsplit(ref).path != ubs.LEGACY_POSITION_DETAIL_PATH:
        raise RuntimeError(f"the dispatcher frames {urlsplit(ref).path}, "
                           "not an account page")
    return client.page(ref, referer=client.url(dispatcher_ref))


def _capture_legacy(client: RatesClient, rates_dir: Path, covered: set[str],
                    result: dict, entry: str) -> None:
    """Fetch the page of each listed account the modern pass did not cover.

    The list's links are ids the server resolves for the render that
    printed them. Each account is followed from a fresh render, so the
    list is read again before every account after the first, and the
    account found again by its IBAN. A row that prints no IBAN cannot be
    found again, and is recorded rather than followed. A page is kept
    only when its own IBAN is its row's.
    """
    list_ref, html = _contract_list(client, entry)
    rows = rate_parsers.legacy_account_rows(html)
    legacy = result["legacy"]
    legacy["listed"] = len(rows)
    pending: list[str] = []
    for row in rows:
        token = bronze.short_token(row["ref"])
        if row["iban"] is None:
            result["errors"].append({"link_short_id": token,
                                     "stage": "legacy",
                                     "error": "the row prints no IBAN"})
        elif row["iban"] in covered:
            legacy["skipped_covered"].append(token)
        elif row["iban"] not in pending:
            pending.append(row["iban"])
    fresh = True
    for iban in pending:
        if iban in covered:
            continue
        if not fresh:
            list_ref, html = _contract_list(client, entry)
        fresh = False
        rows = rate_parsers.legacy_account_rows(html)
        row = next((r for r in rows if r["iban"] == iban), None)
        if row is None:
            result["errors"].append({"stage": "legacy",
                                     "error": "an account left the list"})
            continue
        token = bronze.short_token(row["ref"])
        legacy["attempted"].append(token)
        try:
            page = _account_page(client, row["ref"], list_ref,
                                 {r["ref"] for r in rows})
        except Exception as e:  # noqa: BLE001 — one account, not the pass
            log.warning("legacy rates for %s… failed: %s",
                        token[:8], debugcap.safe_error(e))
            result["errors"].append({"link_short_id": token,
                                     "stage": "legacy",
                                     "error": debugcap.safe_error(e)})
            continue
        page_iban = rate_parsers.legacy_iban(page)
        if page_iban is None:
            result["errors"].append({"link_short_id": token,
                                     "stage": "legacy",
                                     "error": "the page states no IBAN"})
            continue
        if page_iban != iban:
            result["errors"].append({"link_short_id": token,
                                     "stage": "legacy",
                                     "error": "the page names another "
                                              "account than its row"})
            continue
        covered.add(page_iban)
        name = legacy_file_name(page_iban)
        bronze.atomic_write_bytes(rates_dir / name, page.encode("utf-8"))
        legacy["answered"].append({"link_short_id": token, "file": name})
