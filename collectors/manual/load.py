#!/usr/bin/env python3
"""manual — load hand-maintained private-holding CSVs into a SQLite silver.

The "manual" collector is the odd one out in wealthdb: there is **no source
to fetch from**. Bronze is a few hand-maintained CSV files in $XDG_DATA_HOME/wealthdb/manual/ for private holdings that have no
bank or portal behind them — real estate, direct private-company equity,
convertible notes, fund LP interests, single-deal SPVs, and other illiquid
positions (e.g. a receivable). The position `kind` is the
canonical asset class, so the set is open-ended (see POSITION_KINDS).

The collector tracks only what nothing else does: the illiquid POSITIONS and
their VALUATIONS. It deliberately does NOT record cash-flow transactions — the
wires that fund a purchase, pay a fee, or return a distribution are real
movements in the bank accounts, already captured by the bank collectors
(UBS / Schwab / …). The acquisition date lives on the position
(`acquired_at`), so a separate transactions ledger would only duplicate the
banks. See DESIGN.md §6.

There is therefore no `login` and no `download` step — only `load`:

  1. Read accounts.csv / positions.csv / valuations.csv / cost_basis.csv.
     accounts.csv is optional: without it every position lands in one
     account. With it, a book can span tax sleeves — a holding under one
     wrapper beside one held outright — without a second silver source per
     sleeve. cost_basis.csv is optional too: a paid-in series for a
     position whose cost is not the valuation at its acquired_at, such as
     a fund commitment paid in over time.
  2. Validate aggressively (bad rows fail loudly with file:row:column
     context — never silently dropped).
  3. Rebuild the SQLite silver from the current CSVs (full truncate-reload,
     inside one transaction, so a validation failure leaves silver
     untouched).

The CSVs are a small, hand-edited dataset (a few rows a year), so a full
rebuild on every load is the simplest correct model: same CSVs in => same
silver out. See DESIGN.md for the schema + the gold mapping.

PII: the real CSVs name real properties, companies, and amounts and
live OUTSIDE the repo under $XDG_DATA_HOME/wealthdb/manual/. Only synthetic placeholders
(see examples/) ever belong in tracked files.
"""
from __future__ import annotations

import argparse
import csv
import json
import logging
import os
import sqlite3
import sys
from datetime import date, datetime, timezone
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import NoReturn

from collectorkit import cli, silver

