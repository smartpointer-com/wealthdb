#!/usr/bin/env python3
"""
viac Phase 3: bronze scrape (pure httpx, no browser).

Reads the session-state file produced by login.py, walks the
VIAC REST API, and writes a timestamped bronze dump to disk.

Bronze layout (mirrors the sibling toolkits):

    <bronze-dir>/<UTC-ts>/
    ├── run.json                          manifest
    ├── customer.json                     customer profile
    ├── wealth/
    │   ├── portfolio-inventory.json      list of all portfolios (p3a + pvb)
    │   ├── summary.json                  daily NAV time series
    │   └── allocation.json               overall allocation
    ├── positions/<portfolio-num>/
    │   ├── strategy.json                 strategy / target weights
    │   ├── assets.json                   current holdings (per fund)
    │   └── fees.json                     fee config
    ├── transactions/all.json             every transaction across portfolios
    ├── documents/
    │   ├── index.json                    document catalogue (1019 entries)
    │   └── <docid>.pdf                   PDF binaries (see gating below)
    └── (manual/ ... user-uploaded artefacts, ingested by load.py)

PDF gating (in order):
  - DATE gate: doc `timestamp` must fall in the run's window. The
    shared --lookback contract names its start and runs to today,
    defaulting to the last 90 days; --lookback all lifts it to ~30y.
    Docs with a missing/unparseable timestamp are kept (better to
    over-fetch than silently lose data the source didn't time-stamp).
  - TYPE gate, applied to in-window docs:
    * Non-TRANSACTION docs (statements, Bescheinigungen, contracts,
      investment profile, …) always download.
    * SECURITY_FUSION PDFs always download — the JSON transaction
      record carries `amountInChf: 0`; the old→new ISIN mapping is
      ONLY in the PDF.
    * Other TRANSACTION docs (TRADE_REPORT, FEE_CHARGE, INTEREST,
      DIVIDEND, DIVIDEND_CANCELLATION) downloaded by default (~950
      per-event PDFs); `--no-transaction-documents` skips them.

Transactions are written FULL to bronze (the REST envelope is one
small JSON; keeping it complete preserves bronze faithfulness). The
window is recorded in run.json's `windows` block so the silver
loader can re-apply it without taking its own CLI args.

`--dry-run` walks every URL (verifying the session and enumerating
the export surfaces) and logs the plan, but persists NOTHING under
`--bronze-dir`: no run dir, no manifest, no JSON, no PDFs. It leaves no
bronze dump for load/prune to see — useful for checking that
selectors/endpoints still match landmarks without touching bronze.

PDF fetches are download-avoidant via the shared
`collectorkit.docdedup` engine, keyed by document number and chosen
per document class so the fetch-avoidance never serves a stale figure:
  - PARSED / restatement-prone documents — the `INVESTMENT_REPORTING`
    period-end statements load.py parses for historical holdings, every
    `TAX` document (the Pillar-3a `Bescheinigung` certificates), and the
    data-bearing `SECURITY_FUSION` PDF (its old→new ISIN map is slated for
    a parser pass) — are ALWAYS fetched and content-compared against the
    prior copy: a byte-identical one is hardlinked (disk reclaimed), a
    changed one keeps its fresh bytes, so a re-issued/corrected statement
    is never missed;
  - executed-once, immutable, unparsed documents (contracts, investment
    profiles, credit notes, communications, and the per-event transaction
    receipts) are HARDLINKED from a prior complete run when the document
    number matches, and the fetch is skipped — the fetch-avoidance win at
    zero silver-correctness risk (any hardlink error falls through to a
    real fetch, so a document degrades to a fetch, never to a miss);
  - any unrecognised document type is fetch-verified too (the safe
    default).
Every run dir stays self-contained (a hardlink is a real in-run file),
so load.py needs no cross-run fallback. --documents-force bypasses the
download-avoidance index entirely.

Read-only — see CLAUDE.md §1. Never invokes a write-state endpoint.
"""

from __future__ import annotations

