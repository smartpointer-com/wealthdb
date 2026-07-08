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

    Document fetches are download-avoidant via the shared
    ``collectorkit.docdedup`` engine, chosen per document class:
      - parsed / restatement-prone documents — capital-account statements,
        K-1s, and financial reports — can be re-issued/corrected under a stable
        Relay doc.id, so they are ALWAYS fetched and content-compared against
        the prior copy: an unchanged (byte-identical) one is hardlinked (disk
        reclaimed), a changed one keeps its fresh bytes (a re-issue is never
        missed). This is the correctness-safe default for anything load.py reads
        for figures;
      - executed-once legal / offering documents (an explicit, owner-confirmable
        allow-list) are immutable once signed and are not parsed, so an
        identical copy from a prior complete run is HARDLINKED into the new run
        dir and the fetch is skipped (any hardlink error falls through to a real
        fetch — a document degrades to a fetch, never to a miss);
      - any other / unrecognised document type is fetch-verified too (the
        safe default — always fetched, a byte-identical copy still deduped).
    Every run dir stays self-contained (a hardlink is a real in-run file), so
    the loader needs no cross-run fallback. --documents-force bypasses the
    index entirely.

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

from collectorkit import bronze, cli, docdedup, envfile

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
                        "Heavy (fetches every offering's document PDFs); off by default. "
                        "Download-avoidant: an executed-once legal/offering document "
                        "identical to a prior run is hardlinked in rather than re-fetched; "
                        "parsed / restatement-prone documents (statements, K-1s, reports) "
                        "are always fetched and content-compared (see collectorkit.docdedup).")
    p.add_argument("--documents-force", action="store_true",
                   help="Bypass the document download-avoidance index: fetch every blob "
                        "even when a byte-identical copy exists in a prior bronze run "
                        "(no hardlink reuse). Use to re-establish ground truth or as the "
                        "first-run confidence check (run once with, once without, diff "
                        "silver under `load --force`).")
    p.add_argument("--dry-run", action="store_true",
                   help="Capture the offerings list (all stages) and log what would be fetched, "
                        "but visit no per-offering pages and write no bronze. Read-only smoke test.")
    p.add_argument("--timeout", type=int, default=45,
                   help="Per-page GraphQL-capture timeout in seconds. Default: %(default)s.")
    p.add_argument("--debug", action="store_true",
                   help="Uniform debug gate: keep any debug artefact out of a bronze run "
                        "dir unless set. download.py writes none today — its diagnostics "
                        "live externally (`login --debug-dir` screenshots and `explore`'s "
                        "/debug HAR/trace/click log, never the bronze tree) — so this flag "
                        "currently gates nothing bronze-resident; it exists so the gate is "
                        "uniform across collectors and any future capture stays off by default.")
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


# --- documentType → docdedup class (mode selection) ------------------------
#
# The observed EquityZen `documentType` inventory (names only — the pinned
# vocabulary this mapping is built against):
#   CAPITAL_ACCOUNT_STATEMENT, QUARTERLY_REPORT, ANNUAL_FINANCIAL_STATEMENTS,
#   K1, COUNTERSIGN_SUB_AGT, SUB_AGT, SERIES_SCHEDULE, FUND_W_9, W_8,
#   SUITABILITY, SUMMARY_SHEET, TERMSHEET, OFFERING_DOC.
# Of these, only CAPITAL_ACCOUNT_STATEMENT and K1 are parsed by load.py for
# figures; the rest are archival only, so mis-linking a non-parsed type has no
# silver-correctness exposure.

# fetch-verify (always re-read + content-compare): parsed and/or
# restatement-prone documents. A capital-account statement or K-1 is parsed for
# NAV / tax-basis figures and can be RESTATED/CORRECTED under a stable Relay
# doc.id (Relay ids are entity ids, NOT content-addressed), so link-mode would
# serve a stale figure into the positions/NAV replay — a correctness bug.
# Financial reports can be restated the same way. fetch-verify always fetches,
# so a restatement is caught (CHANGED → keep fresh bytes) while a byte-identical
# unchanged copy is still hardlinked (disk dedup).
_FETCH_VERIFY_TYPES = frozenset({
    "CAPITAL_ACCOUNT_STATEMENT",
    "K1",
    "QUARTERLY_REPORT",
    "ANNUAL_FINANCIAL_STATEMENTS",
})

