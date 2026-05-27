#!/usr/bin/env python3
"""
VIAC bronze -> silver loader.

Walks a bronze directory tree (as produced by download.py), applies
any pending schema migrations, then loads each not-yet-ingested
dump into a SQLite silver database. One dump = one transaction; the
`dump_runs` row is the last INSERT before COMMIT, so failures
mid-load roll the whole dump back and re-runs retry idempotently.

Usage:
    load.py [--silver-db PATH] [--bronze-dir PATH] [-v]

By default, walks /data (= ~/wealthdb/viac on the host) and
loads into /data/viac.db.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

logger = logging.getLogger("load")

# Bronze run dir name: YYYYMMDDTHHMMSSZ — same convention as the
# sibling repos.
RUN_DIR_RE = re.compile(r"^\d{8}T\d{6}Z$")

# Path to migrations dir relative to this script.
MIGRATIONS_DIR = Path(__file__).parent / "migrations"

# Default mount points inside the container.
DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_SILVER_DB = Path("/data/viac.db")


# ============================================================
# Taxonomy mappings
# ============================================================

# VIAC's top-level assetsByClasses key → wealthdb canonical
# asset_class. Anything VIAC adds that we haven't catalogued
# falls through to 'other'; the original VIAC label is preserved
# on positions.viac_asset_class for downstream review.
VIAC_ASSET_CLASS_MAP: dict[str, str] = {
    "EQUITIES": "equity",
    "BONDS": "bond",
    "REAL_ESTATE": "fund",          # property funds / REITs
    "ALTERNATIVES": "other",
    "COMMODITIES": "metal",
    "LIQUIDITY": "money_market",
}

# VIAC's transactions[].type → wealthdb canonical kind. The full
# list of observed values is in the silver schema header comment;
# anything new falls through to 'other'.
VIAC_TX_KIND_MAP: dict[str, str] = {
    "INTEREST":              "interest",
    "FEE_CHARGE":            "fee",
    "CONTRIBUTION":          "deposit",
    "TRADE_BUY":             "buy",
    "TRADE_SELL":            "sell",
    "DIVIDEND":              "dividend",
    "DIVIDEND_CANCELLATION": "corporate_action",
    "FUSION_NEW_FONDS":      "corporate_action",
    "FUSION_FONDS_RESET":    "corporate_action",
}


# ============================================================
# DB plumbing
# ============================================================

def open_db(path: Path) -> sqlite3.Connection:
    """Open (or create) the silver DB with sensible defaults."""
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), isolation_level=None)  # autocommit; we BEGIN/COMMIT explicitly
    conn.row_factory = sqlite3.Row
    # foreign_keys is a per-connection PRAGMA; it must be re-set
    # on every new connection regardless of what's in the schema.
    conn.execute("PRAGMA foreign_keys = ON;")
    conn.execute("PRAGMA journal_mode = WAL;")
    return conn


def current_schema_version(conn: sqlite3.Connection) -> int:
    """Read MAX(silver_schema_version), or 0 if schema_meta is absent."""
    row = conn.execute(
        "SELECT name FROM sqlite_master "
        "WHERE type='table' AND name='schema_meta'"
    ).fetchone()
    if not row:
        return 0
    row = conn.execute(
        "SELECT COALESCE(MAX(silver_schema_version), 0) AS v FROM schema_meta"
    ).fetchone()
    return int(row["v"])


def run_migrations(conn: sqlite3.Connection) -> int:
    """Apply any pending migration files in order. Returns the
    version after migrations have run."""
    if not MIGRATIONS_DIR.is_dir():
        raise RuntimeError(f"migrations dir not found: {MIGRATIONS_DIR}")
    current = current_schema_version(conn)
    files = sorted(MIGRATIONS_DIR.glob("[0-9][0-9][0-9][0-9]_*.sql"))
    if not files:
        logger.warning("no migration files found under %s", MIGRATIONS_DIR)
        return current
    for path in files:
        version = int(path.name[:4])
        if version <= current:
            continue
        logger.info("applying migration %s", path.name)
        # executescript() does an implicit COMMIT before running,
        # then runs the script under autocommit; the migration
        # file is responsible for its own BEGIN/COMMIT.
        conn.executescript(path.read_text(encoding="utf-8"))
        current = version
    return current


# ============================================================
# Helpers
# ============================================================

def ts_from_run_dir(name: str) -> int:
    """Parse YYYYMMDDTHHMMSSZ into Unix seconds UTC."""
    dt = datetime.strptime(name, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def iso_date_to_epoch(s: str | None) -> int | None:
    """Parse an ISO date (with or without time) into Unix seconds
    UTC at the day's midnight. Returns None for falsy /
    unparseable input."""
    if not s:
        return None
    head = s[:10]  # 'YYYY-MM-DD'
    try:
        dt = datetime.strptime(head, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except ValueError:
        return None


def iso_datetime_to_epoch(s: str | None) -> int | None:
    """Parse an ISO datetime into Unix seconds UTC. VIAC sometimes
    emits microsecond-precision timestamps without a timezone
    (e.g. `2021-11-28T14:18:13.222912`); treat those as UTC."""
    if not s:
        return None
    try:
        # Trim sub-second precision; ignore the literal Z if present.
        head = s.rstrip("Z")
        if "." in head:
            head = head.split(".", 1)[0]
        dt = datetime.strptime(head, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except (ValueError, TypeError):
        return None


def canonical_json(obj: Any) -> str:
    """Stable JSON serialisation for `payload` columns."""
    return json.dumps(obj, sort_keys=True, ensure_ascii=False, separators=(",", ":"))


def sha256_file(path: Path) -> tuple[str, int]:
    """Return (hex sha256, byte size) for a file."""
    h = hashlib.sha256()
    size = 0
    with path.open("rb") as f:
        while True:
            chunk = f.read(1 << 20)
            if not chunk:
                break
            h.update(chunk)
            size += len(chunk)
    return h.hexdigest(), size


def parse_account_id(account_external_id: str) -> tuple[str, str]:
    """Split a VIAC portfolio number into (product_code, portfolio_index).

    Examples:
      '3.NNN.NNN.NNN.NN' -> ('3', 'NN')   (Pillar-3a)
      '2.NNN.NNN.NNN.O'  -> ('2', 'O')    (PVB mandatory)
      '2.NNN.NNN.NNN.U'  -> ('2', 'U')    (PVB extra-mandatory)
    """
    parts = account_external_id.split(".")
    if len(parts) < 2:
        return ("", "")
    return (parts[0], parts[-1])


def synthesize_transaction_id(
    account_external_id: str,
    tx_type: str,
    value_date_epoch: int,
    amount_chf: float | None,
    document_number: str | None,
) -> str:
    """Deterministic synthetic id over the row's promoted columns.

    VIAC doesn't surface a stable per-event id on the wire — the
    `documentNumber` is shared across the two legs of corporate-
    action pairs and is occasionally absent. SHA-256 prefix gives
    us replay convergence: re-loading the same bronze produces the
    same id.
    """
    blob = f"{account_external_id}|{tx_type}|{value_date_epoch}|" \
           f"{amount_chf if amount_chf is not None else ''}|" \
           f"{document_number or ''}"
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()[:16]


def asset_class_for(viac_class: str | None) -> str:
    """Map VIAC's top-level assetsByClasses key to canonical."""
    if not viac_class:
        return "other"
    return VIAC_ASSET_CLASS_MAP.get(viac_class.upper(), "other")