import argparse
import logging
import sys
import time
from datetime import date
from pathlib import Path

import httpx

from collectorkit import bronze, cli, docdedup
from viac_client import ViacClient

log = logging.getLogger("viac.download")

DEFAULT_STATE_PATH = Path("/secrets/viac-state.json")
DEFAULT_BRONZE_DIR = Path("/data")

# Build-bound endpoint version suffixes. The SPA appends these
# (e.g. `customer/current/7-5`); the digits are bundle-build-
# bound and appear stable per VIAC deploy. If any endpoint
# starts 404-ing, capture a fresh login flow against the SPA to
# discover the new suffixes (see DESIGN.md §2.2).
ENDPOINT_SUFFIXES = {
    "customer/current": "7-5",
    "notification": "6-2",
    "p3a/portfolio/moneyPaymentInfo": "6-2",
    "document": "7-0",
}
PORTFOLIO_FEES_SUFFIX = "70"  # /p3a/portfolio/<num>/fees-<this>

# Document types that always get downloaded regardless of the
# --no-transaction-documents opt-out (PDF content not derivable
# from the JSON API).
ALWAYS_DOWNLOAD_TX_SUBTYPES = frozenset({"SECURITY_FUSION"})


# --- document (type, subType) → docdedup class (link vs fetch-verify) ------
#
# VIAC's document index carries a (type, subType) taxonomy (see
# migrations/0001_initial.sql):
#   TRANSACTION      TRADE_REPORT, FEE_CHARGE, INTEREST, DIVIDEND,
#                    DIVIDEND_CANCELLATION, SECURITY_FUSION
#   CONTRACT         PROVISION_CONTRACT, INVESTMENT_PROFILE
#   REPORT           INVESTMENT_REPORTING, MANUAL_INVESTMENT_REPORTING
#   TAX              TAX_REPORT (Pillar-3a Bescheinigungen)
#   ACCOUNT_MOVEMENT CONTRIBUTION_CREDIT_NOTE
#   COMMUNICATION    GENERIC_COMMUNICATION
# docdedup's fetch-avoidance mode is a property of the document CLASS, and
# fetch-verify is the safe default: only classes that are immutable-under-
# their-docid AND never parsed for a silver figure are opted into link-mode.

# fetch-verify (always re-read + content-compare): parsed / restatement-
# prone documents. The REPORT statements are parsed by load.py's
# load_historical_reports_phase (pdf_parsers) for historical holdings and
# can be re-issued under a stable documentNumber, so link-mode would risk
# feeding a superseded figure into the positions replay — a correctness bug.
_REPORT_SUBTYPES = frozenset({
    "INVESTMENT_REPORTING",
    "MANUAL_INVESTMENT_REPORTING",
})
# The SECURITY_FUSION PDF is data-bearing — its old→new ISIN fusion map is
# the ONLY copy (the JSON tx carries amountInChf:0) and a parser pass is
# planned (DESIGN.md §9) — so it is fetch-verified, never linked. Aliased to
# the gate's always-download set on purpose: a TRANSACTION subtype worth
# fetching because its data lives only in the PDF is by that same token one
# to fetch-verify, not to trust a link for.
_FUSION_SUBTYPES = ALWAYS_DOWNLOAD_TX_SUBTYPES

# link (fetch-avoidance): executed-once, immutable-under-docid, NOT parsed
# for any silver figure — an explicit allow-list, never a default. The
# per-event TRANSACTION receipts document a single settled event and are
# recorded in silver only by sha256 + (type, subType) metadata (DESIGN.md
# §8), so a prior identical copy is safe to hardlink in.
_LINK_SUBTYPES = frozenset({
    # CONTRACT
    "PROVISION_CONTRACT", "INVESTMENT_PROFILE",
    # ACCOUNT_MOVEMENT
    "CONTRIBUTION_CREDIT_NOTE",
    # COMMUNICATION
    "GENERIC_COMMUNICATION",
    # per-event TRANSACTION receipts (SECURITY_FUSION handled above)
    "TRADE_REPORT", "FEE_CHARGE", "INTEREST", "DIVIDEND",
    "DIVIDEND_CANCELLATION",
})


