#!/usr/bin/env python3
"""
Interest-condition capture for ubs-web bronze.

Two surfaces state a cash account's credit and debit interest
(DESIGN.md §3.11). The pass reads both from what the browser renders.
It never requests a page of its own: the netbanking answers its own
SPA's calls, and the legacy asset view binds its tokens to the render
the browser holds.

* **The modern account API.** Each homepage account's "Account
  information" route makes the SPA call
  ``GET /api/v3/cash-accounts/<accountId>?include=INTEREST_RATES``. The
  pass navigates to that route and keeps the SPA's own response, one
  JSON per account, with its ``interestItems``.
* **The legacy asset view.** The SPA's "Account balances" view frames
  the entry ``ebidispatcher.do?navitemid=AssetsPg&...&assetId=<token>``,
  and the entry frames the contract list
  ``getvaluatedcontractlist.do?parameters=<token>``. Each account row's
  "Account details" item opens ``link.do?link=<id>&`` in the list's
  parent frame. The browser follows its redirect to
  ``ebidispatcher.do?uri=getpositiondetail.do&parameters=<token>``,
  which frames the account's "Account information" page
  ``getpositiondetail.do?parameters=<token>``. The pass opens the link
  in that same frame and reads the page the browser renders. It reaches
  the accounts the homepage does not tile.

The modern pass runs first. The legacy pass then reads only the
accounts the modern pass did not cover, matched by IBAN. An API
response covers its account only when it states ``interestItems``.

Read-only by construction. While the pass runs, a route guard on the
page aborts every request under `/assetview/` or
`/api/v3/cash-accounts` that is not a GET, and every GET to a legacy
page or to the account API that :func:`refuse_request` refuses: the
SPA's and the legacy app's own requests too. Each abort is an error
entry. Every reference the pass itself opens goes through
:func:`refuse_request` first. A route handler sees only a redirect's
first URL, so the page a redirect lands on is checked against the same
allow-list once it has loaded. AGENTS.md §1 states the surface; this
is where it is held.
"""

from __future__ import annotations

import hashlib
import logging
import re
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlsplit

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


RESPONSE_TIMEOUT_MS = 30_000
# The first characters of a failed response's body an error keeps.
BODY_EXCERPT_CHARS = 200

# The requests the route guard sees: everything under the legacy app,
# and the account API with its list endpoint. Of these, a GET is
# checked when it is a legacy page or an account; the legacy app's
# scripts and styles, and the SPA's list call, pass.
_GUARDED = r"/(?:assetview/|api/v3/cash-accounts(?![\w-]))"
_GUARDED_PATH_RE = re.compile(rf"^{_GUARDED}")
_CHECKED_GET_RE = re.compile(
    r"^/(?:assetview/.*\.do|api/v3/cash-accounts/.+)$")
# A token-shaped run of characters, as an error, a body or a reference
# may print one: long, and with a digit in it.
_TOKEN_SHAPED_RE = re.compile(r"[A-Za-z0-9_\-+%~]{20,}")


def _ref(url: str) -> str:
    """A URL's site-relative reference: its path and query."""
    split = urlsplit(url)
    return split.path + (f"?{split.query}" if split.query else "")


def redact(ref: str) -> str:
    """A reference as an error may print it: each token in its path as
    its short token, and the query by its names alone."""
    split = urlsplit(ref)
    path = scrub(split.path)
    names = [f.partition("=")[0] for f in split.query.split("&") if f]
    return path + (f"?{'&'.join(n + '=…' for n in names)}" if names else "")


def scrub(text: str, *, drop: bool = False) -> str:
    """`text` with each token-shaped run as its short token, or left
    out with `drop`."""
    def one(m: re.Match) -> str:
        run = m.group(0)
        if not any(c.isdigit() for c in run):
            return run
        return "<token>" if drop else bronze.short_token(run)
    return _TOKEN_SHAPED_RE.sub(one, text)


def excerpt(body: str) -> str:
    """The start of a failed response's body, with every token-shaped
    run of characters left out. The body is scrubbed before it is cut,
    so a token the cut splits is still dropped whole."""
    return scrub(body, drop=True)[:BODY_EXCERPT_CHARS]


class _Failed(RuntimeError):
    """A failure with the fields its error entry carries."""

    def __init__(self, message: str, **fields) -> None:
        super().__init__(message)
        self.fields = fields


def error_text(e: Exception) -> str:
    """An exception as an error entry and a log line print it."""
    return scrub(debugcap.safe_error(e))


