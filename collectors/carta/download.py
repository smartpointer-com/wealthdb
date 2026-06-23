#!/usr/bin/env python3
"""carta bronze fetcher (read-only) — authenticated REST/JSON walk.

Reuses the Camoufox session minted by login.py (the persistent profile dir),
lands in the app to establish the Cloudflare-clearance + session cookies,
then replays Carta's internal holder REST API with the browser context's
authenticated request API and writes every raw response to a UTC-timestamped
bronze run dir. All reads (GET); never a write — even the documents archive
is pulled per-PDF via each document's own download URL, so the bulk-download
POST is not used.

Discovery chain (all ids learned at runtime — none hardcoded):
  1. Land on app.carta.com → final URL carries the individual id (IID):
     /investors/individual/<IID>/portfolio/...
  2. navigation-config → organizationPk = firm id (fallback: account-switcher
     accounts[].id = "organization_pk:<firm>").
  3. list_individual_portfolio_investments(firm, IID) → the held entities,
     each {corporation_id, legal_name, is_fund_investment, entity_type}.

Per entity (corporation_id):
  * cap-table side (always attempted): holdings-dashboard + one list per
    security type (shares / options / rsu / rsa / warrants / convertibles /
    sar / piu / equity-grants) + overview-captable summary + post-money
    list; then per option grant with vesting, the vesting-data endpoint.
  * fund-LP side (when is_fund_investment): the fund entity tabs, the
    fund's look-through company list, active capital calls, and the
    fund-admin partner metrics (best-effort — some need uuids and may 404).

Documents (the archive — covers K-1, 1042-S, capital-account statements,
quarterly financials, capital-call / distribution notices):
  * get-all-received-documents (paginated) → one metadata row per PDF.
  * each row's document_url GET → JSON { url: signed-CDN }; that url GET →
    the PDF bytes, saved by document id.

Output layout (collectorkit.bronze; ids appear only in the gitignored
bronze tree, never the repo):

    $XDG_DATA_HOME/wealthdb/carta/<UTC-ts>/
      run.json                          manifest (ids, entities, counts, errors)
      bootstrap/{navigation-config,account-switcher,investments}.json
      entities/<corp|fund>_<id>/
        meta.json                       {corporation_id, legal_name, is_fund_investment, ...}
        holdings-dashboard.json shares.json options.json rsu.json rsa.json
        warrants.json convertibles.json sar.json piu.json equity-grants.json
        overview-captable-summary.json post-money-list.json
        vesting/grant_<id>.json
        fund-tabs.json fund-companies.json fund-cap-calls.json
        fund-admin/{app-init,partner-metrics}.json
      documents/
        index.json                      merged get-all-received-documents
        doc_<id>.pdf                    one per document_url

--dry-run does bootstrap + the investments list, logs the work-list, and
writes nothing. Per-endpoint failures are recorded in run.json and never
abort the run.

Read-only (CLAUDE.md): GET endpoints on the portfolio holder's own holdings only. Never
the issuer/company-admin or fund-admin GP console, never an exercise / sell /
transfer / accept action.
"""
from __future__ import annotations

import argparse
import json
import logging
import re
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import cli

log = logging.getLogger("carta.download")

APP = "https://app.carta.com"
FUND_ADMIN = "https://fund-admin.app.carta.com"
LOGIN_HOST = "login.app.carta.com"

DEFAULT_PROFILE_DIR = Path("/secrets/carta-profile")
DEFAULT_BRONZE_DIR = Path("/data")

# Per-corporation security-type lists, each returning {rows:[…], totals:{…}}.
# The holdings-dashboard summary is fetched separately (it isn't a
# {rows, totals} list).
SECURITY_TYPES = (
    "shares", "options", "rsu", "rsa", "warrants",
    "convertibles", "sar", "piu", "equity-grants",
)