def _document_class(doc: dict) -> str | None:
    """Map a VIAC document-index entry to a docdedup class (mode selection).

    Parsed / restatement-prone documents resolve to fetch-verify: every
    ``TAX`` document (the Pillar-3a ``Bescheinigung`` certificates) is
    ``tax``, and the parsed ``REPORT`` statements plus the data-bearing
    ``SECURITY_FUSION`` PDF are ``mutable`` — both fetch-verify-dedup, so a
    re-issue under a stable documentNumber is caught rather than served
    stale. The executed-once, immutable, unparsed documents
    (``_LINK_SUBTYPES``) are ``immutable`` → link-mode. Anything else is
    left unclassified → the engine fetch-verifies it (the safe default).
    The safe classes trigger on ``type`` as well as ``subType`` so a
    not-yet-catalogued tax/report subtype still fetch-verifies.
    """
    dtype = doc.get("type")
    subtype = doc.get("subType")
    if dtype == "TAX":
        return docdedup.CLASS_TAX
    if dtype == "REPORT" or subtype in _REPORT_SUBTYPES:
        return docdedup.CLASS_MUTABLE
    if subtype in _FUSION_SUBTYPES:
        return docdedup.CLASS_MUTABLE
    if subtype in _LINK_SUBTYPES:
        return docdedup.CLASS_IMMUTABLE
    return None


def extract_viac(run_dir, manifest):
    """docdedup extract hook (disk-driven): recover each prior run's document
    identities from its ``documents/<docid>.pdf`` files. Key = ``(docid,)``,
    the ``documentNumber`` the file is named after — collision-free, so the
    same logical document lands at the same path every run (this is exactly
    the identity the old ``find_existing_pdf`` glob keyed on). No reliable
    pre-fetch issue date drives a freshness window here, so ``doc_date`` is
    None. Enumerating on-disk files means a key counts only while its blob
    still exists, so a pruned bronze self-heals. ``index.json`` and any
    non-PDF stray under ``documents/`` are ignored."""
    docs_root = run_dir / "documents"
    if not docs_root.is_dir():
        return
    for f in docs_root.iterdir():
        if f.is_file() and not f.is_symlink() and f.suffix == ".pdf":
            yield docdedup.DocRef(key=(f.stem,), doc_date=None,
                                  relpath=str(f.relative_to(run_dir)))


# The run.json documents audit block (shared docdedup counters). `skipped`
# (a doc the should_download_pdf gate declined, or a dry-run) and `total` are
# set by the walk itself, not by a docdedup outcome, so `skipped` is a viac
# extra bucket alongside the standard ones.
def _empty_doc_counts() -> dict:
    return docdedup.empty_audit("skipped")


def _tally(counts: dict, status: str) -> None:
    docdedup.tally(counts, status)


# Transient httpx errors worth retrying. Empirically VIAC has
# dropped an HTTP/2 stream once during a 1019-PDF run with the
# h2 "ConnectionTerminated" diagnostic — that surfaces in httpx
# as RemoteProtocolError. Network blips during the same run could
# also trip TimeoutException or NetworkError; treat them all the
# same.
RETRYABLE_HTTPX_ERRORS = (
    httpx.RemoteProtocolError,
    httpx.NetworkError,
    httpx.TimeoutException,
)