class RouteGuard:
    """The pass's read-only guard on the page, and its record of what
    the legacy pages and the account API answered.

    :meth:`handle` is the page's route handler for every guarded
    request; :meth:`on_response` notes each guarded response's status,
    and the start of its body when it failed, for the error that
    follows.
    """

    def __init__(self, origin: str) -> None:
        self.origin = origin.rstrip("/")
        self.pattern = re.compile(rf"^{re.escape(self.origin)}{_GUARDED}")
        self.aborted: list[dict] = []
        self.answers: dict[str, dict] = {}

    @staticmethod
    def refusal(method: str, url: str) -> str | None:
        """Why the guard aborts this request, or None."""
        split = urlsplit(url)
        if not _GUARDED_PATH_RE.match(split.path):
            return None
        if method.upper() != "GET":
            return f"{method} is not a read"
        if _CHECKED_GET_RE.match(split.path):
            return refuse_request("GET", _ref(url))
        return None

    def handle(self, route, request) -> None:
        why = self.refusal(request.method, request.url)
        why = scrub(why) if why is not None else None
        if why is None:
            route.continue_()
            return
        log.warning("rate guard aborted %s %s: %s", request.method,
                    redact(_ref(request.url)), why)
        self.aborted.append({"stage": "guard", "method": request.method,
                             "ref": redact(_ref(request.url)),
                             "error": why})
        route.abort()

    def on_response(self, response) -> None:
        split = urlsplit(response.url)
        if not _CHECKED_GET_RE.match(split.path):
            return
        # The status is noted before the body is read: a handler's read
        # waits for the body while the pass goes on.
        answer = {"http_status": response.status}
        self.answers[split.path] = answer
        if response.status >= 400:
            try:
                answer["body"] = excerpt(response.text())
            except Exception:  # noqa: BLE001 — a diagnostic, not a read
                pass

    def forget(self, path: str) -> None:
        """Drop what the page at `path` answered before, ahead of a new
        load of it."""
        self.answers.pop(path, None)

    def answer(self, path: str) -> dict:
        """What the page at `path` last answered, as error fields."""
        return dict(self.answers.get(path, {}))


@dataclass
class Surface:
    """The browser the pass reads, and what `download.py` does for it.

    `spa_url` makes a hash route a URL of the SPA, `open_legacy` opens
    the "Account balances" view and returns the frame it renders the
    legacy entry in, and `shot` takes a named screenshot when the run
    asked for them.
    """

    page: object
    origin: str
    spa_url: Callable[[str], str]
    open_legacy: Callable[[], object]
    shot: Callable[[str], None]


def legacy_file_name(iban: str) -> str:
    """The bronze name of a legacy page: its account's IBAN, hashed."""
    return f"legacy-{hashlib.sha256(iban.encode('ascii')).hexdigest()}.html"


def capture(surface: Surface, run_dir: Path, accounts: list[dict]) -> dict:
    """Capture both surfaces into ``<run_dir>/rates/``, under the guard.

    Returns the manifest block for ``run.json``. Each account stands
    alone: one that fails is recorded as an error and the rest still
    land. A missed legacy entry is recorded and the API files stand.
    The legacy pass runs even when the modern one covered nobody. The
    guard is removed when the pass ends, also on failure.
    """
    rates_dir = run_dir / "rates"
    rates_dir.mkdir(parents=True, exist_ok=True)
    result: dict = {"api": {"attempted": [], "answered": []},
                    "legacy": {"listed": 0, "attempted": [], "answered": [],
                               "skipped_covered": []},
                    "errors": []}
    page = surface.page
    guard = RouteGuard(surface.origin)
    page.route(guard.pattern, guard.handle)
    page.on("response", guard.on_response)
    try:
        covered = _capture_api(surface, guard, rates_dir, accounts, result)
        try:
            frame = surface.open_legacy()
        except Exception as e:  # noqa: BLE001 — the modern files stay
            log.warning("legacy rate entry not found: %s", error_text(e))
            result["errors"].append({"stage": "legacy-entry",
                                     "error": error_text(e)})
            surface.shot("rates-legacy-entry-failed")
        else:
            try:
                _capture_legacy(surface, guard, frame, rates_dir, covered,
                                result)
            except Exception as e:  # noqa: BLE001 — the modern files stay
                log.warning("legacy rate pass failed: %s", error_text(e))
                result["errors"].append({
                    "stage": "legacy-list", "error": error_text(e),
                    **guard.answer(ubs.LEGACY_CONTRACT_LIST_PATH)})
                surface.shot("rates-legacy-list-failed")
    finally:
        page.unroute(guard.pattern, guard.handle)
        page.remove_listener("response", guard.on_response)
        result["errors"].extend(guard.aborted)

    log.info("rates: api %d of %d answered, %d stating conditions; "
             "legacy %d of %d answered, %d covered by the api; %d error(s)",
             len(result["api"]["answered"]), len(result["api"]["attempted"]),
             sum(a["states_conditions"] for a in result["api"]["answered"]),
             len(result["legacy"]["answered"]),
             len(result["legacy"]["attempted"]),
             len(result["legacy"]["skipped_covered"]),
             len(result["errors"]))
    return result


