#!/usr/bin/env python3
"""
Relevate bronze -> silver loader.

Walks a bronze directory tree (as produced by download.py), applies
any pending schema migrations, then loads each not-yet-ingested
dump into a SQLite silver database through the shared load frame
(collectorkit.loader): one dump = one transaction, the `dump_runs`
row last, so a failure mid-load rolls the whole dump back and a re-run
retries it. After the dumps, the two PDF passes read the reports and
credit notes the `documents` index holds.

Usage:
    load.py [--silver-db PATH] [--bronze-dir PATH] [-v]

By default, walks /data (= $XDG_DATA_HOME/wealthdb/relevate on the host) and
loads into /data/relevate.db.
"""

from __future__ import annotations

import argparse
import json
import logging
import sqlite3
from pathlib import Path
from typing import Any

from collectorkit import (
    bronze, derivations, documents, loader, parse, parsecache, silver, srcfp,
)
from collectorkit.silver import canonical_json

import pdf_parsers
from pdf_parsers import parse_credit_note, parse_quarterly_report

logger = logging.getLogger("load")

# Path to migrations dir relative to this script.
MIGRATIONS_DIR = Path(__file__).parent / "migrations"

# Both PDF passes key their rows on what the parser read off the page — the
# historical tables have their whole primary keys parsed out of the report,
# so an edit to any capture re-keys the row and `INSERT OR REPLACE` has
# nothing left to replace. `document_derivations` (migration 0004) records,
# per document, which generation derived its rows; a load derives only the
# documents this generation has not. When the generation has moved, both
# passes' rows are dropped and every document is derived again. One scope
# for both passes: they share `pdf_parsers`, so one fingerprint moves both.
PDF_GENERATION_SCOPE = "pdf_reports"
PDF_GENERATION = srcfp.parser_fingerprint([pdf_parsers], ("pypdf",))

# Parse results are cached per document and parser generation
# (collectorkit.parsecache), so a load reads a PDF only the first time this
# generation sees it, a `load --force` included. One namespace per parser.
QUARTERLY_NAMESPACE = f"quarterly-{PDF_GENERATION}"
CREDIT_NOTE_NAMESPACE = f"credit-note-{PDF_GENERATION}"

# The documents each pass derives, one row per document: its content
# sha256 (the derivation key), its Relevate id and its path under the
# bronze root.
QUARTERLY_DOCUMENTS = (
    "SELECT content_sha256, relevate_doc_id, bronze_path "
    "FROM documents WHERE doc_kind = 'quarterly_report'")
CREDIT_NOTE_DOCUMENTS = (
    "SELECT content_sha256, relevate_doc_id, bronze_path "
    "FROM documents WHERE doc_kind = 'credit_note'")

# The tables the two PDF passes own outright, and the predicate that names
# their rows. The `transactions` table admits other sources (its `source`
# CHECK), so the credit-note rows are named by `source`; the two historical
# tables hold nothing else.
_PDF_PASS_TABLES = (
    ("historical_position_snapshots", ""),
    ("historical_cash_balances", ""),
    ("transactions", " WHERE source = 'credit_note_pdf'"),
)

# Default mount points inside the container.
DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_SILVER_DB = Path("/data/relevate.db")


# ============================================================
# Helpers
# ============================================================


