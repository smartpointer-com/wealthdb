#!/usr/bin/env python3
"""equityzen silver loader.

Parses the bronze captured by download.py into the source-shaped SQLite
silver DB, the input contract to the gold engine. Idempotent: bronze runs
already recorded in `dump_runs` are skipped; --force rebuilds it from all
bronze, beside the old silver, which a failed rebuild leaves in place. A run
that never finished (no run.json, or a status other than "complete") is
skipped too: `prune` reclaims it.

Per bronze run (`$XDG_DATA_HOME/wealthdb/equityzen/<UTC-ts>/`):

  * investments.json             {stage: getBuyerInvestments body} — the
                                 per-stage lists (the fallback node).
  * offerings/<slug>/detail.json getMyInvestmentDetails per offering — the
                                 rich node (basis, share counts, the
                                 transaction ledger, documents).

Schema (see migrations/0001_initial.sql):

  offerings      IMMUTABLE, one row per investment (deal_external_id):
                 identity + entry terms (basis / purchase_price /
                 shares_original / currency / company / fund / kind).
  positions      EVENT-SOURCED valuation history — one row per capital event
                 (investment → dispositions → exit), replayed from the
                 ledger, from the original investment date forward. Stores
                 changes only; reconstruct any day's holdings by taking each
                 position's latest event ≤ that day and keeping is_open=1.
  cash_flows     purchase + distributions (CLOSED transactions), id-keyed.
  tax_documents  per-offering document metadata, id-keyed.

Valuation uses CLOSED-deal prices (purchase + tenders); order-book asks
and deal-era implied valuations are excluded. Multi-company funds, which
have no per-share tender price, instead revalue to the parsed capital-
account-statement NAV, injected as `statement` events (see statements.py).
See DESIGN.md §4/§5/§6.
"""
from __future__ import annotations

import argparse
import functools
import json
import logging
import os
import sqlite3
import sys
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import (
    bronze, derivations, extract, loader, pdftotext, silver, srcfp,
)

import statements  # PDF parsers for capital-account statements + K-1s