def tx_kind_for(viac_type: str | None) -> str:
    if not viac_type:
        return "other"
    return VIAC_TX_KIND_MAP.get(viac_type, "other")


def etf_link_en(position_payload: dict[str, Any]) -> str | None:
    """Pull the English factsheet URL from VIAC's etfLinkByLanguages
    map when present. Convenience promote — gold can re-derive from
    payload if it wants other languages."""
    links = position_payload.get("etfLinkByLanguages")
    if isinstance(links, dict):
        return links.get("en") or links.get("EN")
    return None


# ============================================================
# Per-bronze-dump load
# ============================================================

def load_accounts_phase(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
) -> list[dict[str, Any]]:
    """Read portfolio-inventory.json + per-portfolio strategy.json,
    insert into `accounts`. Returns the inventory's flat portfolio
    list (p3a + pvb + inv) so the positions phase can iterate."""
    inventory_path = run_dir / "wealth" / "portfolio-inventory.json"
    if not inventory_path.is_file():
        logger.warning(
            "no wealth/portfolio-inventory.json in %s — skipping accounts phase",
            run_dir,
        )
        return []
    inventory = json.loads(inventory_path.read_text(encoding="utf-8"))
    flat: list[dict[str, Any]] = []
    for product_key in ("p3a", "pvb", "inv"):
        for entry in (inventory.get(product_key) or []):
            entry["_product_key"] = product_key
            flat.append(entry)

    for entry in flat:
        number = entry.get("number")
        if not number:
            logger.warning("portfolio entry missing 'number': %s", entry)
            continue
        product_code, portfolio_index = parse_account_id(number)

        # p3a portfolios get a strategy.json with current/target
        # strategy, custody bank, investment type, etc. PVB / INV
        # entries don't (download.py doesn't fetch them); the
        # corresponding columns stay NULL.
        strategy_payload: dict[str, Any] = {}
        strategy_path = run_dir / "positions" / number / "strategy.json"
        if strategy_path.is_file():
            try:
                strategy_payload = json.loads(strategy_path.read_text(encoding="utf-8"))
            except json.JSONDecodeError as e:
                logger.warning("strategy.json for %s parse failed: %s", number, e)

        current_strategy = strategy_payload.get("currentStrategy") or {}
        strategy_obj = entry.get("strategy") or {}

        merged_payload = {
            "inventory": entry,
            "strategy": strategy_payload or None,
        }

        conn.execute(
            """
            INSERT INTO accounts (
                snapshot_at, account_external_id,
                product_code, portfolio_index,
                name, state,
                inventory_index, inventory_sort_index,
                investment_focus, risk_level,
                strategy_id, strategy_is_custom,
                custody_bank, remainder_allocation,
                investment_type, interest_rate,
                foundation, portfolio_type,
                currency_code, payload
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
            """,
            (
                snapshot_at, number,
                product_code, portfolio_index,
                entry.get("name"), entry.get("state"),
                entry.get("index"), entry.get("sortIndex"),
                strategy_obj.get("investmentFocus"),
                strategy_obj.get("riskLevel"),
                current_strategy.get("id"),
                1 if current_strategy.get("custom") else (0 if "custom" in current_strategy else None),
                current_strategy.get("custodyBank"),
                current_strategy.get("remainderAllocation"),
                strategy_payload.get("investmentType"),
                strategy_payload.get("interestRate"),
                entry.get("foundation"),
                entry.get("portfolioType"),
                "CHF",
                canonical_json(merged_payload),
            ),
        )
    logger.info("  accounts phase: %d portfolio(s)", len(flat))
    return flat


