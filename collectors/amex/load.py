#!/usr/bin/env python3
"""amex silver loader: bronze REST captures → source-shaped SQLite.

Parses each bronze run dir (see download.py for the layout) into the silver
schema (migrations/): the card roster, the transaction ledger, the per-period
statement balances, and the document inventory. Idempotent — an already-loaded
dump is skipped, and posted transactions INSERT OR IGNORE on their stable id,
so re-running converges.

The **activity JSON is the ledger of record** (DESIGN.md §D). It alone carries
the provider's stable 18-digit id, both the charge and post dates, the
merchant category, pending rows, and the cycle balance block. The CSV/QFX
exports are captured for provenance only and are never read here: they carry a
subset of the same rows under the same ids, so joining them would add nothing
and would make a row's identity depend on which files a run happened to land.

SIGNS: Amex's JSON states spend as a POSITIVE "amount charged"; the fleet's
silver card convention (set by chase, and matching Amex's own QFX export) is
the opposite. Every amount is negated on the way in — see `signed_amount` and
the sign note in migrations/0001_initial.sql.

Bronze run-dir layout consumed:

    <run>/
      run.json                      status manifest
      accounts.json                 [{account_key, account_token, …}, …]
      activity/<key>.json           {transactions:[…], categories:{},
                                     balancesDetails:{}, …}
      statements/<key>/<date>.pdf   statement + year-end-summary PDFs
      transactions/<key>.{csv,qfx}  (provenance only — not read here)
      raw/*.json                    (provenance only — not read here)
"""
from __future__ import annotations

import argparse
import json
import logging
import re
import shutil
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

import statement_parser