def with_retry(
    fn,
    *,
    label: str,
    max_attempts: int = 3,
    base_delay: float = 1.0,
):
    """Call `fn()` with simple exponential backoff on transient
    httpx-level errors. Non-transient errors (4xx via
    raise_for_status, programming bugs, etc.) propagate
    immediately."""
    for attempt in range(1, max_attempts + 1):
        try:
            return fn()
        except RETRYABLE_HTTPX_ERRORS as e:
            if attempt == max_attempts:
                log.warning(
                    "%s: %s after %d attempts; giving up",
                    label, type(e).__name__, attempt)
                raise
            delay = base_delay * (2 ** (attempt - 1))
            log.warning(
                "%s: transient %s (attempt %d/%d): %s; retry in %.1fs",
                label, type(e).__name__, attempt, max_attempts, e, delay)
            time.sleep(delay)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--state-path", default=DEFAULT_STATE_PATH, type=Path,
        help=f"Session-state JSON from login.py. Default: {DEFAULT_STATE_PATH}.",
    )
    p.add_argument(
        "--bronze-dir", default=DEFAULT_BRONZE_DIR, type=Path,
        help=(f"Bronze tree root. Each run lands under "
              f"<bronze-dir>/<UTC-ts>/. Default: {DEFAULT_BRONZE_DIR}."),
    )
    p.add_argument(
        "--no-transaction-documents", dest="with_transaction_documents",
        action="store_false",
        help=("Skip the per-event TRANSACTION PDFs (TRADE_REPORT, "
              "FEE_CHARGE, INTEREST, DIVIDEND, DIVIDEND_CANCELLATION; "
              "~950 PDFs). These are downloaded by DEFAULT; pass this to "
              "skip them. This is the narrower per-event-receipt opt-out — "
              "distinct from the always-on document centre. SECURITY_FUSION "
              "is downloaded regardless of this flag."),
    )
    p.add_argument(
        "--documents-force", action="store_true",
        help=("Bypass the document download-avoidance index: fetch every "
              "in-gate PDF even when a byte-identical copy exists in a prior "
              "bronze run (no hardlink reuse). Use to re-establish ground "
              "truth or as a first-run confidence check (run once with, once "
              "without, diff silver under `load --force`)."),
    )
    # Shared date-window contract. VIAC's REST endpoints don't take
    # a date filter (the transactions and documents-index calls
    # always return the full history), so we apply the windows
    # client-side: documents are filtered AT FETCH (skip the PDF
    # binaries whose `timestamp` falls outside [documents-since,
    # documents-until]) and transactions are written full to bronze
    # then filtered AT SILVER-LOAD (the JSON envelope is small;
    # keeping it complete preserves bronze faithfulness and lets the
    # loader re-derive any window without a re-download). Both
    # windows are persisted into run.json's `windows` block so the
    # loader doesn't need its own CLI args.
    cli.add_standard_args(p, verb="download")
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Walk the REST API (verify the session, enumerate the "
              "surfaces, log the plan) but persist NOTHING under "
              "--bronze-dir — no run dir, manifest, JSON, or PDFs. "
              "Useful for landmark checks without touching bronze."),
    )
    p.add_argument(
        "--debug", action="store_true",
        help=("Uniform debug-artefact gate shared across the collectors. "
              "viac is REST-only (pure httpx, no browser) and writes no "
              "bronze-resident debug artefacts, so this currently gates "
              "nothing; use -v/--verbose for DEBUG logging to stderr. The "
              "flag exists so `--debug` means the same thing everywhere."),
    )
    return p.parse_args(argv)


def fetch_json(client: ViacClient, path: str, dest_file: Path,
               *, persist: bool = True) -> dict | list:
    """GET `path` (with retry on transient errors), save the
    response body to `dest_file`, return the parsed JSON.
    Raises on non-2xx.

    With `persist=False` (the dry-run walk) the GET + status check +
    parse still run — that's the point of a dry-run: verify the session
    and enumerate the export surfaces — but nothing is written to disk
    (no `dest_file`, no parent dir), so the bronze root stays untouched."""
    log.debug("GET %s", path)
    resp = with_retry(lambda: client.get(path), label=f"GET {path}")
    if resp.status_code != 200:
        raise RuntimeError(
            f"GET {path}: HTTP {resp.status_code} (expected 200) — "
            f"if a /N-N suffix changed, re-discover from a fresh "
            f"login capture (see DESIGN.md §2.2).")
    if persist:
        dest_file.parent.mkdir(parents=True, exist_ok=True)
        dest_file.write_bytes(resp.content)
    return resp.json()