# Browser-class headers so the Django views treat us like the SPA's XHRs.
API_HEADERS = {
    "X-Requested-With": "XMLHttpRequest",
    "Accept": "application/json, text/plain, */*",
    "Referer": f"{APP}/",
}

REQUEST_TIMEOUT_MS = 60_000


def slug_entity(entity: dict) -> str:
    """Readable, name-free dir slug for an entity: `fund_<id>` / `corp_<id>`.
    The numeric Carta id is a surrogate key (not a name / account number) and
    the bronze tree is gitignored, so this leaks nothing into the repo while
    staying greppable. Legal names live inside meta.json only."""
    eid = entity.get("corporation_id") or entity.get("entity_id") or "x"
    kind = "fund" if entity.get("is_fund_investment") else "corp"
    return f"{kind}_{eid}"


class Api:
    """Thin wrapper over the browser context's authenticated request API.
    Records every call (url, status) so the manifest can report coverage and
    failures without aborting the run on a single 404/403."""

    def __init__(self, context):
        self._rc = context.request
        self.errors: list[dict] = []

    def get(self, url: str, *, expect="json"):
        """GET `url` with the SPA headers. Returns (status, body) where body
        is parsed JSON (expect='json'), raw bytes (expect='bytes'), or None on
        a non-2xx / parse failure. Failures are logged + recorded, not raised."""
        try:
            resp = self._rc.get(url, headers=API_HEADERS,
                                 timeout=REQUEST_TIMEOUT_MS)
        except Exception as exc:  # network / browser error
            log.warning("GET %s failed: %s", _short(url), exc)
            self.errors.append({"url": url, "error": repr(exc)})
            return None, None
        if resp.status // 100 != 2:
            log.warning("GET %s -> HTTP %d", _short(url), resp.status)
            self.errors.append({"url": url, "status": resp.status})
            return resp.status, None
        if expect == "bytes":
            return resp.status, resp.body()
        try:
            return resp.status, resp.json()
        except Exception:
            # Some endpoints we treat as JSON return HTML on edge cases;
            # keep the text so the manifest/loader can see what happened.
            return resp.status, {"_non_json_text": resp.text()[:2000]}


def _short(url: str) -> str:
    """URL with the query string dropped — keeps logs free of tokens."""
    return url.split("?", 1)[0]


def write_json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(obj, indent=2, sort_keys=True, default=str),
                   encoding="utf-8")
    tmp.replace(path)


def write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_bytes(data)
    tmp.replace(path)


# --------------------------------------------------------------------------
# Discovery
# --------------------------------------------------------------------------

def land_and_get_individual_id(page) -> str:
    """Load the app and read the individual id from the settled URL. Raises if
    the session bounced to the Cloudflare-fronted login host."""
    page.goto(APP, wait_until="domcontentloaded", timeout=60_000)
    for _ in range(30):
        url = page.url
        if LOGIN_HOST in url or "/accounts/login" in url:
            raise RuntimeError(
                "session is not authenticated — run `./carta login` first")
        m = re.search(r"/investors/individual/(\d+)/", url)
        if m:
            return m.group(1)
        page.wait_for_timeout(500)
    raise RuntimeError(
        f"could not read the individual id from the landing URL ({page.url}); "
        f"the app routing may have changed — re-run `./carta explore`")


def discover_firm_id(api: Api, iid: str) -> tuple[str | None, dict, dict]:
    """Return (firm_id, navigation_config, account_switcher). firm_id is the
    org PK from navigation-config, falling back to account-switcher."""
    _, nav = api.get(f"{APP}/api/investors/portfolio/{iid}/navigation-config/")
    _, acct = api.get(f"{APP}/api/fe-platform/account-switcher/")
    firm_id = None
    if isinstance(nav, dict) and nav.get("organizationPk") is not None:
        firm_id = str(nav["organizationPk"])
    if firm_id is None and isinstance(acct, dict):
        for a in acct.get("accounts") or []:
            m = re.match(r"organization_pk:(\d+)", str(a.get("id", "")))
            if m:
                firm_id = m.group(1)
                break
    return firm_id, (nav or {}), (acct or {})