log = logging.getLogger("amex.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Re-export so tests can call load.apply_migrations directly.
apply_migrations = silver.apply_migrations

# transactions.source tags. The activity JSON is the single source of record
# for the era it reaches; SOURCE_STATEMENT is the deeper tail the
# statement PDFs carry below it.
SOURCE_ACTIVITY = "activity"
SOURCE_STATEMENT = "statement"


# A year-end summary is filed under this stem by download.py.
_YEAR_SUMMARY_PREFIX = "yes-"


# ============================================================
# Date / money parsing
# ============================================================

def epoch_day(d) -> int:
    """Unix seconds UTC at midnight of the given datetime or date."""
    return int(datetime(d.year, d.month, d.day,
                        tzinfo=timezone.utc).timestamp())


def parse_date(raw) -> int | None:
    """An activity row's date → epoch-midnight seconds.

    Amex renders dates as ISO `YYYY-MM-DD` throughout the JSON. `MM/DD/YYYY`
    and an epoch integer are also accepted so a wire-format variant degrades
    gracefully rather than dropping the row. None on anything unparseable."""
    if raw is None:
        return None
    if isinstance(raw, (int, float)) or (isinstance(raw, str)
                                         and raw.strip().isdigit()):
        n = int(raw)
        if n > 10_000_000_000:          # too large for seconds → milliseconds
            n //= 1000
        try:
            return epoch_day(datetime.fromtimestamp(n, tz=timezone.utc))
        except (OverflowError, OSError, ValueError):
            return None
    s = str(raw).strip()
    if not s:
        return None
    for fmt in ("%Y-%m-%d", "%m/%d/%Y", "%m/%d/%y"):
        try:
            return epoch_day(datetime.strptime(s[:10], fmt))
        except ValueError:
            continue
    return None


def parse_money(raw) -> float | None:
    """Parse a signed money value. Accepts a native number or a string with
    thousands separators, a leading currency symbol, and parenthesised
    negatives; returns None on blank/unparseable."""
    if raw is None:
        return None
    if isinstance(raw, (int, float)):
        return float(raw)
    s = str(raw).strip()
    if not s:
        return None
    neg = s.startswith("(") and s.endswith(")")
    s = s.strip("()").replace(",", "").replace("$", "").strip()
    if not s:
        return None
    try:
        val = float(s)
    except ValueError:
        return None
    return -val if neg else val


def _money2(value: float | None) -> float | None:
    """Money rounded to cents. Everything downstream (the gold adapter's
    float→decimal step) assumes silver holds display precision."""
    return None if value is None else round(value, 2)


# ============================================================
# Activity JSON → ledger rows
# ============================================================

def signed_amount(row: dict) -> float | None:
    """The row's amount in the FLEET card convention: spend NEGATIVE,
    anything reducing the balance owed POSITIVE.

    Amex states the opposite — a purchase is a positive "amount charged" — so
    the magnitude is taken from `transactionAmount.amount` and the direction
    from the explicit `type`, which is robust whether the raw amount carries a
    sign or not. With no usable `type`, the raw sign is negated, which is the
    same rule stated the other way. See migrations/0001_initial.sql."""
    amt = parse_money((row.get("transactionAmount") or {}).get("amount"))
    if amt is None:
        return None
    kind = str(row.get("type") or "").strip().upper()
    if kind == "DEBIT":
        return -abs(amt)
    if kind == "CREDIT":
        return abs(amt)
    return -amt


def activity_rows(payload: dict) -> list[dict]:
    """Project a bronze `activity/<key>.json` into silver ledger rows.

    The category code is resolved through the same payload's own code → label
    map (DESIGN.md §F), so a category the provider adds later lands as its
    label rather than as an unmapped code. A row missing a parseable post date
    or amount is skipped; everything else is preserved in `payload`."""
    categories = payload.get("categories")
    categories = categories if isinstance(categories, dict) else {}
    out = []
    for row in payload.get("transactions") or []:
        if not isinstance(row, dict):
            continue
        posted_at = parse_date(row.get("postDate") or row.get("displayDate"))
        amount = signed_amount(row)
        if posted_at is None or amount is None:
            continue
        txn_id = str(row.get("identifier")
                     or row.get("referenceNumber") or "").strip()
        if not txn_id:
            continue
        code = str(row.get("categoryCode") or "").strip()
        description = str(row.get("displayDescription") or "").strip() or None
        raw_amount = (row.get("transactionAmount") or {}).get("amount")
        out.append({
            "txn_id": txn_id,
            "posted_at": posted_at,
            "amount": _money2(amount),
            "kind": str(row.get("type") or "").strip().upper() or None,
            "description": description,
            # The merchant is the same string as the description on this
            # source: Amex publishes one merchant line, not a split. It is
            # promoted anyway because it is what gold's merchant signature
            # keys on, and a later source shape may separate the two.
            "merchant": description,
            "category": categories.get(code) or None,
            "category_code": code or None,
            "txn_date": parse_date(row.get("chargeDate")),
            "statement_end_at": parse_date(row.get("statementEndDate")),
            "currency": (row.get("transactionAmount") or {}).get("currency")
                        or None,
            "is_pending": 1 if str(row.get("status", "")).lower() == "pending"
                          else 0,
            "source": SOURCE_ACTIVITY,
            # provider_amount keeps the figure as the provider signed it, so
            # the negation above is auditable from silver alone.
            "payload": {**row, "provider_amount": raw_amount},
        })
    return out


def statement_balance_row(payload: dict) -> dict | None:
    """The cycle balance block from an activity payload, as a
    `statement_balances` row — or None when the payload carries no period to
    anchor it to.

    Which figures the block states is view-dependent (DESIGN.md §H); the
    windowed view this collector fetches states a total balance rather than a
    statement one, so `closing` is filled from whichever is present."""
    bd = payload.get("balancesDetails")
    if not isinstance(bd, dict):
        return None
    summary = (bd.get("summary") or {}).get("standard")
    if not isinstance(summary, dict):
        return None

    def amt(key):
        block = summary.get(key)
        if not isinstance(block, dict):
            return None
        return _money2(parse_money(block.get("amount")))

    period_end = parse_date(payload.get("until"))
    period_start = parse_date(payload.get("since"))
    if period_end is None:
        return None
    closing = amt("statementBalance")
    if closing is None:
        closing = amt("totalBalance")
    return {
        "period_start": period_start if period_start is not None
                        else period_end,
        "period_end": period_end,
        "opening": amt("previousBalance"),
        "closing": closing,
        "new_charges": amt("newCharges"),
        "payments_and_credits": amt("paymentsAndCredits"),
        "fees": amt("fees"),
        "interest": amt("interestCharges"),
    }


# ============================================================
# Statement PDFs → the deep-era ledger
# ============================================================

def statement_period_starts(period_ends: list[int]) -> dict[int, int]:
    """Chain each period's START from the previous period's end.

    An Amex statement states only its CLOSING date (statement_parser's module
    docstring), so a period's start cannot come from its own document. Chaining
    over the account's own set of periods is deterministic and needs nothing
    the collector does not already have. The oldest period has no predecessor
    and takes its own end — it anchors a balance, and nothing reads its start.

    Chained over the periods that PARSED, so a statement the gates refused
    stretches its successor's start back across the gap. That shows only in
    `statement_balances.period_start`, which no gold adapter reads; the
    closing figure and its period_end, which every consumer does read, are
    unaffected.
    """
    out: dict[int, int] = {}
    ordered = sorted(set(period_ends))
    for i, end in enumerate(ordered):
        out[end] = (ordered[i - 1] + 86400) if i else end
    return out


def statement_rows(account_external_id: str,
                   parsed: statement_parser.ParsedCardStatement) -> list[dict]:
    """Project one parsed statement's activity into silver ledger rows.

    The document signs a charge POSITIVE and a payment or credit NEGATIVE —
    the inverse of silver's convention — so every amount is negated, exactly as
    the activity JSON's are.

    A statement dates its rows by TRANSACTION date; the cycle bills by post
    date, and the document does not state one. `posted_at` therefore carries
    the transaction date, a deliberate approximation the payload records, and
    `txn_date` carries the same value so a consumer can see that the two are
    not independent here.
    """
    period_end = parsed.period_end
    if period_end is None:
        return []
    stamp = epoch_day(period_end)
    out = []
    for index, txn in enumerate(parsed.transactions):
        when = epoch_day(txn.when)
        out.append({
            # Deterministic and stable across re-parses: the period plus the
            # row's position within it. The document offers no id of its own.
            "txn_id": f"stmt:{account_external_id}:{period_end.isoformat()}"
                      f":{index:04d}",
            "posted_at": when,
            "amount": _money2(-float(txn.amount)),
            "kind": txn.kind,
            "description": txn.description or None,
            "merchant": txn.description or None,
            "category": None,       # the deep era carries no spend category
            "category_code": None,
            "txn_date": when,
            "statement_end_at": stamp,
            "currency": None,
            "is_pending": 0,
            "source": SOURCE_STATEMENT,
            "payload": {
                "statement_period_end": period_end.isoformat(),
                "section": txn.kind,
                "posting_date_marked": txn.posting_date,
                "provider_amount": str(txn.amount),
                # posted_at is the TRANSACTION date here; the document states
                # no post date at all.
                "posted_at_basis": "transaction_date",
            },
        })
    return out


def export_seam(conn, account_external_id: str) -> int | None:
    """The oldest row the ACTIVITY channel reaches for this account.

    Anchored to activity-sourced rows only, never to all rows: that is what
    keeps the two channels disjoint and keeps the seam from drifting as
    statements are added. None when the account has no activity loaded yet."""
    row = conn.execute(
        "SELECT MIN(posted_at) FROM transactions WHERE account_external_id=? "
        "AND source=?", (account_external_id, SOURCE_ACTIVITY)).fetchone()
    return row[0] if row and row[0] is not None else None


def activity_reach(conn, account_external_id: str) -> int | None:
    """The newest row the ACTIVITY channel reaches for this account.

    The seam's upper twin. A period lying between the two is one the activity
    covers end to end; one that runs past this is not, however new it looks.
    The bound is the DATA, not a narrower request: no verb exposes an upper
    end for the window (collectorkit.cli.resolve_lookback always ends today),
    so what leaves the gap is a card that simply posted nothing after the
    period closed — or a caller passing walk() an explicit `until`, which
    only the tests do."""
    row = conn.execute(
        "SELECT MAX(posted_at) FROM transactions WHERE account_external_id=? "
        "AND source=?", (account_external_id, SOURCE_ACTIVITY)).fetchone()
    return row[0] if row and row[0] is not None else None


def statement_balance_from_pdf(parsed: statement_parser.ParsedCardStatement,
                               period_start: int) -> dict:
    """A parsed statement's printed figures as a `statement_balances` row."""
    def num(value):
        return None if value is None else _money2(float(value))
    return {
        "period_start": period_start,
        "period_end": epoch_day(parsed.period_end),
        "opening": num(parsed.previous_balance),
        "closing": num(parsed.new_balance),
        "new_charges": num(parsed.new_charges),
        "payments_and_credits": num(parsed.payments_credits),
        "fees": num(parsed.fees),
        "interest": num(parsed.interest_charged),
    }


# ============================================================
# DB inserts
# ============================================================

def _insert_account(conn, snapshot_at: int, acct: dict,
                    pending: float | None) -> None:
    """Upsert one roster account, content-deduped: a new snapshot row lands
    only when the canonical payload changed since the last one.

    `pending` comes from the activity payload rather than the roster, which
    does not report it; it is folded into the dedup payload so a change in the
    unposted total is itself a new observation."""
    ext = str(acct.get("account_key") or "").strip()
    if not ext:
        return
    record = dict(acct)
    if pending is not None:
        record["pending_charges"] = pending
    payload = silver.canonical_json(record)
    prev = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id=? "
        "ORDER BY snapshot_at DESC LIMIT 1", (ext,)).fetchone()
    if prev is not None and prev[0] == payload:
        return  # content-dedup: unchanged since the last snapshot
    balance = acct.get("balance") or {}
    due = acct.get("payment_due") or {}
    conn.execute(
        "INSERT OR REPLACE INTO accounts (snapshot_at, account_external_id, "
        "account_token, display_name, mask, currency, balance, "
        "pending_charges, payment_due_at, account_status, line_of_business, "
        "user_type, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (snapshot_at, ext, acct.get("account_token") or None,
         acct.get("product_display_name") or None,
         acct.get("display_account_number") or None,
         balance.get("currency") or "USD",
         _money2(parse_money(balance.get("amount"))), _money2(pending),
         parse_date(due.get("date")), acct.get("account_status") or None,
         acct.get("line_of_business") or None, acct.get("user_type") or None,
         payload))