def _capture_api(surface: Surface, guard: RouteGuard, rates_dir: Path,
                 accounts: list[dict], result: dict) -> set[str]:
    """Keep the SPA's own account API response for each homepage
    account; return the IBANs whose conditions it stated."""
    covered: set[str] = set()
    for account in accounts:
        account_id = account["account_id"]
        short = bronze.short_token(account_id)
        result["api"]["attempted"].append(short)
        try:
            payload = _api_response(surface, account_id)
        except Exception as e:  # noqa: BLE001 — one account, not the pass
            log.warning("rates for account %s… failed: %s", short[:8],
                        error_text(e))
            result["errors"].append({"account_short_id": short,
                                     "stage": "api", "error": error_text(e),
                                     **getattr(e, "fields", {})})
            surface.shot(f"rates-api-{short}-failed")
            continue
        name = f"{account_id}.json"
        bronze.atomic_write_json(rates_dir / name, payload)
        states = rate_parsers.api_states_conditions(payload)
        result["api"]["answered"].append({"account_short_id": short,
                                          "file": name,
                                          "states_conditions": states})
        if states and (iban := rate_parsers.api_iban(payload)):
            covered.add(iban)
    return covered


def _api_response(surface: Surface, account_id: str) -> dict:
    """The SPA's response to its own call for one account's conditions.

    The pass opens the account's "Account information" route; the SPA
    makes the call with its own headers. Only the response to exactly
    the account's reference counts.
    """
    if not _TOKEN_RE.match(account_id or ""):
        raise RuntimeError("account token is not in the admitted set")
    want = (f"{ubs.RATES_API_ACCOUNT_PREFIX}{account_id}"
            f"?{ubs.RATES_API_INCLUDE_QUERY}")
    page = surface.page

    def is_ours(response) -> bool:
        split = urlsplit(response.url)
        return (f"{split.scheme}://{split.netloc}" == surface.origin
                and _ref(response.url) == want)

    try:
        with page.expect_response(is_ours,
                                  timeout=RESPONSE_TIMEOUT_MS) as info:
            page.goto(surface.spa_url(
                ubs.account_information_route(account_id)),
                wait_until="domcontentloaded")
        response = info.value
    except Exception as e:
        raise _Failed("no account API response within "
                      f"{RESPONSE_TIMEOUT_MS // 1000}s: "
                      f"{error_text(e)}") from e
    if not response.ok:
        try:
            body = excerpt(response.text())
        except Exception:  # noqa: BLE001 — a diagnostic, not a read
            body = ""
        raise _Failed(f"HTTP {response.status} for the account API",
                      http_status=response.status, body=body)
    return response.json()


def _framed(frame, guard: RouteGuard, path: str):
    """The frame `frame` renders in its main frame, once it shows the
    page at `path` and that page did not answer an error."""
    el = frame.wait_for_selector(f'iframe[name="{ubs.LEGACY_MAIN_FRAME_NAME}"]',
                                 state="attached",
                                 timeout=RESPONSE_TIMEOUT_MS)
    child = el.content_frame() if el is not None else None
    if child is None:
        raise RuntimeError("the page frames no page")
    child.wait_for_load_state("load", timeout=RESPONSE_TIMEOUT_MS)
    shown = urlsplit(child.url).path
    if shown != path:
        raise RuntimeError(f"the frame shows {shown.rsplit('/', 1)[-1]}, "
                           f"not {path.rsplit('/', 1)[-1]}")
    answer = guard.answer(path)
    if answer.get("http_status", 200) >= 400:
        raise _Failed(f"HTTP {answer['http_status']} for "
                      f"{path.rsplit('/', 1)[-1]}", **answer)
    return child