def _doc_date(doc: dict) -> date | None:
    """Extract a YYYY-MM-DD date from a doc-index entry's `timestamp`.
    Returns None if the field is missing or unparseable — callers
    treat "unknown date" as "in window" so we never silently drop a
    document the source didn't time-stamp.

    VIAC's timestamps come back as `YYYY-MM-DDTHH:MM:SS.ffffff` with
    no timezone suffix; we only need the date portion so the first
    10 chars are enough."""
    ts = doc.get("timestamp")
    if not isinstance(ts, str) or len(ts) < 10:
        return None
    try:
        return date.fromisoformat(ts[:10])
    except ValueError:
        return None


def should_download_pdf(doc: dict, with_tx: bool,
                        docs_since: date, docs_until: date) -> bool:
    """True if this document should be downloaded under the gate."""
    # Date gate first: drop anything outside [docs_since, docs_until]
    # regardless of type. Docs with no/unparseable timestamp are kept
    # — we'd rather over-fetch than silently lose a document the
    # source didn't time-stamp.
    d = _doc_date(doc)
    if d is not None and (d < docs_since or d > docs_until):
        return False
    if doc.get("type") != "TRANSACTION":
        return True  # non-tx: statements, Bescheinigungen, etc.
    if doc.get("subType") in ALWAYS_DOWNLOAD_TX_SUBTYPES:
        return True  # SECURITY_FUSION: critical, JSON has no ISIN map
    return with_tx


def fetch_pdf(client: ViacClient, doc: dict, docid: str, target: Path,
              skip: docdedup.SkipSet, *, force: bool = False) -> str:
    """Fetch a document PDF into `target`, routing the download-avoidance
    decision through the shared `collectorkit.docdedup` engine by the
    document's class (`_document_class`): the immutable, unparsed documents
    are hardlinked from a prior identical copy (fetch avoided), while the
    parsed / tax / fusion documents are always fetched and content-compared
    so a re-issue is never served stale. Any hardlink error falls through to
    a real fetch (degrade to a fetch, never to a miss). Returns a docdedup
    outcome code (LINKED / FETCHED / VERIFIED / CHANGED / FETCH_FAILED)."""
    path = f"/files/document/{docid}"

    def _stream_to_disk() -> int:
        # If a prior attempt wrote a partial file, drop it — we
        # restart from scratch on retry (range-resume isn't
        # supported by VIAC's CDN as far as we can tell, and
        # PDFs are small enough that re-fetching is cheap).
        if target.exists():
            target.unlink()
        n = 0
        with client.stream("GET", path) as resp:
            if resp.status_code != 200:
                resp.read()
                raise RuntimeError(
                    f"GET {path}: HTTP {resp.status_code} (expected 200)")
            with target.open("wb") as fh:
                for chunk in resp.iter_bytes(chunk_size=65536):
                    fh.write(chunk)
                    n += len(chunk)
        return n

    def _fetch() -> Path:
        # docdedup's fetch closure: stream the blob to `target` (with the
        # transient-error retry) and return its path. A non-transient error
        # (non-200, exhausted retries) propagates to the walk, which tallies
        # it as a document error — same failure semantics as before docdedup.
        target.parent.mkdir(parents=True, exist_ok=True)
        log.debug("GET %s", path)
        with_retry(_stream_to_disk, label=f"GET {path}")
        return target

    return docdedup.process(
        skip, key=(docid,), doc_class=_document_class(doc),
        target_dir=target.parent, stem=docid,
        fetch=_fetch, force=force)