def _insert_transaction(conn, account_external_id: str, tx: dict) -> None:
    conn.execute(
        "INSERT OR IGNORE INTO transactions (txn_id, posted_at, "
        "account_external_id, amount, kind, description, merchant, category, "
        "category_code, txn_date, statement_end_at, currency, is_pending, "
        "source, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (tx["txn_id"], tx["posted_at"], account_external_id, tx["amount"],
         tx["kind"], tx["description"], tx["merchant"], tx["category"],
         tx["category_code"], tx["txn_date"], tx["statement_end_at"],
         tx["currency"], tx["is_pending"], tx["source"],
         silver.canonical_json(tx["payload"])))


def _replace_pending(conn, account_external_id: str, rows: list[dict]) -> int:
    """Replace this account's pending rows wholesale.

    A pending row's id is provisional — the charge gets a different, permanent
    id when it posts (DESIGN.md §D) — so accumulating pending rows would leave
    a stale twin beside every charge that later posted. Rebuilding the pending
    set from the newest run each load is the fleet's derived-table rule
    (rebuild from bronze, never patch incrementally), and it is what makes
    that convergence automatic. Runs are loaded oldest-first, so the newest
    run's view wins."""
    conn.execute("DELETE FROM transactions WHERE account_external_id=? "
                 "AND is_pending=1", (account_external_id,))
    for tx in rows:
        _insert_transaction(conn, account_external_id, tx)
    return len(rows)


