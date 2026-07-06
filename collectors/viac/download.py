#!/usr/bin/env python3
"""
viac Phase 3: bronze scrape (pure httpx, no browser).

Reads the session-state file produced by login.py, walks the
VIAC REST API, and writes a timestamped bronze dump to disk.

Bronze layout (mirrors the sibling toolkits):

    <dest>/<UTC-ts>/
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
  - DATE gate: doc `timestamp` must fall in [documents-since,
    documents-until]. The shared --since/--until/--lookback contract
    defaults to a 90-day window; --lookback all lifts it to ~30y.
    Docs with a missing/unparseable timestamp are kept (better to
    over-fetch than silently lose data the source didn't time-stamp).
  - TYPE gate, applied to in-window docs:
    * Non-TRANSACTION docs (statements, Bescheinigungen, contracts,
      investment profile, …) always download.
    * SECURITY_FUSION PDFs always download — the JSON transaction
      record carries `amountInChf: 0`; the old→new ISIN mapping is
      ONLY in the PDF.
    * Other TRANSACTION docs (TRADE_REPORT, FEE_CHARGE, INTEREST,
      DIVIDEND, DIVIDEND_CANCELLATION) gated by
      `--with-transaction-documents`. Default is off (skip the ~950
      per-event PDFs; load only when needed).

Transactions are written FULL to bronze (the REST envelope is one
small JSON; keeping it complete preserves bronze faithfulness). The
window is recorded in run.json's `windows` block so the silver
loader can re-apply it without taking its own CLI args.

`--dry-run` walks every URL (verifying the session and enumerating
the export surfaces) and logs the plan, but persists NOTHING under
`--dest`: no run dir, no manifest, no JSON, no PDFs. It leaves no
bronze dump for load/prune to see — useful for checking that
selectors/endpoints still match landmarks without touching bronze.

PDFs are deduplicated across prior bronze runs by document number:
if `<dest>/*/documents/<docid>.pdf` already exists, we hard-link
it into the current run rather than re-fetching. Saves time and
bandwidth on re-runs; the loader keys on content hash anyway.

Read-only — see CLAUDE.md §1. Never invokes a write-state endpoint.
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
import time
from datetime import date, datetime, timezone
from pathlib import Path

import httpx

from collectorkit import bronze, cli
from viac_client import ViacClient

log = logging.getLogger("viac.download")

DEFAULT_STATE_PATH = Path("/secrets/viac-state.json")
DEFAULT_DEST = Path("/data")

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
# --with-transaction-documents flag (PDF content not derivable
# from the JSON API).
ALWAYS_DOWNLOAD_TX_SUBTYPES = frozenset({"SECURITY_FUSION"})


def utc_ts() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


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
        "--dest", default=DEFAULT_DEST, type=Path,
        help=(f"Bronze destination root. Each run lands under "
              f"<dest>/<UTC-ts>/. Default: {DEFAULT_DEST}."),
    )
    p.add_argument(
        "--with-transaction-documents", action="store_true",
        help=("Download per-event TRANSACTION PDFs (TRADE_REPORT, "
              "FEE_CHARGE, INTEREST, DIVIDEND, "
              "DIVIDEND_CANCELLATION). ~950 PDFs at present. "
              "SECURITY_FUSION is downloaded regardless of this flag."),
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
    cli.add_lookback_args(p)
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Walk the REST API (verify the session, enumerate the "
              "surfaces, log the plan) but persist NOTHING under --dest "
              "— no run dir, manifest, JSON, or PDFs. Useful for landmark "
              "checks without touching bronze."),
    )
    p.add_argument(
        "--debug", action="store_true",
        help=("Uniform debug-artefact gate shared across the collectors. "
              "viac is REST-only (pure httpx, no browser) and writes no "
              "bronze-resident debug artefacts, so this currently gates "
              "nothing; use -v/--verbose for DEBUG logging to stderr. The "
              "flag exists so `--debug` means the same thing everywhere."),
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
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


def find_existing_pdf(dest_root: Path, docid: str) -> Path | None:
    """Look across prior bronze runs for an already-downloaded PDF."""
    # Pattern: <dest>/<any-ts>/documents/<docid>.pdf
    for hit in dest_root.glob(f"*/documents/{docid}.pdf"):
        if hit.is_file():
            return hit
    return None


def fetch_pdf(client: ViacClient, docid: str, target: Path,
              dest_root: Path) -> tuple[str, int]:
    """Download a PDF, deduplicating against prior bronze runs via
    hard-link. Returns (status, bytes) where status is one of
    'fetched' / 'linked' / 'skipped'."""
    target.parent.mkdir(parents=True, exist_ok=True)
    existing = find_existing_pdf(dest_root, docid)
    if existing is not None:
        try:
            os.link(existing, target)
            return ("linked", target.stat().st_size)
        except OSError as e:
            log.debug("hardlink %s → %s failed: %s; falling through to fetch.",
                      existing, target, e)
    path = f"/files/document/{docid}"
    log.debug("GET %s", path)

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

    n_bytes = with_retry(_stream_to_disk, label=f"GET {path}")
    return ("fetched", n_bytes)


def walk(client: ViacClient, dest_root: Path, *,
         with_tx_docs: bool, dry_run: bool,
         since: date, until: date,
         documents_since: date, documents_until: date) -> dict:
    """Run the full bronze fetch. Returns the manifest.

    The four window bounds are recorded into run.json's `windows`
    block so the silver loader can re-apply them at insert time
    without taking its own CLI args. Per the fetch/load split:
    document PDFs are filtered AT FETCH (PDF binaries are big; don't
    download what we won't insert), but transactions are written
    full and filtered AT LOAD (the REST envelope is one small JSON;
    keeping it complete preserves bronze faithfulness)."""
    ts = utc_ts()
    bronze_dir = dest_root / ts
    if dry_run:
        # Export-nothing dry-run (root CLAUDE.md §2): still walk every
        # endpoint (verify the session, enumerate the surfaces, log the
        # plan) but persist nothing under --dest — no run dir, no
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
        "documents": {"total": 0, "fetched": 0, "linked": 0, "skipped": 0},
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
            status, n_bytes = fetch_pdf(client, docid, target, dest_root)
            manifest["documents"][status] += 1
            log.debug("doc %s: %s (%d bytes)", docid, status, n_bytes)
        except Exception as e:
            log.warning("doc %s: %s", docid, e)
            manifest["documents"].setdefault("errors", 0)
            manifest["documents"]["errors"] += 1

    return manifest


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    # Resolve the shared --since/--until/--lookback/--documents-*
    # contract once; pass concrete dates into walk(). Documents are
    # filtered AT FETCH (avoid wasted PDF binaries); transactions
    # carry the window through bronze run.json so the silver loader
    # can re-apply it without taking its own CLI args.
    since, until, docs_since, docs_until = cli.resolve_lookback(args)

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
                client, args.dest,
                with_tx_docs=args.with_transaction_documents,
                dry_run=args.dry_run,
                since=since, until=until,
                documents_since=docs_since,
                documents_until=docs_until,
            )
    except httpx.HTTPError as e:
        log.error("HTTP error during walk: %s", e)
        return 2

    # Stamp the terminal status and atomically (tmp + rename) overwrite
    # the in-progress marker, so a prune racing the finalisation never
    # reads a half-written manifest and the run's state is legible
    # throughout. status="complete" is the forward signal load and prune
    # key on. A --dry-run persists NOTHING under --dest (root CLAUDE.md
    # §2 "export nothing"): the walk left no run dir, so there is no
    # in-progress marker to finalise — skip the terminal write entirely
    # rather than resurrect a "dry-run" shell that load/prune would then
    # have to reason about.
    if args.dry_run:
        log.info("dry-run complete: nothing written to bronze under %s",
                 args.dest)
        return 0
    manifest["status"] = "complete"
    bronze_dir = args.dest / manifest["timestamp"]
    bronze.atomic_write_json(bronze_dir / "run.json", manifest)
    log.info(
        "done. documents: total=%d fetched=%d linked=%d skipped=%d",
        manifest["documents"]["total"],
        manifest["documents"]["fetched"],
        manifest["documents"]["linked"],
        manifest["documents"]["skipped"],
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
