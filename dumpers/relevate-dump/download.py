#!/usr/bin/env python3
"""
Phase 3 — REST dump for Relevate (portal.pens-expert.ch).

Replays the `/middlelayer/v2/` REST surface from a persisted
session (cookies established by `login.py`) and lands JSON + PDF
artefacts in a versioned bronze tree:

    /data/<UTC-ts>/
    ├── run.json
    ├── accounts/
    │   ├── investment-overview.json
    │   ├── services.json
    │   ├── compliance-products.json
    │   ├── contact-messages.json
    │   ├── sso-claims.json
    │   └── …
    ├── portfolios/
    │   └── <sha256(externalId)[:16]>/
    │       ├── deposits-YYYY.json   (one per year from firstInvestmentDate→now)
    │       ├── performance.json
    │       ├── fees.json
    │       ├── investment-allocation.json
    │       └── modelportfolio.json
    └── documents/
        ├── index.json
        ├── <docId>.pdf
        └── <docId>.unexpected.<ext>   (only if API returned non-PDF)

Read-only: only GETs against the observed `/middlelayer/v2/`
endpoints. No write surfaces, no `/change` URLs, no mutation
flags accepted.

Iteration-cheap flags (per the user's standing feedback): use
--mode / --skip-documents / --limit-portfolios / --limit-documents
so iterating on one phase doesn't pay for the others. --dry-run
hits only the master listing endpoints (investment-overview +
documents) to enumerate work without fetching per-portfolio or
per-document payloads.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import requests
from requests.exceptions import RequestException

BASE = "https://portal.pens-expert.ch"
PROBE = f"{BASE}/auth/rest/protected/self-service/ui/configuration/portal"
DASHBOARD_REFERER = f"{BASE}/dashboard/"

DEFAULT_STATE_PATH = Path("/secrets/relevate-state.json")
DEFAULT_DEST = Path("/data")

USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)
SEC_CH_UA = (
    '"Not_A Brand";v="99", "Chromium";v="147", "Google Chrome";v="147"'
)

# Endpoints. Note the inconsistent casing of "Portfolio" vs
# "portfolio" — observed in the wire; do NOT normalise.
EP_INVESTMENT_OVERVIEW = "/middlelayer/v2/portfolio/investment-overview"
EP_DOCUMENTS_INDEX = "/middlelayer/v2/documents"
EP_SERVICES = "/middlelayer/v2/portfolio/services"
EP_COMPLIANCE_PRODUCTS = "/middlelayer/v2/compliance/products"
EP_CONTACT_MESSAGES = "/middlelayer/v2/contact/messages"
EP_CONTACT_NOTIFICATION = "/middlelayer/v2/contact/notification"
EP_CONTACT_LANGUAGE = "/middlelayer/v2/contact/language"
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


# Ancillary GETs that return useful 200/204 bodies. First real run
# revealed that /portfolio/services 400s without query params we
# can't synthesise, and /contact/language 405s on GET — dropped
# both. /contact/risk-protection returns 204 (no content) for
# accounts without protection set up; kept and handled specially
# in get_and_save_json so 204 is a non-error.
ANCILLARY_ACCOUNTS = (
    ("compliance-products.json", EP_COMPLIANCE_PRODUCTS),
    ("contact-messages.json", EP_CONTACT_MESSAGES),
    ("contact-notification.json", EP_CONTACT_NOTIFICATION),
    ("contact-risk-protection.json", EP_CONTACT_RISK_PROTECTION),
    ("sso-claims.json", EP_SSO_CLAIMS),
)

# Relevate uses 0001-01-01 as a sentinel for "unknown / never" on
# firstInvestmentDate. Don't try to parse that as a year.
SENTINEL_DATE_PREFIX = "0001-"


logger = logging.getLogger("download")


# ----------------------------------------------------------------------
# State + session
# ----------------------------------------------------------------------


def load_state(path: Path) -> dict[str, Any] | None:
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        logger.warning("could not load state %s: %s", path, exc)
        return None


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


def new_session_from_state(state_path: Path) -> requests.Session:
    state = load_state(state_path)
    if state is None:
        raise FileNotFoundError(
            f"state file missing: {state_path}. Run "
            "`./relevate-dump login` first.",
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
    session.relevate_state_minted_at = state.get("minted_at")  # type: ignore[attr-defined]
    return session


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


class Manifest:
    """
    Incremental run.json writer. Flush after every successful fetch
    so a Ctrl-C run still leaves an inspectable manifest.
    """

    def __init__(self, run_dir: Path, mode: str, dry_run: bool) -> None:
        self.path = run_dir / "run.json"
        self.data: dict[str, Any] = {
            "tool": "relevate-dump.download",
            "schema_version": 1,
            "started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "ended_at": None,
            "mode": mode,
            "dry_run": dry_run,
            "state_minted_at": None,
            "accounts": [],
            "documents": {
                "count_in_index": 0,
                "fetched": 0,
                "skipped": 0,
                "unexpected_content_type": [],
                "files": [],
            },
            "errors": [],
            "files": [],
        }

    def set_state_minted_at(self, ts: str | None) -> None:
        self.data["state_minted_at"] = ts
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

    def finish(self) -> None:
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


def get_and_save_json(
    session: requests.Session,
    endpoint: str,
    dest_path: Path,
    manifest: Manifest,
    *,
    relative_to: Path,
    timeout: int = 30,
) -> dict[str, Any] | None:
    """
    GET an endpoint expecting JSON, save the response body verbatim
    to dest_path, return the parsed body. On error, log + record in
    manifest.errors and return None. Idempotent at the dir level —
    the caller is responsible for not re-fetching if the file
    already exists.
    """
    url = BASE + endpoint
    try:
        resp = session.get(url, timeout=timeout, allow_redirects=False)
    except RequestException as exc:
        logger.error("GET %s: network failure: %s", endpoint, exc)
        manifest.add_error(
            endpoint=endpoint, status=None, message=str(exc),
        )
        return None
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
) -> str | None:
    """
    GET an endpoint expecting a binary body (e.g. PDF), save the
    response verbatim. Returns the actual content-type on success,
    None on failure. If the content-type differs from expected,
    save under <name>.unexpected.<ext> instead and record.
    """
    url = BASE + endpoint
    try:
        resp = session.get(url, timeout=timeout, allow_redirects=False)
    except RequestException as exc:
        logger.error("GET %s: network failure: %s", endpoint, exc)
        manifest.add_error(
            endpoint=endpoint, status=None, message=str(exc),
        )
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
                relative_to=run_dir,
            )

    return overview


def fetch_portfolio(
    session: requests.Session,
    portfolio: dict[str, Any],
    run_dir: Path,
    manifest: Manifest,
    *,
    year_from: int | None,
    year_to: int | None,
) -> None:
    """
    Fetch every observed per-portfolio endpoint into
    /data/<run>/portfolios/<slug>/.
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

    # Resolve the deposits-iteration policy:
    # - --year-from + --year-to override: iterate that explicit
    #   range.
    # - firstInvestmentDate is plausible (parses to year >= 1900,
    #   not the 0001-01-01 sentinel): iterate from that year to
    #   current.
    # - Otherwise (sentinel or unparseable): call once without
    #   ?year=. The first real run showed that /deposits returns
    #   the same empty {transactions:[]} envelope regardless of
    #   ?year=YYYY for these account types — transactions are
    #   exposed via credit-note PDFs, not this endpoint (see
    #   DESIGN §11). One call is enough to record "asked, empty".
    year_window: tuple[int, int] | None = None
    if year_from is not None:
        year_window = (
            year_from,
            year_to or datetime.now(timezone.utc).year,
        )
    elif (first_investment_date
          and not str(first_investment_date).startswith(SENTINEL_DATE_PREFIX)):
        try:
            yf = int(str(first_investment_date)[:4])
            if yf >= 1900:
                yt = year_to or datetime.now(timezone.utc).year
                year_window = (min(yf, yt), yt)
        except ValueError:
            pass

    logger.info(
        "portfolio id=%s slug=%s product=%s proposal=%s "
        "deposits=%s",
        pid, slug, product.get("key"), proposal_id,
        f"years {year_window[0]}-{year_window[1]}"
        if year_window else "single-call",
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

    if year_window:
        for year in range(year_window[0], year_window[1] + 1):
            get_and_save_json(
                session,
                ep_deposits(pid) + f"?year={year}",
                pdir / f"deposits-{year}.json",
                manifest,
                relative_to=run_dir,
            )
    else:
        get_and_save_json(
            session,
            ep_deposits(pid),
            pdir / "deposits.json",
            manifest,
            relative_to=run_dir,
        )

    # The four single-shot per-portfolio endpoints.
    get_and_save_json(
        session, ep_performance(pid),
        pdir / "performance.json", manifest, relative_to=run_dir,
    )
    get_and_save_json(
        session, ep_fees(pid),
        pdir / "fees.json", manifest, relative_to=run_dir,
    )
    get_and_save_json(
        session, ep_allocation(pid),
        pdir / "investment-allocation.json", manifest, relative_to=run_dir,
    )
    if proposal_id is not None:
        get_and_save_json(
            session, ep_modelportfolio(proposal_id),
            pdir / "modelportfolio.json", manifest, relative_to=run_dir,
        )
    else:
        logger.info(
            "portfolio id=%s has no portfolioProposalId; "
            "skipping modelportfolio",
            pid,
        )


def fetch_documents(
    session: requests.Session,
    run_dir: Path,
    manifest: Manifest,
    *,
    limit: int | None,
) -> None:
    docs_dir = run_dir / "documents"
    index = get_and_save_json(
        session, EP_DOCUMENTS_INDEX,
        docs_dir / "index.json", manifest, relative_to=run_dir,
    )
    if index is None:
        logger.error("documents index failed — skipping per-doc fetch")
        return
    docs = index.get("documents") or []
    manifest.documents()["count_in_index"] = len(docs)
    manifest.flush()
    logger.info("documents index: %d entries", len(docs))

    if limit is not None:
        docs = docs[:limit]
        logger.info("--limit-documents %d", limit)

    for entry in docs:
        doc_id = entry.get("id")
        if doc_id is None:
            continue
        target = docs_dir / f"{doc_id}.pdf"
        if target.exists():
            logger.debug("document %s already present; skipping", doc_id)
            manifest.documents()["skipped"] += 1
            manifest.flush()
            continue
        ct = get_and_save_binary(
            session, ep_document(doc_id),
            target, manifest, relative_to=run_dir,
            expected_content_type="application/pdf",
        )
        if ct == "application/pdf":
            manifest.documents()["fetched"] += 1
        elif ct is not None:
            manifest.documents()["unexpected_content_type"].append({
                "id": doc_id, "content_type": ct,
            })
        manifest.flush()


# ----------------------------------------------------------------------
# Orchestration
# ----------------------------------------------------------------------


def do_dry_run(
    session: requests.Session,
    run_dir: Path,
    manifest: Manifest,
) -> int:
    """
    --dry-run: hit the master listing endpoints (cheap),
    enumerate work, exit. No per-portfolio or per-document fetches.
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
    n_docs = len(((docs or {}).get("documents")) or [])
    logger.info(
        "dry-run: would fetch %d portfolios + %d documents",
        n_portfolios, n_docs,
    )
    if docs is not None:
        manifest.documents()["count_in_index"] = n_docs
        manifest.flush()
    return 0 if (overview is not None and docs is not None) else 1


def do_download(args: argparse.Namespace) -> int:
    try:
        session = new_session_from_state(args.state_path)
    except FileNotFoundError as exc:
        logger.error("%s", exc)
        return 64

    if not probe_session_alive(session):
        logger.error(
            "session probe failed — run `./relevate-dump login` "
            "to mint a fresh session.",
        )
        return 1

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = args.dest / ts
    run_dir.mkdir(parents=True, exist_ok=True)
    logger.info("bronze run dir: %s", run_dir)

    manifest = Manifest(run_dir, mode=args.mode, dry_run=args.dry_run)
    manifest.set_state_minted_at(getattr(session, "relevate_state_minted_at", None))

    if args.dry_run:
        rc = do_dry_run(session, run_dir, manifest)
        manifest.finish()
        return rc

    overview = None
    if args.mode in ("all", "accounts", "portfolios"):
        overview = fetch_accounts_phase(
            session, run_dir, manifest,
            skip_ancillary=(args.mode != "all"),
        )
        if overview is None:
            manifest.finish()
            return 1

    if args.mode in ("all", "portfolios"):
        portfolios = (overview or {}).get("portfolios") or []
        if args.limit_portfolios is not None:
            portfolios = portfolios[:args.limit_portfolios]
            logger.info("--limit-portfolios %d", args.limit_portfolios)
        for portfolio in portfolios:
            fetch_portfolio(
                session, portfolio, run_dir, manifest,
                year_from=args.year_from,
                year_to=args.year_to,
            )

    if args.mode in ("all", "documents") and not args.skip_documents:
        fetch_documents(
            session, run_dir, manifest,
            limit=args.limit_documents,
        )

    manifest.finish()
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
        description="Phase 3 — REST bronze dump from /middlelayer/v2/.",
    )
    p.add_argument(
        "--state-path", type=Path, default=DEFAULT_STATE_PATH,
        help="Persisted Airlock cookie jar (default: %(default)s).",
    )
    p.add_argument(
        "--dest", type=Path, default=DEFAULT_DEST,
        help=(
            "Parent dir for the per-run timestamped bronze dir "
            "(default: %(default)s, mounted from "
            "~/wealthdb/relevate)."
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
        "--skip-documents", action="store_true",
        help="Even in --mode all, skip document PDFs.",
    )
    p.add_argument(
        "--year-from", type=int, default=None,
        help=(
            "Earliest year for /deposits iteration. Defaults to "
            "each portfolio's firstInvestmentDate's year."
        ),
    )
    p.add_argument(
        "--year-to", type=int, default=None,
        help="Latest year for /deposits iteration. Default: current.",
    )
    p.add_argument(
        "--limit-portfolios", type=int, default=None,
        help="Process at most N portfolios. For iteration.",
    )
    p.add_argument(
        "--limit-documents", type=int, default=None,
        help="Fetch at most N documents. For iteration.",
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
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