def _insert_statement_balance(conn, snapshot_at: int,
                              account_external_id: str, row: dict,
                              source: str, *, replace: bool = True) -> None:
    """Record a period's balances, keyed on (account, period_end) so
    re-reading the same period converges on the newest copy.

    The two channels share that key, so a statement closing on the same day
    a run's own activity window ended displaces that run's row — the printed
    figures are the better statement of the period, and the statement
    channel wins a shared day. `replace=False` is how the rebuild
    puts the displaced activity row back before re-importing the statements
    over it, so an incremental load and a `--force` rebuild agree and a
    period whose statement copies are all refused keeps a mark."""
    verb = "INSERT OR REPLACE" if replace else "INSERT OR IGNORE"
    conn.execute(
        f"{verb} INTO statement_balances (account_external_id, "
        "period_start, period_end, opening, closing, new_charges, "
        "payments_and_credits, fees, interest, transactions_covered, source, "
        "snapshot_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (account_external_id, row["period_start"], row["period_end"],
         row["opening"], row["closing"], row["new_charges"],
         row["payments_and_credits"], row["fees"], row["interest"],
         row.get("transactions_covered", 0), source, snapshot_at))


def _insert_document(conn, snapshot_at: int, account_external_id: str,
                     pdf: Path) -> None:
    sha, size = bronze.sha256_file(pdf)
    kind = ("year_end_summary" if pdf.name.startswith(_YEAR_SUMMARY_PREFIX)
            else "statement")
    conn.execute(
        "INSERT OR IGNORE INTO documents (sha256, snapshot_at, "
        "account_external_id, doc_date, doc_kind, file_format, filename, "
        "size_bytes, payload) VALUES (?,?,?,?,?,?,?,?,?)",
        (sha, snapshot_at, account_external_id, document_date(pdf.name), kind,
         "pdf", pdf.name, size,
         silver.canonical_json({"source_name": pdf.name})))


# A statement PDF is filed under its safe-stemmed cycle end date
# (`YYYY-MM-DD.pdf`); a year-end summary under `yes-<YYYY>.pdf`, which carries
# a year but no day, so it gets no doc_date.
_DATE_YMD_RE = re.compile(r"(20\d{2})-(\d{2})-(\d{2})")


def document_date(name: str) -> int | None:
    """Best-effort document date from a filename. None when none is
    embedded — including for a year-end summary, whose name states only a
    year and would otherwise get a fabricated day."""
    if name.startswith(_YEAR_SUMMARY_PREFIX):
        return None
    m = _DATE_YMD_RE.search(name)
    if not m:
        return None
    try:
        return epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


# ============================================================
# Per-run loader
# ============================================================

_SAFE_STEM_RE = re.compile(r"[^\w.-]+")


def _log_id(value: str) -> str:
    """An account key shortened for the LOG — see download.log_id. Files keep
    the full key, which is what the tables join on."""
    stem = _safe_stem(value)
    return stem if len(stem) <= 12 else stem[:8] + "…"