def walk(client: ViacClient, dest_root: Path, *,
         with_tx_docs: bool, dry_run: bool, documents_force: bool = False,
         since: date, until: date,
         documents_since: date, documents_until: date) -> dict:
    """Run the full bronze fetch. Returns the manifest.

    The four window bounds are recorded into run.json's `windows`
    block so the silver loader can re-apply them at insert time
    without taking its own CLI args. Per the fetch/load split:
    document PDFs are filtered AT FETCH (PDF binaries are big; don't
    download what we won't insert), but transactions are written
    full and filtered AT LOAD (the REST envelope is one small JSON;
    keeping it complete preserves bronze faithfulness).

    In-gate document PDFs run through the `collectorkit.docdedup`
    download-avoidance engine (link the immutable/unparsed docs, fetch-verify
    the parsed/tax/fusion ones); `documents_force` bypasses that index."""
    ts = bronze.ts_slug()
    bronze_dir = dest_root / ts
    if dry_run:
        # Export-nothing dry-run (root CLAUDE.md §2): still walk every
        # endpoint (verify the session, enumerate the surfaces, log the
        # plan) but persist nothing under --bronze-dir — no run dir, no
        # in-progress marker, no JSON. A dry-run therefore leaves NO
        # bronze dump for load/prune to pick up.
        log.info("dry-run: walking endpoints, nothing written to bronze")
    else:
        bronze_dir.mkdir(parents=True)
        log.info("bronze: %s", bronze_dir)

    manifest: dict = {
        "timestamp": ts,
        "status": "in-progress",
        "dry_run": dry_run,
        "with_transaction_documents": with_tx_docs,
        "windows": {
            "since": since.isoformat(),
            "until": until.isoformat(),
            "documents_since": documents_since.isoformat(),
            "documents_until": documents_until.isoformat(),
        },
        "endpoints": [],
        "portfolios": [],
        "documents": _empty_doc_counts(),
    }
    # Drop an "in-progress" manifest up front; main() overwrites it with
    # the terminal status once the walk returns. A crash mid-walk leaves
    # status="in-progress", which load skips (only "complete" and legacy
    # statusless dumps load) and prune reclaims once quiescent — a
    # stronger signal than the older "no run.json = incomplete" heuristic,
    # which a partial run.json write could defeat. Skipped on a dry-run:
    # that path writes nothing at all.
    if not dry_run:
        bronze.atomic_write_json(bronze_dir / "run.json", manifest)

    def get(path: str, rel: str) -> dict | list:
        manifest["endpoints"].append(path)
        return fetch_json(client, path, bronze_dir / rel, persist=not dry_run)

    # Customer.
    get(f"/rest/web/customer/current/{ENDPOINT_SUFFIXES['customer/current']}",
        "customer.json")

    # Wealth-level.
    inventory = get("/rest/web/wealth/portfolio-inventory",
                    "wealth/portfolio-inventory.json")
    get("/rest/web/wealth/summary", "wealth/summary.json")
    get("/rest/web/wealth/allocation", "wealth/allocation.json")

    # Per-portfolio (Pillar-3a only; Pillar-2 vested-benefits surface
    # not yet mapped — see DESIGN.md open questions).
    p3a_portfolios = inventory.get("p3a") if isinstance(inventory, dict) else []
    for pf in p3a_portfolios:
        num = pf["number"]
        log.info("portfolio %s (%s)", num, pf.get("name", "?"))
        get(f"/rest/web/p3a/portfolio/{num}/strategySummary",
            f"positions/{num}/strategy.json")
        get(f"/rest/web/p3a/portfolio/{num}/assetsOverview",
            f"positions/{num}/assets.json")
        get(f"/rest/web/p3a/portfolio/{num}/fees-{PORTFOLIO_FEES_SUFFIX}",
            f"positions/{num}/fees.json")
        manifest["portfolios"].append({
            "number": num,
            "name": pf.get("name"),
            "state": pf.get("state"),
            "strategy": pf.get("strategy"),
        })

    # Transactions (one shot, all portfolios).
    get("/rest/web/p3a/portfolio/transactions", "transactions/all.json")

    # Documents.
    doc_index = get(
        f"/rest/web/document/{ENDPOINT_SUFFIXES['document']}",
        "documents/index.json")
    if not isinstance(doc_index, list):
        raise RuntimeError(
            f"document index is not a list: got {type(doc_index).__name__}")
    log.info("document index: %d entries", len(doc_index))
    manifest["documents"]["total"] = len(doc_index)

    # Download-avoidance index (documents only): rebuilt statelessly from the
    # COMPLETE prior runs' on-disk documents/<docid>.pdf files (extract_viac),
    # keyed (docid,). link-mode reuses an identical prior copy for the
    # immutable/unparsed docs; fetch-verify re-reads the parsed/tax/fusion
    # ones. The current run (still status="in-progress") is excluded, so it
    # never seeds itself. No freshness window: extract_viac has no reliable
    # pre-fetch doc_date, and the parsed docs are guarded by fetch-verify, not
    # by a window. Skipped on a dry-run (which fetches nothing).
    skip = None
    if not dry_run:
        skip = docdedup.SkipSet.derive(
            dest_root, extract_viac,
            freshness_days=None, exclude_run=bronze_dir)

    for doc in doc_index:
        docid = doc.get("documentNumber")
        if not docid:
            continue
        if not should_download_pdf(doc, with_tx_docs,
                                   documents_since, documents_until):
            manifest["documents"]["skipped"] += 1
            continue
        if dry_run:
            manifest["documents"]["skipped"] += 1
            continue
        target = bronze_dir / "documents" / f"{docid}.pdf"
        try:
            status = fetch_pdf(client, doc, docid, target, skip,
                               force=documents_force)
            _tally(manifest["documents"], status)
            log.debug("doc %s: %s", docid, status)
        except Exception as e:
            log.warning("doc %s: %s", docid, e)
            manifest["documents"]["errors"] += 1

    return manifest


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    if args.debug:
        # TODO(second pass): write bronze-resident debug captures under --debug.
        cli.warn_debug_noop("viac", log)
    # Resolve the shared --lookback contract once into the concrete
    # [since..today] window; pass the dates into walk(). Documents are
    # filtered AT FETCH (avoid wasted PDF binaries); transactions
    # carry the window through bronze run.json so the silver loader
    # can re-apply it without taking its own CLI args.
    since, until = cli.resolve_lookback(args)

    if not args.state_path.is_file():
        log.error(
            "no session state at %s; run login.py first.", args.state_path)
        return 1

    try:
        client = ViacClient.from_state(args.state_path)
    except Exception as e:
        log.error("could not load %s: %s", args.state_path, e)
        return 1

    # Probe session liveness before bronze-dir-create so a dead
    # session doesn't leave an empty timestamped dir.
    try:
        with client:
            resp = client.get("/rest/web/heartbeat")
            if resp.status_code != 204:
                log.error(
                    "heartbeat returned %d — session likely dead; "
                    "re-run login.py.", resp.status_code)
                return 1
            manifest = walk(
                client, args.bronze_dir,
                with_tx_docs=args.with_transaction_documents,
                dry_run=args.dry_run,
                documents_force=args.documents_force,
                since=since, until=until,
                documents_since=since,
                documents_until=until,
            )
    except httpx.HTTPError as e:
        log.error("HTTP error during walk: %s", e)
        return 2

    # Stamp the terminal status and atomically (tmp + rename) overwrite
    # the in-progress marker, so a prune racing the finalisation never
    # reads a half-written manifest and the run's state is legible
    # throughout. status="complete" is the forward signal load and prune
    # key on. A --dry-run persists NOTHING under --bronze-dir (root CLAUDE.md
    # §2 "export nothing"): the walk left no run dir, so there is no
    # in-progress marker to finalise — skip the terminal write entirely
    # rather than resurrect a "dry-run" shell that load/prune would then
    # have to reason about.
    if args.dry_run:
        log.info("dry-run complete: nothing written to bronze under %s",
                 args.bronze_dir)
        return 0
    manifest["status"] = "complete"
    run_dir = args.bronze_dir / manifest["timestamp"]
    bronze.atomic_write_json(run_dir / "run.json", manifest)
    log.info("done. documents: %s",
             docdedup.audit_summary(manifest["documents"]))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