def load_positions_phase(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
    portfolios: list[dict[str, Any]],
) -> None:
    """For each portfolio with an assets.json (Pillar-3a in the
    current download.py), insert positions + cash_balances and
    upsert instruments."""
    n_positions = 0
    n_cash = 0
    for entry in portfolios:
        number = entry.get("number")
        if not number:
            continue
        assets_path = run_dir / "positions" / number / "assets.json"
        if not assets_path.is_file():
            continue
        assets = json.loads(assets_path.read_text(encoding="utf-8"))

        # cash_balances: assets.cashAmount → balance_kind='cash'
        cash_amount = assets.get("cashAmount")
        if cash_amount is not None:
            conn.execute(
                """
                INSERT INTO cash_balances (
                    snapshot_at, account_external_id, currency,
                    balance_kind, amount, payload
                ) VALUES (?,?,?,?,?,?)
                """,
                (
                    snapshot_at, number, "CHF", "cash",
                    float(cash_amount),
                    canonical_json({
                        "source_field": "cashAmount",
                        "interestRate": assets.get("interestRate"),
                    }),
                ),
            )
            n_cash += 1

        # positions + instruments
        classes = assets.get("assetsByClasses") or {}
        for viac_class, items in classes.items():
            canonical = asset_class_for(viac_class)
            for pos in (items or []):
                isin = pos.get("isin")
                if not isin:
                    # ISIN-less positions shouldn't happen for VIAC's
                    # ETF/fund universe — log so we notice if it
                    # does and decide how to key them.
                    logger.warning(
                        "position without ISIN in %s (%s): %s",
                        number, viac_class, pos.get("name"),
                    )
                    continue
                conn.execute(
                    """
                    INSERT INTO positions (
                        snapshot_at, account_external_id, instrument_external_id,
                        asset_class, viac_asset_class, sub_asset_class,
                        currency_code, name,
                        amount, ratio, ratio_chf,
                        acquisition_price, asset_price, rate_of_return,
                        payload
                    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                    """,
                    (
                        snapshot_at, number, isin,
                        canonical, viac_class, pos.get("subAssetClassType"),
                        pos.get("currencyCode"), pos.get("name"),
                        pos.get("amount"),
                        pos.get("ratio"), pos.get("ratioInChf"),
                        pos.get("acquisitionPrice"),
                        pos.get("assetPrice"),
                        pos.get("rateOfReturn"),
                        canonical_json(pos),
                    ),
                )
                n_positions += 1
                conn.execute(
                    """
                    INSERT INTO instruments (
                        instrument_external_id, isin, name,
                        currency_code, asset_class,
                        viac_asset_class, sub_asset_class,
                        etf_link_en,
                        first_seen_at, last_seen_at, payload
                    ) VALUES (?,?,?,?,?,?,?,?,?,?,?)
                    ON CONFLICT(instrument_external_id) DO UPDATE SET
                        name = excluded.name,
                        currency_code = excluded.currency_code,
                        asset_class = excluded.asset_class,
                        viac_asset_class = excluded.viac_asset_class,
                        sub_asset_class = excluded.sub_asset_class,
                        etf_link_en = excluded.etf_link_en,
                        last_seen_at = MAX(instruments.last_seen_at, excluded.last_seen_at),
                        payload = excluded.payload
                    """,
                    (
                        isin, isin, pos.get("name"),
                        pos.get("currencyCode"), canonical,
                        viac_class, pos.get("subAssetClassType"),
                        etf_link_en(pos),
                        snapshot_at, snapshot_at,
                        canonical_json(pos),
                    ),
                )
    logger.info(
        "  positions phase: %d position(s), %d cash_balance(s)",
        n_positions, n_cash,
    )