def _safe_stem(value: str) -> str:
    """The filesystem-safe stem download.py derives a directory / file name
    from (kept in sync with download.safe_stem). Copied rather than imported:
    pulling download.py in for two string helpers would drag the browser-side
    chain behind it (download → login → explore, and Playwright with them)
    into a loader that never opens a browser."""
    stem = _SAFE_STEM_RE.sub("-", str(value)).strip("-.")
    return stem or "item"


def _read_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (ValueError, OSError):
        log.warning("%s: not valid JSON; skipping", path.name)
        return None


def _read_accounts(run_dir: Path) -> list[dict]:
    data = _read_json(run_dir / "accounts.json") if (
        run_dir / "accounts.json").is_file() else None
    if isinstance(data, dict):
        data = data.get("accounts", [])
    return [a for a in (data or []) if isinstance(a, dict)]


def _pending_total(payload: dict) -> float | None:
    """The unposted total the activity payload reports, when it does. Only the
    rolling views carry it (DESIGN.md §H), so a windowed run may not have
    one."""
    bd = payload.get("balancesDetails")
    if not isinstance(bd, dict):
        return None
    charges = (bd.get("pendingBalances") or {}).get("charges")
    if not isinstance(charges, dict):
        return None
    return _money2(parse_money(charges.get("amount")))


def _parse_statements(copies_by_name: dict) -> tuple[dict, int]:
    """Parse one usable copy of each period, newest run first.

    Returns ({period_end_epoch: (run_ts, ParsedCardStatement)}, parse_errors).

    Every run re-fetches the same statements, so a tree with N runs holds N
    copies of each period, and parsing costs a `pdftotext` subprocess apiece.
    The newest copy is tried first and the walk stops at the first that
    passes the gates — one parse per period whenever the newest render is
    good, which is the ordinary case. Older copies get a turn only when it is
    refused, which is what keeps one bad re-render from erasing a period an
    earlier run had read cleanly; a copy whose bytes match one already
    refused for that period is skipped without parsing, a hash being far
    cheaper than the subprocess.

    A statement whose own summary does not add up, or whose sections do not
    sum to the figures printed for them, was mis-read: nothing from it is
    kept — not its rows, not even its balances. (The accessible-PDF rendering
    is one such: it carries no summary this parser reads.) `parse_errors`
    counts documents that RAISED, which is the tooling-fault signal the
    rebuild gates on; a refusal by the gates is not one.
    """
    out: dict = {}
    errors = 0
    for name, chain in sorted(copies_by_name.items()):
        refused: set[str] = set()
        for index, (run_ts, slug, pdf) in enumerate(chain):
            digest = None
            if index:
                digest, _ = bronze.sha256_file(pdf)
                if digest in refused:
                    continue
            try:
                parsed = statement_parser.parse_card_statement_pdf(pdf)
            except Exception as exc:        # noqa: BLE001 — any parse failure
                log.warning("statement %s (run %s): parse failed (%r); "
                            "skipped", name, slug, exc)
                errors += 1
                continue
            if parsed.period_end is None:
                reason = "no closing date"
            elif not statement_parser.rows_reconcile(parsed):
                reason = "does not reconcile"
            else:
                if index:
                    log.warning("statement %s: the newest copy (run %s) was "
                                "refused; the copy from run %s stood in",
                                name, chain[0][1], slug)
                out[epoch_day(parsed.period_end)] = (run_ts, parsed)
                break
            log.warning("statement %s (run %s): %s; skipped",
                        name, slug, reason)
            if digest is None:
                digest, _ = bronze.sha256_file(pdf)
            refused.add(digest)
    return out, errors