# Relevate's API serves fileNames in German; the older English
# needles are kept as defence in case the API ever serves an
# en-locale flag the collector doesn't pass.
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
    # download.py names the portfolio dir with the same token.
    slug = bronze.short_token(external_id)
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
    the index.

    The provider's document id is the IDENTITY and content_sha256 the
    version: new content under an id silver already holds is a
    RESTATEMENT — Relevate re-issued the document — and the row it
    supersedes gives way to it. Without that, the insert would collide
    with UNIQUE(relevate_doc_id) and roll back the whole dump, its
    `dump_runs` row included, so the dump would stay pending and block
    every later dump and both PDF passes.
    One row per document id is also what the ids derived from it need:
    `credit_note:<doc id>` transaction ids and `historical_*.document_id`
    would both be ambiguous with two. A missing PDF can be an
    .unexpected.<ext> path; only PDFs that landed correctly are indexed."""
    index_path = run_dir / "documents" / "index.json"
    if not index_path.is_file():
        logger.info("  documents phase: no index — skipping")
        return
    index = json.loads(index_path.read_text(encoding="utf-8"))

    def columns(d: dict, doc_id, file_size: int) -> dict:
        file_name = d.get("fileName") or ""
        return {
            "relevate_doc_id": doc_id,
            "relevate_external_id": d.get("externalId"),
            "file_name": file_name,
            "file_size": file_size,
            "doc_kind": doc_kind_from_filename(file_name),
            "document_type_code": d.get("documentType"),
            "category_code": d.get("category"),
            "document_year": d.get("documentYear"),
            "create_date": d.get("createDate"),
            "valid_till": d.get("validTill"),
            "foundation_id": d.get("foundationId"),
            "contract_id": d.get("contractId"),
            "owner_id": d.get("ownerId"),
        }

    n = documents.index_documents(
        conn, snapshot_at, run_dir, index.get("documents") or [],
        id_of=lambda d: d.get("id"), columns_of=columns,
        supersede_on="relevate_doc_id")
    logger.info(
        "  documents phase: %d new (%d superseding a restated document), "
        "%d existing refreshed, %d missing-on-disk",
        n.inserted, n.restated, n.refreshed, n.missing,
    )


def load_run(conn: sqlite3.Connection, run: loader.Run) -> None:
    """Load one bronze run dir into silver, inside the frame's
    transaction for the run."""
    logger.info("loading dump %s (snapshot_at=%d)", run.dir.name,
                run.snapshot_at)
    overview = load_accounts_phase(conn, run.snapshot_at, run.dir)
    if overview is not None:
        for p in (overview.get("portfolios") or []):
            load_portfolio_artefacts(conn, run.snapshot_at, run.dir, p)
    load_documents_phase(conn, run.snapshot_at, run.dir)


def run_columns(run: loader.Run) -> dict:
    """The run's `dump_runs` row. `run_dir` is the bare run-ts dirname
    (relative to the bronze root), the same convention as
    documents.bronze_path: consumers join it with their own bronze root,
    so one silver row resolves both in-container and on the host."""
    manifest = run.manifest
    return {
        "snapshot_at": run.snapshot_at,
        "silver_schema_version": run.version,
        "run_dir": run.dir.name,
        "mode": manifest.get("mode"),
        "dry_run": 1 if manifest.get("dry_run") else 0,
        "state_minted_at": parse.iso_timestamp_to_epoch(
            manifest.get("state_minted_at")),
        "bronze_files_total": len(manifest.get("files") or []),
        "bronze_errors_total": len(manifest.get("errors") or []),
        "payload": canonical_json(manifest),
    }


# ============================================================
# Orchestration
# ============================================================

def _parsed(cache: parsecache.ParseCache, namespace: str, parse_pdf,
            content_sha256: str, pdf: Path) -> dict:
    """`parse_pdf(pdf, content_sha256)`, from `cache` when this generation
    parsed the document before. A parse that raises stores nothing."""
    result = cache.get(content_sha256, namespace)
    if result is None:
        result = parse_pdf(pdf, content_sha256)
        cache.put(content_sha256, namespace, result)
    return result


def load_historical_snapshots(
    conn: sqlite3.Connection, bronze_root: Path,
    cache: parsecache.ParseCache, pending: list,
) -> list[tuple[str, int]]:
    """
    Derive the Quartalsbericht PDFs of `pending` (rows of
    QUARTERLY_DOCUMENTS), parsed or from `cache`, INSERT OR REPLACE into
    `historical_position_snapshots` + `historical_cash_balances`, and
    return each derived document's sha256 and row count.

    Both target tables are keyed on (snapshot_at, account_external_id,
    isin or balance_kind) — the dimensions parsed from the PDF — not on
    document_id, so a document derived again lands on its own rows. One
    PDF parse error doesn't kill the loop; logged and skipped, and the
    next `load` attempt will retry. Runs inside the caller's transaction.
    """
    if not pending:
        return []
    logger.info(
        "historical: parsing %d Quartalsbericht PDF(s)", len(pending),
    )
    derived: list[tuple[str, int]] = []
    n_positions = 0
    n_cash = 0
    for content_sha256, doc_id, bronze_path in pending:
        pdf_abs = bronze_root / bronze_path
        if not pdf_abs.is_file():
            logger.warning(
                "historical: PDF missing on disk, skipping doc_id=%s",
                doc_id,
            )
            continue
        try:
            parsed = _parsed(cache, QUARTERLY_NAMESPACE,
                             parse_quarterly_report, content_sha256, pdf_abs)
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

        derived.append(
            (content_sha256, len(parsed["positions"]) + len(parsed["cash"])))
    logger.info(
        "historical: parsed %d/%d PDF(s); %d position rows, %d cash rows",
        len(derived), len(pending), n_positions, n_cash,
    )
    return derived


def load_credit_note_transactions(
    conn: sqlite3.Connection, bronze_root: Path,
    cache: parsecache.ParseCache, pending: list,
) -> list[tuple[str, int]]:
    """
    Derive the Gutschriftsanzeige PDFs of `pending` (rows of
    CREDIT_NOTE_DOCUMENTS), parsed or from `cache`, INSERT OR REPLACE into
    `transactions` with source='credit_note_pdf', and return each derived
    document's sha256 and row count.

    `transaction_external_id` is derived deterministically from the
    document id, so a document derived again collapses to the same row.
    `snapshot_at` is the newest dump_runs snapshot, rewritten on every
    load for every credit note this generation derived
    (`_restamp_credit_notes`). Runs inside the caller's transaction.
    """
    if not pending:
        return []
    latest = _latest_dump(conn)
    if not latest:
        logger.warning(
            "credit_note: no dump_runs yet — skipping (need a snapshot_at "
            "anchor for transactions.snapshot_at)",
        )
        return []

    logger.info(
        "credit_note: parsing %d Gutschriftsanzeige PDF(s)", len(pending),
    )
    derived: list[tuple[str, int]] = []
    for content_sha256, doc_id, bronze_path in pending:
        pdf_abs = bronze_root / bronze_path
        if not pdf_abs.is_file():
            logger.warning(
                "credit_note: PDF missing on disk, skipping doc_id=%s",
                doc_id,
            )
            continue
        try:
            parsed = _parsed(cache, CREDIT_NOTE_NAMESPACE,
                             parse_credit_note, content_sha256, pdf_abs)
        except Exception as exc:  # noqa: BLE001 — best-effort batch
            logger.warning(
                "credit_note: parse failed for doc_id=%s: %s",
                doc_id, exc,
            )
            continue

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
                _credit_note_id(doc_id), latest, parsed["occurred_at"],
                parsed["account_external_id"], None,
                parsed["kind"], parsed["currency"],
                parsed["amount"], parsed["amount"],
                None, None,
                "credit_note_pdf",
                canonical_json(payload),
            ),
        )
        derived.append((content_sha256, 1))
    logger.info(
        "credit_note: parsed %d/%d PDF(s)", len(derived), len(pending),
    )
    return derived


def _credit_note_id(doc_id) -> str:
    return f"credit_note:{doc_id}"


def _latest_dump(conn: sqlite3.Connection) -> int:
    """The newest dump_runs snapshot, or 0 before the first dump."""
    return conn.execute(
        "SELECT COALESCE(MAX(snapshot_at), 0) FROM dump_runs").fetchone()[0]


def _restamp_credit_notes(conn: sqlite3.Connection, bronze_root: Path,
                          derived_now: set[str]) -> None:
    """Give every credit note this generation derived the newest dump's
    snapshot_at.

    transactions.snapshot_at is NOT NULL and a PDF carries no dump time
    of its own, so a credit-note row takes the latest dump_run's, and
    moves with it at every load. That covers the documents derived in
    this load (`derived_now`, by sha256) and those derived before under
    this generation whose PDF is still on disk."""
    latest = _latest_dump(conn)
    if not latest:
        return
    rows = conn.execute(
        CREDIT_NOTE_DOCUMENTS + " AND content_sha256 IN ("
        "SELECT content_sha256 FROM document_derivations "
        "WHERE scope = ? AND generation = ?)",
        (PDF_GENERATION_SCOPE, PDF_GENERATION)).fetchall()
    ids = {doc_id for sha, doc_id, path in conn.execute(CREDIT_NOTE_DOCUMENTS)
           if sha in derived_now}
    ids |= {doc_id for _, doc_id, path in rows
            if (bronze_root / path).is_file()}
    for doc_id in sorted(ids, key=str):
        conn.execute(
            "UPDATE transactions SET snapshot_at = ? "
            "WHERE transaction_external_id = ? AND snapshot_at != ?",
            (latest, _credit_note_id(doc_id), latest))


def load_pdf_passes(conn: sqlite3.Connection, load: loader.Load) -> None:
    """The two PDF passes, after every dump of the load, in one
    transaction.

    They run after the per-dump phases so the `documents` table reflects
    every PDF the loader knows about, including ones from earlier dumps
    that survived as content-deduped rows. Each derives only the
    documents this parser generation has not derived
    (collectorkit.derivations), so a load with no new document parses
    nothing. A moved generation drops both passes' rows and derives every
    document again."""
    # A document the index no longer holds (a restated one) leaves its
    # record, so the same bytes indexed again are derived again.
    conn.execute(
        "DELETE FROM document_derivations WHERE scope = ? AND "
        "content_sha256 NOT IN (SELECT content_sha256 FROM documents)",
        (PDF_GENERATION_SCOPE,))
    moved = _generation_moved(conn)
    reports = derivations.pending_derivations(
        conn, PDF_GENERATION_SCOPE, PDF_GENERATION, QUARTERLY_DOCUMENTS)
    notes = derivations.pending_derivations(
        conn, PDF_GENERATION_SCOPE, PDF_GENERATION, CREDIT_NOTE_DOCUMENTS)
    cache = parsecache.ParseCache(parsecache.default_dir("relevate"))
    with silver.transaction(conn):
        # Counted only when the answer will be read: an ordinary load has
        # nothing to compare and should not pay for three COUNT(*).
        before = _pdf_pass_row_count(conn) if moved else 0
        _purge_stale_pdf_rows(conn, moved)
        derived = load_historical_snapshots(
            conn, load.bronze_dir, cache, reports)
        derived_notes = load_credit_note_transactions(
            conn, load.bronze_dir, cache, notes)
        _restamp_credit_notes(conn, load.bronze_dir,
                              {sha for sha, _ in derived_notes})
        after = _pdf_pass_row_count(conn) if moved else 0
        if moved and after < before:
            # The re-derivation came back short — a PDF that no longer
            # parses, a document gone from the archive. Not recording is
            # what makes the next load try again rather than commit the
            # shortfall.
            logger.warning(
                "re-derivation produced %d row(s) where silver held %d; not "
                "recording the parser generation, so the next load "
                "re-derives again", after, before)
        else:
            for sha, n in derived + derived_notes:
                derivations.record_derivation(
                    conn, sha, PDF_GENERATION_SCOPE, PDF_GENERATION, n)
            # The rows of the older generations were dropped above.
            conn.execute(
                "DELETE FROM document_derivations "
                "WHERE scope = ? AND generation != ?",
                (PDF_GENERATION_SCOPE, PDF_GENERATION))
    cache.sweep({QUARTERLY_NAMESPACE, CREDIT_NOTE_NAMESPACE})


def _generation_moved(conn: sqlite3.Connection) -> bool:
    """Whether a document was derived by another parser generation."""
    return conn.execute(
        "SELECT 1 FROM document_derivations WHERE scope = ? "
        "AND generation != ? LIMIT 1",
        (PDF_GENERATION_SCOPE, PDF_GENERATION)).fetchone() is not None


def _pdf_pass_row_count(conn: sqlite3.Connection) -> int:
    """How many rows the two PDF passes are holding — the before/after a
    re-derivation is judged on."""
    return sum(conn.execute(f"SELECT COUNT(*) FROM {table}{where}").fetchone()[0]
               for table, where in _PDF_PASS_TABLES)


def _purge_stale_pdf_rows(conn: sqlite3.Connection, stale: bool) -> int:
    """Drop what the PDF passes wrote so they re-derive rather than add to
    it. `stale` is the caller's verdict on the parser generation, taken as
    an argument rather than re-derived here so that one condition decides
    when this fires.

    Whole-pass rather than per-document: a parser edit moves the row's own
    key, so no key-derived scope can name the rows it left behind. A moved
    generation makes every indexed document pending, so the re-derivation
    is complete by construction.
    """
    if not stale:
        return 0
    dropped = sum(conn.execute(f"DELETE FROM {table}{where}").rowcount
                  for table, where in _PDF_PASS_TABLES)
    logger.info("the report parser has changed since these rows were "
                "written; dropped %d for re-derivation", dropped)
    return dropped


def _parser() -> argparse.ArgumentParser:
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
    return p


# A run dir without a run.json, or one whose walk never completed, is not
# pending (bronze.pending_run_dirs); a run.json that cannot be read ends
# the load.
SPEC = loader.LoaderSpec(
    name="relevate", schema=MIGRATIONS_DIR, load_run=load_run,
    parser=_parser, run_columns=run_columns, after_runs=load_pdf_passes,
    manifest=loader.MANIFEST_REQUIRED)


def main(argv: list[str] | None = None) -> int:
    return loader.main(SPEC, argv)


if __name__ == "__main__":
    raise SystemExit(main())