def load_transactions_phase(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
) -> None:
    """Parse transactions/all.json — keyed by portfolio number,
    each value a list of {type, amountInChf, valueDate,
    balanceAfterBooking, documentNumber}."""
    tx_path = run_dir / "transactions" / "all.json"
    if not tx_path.is_file():
        logger.info("  transactions phase: no transactions/all.json — skipping")
        return
    tx_root = json.loads(tx_path.read_text(encoding="utf-8"))
    per_portfolio = tx_root.get("transactions") or {}
    n_total = 0
    n_skipped = 0
    for number, items in per_portfolio.items():
        for t in (items or []):
            tx_type = t.get("type")
            value_date = iso_date_to_epoch(t.get("valueDate"))
            if not tx_type or value_date is None:
                n_skipped += 1
                continue
            amount_chf = t.get("amountInChf")
            doc_num = t.get("documentNumber")
            tx_id = synthesize_transaction_id(
                number, tx_type, value_date, amount_chf, doc_num,
            )
            # INSERT OR REPLACE on the synthetic id → re-loading
            # converges. snapshot_at records the dump that first
            # observed the event; we keep the original first-
            # observation timestamp on conflict via a guard.
            existing = conn.execute(
                "SELECT snapshot_at FROM transactions WHERE transaction_external_id = ?",
                (tx_id,),
            ).fetchone()
            first_observation = (
                existing["snapshot_at"] if existing else snapshot_at
            )
            conn.execute(
                """
                INSERT OR REPLACE INTO transactions (
                    transaction_external_id, snapshot_at,
                    occurred_at, account_external_id,
                    type, kind,
                    amount_chf, balance_after_chf,
                    document_number, currency, payload
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    tx_id, first_observation,
                    value_date, number,
                    tx_type, tx_kind_for(tx_type),
                    amount_chf, t.get("balanceAfterBooking"),
                    doc_num, "CHF", canonical_json(t),
                ),
            )
            n_total += 1
    logger.info(
        "  transactions phase: %d loaded, %d skipped (missing fields)",
        n_total, n_skipped,
    )


def load_wealth_history_phase(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
) -> None:
    """Zip dailyWealth + dailyPerformance + dailyInvestedAmounts by
    date into one row per (snapshot, value_date)."""
    path = run_dir / "wealth" / "summary.json"
    if not path.is_file():
        logger.info("  wealth_history phase: no summary.json — skipping")
        return
    summary = json.loads(path.read_text(encoding="utf-8"))

    def by_date(series_name: str) -> dict[int, dict[str, Any]]:
        out: dict[int, dict[str, Any]] = {}
        for row in (summary.get(series_name) or []):
            d = iso_date_to_epoch(row.get("date"))
            if d is not None:
                out[d] = row
        return out

    wealth   = by_date("dailyWealth")
    perf     = by_date("dailyPerformance")
    invested = by_date("dailyInvestedAmounts")

    all_dates = sorted(set(wealth) | set(perf) | set(invested))
    for d in all_dates:
        w = wealth.get(d, {})
        p = perf.get(d, {})
        i = invested.get(d, {})
        conn.execute(
            """
            INSERT INTO wealth_history (
                snapshot_at, value_date,
                wealth_value, performance_value, invested_amount,
                payload
            ) VALUES (?,?,?,?,?,?)
            """,
            (
                snapshot_at, d,
                w.get("value"), p.get("value"), i.get("value"),
                canonical_json({"wealth": w, "performance": p, "invested": i}),
            ),
        )
    logger.info("  wealth_history phase: %d daily row(s)", len(all_dates))


def load_documents_phase(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
) -> None:
    """Read documents/index.json + the PDFs on disk, content-dedup,
    upsert `documents`. PDFs themselves stay on disk; this is just
    the index."""
    index_path = run_dir / "documents" / "index.json"
    if not index_path.is_file():
        logger.info("  documents phase: no index.json — skipping")
        return
    index = json.loads(index_path.read_text(encoding="utf-8"))
    if not isinstance(index, list):
        logger.warning(
            "documents/index.json is %s (expected list) — skipping",
            type(index).__name__,
        )
        return
    inserted = 0
    refreshed = 0
    missing = 0
    for d in index:
        doc_id = d.get("documentNumber")
        if not doc_id:
            continue
        pdf_path = run_dir / "documents" / f"{doc_id}.pdf"
        if not pdf_path.is_file():
            # Indexed but not downloaded — either gated by
            # `--with-transaction-documents` or a fetch failure.
            # The next dump that does download it will catch up.
            missing += 1
            continue
        sha, size = sha256_file(pdf_path)
        bronze_path = pdf_path.relative_to(run_dir.parent).as_posix()
        ts = iso_datetime_to_epoch(d.get("timestamp"))
        row = conn.execute(
            "SELECT 1 FROM documents WHERE content_sha256 = ?",
            (sha,),
        ).fetchone()
        if row is None:
            conn.execute(
                """
                INSERT INTO documents (
                    content_sha256, viac_doc_id,
                    doc_type, doc_subtype,
                    mime_type, language, product, timestamp,
                    file_size, bronze_path,
                    first_seen_at, last_seen_at, payload
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    sha, doc_id,
                    d.get("type") or "UNKNOWN", d.get("subType"),
                    d.get("mimeType"), d.get("language"), d.get("product"), ts,
                    size, bronze_path,
                    snapshot_at, snapshot_at, canonical_json(d),
                ),
            )
            inserted += 1
        else:
            conn.execute(
                """
                UPDATE documents SET
                    last_seen_at = MAX(last_seen_at, ?),
                    bronze_path = ?,
                    payload = ?
                WHERE content_sha256 = ?
                """,
                (snapshot_at, bronze_path, canonical_json(d), sha),
            )
            refreshed += 1
    logger.info(
        "  documents phase: %d new, %d existing refreshed, %d indexed-but-not-on-disk",
        inserted, refreshed, missing,
    )


