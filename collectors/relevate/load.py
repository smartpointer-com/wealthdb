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

By default, walks /data (= $XDG_DATA_HOME/wealthdb/relevate on the host) and
loads into /data/relevate.db.
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
from typing import Any

from collectorkit import bronze, cli, parse, silver

from pdf_parsers import parse_credit_note, parse_quarterly_report

logger = logging.getLogger("load")

# Path to migrations dir relative to this script.
MIGRATIONS_DIR = Path(__file__).parent / "migrations"

# Default mount points inside the container.
DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_SILVER_DB = Path("/data/relevate.db")


# ============================================================
# DB plumbing
# ============================================================

# Silver DB plumbing (open_db + schema versioning + the migration runner)
# lives in collectorkit.silver. relevate uses the manual-transaction model
# (isolation_level=None; explicit BEGIN/COMMIT per dump) that silver.open_db
# provides, so it shares that opener directly.
open_db = silver.open_db


# ============================================================
# Helpers
# ============================================================

def canonical_json(obj: Any) -> str:
    """Stable JSON serialisation for `payload` columns. Sorted keys
    so two equivalent payloads compare equal byte-for-byte (useful
    for any later content-dedup pass)."""
    return json.dumps(obj, sort_keys=True, ensure_ascii=False, separators=(",", ":"))