def _open(surface: Surface, guard: RouteGuard, frame, ref: str,
          referer: str) -> None:
    """Open the reference a page gave in `frame`, as its link does,
    with the referer of the page that gave it."""
    refusal = refuse_request("GET", ref)
    if refusal is not None:
        raise RuntimeError(f"refusing to open: {scrub(refusal)}")
    guard.forget(ubs.LEGACY_DISPATCHER_PATH)
    guard.forget(ubs.LEGACY_POSITION_DETAIL_PATH)
    response = frame.goto(surface.origin + ref, wait_until="load",
                          referer=referer, timeout=RESPONSE_TIMEOUT_MS)
    if response is not None and response.status >= 400:
        # The response that ends the redirect chain, so the page named
        # is the one that failed.
        path = urlsplit(response.url).path
        raise _Failed(f"HTTP {response.status} for "
                      f"{path.rsplit('/', 1)[-1]}",
                      **guard.answer(path))


def _contract_list(frame, guard: RouteGuard) -> tuple[list[dict], str]:
    """The account rows of the contract list the entry frame renders,
    and the list's URL."""
    if urlsplit(frame.url).path != ubs.LEGACY_DISPATCHER_PATH:
        raise RuntimeError("the account balances view frames another page")
    listing = _framed(frame, guard, ubs.LEGACY_CONTRACT_LIST_PATH)
    return (rate_parsers.legacy_account_rows(
        debugcap.scrub_dom(listing.content())), listing.url)


def _account_page(surface: Surface, guard: RouteGuard, frame,
                  link: str, list_url: str) -> str:
    """One account's page, by its "Account details" link, opened in the
    list's parent frame as the item opens it, from the list. The
    browser follows the link's redirect; the dispatcher it lands on
    frames the page.

    A route handler sees only a redirect's first URL, so the page the
    redirect lands on is checked against the same allow-list once it
    has loaded.
    """
    _open(surface, guard, frame, link, referer=list_url)
    refusal = refuse_request("GET", _ref(frame.url))
    if refusal is not None:
        raise RuntimeError(
            f"the details link led to a refused page: {scrub(refusal)}")
    landed = urlsplit(frame.url).path
    if landed != ubs.LEGACY_DISPATCHER_PATH:
        raise RuntimeError(f"the details link led to "
                           f"{landed.rsplit('/', 1)[-1]}")
    # Kept in bronze, so serialised as every captured DOM is.
    return debugcap.scrub_dom(_framed(
        frame, guard, ubs.LEGACY_POSITION_DETAIL_PATH).content())


def _capture_legacy(surface: Surface, guard: RouteGuard, frame,
                    rates_dir: Path, covered: set[str], result: dict) -> None:
    """Read the page of each listed account the modern pass did not
    cover.

    The list's links are ids the server resolves for the render that
    printed them. Each account is read from a fresh render: before
    every account after the first, "Account balances" is opened again
    as the menu opens it, and the account found again by its IBAN. A
    row that prints no IBAN cannot be found again, and is recorded
    rather than read. A page is kept only when its own IBAN is its
    row's. A fresh render that fails is recorded and ends the pass:
    the accounts after it are not attempted.
    """
    rows, list_url = _contract_list(frame, guard)
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
        if not fresh:
            guard.forget(ubs.LEGACY_CONTRACT_LIST_PATH)
            try:
                frame = surface.open_legacy()
                rows, list_url = _contract_list(frame, guard)
            except Exception as e:  # noqa: BLE001 — the read pages stay
                log.warning("legacy rate entry not found again: %s",
                            error_text(e))
                result["errors"].append({
                    "stage": "legacy-entry", "error": error_text(e),
                    **getattr(e, "fields", {})})
                surface.shot("rates-legacy-entry-failed")
                break
        fresh = False
        row = next((r for r in rows if r["iban"] == iban), None)
        if row is None:
            result["errors"].append({"stage": "legacy",
                                     "error": "an account left the list"})
            continue
        token = bronze.short_token(row["ref"])
        legacy["attempted"].append(token)
        try:
            html = _account_page(surface, guard, frame, row["ref"],
                                 list_url)
        except Exception as e:  # noqa: BLE001 — one account, not the pass
            log.warning("legacy rates for %s… failed: %s",
                        token[:8], error_text(e))
            result["errors"].append({
                "link_short_id": token, "stage": "legacy",
                "error": error_text(e),
                **getattr(e, "fields", {})})
            surface.shot(f"rates-legacy-{token}-failed")
            continue
        page_iban = rate_parsers.legacy_iban(html)
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
        name = legacy_file_name(page_iban)
        bronze.atomic_write_bytes(rates_dir / name, html.encode("utf-8"))
        legacy["answered"].append({"link_short_id": token, "file": name})
