#!/usr/bin/env python3
"""
REST bronze dump for Relevate (portal.pens-expert.ch).

Replays the `/middlelayer/v2/` REST surface from a persisted
session (cookies established by `login.py`) and lands JSON + PDF
artefacts in a versioned bronze tree:

    /data/<UTC-ts>/
    ├── run.json
    ├── accounts/
    │   ├── investment-overview.json
    │   ├── compliance-products.json
    │   ├── contact-messages.json
    │   ├── contact-notification.json
    │   ├── contact-risk-protection.json
    │   └── sso-claims.json
    ├── portfolios/
    │   └── <sha256(externalId)[:16]>/
    │       ├── deposits-YYYY.json    one per year the --lookback window spans
    │       ├── performance.json
    │       ├── fees.json
    │       ├── investment-allocation.json
    │       └── modelportfolio.json
    └── documents/
        ├── index.json
        ├── <docId>.pdf
        └── <docId>.unexpected.<ext>   (only if API returned non-PDF)

Read-only: only GETs against `/middlelayer/v2/`. No write
surfaces, no `/change` URLs, no mutation flags accepted.

Document fetches are download-avoidant via the shared
`collectorkit.docdedup` engine, chosen per document kind (the
fileName-derived label load.py parses on): executed-once immutable
kinds not parsed by load (fee statements, pension agreements/plans,
investor profiles, account-opening docs) hardlink an identical copy
from a prior complete run instead of re-fetching (a hardlink error
falls through to a real fetch — never a miss); parsed / tax-adjacent
kinds (quarterly reports, credit notes, leaving statements) and any
unrecognised kind are always fetched and content-compared (a
byte-identical copy is still hardlinked to reclaim disk, a re-issue
keeps its fresh bytes). Every run dir stays self-contained (a
hardlink is a real in-run file), so the loader needs no cross-run
fallback. --documents-force bypasses the index entirely.

Iteration-cheap flags: --mode / --no-documents /
--limit-portfolios / --limit-documents let one section of the
work be re-run without paying for the others. --dry-run hits
only the master listing endpoints (investment-overview +
documents) to enumerate work without fetching per-portfolio or
per-document payloads.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import sys
import tempfile
import time
from datetime import date, datetime, timezone
from pathlib import Path
from typing import Any

try:
    import requests
    from requests.exceptions import RequestException
except ModuleNotFoundError:  # pragma: no cover
    # The HTTP stack drives the live REST walk, not the pure-Python
    # download-avoidance helpers (docdedup wiring, class mapping, extract
    # hook). Guarding the import lets that logic be imported and unit-tested
    # host-side without the collector's full container deps, the same way the
    # browser collectors defer their heavy imports into the run path.
    requests = None  # type: ignore[assignment]

    class RequestException(Exception):  # type: ignore[no-redef]
        """Fallback so ``except RequestException`` still resolves when the real
        ``requests`` is absent (the HTTP paths are never entered then)."""

from collectorkit import cli, debugcap, docdedup, session as ck_session

# doc_kind_from_filename is the SAME fileName -> kind derivation load.py parses
# on, so the download-avoidance mode is chosen off the exact label load reads
# figures from (a parsed kind can never be mis-linked). See _document_class.
from load import doc_kind_from_filename

BASE = "https://portal.pens-expert.ch"
PROBE = f"{BASE}/auth/rest/protected/self-service/ui/configuration/portal"
DASHBOARD_REFERER = f"{BASE}/dashboard/"

DEFAULT_STATE_PATH = Path("/secrets/relevate-state.json")
DEFAULT_BRONZE_DIR = Path("/data")

USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)
SEC_CH_UA = (
    '"Not_A Brand";v="99", "Chromium";v="147", "Google Chrome";v="147"'
)

# Endpoints. Note the inconsistent casing of "Portfolio" vs
# "portfolio" — observed on the wire; do NOT normalise.
# /portfolio/services (400 without query params we can't synthesise)
# and /contact/language (405 on GET) are deliberately omitted.
EP_INVESTMENT_OVERVIEW = "/middlelayer/v2/portfolio/investment-overview"
EP_DOCUMENTS_INDEX = "/middlelayer/v2/documents"
EP_COMPLIANCE_PRODUCTS = "/middlelayer/v2/compliance/products"
EP_CONTACT_MESSAGES = "/middlelayer/v2/contact/messages"
EP_CONTACT_NOTIFICATION = "/middlelayer/v2/contact/notification"
EP_CONTACT_RISK_PROTECTION = "/middlelayer/v2/contact/risk-protection"
EP_SSO_CLAIMS = "/middlelayer/v2/sso/claims"


def ep_deposits(pid: int) -> str:
    return f"/middlelayer/v2/Portfolio/{pid}/deposits"


def ep_performance(pid: int) -> str:
    return f"/middlelayer/v2/portfolio/{pid}/performance"


def ep_fees(pid: int) -> str:
    return f"/middlelayer/v2/portfolio/{pid}/fees"


def ep_allocation(pid: int) -> str:
    return f"/middlelayer/v2/portfolio/{pid}/investment/allocation"


def ep_modelportfolio(proposal_id: int) -> str:
    return f"/middlelayer/v2/Portfolio/proposal/{proposal_id}/modelportfolio"


def ep_document(doc_id: int) -> str:
    return f"/middlelayer/v2/document/{doc_id}"


# Ancillary GETs that return useful 200/204 bodies.
# /contact/risk-protection returns 204 (no content) for accounts
# without protection set up; kept and handled specially in
# get_and_save_json so 204 is a non-error.
ANCILLARY_ACCOUNTS = (
    ("compliance-products.json", EP_COMPLIANCE_PRODUCTS),
    ("contact-messages.json", EP_CONTACT_MESSAGES),
    ("contact-notification.json", EP_CONTACT_NOTIFICATION),
    ("contact-risk-protection.json", EP_CONTACT_RISK_PROTECTION),
    ("sso-claims.json", EP_SSO_CLAIMS),
)

logger = logging.getLogger("download")


# ----------------------------------------------------------------------
# State + session
# ----------------------------------------------------------------------


def load_state(path: Path) -> dict[str, Any] | None:
    return ck_session.load_state(path)


def state_into_jar(state_cookies: list[dict[str, Any]], jar) -> None:
    for c in state_cookies:
        jar.set(
            c["name"], c["value"],
            domain=c.get("domain"),
            path=c.get("path", "/"),
            secure=c.get("secure", False),
            expires=c.get("expires"),
            rest=c.get("rest", {}),
        )


def new_session_from_state(
    state_path: Path,
) -> tuple[requests.Session, str | None]:
    """Load the persisted state file and build a ready-to-use
    requests.Session. Returns (session, minted_at) where minted_at
    is the ISO timestamp the state was minted at (or None if
    absent from the file)."""
    state = load_state(state_path)
    if state is None:
        raise FileNotFoundError(
            f"state file missing: {state_path}. Run "
            "`./relevate login` first.",
        )
    session = requests.Session()
    session.headers.update({
        "User-Agent": USER_AGENT,
        # Mirror the SPA's literal-undefined header. Airlock doesn't
        # validate it; sending the same string keeps our fingerprint
        # identical to the browser's.
        "Authorization": "bearer undefined",
        "Accept": "application/vnd.api+json, application/json, */*",
        "Accept-Language": "en-US,en;q=0.9",
        "Origin": BASE,
        "Referer": DASHBOARD_REFERER,
        "Sec-Ch-Ua": SEC_CH_UA,
        "Sec-Ch-Ua-Mobile": "?0",
        "Sec-Ch-Ua-Platform": '"macOS"',
        "Sec-Fetch-Dest": "empty",
        "Sec-Fetch-Mode": "cors",
        "Sec-Fetch-Site": "same-origin",
        "X-Same-Domain": "1",
    })
    state_into_jar(state.get("cookies", []), session.cookies)
    return session, state.get("minted_at")


def probe_session_alive(session: requests.Session) -> bool:
    try:
        resp = session.get(PROBE, timeout=15, allow_redirects=False)
    except RequestException as exc:
        logger.warning("session probe failed: %s", exc)
        return False
    return resp.status_code == 200


# ----------------------------------------------------------------------
# Manifest
# ----------------------------------------------------------------------


def _empty_doc_counts() -> dict[str, Any]:
    """The documents audit block. ``count_in_*`` record the index/window split;
    the rest is the per-document download-avoidance audit (collectorkit.docdedup
    outcomes; see _tally / docdedup.tally). ``total`` is the count of in-window docs
    routed through the engine; the rest tally how each resolved: ``fetched``
    fresh bytes kept, ``linked`` an immutable prior copy hardlinked in (fetch
    avoided), ``verified`` fetch-verify byte-identical to prior (hardlinked,
    disk reclaimed), ``changed`` fetch-verify re-issue (fresh bytes kept),
    ``errors`` a per-document fetch that produced no PDF (non-200, network
    error, or a non-PDF body), ``other`` an unmapped outcome."""
    return {
        "count_in_index": 0,
        "count_in_window": 0,
        "count_outside_window": 0,
        **docdedup.empty_audit(),   # total + per-outcome buckets
        "unexpected_content_type": [],
        "files": [],
    }


class Manifest:
    """
    Incremental run.json writer. Flush after every successful fetch
    so a Ctrl-C run still leaves an inspectable manifest.

    Carries the fleet-uniform ``status`` lifecycle: the manifest is
    born ``"in-progress"`` (flushed at construction, so the marker
    exists from the moment the run dir is created), and ``finish()``
    overwrites it with the terminal ``"complete"`` / ``"dry-run"`` /
    ``"incomplete"`` at the end. ``prune`` keys on that field to tell
    a finished dump from a crashed walk; ``load`` skips a dump still
    marked ``"in-progress"`` or ``"dry-run"``. ``ended_at`` / ``dry_run``
    are written alongside it — they are the terminal signal ``prune``
    falls back to for a manifest carrying no ``status``.
    """

    def __init__(self, run_dir: Path, mode: str, dry_run: bool) -> None:
        self.path = run_dir / "run.json"
        self.data: dict[str, Any] = {
            "tool": "relevate.download",
            "schema_version": 1,
            "started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "status": "in-progress",
            "ended_at": None,
            "mode": mode,
            "dry_run": dry_run,
            "state_minted_at": None,
            "windows": None,
            "accounts": [],
            "documents": _empty_doc_counts(),
            "errors": [],
            "files": [],
        }
        # Persist the "in-progress" marker immediately, so a run dir
        # that crashes before any fetch still carries a status prune
        # can classify (and load can skip) — not an empty dir.
        self.flush()

    def set_state_minted_at(self, ts: str | None) -> None:
        self.data["state_minted_at"] = ts
        self.flush()

    def set_windows(self, *, since: date, until: date,
                    documents_since: date, documents_until: date) -> None:
        """Record the resolved date windows for this run. Mirrors the
        viac/run.json shape so any future shared loader can read either."""
        self.data["windows"] = {
            "since": since.isoformat(),
            "until": until.isoformat(),
            "documents_since": documents_since.isoformat(),
            "documents_until": documents_until.isoformat(),
        }
        self.flush()

    def add_file(self, rel_path: str) -> None:
        self.data["files"].append(rel_path)
        self.flush()

    def add_error(
        self, *, endpoint: str, status: int | None, message: str,
    ) -> None:
        self.data["errors"].append({
            "endpoint": endpoint,
            "status": status,
            "message": message,
        })
        self.flush()

    def add_account(self, account: dict[str, Any]) -> None:
        self.data["accounts"].append(account)
        self.flush()

    def documents(self) -> dict[str, Any]:
        return self.data["documents"]

    def finish(self, status: str = "complete") -> None:
        """Stamp the terminal ``status`` + ``ended_at`` and flush,
        overwriting the ``"in-progress"`` marker set at construction.
        ``status`` is ``"complete"`` for a finished real walk (set even
        when some endpoints errored — the walk still ran and load
        ingests dumps-with-errors), ``"dry-run"`` for a ``--dry-run``
        shell, ``"incomplete"`` when the walk aborted early."""
        self.data["status"] = status
        self.data["ended_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
        self.flush()

    def flush(self) -> None:
        tmp = self.path.with_suffix(self.path.suffix + ".tmp")
        tmp.write_text(json.dumps(self.data, indent=2), encoding="utf-8")
        tmp.replace(self.path)


# ----------------------------------------------------------------------
# Fetch helpers
# ----------------------------------------------------------------------


def account_slug(external_id: str) -> str:
    return hashlib.sha256(external_id.encode("utf-8")).hexdigest()[:16]


# Stands in for callers with no run dir to write into. A disabled trace
# swallows every record(), so the request path needs no None-guard.
_NO_TRACE = debugcap.HttpTrace(None, log=logger, enabled=False)


def get_and_save_json(
    session: requests.Session,
    endpoint: str,
    dest_path: Path,
    manifest: Manifest,
    *,
    relative_to: Path,
    timeout: int = 30,
    trace: debugcap.HttpTrace = _NO_TRACE,
) -> dict[str, Any] | None:
    """
    GET an endpoint expecting JSON, save the response body verbatim
    to dest_path, return the parsed body. On error, log + record in
    manifest.errors and return None. Idempotent at the dir level —
    the caller is responsible for not re-fetching if the file
    already exists.

    One of the two request choke points, so `trace` records the exchange
    here, before the status is branched on: the 200, the 204 marker, the
    unexpected status, and the failure that never reached Relevate all
    land as one line each.
    """
    url = BASE + endpoint
    t0 = time.monotonic()
    try:
        resp = session.get(url, timeout=timeout, allow_redirects=False)
    except RequestException as exc:
        # No status: nothing came back to carry one.
        trace.record("GET", url, elapsed_ms=(time.monotonic() - t0) * 1000,
                     error=f"{type(exc).__name__}: {exc}")
        logger.error("GET %s: network failure: %s", endpoint, exc)
        manifest.add_error(
            endpoint=endpoint, status=None, message=str(exc),
        )
        return None
    trace.record("GET", url, status=resp.status_code,
                 elapsed_ms=(time.monotonic() - t0) * 1000,
                 bytes_=len(resp.content), headers=resp.headers)
    if resp.status_code == 204:
        # No-content success. Save a marker so the silver loader
        # can tell "we asked, Relevate said no data" vs "we never
        # asked". Not an error.
        dest_path.parent.mkdir(parents=True, exist_ok=True)
        dest_path.write_text(
            json.dumps({"_no_content": True, "_status": 204}, indent=2),
            encoding="utf-8",
        )
        manifest.add_file(str(dest_path.relative_to(relative_to)))
        logger.info("GET %s: 204 No Content (marker saved)", endpoint)
        return None
    if resp.status_code != 200:
        logger.error(
            "GET %s: expected 200, got %s",
            endpoint, resp.status_code,
        )
        manifest.add_error(
            endpoint=endpoint,
            status=resp.status_code,
            message=f"unexpected status {resp.status_code}",
        )
        return None
    dest_path.parent.mkdir(parents=True, exist_ok=True)
    dest_path.write_bytes(resp.content)
    manifest.add_file(str(dest_path.relative_to(relative_to)))
    try:
        return resp.json()
    except ValueError:
        logger.warning(
            "GET %s: body wasn't JSON despite saved %d bytes",
            endpoint, len(resp.content),
        )
        return None


def get_and_save_binary(
    session: requests.Session,
    endpoint: str,
    dest_path: Path,
    manifest: Manifest,
    *,
    relative_to: Path,
    expected_content_type: str = "application/pdf",
    timeout: int = 60,
    trace: debugcap.HttpTrace = _NO_TRACE,
) -> str | None:
    """
    GET an endpoint expecting a binary body (e.g. PDF), save the
    response verbatim. Returns the actual content-type on success,
    None on failure. If the content-type differs from expected,
    save under <name>.unexpected.<ext> instead and record.

    The second request choke point; traced like its JSON twin. The
    content-type that decided the .unexpected rename is on the trace line
    too — it is a whitelisted response header.
    """
    url = BASE + endpoint
    t0 = time.monotonic()
    try:
        resp = session.get(url, timeout=timeout, allow_redirects=False)
    except RequestException as exc:
        trace.record("GET", url, elapsed_ms=(time.monotonic() - t0) * 1000,
                     error=f"{type(exc).__name__}: {exc}")
        logger.error("GET %s: network failure: %s", endpoint, exc)
        manifest.add_error(
            endpoint=endpoint, status=None, message=str(exc),
        )
        return None
    trace.record("GET", url, status=resp.status_code,
                 elapsed_ms=(time.monotonic() - t0) * 1000,
                 bytes_=len(resp.content), headers=resp.headers)
    if resp.status_code != 200:
        logger.error(
            "GET %s: expected 200, got %s",
            endpoint, resp.status_code,
        )
        manifest.add_error(
            endpoint=endpoint,
            status=resp.status_code,
            message=f"unexpected status {resp.status_code}",
        )
        return None
    ct = (resp.headers.get("content-type") or "").split(";")[0].strip()
    if ct != expected_content_type:
        logger.warning(
            "GET %s: unexpected content-type %r (expected %r); "
            "saving with .unexpected suffix",
            endpoint, ct, expected_content_type,
        )
        # Choose a safe extension based on content-type.
        ext = "bin"
        if "json" in ct:
            ext = "json"
        elif "html" in ct:
            ext = "html"
        elif "xml" in ct:
            ext = "xml"
        elif "text" in ct:
            ext = "txt"
        dest_path = dest_path.with_name(dest_path.stem + ".unexpected." + ext)
    dest_path.parent.mkdir(parents=True, exist_ok=True)
    dest_path.write_bytes(resp.content)
    manifest.add_file(str(dest_path.relative_to(relative_to)))
    return ct


# ----------------------------------------------------------------------
# Phase steps
# ----------------------------------------------------------------------


def fetch_accounts_phase(
    session: requests.Session,
    run_dir: Path,
    manifest: Manifest,
    *,
    skip_ancillary: bool,
    trace: debugcap.HttpTrace = _NO_TRACE,
) -> dict[str, Any] | None:
    """
    Fetch /portfolio/investment-overview (the master enumeration)
    plus the small ancillary user-scope GETs. Returns the parsed
    investment-overview body so subsequent phases can walk
    portfolios[]; None on failure.
    """
    accounts_dir = run_dir / "accounts"
    overview = get_and_save_json(
        session,
        EP_INVESTMENT_OVERVIEW,
        accounts_dir / "investment-overview.json",
        manifest,
        relative_to=run_dir,
        trace=trace,
    )
    if overview is None:
        logger.error("investment-overview failed — cannot enumerate portfolios")
        return None
    n_portfolios = len(overview.get("portfolios") or [])
    logger.info("investment-overview: %d portfolios", n_portfolios)

    if not skip_ancillary:
        for fname, ep in ANCILLARY_ACCOUNTS:
            get_and_save_json(
                session, ep, accounts_dir / fname, manifest,
                relative_to=run_dir, trace=trace,
            )

    return overview


def fetch_portfolio(
    session: requests.Session,
    portfolio: dict[str, Any],
    run_dir: Path,
    manifest: Manifest,
    *,
    year_from: int,
    year_to: int,
    trace: debugcap.HttpTrace = _NO_TRACE,
) -> None:
    """
    Fetch every observed per-portfolio endpoint into
    /data/<run>/portfolios/<slug>/.

    ``year_from`` / ``year_to`` bound the /deposits iteration: year is the
    smallest granularity that endpoint accepts, so the run's window is
    expressed as the years it spans.
    """
    pid = portfolio.get("id")
    external_id = portfolio.get("externalId")
    if pid is None or not external_id:
        logger.warning(
            "portfolio missing id or externalId: %s",
            {k: v for k, v in portfolio.items() if k in ("id", "externalId")},
        )
        return
    slug = account_slug(str(external_id))
    pdir = run_dir / "portfolios" / slug
    pdir.mkdir(parents=True, exist_ok=True)

    product = portfolio.get("product") or {}
    proposal_id = portfolio.get("portfolioProposalId")
    first_investment_date = portfolio.get("firstInvestmentDate")
    currency = (portfolio.get("currency") or {}).get("currencyCode")

    year_window = (year_from, year_to)

    logger.info(
        "portfolio id=%s slug=%s product=%s proposal=%s "
        "deposits=years %s-%s",
        pid, slug, product.get("key"), proposal_id,
        year_window[0], year_window[1],
    )

    manifest.add_account({
        "id": pid,
        "slug": slug,
        "product_key": product.get("key"),
        "product_name": product.get("name"),
        "product_offer_id": product.get("productOfferId"),
        "currency": currency,
        "proposal_id": proposal_id,
        "is_active": portfolio.get("isActive"),
        "first_investment_date": first_investment_date,
        "portfolio_type_id": portfolio.get("portfolioTypeId"),
        "portfolio_status_id": portfolio.get("portfolioStatusId"),
        "deposits_year_window": list(year_window) if year_window else None,
    })

    for year in range(year_window[0], year_window[1] + 1):
        get_and_save_json(
            session,
            ep_deposits(pid) + f"?year={year}",
            pdir / f"deposits-{year}.json",
            manifest,
            relative_to=run_dir,
            trace=trace,
        )

    # The four single-shot per-portfolio endpoints.
    get_and_save_json(
        session, ep_performance(pid),
        pdir / "performance.json", manifest, relative_to=run_dir, trace=trace,
    )
    get_and_save_json(
        session, ep_fees(pid),
        pdir / "fees.json", manifest, relative_to=run_dir, trace=trace,
    )
    get_and_save_json(
        session, ep_allocation(pid),
        pdir / "investment-allocation.json", manifest, relative_to=run_dir,
        trace=trace,
    )
    if proposal_id is not None:
        get_and_save_json(
            session, ep_modelportfolio(proposal_id),
            pdir / "modelportfolio.json", manifest, relative_to=run_dir,
            trace=trace,
        )
    else:
        logger.info(
            "portfolio id=%s has no portfolioProposalId; "
            "skipping modelportfolio",
            pid,
        )


def _doc_create_date(entry: dict) -> date | None:
    """Extract a YYYY-MM-DD date from a /middlelayer/v2/documents index
    entry's `createDate`. Returns None on missing / unparseable —
    callers treat None as "in window" so we never silently drop an
    entry the source didn't time-stamp.

    Relevate's createDate format is `YYYY-MM-DDTHH:MM:SS` with no
    timezone suffix; the first 10 chars are the date portion."""
    s = entry.get("createDate")
    if not isinstance(s, str) or len(s) < 10:
        return None
    try:
        return date.fromisoformat(s[:10])
    except ValueError:
        return None


# --- doc_kind -> docdedup class (download-avoidance mode selection) ---------
#
# The mode is a property of the (collector, document-kind) pair, keyed on the
# SAME fileName-derived kind load.py parses on (doc_kind_from_filename), so the
# safe direction is established off exactly what reaches silver: anything load
# parses is fetch-verified, never linked. fetch-verify is the default; a kind is
# opted into link only when a byte-identical prior copy is safe to serve in
# place of a fresh fetch.

# fetch-verify (always re-read + content-compare): parsed and/or tax-adjacent
# kinds. quarterly_report feeds historical_position_snapshots and credit_note
# feeds transactions — both parsed by load.py for figures, and either can be
# re-issued/corrected under a stable Relevate document id, so link-mode would
# risk serving a superseded figure into silver. leaving_statement is a payout /
# tax-adjacent statement, fetch-verified for the same reason. fetch-verify still
# hardlinks a byte-identical prior (disk reclaimed) while always catching a
# re-issue (CHANGED -> fresh bytes kept).
_FETCH_VERIFY_KINDS = frozenset({
    "quarterly_report",   # Quartalsbericht — parsed for positions + cash
    "credit_note",        # Gutschriftsanzeige — parsed for transactions
    "leaving_statement",  # payout, tax-adjacent
})

# link (fetch-avoidance): executed-once documents that are immutable once
# issued and are NOT parsed by load.py, so linking a prior identical copy
# realizes the avoidance win at zero silver-correctness risk. An explicit
# allow-list — anything not named here (incl. the catch-all 'other') is
# fetch-verified, never linked.
_LINK_KINDS = frozenset({
    "quarterly_fee",       # Gebührenabrechnung / fee statement
    "pension_agreement",   # Vorsorgevereinbarung
    "pension_plan",
    "investor_profile",    # Anlegerprofil
    "account_opening",     # Eröffnung / Eintritt
})


def _document_class(entry: dict) -> str | None:
    """Map a /middlelayer/v2/documents index entry to a docdedup class.

    The kind is derived from the entry's ``fileName`` by
    ``load.doc_kind_from_filename`` — the same label load.py parses on.
    Parsed / tax-adjacent kinds (``_FETCH_VERIFY_KINDS``) resolve to
    ``mutable`` / ``tax`` -> fetch-verify (always fetched and content-compared,
    so a re-issue under a stable id is never linked to a stale copy). Executed-
    once immutable kinds (``_LINK_KINDS``, an explicit allow-list) resolve to
    ``immutable`` -> link (hardlink the prior identical copy, skip the fetch).
    Any other kind (incl. the ``'other'`` catch-all) is left unclassified ->
    the engine fetch-verifies it (the safe default: always fetched, a
    byte-identical copy still deduped).
    """
    kind = doc_kind_from_filename(entry.get("fileName"))
    if kind in _FETCH_VERIFY_KINDS:
        return docdedup.CLASS_TAX if kind == "leaving_statement" \
            else docdedup.CLASS_MUTABLE
    if kind in _LINK_KINDS:
        return docdedup.CLASS_IMMUTABLE
    return None


def extract_relevate(run_dir: Path, manifest: dict | None):
    """docdedup extract hook (disk-driven): recover each prior run's document
    identities from disk.

    Key = ``(doc_id,)`` — the numeric Relevate id parsed from the ``<id>.pdf``
    filename. One file per logical document, so the same document lands at the
    same path every run (multiplicity 1) and the key is collision-free (no size
    guard needed). ``doc_date`` is the entry's ``createDate`` from the same
    run's ``documents/index.json`` when parseable (it drives the freshness
    window); ``None`` when the index is absent or the date is unparseable — a
    ``None`` doc_date never falls inside the window. Enumerating on-disk PDFs
    means a key counts only while its blob still exists (pruned bronze
    self-heals); the ``<id>.unexpected.<ext>`` shells left behind for a non-PDF
    body are skipped."""
    docs_dir = run_dir / "documents"
    if not docs_dir.is_dir():
        return
    # createDate lives in the run's own index.json, not run.json; map id -> date
    # so a prior doc carries the pre-fetch issue date the freshness window keys
    # on.
    dates: dict[int, date] = {}
    try:
        index = json.loads(
            (docs_dir / "index.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        index = None
    if isinstance(index, dict):
        for entry in index.get("documents") or []:
            doc_id = entry.get("id")
            if isinstance(doc_id, int) and not isinstance(doc_id, bool):
                d = _doc_create_date(entry)
                if d is not None:
                    dates[doc_id] = d
    for f in sorted(docs_dir.glob("*.pdf")):
        if ".unexpected." in f.name or f.is_symlink() or not f.is_file():
            continue
        try:
            doc_id = int(f.stem)
        except ValueError:
            continue
        yield docdedup.DocRef(
            key=(doc_id,),
            doc_date=dates.get(doc_id),
            relpath=str(f.relative_to(run_dir)),
        )


# Tally a docdedup outcome into the manifest audit block (shared counters). A
# per-document fetch that produced no PDF (non-200, network error, or a non-PDF
# body) surfaces as FETCH_FAILED and is counted under ``errors``; an unmapped
# outcome goes to ``other``, never silently ``fetched``.
def _tally(counts: dict, status: str) -> None:
    docdedup.tally(counts, status)


def fetch_documents(
    session: requests.Session,
    run_dir: Path,
    manifest: Manifest,
    *,
    limit: int | None,
    documents_since: date,
    documents_until: date,
    force: bool = False,
    trace: debugcap.HttpTrace = _NO_TRACE,
) -> None:
    docs_dir = run_dir / "documents"
    index = get_and_save_json(
        session, EP_DOCUMENTS_INDEX,
        docs_dir / "index.json", manifest, relative_to=run_dir, trace=trace,
    )
    if index is None:
        logger.error("documents index failed — skipping per-doc fetch")
        return
    docs = index.get("documents") or []
    manifest.documents()["count_in_index"] = len(docs)
    manifest.flush()
    logger.info("documents index: %d entries", len(docs))

    # Window filter on createDate. Entries with no/unparseable date
    # are kept (better to over-fetch than silently lose an undated
    # entry). The full index.json is written above regardless so
    # bronze stays a faithful inventory of what was visible.
    in_window: list[dict] = []
    n_outside = 0
    for entry in docs:
        d = _doc_create_date(entry)
        if d is not None and (d < documents_since or d > documents_until):
            n_outside += 1
            continue
        in_window.append(entry)
    manifest.documents()["count_in_window"] = len(in_window)
    manifest.documents()["count_outside_window"] = n_outside
    manifest.flush()
    if n_outside:
        logger.info(
            "documents window %s..%s: %d in scope, %d outside",
            documents_since, documents_until, len(in_window), n_outside,
        )
    docs = in_window

    if limit is not None:
        docs = docs[:limit]
        logger.info("--limit-documents %d", limit)

    # Download-avoidance index (documents only): rebuilt statelessly from the
    # COMPLETE prior runs' on-disk PDFs (extract_relevate), keyed (doc_id,).
    # The current run — still status="in-progress" from the marker Manifest
    # wrote at construction — is excluded, so it never seeds itself. link-mode
    # hardlinks an identical prior copy for the immutable kinds; fetch-verify
    # re-reads everything parsed / tax-adjacent / unclassified. --documents-
    # force bypasses the index (always fetch, no hardlink reuse).
    skip = docdedup.SkipSet.derive(
        run_dir.parent, extract_relevate, exclude_run=run_dir)

    counts = manifest.documents()

    def _fetch_blob(doc_id: int, target: Path):
        """Fetch one document blob to ``<id>.pdf`` and return that path (the
        docdedup fetch contract), or None. A non-PDF body is saved to
        ``<id>.unexpected.<ext>`` by get_and_save_binary and noted here, and
        yields no PDF -> None (the engine reports FETCH_FAILED)."""
        ct = get_and_save_binary(
            session, ep_document(doc_id),
            target, manifest, relative_to=run_dir,
            expected_content_type="application/pdf",
            trace=trace,
        )
        if ct == "application/pdf":
            return target
        if ct is not None:
            counts["unexpected_content_type"].append(
                {"id": doc_id, "content_type": ct})
        return None

    for entry in docs:
        doc_id = entry.get("id")
        if doc_id is None:
            continue
        counts["total"] += 1
        target = docs_dir / f"{doc_id}.pdf"
        status = docdedup.process(
            skip, key=(doc_id,), doc_class=_document_class(entry),
            target_dir=docs_dir, stem=str(doc_id),
            fetch=(lambda d=doc_id, t=target: _fetch_blob(d, t)),
            force=force,
        )
        _tally(counts, status)
        manifest.flush()

    logger.info("documents: %s", docdedup.audit_summary(counts))


# ----------------------------------------------------------------------
# Orchestration
# ----------------------------------------------------------------------


def do_dry_run(
    session: requests.Session,
    run_dir: Path,
    manifest: Manifest,
    *,
    documents_since: date,
    documents_until: date,
) -> int:
    """
    --dry-run: hit the master listing endpoints (cheap),
    enumerate work, exit. No per-portfolio or per-document fetches.
    Also reports how many of the indexed docs survive the window
    filter, which is what makes a --lookback value checkable.

    Deliberately untraced: `run_dir` here is the throwaway temp dir the
    caller deletes on exit, so a `--debug` capture written into it would
    go with it. The default no-op trace makes that a stated choice rather
    than a capture nobody could read.
    """
    logger.info("dry-run: hitting master listing endpoints only")
    overview = get_and_save_json(
        session, EP_INVESTMENT_OVERVIEW,
        run_dir / "accounts" / "investment-overview.json",
        manifest, relative_to=run_dir,
    )
    docs = get_and_save_json(
        session, EP_DOCUMENTS_INDEX,
        run_dir / "documents" / "index.json",
        manifest, relative_to=run_dir,
    )
    n_portfolios = len(((overview or {}).get("portfolios")) or [])
    doc_entries = ((docs or {}).get("documents")) or []
    n_docs_total = len(doc_entries)
    n_in_window = 0
    for entry in doc_entries:
        d = _doc_create_date(entry)
        if d is None or (documents_since <= d <= documents_until):
            n_in_window += 1
    logger.info(
        "dry-run: would fetch %d portfolios + %d of %d documents "
        "(window %s..%s)",
        n_portfolios, n_in_window, n_docs_total,
        documents_since, documents_until,
    )
    if docs is not None:
        manifest.documents()["count_in_index"] = n_docs_total
        manifest.documents()["count_in_window"] = n_in_window
        manifest.documents()["count_outside_window"] = n_docs_total - n_in_window
        manifest.flush()
    return 0 if (overview is not None and docs is not None) else 1


def do_download(args: argparse.Namespace) -> int:
    # Translate the one window into relevate's year granularity for
    # /deposits, and keep the exact dates for the client-side document
    # filter: /middlelayer/v2/documents returns the full index on every
    # call, so fetch_documents() applies the window itself to avoid the
    # PDF-binary fetches for entries outside it.
    since, until = cli.resolve_lookback(args)
    year_from, year_to = since.year, until.year

    try:
        sess, state_minted_at = new_session_from_state(args.state_path)
    except FileNotFoundError as exc:
        logger.error("%s", exc)
        return 64

    if not probe_session_alive(sess):
        logger.error(
            "session probe failed — run `./relevate login` "
            "to mint a fresh session.",
        )
        return 1
    session = sess  # restore local name for the rest of the function

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")

    if args.dry_run:
        # Root CLAUDE.md §2: `download --dry-run` must export NOTHING.
        # Run the read-only walk (verify the session, enumerate the
        # investment-overview + document index, log the plan/counts) but
        # point the run dir at a throwaway temp dir instead of a bronze
        # one. TemporaryDirectory removes it on exit — even on a crash,
        # via the context manager — so nothing is ever persisted under
        # --bronze-dir and `load` never picks up a dry-run shell. The walk +
        # manifest code is otherwise identical to a real run; only the
        # output target differs.
        with tempfile.TemporaryDirectory(prefix="relevate-dryrun-") as scratch:
            run_dir = Path(scratch) / ts
            run_dir.mkdir(parents=True, exist_ok=True)
            logger.info("dry-run: nothing written to bronze")
            manifest = Manifest(run_dir, mode=args.mode, dry_run=True)
            manifest.set_state_minted_at(state_minted_at)
            manifest.set_windows(since=since, until=until,
                                 documents_since=since,
                                 documents_until=until)
            rc = do_dry_run(session, run_dir, manifest,
                            documents_since=since,
                            documents_until=until)
            manifest.finish(status="dry-run")
            return rc

    run_dir = args.bronze_dir / ts
    run_dir.mkdir(parents=True, exist_ok=True)
    logger.info("bronze run dir: %s", run_dir)

    manifest = Manifest(run_dir, mode=args.mode, dry_run=args.dry_run)
    manifest.set_state_minted_at(state_minted_at)
    manifest.set_windows(since=since, until=until,
                         documents_since=since,
                         documents_until=until)

    # One trace for the whole walk. Scoped to the phases below rather than
    # the session probe above: the probe reports its own verdict and returns
    # before any run dir exists to capture into.
    trace = debugcap.HttpTrace(run_dir, log=logger, enabled=args.debug)

    overview = None
    if args.mode in ("all", "accounts", "portfolios"):
        overview = fetch_accounts_phase(
            session, run_dir, manifest,
            skip_ancillary=(args.mode != "all"),
            trace=trace,
        )
        if overview is None:
            # The master enumeration failed, so no portfolio/document
            # work could run: mark the dump incomplete (prune reclaims
            # it, load skips it) rather than leaving it "in-progress".
            manifest.finish(status="incomplete")
            return 1

    if args.mode in ("all", "portfolios"):
        portfolios = (overview or {}).get("portfolios") or []
        if args.limit_portfolios is not None:
            portfolios = portfolios[:args.limit_portfolios]
            logger.info("--limit-portfolios %d", args.limit_portfolios)
        for portfolio in portfolios:
            fetch_portfolio(
                session, portfolio, run_dir, manifest,
                year_from=year_from,
                year_to=year_to,
                trace=trace,
            )

    if args.mode in ("all", "documents") and not args.no_documents:
        fetch_documents(
            session, run_dir, manifest,
            limit=args.limit_documents,
            documents_since=since,
            documents_until=until,
            force=args.documents_force,
            trace=trace,
        )

    manifest.finish(status="complete")
    n_errors = len(manifest.data["errors"])
    logger.info(
        "done. files=%d errors=%d  bronze=%s",
        len(manifest.data["files"]), n_errors, run_dir,
    )
    return 0 if n_errors == 0 else 2


# ----------------------------------------------------------------------
# CLI
# ----------------------------------------------------------------------


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="REST bronze dump from /middlelayer/v2/.",
    )
    p.add_argument(
        "--state-path", type=Path, default=DEFAULT_STATE_PATH,
        help="Persisted Airlock cookie jar (default: %(default)s).",
    )
    p.add_argument(
        "--bronze-dir", type=Path, default=DEFAULT_BRONZE_DIR,
        help=(
            "A UTC-timestamped run dir is created here per invocation "
            "(default: %(default)s, mounted from "
            "$XDG_DATA_HOME/wealthdb/relevate)."
        ),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=(
            "Hit only the master listing endpoints to enumerate "
            "work + verify session; no per-portfolio or per-doc "
            "fetches. Allowed without explicit user permission."
        ),
    )
    p.add_argument(
        "--mode",
        choices=("all", "accounts", "portfolios", "documents"),
        default="all",
        help=(
            "Restrict the run to one phase. 'accounts' = master "
            "listing + ancillaries only. 'portfolios' = listing + "
            "per-portfolio endpoints. 'documents' = doc index + "
            "PDFs. Use 'all' for everything (default)."
        ),
    )
    p.add_argument(
        "--no-documents", dest="no_documents", action="store_true",
        help="Skip document PDFs (fetched by default in --mode all).",
    )
    p.add_argument(
        "--documents-force", action="store_true",
        help=(
            "Bypass the document download-avoidance index: fetch every PDF "
            "even when a byte-identical copy exists in a prior complete "
            "bronze run (no hardlink reuse). Use to re-establish ground "
            "truth or as a first-run confidence check (run once with, once "
            "without, and diff silver under `load --force`)."
        ),
    )
    # The one window flag drives every facet. relevate's /deposits
    # endpoint takes year granularity only, so the window's start year
    # bounds the iteration; documents are filtered client-side, since
    # the /middlelayer/v2/documents listing has no server-side date
    # filter (do_download applies it to each entry's createDate in
    # fetch_documents() — see DESIGN §11). There is deliberately no
    # explicit-year escape hatch: a second window knob is what let a
    # bare year silently override the resolved window.
    cli.add_standard_args(p, verb="download")
    p.add_argument(
        "--limit-portfolios", type=int, default=None,
        help="Process at most N portfolios. For iteration.",
    )
    p.add_argument(
        "--limit-documents", type=int, default=None,
        help="Fetch at most N documents. For iteration.",
    )
    p.add_argument(
        "--debug", action="store_true",
        help=("Capture debug artefacts into the bronze run dir (default "
              "off): an HTTP trace at <run>/screenshots/http-trace.jsonl, "
              "one line per Relevate request with its status, timing, size "
              "and rate-limit headers — the shape of the exchange, which "
              "the saved bodies beside it do not record. Bodies are not "
              "duplicated and no credential is written. `prune` reclaims "
              "the trace; `load` never reads it. A --dry-run writes into a "
              "throwaway dir, so it captures nothing."),
    )
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    return do_download(args)


if __name__ == "__main__":
    raise SystemExit(main())