def list_investments(api: Api, firm_id: str, iid: str) -> list[dict]:
    """The held entities (corporations + fund investments)."""
    _, body = api.get(
        f"{APP}/api/investors/portfolio/firm/{firm_id}"
        f"/list_individual_portfolio_investments/{iid}/list/")
    if isinstance(body, list):
        return body
    if isinstance(body, dict) and isinstance(body.get("results"), list):
        return body["results"]
    return []


# --------------------------------------------------------------------------
# Per-entity capture
# --------------------------------------------------------------------------

def capture_exercises(api: Api, corp_id: str, grant_ids: set[str],
                      edir: Path) -> int:
    """For each option grant, read its modal HTML, enumerate the exercise
    requests, and download each one's exercise-detail (`edr`) attachment — an
    xlsx carrying the exercise date, shares, exercise price, and
    fair-market-value-on-exercise (the 409A at exercise). Saved under
    exercises/. Best-effort; records errors, never raises. Returns the number
    of xlsx written."""
    n = 0
    for gid in sorted(grant_ids):
        _, modal = api.get(f"{APP}/options/modal/?grant_pk={gid}")
        html = modal.get("html") if isinstance(modal, dict) else None
        if not html:
            continue
        erids = sorted(set(re.findall(r"exercise_request/(\d+)/attachment",
                                      html)))
        for erid in erids:
            _, data = api.get(
                f"{APP}/options/{gid}/exercise_request/{erid}"
                f"/attachment?file_type=edr", expect="bytes")
            if data and data[:2] == b"PK":   # xlsx is a zip archive
                write_bytes(edir / "exercises" / f"grant_{gid}_er_{erid}.xlsx",
                            data)
                n += 1
    if n:
        log.info("  %d exercise-detail xlsx", n)
    return n


def capture_captable(api: Api, iid: str, corp_id: str, edir: Path,
                     *, is_fund: bool = False) -> int:
    """Fetch the per-corporation holdings dashboard + every security-type
    list, then the vesting schedule for each grant that has one. Returns the
    number of grant vesting files written. is_fund skips post-money-list (a
    cap-table-only endpoint that 403s for fund entities)."""
    # Per-corporation summary (held_since / ownership / cost) — distinct from
    # the {rows, totals} security lists below.
    _, hd = api.get(
        f"{APP}/api/investors/holdings/portfolio/{iid}"
        f"/corporation/{corp_id}/holdings-dashboard/")
    if hd is not None:
        write_json(edir / "holdings-dashboard.json", hd)

    grant_ids: set[str] = set()
    for kind in SECURITY_TYPES:
        _, body = api.get(
            f"{APP}/api/investors/holdings/portfolio/{iid}"
            f"/corporation/{corp_id}/{kind}/")
        if body is None:
            continue
        write_json(edir / f"{kind}.json", body)
        # Collect grant ids (with vesting) from the option-bearing lists.
        if kind in ("options", "equity-grants", "rsu", "sar"):
            for row in (body.get("rows") if isinstance(body, dict) else None) or []:
                if row.get("has_vesting") and row.get("id") is not None:
                    grant_ids.add(str(row["id"]))

    if not is_fund:
        _, pml = api.get(f"{APP}/api/corporations/{corp_id}/post-money-list/")
        if pml is not None:
            write_json(edir / "post-money-list.json", pml)

    n_vest = 0
    for gid in sorted(grant_ids):
        _, vest = api.get(
            f"{APP}/api/corporations/{corp_id}/option_grant/{gid}/vesting-data/")
        if vest is not None:
            write_json(edir / "vesting" / f"grant_{gid}.json", vest)
            n_vest += 1
    if not is_fund:
        capture_exercises(api, corp_id, grant_ids, edir)
    return n_vest