def _statement_copies(bronze_dir: Path,
                      stems: dict[str, str]) -> tuple[dict, dict]:
    """One walk of bronze for both things the rebuild reads from it.

    Returns (copies, runs_by_until):

    * `copies[account][file name]` — every copy of that period, NEWEST RUN
      FIRST. Deduped on the PERIOD, never on the bytes: a re-rendered PDF is
      a new hash for the same statement, so byte-level dedup would parse it
      again (the fleet's dedupe-on-logical-identity rule). The older copies
      are kept because the newest is not always the readable one.
    * `runs_by_until[period_end]` — (run_ts, run_dir) for the complete run
      whose activity window closed on that day, which is the key an
      activity-channel balance row is recorded under.
    """
    copies: dict[str, dict[str, list]] = {}
    runs_by_until: dict[int, tuple[int, Path]] = {}
    # iter_run_dirs yields oldest first, so inserting at the head leaves each
    # period's chain newest first.
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        manifest = (_read_json(run_dir / "run.json")
                    if (run_dir / "run.json").is_file() else None)
        manifest = manifest if isinstance(manifest, dict) else {}
        if manifest.get("status") not in ("complete", None):
            continue
        try:
            run_ts = bronze.parse_run_ts(run_dir.name)
        except ValueError:
            continue
        until = parse_date(manifest.get("until"))
        if until is not None:
            runs_by_until[until] = (run_ts, run_dir)
        stmt_dir = run_dir / "statements"
        if not stmt_dir.is_dir():
            continue
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            acct_id = stems.get(acct_dir.name, acct_dir.name)
            for pdf in sorted(acct_dir.glob("*.pdf")):
                # A year-end summary is a different document with no billing
                # period; only the monthly statements are parsed.
                if pdf.name.startswith(_YEAR_SUMMARY_PREFIX):
                    continue
                copies.setdefault(acct_id, {}).setdefault(
                    pdf.name, []).insert(0, (run_ts, run_dir.name, pdf))
    return copies, runs_by_until


def _statement_period_count(conn) -> int:
    """How many statement-sourced periods silver currently holds."""
    row = conn.execute(
        "SELECT COUNT(*) FROM statement_balances WHERE source=?",
        (SOURCE_STATEMENT,)).fetchone()
    return row[0] if row else 0


def _restore_activity_balances(conn, account_external_id: str,
                               file_names, runs_by_until: dict) -> None:
    """Put back the activity-channel balance row each statement displaces.

    `statement_balances` is keyed (account, period_end), so a statement
    closing on the same day a run's own activity window ended replaced that
    run's row — and the run is never replayed, while this rebuild deletes the
    replacement on every load. Re-deriving the row from bronze first is what
    makes an incremental load and a `--force` rebuild converge, and what
    leaves a mark standing when a period's statement copies are all refused.

    Driven off the statement FILE NAMES, each of which is its period's
    closing date, rather than off what parsed — a period whose every copy is
    refused is exactly the one that would otherwise be left with no mark at
    all. One small JSON read per statement period, not a re-read of every
    run.
    """
    stem = _safe_stem(account_external_id)
    for name in file_names:
        period_end = document_date(name)
        if period_end is None:
            continue
        found = runs_by_until.get(period_end)
        if found is None:
            continue
        run_ts, run_dir = found
        payload = _read_json(run_dir / "activity" / f"{stem}.json")
        if not isinstance(payload, dict):
            continue
        row = statement_balance_row(payload)
        if row is None or row["period_end"] != period_end:
            continue
        _insert_statement_balance(conn, run_ts, account_external_id, row,
                                  SOURCE_ACTIVITY, replace=False)


def _import_statements(conn, account_external_id: str,
                       parsed_by_end: dict) -> None:
    """Write one account's parsed periods: the deep-era rows below the seam,
    and an anchor for every period either way.

    The seam gate is on the whole billing PERIOD, not each row's date: a
    statement dates rows by transaction date while the cycle bills by post
    date, so a row-level gate would let a row transacted before the seam but
    posted after it land from both channels. The one period straddling the
    seam is given up rather than double-counted, and marked
    `transactions_covered = 0`. So is any period that cannot be SHOWN to be
    covered: the archive's oldest, which has no predecessor to chain a start
    from, and any that runs past the newest activity row. Every other period
    is flagged covered — below the seam its rows are imported here, inside
    the activity's reach that channel carries them. An unflagged period looks
    exactly like any other pair of anchors, so a consumer reconciling
    transactions between two of them would otherwise be left with an
    unexplainable residual.
    """
    starts = statement_period_starts(list(parsed_by_end))
    # The oldest period has no predecessor to chain a start from, so its
    # start is unknown and it can never be shown to lie above the seam.
    oldest = min(parsed_by_end)
    seam = export_seam(conn, account_external_id)
    reach = activity_reach(conn, account_external_id)
    imported = 0
    for end, (run_ts, parsed) in sorted(parsed_by_end.items(),
                                        key=lambda item: item[0]):
        covered = 0
        if seam is not None and end < seam:
            # Wholly below the seam: this document is the only channel
            # holding these rows, so it is what imports them.
            for tx in statement_rows(account_external_id, parsed):
                _insert_transaction(conn, account_external_id, tx)
                imported += 1
            covered = 1
        elif (seam is not None and end != oldest
                and starts[end] >= seam
                and reach is not None and end <= reach):
            # Wholly INSIDE the activity's reach: that channel already
            # carries every row in the period, so the rows ARE in silver and
            # the flag says so. Bounded at both ends — chase's rule is the
            # lower half, and the upper half matters here because the
            # activity proves coverage only up to its newest row: a period
            # closing after the last posting stays uncovered.
            covered = 1
        balances = statement_balance_from_pdf(parsed, starts[end])
        balances["transactions_covered"] = covered
        # snapshot_at names the bronze run the SURVIVING copy came from, per
        # the schema comment — not whichever run happened to be loaded last.
        _insert_statement_balance(conn, run_ts, account_external_id, balances,
                                  SOURCE_STATEMENT)
    if seam is None:
        # No activity for this account, so there is no seam to bound against
        # and importing would put a statement copy of every row beside the
        # copy the first activity fetch lands. The anchors are recorded; the
        # rows arrive once the activity does.
        log.warning("  %s: %d statement period(s) recorded, rows deferred "
                    "until this account's activity is loaded",
                    _log_id(account_external_id), len(parsed_by_end))
    else:
        log.info("  %s: %d statement period(s), %d deep-era row(s) below "
                 "the seam", _log_id(account_external_id), len(parsed_by_end),
                 imported)


