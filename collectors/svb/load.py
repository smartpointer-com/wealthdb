#!/usr/bin/env python3
"""Build the svb silver DBs — one per statement family — from the archive.

Brokerage, deposit and mortgage accounts that predate the live collectors —
STATIC historical data, so this is a one-shot builder rather than a recurring
docker collector. It parses the statement PDFs with :mod:`pdf_parsers_svbwa`
and :mod:`pdf_parsers_svbdep` into silver SQLites that use the **fidelity-web**
schema, so the existing fidelity gold adapter projects them — but under
SEPARATE source ids (``svb``, ``svb-deposit``, ``svb-mortgage``), one per
statement family. Keeping them apart from fidelity is load-bearing: the gold
history macros carry positions forward per *source*, so folding these
staggered-date accounts into ``fidelity-web`` would let unrelated fidelity
snapshots supersede and drop them. Keeping the families apart from each other
is load-bearing for a second reason — see :data:`_SILVER_BY_FAMILY`. See
DESIGN.md.

The archive is hand-filed, so discovery is recursive and the folder layout
carries no meaning. Three document families share this layout, and all three are
loaded, each into its own silver DB: the brokerage statements have a text layer
and are read directly; the deposit and mortgage statements have none, so they
are rastered and OCRed by :mod:`pdf_parsers_svbdep` first. Every PDF is
classified from its own text before parsing, and the build summary prints a
per-family census — an uncounted skip is how a statement goes missing unnoticed.

Zero policy: a $0 month is recorded only where the statement STATES $0 (its
``TOTAL VALUE OF YOUR PORTFOLIO`` / ``ENDING VALUE`` line). An empty holdings
table on its own carries the account forward instead, because a future parse
failure would otherwise read as a real zero. Closure is never inferred: an
account can state $0 one month and a residual the next (a late dividend
landing), so the series ends where the statements end.

A trade reaches silver carrying the key of the holding its statements' own
arithmetic proves it moved (:mod:`instrument_links`); a row the arithmetic
cannot settle keeps none and states what it was looked up by instead.

Runs on the host, not in docker: stdlib sqlite3 plus the extraction stacks and
workbook reader pinned in requirements.txt. Idempotent and
reproducible-from-bronze: re-running against the same bronze dir converges.

Parsing the statement PDFs dominates the run and is CPU-bound, so it is fanned
out across a process pool and memoised in a persistent sidecar cache keyed by
(statement sha256, parser-logic fingerprint, signature set) — the fingerprint
folds in both parsers' import closure and the versions of both extraction
stacks (:data:`_EXTRACTOR_DISTS`), the per-platform OCR recogniser included, so
a cache does not move between platforms. Since the bronze is a static,
closed-account archive, a warm run replays every parse from the sidecar and
re-emits byte-identical silver. See :func:`parse_statements`.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import sqlite3
import sys
from collections import Counter
from concurrent.futures import ProcessPoolExecutor
from datetime import date, datetime, timezone
from itertools import repeat
from pathlib import Path

from collectorkit import cli, silver, srcfp

import derived_marks
import instrument_links
import pdf_parsers_svbdep
import pdf_parsers_svbwa

log = logging.getLogger("svb")

_SYNTHETIC_PORTFOLIO = "SVB-Sleeves"
_SYNTHETIC_KIND = "other"  # gold default → taxable_personal; config sets the real wrapper

# Description of the row that records a statement's STATED $0 total. It is a
# real observation carrying the statement's own sha, not a marker: the account
# held nothing that month, which a later statement may reverse.
_ZERO_DESC = "NO POSITIONS"
_SIGNATURE_SIDECAR = "signature.txt"
_DERIVED_MARKS_FILE = "derived-marks.xlsx"
_PARSE_CACHE_FILE = "parse-cache.json"
_PARSE_CACHE_SCHEMA = 1  # bump whenever the sidecar's on-disk layout changes
_MIGRATIONS = (
    "0001_initial.sql",
    "0002_currency_asset_class_core_position.sql",
    "0003_management_style.sql",
    "0004_historical_position_snapshots.sql",
)


def ts_from_iso(d: str) -> int:
    """ISO ``YYYY-MM-DD`` → Unix seconds at midnight UTC."""
    return int(
        datetime.strptime(d, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp()
    )


def read_signatures(bronze_dir: Path,
                    override: list[str] | None) -> tuple[str, ...]:
    """The page-1 substrings a statement may be signed with.

    A statement is titled by the registration its account is held under, and one
    archive can span several of them, so the guard accepts a SET: one
    blank-and-comment-stripped line of ``signature.txt`` per registration, or
    one ``--statement-signature`` per registration. A statement matching none of
    them is refused, which is the point: a misfiled PDF must not load. Empty
    means no guard is configured.
    """
    if override:
        return tuple(override)
    sidecar = bronze_dir / _SIGNATURE_SIDECAR
    if not sidecar.is_file():
        return ()
    return tuple(
        s for s in (line.strip()
                    for line in sidecar.read_text(encoding="utf-8").splitlines())
        if s and not s.startswith("#")
    )


def apply_migrations(conn: sqlite3.Connection, migrations_dir: Path) -> None:
    for name in _MIGRATIONS:
        sql = (migrations_dir / name).read_text(encoding="utf-8")
        conn.executescript(sql)


def holdings_known(parsed: dict, acct: dict) -> bool:
    """Whether the statement says what the account held: it prints a
    holdings table, or states a $0 total in place of one. Otherwise the
    table was lost and the holdings are unknown, which is not the same
    as empty."""
    return bool(acct.get("holdings")) or parsed.get("stated_total") == 0


def insert_statement(conn: sqlite3.Connection, parsed: dict, sha: str) -> int:
    """Insert one historical row per holding, or a single $0 row for a
    statement that STATES a zero portfolio total.

    Accounts with holdings overwrite any prior row at the same (as_of,
    account, description). An account with no holdings and no stated zero is
    skipped, so it carries its last real value forward — a parse that lost the
    table must not read as a real unwind.
    """
    period_end = parsed.get("period_end")
    if not period_end:
        return 0
    as_of = ts_from_iso(period_end)
    inserted = 0
    for acct in parsed.get("accounts", []):
        aid = acct.get("account_external_id")
        holdings = acct.get("holdings", [])
        if not aid:
            continue
        if not holdings:
            if holdings_known(parsed, acct):
                conn.execute(
                    "INSERT OR REPLACE INTO historical_position_snapshots ("
                    "as_of_date, account_external_id, description, instrument_key, "
                    "quantity, price, market_value, percent_of_total, currency, "
                    "source_sha256, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        as_of, aid, _ZERO_DESC, None, None, None, 0.0, None, "USD",
                        sha, json.dumps({"description": _ZERO_DESC,
                                         "market_value": 0.0,
                                         "source": "stated-portfolio-total"},
                                        separators=(",", ":")),
                    ),
                )
                inserted += 1
            continue
        # The PK is (as_of, account, description), but option legs share a
        # description (the strike lives on a separate line), so a naive insert
        # collapses them and drops the short legs. Disambiguate a colliding
        # description with its unique instrument key (the OCC symbol), falling
        # back to an index, so every leg survives.
        seen: set[str] = set()
        for h in holdings:
            desc = (h.get("description") or "").strip()
            if not desc:
                continue
            key = h.get("instrument_key")
            uniq = desc
            if uniq in seen:
                uniq = f"{desc} [{key}]" if key else desc
                i = 2
                while uniq in seen:
                    uniq = f"{desc} [{key or ''}#{i}]"
                    i += 1
            seen.add(uniq)
            conn.execute(
                "INSERT OR REPLACE INTO historical_position_snapshots ("
                "as_of_date, account_external_id, description, instrument_key, "
                "quantity, price, market_value, percent_of_total, currency, "
                "source_sha256, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                (
                    as_of, aid, uniq, key,
                    h.get("quantity"), h.get("price"), h.get("market_value"),
                    None, "USD", sha, json.dumps(h, separators=(",", ":")),
                ),
            )
            inserted += 1
    return inserted


# ============================================================
# Activity → transactions
# ============================================================
#
# The silver is read by the Fidelity gold adapter, so `transactions.kind` has
# to be a verb that adapter's kindFor() understands. These are the SVB
# Transaction-column verbs translated into it.
#
# Four translations are deliberately NOT the obvious one, because the canonical
# sign for the obvious kind is pinned and would invert the row:
#   * ADJ NON-RESIDENT TAX is a withholding REVERSAL and is always a credit;
#     TAX pins the sign negative, which would book a refund as a charge.
#   * DIVIDEND ADJUSTMENT is a dividend CLAWBACK and is always a debit;
#     DIVIDEND pins the sign positive.
# These map to ADJUSTMENT, which the adapter keeps source-signed; so do the two
# trade cancellations, for the reason given at their entries below.
_KIND_BY_VERB = {
    "DIVIDEND RECEIVED": "DIVIDEND",
    "DIVIDEND ADJUSTMENT": "ADJUSTMENT",
    "INTEREST": "INTEREST",
    "MARGIN INTEREST": "INTEREST",
    "RETURN OF CAPITAL": "RETURN_OF_CAPITAL",
    "DISTRIBUTION": "DISTRIBUTION",
    "NON-RESIDENT TAX": "TAX",
    "FOREIGN TAX PAID": "TAX",
    "ADJ NON-RESIDENT TAX": "ADJUSTMENT",
    "FEE PAID": "FEE",
    "ADVISOR FEE DEDUCTED": "ADVISOR",
    "ADJUSTMENT": "ADJUSTMENT",
    "TRANSFERRED TO": "TRANSFER",
    "TRANSFERRED FROM": "TRANSFER",
    "INTER BROKER DELIVER": "TRANSFER",
    "INTER BROKER RECEIVE": "TRANSFER",
    "INTER BROKER DEBIT": "TRANSFER",
    "INTER BROKER CREDIT": "TRANSFER",
    # An in-kind deposit of securities. Source-signed like the other position
    # transfers: whether it is external capital or the far leg of a move
    # between tracked accounts is gold's to decide, not the collector's.
    "RECEIVED FROM YOU": "TRANSFER",
    "JOURNALED": "JOURNAL",
    # Direction lives in the sign alone — the verb pairs carry it too, but the
    # adapter reads the sign, so an inverted parse would reverse the flow
    # rather than fail.
    "WIRE TRANS TO BANK": "WIRE",
    "WIRE TRANS FROM BANK": "WIRE",
    "DIRECT DEBIT": "DIRECT_DEBIT",
    "DIRECT DEPOSIT": "DIRECT_DEPOSIT",
    "MERGER": "MERGER",
    "TENDERED": "TENDER",
    "EXPIRED": "EXPIRATION",
    "IN LIEU OF FRX SHARE": "CASH_IN_LIEU",
    "REINVESTMENT": "REINVESTMENT",
    "YOU BOUGHT": "BUY",
    "BOUGHT": "BUY",
    "YOU SOLD": "SELL",
    "SOLD": "SELL",
    "REDEEMED": "REDEMPTION",
    # Trade cancellations print in the reversed direction — a cancelled buy is
    # a credit — so they stay source-signed rather than taking BUY/SELL's
    # pinned one.
    "CANCELLED BUY": "ADJUSTMENT",
    "CANCELLED SELL": "ADJUSTMENT",
    # A brokerage statement's checking sub-section. The number stays in the
    # description, as the deposit ledger's cleared checks do — gold's
    # `check_number` column is filled by no svb path today.
    "CHECK PAID": "WITHDRAWAL",
    # The deposit ledger has no Transaction column. A row's direction is the
    # column its figure lands in, and its KIND is what the statement's own
    # wording says it is — a credit the summary counts as interest, a debit it
    # counts as a service charge — which the parser resolves into the verb and
    # only when all four buckets reconcile against that summary. The two below
    # are what is left once those are taken out, so there is no sign to read.
    "DEPOSIT": "DEPOSIT",
    "WITHDRAWAL": "WITHDRAWAL",
}

# Core-fund rows are the account's cash ↔ money-market sweep, which the
# adapter has its own source-signed pair of kinds for. They are the same
# YOU BOUGHT / YOU SOLD / REINVESTMENT verbs the blotter uses, so the section
# is what tells them apart, not the verb.
_CORE_FUND_KINDS = {"BUY": "CASH_SWEEP_IN", "REINVESTMENT": "CASH_SWEEP_IN",
                    "SELL": "CASH_SWEEP_OUT"}

# A row whose Transaction column was blank or held no known verb. It is not
# booked: without the verb neither the movement's kind nor the boundary
# between it and the description is known, and a row booked under a guessed
# kind is worse than one the build reports by name. The reconciliation still
# counts it, so the section it sits in continues to add up.
_UNKNOWN_KIND = "UNKNOWN"

# Activity sections that reach `transactions` — the deposit ledger, the
# brokerage statements' cash-movement sections, and the trade blotter. The
# blotter books for the reason every other broker adapter books its trades:
# a sale here and the purchase it funded at another custodian are one round
# trip, and omitting this half leaves the other reading as capital from
# nowhere. Both kinds are internal to the returns engine (`BankExternal`
# omits them), so booking them moves no net flow.
#
# The two PENDING sections stay out: they are projections that settle into a
# later statement, which books them again.
_BOOKED_SECTIONS = frozenset({
    pdf_parsers_svbdep.SECTION_LEDGER,
    pdf_parsers_svbwa.SECTION_ADDITIONS,
    pdf_parsers_svbwa.SECTION_INCOME,
    pdf_parsers_svbwa.SECTION_TAXES_FEES,
    pdf_parsers_svbwa.SECTION_MISC,
    pdf_parsers_svbwa.SECTION_CORE_FUND,
    pdf_parsers_svbwa.SECTION_OTHER,
    pdf_parsers_svbwa.SECTION_TRADES,
})


def unreadable_sections(parsed: dict) -> list[str]:
    """Return one message per account section the parser refused.

    A section is refused when the statement's own arithmetic did not close —
    which on the OCRed families is the only thing that can tell a misread digit
    from a real one. It reaches silver as nothing at all rather than as a
    plausible wrong number, so the account carries its last real value forward
    and the build says which statement to look at.
    """
    return [f"{acct.get('account_external_id') or '?'}: {acct['_error']}"
            for acct in parsed.get("accounts", []) if acct.get("_error")]


def activity_kind(row: dict) -> str:
    """Translate one activity row's Transaction verb into the silver `kind`
    the Fidelity gold adapter reads."""
    kind = _KIND_BY_VERB.get(row.get("verb") or "", _UNKNOWN_KIND)
    if row.get("section") == pdf_parsers_svbwa.SECTION_CORE_FUND:
        kind = _CORE_FUND_KINDS.get(kind, kind)
    return kind


def activity_id(sha: str, account: str, row: dict) -> str:
    """Deterministic primary key for one activity row.

    Keyed on the statement's sha, the account the row was printed under, and
    the row's own date, verb, amount and ordinal. The account and the ordinal
    are both required, not belt-and-braces: a statement can print two same-day
    transfers of the same shape from the same counterparty, which only the
    ordinal separates, and one document can carry several account sections,
    each numbering its rows from zero, which only the account separates. Every
    part is a parsed value rather than a rendered one, so a warm parse-cache
    replay reproduces the id byte for byte.
    """
    amount = row.get("amount")
    parts = (
        sha, account, row.get("date") or "", row.get("verb") or "",
        "" if amount is None else f"{amount:.6f}", str(row.get("ordinal")),
    )
    h = hashlib.sha256("\x00".join(parts).encode("utf-8"))
    return "svb-" + h.hexdigest()[:28]


def booked_activity(parsed: dict):
    """Yield ``(account, row, kind)`` for every settled activity row the
    statement books — the one filter both the inserts and the instrument
    links read, so the rows linked are exactly the rows written. ``kind`` is
    :data:`_UNKNOWN_KIND` for a row whose verb is not recognised, which is
    reported rather than booked."""
    for acct in parsed.get("accounts", []):
        aid = acct.get("account_external_id")
        if not aid:
            continue
        for row in acct.get("activity", []):
            if row.get("section") not in _BOOKED_SECTIONS:
                continue
            if row.get("amount") is None or not row.get("date"):
                continue
            yield aid, row, activity_kind(row)


def _moves_a_position(row: dict, kind: str) -> bool:
    """Whether the row is one the instrument links try: a booked row that
    moves a quantity of a security — a trade, a corporate action, an in-kind
    transfer. The core-fund sweeps are the exception. They move the account's
    cash in and out of its money fund, name no investment, and would put the
    one large equation in the proof for nothing a report reads."""
    return (kind != _UNKNOWN_KIND and row.get("quantity") is not None
            and row.get("section") != pdf_parsers_svbwa.SECTION_CORE_FUND)


def _activity_ref(sha: str, account: str, row: dict) -> tuple:
    return sha, account, row.get("ordinal")


def link_statements(results: list[tuple[str, dict]]) -> instrument_links.Links:
    """Link each position-moving row to the holding its statements prove it
    moved (see :mod:`instrument_links`), over every parsed statement.

    A statement opens empty only where it STATES a $0.00 beginning value, the
    real-zero rule applied at the other end of the period."""
    statements = []
    for sha, parsed in results:
        if not parsed.get("period_end"):
            continue
        start = parsed.get("period_start")
        moves = {}
        for aid, row, kind in booked_activity(parsed):
            if _moves_a_position(row, kind):
                moves.setdefault(aid, []).append(instrument_links.Movement(
                    ref=_activity_ref(sha, aid, row),
                    name=row.get("description") or "",
                    quantity=row["quantity"],
                    stated_key=row.get("stated_key")))
        for acct in parsed.get("accounts", []):
            aid = acct.get("account_external_id")
            if not aid:
                continue
            holdings = None
            if holdings_known(parsed, acct):
                holdings = tuple(
                    instrument_links.Holding(h["instrument_key"],
                                             h.get("description") or "",
                                             h["quantity"])
                    for h in acct.get("holdings", [])
                    if h.get("instrument_key") and h.get("quantity") is not None)
            statements.append(instrument_links.Statement(
                account=aid,
                start=date.fromisoformat(start) if start else None,
                end=date.fromisoformat(parsed["period_end"]),
                holdings=holdings,
                opens_empty=parsed.get("stated_opening") == 0,
                movements=tuple(moves.get(aid, ()))))
    return instrument_links.link(statements)


def insert_transactions(conn: sqlite3.Connection, parsed: dict, sha: str,
                        links: instrument_links.Links) -> tuple[int, int]:
    """Insert the statement's settled activity rows. Returns
    ``(inserted, unknown-verb count)``.

    `instrument_key` is the holding key ``links`` proved the row moved. The
    Activity region prints a security's kerned NAME, never the key the
    Holdings rows carry, so the key comes from the statements' arithmetic —
    see :mod:`instrument_links` — and a row the arithmetic cannot settle keeps
    none. Such a row states what it was looked up by as `InstrumentHint`, the
    token a `transaction_instruments` config entry closes it by.

    `Action` is the row's narrative, which gold categorises the row by and
    which `wealthdb resolve-symbols` looks a row with no instrument up by.
    The name is also kept unprefixed under `Description`, and `Section` says
    whether that name is a security at all (income / taxes / corporate
    actions) or a counterparty account (additions and withdrawals).

    `price` and `settlement_date` stay NULL — the layout prints neither
    column, and its single date column is the settlement date in some sections
    and the effective date in others, so there is no second date to record.
    """
    inserted = unknown = 0
    for aid, row, kind in booked_activity(parsed):
        if kind == _UNKNOWN_KIND:
            unknown += 1
            continue
        verb = row.get("verb") or ""
        description = row.get("description") or ""
        payload = {
            # `Action` is the narrative the gold adapter categorises a row
            # by; it reads the verb and the counterparty together, exactly
            # as the live Fidelity feed's own Action column does.
            "Action": f"{verb} {description}".strip(),
            "Description": description,
            "Transaction": verb,
            "Section": row.get("section"),
            "AccountType": row.get("account_type"),
        }
        ref = _activity_ref(sha, aid, row)
        unlinked = links.unlinked.get(ref)
        if unlinked is not None:
            reason, payload["InstrumentHint"] = unlinked
            log.debug("instrument not linked: %s %s %s %s — %s",
                      aid, row["date"], verb, payload["InstrumentHint"], reason)
        conn.execute(
            "INSERT OR REPLACE INTO transactions ("
            "activity_id, timestamp, account_external_id, kind, "
            "instrument_key, quantity, price, amount, settlement_date, "
            "currency, source_sha256, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                activity_id(sha, aid, row), ts_from_iso(row["date"]), aid,
                kind, links.keys.get(ref), row.get("quantity"), None,
                row.get("amount"), None, "USD", sha,
                json.dumps(payload, separators=(",", ":")),
            ),
        )
        inserted += 1
    return inserted, unknown


def reconcile_activity(parsed: dict) -> list[str]:
    """Return one message per Activity section whose parsed rows do not sum to
    the total the statement states for it.

    Each section strikes its own total, so this is the statement checking the
    parse against itself — the only independent arithmetic these documents
    offer. Reported, not fatal: a stale total on a re-issued statement must not
    stop the rest of the archive loading.
    """
    out = []
    for acct in parsed.get("accounts", []):
        sums: dict[str, float] = {}
        for row in acct.get("activity", []):
            if row.get("amount") is not None:
                section = row.get("section")
                sums[section] = sums.get(section, 0.0) + row["amount"]
        for section, stated in (acct.get("activity_totals") or {}).items():
            got = round(sums.get(section, 0.0), 2)
            if abs(got - stated) >= 0.01:
                out.append(f"{acct.get('account_external_id')} {section}: "
                           f"parsed {got:.2f} vs stated {stated:.2f}")
    return out


def insert_derived_marks(conn: sqlite3.Connection, workbook: Path | None) -> int:
    """Fill the archive's interior gaps with the advisor workbook's month-end
    values, and return how many were written.

    Runs after every statement is in, so "where the archive has nothing" means
    the finished archive, not whatever had been read so far. A statement that
    later joins the archive takes its month back with no other change.

    The workbook names each sheet after a brokerage account serial
    (``derived_marks._SHEET_RE``), so it can only yield ``SV[MRT]-NNNNNN``
    ids; the deposit and mortgage families number their accounts as bare
    10-digit ids. The brokerage DB is therefore the only one with gaps this
    can fill — running it against the other two would match nothing.
    """
    marks = derived_marks.read_workbook(workbook)
    if not marks:
        return 0
    covered: dict[str, set[tuple[int, int]]] = {}
    for ts, account in conn.execute(
            "SELECT DISTINCT as_of_date, account_external_id "
            "FROM historical_position_snapshots"):
        when = datetime.fromtimestamp(ts, timezone.utc).date()
        covered.setdefault(account, set()).add((when.year, when.month))
    fill = derived_marks.gaps_to_fill(marks, covered)
    for mark in fill:
        conn.execute(
            "INSERT OR REPLACE INTO historical_position_snapshots ("
            "as_of_date, account_external_id, description, instrument_key, "
            "quantity, price, market_value, percent_of_total, currency, "
            "source_sha256, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (
                ts_from_iso(mark.as_of), mark.account_external_id,
                derived_marks.DERIVED_DESC, None, None, None,
                mark.market_value, None, "USD", derived_marks.DERIVED_SHA,
                json.dumps({"description": derived_marks.DERIVED_DESC,
                            "market_value": mark.market_value,
                            "source": derived_marks.DERIVED_SHA},
                           separators=(",", ":")),
            ),
        )
        log.info("derived mark: %s %s %.2f", mark.account_external_id,
                 mark.as_of, mark.market_value)
    return len(fill)


def synthesize_masters(conn: sqlite3.Connection) -> int:
    """One synthetic accounts + portfolios master per account (neutral kind, so
    the gold adapter defaults to taxable_personal and config overrides set the
    precise wrapper/management). Mirrors fidelity-web's
    _synthesize_missing_account_masters but with SVB-neutral classifiers."""
    rows = conn.execute(
        "SELECT account_external_id, MAX(as_of_date) FROM historical_position_snapshots "
        "GROUP BY account_external_id"
    ).fetchall()
    payload = json.dumps({"source": "svb-sleeve-synthetic"})
    n = 0
    for aid, latest in rows:
        conn.execute(
            "INSERT OR IGNORE INTO portfolios (snapshot_at, portfolio_external_id, "
            "kind, payload) VALUES (?,?,?,?)",
            (latest, _SYNTHETIC_PORTFOLIO, _SYNTHETIC_KIND, payload),
        )
        cur = conn.execute(
            "INSERT OR IGNORE INTO accounts (snapshot_at, account_external_id, "
            "portfolio_external_id, nickname, payload, management_style) "
            "VALUES (?,?,?,?,?,?)",
            (latest, aid, _SYNTHETIC_PORTFOLIO, None, payload, None),
        )
        n += cur.rowcount
    return n


def mark_dump_run(conn: sqlite3.Connection) -> None:
    """Record one synthetic dump_runs row at the latest as_of. The fidelity
    gold adapter keys its change-trigger on dump_runs/transactions, so a build
    whose positions are all historical needs this marker for `wealthdb load` to
    pick it up."""
    row = conn.execute(
        "SELECT MAX(as_of_date) FROM historical_position_snapshots").fetchone()
    if row is None or row[0] is None:
        return
    # Only the NOT-NULL columns; the *_present flags describe what a download's
    # dump dir held, and this build has no dump dir, so they stay 0.
    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version, "
        "run_dir, mode) VALUES (?,?,?,?)",
        (row[0], 4, "svb-sleeves-build", "historical"),
    )


# ============================================================
# Statement parsing: process pool + persistent parse cache
# ============================================================
#
# The cached value is exactly the dict the parser returns and insert_statement
# consumes it identically, so cached and freshly-parsed silver are byte-for-byte
# identical. Statement sha256 is computed in the parent (statements are tens of
# KB, so hashing is negligible) so a cache HIT can skip the parse entirely — a
# warm run therefore never spawns a pool.


def _parse_statement(path: str, signatures: tuple[str, ...]) -> dict:
    """Parse one statement PDF into the parser's structured dict.

    The text layer decides which parser reads it: a brokerage statement has
    one, and the deposit and mortgage families have none at all, so a document
    the first pass reports as image-only goes round again through OCR. Probing
    for a text layer costs a cheap open and saves a whole OCR pass on every
    document that has one.

    Module-level (not a closure) so it pickles for
    :class:`~concurrent.futures.ProcessPoolExecutor` under the ``spawn`` start
    method used on macOS.
    """
    parsed = pdf_parsers_svbwa.parse_svbwa_statement_pdf(
        path, expected_signatures=signatures)
    if parsed.get("family") == pdf_parsers_svbwa.FAMILY_IMAGE_ONLY:
        return pdf_parsers_svbdep.parse_svbdep_statement_pdf(
            path, expected_signatures=signatures)
    return parsed


def _default_cache_dir() -> Path:
    """XDG cache location for the parse sidecar.

    Defaults to ``$XDG_CACHE_HOME/wealthdb/svb`` (``~/.cache/wealthdb/svb`` when
    the variable is unset). The sidecar holds parsed statement data — derived
    PII — so it stays outside the repo, exactly like the silver DB, and never
    under a secrets dir.
    """
    base = os.environ.get("XDG_CACHE_HOME") or str(Path.home() / ".cache")
    return Path(base) / "wealthdb" / "svb"


# Both extraction stacks: pdfplumber reads the brokerage statements' text
# layer, pypdfium2 plus a recogniser rasters and OCRs the families that have
# none. A version change in any of them can shift what a parse produces for
# identical input bytes, so all of them belong in the cache key.
#
# Both recognisers are listed although only one is installed on a given
# platform — which is the point. srcfp records an absent one as unavailable,
# so a macOS key and a Linux key differ, and moving a cache between them
# re-parses instead of replaying text the other engine produced.
_EXTRACTOR_DISTS = ("pdfplumber", "pdfminer.six", "pypdfium2",
                    "ocrmac", "rapidocr")


def _parser_logic_fingerprint() -> str:
    """Fingerprint of the parsing logic, folded into every cache key so a code
    edit to either parser (or anything in their import closure) or an upgrade
    to either extraction stack auto-invalidates cached parses, while a comment
    / formatting / docstring edit — which can't change a parse — does not. See
    :func:`collectorkit.srcfp.parser_fingerprint`."""
    return srcfp.parser_fingerprint(
        [pdf_parsers_svbwa, pdf_parsers_svbdep], _EXTRACTOR_DISTS)


def _cache_key(file_sha: str, logic_fp: str,
               signatures: tuple[str, ...]) -> str:
    # Opaque key; the statement sha and logic fingerprint are hex (no ':'), and
    # the signature set is last and order-normalised, so the join stays
    # unambiguous and reordering the sidecar's lines does not evict the cache.
    return f"{file_sha}:{logic_fp}:" + "\x00".join(sorted(signatures))


def _load_parse_cache(cache_dir: Path | None) -> dict[str, dict]:
    """Load the sidecar's parsed-dict entries, or ``{}`` when caching is off
    (``cache_dir is None``), the sidecar is absent/corrupt, or its schema is
    from an older layout (in which case it is ignored and rebuilt)."""
    if cache_dir is None:
        return {}
    try:
        blob = json.loads(
            (cache_dir / _PARSE_CACHE_FILE).read_text(encoding="utf-8"))
    except (FileNotFoundError, ValueError):
        return {}
    if blob.get("schema") != _PARSE_CACHE_SCHEMA:
        return {}
    return blob.get("entries", {})


def _save_parse_cache(cache_dir: Path | None, entries: dict[str, dict]) -> None:
    """Persist the parsed-dict entries to the sidecar (atomic rename). No-op
    when caching is off (``cache_dir is None``)."""
    if cache_dir is None:
        return
    cache_dir.mkdir(parents=True, exist_ok=True)
    path = cache_dir / _PARSE_CACHE_FILE
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(
        json.dumps({"schema": _PARSE_CACHE_SCHEMA, "entries": entries},
                   separators=(",", ":")),
        encoding="utf-8")
    tmp.replace(path)


def parse_statements(pdfs: list[Path], shas: list[str], *,
                     signatures: tuple[str, ...], cache_dir: Path | None,
                     max_workers: int | None) -> list[dict]:
    """Return the parsed dict for each PDF in ``pdfs`` order.

    Cache hits (matched against the sidecar's pre-run state only, so that two
    statements with identical content are still each parsed rather than one
    shadowing the other) are replayed directly; misses are parsed — in a process
    pool when more than one needs parsing and ``max_workers`` allows it, else in
    process — and their results folded back into the sidecar.
    """
    logic_fp = _parser_logic_fingerprint()
    cached = _load_parse_cache(cache_dir)

    parsed: list[dict | None] = [None] * len(pdfs)
    misses: list[int] = []
    for i, sha in enumerate(shas):
        hit = cached.get(_cache_key(sha, logic_fp, signatures))
        if hit is not None:
            parsed[i] = hit
        else:
            misses.append(i)

    if misses:
        paths = [str(pdfs[i]) for i in misses]
        workers = (max_workers if max_workers is not None
                   else min(len(paths), os.cpu_count() or 1))
        if workers > 1 and len(paths) > 1:
            with ProcessPoolExecutor(max_workers=workers) as pool:
                outputs = list(pool.map(_parse_statement, paths,
                                        repeat(signatures)))
        else:
            outputs = [_parse_statement(p, signatures) for p in paths]
        fresh = dict(cached)
        for i, out in zip(misses, outputs):
            parsed[i] = out
            if not out.get("_error"):
                fresh[_cache_key(shas[i], logic_fp, signatures)] = out
        _save_parse_cache(cache_dir, fresh)

    return parsed  # type: ignore[return-value]


def discover_statements(bronze_dir: Path) -> list[Path]:
    """Every PDF under the bronze archive, in a deterministic order.

    The archive is hand-filed and the folder layout carries no meaning, so
    discovery is recursive. The order is the POSIX relative path, and it is
    load-bearing rather than cosmetic: inserts replay it serially and
    `INSERT OR REPLACE` is last-writer-wins, so two statements that touch the
    same key must resolve the same way on every run.
    """
    if not bronze_dir.is_dir():
        return []
    pdfs = [p for p in bronze_dir.rglob("*")
            if p.is_file() and p.suffix.lower() == ".pdf"]
    return sorted(pdfs, key=lambda p: p.relative_to(bronze_dir).as_posix())


# One silver DB per statement calendar, each registered in gold under its own
# source id. That separation is load-bearing, not tidiness: gold ends an
# account's series at the first later snapshot of its SOURCE that re-covers
# every account seen alongside it. A month-end brokerage statement and a
# month-end deposit statement share a snapshot date, so in one source each
# family's next statement repeatedly reads as the others' closure and their
# values flicker in and out. Separate ids give each family its own clock.
#
# The file for each sits beside the one `--silver-db` names, which stays the
# brokerage DB so the original id keeps its path. A family the archive does not
# hold gets no file at all — see `build`.
_SILVER_BY_FAMILY = {
    pdf_parsers_svbwa.FAMILY_BROKERAGE: "",
    pdf_parsers_svbdep.FAMILY_DEPOSIT: "-deposit",
    pdf_parsers_svbdep.FAMILY_MORTGAGE: "-mortgage",
}


def silver_paths(silver_db: Path) -> dict[str, Path]:
    """The output path per statement family, keyed as the parsers report it."""
    return {family: silver_db.with_name(f"{silver_db.stem}{suffix}{silver_db.suffix}")
            for family, suffix in _SILVER_BY_FAMILY.items()}


def build(silver_db: Path, bronze_dir: Path, *, signatures: tuple[str, ...],
          migrations_dir: Path, cache_dir: Path | None = None,
          max_workers: int | None = None, workbook: Path | None = None) -> None:
    # Validate the bronze BEFORE touching the existing silver: a mis-pointed
    # --bronze-dir must fail loudly, not silently replace good silver DBs with
    # an empty rebuild (which then zeroes the sources out of gold).
    pdfs = discover_statements(bronze_dir)
    if not pdfs:
        raise SystemExit(
            f"svb load: no statement PDFs in {bronze_dir} — refusing to "
            f"rebuild {silver_db} from an empty bronze. Point --data-dir / "
            f"--bronze-dir at the archive (PDFs live in <data-dir>/bronze/).")
    # Hash then parse every statement (cache replay + process pool) before the
    # existing silver is touched, so a parse crash also leaves the good DBs in
    # place. Inserts still run serially below in sorted-PDF order, preserving the
    # INSERT OR REPLACE last-writer semantics and the per-statement log order.
    shas = [hashlib.sha256(pdf.read_bytes()).hexdigest() for pdf in pdfs]
    results = parse_statements(pdfs, shas, signatures=signatures,
                               cache_dir=cache_dir, max_workers=max_workers)
    # Build a silver only for a family the archive actually holds. An archive
    # holding one family alone would otherwise get an empty DB per absent
    # family sitting beside its own, each indistinguishable from a source whose
    # statements had all been withdrawn.
    # A file that ALREADY exists is still rebuilt even when its family drops
    # out of bronze, so withdrawing a family's statements empties its source in
    # gold rather than leaving the previous load's values standing, and a path
    # some config already names never goes missing.
    present = {res.get("family") for res in results if not res.get("_error")}
    outputs = {family: path for family, path in silver_paths(silver_db).items()
               if family in present or path.exists()}
    for path in outputs.values():
        silver.reset(path)  # full rebuild — reproducible from bronze
    conns = {}
    try:
        for family, path in outputs.items():
            conns[family] = sqlite3.connect(str(path))
            silver.own_only(path)
            apply_migrations(conns[family], migrations_dir)
        # Linked across the whole archive before anything is written: a
        # window reaches back to the account's previous statement.
        links = link_statements(
            [(sha, res) for sha, res in zip(shas, results)
             if not res.get("_error") and res.get("family") in conns])
        log.info("instrument links: %d activity row(s) linked, %d not [%s]",
                 len(links.keys), len(links.unlinked),
                 ", ".join(f"{k}={v}" for k, v in sorted(links.census().items())))
        holdings = txns = unknown_verbs = 0
        census: Counter[str] = Counter()
        for pdf, sha, res in zip(pdfs, shas, results):
            if res.get("_error"):
                log.warning("skip %s: %s", pdf.name, res["_error"])
                census[res["_error"]] += 1
                continue
            family = res.get("family") or pdf_parsers_svbwa.FAMILY_UNKNOWN
            census[family] += 1
            if family not in conns:
                log.info("not parsed (%s): %s", family, pdf.name)
                continue
            conn = conns[family]
            for msg in unreadable_sections(res):
                log.warning("refusing %s — %s", pdf.name, msg)
                census["unreadable-section"] += 1
            n = insert_statement(conn, res, sha)
            holdings += n
            t, u = insert_transactions(conn, res, sha, links)
            txns += t
            unknown_verbs += u
            if n == 0:
                log.info("carry-forward (no holdings, no stated zero): %s",
                         pdf.name)
            if u:
                log.warning("%d activity row(s) in %s carry no recognised "
                            "transaction verb and were not booked", u, pdf.name)
            for msg in reconcile_activity(res):
                log.warning("activity does not reconcile in %s — %s",
                            pdf.name, msg)
        # Brokerage only: the workbook keys its sheets on brokerage serials.
        # An archive with no brokerage statements has no DB to fill and no
        # sheet that could match one.
        brokerage = conns.get(pdf_parsers_svbwa.FAMILY_BROKERAGE)
        derived = insert_derived_marks(brokerage, workbook) if brokerage else 0
        synth = 0
        for conn in conns.values():
            synth += synthesize_masters(conn)
            mark_dump_run(conn)
            conn.commit()
        log.info(
            "svb silver built: %d holdings rows + %d transactions from %d "
            "document(s) [%s], %d derived mark(s), %d unbooked activity row(s), "
            "%d master(s) synthesised across %d silver DB(s)",
            holdings, txns, len(pdfs),
            ", ".join(f"{k}={v}" for k, v in sorted(census.items())),
            derived, unknown_verbs, synth, len(conns),
        )
    finally:
        for conn in conns.values():
            conn.close()


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--silver-db", type=Path, required=True,
                   help="brokerage silver SQLite path; a deposit and a "
                        "mortgage DB sit beside it when the archive holds "
                        "those families")
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="root of the statement archive (searched recursively) "
                        "+ signature.txt")
    p.add_argument("--statement-signature", action="append", default=None,
                   help="page-1 signature substring; repeat to accept any one "
                        "of several registrations (else read the lines of "
                        "signature.txt)")
    p.add_argument("--derived-marks", type=Path, default=None,
                   help="advisor workbook of month-end account values, used "
                        "to fill the brokerage months no statement covers "
                        "(default "
                        "<bronze>/derived-marks.xlsx). Absent means the "
                        "archive stands on its statements alone.")
    p.add_argument("--migrations-dir", type=Path,
                   default=Path(__file__).parent / "migrations")
    p.add_argument("--parse-cache-dir", type=Path, default=_default_cache_dir(),
                   help="directory for the persistent parse cache sidecar "
                        "(default $XDG_CACHE_HOME/wealthdb/svb). Keyed by "
                        "(statement sha256, parser-logic fingerprint, "
                        "signature set), "
                        "so a parser edit or an upgrade to either extraction "
                        "stack auto-invalidates it — the OCR recogniser is "
                        "per-platform and part of the key, so a cache does not "
                        "move between platforms. A warm run replays every parse "
                        "from it. It holds parsed statement data, so it lives "
                        "outside the repo like the silver DB.")
    # svb has no incremental path — build() always deletes and rebuilds —
    # so --force parses but cannot change the outcome.
    cli.add_standard_args(p, verb="load", always_rebuilds=True)
    args = p.parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(levelname)s %(message)s")
    sigs = read_signatures(args.bronze_dir, args.statement_signature)
    if not sigs:
        log.warning("no signature configured (--statement-signature or "
                    "signature.txt); ingesting every PDF unverified")
    workbook = args.derived_marks or (args.bronze_dir / _DERIVED_MARKS_FILE)
    build(args.silver_db, args.bronze_dir, signatures=sigs,
          migrations_dir=args.migrations_dir, cache_dir=args.parse_cache_dir,
          workbook=workbook)
    return 0


if __name__ == "__main__":
    sys.exit(main())
