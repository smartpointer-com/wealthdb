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

By default, walks /data (= $XDG_DATA_HOME/wealthdb/viac on the host) and
loads into /data/viac.db.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import sqlite3
import sys
from datetime import date, datetime, timezone
from pathlib import Path
from typing import Any

from collectorkit import bronze, cli, parse, silver, srcfp

import pdf_parsers

logger = logging.getLogger("load")

# A report row's identity is (snapshot_at, account, ISIN) — no free text,
# but every component is a regex capture off the page, so moving the as-of
# date, the portfolio anchor or the ISIN token re-lands a whole quarter
# under a different key while the old rows stay. What stops that is the
# per-document delete in `load_historical_reports_phase`: re-parsing a
# report replaces its own rows, whatever keys they land under.
#
# `parser_generations` (migration 0004) records which generation produced
# them, so silver can be asked in plain SQL. It is a record, not a gate —
# the pass walks one dump's index, so it cannot re-derive a report the dump
# does not carry, and `load --force` is what re-reads the whole archive.
REPORT_GENERATION_SCOPE = "reports"
REPORT_GENERATION = srcfp.parser_fingerprint([pdf_parsers], ("pypdfium2",))

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

# Silver DB plumbing (open_db + schema versioning + the migration runner)
# lives in collectorkit.silver. VIAC uses the manual-transaction model
# (isolation_level=None; explicit BEGIN/COMMIT per dump) that silver.open_db
# provides, so it shares that opener directly.
open_db = silver.open_db


# ============================================================
# Helpers
# ============================================================

def ts_from_run_dir(name: str) -> int:
    """Parse YYYYMMDDTHHMMSSZ into Unix seconds UTC."""
    return bronze.parse_run_ts(name)


