#!/usr/bin/env python3
"""raiffeisen_at silver loader: bronze REST captures → source-shaped SQLite.

Parses each bronze run dir (see download.py for the layout) into the silver
schema (migrations/): the account roster (enriched with the account-info
detail), the transaction ledger, the daily closing-balance series, and the
statement inventory. Idempotent — an already-loaded dump is skipped, and
transactions INSERT OR IGNORE on their stable id, so re-running converges.

The `kontoumsaetze` history JSON (`history/<iban>.json`) is the authoritative
ledger: every row carries a stable `id`, a signed `betrag.amount`, and both a
booking (`buchungstag`) and value (`valuta`) date. Unlike the US siblings the
history has **no per-row running balance**; the closing-balance time series
comes from the `kontostaende` daily series (`balances/<iban>.json`) into the
`daily_balances` table. Statements are ingested as documents (PDFs belong in
bronze), not as a transaction source — the archive reaches only ~5 months
past the history floor (DESIGN.md §G).

The deep past comes from transaction listings the bank supplies on request,
dropped into `<bronze-dir>/supplied/` and read on every load (DESIGN.md §I):
listing_parser.py reads each one, stitch.py decides which days each listing
owns next to the live history, and `load_supplied` below writes that layer —
re-derived every run, so a new listing, a removed one, or a deeper download
converges without duplicates.

Browserless: imports only collectorkit, the stdlib and this collector's own
pure modules, so the `load` runtime never pulls in login.py's Playwright
dependency.

Bronze run-dir layout consumed:

    <run>/
      run.json                      status manifest
      accounts.json                 [{iban, type, balance:{amount,currency}, …}, …]
      details/<iban>.json           account-information detail (detailgruppen)
      history/<iban>.json           {iban, minBuchungstag, complete, transactions:[…]}
      balances/<iban>.json          {tagessalden:[{tag, saldo}], kontostand, …}
      statements/<iban>/<date>_<sys>_<id>[_v<n>].pdf

and beside the run dirs:

    supplied/                       transaction listings (any *.pdf / *.PDF)
"""
from __future__ import annotations

import argparse
import hashlib
import json
import logging
import re
import sqlite3
import sys
from collections import Counter, defaultdict
from dataclasses import dataclass
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

from collectorkit import bronze, cli, pdftotext, silver

import listing_parser
import stitch

log = logging.getLogger("raiffeisen_at.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Re-export so tests + siblings can call load.apply_migrations directly.
apply_migrations = silver.apply_migrations

# transactions.source / daily_balances.source tags. The kontoumsaetze JSON is
# the live source of record; supplied transaction listings fill the days no
# download covers (DESIGN.md §I). LIVE_SOURCES is the one place that says which
# rows are live — the stitch's ownership hangs on it.
SOURCE_HISTORY = "history"
SOURCE_SUPPLIED = "supplied_listing"
LIVE_SOURCES = (SOURCE_HISTORY,)

# documents.doc_kind of a supplied listing, and the directory listings are
# read from, relative to the bronze dir.
DOC_KIND_LISTING = "transaction_listing"
SUPPLIED_DIRNAME = "supplied"


# ============================================================
# Date / money parsing
# ============================================================

def _epoch_day(d) -> int:
    """Unix seconds UTC at midnight of the given datetime or date."""
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


def parse_date(raw) -> int | None:
    """An ISO date (`YYYY-MM-DD`, optionally with a time) → epoch-midnight
    seconds. Mein ELBA renders `buchungstag` / `valuta` / `tag` as ISO dates.
    None on anything unparseable."""
    if raw is None:
        return None
    m = re.match(r"(\d{4})-(\d{2})-(\d{2})", str(raw).strip())
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


def parse_money(raw) -> float | None:
    """Coerce a money value to float. The REST amounts arrive as native
    numbers (`betrag.amount`, `saldo`); a numeric string is tolerated. None on
    blank/unparseable. (German-formatted display strings never reach a numeric
    column — those live only in the detail payload.)"""
    if raw is None:
        return None
    if isinstance(raw, (int, float)):
        return float(raw)
    s = str(raw).strip()
    if not s:
        return None
    try:
        return float(s)
    except ValueError:
        return None


# ============================================================
# History JSON → ledger rows
# ============================================================

def _first(d: dict, *keys, default=None):
    """First present, non-None value among `keys` — tolerance for minor wire
    naming drift."""
    for k in keys:
        if k in d and d[k] is not None:
            return d[k]
    return default


def _amount_and_currency(row: dict) -> tuple[float | None, str | None]:
    """The signed amount + currency from a history row's `betrag`
    (`{amount, currency}`). `amount` is already signed at the source (negative
    = debit)."""
    betrag = row.get("betrag")
    if not isinstance(betrag, dict):
        return None, None
    return parse_money(betrag.get("amount")), (
        betrag.get("currency") or betrag.get("currencyCode"))


def _hash_txn_id(iban: str, posted_at: int, amount: float,
                 description: str | None) -> str:
    """Stable synthetic id for a row with no `id` (never observed, but
    tolerated so a malformed row is not silently dropped). Deterministic, so
    re-loads converge on it."""
    basis = "|".join([iban, str(posted_at), f"{amount:.2f}",
                      (description or "").strip()])
    return "syn_" + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:24]