def load_one_dump(
    conn: sqlite3.Connection,
    run_dir: Path,
    schema_version: int,
) -> None:
    """Load one bronze run dir into silver in a single transaction.
    Idempotency: caller has already checked dump_runs."""
    name = run_dir.name
    snapshot_at = ts_from_run_dir(name)
    logger.info("loading dump %s (snapshot_at=%d)", name, snapshot_at)

    run_json_path = run_dir / "run.json"
    if not run_json_path.is_file():
        raise RuntimeError(f"no run.json in {run_dir} — refusing to load")
    run_manifest = json.loads(run_json_path.read_text(encoding="utf-8"))
    docs_counts = run_manifest.get("documents") or {}

    conn.execute("BEGIN")
    try:
        portfolios = load_accounts_phase(conn, snapshot_at, run_dir)
        load_positions_phase(conn, snapshot_at, run_dir, portfolios)
        load_transactions_phase(conn, snapshot_at, run_dir)
        load_wealth_history_phase(conn, snapshot_at, run_dir)
        load_documents_phase(conn, snapshot_at, run_dir)

        # dump_runs row last → a failure mid-load rolls everything
        # back and the dump remains "not yet loaded" on re-run.
        conn.execute(
            """
            INSERT INTO dump_runs (
                snapshot_at, silver_schema_version, run_dir,
                dry_run, with_transaction_documents,
                bronze_docs_total, bronze_docs_fetched,
                bronze_docs_linked, bronze_docs_skipped,
                bronze_docs_errors,
                payload
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?)
            """,
            (
                snapshot_at, schema_version, str(run_dir),
                1 if run_manifest.get("dry_run") else 0,
                1 if run_manifest.get("with_transaction_documents") else 0,
                int(docs_counts.get("total", 0)),
                int(docs_counts.get("fetched", 0)),
                int(docs_counts.get("linked", 0)),
                int(docs_counts.get("skipped", 0)),
                int(docs_counts.get("errors", 0)),
                canonical_json(run_manifest),
            ),
        )
    except sqlite3.DatabaseError:
        conn.execute("ROLLBACK")
        raise
    conn.execute("COMMIT")