def capture_captable_summary(api: Api, firm_id: str, entity_id: str,
                             edir: Path) -> None:
    _, summary = api.get(
        f"{APP}/api/investors/firm/{firm_id}/entity/{entity_id}"
        f"/overview-captable/summary")
    if summary is not None:
        write_json(edir / "overview-captable-summary.json", summary)


def capture_fund(api: Api, iid: str, entity_id: str, edir: Path) -> None:
    """Fund-LP structured capture. Best-effort: the fund-admin metrics need
    org/fund uuids that we only partly discover, so missing pieces are
    recorded (in api.errors) rather than fatal. The fund's substance —
    statements, K-1s, capital calls — is captured comprehensively as
    documents elsewhere."""
    _, tabs = api.get(
        f"{APP}/api/investors/portfolio/fund/{iid}/entity/{entity_id}/tabs/")
    if tabs is not None:
        write_json(edir / "fund-tabs.json", tabs)

    _, companies = api.get(f"{APP}/api/investors/portfolio/fund/{iid}/list/")
    if companies is not None:
        write_json(edir / "fund-companies.json", companies)

    _, calls = api.get(
        f"{APP}/api/investors/portfolio/{iid}/list/individual_lp_active_cap_calls/")
    if calls is not None:
        write_json(edir / "fund-cap-calls.json", calls)

    # fund-admin partner dashboard bootstrap → portfolio_uuid → partner metrics.
    _, init = api.get(f"{FUND_ADMIN}/partner-portfolios/{iid}/app/init")
    if init is not None:
        write_json(edir / "fund-admin" / "app-init.json", init)
        puuid = init.get("portfolio_uuid") if isinstance(init, dict) else None
        if puuid:
            _, metrics = api.get(f"{FUND_ADMIN}/v2/partners/{puuid}/metrics/")
            if metrics is not None:
                write_json(edir / "fund-admin" / "partner-metrics.json", metrics)


# --------------------------------------------------------------------------
# Documents
# --------------------------------------------------------------------------

def capture_documents(api: Api, iid: str, docs_dir: Path) -> tuple[int, int]:
    """Fetch the received-documents index (all pages) and download each PDF by
    its document_url. Returns (n_index, n_pdf). Idempotent within a run:
    a PDF already on disk is not re-fetched."""
    all_rows: list[dict] = []
    page_no = 1
    while True:
        _, body = api.get(
            f"{APP}/api/investors/individual/{iid}/get-all-received-documents/"
            f"?page={page_no}")
        if not isinstance(body, dict):
            break
        rows = body.get("results") or []
        all_rows.extend(rows)
        if not body.get("has_next"):
            break
        page_no += 1
        if page_no > 100:  # safety bound
            log.warning("documents pagination exceeded 100 pages; stopping")
            break

    write_json(docs_dir / "index.json",
               {"count": len(all_rows), "results": all_rows})

    n_pdf = 0
    for row in all_rows:
        url = row.get("document_url")
        doc_id = row.get("id") or row.get("uuid")
        if not url or doc_id is None:
            continue
        if not url.startswith("http"):
            url = APP + url if url.startswith("/") else f"{APP}/{url}"
        out = docs_dir / f"doc_{doc_id}.pdf"
        if out.exists():
            n_pdf += 1
            continue
        # document_url returns a JSON envelope { "url": <signed CDN url> },
        # NOT the PDF bytes — follow it to documents.carta.com for the binary.
        _, env = api.get(url)
        signed = env.get("url") if isinstance(env, dict) else None
        if not signed:
            log.warning("doc %s: no signed url in envelope; skipping", doc_id)
            continue
        _, data = api.get(signed, expect="bytes")
        if data and data[:1] not in (b"{", b"<"):   # guard vs JSON/HTML error
            write_bytes(out, data)
            n_pdf += 1
        else:
            log.warning("doc %s: signed-url fetch empty / not a binary", doc_id)
    log.info("documents: %d indexed, %d PDF(s) on disk", len(all_rows), n_pdf)
    return len(all_rows), n_pdf