def history_rows(iban: str, transactions: list) -> list[dict]:
    """Project the `kontoumsaetze` `transactions` list into silver ledger
    rows. A row missing a parseable booking date or amount is skipped;
    everything else is preserved in `payload`."""
    out = []
    for row in transactions:
        if not isinstance(row, dict):
            continue
        posted_at = parse_date(row.get("buchungstag"))
        amount, currency = _amount_and_currency(row)
        if posted_at is None or amount is None:
            continue
        description = _first(row, "verwendungszweckZeile1", "verwendungszweck")
        description = str(description).strip() if description else None
        txn_id = row.get("id")
        txn_id = str(txn_id).strip() if txn_id is not None else ""
        if not txn_id:
            txn_id = _hash_txn_id(iban, posted_at, amount, description)
        counterparty = _first(row, "transaktionsteilnehmerZeile1")
        out.append({
            "txn_id": txn_id,
            "posted_at": posted_at,
            "value_at": parse_date(row.get("valuta")),
            "amount": amount,
            "currency": currency,
            "kind": "DEBIT" if amount < 0 else "CREDIT",
            "category": _first(row, "kategorieCode", "autoKategorieCode"),
            "description": description,
            "counterparty": str(counterparty).strip() if counterparty else None,
            "source": SOURCE_HISTORY,
            "payload": row,
        })
    return out


# ============================================================
# Account-information detail → curated attributes
# ============================================================

# Deposit-relevant detail rows, keyed by their German `bezeichnung`. The card
# block (Karteninformationen) and the account-holder name (Kontobezeichnung)
# are deliberately NOT promoted — deposit-only scope, and the name is PII that
# needn't leave the detail out of an account attribute.
_DETAIL_FIELDS = {
    "Kontoart": "kontoart",
    "Währung": "currency",
    "Kontoführendes Institut": "institution",
    "BIC": "bic",
    "Aktueller Zinssatz Haben": "interest_credit",
    "Aktueller Zinssatz Soll": "interest_debit",
}


def parse_details(details: dict) -> dict:
    """Curate the account-information `details` payload (DESIGN.md §C) into the
    handful of deposit-relevant attributes silver keeps: account type,
    currency, holding institution + BIC, and the credit / debit interest-rate
    strings. Tolerant of a missing/misshaped payload (returns {})."""
    if not isinstance(details, dict):
        return {}
    out = {}
    for gruppe in details.get("detailgruppen") or []:
        if not isinstance(gruppe, dict):
            continue
        for row in gruppe.get("details") or []:
            if not isinstance(row, dict):
                continue
            key = _DETAIL_FIELDS.get(str(row.get("bezeichnung", "")))
            if not key:
                continue
            inhalt = row.get("inhalt")
            if isinstance(inhalt, list):
                inhalt = " ".join(str(x) for x in inhalt if x)
            value = str(inhalt).strip() if inhalt else None
            if value:
                out[key] = value
    return out


# ============================================================
# Balances → daily series rows
# ============================================================

def balance_rows(balances: dict) -> list[dict]:
    """Project a `kontostaende` payload's `tagessalden` (`[{tag, saldo}]`) into
    `{balance_date, balance}` rows. A row with an unparseable date or saldo is
    skipped."""
    out = []
    if not isinstance(balances, dict):
        return out
    for entry in balances.get("tagessalden") or []:
        if not isinstance(entry, dict):
            continue
        day = parse_date(entry.get("tag"))
        saldo = parse_money(entry.get("saldo"))
        if day is None or saldo is None:
            continue
        out.append({"balance_date": day, "balance": saldo})
    return out


# ============================================================
# Roster projection
# ============================================================

def _mask(iban: str) -> str | None:
    """A human-facing last-4 mask ("…1234") from the IBAN. Never more."""
    digits = re.sub(r"\D", "", str(iban or ""))
    if len(digits) >= 4:
        return "…" + digits[-4:]
    return None