log = logging.getLogger("manual.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS_DIR = HERE / "migrations"

# Default data layout: $XDG_DATA_HOME/wealthdb/manual/{positions,valuations}.csv
# with the silver DB (manual.db) alongside them. The bronze dir comes from
# --bronze-dir (the wrapper resolves --data-dir / MANUAL_DATA_DIR /
# WEALTHDB_DATA_ROOT and passes it through), else the default; the silver DB
# is --silver-db > $MANUAL_SILVER_DB > <bronze-dir>/manual.db.
ENV_SILVER_DB = "MANUAL_SILVER_DB"
DEFAULT_BRONZE_DIR = cli.default_data_root() / "manual"
SILVER_DB_NAME = "manual.db"


def _env_path(name: str) -> Path | None:
    value = os.environ.get(name)
    return Path(value).expanduser() if value else None


def resolve_paths(args: argparse.Namespace) -> tuple[Path, Path]:
    """Resolve (bronze_dir, silver_db). The bronze dir is --bronze-dir (the
    wrapper resolves the data dir + MANUAL_DATA_DIR / WEALTHDB_DATA_ROOT and
    passes it) else the default; the silver DB is flag > $MANUAL_SILVER_DB >
    manual.db inside the *resolved* bronze dir, so overriding only the bronze
    dir keeps the DB beside the CSVs."""
    bronze = args.bronze_dir or DEFAULT_BRONZE_DIR
    silver_db = (args.silver_db or _env_path(ENV_SILVER_DB)
                 or Path(bronze) / SILVER_DB_NAME)
    return Path(bronze), Path(silver_db)


# --- Accepted position kinds (the single source of truth; the silver schema
# keeps `kind` as plain TEXT so adding a kind needs no migration). Position
# `kind` is deliberately identical to the canonical gold `asset_class` (the
# gold classmap is then an identity), so the bronze CSV self-documents the
# asset class.
# `mortgage` is a real-property-backed LIABILITY: its valuation is entered as a
# positive outstanding balance (direction comes from kind), which the gold
# adapter negates so it nets against the property it secures.
POSITION_KINDS = {"real_estate", "private_equity", "convertible_note",
                  "private_fund", "spv", "mortgage", "other"}

# --- Accepted vehicles (the wrapper dimension of the 2-D taxonomy; see
# wealthdb docs/TAXONOMY.md). `kind` carries the asset class; `vehicle`
# records how the exposure is held. The full canonical vehicle vocabulary is
# accepted because these rows are authored by hand, but only the ones a
# manual (illiquid, hand-tracked) holding realistically uses are expected.
POSITION_VEHICLES = {
    "stock", "etf", "fund", "spv", "bond", "convertible_note", "loan",
    "option", "future", "forward", "time_deposit", "demand_deposit",
    "physical", "structured_product", "right", "mortgage", "escrow", "other",
}

# The `vehicle` column is optional: when the CSV omits it, default it from
# `kind`. `other` defaults to `other`; an escrow receivable or private loan
# filed under kind=other should set vehicle explicitly (escrow / loan) to
# carry the right wrapper into gold.
DEFAULT_VEHICLE_BY_KIND = {
    "real_estate": "physical",
    "private_equity": "stock",
    "convertible_note": "convertible_note",
    "private_fund": "fund",
    "spv": "spv",
    "mortgage": "mortgage",
    "other": "other",
}

# --- The canonical gold vocabularies an account row is checked against
# (wealthdb internal/canonical/enums.go). Kept here as literal sets because
# the collector cannot import Go; a value gold would reject must fail at load,
# where the CSV row number is still in hand, rather than land unnoticed.
ACCOUNT_KINDS = {
    "brokerage", "cash", "safekeeping", "custody", "overlay", "crypto",
    "crypto_exchange", "crypto_self_custody", "mortgage", "card",
    "donor_advised_fund", "other",
}
TAX_WRAPPERS = {
    "taxable_personal", "taxable_joint", "foundation", "traditional_ira",
    "roth_ira", "sep_ira", "simple_ira", "401k", "403b", "457b", "529",
    "coverdell_esa", "hsa", "charitable",
    "custodial_utma", "custodial_ugma", "trust_grantor", "trust_non_grantor",
    "trust_charitable", "pillar_2", "vested_benefits", "pillar_3a", "other",
}
MANAGEMENT_STYLES = {"self_directed", "advisory", "discretionary", "automated"}

# The account a position falls into when accounts.csv is absent or the
# position names none — the single account every manual position was in
# before accounts existed, with the taxonomy it always had. Keeping the id
# stable is what makes this change invisible to a deployment that does not
# want accounts: same gold account_external_id, same wrapper, same style.
DEFAULT_ACCOUNT_ID = "manual"
DEFAULT_ACCOUNT_NAME = "Manual"
DEFAULT_ACCOUNT_KIND = "other"
DEFAULT_TAX_WRAPPER = "taxable_personal"
DEFAULT_MANAGEMENT_STYLE = "self_directed"

# --- CSV column contracts. Required columns must be present in the header;
# optional columns default to empty when a file omits them; any unexpected
# column is rejected as a likely typo.
ACCOUNTS_REQUIRED = ["id", "display_name", "account_kind"]
ACCOUNTS_OPTIONAL = ["tax_wrapper", "management_style", "notes", "payload"]
POSITIONS_REQUIRED = ["id", "kind", "display_name", "currency", "acquired_at"]
POSITIONS_OPTIONAL = ["closed_at", "notes", "payload", "vehicle", "account_id"]
VALUATIONS_REQUIRED = ["position_id", "as_of_date", "value", "currency"]
VALUATIONS_OPTIONAL = ["notes", "payload"]
COST_BASIS_REQUIRED = ["position_id", "as_of_date", "amount", "currency"]
COST_BASIS_OPTIONAL = ["notes"]


class LoadError(SystemExit):
    """A validation failure with file:row:column context. Subclasses
    SystemExit so an uncaught one exits non-zero with a clear message."""


def _fail(fname: str, rownum: int | str, col: str, msg: str,
          value=None) -> NoReturn:
    suffix = "" if value is None else f" (got {value!r})"
    raise LoadError(f"{fname}:row {rownum}:{col}: {msg}{suffix}")


# ----------------------------------------------------------------------
# Field parsers — each raises LoadError with context on bad input.
# ----------------------------------------------------------------------
def _req(fname, rownum, col, value):
    if value is None or value.strip() == "":
        _fail(fname, rownum, col, "required value is empty")
    return value.strip()


def _date(fname, rownum, col, value, *, required=True) -> date | None:
    value = (value or "").strip()
    if value == "":
        if required:
            _fail(fname, rownum, col, "required date is empty")
        return None
    try:
        return date.fromisoformat(value)
    except ValueError:
        _fail(fname, rownum, col, "not an ISO date (YYYY-MM-DD)", value)


def _currency(fname, rownum, col, value):
    value = _req(fname, rownum, col, value).upper()
    if not (len(value) == 3 and value.isalpha()):
        _fail(fname, rownum, col, "not a 3-letter ISO 4217 code", value)
    return value


def _decimal(fname, rownum, col, value, *, non_negative=True) -> Decimal:
    raw = _req(fname, rownum, col, value)
    try:
        d = Decimal(raw)
    except (InvalidOperation, ValueError):
        _fail(fname, rownum, col, "not a number", value)
    if non_negative and d < 0:
        _fail(fname, rownum, col,
              "must be a positive magnitude (direction comes from kind)",
              value)
    return d


def _json_obj(fname, rownum, col, value) -> dict:
    raw = (value or "").strip()
    if raw == "":
        return {}
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError as exc:
        _fail(fname, rownum, col, f"not valid JSON ({exc.msg})", value)
    if not isinstance(obj, dict):
        _fail(fname, rownum, col, "payload must be a JSON object", value)
    return obj


# ----------------------------------------------------------------------
# CSV reading + header validation.
# ----------------------------------------------------------------------
def _read_csv(path: Path, required: list[str], optional: list[str],
              ) -> list[dict]:
    """Read a CSV into a list of row dicts (str values), validating the
    header. Missing file -> empty list (caller decides if that's fatal).
    Missing required column or unknown column -> LoadError."""
    fname = path.name
    if not path.is_file():
        return []
    with path.open(newline="", encoding="utf-8-sig") as fh:
        reader = csv.DictReader(fh)
        seen = [h for h in (reader.fieldnames or []) if h is not None]
        allowed = set(required) | set(optional)
        missing = [c for c in required if c not in seen]
        if missing:
            _fail(fname, "header", ",".join(missing),
                  f"missing required column(s); header was {seen}")
        unknown = [c for c in seen if c not in allowed]
        if unknown:
            _fail(fname, "header", ",".join(unknown),
                  f"unexpected column(s); allowed are {sorted(allowed)}")
        # DictReader rows: enumerate from 2 (row 1 is the header) so the
        # number a user sees in their spreadsheet matches the error.
        return [{"_row": i, **{k: (row.get(k) or "") for k in allowed}}
                for i, row in enumerate(reader, start=2)]


# ----------------------------------------------------------------------
# Per-file validation -> normalized records ready for insert.
# ----------------------------------------------------------------------
def _enum(fname, rownum, col, value, allowed, *, default=None):
    """One canonical enum value, or `default` when the cell is empty."""
    raw = (value or "").strip()
    if raw == "":
        return default
    if raw not in allowed:
        _fail(fname, rownum, col,
              f"not a canonical {col}; expected one of {sorted(allowed)}", raw)
    return raw


def default_account() -> dict:
    """The account a book with no accounts.csv is held in."""
    return {
        "id": DEFAULT_ACCOUNT_ID,
        "display_name": DEFAULT_ACCOUNT_NAME,
        "account_kind": DEFAULT_ACCOUNT_KIND,
        "tax_wrapper": DEFAULT_TAX_WRAPPER,
        "management_style": DEFAULT_MANAGEMENT_STYLE,
        "notes": None,
        "payload": {},
    }


def validate_accounts(rows: list[dict]) -> dict[str, dict]:
    """The declared pseudo-accounts, keyed by id. An empty accounts.csv (or
    none at all) yields the single default account, which is what every
    manual book had before accounts existed."""
    fname = "accounts.csv"
    out: dict[str, dict] = {}
    for r in rows:
        n = r["_row"]
        aid = _req(fname, n, "id", r["id"])
        if aid in out:
            _fail(fname, n, "id", "duplicate account id", aid)
        out[aid] = {
            "id": aid,
            "display_name": _req(fname, n, "display_name", r["display_name"]),
            "account_kind": _enum(fname, n, "account_kind", r["account_kind"],
                                  ACCOUNT_KINDS),
            "tax_wrapper": _enum(fname, n, "tax_wrapper", r["tax_wrapper"],
                                 TAX_WRAPPERS, default=DEFAULT_TAX_WRAPPER),
            "management_style": _enum(fname, n, "management_style",
                                      r["management_style"],
                                      MANAGEMENT_STYLES,
                                      default=DEFAULT_MANAGEMENT_STYLE),
            "notes": r["notes"].strip() or None,
            "payload": _json_obj(fname, n, "payload", r["payload"]),
        }
    if not out:
        out[DEFAULT_ACCOUNT_ID] = default_account()
    return out


def validate_positions(rows: list[dict], accounts: dict[str, dict],
                       ) -> dict[str, dict]:
    fname = "positions.csv"
    out: dict[str, dict] = {}
    row_of: dict[str, int] = {}
    for r in rows:
        n = r["_row"]
        pid = _req(fname, n, "id", r["id"])
        if pid in out:
            _fail(fname, n, "id", "duplicate position id", pid)
        kind = _req(fname, n, "kind", r["kind"])
        if kind not in POSITION_KINDS:
            _fail(fname, n, "kind",
                  f"unknown kind; expected one of {sorted(POSITION_KINDS)}",
                  kind)
        vehicle = (r.get("vehicle") or "").strip() or DEFAULT_VEHICLE_BY_KIND[kind]
        if vehicle not in POSITION_VEHICLES:
            _fail(fname, n, "vehicle",
                  f"unknown vehicle; expected one of {sorted(POSITION_VEHICLES)}",
                  vehicle)
        acquired = _date(fname, n, "acquired_at", r["acquired_at"])
        closed = _date(fname, n, "closed_at", r["closed_at"], required=False)
        if closed is not None and closed < acquired:
            _fail(fname, n, "closed_at",
                  f"closed_at {closed} precedes acquired_at {acquired}")
        account_id = (r.get("account_id") or "").strip() or DEFAULT_ACCOUNT_ID
        if account_id not in accounts:
            _fail(fname, n, "account_id",
                  "references an account not in accounts.csv", account_id)
        out[pid] = {
            "id": pid,
            "account_id": account_id,
            "kind": kind,
            "vehicle": vehicle,
            "display_name": _req(fname, n, "display_name", r["display_name"]),
            "currency": _currency(fname, n, "currency", r["currency"]),
            "acquired_at": acquired,
            "closed_at": closed,
            "notes": r["notes"].strip() or None,
            "payload": _json_obj(fname, n, "payload", r["payload"]),
        }
        row_of[pid] = n
    # Conversion-link integrity (post-pass, so forward references resolve): a
    # position opened by a converting note back-references its source via
    # payload.converted_from_position_id; that source must be a known position.
    # With no transactions ledger, this back-reference + the note's closed_at
    # are the whole record of a conversion.
    for pid, p in out.items():
        src = p["payload"].get("converted_from_position_id")
        if src and src not in out:
            _fail(fname, row_of[pid], "payload",
                  "converted_from_position_id references a position not in "
                  "positions.csv", src)
    return out


def validate_valuations(rows: list[dict], positions: dict[str, dict],
                        ) -> list[dict]:
    fname = "valuations.csv"
    out: list[dict] = []
    seen: set[tuple[str, date]] = set()
    for r in rows:
        n = r["_row"]
        pid = _req(fname, n, "position_id", r["position_id"])
        pos = positions.get(pid)
        if pos is None:
            _fail(fname, n, "position_id",
                  "references a position not in positions.csv", pid)
        as_of = _date(fname, n, "as_of_date", r["as_of_date"])
        if (pid, as_of) in seen:
            _fail(fname, n, "as_of_date",
                  f"duplicate valuation for position {pid} on {as_of}")
        seen.add((pid, as_of))
        ccy = _currency(fname, n, "currency", r["currency"])
        if ccy != pos["currency"]:
            _fail(fname, n, "currency",
                  f"currency {ccy} != position {pid} currency "
                  f"{pos['currency']}")
        if as_of < pos["acquired_at"]:
            log.warning("%s:row %d: valuation date %s precedes %s "
                        "acquisition %s", fname, n, as_of, pid,
                        pos["acquired_at"])
        out.append({
            "position_id": pid,
            "as_of_date": as_of,
            "value": _decimal(fname, n, "value", r["value"]),
            "currency": ccy,
            "notes": r["notes"].strip() or None,
            "payload": _json_obj(fname, n, "payload", r["payload"]),
        })
    return out


def validate_cost_basis(rows: list[dict], positions: dict[str, dict],
                        ) -> list[dict]:
    """The optional paid-in series: the capital paid into a position as of a
    date, gross of any paid back. Where a position has one, gold takes its
    book value from it instead of the valuation at acquired_at."""
    fname = "cost_basis.csv"
    out: list[dict] = []
    seen: set[tuple[str, date]] = set()
    for r in rows:
        n = r["_row"]
        pid = _req(fname, n, "position_id", r["position_id"])
        pos = positions.get(pid)
        if pos is None:
            _fail(fname, n, "position_id",
                  "references a position not in positions.csv", pid)
        as_of = _date(fname, n, "as_of_date", r["as_of_date"])
        if (pid, as_of) in seen:
            _fail(fname, n, "as_of_date",
                  f"duplicate cost basis for position {pid} on {as_of}")
        seen.add((pid, as_of))
        ccy = _currency(fname, n, "currency", r["currency"])
        if ccy != pos["currency"]:
            _fail(fname, n, "currency",
                  f"currency {ccy} != position {pid} currency "
                  f"{pos['currency']}")
        out.append({
            "position_id": pid,
            "as_of_date": as_of,
            "amount": _decimal(fname, n, "amount", r["amount"]),
            "currency": ccy,
            "notes": r["notes"].strip() or None,
        })
    return out


# ----------------------------------------------------------------------
# SQLite silver.
# ----------------------------------------------------------------------
def open_db(path: Path) -> sqlite3.Connection:
    """Open the silver DB (manual transaction control, FK + WAL)."""
    return silver.open_db(Path(path))


def apply_migrations(conn: sqlite3.Connection) -> int:
    """Apply pending migrations; returns the resulting schema version."""
    return silver.apply_migrations(conn, MIGRATIONS_DIR)


def load(conn: sqlite3.Connection, bronze_dir: Path) -> dict:
    """Validate the CSVs and rebuild silver from them in one transaction.
    Returns a counts dict. Raises LoadError (rolling back) on any bad row."""
    bronze_dir = Path(bronze_dir)
    accounts_rows = _read_csv(bronze_dir / "accounts.csv",
                              ACCOUNTS_REQUIRED, ACCOUNTS_OPTIONAL)
    positions_rows = _read_csv(bronze_dir / "positions.csv",
                               POSITIONS_REQUIRED, POSITIONS_OPTIONAL)
    valuations_rows = _read_csv(bronze_dir / "valuations.csv",
                                VALUATIONS_REQUIRED, VALUATIONS_OPTIONAL)
    cost_basis_rows = _read_csv(bronze_dir / "cost_basis.csv",
                                COST_BASIS_REQUIRED, COST_BASIS_OPTIONAL)

    if not positions_rows:
        log.warning("no positions.csv (or it is empty) in %s — nothing to "
                    "load", bronze_dir)
        return {"accounts": 0, "positions": 0, "valuations": 0,
                "cost_basis": 0}

    # Validate everything BEFORE touching silver, so a bad row never leaves
    # a half-rebuilt DB.
    accounts = validate_accounts(accounts_rows)
    positions = validate_positions(positions_rows, accounts)
    valuations = validate_valuations(valuations_rows, positions)
    cost_basis = validate_cost_basis(cost_basis_rows, positions)
    # An account nothing is held in projects no gold account, so it is a
    # typo or a leftover rather than an empty sleeve worth carrying.
    held = {p["account_id"] for p in positions.values()}
    for aid in accounts:
        if aid not in held and aid != DEFAULT_ACCOUNT_ID:
            log.warning("accounts.csv: account %r holds no position", aid)

    conn.execute("BEGIN")
    try:
        conn.execute("DELETE FROM cost_basis")
        conn.execute("DELETE FROM valuations")
        conn.execute("DELETE FROM positions")
        conn.execute("DELETE FROM accounts")
        conn.executemany(
            "INSERT INTO accounts (id, display_name, account_kind, "
            "tax_wrapper, management_style, notes, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?)",
            [(a["id"], a["display_name"], a["account_kind"], a["tax_wrapper"],
              a["management_style"], a["notes"], json.dumps(a["payload"]))
             for a in accounts.values()],
        )
        conn.executemany(
            "INSERT INTO positions (id, account_id, kind, vehicle, "
            "display_name, currency, acquired_at, closed_at, notes, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            [(p["id"], p["account_id"], p["kind"], p["vehicle"],
              p["display_name"], p["currency"],
              p["acquired_at"].isoformat(),
              p["closed_at"].isoformat() if p["closed_at"] else None,
              p["notes"], json.dumps(p["payload"]))
             for p in positions.values()],
        )
        conn.executemany(
            "INSERT INTO valuations (position_id, as_of_date, value, "
            "currency, notes, payload) VALUES (?, ?, ?, ?, ?, ?)",
            [(v["position_id"], v["as_of_date"].isoformat(), str(v["value"]),
              v["currency"], v["notes"], json.dumps(v["payload"]))
             for v in valuations],
        )
        conn.executemany(
            "INSERT INTO cost_basis (position_id, as_of_date, amount, "
            "currency, notes) VALUES (?, ?, ?, ?, ?)",
            [(c["position_id"], c["as_of_date"].isoformat(), str(c["amount"]),
              c["currency"], c["notes"])
             for c in cost_basis],
        )
        counts = {"accounts": len(accounts),
                  "positions": len(positions),
                  "valuations": len(valuations),
                  "cost_basis": len(cost_basis)}
        conn.execute(
            "INSERT INTO load_runs (load_at, silver_schema_version, "
            "bronze_dir, positions_total, valuations_total, payload) "
            "VALUES (?, ?, ?, ?, ?, ?)",
            (int(datetime.now(timezone.utc).timestamp()),
             silver.current_schema_version(conn), str(bronze_dir),
             counts["positions"], counts["valuations"], json.dumps(counts)),
        )
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    return counts


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Load hand-maintained private-holding CSVs into the "
                    "manual SQLite silver.")
    p.add_argument("--bronze-dir", type=Path, default=None,
                   help=f"Directory holding positions.csv / valuations.csv. "
                        f"Precedence: this flag (the wrapper passes it from "
                        f"--data-dir / MANUAL_DATA_DIR / WEALTHDB_DATA_ROOT) > "
                        f"{DEFAULT_BRONZE_DIR}.")
    p.add_argument("--silver-db", type=Path, default=None,
                   help=f"SQLite silver path. Precedence: this flag > "
                        f"${ENV_SILVER_DB} env var > "
                        f"<bronze-dir>/{SILVER_DB_NAME}.")
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    bronze_dir, silver_db = resolve_paths(args)
    if args.force:
        silver.reset(silver_db)
    conn = open_db(silver_db)
    try:
        version = apply_migrations(conn)
        log.info("silver schema at version %d (%s)", version, silver_db)
        counts = load(conn, bronze_dir)
        log.info("loaded %d account(s), %d position(s), %d valuation(s)",
                 counts["accounts"], counts["positions"],
                 counts["valuations"])
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