# link (fetch-avoidance): executed-once legal / offering documents that are
# immutable once signed/issued and are NOT parsed by load.py, so linking a prior
# identical copy realizes the fetch-avoidance win at zero silver-correctness
# risk. This allow-list is the OWNER-CONFIRMABLE set (Move 1 plan §8 doc-type
# inventory is the owner's call) — an explicit allow-list, never a default:
# anything not named here is fetched, not linked.
_LINK_TYPES = frozenset({
    "SUB_AGT",
    "COUNTERSIGN_SUB_AGT",
    "SERIES_SCHEDULE",
    "SUITABILITY",
    "SUMMARY_SHEET",
    "TERMSHEET",
    "OFFERING_DOC",
    "FUND_W_9",
    "W_8",
})


def _document_class(doc: dict) -> str | None:
    """Map an EquityZen document node to a docdedup class (mode selection).

    Parsed / restatement-prone types (capital-account statements, K-1s,
    financial reports; ``_FETCH_VERIFY_TYPES``) are `mutable`/`tax` →
    fetch-verify-dedup: always re-read and content-compare, because they can be
    re-issued/corrected under a stable Relay doc.id and link-mode would risk
    serving a superseded copy — a correctness bug. Executed-once legal / offering
    documents (``_LINK_TYPES``, an explicit owner-confirmable allow-list) are
    immutable and unparsed → `immutable` → link-mode (hardlink the prior
    identical copy, skip the fetch). Any other / unrecognised type is left
    unclassified → the helper fetch-verifies it (the safe default: always
    fetched, a byte-identical copy still deduped).
    """
    dtype = doc.get("documentType")
    if dtype == "K1":
        return docdedup.CLASS_TAX
    if dtype in _FETCH_VERIFY_TYPES:
        return docdedup.CLASS_MUTABLE
    if dtype in _LINK_TYPES:
        return docdedup.CLASS_IMMUTABLE
    return None


def extract_equityzen(run_dir, manifest):
    """docdedup extract hook (disk-driven): recover each prior run's document
    identities from disk. Key = (deal-slug, doc-slug) = the two path components
    under documents/, both sha256(id)[:16] of the Relay deal.id / doc.id, so the
    same logical document lands at the same path every run (multiplicity 1). No
    reliable pre-fetch issue date exists (period_end is parsed from the PDF only
    AFTER the fetch; tax_year is K-1-only), so doc_date is None and the
    freshness window does not apply. Enumerating on-disk files also means a key
    naturally counts only when its blob still exists (pruned bronze
    self-heals)."""
    docs_root = run_dir / "documents"
    if not docs_root.is_dir():
        return
    for deal_dir in docs_root.iterdir():
        if deal_dir.is_symlink() or not deal_dir.is_dir():
            continue
        for f in deal_dir.iterdir():
            if f.is_file() and not f.is_symlink():
                yield docdedup.DocRef(key=(deal_dir.name, f.stem),
                                      doc_date=None,
                                      relpath=str(f.relative_to(run_dir)))


# A document node that carries no downloadUrl yields no blob BY DESIGN — load
# records a null local_path for it, exactly as before. That is not a fetch
# failure, so it gets its own outcome and is NOT counted among `errors`.
NO_BLOB = "no-blob"

# docdedup outcome -> per-offering / top-level manifest counter (mirrors viac's
# {total, fetched, linked, skipped} audit block). EVERY outcome code the walk
# can produce is mapped explicitly; an unmapped/unexpected code is routed to
# `other` by _tally (never silently folded into `fetched`).
_STATUS_KEY = {
    docdedup.LINKED: "linked",       # legal doc: prior copy hardlinked, fetch avoided
    docdedup.FETCHED: "fetched",     # fresh bytes fetched and kept
    docdedup.VERIFIED: "verified",   # fetch-verify: byte-identical to prior, hardlinked (disk reclaimed)
    docdedup.CHANGED: "changed",     # fetch-verify: restated under a stable id, fresh bytes kept
    docdedup.FETCH_FAILED: "errors",  # a real fetch failure (URL present, GET failed)
    NO_BLOB: "no_blob",              # no downloadUrl → no blob by design (not an error)
}


def _empty_doc_counts() -> dict:
    return {"total": 0, "fetched": 0, "linked": 0, "verified": 0,
            "changed": 0, "errors": 0, "no_blob": 0, "other": 0}


def _tally(counts: dict, status: str) -> None:
    bucket = _STATUS_KEY.get(status)
    if bucket is None:
        # An outcome code the map does not know is a programming error, not a
        # 'fetched' document — surface it in its own bucket rather than inflate
        # the fetch count (and warn so it is not lost).
        log.warning("unmapped docdedup outcome %r; counting as 'other'", status)
        bucket = "other"
    counts[bucket] += 1