# ============================================================
# Orchestration
# ============================================================

def list_pending_dumps(
    conn: sqlite3.Connection, bronze_dir: Path,
) -> list[Path]:
    """Return run dirs under bronze_dir that haven't been loaded
    yet, in chronological order."""
    if not bronze_dir.is_dir():
        return []
    loaded = {
        row["snapshot_at"]
        for row in conn.execute("SELECT snapshot_at FROM dump_runs")
    }
    pending: list[Path] = []
    for d in sorted(bronze_dir.iterdir()):
        if not d.is_dir() or not RUN_DIR_RE.match(d.name):
            continue
        snapshot_at = ts_from_run_dir(d.name)
        if snapshot_at in loaded:
            continue
        if not (d / "run.json").is_file():
            logger.info("skipping %s — no run.json (still writing?)", d.name)
            continue
        pending.append(d)
    return pending


def do_load(args: argparse.Namespace) -> int:
    conn = open_db(args.silver_db)
    try:
        version = run_migrations(conn)
        logger.info("silver schema at version %d", version)
        pending = list_pending_dumps(conn, args.bronze_dir)
        if not pending:
            logger.info("no pending dumps under %s", args.bronze_dir)
            return 0
        logger.info("loading %d pending dump(s)", len(pending))
        for d in pending:
            load_one_dump(conn, d, version)
        return 0
    finally:
        conn.close()


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="VIAC bronze -> silver loader.",
    )
    p.add_argument(
        "--silver-db", type=Path, default=DEFAULT_SILVER_DB,
        help="Silver SQLite path (default: %(default)s).",
    )
    p.add_argument(
        "--bronze-dir", type=Path, default=DEFAULT_BRONZE_DIR,
        help=(
            "Parent dir holding YYYYMMDDTHHMMSSZ bronze run dirs "
            "(default: %(default)s)."
        ),
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
    return do_load(args)


if __name__ == "__main__":
    raise SystemExit(main())