def rebuild_statements(conn, bronze_dir: Path) -> None:
    """Rebuild every statement-sourced row in silver, from all of bronze.

    **This is a derived table, not an incremental one, and it has to be.** The
    export seam is `MIN(posted_at)` over the account's ACTIVITY rows, and it
    MOVES: a run with a wider `--lookback` reaches further back, so periods
    that were below the seam yesterday are covered by the activity today. A
    statement row imported under the shallower seam would then sit beside the
    activity's own copy of the same charge — double-counted, and never
    withdrawn, because an incremental load only ever adds.

    That is exactly the fleet's rebuild-derived-tables rule, and it was
    learned here the expensive way: two loads at different windows in one
    session left the same charges on both sides of the seam (DESIGN.md §M).

    So: drop every statement-sourced row and period, re-parse the PDFs across
    all bronze runs, and re-apply the seam as it now stands. Idempotent by
    construction — the result depends only on what bronze holds, never on the
    order loads happened in.

    Two properties keep that rule from turning a fault into data loss, since
    the delete comes first and the deep era exists nowhere else:

    * **Parse first, write second, all in one transaction.** Every document
      is read before anything is deleted, and the delete + re-import run
      inside an explicit BEGIN IMMEDIATE, so a failure anywhere leaves silver
      exactly as it was rather than emptied.
    * **A tooling fault is not a rebuild.** If every parse RAISED and silver
      already holds statement periods, this raises instead of rebuilding —
      that shape is a broken `pdftotext`, not bronze losing its documents. A
      single unreadable document stays a per-document skip.
    """
    stems = _account_stems(conn)
    copies, runs_by_until = _statement_copies(bronze_dir, stems)

    parsed_by_account: dict[str, dict] = {}
    errors = 0
    for acct_id, by_name in copies.items():
        parsed_by_end, failed = _parse_statements(by_name)
        errors += failed
        if parsed_by_end:
            parsed_by_account[acct_id] = parsed_by_end
    if errors and not parsed_by_account and _statement_period_count(conn):
        raise RuntimeError(
            f"every statement parse failed ({errors} document(s)) while "
            f"silver already holds statement-sourced periods. Refusing to "
            f"rebuild: that shape is a parse-tooling fault, not bronze "
            f"losing its documents, and rebuilding would drop the deep era. "
            f"Check `pdftotext`, then re-run `load --force`.")

    conn.execute("BEGIN IMMEDIATE")
    try:
        conn.execute("DELETE FROM transactions WHERE source=?",
                     (SOURCE_STATEMENT,))
        conn.execute("DELETE FROM statement_balances WHERE source=?",
                     (SOURCE_STATEMENT,))
        for acct_id, by_name in copies.items():
            _restore_activity_balances(conn, acct_id, by_name, runs_by_until)
        for acct_id, parsed_by_end in parsed_by_account.items():
            _import_statements(conn, acct_id, parsed_by_end)
        conn.execute("COMMIT")
    except Exception:
        try:
            conn.execute("ROLLBACK")
        except sqlite3.Error:
            pass            # already unwound; the original error is the one
        raise


def _account_stems(conn) -> dict[str, str]:
    """Map each account's safe-stemmed directory name back to its key."""
    rows = conn.execute(
        "SELECT DISTINCT account_external_id FROM accounts").fetchall()
    return {_safe_stem(r[0]): r[0] for r in rows}