def _process_document(skip, *, deal_slug: str, doc: dict, target_dir: Path,
                      fetch_blob, force: bool = False) -> str:
    """Decide and execute one document's download-avoidance outcome, returning a
    status code for :func:`_tally`.

    A node with no ``downloadUrl`` was never going to yield a blob, so it returns
    :data:`NO_BLOB` (a null-blob-by-design — load records a null local_path for
    it, exactly as before — NOT a fetch failure) without touching the
    fetch/dedup path. Otherwise it dispatches to :func:`docdedup.process` by the
    document's class: link-mode for the executed-once legal/offering docs,
    fetch-verify for the parsed/restatement-prone docs, plain fetch for anything
    unclassified. ``force`` bypasses the download-avoidance index. The fetch
    closure is the collector's own ``fetch_blob``, so any hardlink failure falls
    straight through to the real fetch."""
    doc_slug = _slug(doc["id"])
    if not doc.get("downloadUrl"):
        return NO_BLOB
    return docdedup.process(
        skip, key=(deal_slug, doc_slug), doc_class=_document_class(doc),
        target_dir=target_dir, stem=doc_slug,
        fetch=(lambda: fetch_blob(doc.get("downloadUrl"), target_dir / doc_slug)),
        force=force)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    if args.debug:
        log.info("--debug: download writes no bronze-resident debug artefacts; "
                 "external diagnostics live under `login --debug-dir` and "
                 "`explore`'s /debug.")
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

        # First on-disk artefact of a real walk: drop an in-progress marker so
        # a crash here leaves a run.json whose status flags the dump
        # non-complete (prune reclaims it once quiescent). The terminal manifest
        # write below atomically overwrites it with status="complete".
        # --dry-run returned above without ever creating the run dir, so there
        # is no shell to mark.
        bronze.atomic_write_json(run / "run.json", {"status": "in-progress"})
        bronze.atomic_write_json(run / "investments.json", stage_bodies)

        # Download-avoidance index (documents only): rebuilt statelessly from
        # the COMPLETE prior runs' on-disk document files (extract_equityzen),
        # keyed (deal-slug, doc-slug). link-mode reuses an identical prior copy
        # for immutable docs; fetch-verify re-reads everything else. The current
        # run (still status="in-progress") is excluded, so it never seeds itself.
        skip = None
        if args.documents:
            skip = docdedup.SkipSet.derive(
                run.parent, extract_equityzen,
                freshness_days=None,   # no reliable pre-fetch doc_date (extract_equityzen)
                exclude_run=run,
            )

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
        document_blobs = _empty_doc_counts()
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
            doc_counts = _empty_doc_counts()
            if args.documents:
                edges = (((cap["body"] or {}).get("data") or {}).get("buyer") or {}) \
                    .get("buyerDeals", {}).get("edges") or []
                node = edges[0].get("node") if edges else {}
                docs = [d for d in (node.get("documents") or []) if d.get("id")]
                doc_counts["total"] = len(docs)
                for doc in docs:
                    status = _process_document(
                        skip, deal_slug=slug, doc=doc,
                        target_dir=run / "documents" / slug,
                        fetch_blob=fetch_blob, force=args.documents_force)
                    _tally(doc_counts, status)
                for k in document_blobs:
                    document_blobs[k] += doc_counts[k]
                log.info("  offering %s: documents total=%d fetched=%d linked=%d "
                         "verified=%d changed=%d no_blob=%d errors=%d other=%d",
                         slug, doc_counts["total"], doc_counts["fetched"],
                         doc_counts["linked"], doc_counts["verified"],
                         doc_counts["changed"], doc_counts["no_blob"],
                         doc_counts["errors"], doc_counts["other"])
            offerings_meta.append({"slug": slug, "detail": True,
                                   "documents": doc_counts})

        # 4) Manifest — slugs + counts only. `status` is the completeness
        # signal prune keys on: this terminal write atomically overwrites the
        # in-progress marker dropped at run-dir creation.
        manifest = {
            "status": "complete",
            "source": "equityzen", "snapshot_at": snapshot_at,
            "scope": "offerings+positions+cash_flows" + ("+documents" if args.documents else ""),
            "stage_counts": {s: len(_deal_ids_from(b)) for s, b in stage_bodies.items()},
            "investments": len(deal_ids), "offerings": offerings_meta,
            "document_blobs": document_blobs,
        }
        bronze.atomic_write_json(run / "run.json", manifest)
        log.info("wrote bronze to %s (%d/%d offerings with detail)",
                 run, sum(1 for o in offerings_meta if o["detail"]), len(offerings_meta))
        if args.documents:
            log.info("documents: total=%d fetched=%d linked=%d verified=%d "
                     "changed=%d no_blob=%d errors=%d other=%d",
                     document_blobs["total"], document_blobs["fetched"],
                     document_blobs["linked"], document_blobs["verified"],
                     document_blobs["changed"], document_blobs["no_blob"],
                     document_blobs["errors"], document_blobs["other"])
        return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