log = logging.getLogger("equityzen.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS_DIR = HERE / "migrations"

DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_SILVER_DB = Path("/data/equityzen.db")

# EquityZen assetClass -> wealthdb offering kind (DESIGN.md §6, Enums).
ASSET_CLASS_KIND = {
    "ASSET_COMPANY": "spv",
    "ASSET_MULTI_COMPANY_FUND": "private_fund",
}


def canonical_json(obj) -> str:
    return silver.canonical_json(obj, ascii=True)


def read_json(path: Path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def _g(d, *path, default=None):
    cur = d
    for k in path:
        if isinstance(cur, dict) and cur.get(k) is not None:
            cur = cur[k]
        else:
            return default
    return cur


def _r(x):
    return round(x, 2) if isinstance(x, (int, float)) else None


def _find_blob(run_dir: Path, deal_id: str, doc_id: str):
    """The blob download saved for a document: both path components are the
    fleet's file-name token (`bronze.short_token`) of the Relay ids."""
    deal_dir = run_dir / "documents" / bronze.short_token(deal_id)
    matches = list(deal_dir.glob(bronze.short_token(doc_id) + ".*"))
    return matches[0] if matches else None


def _nodes_from_buyerdeals(body) -> list[dict]:
    edges = _g(body, "data", "buyer", "buyerDeals", "edges", default=[]) or []
    return [e["node"] for e in edges if isinstance(e, dict) and e.get("node")]


def _transfer_method(txn: dict) -> str | None:
    for e in (_g(txn, "transfers", "edges", default=[]) or []):
        t = _g(e, "node", "type")
        if t:
            return t
    return None


# ---- row builders --------------------------------------------------------

def _offering_row(last_seen_at: int, node: dict) -> tuple:
    """Immutable identity + entry terms (one row per investment)."""
    deal = node.get("deal") or {}
    company = deal.get("company") or {}
    fund = deal.get("fund") or {}
    pt = node.get("primaryTransaction") or {}
    return (
        deal.get("id"),
        ASSET_CLASS_KIND.get(company.get("assetClass")),
        company.get("assetClass"),
        company.get("id"), company.get("name"),
        fund.get("id"), fund.get("name"),
        _g(deal, "parentDeal", "name"),
        company.get("tickerSymbol"),
        deal.get("flavor"),
        deal.get("dateStart"),
        deal.get("sharePrice"),
        node.get("investmentSize"),     # basis
        pt.get("pricePostSplit"),       # purchase_price
        pt.get("sharesPostSplit"),      # shares_original
        "USD",
        last_seen_at,
        canonical_json(deal),
    )


def _position_events(snapshot_at: int, snapshot_iso: str, node: dict,
                     statements_for_deal: list[dict]) -> list[tuple]:
    """Replay one investment's valuation history into per-event rows: the
    original investment, each disposition (tender), each fund capital-account
    `statement` revaluation (funds only — `statements_for_deal`), and a
    terminal `exit` when EXITED, ordered by date. Ledger marks use CLOSED
    prices (entry, then tender prices); `statement` events mark to the
    statement's ending NAV. Returns positions rows (PK deal_external_id,
    event_seq)."""
    deal = node.get("deal") or {}
    pt = node.get("primaryTransaction") or {}
    deal_id = deal.get("id")
    purchase_price = pt.get("pricePostSplit")
    stage = node.get("dealStage")

    # Build raw events: (date, type, eid, shares_delta, dist_delta, price, nav, txn)
    raw = [(pt.get("transactionDate"), "investment", pt.get("id"),
            (pt.get("sharesPostSplit") or 0), 0.0, purchase_price, None,
            {k: v for k, v in pt.items() if k != "distributedTransactions"})]
    for dt in (pt.get("distributedTransactions") or []):
        raw.append((dt.get("transactionDate"), "disposition", dt.get("id"),
                    -(dt.get("sharesPostSplit") or 0), (dt.get("value") or 0),
                    dt.get("pricePostSplit"), None, dt))
    seen_periods = set()  # one statement revaluation per period_end (some periods ship a dup PDF)
    for st in statements_for_deal:
        pe, nav = st.get("period_end"), st.get("ending_nav")
        if not pe or nav is None or pe in seen_periods:
            continue
        seen_periods.add(pe)
        raw.append((pe, "statement", st["document_external_id"],
                    0.0, 0.0, None, nav,
                    {"capital_account_statement": st["document_external_id"]}))
    if stage == "EXITED":
        exit_date = node.get("dateExit") or _g(deal, "fund", "dateExit") or snapshot_iso
        exit_price = node.get("exitPrice")
        if exit_price is None:
            exit_price = _g(deal, "fund", "officialExitPrice")
        raw.append((exit_date, "exit", None, 0.0, 0.0, exit_price, None,
                    {"exit": True, "dateExit": node.get("dateExit"),
                     "officialExitPrice": _g(deal, "fund", "officialExitPrice")}))

    # Stable sort by date keeps investment first within a date.
    raw.sort(key=lambda e: e[0] or "")

    shares = 0.0
    dist = 0.0
    rows: list[tuple] = []
    for seq, (date, etype, eid, sh_d, di_d, price, nav, txn) in enumerate(raw):
        shares += sh_d
        dist += di_d
        if etype == "statement":
            mv = nav
            price = round(nav / shares, 4) if shares else None
        else:
            mv = shares * price if isinstance(shares, (int, float)) \
                and isinstance(price, (int, float)) else None
        cbr = _r(shares * purchase_price) if isinstance(purchase_price, (int, float)) else None
        status = "EXITED" if etype == "exit" else "CLOSED"
        is_open = 0 if etype == "exit" else (1 if shares > 1e-9 else 0)
        rows.append((deal_id, seq, date, etype, status, is_open, eid, _r(shares),
                     cbr, price, _r(mv), _r(dist), _r((mv or 0) + dist),
                     snapshot_at, canonical_json(txn)))
    return rows


def _cash_flow_rows(snapshot_at: int, node: dict) -> list[tuple]:
    deal_id = _g(node, "deal", "id")
    pt = node.get("primaryTransaction") or {}
    rows: list[tuple] = []
    if pt.get("id"):
        pt_payload = {k: v for k, v in pt.items() if k != "distributedTransactions"}
        rows.append((
            pt["id"], deal_id, snapshot_at, "purchase",
            pt.get("transactionDate"), node.get("investmentSize"),
            pt.get("executionFee"), _transfer_method(pt), "USD",
            "EquityZen purchase", canonical_json(pt_payload),
            pt.get("sharesPostSplit"), pt.get("pricePostSplit"),
        ))
    for dt in (pt.get("distributedTransactions") or []):
        if not dt.get("id"):
            continue
        rows.append((
            dt["id"], deal_id, snapshot_at, "distribution",
            dt.get("transactionDate"), dt.get("value"),
            dt.get("executionFee"), _transfer_method(dt), "USD",
            "EquityZen distribution", canonical_json(dt),
            dt.get("sharesPostSplit"), dt.get("pricePostSplit"),
        ))
    return rows


# ---- extraction and parse cache -------------------------------------------
#
# The same statement/K-1 PDF is hardlinked into every later dump, so a load
# finds each blob many times over. Its extractions are kept on disk between
# loads (collectorkit.extract.cached, keyed on the bytes and the engine),
# and its parse once per process here, by content hash: the hash and the
# parse are functions of the blob content alone, and bronze.sha256_file
# memoizes the hash by file identity.

# How a document is read: its `pdftotext -layout` text, and for a K-1 the
# face page's `pdftotext -bbox` word boxes (collectorkit.k1).
_LAYOUT = functools.partial(extract.pdftotext_layout, timeout=120)
_BBOX = functools.partial(extract.pdftotext_bbox, timeout=120)

_PARSE_CACHE: dict[str, dict] = {}            # sha256 hex -> {kind, cap, text[, k1]}


def _content_hash(blob: Path) -> str:
    """sha256 of a bronze blob; hardlinked copies are read and hashed once
    (bronze.sha256_file's memo)."""
    return bronze.sha256_file(blob)[0]


def _extracted(extractor, blob: Path, chash: str):
    """`extractor` over a blob through the extraction cache, or None when
    the PDF cannot be extracted (missing poppler, encrypted PDF, zip, …):
    the caller treats such a document as metadata-only."""
    try:
        return extract.cached(extractor, blob, chash,
                              extract.default_dir("equityzen"))
    except pdftotext.ExtractionError:
        return None


def _parse_pdf(blob: Path, chash: str) -> dict:
    """Extract, classify, and parse a PDF once per content hash. Returns
    {"kind", "cap", "text"} — the text classification, the statement
    parser's output and the text the K-1 parser reads (_parse_k1). Both are
    pure functions of the extracted text, so the cached result equals a
    fresh parse; the caller selects cap vs k1 by document type."""
    cached = _PARSE_CACHE.get(chash)
    if cached is None:
        layout = _extracted(_LAYOUT, blob, chash)
        text = "" if layout is None else layout.text()
        cached = {
            "kind": statements.classify(text),
            "cap": statements.parse_capital_account(text),
            "text": text,
        }
        _PARSE_CACHE[chash] = cached
    return cached


def _parse_k1(blob: Path, chash: str) -> dict:
    """statements.parse_k1 over a PDF the parse cache holds, once per
    content hash: its text, and its word boxes (`pdftotext -bbox`), which
    are extracted only when the text leaves an Item L figure empty."""
    cached = _PARSE_CACHE[chash]
    if "k1" not in cached:
        cached["k1"] = statements.parse_k1(
            cached["text"], lambda: _extracted(_BBOX, blob, chash))
    return cached["k1"]


# The K-1 reader's scope in document_derivations, and its generation: the
# code of statements.py and the kit modules it imports, and the keys of the
# two extractions it reads.
K1_SCOPE = "k1"
K1_GENERATION = ".".join((srcfp.parser_fingerprint([statements],
                                                   python=False),
                          extract.extraction_key(_LAYOUT),
                          extract.extraction_key(_BBOX)))


def _sweep_extract_cache(_conn, _load) -> None:
    """Remove the extraction-cache entries neither extractor above reads
    any more: those an engine upgrade or a code edit left behind."""
    extract.sweep(extract.default_dir("equityzen"),
                  {extract.extraction_key(e) for e in (_LAYOUT, _BBOX)})


def _pdf_blobs(run_dir: Path, nodes: dict) -> list[tuple[Path, str]]:
    """(blob, content_hash) for every PDF document blob in a run's offerings
    — the deduped input to the parallel pre-extraction below."""
    out = []
    for node in nodes.values():
        deal_id = _g(node, "deal", "id")
        for doc in (node.get("documents") or []):
            did = doc.get("id")
            if not did:
                continue
            blob = _find_blob(run_dir, deal_id, did)
            if blob and blob.suffix == ".pdf":
                out.append((blob, _content_hash(blob)))
    return out


def _prewarm_parses(blobs: list[tuple[Path, str]]) -> None:
    """Fill the parse cache for the blobs not yet seen, extracting their text
    concurrently. pdftotext is a subprocess that releases the GIL while it
    runs, so on a cold extraction cache a thread pool overlaps the launches
    instead of the parent idling in poll() through a strictly serial
    sequence. The subsequent serial load loop then finds every blob already
    cached, leaving SQL order untouched."""
    todo = {chash: blob for blob, chash in blobs if chash not in _PARSE_CACHE}
    if not todo:
        return
    workers = min(max(1, (os.cpu_count() or 2) // 2), len(todo))
    if workers == 1:
        for chash, blob in todo.items():
            _parse_pdf(blob, chash)
        return
    with ThreadPoolExecutor(max_workers=workers) as ex:
        list(ex.map(lambda item: _parse_pdf(item[1], item[0]), todo.items()))


def _parse_documents(run_dir: Path, snapshot_at: int, node: dict):
    """For each of an offering's documents: record the tax_documents metadata
    (with content_hash + local_path when the blob was fetched), and — when a
    PDF blob is present — parse capital-account statements and K-1s. Returns
    (tax_rows, capital_account_rows, k1_rows, parsed_statements) where the
    last is the list fed to the positions replay for funds. Parse work is
    served from the content-hash cache (see _prewarm_parses)."""
    deal_id = _g(node, "deal", "id")
    tax_rows, cap_rows, k1_rows, parsed = [], [], [], []
    for doc in (node.get("documents") or []):
        did = doc.get("id")
        if not did:
            continue
        dtype = doc.get("documentType")
        blob = _find_blob(run_dir, deal_id, did)
        chash = _content_hash(blob) if blob else None
        lpath = str(blob.relative_to(run_dir)) if blob else None
        retrieved = snapshot_at if blob else None
        tax_rows.append((
            did, deal_id, snapshot_at, dtype, doc.get("documentTypeDisplay"),
            doc.get("downloadUrl"), lpath, chash, retrieved, canonical_json(doc)))

        if not (blob and blob.suffix == ".pdf"):
            continue
        parse = _parse_pdf(blob, chash)
        kind = parse["kind"]
        if _is_statement(dtype, kind):
            p = parse["cap"]
            cap_rows.append(_cap_row(did, deal_id, p, chash, snapshot_at))
            if p["period_end"] and p["ending_nav"] is not None:
                parsed.append({"document_external_id": did,
                               "period_end": p["period_end"],
                               "ending_nav": p["ending_nav"]})
        elif _is_k1(dtype, kind):
            k1_rows.append(_k1_row(did, deal_id, _parse_k1(blob, chash),
                                   chash, snapshot_at))
    return tax_rows, cap_rows, k1_rows, parsed


def _is_statement(dtype, kind) -> bool:
    """Whether a document reads as a capital-account statement: its type
    or its text says so."""
    return dtype == "CAPITAL_ACCOUNT_STATEMENT" or kind == "capital_account"


def _is_k1(dtype, kind) -> bool:
    """Whether a document reads as a K-1; a statement never does."""
    return not _is_statement(dtype, kind) and (dtype == "K1" or kind == "k1")


def _cap_row(did, deal_id, p: dict, chash: str, snapshot_at: int) -> tuple:
    """A `capital_account_statements` row, in _INSERT_CAP's column order."""
    return (did, deal_id, p["period_end"], p["beginning_balance"],
            p["contributions"], p["withdrawals"], p["transfers"],
            p["profit_loss"], p["carried_interest"], p["ending_nav"],
            "USD", chash, snapshot_at, canonical_json(p))


_INSERT_CAP = (
    "INSERT OR REPLACE INTO capital_account_statements("
    "document_external_id, deal_external_id, period_end, "
    "beginning_balance, contributions, withdrawals, transfers, "
    "profit_loss, carried_interest, ending_nav, currency, "
    "content_hash, snapshot_at, payload) "
    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)")


def _k1_row(did, deal_id, k: dict, chash: str, snapshot_at: int) -> tuple:
    """A `k1_documents` row, in _INSERT_K1's column order."""
    return (did, deal_id, k["tax_year"], 1 if k["is_final"] else 0,
            k["beginning_capital"], k["current_year_income"],
            k["withdrawals_distributions"], k["ending_capital"],
            "USD", chash, snapshot_at, canonical_json(k))


_INSERT_K1 = (
    "INSERT OR REPLACE INTO k1_documents(document_external_id, "
    "deal_external_id, tax_year, is_final, beginning_capital, "
    "current_year_income, withdrawals_distributions, ending_capital, "
    "currency, content_hash, snapshot_at, payload) "
    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)")


# Each stored K-1 that a reader derived, with the document's type and the
# run its row came from; pending_derivations keeps those an older reader
# derived.
_STORED_K1_SQL = (
    "SELECT k.content_hash, k.document_external_id, k.deal_external_id, "
    "t.document_type, k.snapshot_at FROM k1_documents k "
    "JOIN document_derivations r ON r.content_sha256 = k.content_hash "
    "AND r.scope = ? "
    "JOIN tax_documents t ON t.document_external_id = k.document_external_id "
    "ORDER BY k.document_external_id")


def rederive_k1s(conn: sqlite3.Connection, bronze_dir: Path) -> None:
    """Derive again each stored K-1 that an older reader derived. A run
    reads only its own documents. So after the runs, the load reads each
    such K-1 again from the copy in the run its row came from, and writes
    what a `load --force` writes for it. So a reader change reaches every
    stored K-1. A stored statement is not read again (DESIGN.md §5).

    A document that no longer reads as a K-1 loses its row and its record.
    One that reads as a capital-account statement gets that run's
    statement row instead, unless a later run gave it one already. When
    that run is the offering's latest, its positions are replayed again,
    so the statement's NAV joins them. A document whose copy is gone or
    holds other bytes keeps its row and its record, and is logged."""
    stale = derivations.pending_derivations(
        conn, K1_SCOPE, K1_GENERATION, _STORED_K1_SQL, (K1_SCOPE,))
    replay: dict[str, int] = {}     # deal_id -> latest run given a statement
    for chash, did, deal_id, dtype, snapshot_at in stale:
        run = bronze.ts_slug(datetime.fromtimestamp(snapshot_at, timezone.utc))
        blob = _find_blob(bronze_dir / run, deal_id, did)
        if blob is None or not blob.is_file():
            log.warning("k1 %s: its copy in %s is gone; its row stays as an "
                        "older reader derived it", bronze.log_id(did), run)
            continue
        if _content_hash(blob) != chash:
            log.warning("k1 %s: the copy in %s holds other bytes; its row "
                        "stays as an older reader derived it",
                        bronze.log_id(did), run)
            continue
        parse = _parse_pdf(blob, chash)
        if _is_k1(dtype, parse["kind"]):
            conn.execute(_INSERT_K1, _k1_row(
                did, deal_id, _parse_k1(blob, chash), chash, snapshot_at))
            derivations.record_derivation(conn, chash, K1_SCOPE,
                                          K1_GENERATION, 1)
            continue
        conn.execute("DELETE FROM k1_documents "
                     "WHERE document_external_id = ?", (did,))
        conn.execute("DELETE FROM document_derivations "
                     "WHERE content_sha256 = ? AND scope = ?",
                     (chash, K1_SCOPE))
        if _is_statement(dtype, parse["kind"]) and conn.execute(
                "SELECT 1 FROM capital_account_statements "
                "WHERE document_external_id = ? AND snapshot_at > ?",
                (did, snapshot_at)).fetchone() is None:
            conn.execute(_INSERT_CAP, _cap_row(did, deal_id, parse["cap"],
                                               chash, snapshot_at))
            replay[deal_id] = max(snapshot_at, replay.get(deal_id, 0))
    for deal_id, snapshot_at in replay.items():
        _replay_positions(conn, bronze_dir, deal_id, snapshot_at)


def _replay_positions(conn: sqlite3.Connection, bronze_dir: Path,
                      deal_id: str, snapshot_at: int) -> None:
    """Replay an offering's positions again from the run at `snapshot_at`
    when that run is the offering's latest (`offerings.last_seen_at`):
    load_run replays each offering from every run that lists it, so the
    latest run's replay is the one that stays."""
    latest = conn.execute("SELECT last_seen_at FROM offerings "
                          "WHERE deal_external_id = ?", (deal_id,)).fetchone()
    if latest is None or latest[0] != snapshot_at:
        return
    run_dir = bronze_dir / bronze.ts_slug(
        datetime.fromtimestamp(snapshot_at, timezone.utc))
    node = _run_nodes(run_dir)[deal_id]
    parsed = _parse_documents(run_dir, snapshot_at, node)[3]
    _replace_positions(conn, snapshot_at, node, parsed)


def _after_runs(conn: sqlite3.Connection, load: loader.Load) -> None:
    """After the runs: the K-1s an older reader derived are derived again,
    then the extraction cache is swept."""
    with silver.transaction(conn):
        rederive_k1s(conn, load.bronze_dir)
    _sweep_extract_cache(conn, load)


# ---- per-run load --------------------------------------------------------

# The per-run counts, in load_run's order, as the log names them.
RUN_COUNTS = ("offerings", "position-events", "cash_flows", "tax_documents",
              "capital_account_statements", "k1_documents")


def _run_nodes(run_dir: Path) -> dict[str, dict]:
    """A run's offerings: deal_id -> node. The rich detail node wins; the
    list node stands in for an offering whose detail is absent."""
    nodes: dict[str, dict] = {}
    inv_path = run_dir / "investments.json"
    if inv_path.exists():
        for body in (read_json(inv_path) or {}).values():
            for node in _nodes_from_buyerdeals(body):
                did = _g(node, "deal", "id")
                if did:
                    nodes.setdefault(did, node)
    for detail in sorted((run_dir / "offerings").glob("*/detail.json")):
        for node in _nodes_from_buyerdeals(read_json(detail)):
            did = _g(node, "deal", "id")
            if did:
                nodes[did] = node
    return nodes


def _replace_positions(conn: sqlite3.Connection, snapshot_at: int,
                       node: dict, parsed: list[dict]) -> int:
    """Replace an offering's positions with the replay from one run's
    ledger. Fund statements (NAV) join it as revaluation events only for
    kind='private_fund'; SPVs stay tender-driven. Returns the number of
    events."""
    deal_id = _g(node, "deal", "id")
    snapshot_iso = datetime.fromtimestamp(
        snapshot_at, timezone.utc).date().isoformat()
    is_fund = _g(node, "deal", "company", "assetClass") == "ASSET_MULTI_COMPANY_FUND"
    conn.execute("DELETE FROM positions WHERE deal_external_id = ?", (deal_id,))
    rows = _position_events(snapshot_at, snapshot_iso, node,
                            parsed if is_fund else [])
    for row in rows:
        conn.execute(
            "INSERT INTO positions(deal_external_id, event_seq, as_of_date, "
            "event_type, status, is_open, event_external_id, shares_held, "
            "cost_basis_remaining, price_per_share, market_value, "
            "distributions_cumulative, total_value, snapshot_at, payload) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", row)
    return len(rows)


def load_run(conn: sqlite3.Connection, run: loader.Run) -> tuple[int, ...]:
    """Load one pending run dir and return its counts, in RUN_COUNTS
    order. The frame wraps it in one transaction and writes its
    `dump_runs` row (run_columns) after it."""
    run_dir, snapshot_at = run.dir, run.snapshot_at
    nodes = _run_nodes(run_dir)
    stats = dict.fromkeys(RUN_COUNTS, 0)

    # Extract+parse this run's PDF blobs concurrently before the serial load;
    # the cache makes the per-offering loop below a pure cache read.
    _prewarm_parses(_pdf_blobs(run_dir, nodes))

    for node in nodes.values():
        conn.execute(
            "INSERT OR REPLACE INTO offerings(deal_external_id, kind, "
            "asset_class, company_external_id, company_name, "
            "fund_external_id, fund_name, parent_deal_name, ticker_symbol, "
            "flavor, date_start, deal_share_price, basis, purchase_price, "
            "shares_original, currency, last_seen_at, payload) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            _offering_row(snapshot_at, node))
        stats["offerings"] += 1

        # Parse this offering's document blobs (if fetched): tax_documents
        # metadata + content hashes, capital-account statements, K-1s.
        tax_rows, cap_rows, k1_rows, parsed = _parse_documents(run_dir, snapshot_at, node)
        for row in tax_rows:
            conn.execute(
                "INSERT OR REPLACE INTO tax_documents(document_external_id, "
                "deal_external_id, snapshot_at, document_type, "
                "document_type_display, download_url, local_path, content_hash, "
                "retrieved_at, payload) VALUES (?,?,?,?,?,?,?,?,?,?)", row)
            stats["tax_documents"] += 1
        for row in cap_rows:
            conn.execute(_INSERT_CAP, row)
            stats["capital_account_statements"] += 1
        for row in k1_rows:
            conn.execute(_INSERT_K1, row)
            content_hash = row[9]
            derivations.record_derivation(conn, content_hash, K1_SCOPE,
                                          K1_GENERATION, 1)
            stats["k1_documents"] += 1

        stats["position-events"] += _replace_positions(
            conn, snapshot_at, node, parsed)

        for row in _cash_flow_rows(snapshot_at, node):
            conn.execute(
                "INSERT OR REPLACE INTO cash_flows(cash_flow_external_id, "
                "deal_external_id, snapshot_at, kind, flow_date, amount, "
                "execution_fee, method, currency, description, payload, "
                "shares, price_per_share) "
                "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", row)
            stats["cash_flows"] += 1

    return tuple(stats.values())


def run_columns(run: loader.Run) -> dict:
    """The run's `dump_runs` row: the frame's columns and the manifest."""
    return {**loader.default_run_columns(run),
            "payload": canonical_json(run.manifest)}


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--bronze-dir", type=Path, default=DEFAULT_BRONZE_DIR,
                   help="Bronze root to ingest from. Default: %(default)s.")
    p.add_argument("--silver-db", type=Path, default=DEFAULT_SILVER_DB,
                   help="Silver SQLite DB path. Default: %(default)s.")
    return p


# A walk that crashed or is still running holds a partial capture: its list
# nodes would replace the positions a complete run replayed from the full
# ledger, and its dump_runs row would count as an observation.
# pending_run_dirs skips such a run, a loaded one, and a slug that names no
# real instant. A loadable run's run.json is read as it is; one that cannot
# be read ends the load.
SPEC = loader.LoaderSpec(
    name="equityzen", schema=MIGRATIONS_DIR, load_run=load_run,
    parser=_parser, counts=RUN_COUNTS, run_columns=run_columns,
    after_runs=_after_runs, manifest=loader.MANIFEST_REQUIRED)


def main(argv: list[str]) -> int:
    return loader.main(SPEC, argv)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