def load_run(conn: sqlite3.Connection, run_dir: Path) -> bool:
    """Load one bronze run dir. Returns True if ingested, False if skipped
    (non-complete dump, or already loaded). Idempotent."""
    status = bronze.run_status(run_dir / "run.json")
    if status not in ("complete", None):
        # in-progress / dry-run shells are not silver inputs.
        log.info("skip %s (status=%s)", run_dir.name, status)
        return False
    try:
        snapshot_at = bronze.parse_run_ts(run_dir.name)
    except ValueError:
        log.warning("skip %s (unparseable run slug)", run_dir.name)
        return False
    if snapshot_at in silver.loaded_snapshots(conn):
        return False

    accounts = _read_accounts(run_dir)
    # A statement dir / activity file is named by the safe-stemmed key; map it
    # back so every table keys on the same account_external_id.
    stem_to_id = {_safe_stem(a["account_key"]): str(a["account_key"])
                  for a in accounts if a.get("account_key")}

    # The activity pass runs first: it is what reports the unposted total the
    # roster snapshot records.
    pending_by_account: dict[str, float | None] = {}
    activity_dir = run_dir / "activity"
    if activity_dir.is_dir():
        for path in sorted(activity_dir.glob("*.json")):
            payload = _read_json(path)
            if not isinstance(payload, dict):
                continue
            acct_id = stem_to_id.get(path.stem, path.stem)
            rows = activity_rows(payload)
            # The window a manifest advertises is what was ASKED for; this is
            # what came back. A run whose fetch stopped short is still
            # loadable and still 'complete' (silver ingest is additive) but
            # the shortfall has to be visible, or the seam it moves looks
            # like the account's real reach. Covers runs already on disk,
            # whose manifests predate the download side's coverage block.
            #
            # Measured on the payload, never on `rows`: the projection also
            # drops a row the ledger cannot key (no post date, no amount, no
            # identifier), and counting those as a shortfall would send a
            # reader hunting a pagination bug that never happened.
            expected = payload.get("totalTransactionCount")
            fetched = len(payload.get("transactions") or [])
            if isinstance(expected, int) and fetched < expected:
                log.warning("  %s: activity holds %d of the %d rows the "
                            "source reported for the window — that fetch "
                            "came back short", _log_id(acct_id), fetched,
                            expected)
            posted = [r for r in rows if not r["is_pending"]]
            pending = [r for r in rows if r["is_pending"]]
            for tx in posted:
                _insert_transaction(conn, acct_id, tx)
            _replace_pending(conn, acct_id, pending)
            pending_by_account[acct_id] = _pending_total(payload)
            balances = statement_balance_row(payload)
            if balances is not None:
                _insert_statement_balance(conn, snapshot_at, acct_id, balances,
                                          SOURCE_ACTIVITY)
            log.info("  %s: %d posted, %d pending", _log_id(acct_id),
                     len(posted), len(pending))

    for acct in accounts:
        _insert_account(conn, snapshot_at, acct,
                        pending_by_account.get(str(acct.get("account_key"))))

    # Documents are inventoried per run (a PDF's first sighting is its
    # snapshot); their CONTENT is read by rebuild_statements, once all runs
    # are in, because what it imports depends on the seam every run together
    # produces.
    stmt_dir = run_dir / "statements"
    if stmt_dir.is_dir():
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            acct_id = stem_to_id.get(acct_dir.name, acct_dir.name)
            for pdf in sorted(acct_dir.glob("*.pdf")):
                _insert_document(conn, snapshot_at, acct_id, pdf)

    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version,"
        " run_dir) VALUES (?,?,?)",
        (snapshot_at, silver.current_schema_version(conn), str(run_dir)))
    conn.commit()
    log.info("loaded %s (%d account(s))", run_dir.name, len(accounts))
    return True


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (holds the <UTC-ts>/ run dirs).")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite path. Default: <bronze-dir>/amex.db.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)

    # Preflight, before anything is written or recorded: the statement
    # rebuild DELETES the deep era and re-imports what parses, so a missing
    # `pdftotext` would empty it rather than skip it. Failing here leaves
    # silver untouched and the next invocation retries cleanly.
    if shutil.which("pdftotext") is None:
        log.error("`pdftotext` (poppler-utils) is not on PATH, so no "
                  "statement PDF can be read. Refusing to load: the "
                  "statement rebuild would drop the deep era rather than "
                  "skip it. Install poppler-utils and re-run.")
        return 1

    db_path = args.silver_db or (args.bronze_dir / "amex.db")
    if args.force:
        silver.reset(db_path)
    conn = silver.open_db(db_path)
    try:
        silver.apply_migrations(conn, MIGRATIONS_DIR)
        loaded = 0
        for run_dir in bronze.iter_run_dirs(args.bronze_dir):
            if load_run(conn, run_dir):
                loaded += 1
        if loaded:
            # Only after every run is in: the seam this depends on is the
            # MIN over all of them together.
            try:
                rebuild_statements(conn, args.bronze_dir)
            except RuntimeError as exc:
                log.error("%s", exc)
                return 1
        log.info("done: %d run(s) ingested into %s", loaded, db_path)
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
