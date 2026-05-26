#!/usr/bin/env python3
"""
viac-dump Phase 3: bronze scrape (pure httpx, no browser).

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

PDF gating:
  - Always download non-TRANSACTION docs (statements,
    Bescheinigungen, contracts, investment profile, …).
  - Always download SECURITY_FUSION PDFs — the JSON transaction
    record carries `amountInChf: 0`; the old→new ISIN mapping is
    ONLY in the PDF.
  - Other TRANSACTION docs (TRADE_REPORT, FEE_CHARGE, INTEREST,
    DIVIDEND, DIVIDEND_CANCELLATION) gated by
    `--with-transaction-documents`. Default is off (skip the ~950
    per-event PDFs; load only when needed).

`--dry-run` walks every URL and writes the JSON artefacts but
skips PDF binaries — useful for checking that selectors/endpoints
still match landmarks without the bandwidth cost.

PDFs are deduplicated across prior bronze runs by document number:
if `<dest>/*/documents/<docid>.pdf` already exists, we hard-link
it into the current run rather than re-fetching. Saves time and
bandwidth on re-runs; the loader keys on content hash anyway.

Read-only — see CLAUDE.md §1. Never invokes a write-state endpoint.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
from datetime import datetime, timezone
from pathlib import Path

import httpx

from viac_client import ViacClient

log = logging.getLogger("viac-dump.download")

DEFAULT_STATE_PATH = Path("/secrets/viac-state.json")
DEFAULT_DEST = Path("/data")

# Build-bound endpoint version suffixes. The SPA appends these
# (e.g. `customer/current/7-5`); we don't know what they encode
# (cache version? schema version?). They appear stable per VIAC
# deploy and survived between our discovery sessions. If any
# endpoint starts 404-ing, re-run explore.py to discover the new
# suffixes.
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
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Walk the REST API and write the JSON artefacts but "
              "skip PDF binaries. Useful for landmark checks."),
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


def fetch_json(client: ViacClient, path: str, dest_file: Path) -> dict | list:
    """GET `path`, save the response body to `dest_file`, return the
    parsed JSON. Raises on non-2xx."""
    log.debug("GET %s", path)
    resp = client.get(path)
    if resp.status_code != 200:
        raise RuntimeError(
            f"GET {path}: HTTP {resp.status_code} (expected 200) — "
            f"if a /N-N suffix changed, re-run explore.py to "
            f"discover the new value.")
    dest_file.parent.mkdir(parents=True, exist_ok=True)
    dest_file.write_bytes(resp.content)
    return resp.json()


def should_download_pdf(doc: dict, with_tx: bool) -> bool:
    """True if this document should be downloaded under the gate."""
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
    n_bytes = 0
    with client.stream("GET", path) as resp:
        if resp.status_code != 200:
            resp.read()
            raise RuntimeError(
                f"GET {path}: HTTP {resp.status_code} (expected 200)")
        with target.open("wb") as fh:
            for chunk in resp.iter_bytes(chunk_size=65536):
                fh.write(chunk)
                n_bytes += len(chunk)
    return ("fetched", n_bytes)


def walk(client: ViacClient, dest_root: Path, *,
         with_tx_docs: bool, dry_run: bool) -> dict:
    """Run the full bronze fetch. Returns the manifest."""
    ts = utc_ts()
    bronze_dir = dest_root / ts
    bronze_dir.mkdir(parents=True)
    log.info("bronze: %s", bronze_dir)

    manifest: dict = {
        "timestamp": ts,
        "dry_run": dry_run,
        "with_transaction_documents": with_tx_docs,
        "endpoints": [],
        "portfolios": [],
        "documents": {"total": 0, "fetched": 0, "linked": 0, "skipped": 0},
    }

    def get(path: str, rel: str) -> dict | list:
        manifest["endpoints"].append(path)
        return fetch_json(client, path, bronze_dir / rel)

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
        if not should_download_pdf(doc, with_tx_docs):
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
            )
    except httpx.HTTPError as e:
        log.error("HTTP error during walk: %s", e)
        return 2

    # Write the manifest last so a partial bronze dir is detectable
    # (no run.json = incomplete).
    bronze_dir = args.dest / manifest["timestamp"]
    (bronze_dir / "run.json").write_text(json.dumps(manifest, indent=2))
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
