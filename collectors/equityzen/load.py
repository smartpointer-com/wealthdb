#!/usr/bin/env python3
"""equityzen silver loader.

Parses the bronze captured by download.py into the source-shaped SQLite
silver DB, the input contract to the gold engine. Idempotent: bronze runs
already recorded in `dump_runs` are skipped unless --force.

Per bronze run (`~/wealthdb/equityzen/<UTC-ts>/`):

  * investments.json             {stage: getBuyerInvestments body} — the
                                 per-stage lists (stage label + fallback node).
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

Valuation uses CLOSED-deal prices only (purchase + tenders); order-book
asks and deal-era implied valuations are excluded. K-1 / fund-statement
NAVs (which would enrich fund / un-tendered valuations) are a deferred
parsing increment. See DESIGN.md §4/§5/§6.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import logging
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

import statements  # PDF parsers for capital-account statements + K-1s

log = logging.getLogger("equityzen.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS_DIR = HERE / "migrations"

DEFAULT_DEST = Path("/data")
DEFAULT_DB = Path("/data/equityzen.db")

# EquityZen assetClass -> wealthdb offering kind (DESIGN.md §6.3).
ASSET_CLASS_KIND = {
    "ASSET_COMPANY": "spv",
    "ASSET_MULTI_COMPANY_FUND": "private_fund",
}


def canonical_json(obj) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), default=str)


def read_json(path: Path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def open_db(path: Path) -> sqlite3.Connection:
    """Default-isolation connection so `with conn:` brackets each run's load
    in a BEGIN/COMMIT (ROLLBACK on error)."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path))
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys = ON;")
    conn.execute("PRAGMA journal_mode = WAL;")
    return conn


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


def _slug(s: str) -> str:
    """Match download.py's blob naming: sha256(id)[:16]."""
    return hashlib.sha256(s.encode("utf-8")).hexdigest()[:16]


def _find_blob(run_dir: Path, deal_id: str, doc_id: str):
    matches = list((run_dir / "documents" / _slug(deal_id)).glob(_slug(doc_id) + ".*"))
    return matches[0] if matches else None


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


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
        ))
    for dt in (pt.get("distributedTransactions") or []):
        if not dt.get("id"):
            continue
        rows.append((
            dt["id"], deal_id, snapshot_at, "distribution",
            dt.get("transactionDate"), dt.get("value"),
            dt.get("executionFee"), _transfer_method(dt), "USD",
            "EquityZen distribution", canonical_json(dt),
        ))
    return rows


def _parse_documents(run_dir: Path, snapshot_at: int, node: dict):
    """For each of an offering's documents: record the tax_documents metadata
    (with content_hash + local_path when the blob was fetched), and — when a
    PDF blob is present — parse capital-account statements and K-1s. Returns
    (tax_rows, capital_account_rows, k1_rows, parsed_statements) where the
    last is the list fed to the positions replay for funds."""
    deal_id = _g(node, "deal", "id")
    tax_rows, cap_rows, k1_rows, parsed = [], [], [], []
    for doc in (node.get("documents") or []):
        did = doc.get("id")
        if not did:
            continue
        dtype = doc.get("documentType")
        blob = _find_blob(run_dir, deal_id, did)
        chash = _sha256_file(blob) if blob else None
        lpath = str(blob.relative_to(run_dir)) if blob else None
        retrieved = snapshot_at if blob else None
        tax_rows.append((
            did, deal_id, snapshot_at, dtype, doc.get("documentTypeDisplay"),
            doc.get("downloadUrl"), lpath, chash, retrieved, canonical_json(doc)))

        if not (blob and blob.suffix == ".pdf"):
            continue
        text = statements.pdf_text(blob)
        kind = statements.classify(text)
        if dtype == "CAPITAL_ACCOUNT_STATEMENT" or kind == "capital_account":
            p = statements.parse_capital_account(text)
            cap_rows.append((
                did, deal_id, p["period_end"], p["beginning_balance"],
                p["contributions"], p["withdrawals"], p["transfers"],
                p["profit_loss"], p["carried_interest"], p["ending_nav"],
                "USD", chash, snapshot_at, canonical_json(p)))
            if p["period_end"] and p["ending_nav"] is not None:
                parsed.append({"document_external_id": did,
                               "period_end": p["period_end"],
                               "ending_nav": p["ending_nav"]})
        elif dtype == "K1" or kind == "k1":
            k = statements.parse_k1(text)
            k1_rows.append((
                did, deal_id, k["tax_year"], 1 if k["is_final"] else 0,
                k["beginning_capital"], k["current_year_income"],
                k["withdrawals_distributions"], k["ending_capital"],
                "USD", chash, snapshot_at, canonical_json(k)))
    return tax_rows, cap_rows, k1_rows, parsed


# ---- per-run load --------------------------------------------------------