# Relevate's API serves fileNames in German on every observed
# response; the older English needles are kept as defence in case
# the API ever serves an en-locale flag we don't pass today.
DOC_KIND_PATTERNS: tuple[tuple[str, str], ...] = (
    ("Fee statement",        "quarterly_fee"),
    ("Gebührenabrechnung",   "quarterly_fee"),
    ("Quarterly Report",     "quarterly_report"),
    ("Quartalsbericht",      "quarterly_report"),
    ("Credit note",          "credit_note"),
    ("Gutschriftsanzeige",   "credit_note"),
    ("Pension Agreement",    "pension_agreement"),
    ("Vorsorgevereinbarung", "pension_agreement"),
    ("Pension Plan",         "pension_plan"),
    ("Investor profile",     "investor_profile"),
    ("Anlegerprofil",        "investor_profile"),
    ("Leaving statement",    "leaving_statement"),
    ("Eröffnung / Eintritt", "account_opening"),
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
            value_date = parse.iso_date_to_epoch(v.get("date"))
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
        sha, size = bronze.sha256_file(pdf_path)
        # Store the path relative to the bronze ROOT (not the
        # run dir): the run-ts dirname is the first segment.
        # Consumers join with their own bronze root, so the same
        # silver row resolves both inside the container (root =
        # /data) and on the host (root = $XDG_DATA_HOME/wealthdb/relevate).
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
    snapshot_at = bronze.parse_run_ts(name)
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
                parse.iso_date_to_epoch(run_manifest.get("state_minted_at")),
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
    for d in bronze.iter_run_dirs(bronze_dir):
        snapshot_at = bronze.parse_run_ts(d.name)
        if snapshot_at in loaded:
            continue
        # Skip in-flight dumps that don't have a final run.json yet.
        run_json = d / "run.json"
        if not run_json.is_file():
            logger.info("skipping %s — no run.json (still writing?)", d.name)
            continue
        # Since run.json is now written incrementally from run-dir
        # creation (born status="in-progress"), its mere presence no
        # longer means the dump finished. Skip a dump the walk never
        # completed — a crashed walk left "in-progress", a --dry-run
        # shell left "dry-run" — so a partial capture never lands in
        # silver. A statusless manifest predates the status field and
        # is loaded as before (backward compat).
        try:
            status = json.loads(
                run_json.read_text(encoding="utf-8")).get("status")
        except (OSError, json.JSONDecodeError):
            # Unreadable/corrupt here is not proof of incompleteness;
            # fall through and let load_one_dump surface any real error.
            status = None
        if status in ("in-progress", "dry-run"):
            logger.info(
                "skipping %s — run.json status=%r (not a complete dump)",
                d.name, status)
            continue
        pending.append(d)
    return pending


def load_historical_snapshots(
    conn: sqlite3.Connection, bronze_root: Path,
) -> None:
    """
    Parse every Quartalsbericht PDF currently indexed in `documents`,
    INSERT OR REPLACE into `historical_position_snapshots` +
    `historical_cash_balances`.

    Idempotent: re-running produces the same rows because both target
    tables are keyed on (snapshot_at, account_external_id, isin or
    balance_kind) — the dimensions parsed from the PDF — not on
    document_id. One PDF parse error doesn't kill the loop; logged
    and skipped, and the next `load` attempt will retry.

    Runs in its own transaction (separate from the per-dump
    transactions) so a parser hiccup never rolls back live data.
    """
    rows = conn.execute(
        "SELECT relevate_doc_id, bronze_path, content_sha256, file_name "
        "FROM documents WHERE doc_kind = 'quarterly_report'"
    ).fetchall()
    if not rows:
        logger.info(
            "historical: no doc_kind='quarterly_report' rows in silver — "
            "nothing to do",
        )
        return
    logger.info(
        "historical: parsing %d Quartalsbericht PDF(s)", len(rows),
    )
    parsed_ok = 0
    n_positions = 0
    n_cash = 0
    conn.execute("BEGIN")
    try:
        for doc_id, bronze_path, content_sha256, file_name in rows:
            pdf_abs = bronze_root / bronze_path
            if not pdf_abs.is_file():
                logger.warning(
                    "historical: PDF missing on disk, skipping doc_id=%s",
                    doc_id,
                )
                continue
            try:
                parsed = parse_quarterly_report(pdf_abs)
            except Exception as exc:  # noqa: BLE001 — best-effort batch
                logger.warning(
                    "historical: parse failed for doc_id=%s: %s",
                    doc_id, exc,
                )
                continue

            snapshot_at = parsed["as_of_date"]
            account_id = parsed["account_external_id"]

            for pos in parsed["positions"]:
                conn.execute(
                    """
                    INSERT OR REPLACE INTO historical_position_snapshots (
                        snapshot_at, account_external_id, isin,
                        security_name, asset_class,
                        currency, units, allocation_pct, market_value,
                        document_id, source_sha256, payload
                    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
                    """,
                    (
                        snapshot_at, account_id, pos["isin"],
                        pos["security_name"], pos.get("asset_class"),
                        pos["currency"],
                        pos.get("units"),
                        pos.get("allocation_pct"),
                        pos["market_value"],
                        doc_id, content_sha256,
                        canonical_json(pos),
                    ),
                )
                n_positions += 1

            for c in parsed["cash"]:
                conn.execute(
                    """
                    INSERT OR REPLACE INTO historical_cash_balances (
                        snapshot_at, account_external_id, currency,
                        balance_kind, amount,
                        document_id, source_sha256, payload
                    ) VALUES (?,?,?,?,?,?,?,?)
                    """,
                    (
                        snapshot_at, account_id, c["currency"],
                        c["balance_kind"], c["amount"],
                        doc_id, content_sha256,
                        canonical_json(c),
                    ),
                )
                n_cash += 1

            parsed_ok += 1
    except sqlite3.DatabaseError:
        conn.execute("ROLLBACK")
        raise
    conn.execute("COMMIT")
    logger.info(
        "historical: parsed %d/%d PDF(s); %d position rows, %d cash rows",
        parsed_ok, len(rows), n_positions, n_cash,
    )


def load_credit_note_transactions(
    conn: sqlite3.Connection, bronze_root: Path,
) -> None:
    """
    Parse every Gutschriftsanzeige PDF currently indexed in `documents`,
    INSERT OR REPLACE into `transactions` with source='credit_note_pdf'.

    Idempotent: `transaction_external_id` is derived deterministically
    from the document id so re-running collapses to the same row.
    `snapshot_at` reflects the *latest* dump_run (the moment the loader
    last saw the credit note), matching the historical-positions
    convention.

    Runs in its own transaction so a parser hiccup never rolls back
    the live deposits-endpoint transactions written upstream.
    """
    rows = conn.execute(
        "SELECT relevate_doc_id, bronze_path, content_sha256 "
        "FROM documents WHERE doc_kind = 'credit_note'"
    ).fetchall()
    if not rows:
        logger.info(
            "credit_note: no doc_kind='credit_note' rows in silver — "
            "nothing to do",
        )
        return

    # Latest dump_run snapshot_at is the "when did gold first see this"
    # anchor we attach to every credit-note tx — mirrors what the
    # historical-positions reader uses for its analogous tables.
    latest = conn.execute(
        "SELECT COALESCE(MAX(snapshot_at), 0) FROM dump_runs"
    ).fetchone()[0]
    if not latest:
        logger.warning(
            "credit_note: no dump_runs yet — skipping (need a snapshot_at "
            "anchor for transactions.snapshot_at)",
        )
        return

    logger.info(
        "credit_note: parsing %d Gutschriftsanzeige PDF(s)", len(rows),
    )
    parsed_ok = 0
    conn.execute("BEGIN")
    try:
        for doc_id, bronze_path, content_sha256 in rows:
            pdf_abs = bronze_root / bronze_path
            if not pdf_abs.is_file():
                logger.warning(
                    "credit_note: PDF missing on disk, skipping doc_id=%s",
                    doc_id,
                )
                continue
            try:
                parsed = parse_credit_note(pdf_abs)
            except Exception as exc:  # noqa: BLE001 — best-effort batch
                logger.warning(
                    "credit_note: parse failed for doc_id=%s: %s",
                    doc_id, exc,
                )
                continue

            tx_id = f"credit_note:{doc_id}"
            payload = {
                "occurred_at":   parsed["occurred_at"],
                "currency":      parsed["currency"],
                "amount":        parsed["amount"],
                "kind":          parsed["kind"],
                "source_sha256": parsed["source_sha256"],
                "document_id":   doc_id,
            }
            conn.execute(
                """
                INSERT OR REPLACE INTO transactions (
                    transaction_external_id, snapshot_at, occurred_at,
                    account_external_id, instrument_external_id,
                    kind, currency, gross_amount, net_amount,
                    quantity, price, source, payload
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    tx_id, latest, parsed["occurred_at"],
                    parsed["account_external_id"], None,
                    parsed["kind"], parsed["currency"],
                    parsed["amount"], parsed["amount"],
                    None, None,
                    "credit_note_pdf",
                    canonical_json(payload),
                ),
            )
            parsed_ok += 1
    except sqlite3.DatabaseError:
        conn.execute("ROLLBACK")
        raise
    conn.execute("COMMIT")
    logger.info(
        "credit_note: parsed %d/%d PDF(s)", parsed_ok, len(rows),
    )


def do_load(args: argparse.Namespace) -> int:
    conn = open_db(args.silver_db)
    try:
        version = silver.apply_migrations(conn, MIGRATIONS_DIR)
        logger.info("silver schema at version %d", version)
        pending = list_pending_dumps(conn, args.bronze_dir)
        if pending:
            logger.info("loading %d pending dump(s)", len(pending))
            for d in pending:
                load_one_dump(conn, d, version)
        else:
            logger.info("no pending dumps under %s", args.bronze_dir)

        # Historical PDF parsing runs after all per-dump phases so the
        # `documents` table reflects every PDF the loader knows about,
        # including ones from earlier dumps that survived as content-
        # deduped rows. Idempotent — safe even when no new dumps landed.
        load_historical_snapshots(conn, args.bronze_dir)
        load_credit_note_transactions(conn, args.bronze_dir)

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
    cli.add_force_arg(p)
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    if args.force:
        silver.reset(args.silver_db)
    return do_load(args)


if __name__ == "__main__":
    raise SystemExit(main())