# --------------------------------------------------------------------------
# Driver
# --------------------------------------------------------------------------

def run(context, args, run_dir: Path, snapshot_at: int) -> int:
    page = context.new_page()
    api = Api(context)

    iid = land_and_get_individual_id(page)
    log.info("individual id discovered")
    firm_id, nav, acct = discover_firm_id(api, iid)
    if not firm_id:
        raise RuntimeError(
            "could not determine the firm/organization id from "
            "navigation-config or account-switcher; re-run `./carta explore`")
    investments = list_investments(api, firm_id, iid)
    log.info("found %d investment entit(ies)", len(investments))

    if args.dry_run:
        for e in investments:
            log.info("  entity %s fund=%s",
                     slug_entity(e), bool(e.get("is_fund_investment")))
        log.info("(dry-run) bootstrap + investments verified; nothing written. "
                 "Skipping holdings, fund, documents.")
        return 0

    write_json(run_dir / "bootstrap" / "navigation-config.json", nav)
    write_json(run_dir / "bootstrap" / "account-switcher.json", acct)
    write_json(run_dir / "bootstrap" / "investments.json", investments)

    for e in investments:
        edir = run_dir / "entities" / slug_entity(e)
        write_json(edir / "meta.json", e)
        corp_id = e.get("corporation_id")
        is_fund = bool(e.get("is_fund_investment"))
        log.info("→ entity %s", slug_entity(e))
        if corp_id is None:
            continue
        # Holdings dashboard + security-type lists. Works for both families;
        # a fund just returns empty security lists. is_fund skips the
        # cap-table-only endpoints (post-money / captable summary) that 403
        # for funds.
        n_vest = capture_captable(api, iid, str(corp_id), edir, is_fund=is_fund)
        if n_vest:
            log.info("  %d grant vesting schedule(s)", n_vest)
        if is_fund:
            capture_fund(api, iid, str(corp_id), edir)
        else:
            capture_captable_summary(api, firm_id, str(corp_id), edir)

    n_docs, n_pdf = capture_documents(api, iid, run_dir / "documents")

    write_json(run_dir / "run.json", {
        "schema": 1,
        "snapshot_at": snapshot_at,
        "utc": run_dir.name,
        "dry_run": args.dry_run,
        "individual_id": iid,
        "firm_id": firm_id,
        "entities": [
            {"slug": slug_entity(e),
             "corporation_id": e.get("corporation_id"),
             "is_fund_investment": bool(e.get("is_fund_investment")),
             "entity_type": e.get("entity_type")}
            for e in investments
        ],
        "documents": {"indexed": n_docs, "pdf_on_disk": n_pdf},
        "errors": api.errors,
    })
    log.info("done → %s (%d entit(ies), %d doc(s), %d endpoint error(s))",
             run_dir, len(investments), n_docs, len(api.errors))
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help="Persistent Camoufox profile from login.py. Default: %(default)s.",
    )
    p.add_argument(
        "--dest", type=Path, default=DEFAULT_BRONZE_DIR,
        help=("Bronze root; a UTC-timestamped run dir is created beneath it. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Bootstrap + list investments only; write just run_dir/bootstrap "
              "and log the work-list. No holdings/fund/document fetch."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = args.dest / ts
    if not args.dry_run:
        run_dir.mkdir(parents=True, exist_ok=True)
    snapshot_at = int(time.time())

    from camoufox.sync_api import Camoufox

    with Camoufox(
        persistent_context=True,
        user_data_dir=str(args.profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,   # headed under the entrypoint's Xvfb; clears Cloudflare
        humanize=True,
        geoip=True,
    ) as context:
        try:
            return run(context, args, run_dir, snapshot_at)
        except RuntimeError as exc:
            log.error("%s", exc)
            return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
