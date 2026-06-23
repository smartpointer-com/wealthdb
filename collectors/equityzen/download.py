#!/usr/bin/env python3
"""equityzen bronze fetcher.

Drives the EquityZen buyer-portal SPA with the session minted by login.py
and captures the investor GraphQL responses as raw bronze.

  * getBuyerInvestments (per stage) — the offerings list. The op is keyed by
    a `stage` variable (ONGOING / CLOSED / EXITED) behind the portfolio's
    Ant-Design tabs; each stage returns its whole set in one response (no
    pagination). One node per offering: deal, fund=SPV, company (with
    assetClass = ASSET_COMPANY | ASSET_MULTI_COMPANY_FUND), investmentSize
    (basis), primaryTransaction (fees), buyerStageInfo (status).
  * getMyInvestmentDetails (per offering) — basis, status, share counts
    (primaryTransaction.sharesRemainingPostSplit), and the cash-flow ledger
    (primaryTransaction + distributedTransactions + transfers). This
    holdings surface carries everything valuation needs, including the
    reliable CLOSED-deal prices (your purchase + your tenders). See the
    "Why no /equity/ capture" note in DESIGN.md §4.
  * document blobs (with --documents) — each offering's PDFs (capital-
    account statements, K-1s, etc.) fetched via node.documents[].downloadUrl
    through the authenticated request API. load.py parses the capital-account
    statements (fund NAV) and K-1s (tax-basis capital); see statements.py.

The numeric `<N>` in /portfolio/<N>/ is decoded from
deal.id = base64("DealNode:<N>").

Output layout (collectorkit.bronze; deal-slug = sha256(deal.id)[:16] so no
company name appears in a path; doc files named sha256(doc id) so the Relay
id is filesystem-safe):
    $XDG_DATA_HOME/wealthdb/equityzen/<UTC-ts>/
      investments.json                     {stage: getBuyerInvestments body}
      offerings/<deal-slug>/detail.json    getMyInvestmentDetails
      documents/<deal-slug>/<doc-slug>.pdf document blobs (with --documents)
      run.json                             manifest for the silver loader

Read-only (CLAUDE.md): only the read queries above are issued — never a
write mutation (no createPVR / IOI / order / reserve / sell). EquityZen is a
live marketplace; never click an Invest / Express-Deal / Sell / Accept /
Fund / e-sign / confirm control.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import logging
import sys
import time
from pathlib import Path

from collectorkit import bronze, cli, envfile

from login import FIREFOX_PREFS, _authenticated

log = logging.getLogger("equityzen.download")

BASE = "https://equityzen.com"
GRAPHQL_PATH = "/api/graphql"
PORTFOLIO_URL = f"{BASE}/portfolio/"

DEFAULT_PROFILE_DIR = Path("/secrets/equityzen-profile")
DEFAULT_ENV_FILE = Path("/secrets/equityzen.env")
DEFAULT_DEST = Path("/data")

# Document metadata (id / type / downloadUrl) rides on getMyInvestmentDetails
# (node.documents[]), so no separate document-centre query is needed.
WANTED_OPS = {"getBuyerInvestments", "getMyInvestmentDetails"}


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--dest", type=Path, default=DEFAULT_DEST,
                   help="Bronze root; a UTC-timestamped run dir is created beneath it. Default: %(default)s.")
    p.add_argument("--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
                   help="Persistent browser profile from login.py. Default: %(default)s.")
    p.add_argument("--env-file", type=Path, default=DEFAULT_ENV_FILE,
                   help="Bash-sourced env file. Default: %(default)s.")
    p.add_argument("--documents", action="store_true",
                   help="Also fetch each offering's document PDF blobs (capital-account "
                        "statements, K-1s, etc.) via downloadUrl into bronze documents/. "
                        "Heavy (~200 PDFs); off by default.")
    p.add_argument("--dry-run", action="store_true",
                   help="Capture the offerings list (all stages) and log what would be fetched, "
                        "but visit no per-offering pages and write no bronze. Read-only smoke test.")
    p.add_argument("--timeout", type=int, default=45,
                   help="Per-page GraphQL-capture timeout in seconds. Default: %(default)s.")
    cli.add_common_args(p)
    return p.parse_args(argv)


def _deal_numeric(deal_id_b64: str) -> str | None:
    """deal.id = base64("DealNode:<N>"); return <N> or None."""
    try:
        raw = base64.b64decode(deal_id_b64 + "==").decode("utf-8")
    except Exception:
        return None
    if ":" not in raw:
        return None
    n = raw.split(":", 1)[1]
    return n if n.isdigit() else None


def _slug(s: str) -> str:
    return hashlib.sha256(s.encode("utf-8")).hexdigest()[:16]


def _deal_ids_from(body: dict) -> list[str]:
    edges = (((body or {}).get("data") or {}).get("buyer") or {}) \
        .get("buyerDeals", {}).get("edges") or []
    out = []
    for e in edges:
        did = (((e or {}).get("node") or {}).get("deal") or {}).get("id")
        if did:
            out.append(did)
    return out


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    timeout_ms = args.timeout * 1000

    envfile.source_env_file(args.env_file)  # aligns mounts; no creds used here
    if not args.dry_run:
        bronze.ensure_writable_dir(args.dest)

    from camoufox.sync_api import Camoufox

    captures: dict[str, list] = {}

    def on_response(resp) -> None:
        try:
            if GRAPHQL_PATH not in resp.url:
                return
            reqs = json.loads(resp.request.post_data or "")
        except Exception:
            return
        items = reqs if isinstance(reqs, list) else [reqs]
        if not any(isinstance(it, dict) and it.get("operationName") in WANTED_OPS
                   for it in items):
            return
        try:
            body = resp.json()
        except Exception:
            body = None
        for it in items:
            if isinstance(it, dict) and it.get("operationName") in WANTED_OPS:
                captures.setdefault(it["operationName"], []).append(
                    {"variables": it.get("variables") or {}, "body": body})

    def wait_capture(page, op, match=None, wait_ms=None):
        deadline = time.monotonic() + (wait_ms or timeout_ms) / 1000
        while time.monotonic() < deadline:
            for c in captures.get(op, []):
                if match is None or match(c["variables"]):
                    return c
            page.wait_for_timeout(250)
        return None

    snapshot_at = int(time.time())
    run = bronze.run_dir(args.dest, bronze.ts_slug())

    with Camoufox(
        persistent_context=True, user_data_dir=str(args.profile_dir),
        os="macos", window=(1280, 800), headless=False, humanize=True,
        geoip=True, firefox_user_prefs=FIREFOX_PREFS,
    ) as context:
        page = context.new_page()
        page.set_default_timeout(timeout_ms)
        page.on("response", on_response)

        # 1) Offerings list, per stage (Ant tabs). Authenticates the run too.
        log.info("loading portfolio list")
        page.goto(PORTFOLIO_URL, wait_until="domcontentloaded")
        page.wait_for_timeout(1500)
        if not _authenticated(page):
            raise SystemExit("Not authenticated — run `./equityzen login` first.")
        stage_bodies: dict[str, dict] = {}
        deal_ids: list[str] = []
        for stage, label in (("ONGOING", "Ongoing"), ("CLOSED", "Closed"), ("EXITED", "Exited")):
            tab = page.locator(f".ant-tabs-tab-btn:has-text('{label}')").first
            clicked = False
            try:
                if tab.count() and tab.is_visible():
                    tab.click(); clicked = True
            except Exception:
                pass
            cap = wait_capture(page, "getBuyerInvestments",
                               match=lambda v, s=stage: v.get("stage") == s,
                               wait_ms=(timeout_ms if clicked else 5000))
            if cap is None:
                log.info("stage %-7s: no tab / no data", stage); continue
            stage_bodies[stage] = cap["body"]
            ids = _deal_ids_from(cap["body"])
            log.info("stage %-7s: %d investment(s)", stage, len(ids))
            for did in ids:
                if did not in deal_ids:
                    deal_ids.append(did)
        if not stage_bodies:
            raise SystemExit("No getBuyerInvestments responses captured — re-run `./equityzen explore`.")
        log.info("total: %d distinct investment(s)", len(deal_ids))

        if args.dry_run:
            for did in deal_ids:
                log.info("  would fetch offering %s (deal #%s)", _slug(did), _deal_numeric(did) or "?")
            log.info("--dry-run: no bronze written")
            return 0

        bronze.atomic_write_json(run / "investments.json", stage_bodies)

        # Authenticated blob fetch for a document's downloadUrl → bronze.
        # The blob is named by sha256(doc id) so the Relay id (which has /, +)
        # is filesystem-safe; load.py recomputes the same name to find it.
        def fetch_blob(url, dest_no_ext):
            if not url:
                return None
            full = url if url.startswith("http") else BASE + url
            try:
                resp = context.request.get(full, timeout=timeout_ms)
                if not resp.ok:
                    return None
                body = resp.body()
                ct = resp.headers.get("content-type", "").lower()
                ext = "pdf" if "pdf" in ct else ("zip" if "zip" in ct else "bin")
                path = dest_no_ext.with_suffix("." + ext)
                bronze.atomic_write_bytes(path, body)
                return path
            except Exception as exc:
                log.debug("blob fetch failed for %s: %r", full, exc)
                return None

        # 2) Per-offering detail (positions + cash flows) + document blobs.
        offerings_meta = []
        document_blobs = 0
        for did in deal_ids:
            slug = _slug(did); n = _deal_numeric(did)
            if not n:
                log.warning("offering %s: deal id did not decode; skipping detail", slug)
                offerings_meta.append({"slug": slug, "detail": False}); continue
            log.info("fetching offering %s (deal #%s)", slug, n)
            page.goto(f"{BASE}/portfolio/{n}/", wait_until="domcontentloaded")
            cap = wait_capture(page, "getMyInvestmentDetails",
                               match=lambda v, did=did: v.get("dealId") == did)
            if cap is None:
                log.warning("offering %s: getMyInvestmentDetails did not fire; skipping", slug)
                offerings_meta.append({"slug": slug, "detail": False}); continue
            bronze.atomic_write_json(run / "offerings" / slug / "detail.json", cap["body"])
            n_blobs = 0
            if args.documents:
                edges = (((cap["body"] or {}).get("data") or {}).get("buyer") or {}) \
                    .get("buyerDeals", {}).get("edges") or []
                node = edges[0].get("node") if edges else {}
                docs = [d for d in (node.get("documents") or []) if d.get("id")]
                for doc in docs:
                    if fetch_blob(doc.get("downloadUrl"),
                                  run / "documents" / slug / _slug(doc["id"])):
                        n_blobs += 1
                document_blobs += n_blobs
                log.info("  offering %s: fetched %d/%d document blob(s)", slug, n_blobs, len(docs))
            offerings_meta.append({"slug": slug, "detail": True, "doc_blobs": n_blobs})

        # 4) Manifest — slugs + counts only.
        manifest = {
            "source": "equityzen", "snapshot_at": snapshot_at,
            "scope": "offerings+positions+cash_flows" + ("+documents" if args.documents else ""),
            "stage_counts": {s: len(_deal_ids_from(b)) for s, b in stage_bodies.items()},
            "investments": len(deal_ids), "offerings": offerings_meta,
            "document_blobs": document_blobs,
        }
        bronze.atomic_write_json(run / "run.json", manifest)
        log.info("wrote bronze to %s (%d/%d offerings with detail)",
                 run, sum(1 for o in offerings_meta if o["detail"]), len(offerings_meta))
        return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