def account_row(acct: dict, details: dict | None) -> dict | None:
    """Merge a roster account (`accounts.json`) with its curated detail
    attributes into the silver account projection. Returns None if the row
    carries no IBAN."""
    iban = str(acct.get("iban") or "").strip()
    if not iban:
        return None
    curated = parse_details(details or {})
    bal = acct.get("balance") if isinstance(acct.get("balance"), dict) else {}
    currency = curated.get("currency") or bal.get("currency")
    return {
        "account_external_id": iban,
        "account_type": curated.get("kontoart") or acct.get("type") or None,
        "nickname": None,
        "mask": _mask(iban),
        "currency": currency,
        "balance": parse_money(bal.get("amount")),
        # Content-dedup payload: the roster row folded with the curated detail.
        "payload": {**acct, "details": curated},
    }


# ============================================================
# DB inserts
# ============================================================

def _insert_account(conn, snapshot_at: int, row: dict) -> None:
    """Upsert one roster account, content-deduped: a new snapshot row lands
    only when the canonical payload changed since the last one."""
    ext = row["account_external_id"]
    payload = silver.canonical_json(row["payload"])
    prev = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id=? "
        "ORDER BY snapshot_at DESC LIMIT 1", (ext,)).fetchone()
    if prev is not None and prev[0] == payload:
        return  # content-dedup: unchanged since the last snapshot
    conn.execute(
        "INSERT OR REPLACE INTO accounts (snapshot_at, account_external_id, "
        "account_type, nickname, mask, currency, balance, payload) "
        "VALUES (?,?,?,?,?,?,?,?)",
        (snapshot_at, ext, row["account_type"], row["nickname"], row["mask"],
         row["currency"], row["balance"], payload))


def _insert_transaction(conn, iban: str, tx: dict) -> None:
    conn.execute(
        "INSERT OR IGNORE INTO transactions (txn_id, account_external_id, "
        "posted_at, value_at, amount, currency, kind, category, description, "
        "counterparty, source, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (tx["txn_id"], iban, tx["posted_at"], tx["value_at"], tx["amount"],
         tx["currency"], tx["kind"], tx["category"], tx["description"],
         tx["counterparty"], tx["source"], silver.canonical_json(tx["payload"])))


def _insert_balance(conn, snapshot_at: int, iban: str, row: dict) -> None:
    conn.execute(
        "INSERT OR REPLACE INTO daily_balances (account_external_id, "
        "balance_date, balance, snapshot_at) VALUES (?,?,?,?)",
        (iban, row["balance_date"], row["balance"], snapshot_at))


def _insert_document(conn, snapshot_at: int, iban: str, pdf: Path) -> None:
    sha, size = bronze.sha256_file(pdf)
    silver.record_document(conn, (
        sha, snapshot_at, iban, _statement_date_from_name(pdf.name),
        "statement", "pdf", pdf.name, size,
        silver.canonical_json({"source_name": pdf.name})))


# The statement filename is `<YYYY-MM-DD>_<system>_<id>[_v<n>].pdf`
# (download.py stems the document's creation date + its stable key).
_DATE_YMD_RE = re.compile(r"(20\d{2})-(\d{2})-(\d{2})")


def _statement_date_from_name(name: str) -> int | None:
    """Best-effort statement date from a filename like
    `2026-08-01_EAZ_....pdf`. None when no date is embedded."""
    m = _DATE_YMD_RE.search(name)
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


# ============================================================
# Per-run loader
# ============================================================

def _read_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        log.warning("%s: missing or not valid JSON; skipping", path.name)
        return None


def _read_accounts(run_dir: Path) -> list[dict]:
    data = _read_json(run_dir / "accounts.json")
    if isinstance(data, dict):
        data = data.get("accounts", [])
    return [a for a in data if isinstance(a, dict)] if isinstance(data, list) else []


def load_run(conn: sqlite3.Connection, run_dir: Path) -> bool:
    """Load one bronze run dir, recording what the download covered in
    `run_windows` for the supplied-listing stitch. Returns True if ingested,
    False if skipped (non-complete dump, or already loaded). Idempotent; the
    caller owns the transaction (`main` makes each run one)."""
    status = bronze.run_status(run_dir / "run.json")
    if not bronze.is_loadable_status(status):
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
    for acct in accounts:
        iban = str(acct.get("iban") or "").strip()
        if not iban:
            continue
        details = _read_json(run_dir / "details" / f"{iban}.json")
        row = account_row(acct, details)
        if row is not None:
            _insert_account(conn, snapshot_at, row)

    hist_dir = run_dir / "history"
    if hist_dir.is_dir():
        for hist in sorted(hist_dir.glob("*.json")):
            data = _read_json(hist)
            if not isinstance(data, dict):
                continue
            iban = str(data.get("iban") or hist.stem).strip()
            for tx in history_rows(iban, data.get("transactions") or []):
                _insert_transaction(conn, iban, tx)

    bal_dir = run_dir / "balances"
    if bal_dir.is_dir():
        for balf in sorted(bal_dir.glob("*.json")):
            data = _read_json(balf)
            for row in balance_rows(data or {}):
                _insert_balance(conn, snapshot_at, balf.stem, row)

    stmt_dir = run_dir / "statements"
    if stmt_dir.is_dir():
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            for pdf in sorted(acct_dir.glob("*.pdf")):
                _insert_document(conn, snapshot_at, acct_dir.name, pdf)

    _insert_windows(conn, snapshot_at, run_windows(run_dir))
    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version,"
        " run_dir) VALUES (?,?,?)",
        (snapshot_at, silver.current_schema_version(conn), str(run_dir)))
    log.info("loaded %s (%d account(s))", run_dir.name, len(accounts))
    return True