def load_run(conn: sqlite3.Connection, run_dir: Path, force: bool) -> dict:
    snapshot_at = bronze.parse_run_ts(run_dir.name)
    snapshot_iso = datetime.fromtimestamp(snapshot_at, timezone.utc).date().isoformat()
    already = conn.execute(
        "SELECT 1 FROM dump_runs WHERE snapshot_at = ?", (snapshot_at,)
    ).fetchone()
    if already is not None and not force:
        return {"name": run_dir.name, "skipped": True}

    # deal_id -> (stage, node, body); prefer the rich detail node, fall back
    # to the list node for any offering whose detail is absent.
    nodes: dict[str, tuple] = {}
    inv_path = run_dir / "investments.json"
    if inv_path.exists():
        for stage, body in (read_json(inv_path) or {}).items():
            for node in _nodes_from_buyerdeals(body):
                did = _g(node, "deal", "id")
                if did:
                    nodes.setdefault(did, (stage, node, None))
    for detail in sorted((run_dir / "offerings").glob("*/detail.json")):
        body = read_json(detail)
        for node in _nodes_from_buyerdeals(body):
            did = _g(node, "deal", "id")
            if did:
                prev = nodes.get(did)
                stage = node.get("dealStage") or (prev[0] if prev else None)
                nodes[did] = (stage, node, body)

    stats = {"name": run_dir.name, "snapshot_at": snapshot_at, "skipped": False,
             "offerings": 0, "positions": 0, "cash_flows": 0, "tax_documents": 0,
             "capital_account_statements": 0, "k1_documents": 0}

    with conn:  # BEGIN … COMMIT (ROLLBACK on error)
        if force:
            conn.execute("DELETE FROM dump_runs WHERE snapshot_at = ?", (snapshot_at,))

        for did, (stage, node, body) in nodes.items():
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
                conn.execute(
                    "INSERT OR REPLACE INTO capital_account_statements("
                    "document_external_id, deal_external_id, period_end, "
                    "beginning_balance, contributions, withdrawals, transfers, "
                    "profit_loss, carried_interest, ending_nav, currency, "
                    "content_hash, snapshot_at, payload) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", row)
                stats["capital_account_statements"] += 1
            for row in k1_rows:
                conn.execute(
                    "INSERT OR REPLACE INTO k1_documents(document_external_id, "
                    "deal_external_id, tax_year, is_final, beginning_capital, "
                    "current_year_income, withdrawals_distributions, ending_capital, "
                    "currency, content_hash, snapshot_at, payload) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", row)
                stats["k1_documents"] += 1

            # Event-sourced positions: replace this deal's full history with
            # the replay from this run's ledger. Fund statements (NAV) are
            # injected as revaluation events only for kind='private_fund';
            # SPVs stay tender-driven.
            is_fund = _g(node, "deal", "company", "assetClass") == "ASSET_MULTI_COMPANY_FUND"
            conn.execute("DELETE FROM positions WHERE deal_external_id = ?", (did,))
            for row in _position_events(snapshot_at, snapshot_iso, node,
                                        parsed if is_fund else []):
                conn.execute(
                    "INSERT INTO positions(deal_external_id, event_seq, as_of_date, "
                    "event_type, status, is_open, event_external_id, shares_held, "
                    "cost_basis_remaining, price_per_share, market_value, "
                    "distributions_cumulative, total_value, snapshot_at, payload) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", row)
                stats["positions"] += 1

            for row in _cash_flow_rows(snapshot_at, node):
                conn.execute(
                    "INSERT OR REPLACE INTO cash_flows(cash_flow_external_id, "
                    "deal_external_id, snapshot_at, kind, flow_date, amount, "
                    "execution_fee, method, currency, description, payload) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?)", row)
                stats["cash_flows"] += 1

        manifest_path = run_dir / "run.json"
        manifest = canonical_json(read_json(manifest_path)) if manifest_path.exists() else None
        conn.execute(
            "INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, "
            "payload) VALUES (?,?,?,?)",
            (snapshot_at, silver.current_schema_version(conn),
             str(run_dir.resolve()), manifest))
    return stats


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--dest", type=Path, default=DEFAULT_DEST,
                   help="Bronze root to ingest from. Default: %(default)s.")
    p.add_argument("--db", type=Path, default=DEFAULT_DB,
                   help="Silver SQLite DB path. Default: %(default)s.")
    p.add_argument("--force", action="store_true",
                   help="Re-load snapshots already recorded in dump_runs.")
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    conn = open_db(args.db)
    silver.apply_migrations(conn, MIGRATIONS_DIR)

    runs = list(bronze.iter_run_dirs(args.dest))
    log.info("found %d bronze run(s) under %s", len(runs), args.dest)
    for run_dir in runs:
        stats = load_run(conn, run_dir, args.force)
        if stats.get("skipped"):
            log.info("  %s: skipped (already loaded)", stats["name"])
        else:
            log.info("  %s: offerings=%d position-events=%d cash_flows=%d "
                     "tax_documents=%d capital_account_statements=%d k1_documents=%d",
                     stats["name"], stats["offerings"], stats["positions"],
                     stats["cash_flows"], stats["tax_documents"],
                     stats["capital_account_statements"], stats["k1_documents"])
    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