def iso_date_to_epoch(s: str | None) -> int | None:
    """Parse an ISO date (with or without time) into Unix seconds
    UTC at the day's midnight. Returns None for falsy /
    unparseable input."""
    return parse.iso_date_to_epoch(s)


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

        # management_style: every VIAC product line we've observed
        # is robo-managed (see DESIGN.md §7). The column is set
        # explicitly here so the loader documents the contract; if
        # VIAC ever ships a non-robo product, this branches on
        # product_code or strategy.
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
                management_style,
                currency_code, payload
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
                "automated",
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
                # VIAC's JSON key `amount` carries units (NOT CHF —
                # that's `ratioInChf`); the silver columns are
                # named `quantity` / `market_value_chf` after the
                # 0002 rename to match canonical / gold semantics.
                conn.execute(
                    """
                    INSERT INTO positions (
                        snapshot_at, account_external_id, instrument_external_id,
                        asset_class, viac_asset_class, sub_asset_class,
                        currency_code, name,
                        quantity, ratio, market_value_chf,
                        acquisition_price, asset_price, rate_of_return,
                        payload
                    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                    """,
                    (
                        snapshot_at, number, isin,
                        canonical, viac_class, pos.get("subAssetClassType"),
                        pos.get("currencyCode"), pos.get("name"),
                        pos.get("amount"),         # → quantity
                        pos.get("ratio"),
                        pos.get("ratioInChf"),     # → market_value_chf
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
                        first_seen_at = MIN(instruments.first_seen_at, excluded.first_seen_at),
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


def _parse_window_bound(s: str | None) -> date | None:
    """Parse a YYYY-MM-DD ISO date from a run.json `windows` field.
    Returns None if the field is missing or unparseable — callers
    treat None as "no bound" (load every transaction)."""
    if not s:
        return None
    try:
        return date.fromisoformat(s)
    except ValueError:
        return None


def load_transactions_phase(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
    *,
    since: date | None = None,
    until: date | None = None,
) -> None:
    """Parse transactions/all.json — keyed by portfolio number,
    each value a list of {type, amountInChf, valueDate,
    balanceAfterBooking, documentNumber}.

    ``since`` / ``until`` come from the bronze run's `windows` block
    (populated by download.py). Items whose ``valueDate`` falls
    outside [since, until] are dropped client-side — the REST
    endpoint has no date filter so we always receive the full
    history in bronze. Either bound can be None ("no bound", load
    every transaction)."""
    tx_path = run_dir / "transactions" / "all.json"
    if not tx_path.is_file():
        logger.info("  transactions phase: no transactions/all.json — skipping")
        return
    tx_root = json.loads(tx_path.read_text(encoding="utf-8"))
    per_portfolio = tx_root.get("transactions") or {}
    n_total = 0
    n_skipped = 0
    n_outside = 0
    for number, items in per_portfolio.items():
        for t in (items or []):
            tx_type = t.get("type")
            vd_str = t.get("valueDate")
            value_date = iso_date_to_epoch(vd_str)
            if not tx_type or value_date is None:
                n_skipped += 1
                continue
            # Date-window filter (download-time windows from run.json).
            # Apply on the raw YYYY-MM-DD slice rather than re-deriving
            # from the epoch we just computed — keeps the window check
            # in calendar-date terms and avoids a timezone round-trip.
            if since is not None or until is not None:
                try:
                    vd = date.fromisoformat(vd_str[:10])
                except (TypeError, ValueError):
                    vd = None
                if vd is not None:
                    if since is not None and vd < since:
                        n_outside += 1
                        continue
                    if until is not None and vd > until:
                        n_outside += 1
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
    msg = "  transactions phase: %d loaded, %d skipped (missing fields)"
    args: tuple = (n_total, n_skipped)
    if n_outside:
        msg += ", %d outside window"
        args += (n_outside,)
    logger.info(msg, *args)


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


# Document subtypes whose PDFs carry a full historical holdings
# table. The periodic auto-generated report and the on-demand manual
# one share the same "Securities overview" layout.
REPORT_SUBTYPES = ("INVESTMENT_REPORTING", "MANUAL_INVESTMENT_REPORTING")


def _upsert_report_instrument(
    conn: sqlite3.Connection, pos: "pdf_parsers.ReportPosition", seen_at: int,
) -> None:
    """Upsert an instrument observed in a historical report. Prefers
    existing (live-load) metadata — only fills nulls, lowers
    first_seen_at, raises last_seen_at — and leaves etf_link_en /
    payload untouched, so a richer live row is never clobbered by an
    older report row. This is how the report-only ISINs (instruments
    held historically but sold before live scraping began, e.g. the
    pre-2024-fusion CS/iShares funds) enter the catalogue, while the
    still-held instruments keep their live metadata and merely gain an
    earlier first_seen_at."""
    conn.execute(
        """
        INSERT INTO instruments (
            instrument_external_id, isin, name, currency_code,
            asset_class, viac_asset_class, sub_asset_class, etf_link_en,
            first_seen_at, last_seen_at, payload
        ) VALUES (?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(instrument_external_id) DO UPDATE SET
            name             = COALESCE(instruments.name, excluded.name),
            currency_code    = COALESCE(instruments.currency_code, excluded.currency_code),
            asset_class      = COALESCE(NULLIF(instruments.asset_class, ''), excluded.asset_class),
            viac_asset_class = COALESCE(instruments.viac_asset_class, excluded.viac_asset_class),
            sub_asset_class  = COALESCE(instruments.sub_asset_class, excluded.sub_asset_class),
            first_seen_at    = MIN(instruments.first_seen_at, excluded.first_seen_at),
            last_seen_at     = MAX(instruments.last_seen_at, excluded.last_seen_at)
        """,
        (
            pos.isin, pos.isin, pos.name, pos.currency_code,
            pos.asset_class, pos.viac_section, pos.sub_asset_class, None,
            seen_at, seen_at,
            canonical_json({
                "isin": pos.isin, "name": pos.name,
                "source": "report", "asset_class": pos.asset_class,
            }),
        ),
    )


def load_historical_reports_phase(
    conn: sqlite3.Connection,
    run_dir: Path,
) -> None:
    """Parse INVESTMENT_REPORTING PDFs in this dump into historical
    position + cash snapshots.

    VIAC's REST API only exposes current holdings; these periodic
    "Reporting" statement PDFs are the only source of holdings
    history. Each one is a period-end statement covering every
    portfolio under the contract, so parsing it yields one positions
    snapshot — plus one cash balance per portfolio — as of the report
    date, going back to the contract's first year.

    Rows are tagged source='report:<docid>' and keyed on the report's
    as-of date (which never collides with the live scrape timestamps),
    so historical and live snapshots coexist in the same tables.
    INSERT OR REPLACE makes re-parsing the same stable report across
    successive dumps converge. snapshot_at is the report as-of date,
    NOT the dump time — this phase is deliberately independent of the
    per-dump snapshot_at the live phases use."""
    index_path = run_dir / "documents" / "index.json"
    if not index_path.is_file():
        return
    index = json.loads(index_path.read_text(encoding="utf-8"))
    if not isinstance(index, list):
        return
    n_reports = n_positions = n_cash = 0
    for d in index:
        if d.get("subType") not in REPORT_SUBTYPES:
            continue
        doc_id = d.get("documentNumber")
        if not doc_id:
            continue
        pdf_path = run_dir / "documents" / f"{doc_id}.pdf"
        if not pdf_path.is_file():
            # Not in this dump's documents/ (gated tier, or the
            # hard-link landed it in a different dump). Another dump
            # carries it; skip here.
            continue
        try:
            parsed = pdf_parsers.parse_investment_report(pdf_path)
        except Exception as e:  # noqa: BLE001 — one bad PDF mustn't fail the dump
            logger.warning("report %s parse failed: %s", doc_id, e)
            continue
        as_of = iso_date_to_epoch(parsed.as_of_date)
        if as_of is None:
            logger.warning("report %s: no parseable as-of date — skipping", doc_id)
            continue
        if not parsed.positions:
            logger.warning("report %s: no positions parsed — skipping", doc_id)
            continue
        source = f"report:{doc_id}"
        # Drop what this report wrote last time before writing it again.
        # Per DOCUMENT, because a document is the only unit this pass can
        # re-derive: it walks `run_dir`'s own index, so a scope-wide purge
        # here would delete every other dump's reports and refill none of
        # them. The `source` tag (migration 0003) names a document's rows
        # exactly; the live REST rows carry their own tag and are untouched.
        for table in ("positions", "cash_balances"):
            conn.execute(f"DELETE FROM {table} WHERE source = ?", (source,))
        for p in parsed.positions:
            conn.execute(
                """
                INSERT OR REPLACE INTO positions (
                    snapshot_at, account_external_id, instrument_external_id,
                    asset_class, viac_asset_class, sub_asset_class,
                    currency_code, name,
                    quantity, ratio, market_value_chf,
                    acquisition_price, asset_price, rate_of_return,
                    source, payload
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    as_of, p.account_external_id, p.isin,
                    p.asset_class, p.viac_section, p.sub_asset_class,
                    p.currency_code, p.name,
                    p.quantity, p.ratio, p.market_value_chf,
                    p.acquisition_price, p.asset_price, p.rate_of_return,
                    source,
                    canonical_json({
                        "doc_id": doc_id, "as_of": parsed.as_of_date,
                        "name": p.name, "fx": p.currency_code,
                        "quantity": p.quantity,
                        "market_value_chf": p.market_value_chf,
                        "initial_price": p.acquisition_price,
                        "price": p.asset_price,
                        "return_pct": p.rate_of_return,
                        "section": p.viac_section,
                        "sub_asset_class": p.sub_asset_class,
                    }),
                ),
            )
            n_positions += 1
            _upsert_report_instrument(conn, p, as_of)
        for cb in parsed.cash:
            conn.execute(
                """
                INSERT OR REPLACE INTO cash_balances (
                    snapshot_at, account_external_id, currency,
                    balance_kind, amount, source, payload
                ) VALUES (?,?,?,?,?,?,?)
                """,
                (
                    as_of, cb.account_external_id, cb.currency,
                    "cash", cb.amount, source,
                    canonical_json({
                        "doc_id": doc_id, "as_of": parsed.as_of_date,
                        "source_field": "3a_account_liquidity",
                    }),
                ),
            )
            n_cash += 1
        n_reports += 1
    if n_reports:
        logger.info(
            "  historical reports phase: %d report(s), %d position(s), %d cash row(s)",
            n_reports, n_positions, n_cash,
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
            # Indexed but not downloaded — either skipped via
            # `--no-transaction-documents` or a fetch failure.
            # The next dump that does download it will catch up.
            missing += 1
            continue
        sha, size = bronze.sha256_file(pdf_path)
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
    # Transactions window comes from the bronze run's windows block
    # (set by download.py's resolve_lookback). Older bronze dumps
    # predate the block — they get None/None and load everything,
    # which matches the old behaviour.
    windows = run_manifest.get("windows") or {}
    tx_since = _parse_window_bound(windows.get("since"))
    tx_until = _parse_window_bound(windows.get("until"))

    conn.execute("BEGIN")
    try:
        portfolios = load_accounts_phase(conn, snapshot_at, run_dir)
        load_positions_phase(conn, snapshot_at, run_dir, portfolios)
        load_transactions_phase(conn, snapshot_at, run_dir,
                                since=tx_since, until=tx_until)
        load_wealth_history_phase(conn, snapshot_at, run_dir)
        load_documents_phase(conn, snapshot_at, run_dir)
        # Historical holdings reconstructed from the Reporting PDFs.
        # snapshot_at-independent (keyed on each report's as-of date),
        # so it runs after the live phases and outside their per-dump
        # snapshot grain.
        load_historical_reports_phase(conn, run_dir)
        silver.stamp_generation(conn, REPORT_GENERATION_SCOPE,
                                REPORT_GENERATION)

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
                # "fetched" column = every doc that hit the network: a plain
                # fetch plus both fetch-verify outcomes (verified/changed also
                # download, then dedup or keep). Only "linked" avoids the fetch.
                # Old dumps carry no verified/changed, so this is unchanged for
                # them. The full per-outcome breakdown lives in `payload`.
                int(docs_counts.get("fetched", 0))
                + int(docs_counts.get("verified", 0))
                + int(docs_counts.get("changed", 0)),
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
    for d in bronze.iter_run_dirs(bronze_dir):
        snapshot_at = ts_from_run_dir(d.name)
        if snapshot_at in loaded:
            continue
        run_json_path = d / "run.json"
        if not run_json_path.is_file():
            logger.info("skipping %s — no run.json (still writing?)", d.name)
            continue
        # download.py stamps run.json with a status ("in-progress" at
        # run-dir creation, "complete"/"dry-run" at the end), so its
        # presence alone does not prove the walk finished. Skip a
        # crashed/aborted walk ("in-progress") or a --dry-run shell
        # ("dry-run"); a statusless manifest predates the lifecycle and
        # stays loadable.
        status = bronze.run_status(run_json_path)
        if status in ("in-progress", "dry-run"):
            logger.info("skipping %s — run.json status=%s", d.name, status)
            continue
        pending.append(d)
    return pending


def do_load(args: argparse.Namespace) -> int:
    conn = open_db(args.silver_db)
    try:
        version = silver.apply_migrations(conn, MIGRATIONS_DIR)
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
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    if args.force:
        silver.reset(args.silver_db)
    return do_load(args)


if __name__ == "__main__":
    raise SystemExit(main())