# ============================================================
# Supplied transaction listings (DESIGN.md §I)
# ============================================================
#
# Listings in `<bronze-dir>/supplied/` are read on every load. Each one is
# parsed and held to its own arithmetic (listing_parser), bound to an account,
# and stitched against the live history (stitch). The resulting layer — the
# `source='supplied_listing'` transactions and balances plus one `documents`
# row per bound listing — is recomputed from scratch each run and replaces the
# stored layer only when it differs, under the guards below.

_TXN_COLUMNS = ("txn_id", "account_external_id", "posted_at", "value_at",
                "amount", "currency", "kind", "category", "description",
                "counterparty", "source", "payload")
_DAY = timedelta(days=1)


def _day(epoch: int) -> date:
    return datetime.fromtimestamp(epoch, tz=timezone.utc).date()


def _iso_day(raw) -> date | None:
    epoch = parse_date(raw)
    return _day(epoch) if epoch is not None else None


def _cents(amount: float) -> int:
    return int(round(amount * 100))


def _printed_epoch(listing: listing_parser.Listing) -> int:
    """A listing's print time as printed (the branch's local wall clock),
    stamped as UTC — deterministic, so a re-derived row keeps its stamp."""
    return int(listing.printed_at.replace(tzinfo=timezone.utc).timestamp())


