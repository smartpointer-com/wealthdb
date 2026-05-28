#!/usr/bin/env python3
"""
Relevate bronze -> silver loader.

Walks a bronze directory tree (as produced by download.py), applies
any pending schema migrations, then loads each not-yet-ingested
dump into a SQLite silver database. One dump = one transaction; the
`dump_runs` row is the last INSERT before COMMIT, so failures
mid-load roll the whole dump back and re-runs retry idempotently.

Usage:
    load.py [--silver-db PATH] [--bronze-dir PATH] [-v]

By default, walks /data (= ~/wealthdb/relevate on the host) and
loads into /data/relevate.db.
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
DEFAULT_SILVER_DB = Path("/data/relevate.db")


# ============================================================
# DB plumbing
# ============================================================

def open_db(path: Path) -> sqlite3.Connection:
    """Open (or create) the silver DB with sensible defaults."""
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), isolation_level=None)  # autocommit; we BEGIN/COMMIT explicitly
    conn.row_factory = sqlite3.Row
    # foreign_keys is a per-connection PRAGMA; it must be re-set on
    # every new connection regardless of what's in the schema file.
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
    """Apply any pending migration files in order. Returns the version
    after migrations have run."""
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
        # file is responsible for its own BEGIN/COMMIT (and rolls
        # itself back on error).
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
    """Parse an ISO date (with or without time) into Unix seconds UTC
    at the day's midnight. Returns None for falsy / unparseable
    input."""
    if not s:
        return None
    head = s[:10]  # 'YYYY-MM-DD'
    try:
        dt = datetime.strptime(head, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except ValueError:
        return None


def canonical_json(obj: Any) -> str:
    """Stable JSON serialisation for `payload` columns. Sorted keys
    so two equivalent payloads compare equal byte-for-byte (useful
    for any later content-dedup pass)."""
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


# Heuristic: derive a stable doc_kind label from the fileName the
# index reports. Order matters — keep more-specific matches before
# more-general ones (e.g. 'Fee statement' before 'statement').
DOC_KIND_PATTERNS: tuple[tuple[str, str], ...] = (
    ("Fee statement", "quarterly_fee"),
    ("Quarterly Report", "quarterly_report"),
    ("Credit note", "credit_note"),
    ("Pension Agreement", "pension_agreement"),
    ("Pension Plan", "pension_plan"),
    ("Investor profile", "investor_profile"),
    ("Leaving statement", "leaving_statement"),
)


def doc_kind_from_filename(file_name: str | None) -> str:
    if not file_name:
        return "other"
    for needle, label in DOC_KIND_PATTERNS:
        if needle.lower() in file_name.lower():
            return label
    return "other"


def slug_for_account(external_id: str) -> str:
    """Same slug download.py uses for the bronze portfolio dir."""
    return hashlib.sha256(external_id.encode("utf-8")).hexdigest()[:16]


# ============================================================
# Per-bronze-dump load
# ============================================================

def load_accounts_phase(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
) -> dict[str, Any] | None:
    """Read investment-overview.json, insert into `accounts` +
    `cash_balances`. Returns the parsed top-level document so the
    portfolios phase can drive off it."""
    overview_path = run_dir / "accounts" / "investment-overview.json"
    if not overview_path.is_file():
        logger.warning(
            "no investment-overview.json in %s — skipping accounts phase",
            run_dir,
        )
        return None
    overview = json.loads(overview_path.read_text(encoding="utf-8"))
    portfolios = overview.get("portfolios") or []

    cash_balance_kinds: tuple[tuple[str, str], ...] = (
        # (source-field-name, balance_kind)
        ("cashBalance", "cash"),
        ("investedAmount", "invested"),
        ("currentValue", "current"),
        ("securitiesBalance", "securities"),
        ("savingValuation", "saving"),
        ("investmentValuation", "investment"),
        ("valuationVirtual", "virtual"),
        ("targetInvestment", "target_inv"),
        ("targetSaving", "target_sav"),
    )

    for p in portfolios:
        external_id = p.get("externalId")
        internal_id = p.get("id")
        if external_id is None or internal_id is None:
            logger.warning(
                "portfolio missing externalId or id: %s",
                {k: p.get(k) for k in ("id", "externalId")},
            )
            continue
        product = p.get("product") or {}
        currency = (p.get("currency") or {}).get("currencyCode") or "CHF"
        conn.execute(
            """
            INSERT INTO accounts (
                snapshot_at, account_external_id, portfolio_internal_id,
                contact_id, contact_group_id,
                product_key, product_name, product_offer_id,
                currency_code, name,
                portfolio_type_id, portfolio_status_id, portfolio_proposal_id,
                is_active, is_read_only,
                first_investment_date, create_date,
                payload
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
            """,
            (
                snapshot_at, external_id, internal_id,
                p.get("contactId"), p.get("contactGroupId"),
                product.get("key"), product.get("name"), product.get("productOfferId"),
                currency, p.get("name"),
                p.get("portfolioTypeId"), p.get("portfolioStatusId"),
                p.get("portfolioProposalId"),
                1 if p.get("isActive") else 0,
                1 if p.get("isReadOnly") else 0,
                p.get("firstInvestmentDate"), p.get("createDate"),
                canonical_json(p),
            ),
        )
        for field_name, kind in cash_balance_kinds:
            amount = p.get(field_name)
            if amount is None:
                continue
            conn.execute(
                """
                INSERT INTO cash_balances (
                    snapshot_at, account_external_id, currency,
                    balance_kind, amount, payload
                ) VALUES (?,?,?,?,?,?)
                """,
                (
                    snapshot_at, external_id, currency, kind,
                    float(amount),
                    canonical_json({"source_field": field_name}),
                ),
            )
    logger.info(
        "  accounts phase: %d portfolio(s)", len(portfolios),
    )
    return overview


def load_portfolio_artefacts(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
    portfolio: dict[str, Any],
) -> None:
    """For one portfolio, load modelportfolio (positions +
    instruments) and performance.json (performance_points)."""
    external_id = portfolio.get("externalId")
    if not external_id:
        return
    slug = slug_for_account(external_id)
    pdir = run_dir / "portfolios" / slug
    if not pdir.is_dir():
        logger.warning("no portfolios dir for %s in %s", slug, run_dir)
        return

    # modelportfolio -> positions + instruments
    mp_path = pdir / "modelportfolio.json"
    if mp_path.is_file():
        mp = json.loads(mp_path.read_text(encoding="utf-8"))
        for pos in (mp.get("positions") or []):
            security = pos.get("security") or {}
            sec_id = security.get("id")
            if sec_id is None:
                continue
            iid = str(sec_id)
            isin = security.get("isin")
            asset_class = (security.get("assetClass") or {})
            country = (security.get("country") or {})
            conn.execute(
                """
                INSERT INTO positions (
                    snapshot_at, account_external_id, instrument_external_id,
                    isin, instrument_name,
                    asset_class, asset_class_external,
                    country_code,
                    allocation, trading_price, trading_unit, face_value,
                    payload
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    snapshot_at, external_id, iid,
                    isin, security.get("name"),
                    asset_class.get("name"), asset_class.get("externalId"),
                    country.get("countryCode"),
                    pos.get("allocation"),
                    security.get("tradingPrice"),
                    security.get("tradingUnit"),
                    security.get("faceValue"),
                    canonical_json(pos),
                ),
            )
            # Instruments are slow-changing master data: upsert on
            # instrument_external_id, advancing last_seen_at.
            conn.execute(
                """
                INSERT INTO instruments (
                    instrument_external_id, isin, name,
                    asset_class, asset_class_external,
                    country_code,
                    first_seen_at, last_seen_at, payload
                ) VALUES (?,?,?,?,?,?,?,?,?)
                ON CONFLICT(instrument_external_id) DO UPDATE SET
                    isin = excluded.isin,
                    name = excluded.name,
                    asset_class = excluded.asset_class,
                    asset_class_external = excluded.asset_class_external,
                    country_code = excluded.country_code,
                    last_seen_at = MAX(instruments.last_seen_at, excluded.last_seen_at),
                    payload = excluded.payload
                """,
                (
                    iid, isin, security.get("name"),
                    asset_class.get("name"), asset_class.get("externalId"),
                    country.get("countryCode"),
                    snapshot_at, snapshot_at, canonical_json(security),
                ),
            )

    # performance -> performance_points
    perf_path = pdir / "performance.json"
    if perf_path.is_file():
        perf = json.loads(perf_path.read_text(encoding="utf-8"))
        currency = (perf.get("currency") or {}).get("currencyCode")
        for v in (perf.get("values") or []):
            value_date = iso_date_to_epoch(v.get("date"))
            if value_date is None:
                continue
            conn.execute(
                """
                INSERT INTO performance_points (
                    snapshot_at, account_external_id, value_date,
                    currency, value, amount,
                    cash_balance, securities_balance,
                    cash_flow, deposits, payouts,
                    profit, profit_all, profit_virtual,
                    amount_virtual, additional_value,
                    payload
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    snapshot_at, external_id, value_date,
                    currency,
                    v.get("value"), v.get("amount"),
                    v.get("cashBalance"), v.get("securitiesBalance"),
                    v.get("cashFlow"), v.get("deposits"), v.get("payouts"),
                    v.get("profit"), v.get("profitAll"), v.get("profitVirtual"),
                    v.get("amountVirtual"), v.get("additionalValue"),
                    canonical_json(v),
                ),
            )


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
        logger.info("  documents phase: no index — skipping")
        return
    index = json.loads(index_path.read_text(encoding="utf-8"))
    docs = index.get("documents") or []
    inserted = 0
    refreshed = 0
    missing = 0
    for d in docs:
        doc_id = d.get("id")
        if doc_id is None:
            continue
        pdf_path = run_dir / "documents" / f"{doc_id}.pdf"
        if not pdf_path.is_file():
            # Could be an .unexpected.<ext> path; the documents
            # phase only catalogs PDFs that landed correctly.
            missing += 1
            continue
        sha, size = sha256_file(pdf_path)
        # Store the path relative to the bronze ROOT (not the
        # run dir): the run-ts dirname is the first segment.
        # Consumers join with their own bronze root, so the same
        # silver row resolves both inside the container (root =
        # /data) and on the host (root = ~/wealthdb/relevate).
        bronze_path = pdf_path.relative_to(run_dir.parent).as_posix()
        file_name = d.get("fileName") or ""
        # INSERT first; on conflict, update last_seen_at and any
        # fields that may differ in metadata between dumps.
        row = conn.execute(
            "SELECT 1 FROM documents WHERE content_sha256 = ?",
            (sha,),
        ).fetchone()
        if row is None:
            conn.execute(
                """
                INSERT INTO documents (
                    content_sha256, relevate_doc_id, relevate_external_id,
                    file_name, file_size, doc_kind,
                    document_type_code, category_code, document_year,
                    create_date, valid_till,
                    foundation_id, contract_id, owner_id,
                    bronze_path,
                    first_seen_at, last_seen_at, payload
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    sha, doc_id, d.get("externalId"),
                    file_name, size, doc_kind_from_filename(file_name),
                    d.get("documentType"), d.get("category"), d.get("documentYear"),
                    d.get("createDate"), d.get("validTill"),
                    d.get("foundationId"), d.get("contractId"), d.get("ownerId"),
                    bronze_path,
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
        "  documents phase: %d new, %d existing refreshed, %d missing-on-disk",
        inserted, refreshed, missing,
    )


def load_one_dump(
    conn: sqlite3.Connection,
    run_dir: Path,
    schema_version: int,
) -> None:
    """Load one bronze run dir into silver in a single transaction.
    Idempotency: caller must have already checked dump_runs."""
    name = run_dir.name
    snapshot_at = ts_from_run_dir(name)
    logger.info("loading dump %s (snapshot_at=%d)", name, snapshot_at)

    run_json_path = run_dir / "run.json"
    if not run_json_path.is_file():
        raise RuntimeError(f"no run.json in {run_dir} — refusing to load")
    run_manifest = json.loads(run_json_path.read_text(encoding="utf-8"))

    conn.execute("BEGIN")
    try:
        overview = load_accounts_phase(conn, snapshot_at, run_dir)
        if overview is not None:
            for p in (overview.get("portfolios") or []):
                load_portfolio_artefacts(conn, snapshot_at, run_dir, p)
        load_documents_phase(conn, snapshot_at, run_dir)

        # The dump_runs row is the last INSERT before COMMIT, so a
        # failure mid-load rolls everything back and the dump
        # remains "not yet loaded" — re-runs converge.
        # Store run_dir as the bare run-ts dirname (relative to
        # bronze root), same convention as documents.bronze_path —
        # consumers join with their own bronze root and the same
        # silver row resolves both in-container and on the host.
        conn.execute(
            """
            INSERT INTO dump_runs (
                snapshot_at, silver_schema_version, run_dir,
                mode, dry_run, state_minted_at,
                bronze_files_total, bronze_errors_total, payload
            ) VALUES (?,?,?,?,?,?,?,?,?)
            """,
            (
                snapshot_at, schema_version, run_dir.name,
                run_manifest.get("mode"),
                1 if run_manifest.get("dry_run") else 0,
                iso_date_to_epoch(run_manifest.get("state_minted_at")),
                len(run_manifest.get("files") or []),
                len(run_manifest.get("errors") or []),
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
    """Return run dirs under bronze_dir that haven't been loaded yet,
    in chronological order."""
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
        # Skip in-flight dumps that don't have a final run.json yet.
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
        description="Relevate bronze -> silver loader.",
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