def _read_json_quiet(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None


# ---- what each download covered ---------------------------------------------

def run_windows(run_dir: Path) -> list[tuple[str, str, int, int]]:
    """What one complete download covered, per IBAN, as
    `(iban, kind, first certified day, last day)` rows for `run_windows`:
    transactions from `max(since, minBuchungstag)` (the `--lookback` window,
    floored by the server's history floor) and balances over the
    `kontostaende` `[von, bis]`. The last day is the run's own and is partial
    (stitch.Window). A history walk that did not reach its last page —
    `complete` false, or absent in runs older than the flag — certifies only
    from the day after the oldest day it returned: the pages it missed are
    the oldest ones, and a page boundary can cut that oldest day."""
    try:
        run_day = _day(bronze.parse_run_ts(run_dir.name))
    except ValueError:
        return []
    manifest = _read_json_quiet(run_dir / "run.json")
    manifest = manifest if isinstance(manifest, dict) else {}
    since = _iso_day(manifest.get("since"))
    until = _iso_day(manifest.get("until")) or run_day
    out = []
    for hist in sorted((run_dir / "history").glob("*.json")):
        data = _read_json_quiet(hist)
        if not isinstance(data, dict):
            continue
        iban = str(data.get("iban") or hist.stem).strip()
        booked = [d for d in (_iso_day(t.get("buchungstag"))
                              for t in data.get("transactions") or []
                              if isinstance(t, dict)) if d]
        declared = [d for d in (since, _iso_day(data.get("minBuchungstag"))) if d]
        if data.get("complete") is True:
            start = max(declared) if declared else (min(booked) if booked else None)
        else:
            start = min(booked) + _DAY if booked else None
            if start and declared:
                start = max(start, max(declared))
        if start and start <= until:
            out.append((iban, "transactions", _epoch_day(start), _epoch_day(until)))
    for balf in sorted((run_dir / "balances").glob("*.json")):
        data = _read_json_quiet(balf)
        if not isinstance(data, dict):
            continue
        von, bis = _iso_day(data.get("von")), _iso_day(data.get("bis")) or until
        if von and von <= bis:
            out.append((balf.stem, "balances", _epoch_day(von), _epoch_day(bis)))
    return out


def _insert_windows(conn, snapshot_at: int, windows) -> None:
    conn.executemany(
        "INSERT OR REPLACE INTO run_windows (snapshot_at, account_external_id, "
        "kind, first_day, last_day) VALUES (?,?,?,?,?)",
        [(snapshot_at, *w) for w in windows])


def _backfill_windows(conn, bronze_dir: Path) -> None:
    """Record the windows of runs loaded before `run_windows` existed, once."""
    have = {r[0] for r in conn.execute("SELECT DISTINCT snapshot_at FROM run_windows")}
    loaded = silver.loaded_snapshots(conn)
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        try:
            snapshot_at = bronze.parse_run_ts(run_dir.name)
        except ValueError:
            continue
        if snapshot_at in loaded and snapshot_at not in have:
            _insert_windows(conn, snapshot_at, run_windows(run_dir))


def live_windows(conn) -> dict[str, tuple[list, list]]:
    """Per IBAN, the transaction and balance windows of every loaded run."""
    out: dict[str, tuple[list, list]] = defaultdict(lambda: ([], []))
    for iban, kind, first, last in conn.execute(
            "SELECT account_external_id, kind, first_day, last_day "
            "FROM run_windows ORDER BY snapshot_at"):
        out[iban][0 if kind == "transactions" else 1].append(
            stitch.Window(_day(first), _day(last)))
    return out


def _live_view(conn, iban: str, windows) -> stitch.LiveView:
    marks = ",".join("?" * len(LIVE_SOURCES))
    postings: dict[date, Counter] = defaultdict(Counter)
    for posted_at, amount in conn.execute(
            f"SELECT posted_at, amount FROM transactions WHERE "
            f"account_external_id=? AND source IN ({marks})", (iban, *LIVE_SOURCES)):
        postings[_day(posted_at)][_cents(amount)] += 1
    balances = {_day(d): _cents(b) for d, b in conn.execute(
        f"SELECT balance_date, balance FROM daily_balances WHERE "
        f"account_external_id=? AND source IN ({marks})", (iban, *LIVE_SOURCES))}
    txn, bal = windows.get(iban, ([], []))
    return stitch.LiveView(txn_windows=list(txn), balance_windows=list(bal),
                           postings=dict(postings), balances=balances)


def _view(views: dict[str, stitch.LiveView], iban: str) -> stitch.LiveView:
    """An account's live view; an empty one for an account without a view."""
    return views.get(iban) or stitch.LiveView()


# ---- reading the files --------------------------------------------------------

def _bind(conn, account_number: str) -> str | None:
    """The IBAN a listing's account number belongs to: an Austrian IBAN ends
    in the 11-digit, zero-padded account number. Exactly one silver account
    must match, else the listing is not bound."""
    padded = account_number.zfill(11)
    hits = [row[0] for row in conn.execute(
        "SELECT DISTINCT account_external_id FROM accounts")
        if str(row[0]).endswith(padded)]
    return hits[0] if len(hits) == 1 else None


@dataclass
class _Doc:
    """One file in `supplied/`, as far as it could be read."""
    path: Path
    sha: str
    size: int
    listing: listing_parser.Listing | None = None
    iban: str | None = None
    problems: list[str] | None = None


def _read_docs(files: list[Path], contributors: set[str], conn,
               extract) -> list[_Doc] | None:
    """Read, recognise, parse and bind every file; a file that fails any step
    is skipped and never stops the others. None means stop and keep the
    stored layer (A4): the extractor is missing, or a listing that
    contributed before can no longer be read."""
    docs: list[_Doc] = []
    for path in files:
        sha, size = bronze.sha256_file(path)
        twin = next((d for d in docs if d.sha == sha), None)
        if twin is not None:
            log.info("supplied: %s is a copy of %s; ignored", path.name,
                     twin.path.name)
            continue
        doc = _Doc(path, sha, size)
        docs.append(doc)
        try:
            text = extract(path)
        except pdftotext.ToolMissing as exc:
            log.error("supplied: %s; keeping the stored supplied layer", exc)
            return None
        except pdftotext.ExtractionError as exc:
            if sha in contributors:
                log.error("supplied: %s contributed before but cannot be read "
                          "now (%s); keeping the stored supplied layer",
                          path.name, exc)
                return None
            log.error("supplied: %s cannot be read (%s); skipped", path.name, exc)
            continue
        if not listing_parser.is_listing(text):
            log.warning("supplied: %s is not a transaction listing; skipped",
                        path.name)
            continue
        try:
            doc.listing = listing_parser.parse_listing(text)
            doc.problems = listing_parser.problems(doc.listing)
        except Exception as exc:                  # one bad file, not the load
            log.error("supplied: %s is unreadable as a listing (%s: %s); "
                      "skipped", path.name, type(exc).__name__, exc)
            doc.listing = None
            continue
        doc.iban = _bind(conn, doc.listing.account_number)
        if doc.iban is None:
            log.error("supplied: %s names an account that matches no single "
                      "account in silver; skipped", path.name)
    return docs


# ---- the layer ------------------------------------------------------------------

@dataclass
class _Layer:
    txns: dict        # txn_id -> row
    balances: dict    # (iban, balance_date) -> (balance, snapshot_at)
    docs: dict        # sha256 -> row


def _stored_layer(conn) -> _Layer:
    return _Layer(
        txns={r[0]: tuple(r) for r in conn.execute(
            f"SELECT {', '.join(_TXN_COLUMNS)} FROM transactions "
            f"WHERE source=?", (SOURCE_SUPPLIED,))},
        balances={(r[0], r[1]): (r[2], r[3]) for r in conn.execute(
            "SELECT account_external_id, balance_date, balance, snapshot_at "
            "FROM daily_balances WHERE source=?", (SOURCE_SUPPLIED,))},
        docs={r[0]: tuple(r) for r in conn.execute(
            f"SELECT {', '.join(silver.DOCUMENT_COLUMNS)} FROM documents "
            f"WHERE doc_kind=?", (DOC_KIND_LISTING,))},
    )


def _accepted(doc_row: tuple) -> bool:
    """Whether a listing's `documents` row records that it stitched."""
    return json.loads(doc_row[-1]).get("status") == "accepted"


def _live_seen(view: stitch.LiveView, day: date) -> dict:
    """What live saw of a day, in the form a supplied row records it."""
    return {str(a): n for a, n in sorted(view.postings_on(day).items())}


def _posting_row(iban: str, doc: _Doc, posting, occurrence: int,
                 live_seen: dict) -> tuple:
    lst = doc.listing
    payload = {
        "butag": posting.butag.isoformat(), "an": posting.an,
        "prnr": posting.prnr, "herk": posting.herk, "txt": posting.txt,
        "valuta": posting.valuta.isoformat(),
        "umsatz_cents": posting.amount_cents,
        "saldo_cents": posting.saldo_cents,
        "druckdatum": (posting.druckdatum.isoformat()
                       if posting.druckdatum else None),
        "art": posting.art or None, "lines": list(posting.lines),
        "page": posting.page, "doc_sha256": doc.sha,
    }
    if live_seen:
        payload["live_seen"] = live_seen
    return (listing_parser.txn_id(iban, posting, occurrence), iban,
            _epoch_day(posting.butag), _epoch_day(posting.valuta),
            posting.amount_cents / 100, lst.currency,
            "DEBIT" if posting.amount_cents < 0 else "CREDIT", None,
            posting.description, posting.counterparty, SOURCE_SUPPLIED,
            silver.canonical_json(payload))


def _doc_row(doc: _Doc, outcome: stitch.Outcome) -> tuple:
    lst = doc.listing
    payload = {
        "source_name": doc.path.name,
        "status": outcome.status,
        "reason": outcome.reason,
        "coverage": [lst.coverage_start.isoformat(), lst.print_day.isoformat()],
        "owned": [[a.isoformat(), b.isoformat()] for a, b in outcome.owned],
    }
    return (doc.sha, _printed_epoch(lst), doc.iban, _epoch_day(lst.print_day),
            DOC_KIND_LISTING, "pdf", doc.path.name, doc.size,
            silver.canonical_json(payload))


def _new_layer(docs: list[_Doc], views: dict[str, stitch.LiveView]):
    """Stitch every bound listing against its account's live history and
    project the result into rows. Also returns, per (iban, day) a listing
    speaks for, that listing's postings — what A5 holds the write to."""
    layer = _Layer({}, {}, {})
    expected: dict[tuple[str, date], Counter] = {}
    by_account: dict[str, list[_Doc]] = defaultdict(list)
    for doc in docs:
        if doc.iban:
            by_account[doc.iban].append(doc)
    for iban, account_docs in sorted(by_account.items()):
        live = views[iban]
        admitted = [d for d in account_docs if not d.problems]
        outcomes = stitch.stitch(live, [
            stitch.Candidate(d.sha, d.path.name, d.listing) for d in admitted])
        for doc in account_docs:
            outcome = outcomes.get(doc.sha) or stitch.Outcome(
                "rejected", "; ".join(doc.problems))
            layer.docs[doc.sha] = _doc_row(doc, outcome)
            if outcome.status != "accepted":
                log.error("supplied: %s rejected — %s", doc.path.name,
                          outcome.reason)
                continue
            owned = {d for a, b in outcome.owned for d in stitch.days_between(a, b)}
            lst = doc.listing
            left: dict[date, Counter] = {}
            for posting, n in zip(lst.postings,
                                  listing_parser.occurrences(lst.postings),
                                  strict=True):
                day = posting.butag
                if day not in owned:
                    continue
                expected.setdefault((iban, day), Counter())[posting.amount_cents] += 1
                # On a day live saw part of, contribute only what it lacks.
                unseen = left.setdefault(day, Counter(live.postings_on(day)))
                if unseen[posting.amount_cents] > 0:
                    unseen[posting.amount_cents] -= 1
                    continue
                row = _posting_row(iban, doc, posting, n, _live_seen(live, day))
                layer.txns[row[0]] = row
            # The listing's closing balance stands wherever live has no
            # certified one — including over a download's own, partial day.
            for day, saldo in lst.closing_balances().items():
                if day in owned and day not in live.balance_certified:
                    layer.balances[(iban, _epoch_day(day))] = (
                        saldo / 100, _printed_epoch(lst))
            log.info("supplied: %s accepted — speaks for %d day(s) since %s",
                     doc.path.name, len(owned),
                     min(owned).isoformat() if owned else "-")
    return layer, expected


def _owned_days(layer_docs: dict) -> dict[str, set[date]]:
    out: dict[str, set[date]] = defaultdict(set)
    for row in layer_docs.values():
        payload = json.loads(row[-1])
        if payload.get("status") == "accepted":
            for a, b in payload.get("owned") or []:
                out[row[2]].update(stitch.days_between(date.fromisoformat(a),
                                                       date.fromisoformat(b)))
    return out


# ---- applying it ------------------------------------------------------------------

def _trim(conn, views: dict[str, stitch.LiveView]) -> int:
    """A1: drop supplied rows the live history has overtaken — postings on a
    day live now certifies or of which live now sees something different
    than it did when they were stitched, and balances on a day live now
    certifies. Always safe (the next stitch re-derives whatever is still
    missing), and what hands days over when a deeper download reaches them."""
    doomed = set()
    for iban, posted_at, payload in conn.execute(
            "SELECT account_external_id, posted_at, payload FROM transactions "
            "WHERE source=?", (SOURCE_SUPPLIED,)).fetchall():
        view = _view(views, iban)
        day = _day(posted_at)
        if view.certifies(day) or (
                json.loads(payload).get("live_seen", {}) != _live_seen(view, day)):
            doomed.add((iban, posted_at))
    removed = sum(conn.execute(
        "DELETE FROM transactions WHERE source=? AND account_external_id=? "
        "AND posted_at=?", (SOURCE_SUPPLIED, iban, posted_at)).rowcount
        for iban, posted_at in doomed)
    for iban, balance_date in conn.execute(
            "SELECT account_external_id, balance_date FROM daily_balances "
            "WHERE source=?", (SOURCE_SUPPLIED,)).fetchall():
        view = _view(views, iban)
        day = _day(balance_date)
        if day in view.balance_certified or view.certifies(day):
            removed += conn.execute(
                "DELETE FROM daily_balances WHERE source=? AND "
                "account_external_id=? AND balance_date=?",
                (SOURCE_SUPPLIED, iban, balance_date)).rowcount
    return removed


def _write_layer(conn, layer: _Layer) -> None:
    conn.execute("DELETE FROM transactions WHERE source=?", (SOURCE_SUPPLIED,))
    conn.execute("DELETE FROM daily_balances WHERE source=?", (SOURCE_SUPPLIED,))
    conn.execute("DELETE FROM documents WHERE doc_kind=?", (DOC_KIND_LISTING,))
    silver.upsert_rows(conn, "transactions", _TXN_COLUMNS,
                       layer.txns.values(), replace=False)
    # REPLACE: a listing's balance takes over a live balance live does not
    # certify (the download's own day); certified ones never reach the layer.
    conn.executemany(
        "INSERT OR REPLACE INTO daily_balances (account_external_id, "
        "balance_date, balance, snapshot_at, source) VALUES (?,?,?,?,?)",
        [(iban, day, bal, snap, SOURCE_SUPPLIED)
         for (iban, day), (bal, snap) in layer.balances.items()])
    silver.upsert_rows(conn, "documents", silver.DOCUMENT_COLUMNS,
                       layer.docs.values(), replace=False)


def _check_one_truth_per_day(conn, views: dict[str, stitch.LiveView],
                             expected: dict) -> None:
    """A5, on what was just written: supplied postings sit only on days live
    does not certify, and there, with what live saw, add up to exactly the
    postings of the listing that speaks for the day."""
    got: dict[tuple[str, date], Counter] = defaultdict(Counter)
    for iban, posted_at, amount in conn.execute(
            "SELECT account_external_id, posted_at, amount FROM transactions "
            "WHERE source=?", (SOURCE_SUPPLIED,)):
        got[(iban, _day(posted_at))][_cents(amount)] += 1
    for (iban, day), supplied in got.items():
        view = _view(views, iban)
        if view.certifies(day):
            raise RuntimeError(f"supplied postings on {day}, a day the live "
                               f"history certifies; refusing to commit")
        if supplied + view.postings_on(day) != expected.get((iban, day)):
            raise RuntimeError(f"live and supplied postings on {day} do not add "
                               f"up to the listing's; refusing to commit")


def _stamp(conn, supplied_dir: Path) -> None:
    """Advance the load clock gold watches (`MAX(dump_runs.snapshot_at)`) when
    silver changed without a new download to do it. One past the latest
    stamp, never the wall clock: a download still in flight carries an
    earlier slug and must still move the clock when it lands."""
    latest = conn.execute("SELECT MAX(snapshot_at) FROM dump_runs").fetchone()[0]
    conn.execute(
        "INSERT INTO dump_runs (snapshot_at, silver_schema_version, run_dir) "
        "VALUES (?,?,?)",
        ((latest or 0) + 1, silver.current_schema_version(conn), str(supplied_dir)))


def _apply(conn, docs: list[_Doc], views, contributors: set[str]) -> bool:
    """Stitch, compare with the stored layer, and write it when it changed and
    the guards allow. Returns whether it wrote."""
    new, expected = _new_layer(docs, views)
    stored = _stored_layer(conn)
    if new == stored:                                       # A2
        return False
    # A3: with every earlier contributor still present, a stitch that
    # covers fewer days is a regression (a parser, poppler or gate change),
    # not a decision — keep the stored layer until it is fixed, or until a
    # file is removed or `load --force` rebuilds.
    if contributors and contributors <= {d.sha for d in docs}:
        before, after = _owned_days(stored.docs), _owned_days(new.docs)
        lost = sorted(d for iban, days in before.items()
                      for d in days - after.get(iban, set())
                      - _view(views, iban).certified)
        if lost:
            log.error("supplied: the stitch would drop %d day(s) that listings "
                      "still present covered before (first: %s); keeping the "
                      "stored supplied layer", len(lost), lost[0])
            return False
    _write_layer(conn, new)
    _check_one_truth_per_day(conn, views, expected)
    log.info("supplied: %d listing(s) stitched — %d posting(s), %d balance "
             "day(s)", sum(1 for r in new.docs.values() if _accepted(r)),
             len(new.txns), len(new.balances))
    return True


def load_supplied(conn: sqlite3.Connection, bronze_dir: Path,
                  supplied_dir: Path, *, live_changed: bool,
                  extract=None) -> bool:
    """Stitch the listings in `supplied_dir` into silver. Returns True when
    silver changed. `live_changed` says whether this load ingested a download
    (which already moved gold's load clock)."""
    extract = extract or pdftotext.layout_text
    with silver.transaction(conn):
        _backfill_windows(conn, bronze_dir)
    windows = live_windows(conn)
    stored = _stored_layer(conn)
    ibans = {r[0] for r in conn.execute(
        "SELECT DISTINCT account_external_id FROM accounts")}
    ibans |= {row[1] for row in stored.txns.values()}
    ibans |= {iban for iban, _ in stored.balances}
    views = {iban: _live_view(conn, iban, windows) for iban in ibans}

    # A1 runs first and commits on its own, so whatever happens below, no
    # supplied row outlives the live history's claim on its day.
    with silver.transaction(conn):
        trimmed = _trim(conn, views)
        if trimmed and not live_changed:
            _stamp(conn, supplied_dir)

    if not supplied_dir.is_dir():
        if stored.docs:
            # An absent directory is ambiguous (never created, not mounted);
            # an empty one is an explicit statement and clears the layer.
            log.warning("supplied: %s is missing but silver holds %d supplied "
                        "listing(s); keeping them", supplied_dir, len(stored.docs))
        else:
            log.debug("supplied: %s not a directory; nothing to stitch",
                      supplied_dir)
        return trimmed > 0
    files = sorted(p for p in supplied_dir.iterdir()
                   if p.is_file() and p.suffix.lower() == ".pdf")
    contributors = {sha for sha, row in stored.docs.items() if _accepted(row)}
    docs = _read_docs(files, contributors, conn, extract)
    if docs is None:
        return trimmed > 0

    with silver.transaction(conn):
        wrote = _apply(conn, docs, views, contributors)
        if wrote and not live_changed and not trimmed:
            _stamp(conn, supplied_dir)
    return wrote or trimmed > 0


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (holds the <UTC-ts>/ run dirs).")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite path. Default: <bronze-dir>/raiffeisen_at.db.")
    p.add_argument("--supplied-dir", type=Path, default=None,
                   help="Transaction listings supplied by the bank, stitched "
                        "into silver on every load (DESIGN.md §I). Default: "
                        f"<bronze-dir>/{SUPPLIED_DIRNAME}.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)

    db_path = args.silver_db or (args.bronze_dir / "raiffeisen_at.db")
    supplied_dir = args.supplied_dir or (args.bronze_dir / SUPPLIED_DIRNAME)
    if args.force:
        silver.reset(db_path)
    conn = silver.open_db(db_path)
    try:
        silver.apply_migrations(conn, MIGRATIONS_DIR)
        loaded = 0
        for run_dir in bronze.iter_run_dirs(args.bronze_dir):
            with silver.transaction(conn):          # one atomic load per run
                if load_run(conn, run_dir):
                    loaded += 1
        log.info("done: %d run(s) ingested into %s", loaded, db_path)
        load_supplied(conn, args.bronze_dir, supplied_dir,
                      live_changed=loaded > 0)
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
