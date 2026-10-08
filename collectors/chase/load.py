#!/usr/bin/env python3
"""chase silver loader: bronze exports → source-shaped SQLite.

Parses each bronze run dir (see download.py for the layout) into the silver
schema (migrations/): the account roster, the transaction ledger, and the
statement inventory. Idempotent — an already-loaded dump is skipped; the
deposit, statement and document ledgers INSERT OR IGNORE on their stable id,
and the card ledger goes through `_upsert_card_transaction`, which inserts a
new id and otherwise only fills columns the export that first landed the row
could not carry. Either way re-running converges and nothing is removed.

The transaction ledger is built by **joining the two exports** captured per
account (DESIGN.md §E). Both products export a CSV and a QFX, and neither
file alone is sufficient, but they are complementary in different ways:

  * deposit — QFX supplies a clean type/name/memo split, CSV supplies the
    per-row running balance. The two cover the same rows and join on
    (post date, amount); a row seen only in the CSV (a pending item QFX
    omits) is still ingested so nothing is dropped.
  * card — CSV supplies the transaction date, the provider category and the
    5-way `Type`; QFX supplies the un-mangled descriptor (the CSV replaces a
    descriptor's commas with spaces). There is no running balance in either
    file. The two cover the same rows, so they join on (post date, amount).

Transaction ids are derived from CONTENT plus an occurrence index for BOTH
products (`_txn_id`, the index counted per account by `_id_minter`), never
from the QFX `<FITID>`, which is carried as `payload.fitid` for traceability.
Each format is fetched separately and can fail on its own, so keying on a
field only one of them carries would make a row's identity depend on which
file a run happened to land (see the id section).

The export only reaches back Chase's 24-month cap, so **statement PDFs supply
the older transactions** (`statement_parser`). A post-pass, after the export
runs are loaded, imports only the transactions BEFORE that account's export
seam — `MIN(posted_at)` over its export-sourced rows, so the two sources
never overlap and the seam never moves as statements are added. The seam is
PER ACCOUNT because export depths differ between products: a deposit
account's seam sits wherever its onboarding backfill reached, while a card
export caps at 24 months, and one global minimum would let the deeper
account's seam suppress the shallower one's statement history. A combined
statement carries one segment per product; the account's segment is picked
by balance chaining (anchored on the export's running balances, then
ending == next month's beginning walking backwards), and only a segment
whose beginning + Σ == ending is imported (a mis-parse or ambiguous chain is
skipped, never guessed at). That pass is deposit-shaped; card statements have
their own, `load_card_statements`, because a card statement is a different
document (one card, no product segments, its own period line and sign
convention) read for two things on two gates: its period balances, which load
for EVERY era into `statement_balances`, and its transactions, which stay
below the account's export seam like the deposit pass's. Above the seam a
statement is read for one thing only: the payee of an export-era cheque,
which the export reduces to `CHECK <n>` (`annotate_export_cheques`).

The card period balances then anchor `derive_card_balances`, which
reconstructs the running balance neither card export carries. It rolls the
posted ledger forward between consecutive statement closing balances and, for
the newest span, on to the roster's live balance; a span that fails to land
on its closing anchor keeps no derived balance at all, and the statement era
carries none per row at all (its anchors carry that era's balance truth).
Every reconstructed balance in silver — this pass's and the deposit statement
pass's alike — is marked `payload.balance_basis` so it reads as what it is:
computed here, not read off a provider file. Only the deposit CSV export's
own running-balance column goes in unmarked.

Bronze run-dir layout consumed:

    <run>/
      run.json                      status manifest (its per-product coverage
                                      block is diagnostic; nothing here gates
                                      on it — see `load_run`)
      accounts.json                 [{account_external_id, product,
                                      account_type, nickname, mask,
                                      currency, balance, …card detail}, …]
      transactions/<ext_id>.qfx     OFX export (FITID + clean descriptor)
      transactions/<ext_id>.csv     CSV export (balance / category + type)
      statements/<ext_id>/<name>.pdf
"""
from __future__ import annotations

import argparse
import csv as csvmod
import hashlib
import io
import itertools
import json
import logging
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver, srcfp
from collectorkit.money import parse_money

import statement_parser

log = logging.getLogger("chase.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Re-export so tests + siblings can call load.apply_migrations directly.
apply_migrations = silver.apply_migrations

# transactions.source tags. The export seam's correctness hangs on the
# export set staying in sync with what merge_transactions writes — one home.
SOURCE_QFX = "qfx"
SOURCE_CSV = "csv"
SOURCE_STATEMENT = "statement"
EXPORT_SOURCES = (SOURCE_QFX, SOURCE_CSV)

# The statement passes' rows are re-derived from the PDFs every time they
# run, and their ids hash the description the parser read, so an edit to the
# parser re-keys them. `parser_generations` (migration 0005) records which
# generation produced the rows silver is holding; when it has moved, the
# statement rows are dropped before the passes re-derive them, so a re-parse
# replaces rather than accumulates.
#
# The extraction side is the `pdftotext` BINARY rather than a Python
# distribution, so it is pinned by the version it reports rather than by an
# installed dist: a poppler upgrade that re-renders a column changes every
# parsed description, and it is the drift this collector is otherwise least
# able to see.
STATEMENT_GENERATION_SCOPE = "statement"
STATEMENT_GENERATION = srcfp.parser_fingerprint(
    [statement_parser], extra_tools=(("pdftotext", "-v"),))

# transactions.payload marker for a balance this loader computed rather than
# read off the provider's own file. EXACTLY ONE balance in silver is
# provider-supplied and therefore unmarked: the deposit CSV export's
# running-balance column, joined onto the QFX row it belongs to. Every other
# balance is reconstructed and carries this marker — a deposit statement
# row's, rolled forward from its segment's printed opening figure, and a
# card's, rolled between statement anchors (neither card export has such a
# column). The marker is what lets "no marker" mean "the provider stated this
# number for this row", which is the only reading that is true of all of them.
BALANCE_BASIS_DERIVED = "derived"


# ============================================================
# Products
# ============================================================
# download.py stamps every roster record with `product` ('dda' for the
# deposit accounts, 'card' for the credit cards) and files each account's
# artefacts under its external id (`transactions/<ext>.{csv,qfx}`,
# `statements/<ext>/`). Both products land in the same silver tables,
# discriminated by accounts.product.
PRODUCT_CARD = "card"
PRODUCT_DEPOSIT = "dda"


def account_product(acct: dict) -> str:
    """A bronze roster record's product discriminator. Runs written before
    cards were walked carry no `product` key at all and are deposit-only, so a
    missing key reads as the deposit default and loads exactly as it always
    did."""
    return str(acct.get("product") or PRODUCT_DEPOSIT).strip().lower()


def card_account_ids(accounts: list[dict]) -> set[str]:
    """The external ids a roster stamps as card accounts."""
    ids = {str(a.get("account_external_id") or "").strip()
           for a in accounts if account_product(a) == PRODUCT_CARD}
    ids.discard("")
    return ids


# ============================================================
# Date / money parsing
# ============================================================

def _epoch_day(d) -> int:
    """Unix seconds UTC at midnight of the given datetime or date."""
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


def _iso_day(epoch: int) -> str:
    """An epoch-midnight timestamp back as `YYYY-MM-DD`, for log messages."""
    return datetime.fromtimestamp(epoch, tz=timezone.utc).date().isoformat()


def parse_ofx_date(raw: str) -> int | None:
    """OFX DTPOSTED → epoch-midnight seconds. Accepts `YYYYMMDD` optionally
    followed by time and a `[tz]` suffix; only the date is retained (Chase
    posts at day granularity)."""
    if not raw:
        return None
    m = re.match(r"\s*(\d{4})(\d{2})(\d{2})", raw)
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


def parse_csv_date(raw: str) -> int | None:
    """Chase CSV `Posting Date` (MM/DD/YYYY) → epoch-midnight seconds."""
    raw = (raw or "").strip()
    for fmt in ("%m/%d/%Y", "%m/%d/%y"):
        try:
            return _epoch_day(datetime.strptime(raw, fmt))
        except ValueError:
            continue
    return None


# ============================================================
# QFX (OFX 1.x SGML) parsing
# ============================================================

# OFX 1.x is SGML with unclosed leaf tags, so a per-<STMTTRN> regex sweep is
# the pragmatic parse (no closing-tag assumptions). Value runs to end-of-line
# or the next tag.
_STMTTRN_RE = re.compile(r"<STMTTRN>(.*?)</STMTTRN>", re.S | re.I)


def _ofx_field(block: str, tag: str) -> str | None:
    m = re.search(rf"<{tag}>([^<\r\n]*)", block, re.I)
    return m.group(1).strip() if m else None


def parse_qfx(text: str) -> list[dict]:
    """Parse the STMTTRN rows of an OFX/QFX export into dicts with keys
    fitid, posted_at, amount, kind, description, memo, check_number.
    Malformed rows (no date or amount) are skipped."""
    rows = []
    for block in _STMTTRN_RE.findall(text):
        posted_at = parse_ofx_date(_ofx_field(block, "DTPOSTED") or "")
        amount = parse_money(_ofx_field(block, "TRNAMT"))
        if posted_at is None or amount is None:
            continue
        check = _ofx_field(block, "CHECKNUM")
        rows.append({
            "fitid": _ofx_field(block, "FITID"),
            "posted_at": posted_at,
            "amount": amount,
            "kind": _ofx_field(block, "TRNTYPE"),
            "description": _ofx_field(block, "NAME"),
            "memo": _ofx_field(block, "MEMO"),
            "check_number": check or None,
        })
    return rows


# ============================================================
# CSV parsing
# ============================================================

def _csv_key(name: str | None) -> str:
    """A CSV header name reduced to its lookup form: whitespace and BOM
    stripped, then CASEFOLDED. Case is the drift these exports actually
    take — a provider re-casing `Post Date` to `Post date` renames no
    column, but an exact-match lookup reads it as a missing one and silently
    yields a parse with zero rows. Casefolded lookups read both spellings
    identically, with no data lost either way."""
    return (name or "").strip().lstrip("﻿").casefold()


def _csv_table(text: str) -> tuple[list[str], list[dict]]:
    """The CSV as (header names, rows), every name in its `_csv_key` lookup
    form and every value a string, so column lookups are stable. The header
    comes back alongside the rows so a parser can tell a genuinely absent
    column from a file that simply carried no data lines."""
    reader = csvmod.DictReader(io.StringIO(text))
    rows = [{_csv_key(k): (v or "") for k, v in raw.items()}
            for raw in reader]
    return [_csv_key(k) for k in (reader.fieldnames or [])], rows


def _has_date_column(product: str, column: str, header: list[str]) -> bool:
    """Whether a CSV header states its product's post-date column, which
    every row is keyed on — without it the parser yields nothing at all.

    A header that does not is a format drift, and it is reported rather than
    raised on: this loader walks a whole bronze tree unattended, so an
    exception on one account's export would abort every other account's and
    every other run's load too. An empty header is not drift — that is a file
    with no content — and returns True so the parse falls through to zero
    rows on its own. `load_run` counts the accounts whose CSV parsed to
    nothing, so the drift is named by the run that ingested it."""
    if not header or _csv_key(column) in header:
        return True
    log.warning("unexpected %s CSV header: no '%s' column; 0 rows",
                product, column)
    return False


def _csv_yielded_nothing(text: str, rows: list) -> bool:
    """Whether an export CSV that carried data lines parsed to no rows — the
    shape a header drift takes on the way through.

    A file that is empty, or that is a header line and nothing else, is not
    drift: an account with no activity in the window exports exactly that.
    Only a file with data lines under a header the parsers could not read is,
    and the loss is silent otherwise — the merge falls back to a QFX-only
    ledger, which on a card means every row loses its provider category, its
    transaction date and its 5-way `Type`, and gold's spending population
    then admits none of them."""
    if rows or not text:
        return False
    return len([ln for ln in text.splitlines() if ln.strip()]) > 1


def parse_csv(text: str) -> list[dict]:
    """Parse the Chase DEPOSIT activity CSV into dicts with keys posted_at,
    amount, description, kind, balance, check_number. Column names follow the
    observed header (DESIGN.md §E): Details, Posting Date, Description,
    Amount, Type, Balance, Check or Slip #, matched case-insensitively
    (`_csv_key`). Rows without a parseable date + amount are skipped, and a
    header with no post-date column at all is reported and yields none."""
    header, table = _csv_table(text)
    if not _has_date_column("deposit", "Posting Date", header):
        return []
    rows = []
    for row in table:
        posted_at = parse_csv_date(row.get("posting date", ""))
        amount = parse_money(row.get("amount"))
        if posted_at is None or amount is None:
            continue
        check = (row.get("check or slip #") or "").strip()
        rows.append({
            "posted_at": posted_at,
            "amount": amount,
            "description": (row.get("description") or "").strip() or None,
            "kind": (row.get("type") or "").strip() or None,
            "balance": parse_money(row.get("balance")),
            "check_number": check or None,
        })
    return rows


def parse_card_csv(text: str) -> list[dict]:
    """Parse the Chase CARD activity CSV into dicts with keys posted_at,
    txn_date, amount, description, category, kind, memo.

    Its header is its own (DESIGN.md §E / the card capture):
    `Transaction Date,Post Date,Description,Category,Type,Amount,Memo` — a
    separate transaction date, a provider category, a 5-way `Type`
    (Sale / Payment / Return / Adjustment / Fee), and NO balance or currency
    column. `posted_at` is the POST date, matching the deposit ledger's
    meaning and the QFX `<DTPOSTED>`.

    Amounts stay provider-verbatim: spend negative, anything reducing the
    balance owed positive. `Category` is empty on payments and `Memo` on every
    observed row; both reduce to None. Columns are matched
    case-insensitively (`_csv_key`); rows without a parseable post date +
    amount are skipped, and a header with no post-date column at all is
    reported and yields none."""
    header, table = _csv_table(text)
    if not _has_date_column("card", "Post Date", header):
        return []
    rows = []
    for row in table:
        posted_at = parse_csv_date(row.get("post date", ""))
        amount = parse_money(row.get("amount"))
        if posted_at is None or amount is None:
            continue
        rows.append({
            "posted_at": posted_at,
            "txn_date": parse_csv_date(row.get("transaction date", "")),
            "amount": amount,
            "description": (row.get("description") or "").strip() or None,
            "category": (row.get("category") or "").strip() or None,
            "kind": (row.get("type") or "").strip() or None,
            "memo": (row.get("memo") or "").strip() or None,
        })
    return rows


# The card export's own header, and the card OFX message set. An export pair
# is routed by what the FILES say, not by the roster: a run whose
# accounts.json is missing or unreadable would otherwise feed a card export to
# the deposit parsers, which read no rows from the card CSV (no `Posting Date`
# column) and would key the card's QFX rows into the deposit id space.
_CARD_CSV_HEADER_RE = re.compile(r"^\s*Transaction Date\s*,\s*Post Date\s*,",
                                 re.I)
_CARD_MSGSET_RE = re.compile(r"<CREDITCARDMSGSRSV1>", re.I)


def is_card_export(qfx_text: str, csv_text: str) -> bool:
    """Whether an account's export pair is the card shape. Either file is
    sufficient evidence, so an account exported in only one format still
    routes correctly."""
    if csv_text and _CARD_CSV_HEADER_RE.match(csv_text.lstrip("﻿")):
        return True
    return bool(qfx_text) and _CARD_MSGSET_RE.search(qfx_text) is not None


# ============================================================
# Join + id synthesis
# ============================================================
#
# A transaction's silver id is `<product>:<ext_id>:<content key>:<occurrence>`
# for BOTH products, derived from the row's CONTENT and NEVER from the
# provider's OFX `<FITID>`. The reason both ledgers key this way:
#
#   FORMAT INDEPENDENCE. Each format is fetched and can fail on its own, so an
#   account can land CSV-only in one run and as a complete pair in the next.
#   The FITID lives only in the QFX, so a FITID-keyed id gives the same rows
#   two different identities across those runs — and INSERT OR IGNORE then
#   lands BOTH, permanently doubling that account's ledger and everything
#   computed off it. A content key is the same key whichever file arrived.
#
# For cards there is a second, independent reason: FITIDs ARE NOT UNIQUE, not
# even within one export (see the card section).
#
# The FITID is still carried, as a `fitid` payload field, so a row stays
# traceable back to the provider's own id.
#
# CONTENT KEY, PER PRODUCT. The key may only hash fields BOTH of that
# product's exports state identically — a field one format mangles or omits
# would reintroduce exactly the divergence the content key removes. That
# admits (account, post date, amount) everywhere, plus the descriptor on the
# card ledger, where the two files differ in one known, normalisable way
# (`card_descriptor_key`). It admits no descriptor on the DEPOSIT ledger: the
# QFX splits it into `<NAME>` + `<MEMO>` while the CSV `Description` is one
# longer field, and the two do not reduce to a common form. Nor the check
# number, which the CSV can carry where the QFX omits it, nor `kind` (the two
# vocabularies differ), nor the balance (CSV-only).
#
# OCCURRENCE INDEX. Rows really do collide on a content key — same day, same
# amount, and on a deposit account no descriptor to separate them — so the id
# appends the count of earlier rows of the same account sharing the key
# (`_id_minter`). Without it, identical same-day rows collapse into one id and
# the ledger silently loses rows.
#
# STABILITY: rows sharing a key are by construction identical in every field
# the key covers, so the SET of ids an account's ledger produces does not
# depend on the order the rows are walked in — which is what lets a CSV-only
# load and a later complete load converge on exactly the same ids. Ids remain
# a convergence key for re-loads, not a durable external reference: a re-priced
# row is new content and gets a new id.
#
# What order DOES decide is which row's CONTENT sits under a colliding id,
# because `_insert_transaction` is INSERT OR IGNORE and the first load wins.
# On the deposit ledger the key is the coarser of the two (no descriptor), so
# a row a run saw CSV-only — a pending item, with the CSV's own `Type`,
# `Description` and no running balance — keeps those fields when a later run
# carries the same event in the QFX. That is the same row, described by the
# other file, rather than the second copy the FITID key used to insert; the
# ledger is right either way, and only the pending row's own balance is lost.
# The card ledger cannot settle for that — each of its two exports is the
# ONLY source of several of its columns — so it is written by
# `_upsert_card_transaction` instead, which repairs the stored row from
# whichever export the load that first landed it was missing.

def _join_key(posted_at: int, amount: float) -> tuple:
    """The bucket a CSV row and a QFX row must share to be the same event.

    (post date, amount rounded to cents) — the amount is rounded so float
    noise never splits a match, and the CHECK NUMBER IS DELIBERATELY NOT IN
    THE KEY. The two exports disagree about it: a row can carry a check number
    in the CSV that the QFX omits, and keying on it then failed the join
    outright — the QFX row was emitted, the same CSV row was emitted AGAIN as
    an unmatched leftover, and the withdrawal was double-counted in a ledger
    that feeds the spending base. Content-derived ids do not save it from
    that: two rows the join failed to pair are two rows, and the occurrence
    index dutifully gives them distinct ids. The check number breaks ties
    inside a bucket instead (`_pick_csv_match`)."""
    return (posted_at, round(amount, 2))


def _csv_buckets(csv_rows: list[dict]) -> dict[tuple, list[int]]:
    """Index the CSV rows by their join key, each bucket in file order. Both
    products join through this, so a QFX row's candidates are found in one
    lookup rather than a scan."""
    out: dict[tuple, list[int]] = {}
    for i, r in enumerate(csv_rows):
        out.setdefault(_join_key(r["posted_at"], r["amount"]), []).append(i)
    return out


def _pick_csv_match(csv_rows: list[dict], bucket: list[int],
                    check_number: str | None) -> int | None:
    """Pop and return the index of the CSV row a QFX row joins to, or None
    when the bucket is exhausted. The first unconsumed candidate wins, except
    that a candidate whose check number matches is preferred when BOTH sides
    carry one — only then is a mismatch evidence of a different event."""
    if not bucket:
        return None
    check = (check_number or "").strip()
    if check:
        for pos, idx in enumerate(bucket):
            if (csv_rows[idx]["check_number"] or "").strip() == check:
                return bucket.pop(pos)
    return bucket.pop(0)


def _content_hash(prefix: str, account_external_id: str, posted_at: int,
                  amount: float, description: str | None, tail: str) -> str:
    """The content-hash scheme (field order, 2-decimal amount, 24-hex
    truncation) behind every id this loader mints — stability-critical,
    re-loads converge on it, so it has exactly one implementation."""
    basis = "|".join([account_external_id, str(posted_at), f"{amount:.2f}",
                      (description or "").strip(), tail])
    return prefix + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:24]


def _txn_id(product: str, account_external_id: str, key: str, occ: int) -> str:
    """The id shape both ledgers share: `<product>:<ext_id>:<key>:<occ>`, where
    `key` is the row's content key and `occ` counts how many earlier rows of
    the same account shared it. Namespacing by product and account means no
    two accounts can ever collide, whatever their content."""
    return f"{product}:{account_external_id}:{key}:{occ}"


def _id_minter(product: str, account_external_id: str):
    """An id minter for one account's ledger: call it with a row's content key
    and it returns that row's id, counting the occurrence itself. The count
    spans the whole merge, so a colliding key split across the QFX pass and the
    CSV-leftover pass still yields one id per row."""
    occ_seen: dict[str, int] = {}

    def mint(key: str) -> str:
        occ = occ_seen.get(key, 0)
        occ_seen[key] = occ + 1
        return _txn_id(product, account_external_id, key, occ)
    return mint


def deposit_content_key(account_external_id: str, posted_at: int,
                        amount: float) -> str:
    """A deposit row's content key: account, post date and amount — and
    NOTHING else, because nothing else is stated identically by both exports.

    The descriptor in particular is out. The QFX splits it into `<NAME>` and
    `<MEMO>` while the CSV `Description` is a single, generally longer field
    that neither reproduces, so the two do not reduce to a common form.
    Hashing it would put back exactly the format dependence the content key
    exists to remove. The check number is out for the same reason (the CSV
    can carry one the QFX omits — see `_join_key`), as are `kind` (the two
    vocabularies differ) and the balance (CSV-only).

    That leaves a key that collides freely — same-day same-amount rows are
    ordinary on a deposit account — which is what the occurrence index
    `_id_minter` appends is for. The key is deliberately the join bucket
    (`_join_key`) plus the account, so a row's occurrence index is simply its
    position among the rows the join itself treats as interchangeable."""
    return _content_hash("d_", account_external_id, posted_at, amount, "", "")


def merge_transactions(account_external_id: str, qfx_rows: list[dict],
                       csv_rows: list[dict]) -> list[dict]:
    """Join the CSV running balance onto the QFX rows and return the ledger.

    The QFX row is the row of record — it carries the clean type/name/memo
    split — and takes the balance of the first unconsumed CSV row in its
    (post date, amount) bucket, preferring one whose check number matches when
    both sides carry one (`_pick_csv_match`). Consuming the match pairs
    duplicate same-day/same-amount rows one-to-one. CSV rows left unmatched
    (QFX omitted them, e.g. pending) are appended so nothing is lost.

    Every row is identified by its content key regardless of which file it came
    from (see the section comment above), so loading a deposit CSV-only and
    then loading the complete pair converges on the same ids instead of
    doubling the ledger. The provider FITID stays in `payload`."""
    csv_by_key = _csv_buckets(csv_rows)
    mint = _id_minter(PRODUCT_DEPOSIT, account_external_id)
    out = []
    consumed: set[int] = set()
    for q in qfx_rows:
        bucket = csv_by_key.get(_join_key(q["posted_at"], q["amount"]), [])
        idx = _pick_csv_match(csv_rows, bucket, q["check_number"])
        balance = None
        if idx is not None:
            consumed.add(idx)
            balance = csv_rows[idx]["balance"]
        out.append({
            "fitid": mint(deposit_content_key(account_external_id,
                                              q["posted_at"], q["amount"])),
            "posted_at": q["posted_at"],
            "amount": q["amount"],
            "kind": q["kind"],
            "description": q["description"],
            "check_number": q["check_number"],
            "balance": balance,
            "source": SOURCE_QFX,
            # `fitid` is traceability only — the provider's own id for the
            # row. It is NOT the identity (see the section comment): the CSV
            # export carries none at all.
            "payload": {"fitid": (q["fitid"] or "").strip() or None,
                        **{k: q[k] for k in ("kind", "description", "memo",
                                             "check_number")}},
        })

    # Leftover CSV rows QFX never carried, in the CSV's own row order.
    for i, r in enumerate(csv_rows):
        if i in consumed:
            continue
        out.append({
            "fitid": mint(deposit_content_key(account_external_id,
                                              r["posted_at"], r["amount"])),
            "posted_at": r["posted_at"],
            "amount": r["amount"],
            "kind": r["kind"],
            "description": r["description"],
            "check_number": r["check_number"],
            "balance": r["balance"],
            "source": SOURCE_CSV,
            "payload": {"fitid": None,      # the CSV export carries none
                        "kind": r["kind"], "description": r["description"],
                        "check_number": r["check_number"]},
        })
    return out


# ============================================================
# Card join + id synthesis
# ============================================================
#
# A card row is identified exactly as a deposit row is — content key plus
# occurrence index, never the FITID (see the id doctrine above the join). The
# format-independence argument applies unchanged, and on a card a SECOND,
# independent one does too:
#
#   FITIDs ARE NOT UNIQUE, not even within one export: a credit that offsets
#   an earlier charge is issued the SAME FITID as the charge it reverses, so
#   an INSERT OR IGNORE on the bare FITID drops one leg of every reversal
#   pair. (One FITID family also embeds the row's signed amount, which would
#   make identity move when a row is re-priced.)
#
# The card content key is also one field wider than the deposit one: the two
# card exports state the descriptor in a single known, normalisable way apart
# (`card_descriptor_key`), where the deposit exports do not.

def card_descriptor_key(description: str | None) -> str:
    """The descriptor reduced to the form BOTH card exports agree on.

    The content key must hash a descriptor that is present in EITHER file
    identically, or the id would once again depend on which format arrived.
    The two disagree in exactly one way: the CSV `Description` replaces a
    descriptor's commas with spaces while the QFX `<NAME>` keeps them, so the
    common form is the comma-free one (whitespace runs collapsed, so the extra
    space a replaced comma leaves behind cannot split the two apart)."""
    return re.sub(r"\s+", " ", (description or "").replace(",", " ")).strip()


def card_content_key(account_external_id: str, posted_at: int, amount: float,
                     description: str | None) -> str:
    """A card row's content key: account, post date, amount and the
    both-formats descriptor (`card_descriptor_key`) — the deposit key
    (`deposit_content_key`) plus the one further field the card exports do
    agree on. Content collisions on those fields are real on a card ledger
    too, which is why `_id_minter` appends an occurrence index to this
    key."""
    return _content_hash("c_", account_external_id, posted_at, amount,
                         card_descriptor_key(description), "")


def merge_card_transactions(account_external_id: str, qfx_rows: list[dict],
                            csv_rows: list[dict]) -> list[dict]:
    """Join a card's CSV and QFX exports into the ledger.

    The two cover exactly the same rows and each carries what the other
    lacks — the CSV the transaction date, the provider category and the 5-way
    `Type`, the QFX the FITID and the un-mangled descriptor — so this joins
    them rather than treating one as a supplement to the other. The join is
    the deposit path's: QFX rows are walked in file order and each consumes
    the first CSV row matching (post date, amount), which pairs same-day
    same-amount rows one-to-one in file order. A row either export missed
    survives on its own — QFX-only without a category or transaction date,
    CSV-only without the un-mangled descriptor.

    Every row is identified by its content key regardless of which file it
    came from (see the section comment above), so loading a card CSV-only and
    then loading the complete pair converges on the same ids instead of
    doubling the ledger. Converging the ids is not converging the CONTENT:
    the second load re-mints the same id for a row silver already holds, so
    the card ledger is written by `_upsert_card_transaction`, which lets each
    export repair the columns it alone is authoritative for. Each emitted row
    STATES which export backs it (`_csv_backed` / `_qfx_backed`) so that
    writer reads field authority off the join that decided it, rather than
    re-deriving it from a payload cell the export is allowed to leave blank.

    `kind` takes the CSV `Type`: the QFX `TRNTYPE` is only DEBIT/CREDIT,
    recoverable from the sign, while the CSV distinguishes Sale / Payment /
    Return / Adjustment / Fee. A QFX-only row falls back to its TRNTYPE. The
    raw values of both stay in `payload`, along with the provider FITID.

    No `balance`: neither card export carries a running balance, and the
    QFX `<LEDGERBAL>` is a point-in-time account total, not a per-row one."""
    # The deposit join, unchanged — including `_pick_csv_match`, whose check
    # number tie-break simply never fires on a card (no card export carries
    # one), so the first unconsumed candidate always wins.
    csv_by_key = _csv_buckets(csv_rows)
    mint = _id_minter(PRODUCT_CARD, account_external_id)
    out = []
    consumed: set[int] = set()
    for q in qfx_rows:
        bucket = csv_by_key.get(_join_key(q["posted_at"], q["amount"]), [])
        idx = _pick_csv_match(csv_rows, bucket, None)
        c: dict = {}
        if idx is not None:
            consumed.add(idx)
            c = csv_rows[idx]
        # The QFX <NAME> is the descriptor of record — the CSV replaces a
        # descriptor's commas with spaces.
        merchant = q["description"] or c.get("description")
        csv_desc = c.get("description")
        out.append({
            "fitid": mint(card_content_key(account_external_id, q["posted_at"],
                                           q["amount"], merchant)),
            "posted_at": q["posted_at"],
            "txn_date": c.get("txn_date"),
            "amount": q["amount"],
            "kind": c.get("kind") or q["kind"],
            "description": merchant,
            "merchant": merchant,
            "category": c.get("category"),
            "currency": None,
            "check_number": None,
            "balance": None,
            "source": SOURCE_QFX,
            # Which export backs this row, STATED rather than sniffed off a
            # nullable payload field (`_upsert_card_transaction`).
            "_csv_backed": idx is not None,
            "_qfx_backed": True,
            "payload": {
                # Traceability only — the provider's own id for the row. It is
                # NOT the identity (see the section comment): it is absent
                # CSV-only, and repeats across a reversal pair.
                "fitid": (q["fitid"] or "").strip() or None,
                "trntype": q["kind"], "type": c.get("kind"),
                "name": q["description"],
                "csv_description": csv_desc if csv_desc != merchant else None,
                "category": c.get("category"), "memo": c.get("memo"),
            },
        })

    # CSV rows the QFX never carried, in the CSV's own row order.
    for i, r in enumerate(csv_rows):
        if i in consumed:
            continue
        out.append({
            "fitid": mint(card_content_key(account_external_id, r["posted_at"],
                                           r["amount"], r["description"])),
            "posted_at": r["posted_at"],
            "txn_date": r["txn_date"],
            "amount": r["amount"],
            "kind": r["kind"],
            "description": r["description"],
            "merchant": r["description"],
            "category": r["category"],
            "currency": None,
            "check_number": None,
            "balance": None,
            "source": SOURCE_CSV,
            "_csv_backed": True,
            "_qfx_backed": False,
            "payload": {
                "fitid": None,          # the CSV export carries none
                "trntype": None, "type": r["kind"], "name": None,
                "csv_description": r["description"],
                "category": r["category"], "memo": r["memo"],
            },
        })
    return out


# ============================================================
# DB inserts
# ============================================================

def _payload_dict(raw) -> dict:
    """A stored `payload` column decoded back to a dict, and {} for anything
    that is not one. Every pass that REWRITES a payload reads it through
    here first, so a row whose blob is missing or malformed degrades to an
    empty overlay instead of aborting the load."""
    try:
        payload = json.loads(raw) if raw else {}
    except ValueError:                                      # pragma: no cover
        return {}
    return payload if isinstance(payload, dict) else {}


def _insert_account(conn, snapshot_at: int, acct: dict) -> None:
    ext = str(acct.get("account_external_id") or "").strip()
    if not ext:
        return
    payload = silver.canonical_json(acct)
    prev = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id=? "
        "ORDER BY snapshot_at DESC LIMIT 1", (ext,)).fetchone()
    if prev is not None and prev[0] == payload:
        return  # content-dedup: unchanged since the last snapshot
    conn.execute(
        "INSERT OR REPLACE INTO accounts (snapshot_at, account_external_id, "
        "product, account_type, nickname, mask, currency, balance, "
        "pending_charges, payload) VALUES (?,?,?,?,?,?,?,?,?,?)",
        (snapshot_at, ext, account_product(acct), acct.get("account_type"),
         acct.get("nickname"), acct.get("mask"), acct.get("currency"),
         # Provider-verbatim: a card's balance is the POSITIVE amount owed.
         parse_money(acct.get("balance")),
         parse_money(acct.get("pending_charges_amount")), payload))


def _insert_transaction(conn, account_external_id: str, tx: dict) -> None:
    """Insert one ledger row, ignoring a re-insert of an id already present
    (this is what makes a re-load converge). The card-only columns default to
    NULL, so a deposit row's dict needs no card keys."""
    conn.execute(
        "INSERT OR IGNORE INTO transactions (fitid, posted_at, "
        "account_external_id, amount, kind, description, check_number, "
        "balance, source, payload, txn_date, merchant, category, currency) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (tx["fitid"], tx["posted_at"], account_external_id, tx["amount"],
         tx["kind"], tx["description"], tx["check_number"], tx["balance"],
         tx["source"], silver.canonical_json(tx["payload"]),
         tx.get("txn_date"), tx.get("merchant"), tx.get("category"),
         tx.get("currency")))


# Which card column each export is authoritative for. The CSV alone carries
# the transaction date, the provider category and the 5-way `Type`; the QFX
# alone carries the un-mangled descriptor (the CSV replaces a descriptor's
# commas with spaces) and `source`, which names the export a row was read
# from and so appears in no CSV set — see `merge_card_transactions`.
_CARD_CSV_COLUMNS = ("category", "txn_date", "kind")
_CARD_QFX_COLUMNS = ("merchant", "description", "source")


def _upsert_card_transaction(conn, account_external_id: str,
                             tx: dict) -> None:
    """Insert a card ledger row, or repair the stored one with what this
    export knows and the export that first landed it did not.

    INSERT OR IGNORE is right for the deposit and statement ledgers, where
    the first writer is the only one with anything to say about a row. It is
    wrong for a card, because the two card exports are authoritative over
    DIFFERENT columns and each is fetched separately and can fail on its own.
    The ids converge whichever file arrived (`card_content_key`), which is
    exactly what makes a plain re-insert a no-op — so under INSERT OR IGNORE
    a run that landed the QFX alone pins `category` and `txn_date` NULL and
    the two-way `TRNTYPE` as `kind` forever, and a run that landed the CSV
    alone pins the comma-mangled descriptor as the merchant forever. Gold
    projects both verbatim, and only `load --force` ever undid it.

    Which export backs an incoming row is STATED by the join that built it
    (`merge_card_transactions` stamps `_csv_backed` / `_qfx_backed`), never
    sniffed off a payload cell: a card CSV row may legitimately leave `Type`
    or `Category` blank, and a writer that read provenance off those refused
    to adopt the very columns the row did carry. Each side's columns are
    offered only by a row that side backs, so a later CSV-only run never
    overwrites an un-mangled descriptor with the mangled one.

    A column is written only when the incoming row actually CARRIES it. The
    repair is additive, like every other silver write: it fills a column the
    first export could not and corrects one a poorer export pinned, and an
    absent value never wins over a stored one. `kind` needs the payload to
    say that: the merged row falls back to the QFX TRNTYPE where the CSV
    stated no `Type`, and only `payload["type"]` — the cell verbatim —
    distinguishes the fallback from a stated value. Otherwise a single blanked
    `Category` cell — or a header drift that renames only that column, which
    still parses every row and so trips no drift report — would NULL the
    provider category the gold spending tier reads, silently moving a whole
    card onto the paid model backlog.

    Nothing outside those two column sets moves. Not `posted_at` or `amount`,
    which the content key already fixes; and above all not `balance`, which
    no card export carries and `derive_card_balances` owns. The payload is
    merged key by key rather than replaced, so `balance_basis` — and
    anything else a later pass stored inside it — survives the repair."""
    csv_backed = bool(tx.pop("_csv_backed", False))
    qfx_backed = bool(tx.pop("_qfx_backed", False))
    stored = conn.execute("SELECT payload FROM transactions WHERE fitid = ?",
                          (tx["fitid"],)).fetchone()
    if stored is None:
        _insert_transaction(conn, account_external_id, tx)
        return
    columns = ()
    if csv_backed:
        columns += _CARD_CSV_COLUMNS
    if qfx_backed:
        columns += _CARD_QFX_COLUMNS
    # `kind` is CSV-authoritative, but the merged value FALLS BACK to the
    # QFX two-way TRNTYPE when the CSV `Type` cell was absent
    # (`merge_card_transactions`), and a fallback is indistinguishable from
    # a stated value by the time it gets here. `payload["type"]` is the CSV
    # cell verbatim, None exactly when it was blank, so it is what says
    # whether the CSV stated a kind at all. Without this, one blanked cell —
    # or a header drift renaming only `Type`, which still parses every row
    # and so trips no drift report — would overwrite the stored 5-way kind
    # with DEBIT/CREDIT, which silver maps to `other` and gold drops out of
    # the spending base: the same failure the `Category` guard above exists
    # to stop, reached through a different column. The INSERT path keeps the
    # fallback, so a QFX-only row still lands with its TRNTYPE.
    if tx["payload"].get("type") is None:
        columns = tuple(c for c in columns if c != "kind")
    cols = tuple(c for c in columns if tx.get(c) is not None)
    payload = _payload_dict(stored[0])
    merged = dict(payload)
    merged.update({k: v for k, v in tx["payload"].items() if v is not None})
    if not cols and merged == payload:
        return                          # this export repairs nothing here
    sets = ", ".join(["payload = ?", *(f"{c} = ?" for c in cols)])
    conn.execute(
        f"UPDATE transactions SET {sets} WHERE fitid = ?",
        (silver.canonical_json(merged), *(tx.get(c) for c in cols),
         tx["fitid"]))


def _insert_document(conn, snapshot_at: int, account_external_id: str,
                     pdf: Path) -> None:
    sha, size = bronze.sha256_file(pdf)
    doc_date = _statement_date_from_name(pdf.name)
    silver.record_document(conn, (
        sha, snapshot_at, account_external_id, doc_date, "statement", "pdf",
        pdf.name, size, silver.canonical_json({"source_name": pdf.name})))


_DATE_IN_NAME_RE = re.compile(r"(20\d{2})[-_]?(\d{2})[-_]?(\d{2})")


def _statement_date_from_name(name: str) -> int | None:
    """Best-effort statement date from a filename like
    `…-statements-20260731-…pdf`. None when no date is embedded."""
    m = _DATE_IN_NAME_RE.search(name)
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


# ============================================================
# Statement-PDF transactions — the pre-export tail
# ============================================================

_EXPORT_SOURCES_SQL = f"source IN ({','.join('?' * len(EXPORT_SOURCES))})"


def _export_seams(conn) -> dict[str, int]:
    """Each account's earliest export-sourced transaction date — the cutover
    below which its statement PDFs are authoritative. Anchored to the export
    rows, NOT all rows, so importing statements never drags a seam downward.
    Accounts with no export row are absent from the mapping.

    PER ACCOUNT, not one global minimum: export depth differs by product and
    by onboarding. A deposit account's backfill can pin its seam years before
    any card export reaches — the card export caps at 24 months, and only its
    statement PDFs go deeper — and a single MIN(posted_at) would then gate
    every account at the deepest one's seam and silently discard the shallower
    account's whole pre-export statement history. With one product and one
    account the per-account seam is that same global minimum, so the deposit
    ledger is unaffected."""
    return {ext: seam for ext, seam in conn.execute(
        "SELECT account_external_id, MIN(posted_at) FROM transactions "
        f"WHERE {_EXPORT_SOURCES_SQL} GROUP BY account_external_id",
        EXPORT_SOURCES) if seam is not None}


def _cents(x) -> int:
    """Balance identity normalized to integer cents — the one equality regime
    for matching a statement balance against another balance (Decimal from the
    parser or REAL from silver)."""
    return int(round(float(x) * 100))


def _statement_fitid(account_external_id: str, posted_at: int, amount: float,
                     description: str | None, occ: int) -> str:
    """Stable synthetic id for a statement transaction (no bank FITID). The
    occurrence index disambiguates identical same-day charges; the parse is
    deterministic, so the index — and the id — are stable across re-loads."""
    return _content_hash("stmt_", account_external_id, posted_at, amount,
                         description, f"#{occ}")


def _import_statement(conn, account_external_id: str, seg, seam) -> int:
    """Insert a reconciling segment's transactions that fall BEFORE the export
    seam (the export owns everything on/after it).

    The running balance is RECONSTRUCTED, rolled forward from the segment's
    printed beginning balance — the statement prints no per-row balance — so
    it carries `payload.balance_basis`, exactly as a card's derived balance
    does. Only the deposit CSV export's own running-balance column goes in
    unmarked."""
    occ_seen: dict[tuple, int] = {}
    n = 0
    for txn, bal in statement_parser.running_balances(seg):
        posted = _epoch_day(txn.posted_at)
        if seam is not None and posted >= seam:
            continue                                # export owns this period
        amount = float(txn.amount)
        desc = txn.description or None
        key = (posted, round(amount, 2), (desc or "").strip())
        occ = occ_seen.get(key, 0)
        occ_seen[key] = occ + 1
        payload = {"description": desc, "basis": "statement_pdf"}
        if bal is not None:
            payload["balance_basis"] = BALANCE_BASIS_DERIVED
        _insert_transaction(conn, account_external_id, {
            "fitid": _statement_fitid(account_external_id, posted, amount, desc, occ),
            "posted_at": posted, "amount": amount, "kind": None,
            "description": desc, "check_number": txn.check_number,
            "balance": float(bal) if bal is not None else None,
            "source": SOURCE_STATEMENT,
            "payload": payload,
        })
        n += 1
    return n


def _export_balances_between(conn, account_external_id: str,
                             lo: int, hi: int) -> set[int]:
    """The account's export running balances (in cents) posted in [lo, hi] —
    the anchor set a statement segment's ending balance is matched against."""
    return {_cents(r[0]) for r in conn.execute(
        f"SELECT balance FROM transactions WHERE {_EXPORT_SOURCES_SQL} "
        "AND account_external_id = ? AND posted_at BETWEEN ? AND ? "
        "AND balance IS NOT NULL",
        (*EXPORT_SOURCES, account_external_id, lo, hi))}


def _chain_segments(conn, account_external_id: str, parsed_stmts: list) -> list:
    """Attribute one segment per statement to the account, newest → oldest.

    A combined statement carries one segment per product, and the section
    names repeat across segments — so the account's rows can only be told
    apart by balances. The newest statement (which overlaps the export) is
    anchored by its segment whose ending balance appears among the export's
    running balances in-period; every older statement then chains on the
    deposit-account invariant ending(month k) == beginning(month k+1). An
    ambiguous or broken link stops the walk — older statements are skipped,
    never guessed at. Returns [(parsed, segment)] for the chained tail.

    An account that matches NO segment on the very FIRST statement of its walk
    is not a broken chain, it is an account this statement set never printed.
    The pool is relationship-wide, so an account opened after the statement
    era — or held on another relationship — reaches here on every load with
    nothing to anchor; that is its steady state and it is logged at debug. The
    warning is kept for a walk that anchors and then breaks, and for an
    ambiguous match, which are the cases where rows really are given up.

    `parsed_stmts` must be sorted by period_end descending."""
    chosen: list = []
    expected = None      # the newer statement's begin balance, in cents
    for parsed in parsed_stmts:
        if expected is None:
            anchors = _export_balances_between(
                conn, account_external_id,
                _epoch_day(parsed.period_start), _epoch_day(parsed.period_end))
        else:
            anchors = {expected}
        cands = [s for s in parsed.segments if s.ending_balance is not None
                 and _cents(s.ending_balance) in anchors]
        if len(cands) != 1:
            report = log.debug if not chosen and not cands else log.warning
            report(
                "statements: cannot attribute a segment for the period ending "
                "%s (%d candidate(s)); skipping it and everything older",
                parsed.period_end, len(cands))
            break
        chosen.append((parsed, cands[0]))
        expected = None if cands[0].beginning_balance is None \
            else _cents(cands[0].beginning_balance)
    return chosen


# How far past a day the statement covering it can be dated: a statement
# period spans at most ~5 weeks, and the file is named for its end.
_STATEMENT_SPAN = 45 * 86400


def _deposit_statements_in_tree(bronze_dir: Path, card_ids: set[str],
                                wanted) -> dict:
    """Parse the deposit statement PDFs in the bronze tree into
    {period_end: parsed}, RELATIONSHIP-WIDE.

    One combined statement carries a segment per product and per deposit
    account (DESIGN.md §C/§E), and the documents surface it comes off has no
    per-account selector, so the account directory it was filed under is a
    bucket rather than a claim about whose statement it is
    (`download._download_deposit_statements`). Pooling the PDFs and letting
    every deposit account chain against the same pool is what lets a second
    deposit account reach its own segments at all — and it needs no copy of
    the PDF in bronze, which a per-directory pass would.

    Deduped twice, in the order the two duplications arise: by content hash,
    because bronze runs re-download overlapping months, and then by period,
    keeping the first copy parsed. One sha is one document however many
    account directories it sits in, so the hash set is pool-wide.

    `wanted(name_date)` says which statements are worth parsing, by the
    period-end date in the file's name (None when the name carries none,
    which is always parsed): a PDF it turns down is skipped unparsed."""
    pool: dict = {}
    seen: set[str] = set()
    cards_seen: set[str] = set()
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        stmt_dir = run_dir / "statements"
        if not stmt_dir.is_dir():
            continue
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            if acct_dir.name in card_ids:
                cards_seen.add(acct_dir.name)
                continue
            for pdf in sorted(acct_dir.glob("*.pdf")):
                name_date = _statement_date_from_name(pdf.name)
                if name_date is not None and not wanted(name_date):
                    continue
                sha, _ = bronze.sha256_file(pdf)
                if sha in seen:
                    continue
                seen.add(sha)
                try:
                    parsed = statement_parser.parse_statement_pdf(pdf)
                except Exception as exc:            # pragma: no cover — env/poppler
                    log.warning("statement %s: parse failed (%r); skipping",
                                pdf.name, exc)
                    continue
                if parsed.period_start is None or parsed.period_end is None:
                    log.warning("statement %s: no period parsed; skipping",
                                pdf.name)
                    continue
                pool.setdefault(parsed.period_end, parsed)
    if cards_seen:
        log.info("statements: %d card account(s)' statements are inventoried "
                 "but not parsed by this pass", len(cards_seen))
    return pool


def _statement_row_count(conn: sqlite3.Connection) -> int:
    """How many statement-derived rows silver is holding — the before/after
    the re-derivation is judged on."""
    return conn.execute(
        "SELECT COUNT(*) FROM transactions WHERE source = ?",
        (SOURCE_STATEMENT,)).fetchone()[0]


def _purge_stale_statement_rows(conn: sqlite3.Connection, stale: bool) -> int:
    """Drop the statement-derived ledger so the passes below re-derive it
    rather than add to it.

    `stale` is the caller's verdict on the parser generation, taken as an
    argument rather than re-derived here so that ONE condition decides when
    this fires. Asking twice lets an edit to the call site's condition be
    silently ignored by the helper.

    Both statement passes write `source='statement'` and both key their rows
    on a hash that includes the parsed description, so they share one
    generation and one purge — nothing on a row separates a deposit
    statement's from a card's, and the re-derivation is per-tree either way.
    The export ledgers are untouched: their ids carry no parsed text.

    Deliberately not wrapped in a transaction of its own. `post_passes_pending`
    is this loader's recovery mechanism: a crash between the purge and the
    re-import leaves the flag raised and the generation unstamped, so the next
    load purges and re-derives in full. That is the window a crash part-way
    through an import already had.

    This drops BEFORE the re-parse rather than after, so a load on which the
    statements stopped parsing at all — poppler gone, the PDFs unreadable —
    leaves silver short of its statement rows until the next good load. The
    caller's row-count check is what makes that loud and temporary: a
    re-derivation that comes back short is not stamped, so the next load
    tries again. Deferring the purge until a statement has actually parsed
    would close that window, but it would also let a load where nothing
    parsed stamp the new generation over rows the old parser wrote — which
    re-arms the very duplication this exists to stop.
    """
    if not stale:
        return 0
    dropped = conn.execute("DELETE FROM transactions WHERE source = ?",
                           (SOURCE_STATEMENT,)).rowcount
    # The period anchors go too. `statement_balances` is keyed on
    # (account, period_end) and the period is READ OFF THE STATEMENT, so a
    # parser that dates a period differently mints a new anchor and leaves
    # the old one — and `_card_balance_anchors` takes every anchor an
    # account has, so the orphan lands in the span walk `derive_card_balances`
    # rolls between. They are re-derived from the same PDFs by the same pass.
    anchors = conn.execute("DELETE FROM statement_balances").rowcount
    log.info("statements: the parser has changed since these rows were "
             "written; dropped %d transaction(s) and %d period anchor(s) "
             "for re-derivation", dropped, anchors)
    return dropped


def load_statement_transactions(conn: sqlite3.Connection, bronze_dir: Path) -> int:
    """Parse the statement PDFs under the bronze tree and import the
    transactions older than the export window. Run after the export runs so
    every seam is complete; idempotent (synthetic ids, deterministic per
    content). Each account is gated on ITS OWN export seam (`_export_seams`);
    an account with no export row at all imports nothing, since
    `_chain_segments` has nothing to anchor its chain to. Only statements that
    can contribute pre-seam rows are read; each statement's account segment is
    picked by balance chaining, and a segment that does not reconcile is
    skipped with a warning, never imported — other products' and other
    accounts' segments on a combined statement are never touched.

    The parsed statements are ONE relationship-wide pool
    (`_deposit_statements_in_tree`), walked once per deposit account: a
    combined statement belongs to every deposit account on it, and which
    directory bronze filed it under says nothing about whose it is. An
    account whose chain cannot anchor in that pool simply imports nothing —
    silently when it never appears on any of those statements at all, with a
    warning when its chain anchors and then breaks (`_chain_segments`).

    Deposit accounts only. Card statements have their own layout (a different
    period line, per-row signs, no combined segments), their own parser and
    their own pass (`load_card_statements`). Which accounts those are is
    decided by `card_account_ids_in_tree` — the same file-content signal that
    routes the exports, not one run's roster."""
    seams = _export_seams(conn)
    card_ids = card_account_ids_in_tree(bronze_dir)
    deposit_seams = {ext: seam for ext, seam in seams.items()
                     if ext not in card_ids}
    if not deposit_seams:
        log.info("statements: no deposit export loaded yet — nothing to "
                 "anchor the statement chain to; skipping")
        return 0
    # The LATEST seam over the deposit accounts: a statement period spans at
    # most ~5 weeks, so a filename date this far past it can only cover days
    # some export already owns for every account.
    deepest_seam = max(deposit_seams.values())
    pool = _deposit_statements_in_tree(
        bronze_dir, card_ids,
        lambda name_date: name_date < deepest_seam + _STATEMENT_SPAN)
    imported = skipped = 0
    for ext_id, seam in sorted(deposit_seams.items()):
        # Per account: the statements whose period OPENED before its seam —
        # the others carry nothing the export does not already own.
        stmts = sorted((p for p in pool.values()
                        if _epoch_day(p.period_start) < seam),
                       key=lambda p: p.period_end, reverse=True)
        for parsed, seg in _chain_segments(conn, ext_id, stmts):
            if not statement_parser.segment_reconciles(seg):
                log.warning("statement for the period ending %s: segment does "
                            "not reconcile; not imported", parsed.period_end)
                skipped += 1
                continue
            imported += _import_statement(conn, ext_id, seg, seam)
    if imported:
        conn.commit()
    if imported or skipped:
        log.info("statements: imported %d tx before the export seam "
                 "(%d segment(s) skipped)", imported, skipped)
    return imported


# ============================================================
# Export cheques — the payee the statement prints
# ============================================================
#
# Both exports reduce a cheque to `CHECK <n>`. One the payee converted to an
# electronic debit prints on the statement as `Check # <n> <payee> Payment
# Arc ID: …`, so the statement names who was paid where the export does not,
# and the narrative is what categorises a row. The statement pass imports
# rows only below the export seam, so this pass reads the statements covering
# export-era cheques for that alone: a deposit export row whose narrative is
# a bare cheque takes the narrative of the one statement row with its number
# and amount, when that row names more than the cheque. A paper cheque, which
# the statement prints without a payee, keeps the export's narrative.
#
# The row's id carries no text (`deposit_content_key`), so the rewrite
# re-keys nothing, and a re-load never overwrites it (`_insert_transaction`
# ignores an id already present). `payload.description_basis` marks a
# narrative read off the statement, and `payload.cheque_statement_read` a row
# whose covering statement has been read, so each is read once; a row no
# statement covers yet is tried again on the next load.

_BARE_CHEQUE_RE = re.compile(r"^\s*CHECK\s*#?\s*(\d+)\s*$", re.I)
_STATEMENT_CHEQUE_RE = re.compile(r"^\s*Check\s*#\s*\d+\s*", re.I)


def _names_a_payee(description: str | None) -> bool:
    """Whether a statement cheque row's narrative says more than the cheque
    it is: text with a letter in it once the `Check # <n>` opener is gone."""
    rest = _STATEMENT_CHEQUE_RE.sub("", description or "", count=1)
    return any(ch.isalpha() for ch in rest)


def annotate_export_cheques(conn: sqlite3.Connection, bronze_dir: Path) -> int:
    """Give each export-era cheque the payee its statement prints, and
    return how many rows took one. Cheap when there is nothing to do: the
    statements are opened only for rows still owed a read."""
    card_ids = card_account_ids_in_tree(bronze_dir)
    todo = []
    for fitid, acct, posted, amount, check, desc, payload in conn.execute(
            "SELECT fitid, account_external_id, posted_at, amount, "
            "check_number, description, payload FROM transactions "
            f"WHERE {_EXPORT_SOURCES_SQL} AND upper(description) LIKE 'CHECK%'",
            EXPORT_SOURCES):
        m = _BARE_CHEQUE_RE.match(desc or "")
        if acct in card_ids or not m:
            continue
        body = _payload_dict(payload)
        if body.get("cheque_statement_read"):
            continue
        todo.append((fitid, posted, amount, (check or m.group(1)).strip(), body))
    if not todo:
        return 0
    days = [posted for _, posted, *_ in todo]
    pool = _deposit_statements_in_tree(
        bronze_dir, card_ids,
        lambda name_date: any(d <= name_date <= d + _STATEMENT_SPAN
                              for d in days))
    named = 0
    for fitid, posted, amount, number, body in todo:
        covering = [p for p in pool.values()
                    if _epoch_day(p.period_start) <= posted
                    <= _epoch_day(p.period_end)]
        if not covering:
            continue                    # its statement is not out yet
        matches = [t for p in covering for seg in p.segments
                   for t in seg.transactions
                   if (t.check_number or "").strip() == number
                   and _cents(t.amount) == _cents(amount)]
        body["cheque_statement_read"] = True
        description = None
        if len(matches) == 1 and _names_a_payee(matches[0].description):
            description = " ".join(matches[0].description.split())
            body["description_basis"] = "statement_pdf"
            named += 1
        conn.execute(
            "UPDATE transactions SET description = COALESCE(?, description), "
            "payload = ? WHERE fitid = ?",
            (description, silver.canonical_json(body), fitid))
    conn.commit()
    if named:
        log.info("statements: %d export cheque(s) took the payee their "
                 "statement prints", named)
    return named


# ============================================================
# Card statements — balances every era, transactions below the seam
# ============================================================
#
# A card statement is read for TWO independent things, on two different
# gates:
#
#   * its PERIOD BALANCES, which load for every era. The card export has no
#     running-balance column, so the statement's `Previous Balance` /
#     `New Balance` pair is the only balance a card ever asserts about the
#     past — and it is just as wanted for a period the export already covers
#     (it is what anchors that period's reconstruction) as for one it does
#     not. Gating them on the export seam would leave a card with a
#     transaction history and no continuous balance history.
#   * its TRANSACTIONS, which stay behind the per-account export seam exactly
#     as the deposit pass's do, so the two sources never cover the same day.
#     A card with no export loaded has no seam at all, and an absent seam is
#     not an open one: the statement era would be unbounded, so every period
#     would import and the first export to land would insert its own copy of
#     the same rows beside them. Such a card records its balances and imports
#     nothing until an export exists.
#
# The transaction gate is on the STATEMENT'S PERIOD, not on each row's date,
# and that is deliberate. A statement row prints the TRANSACTION date; which
# billing period it belongs to is decided by its POST date, which the
# statement never prints. Gating row by row would therefore let a row
# transacted just before the seam but posted just after it land twice — once
# from the statement and once from the export that also carries it — and a
# double-counted charge is exactly the error a spending ledger cannot
# tolerate. Requiring the whole period to close before the seam makes the
# overlap impossible (every row of such a statement posted on or before its
# period end), at the cost of the ONE part-period straddling the seam: its
# pre-seam rows are given up rather than guessed at. That hole cannot grow —
# the seam is a MIN over export rows already loaded — but it is real, so the
# pass warns about the period by name and stamps its `statement_balances` row
# `transactions_covered = 0`, which is how a consumer tells an anchored but
# unpopulated period from one whose rows are all there.
#
# `posted_at` on a statement-era card row is therefore the row's TRANSACTION
# date — a deliberate approximation, not a bug. It is the only date the
# document carries, it is what the export's own `Transaction Date` column
# agrees with, and the two eras never meet. It is also why a statement-era
# row carries NO per-row balance: see `_import_card_statement`.

def _latest_account_row(conn, account_external_id: str) -> dict:
    """The most recent roster observation for an account, as a dict of its
    promoted columns plus its payload fields. Empty when the account has
    never been observed."""
    row = conn.execute(
        "SELECT balance, pending_charges, payload FROM accounts "
        "WHERE account_external_id = ? ORDER BY snapshot_at DESC LIMIT 1",
        (account_external_id,)).fetchone()
    if row is None:
        return {}
    return {**_payload_dict(row[2]),
            "balance": row[0], "pending_charges": row[1]}


def _card_account_ids(conn) -> set[str]:
    """The card accounts silver's own roster claims. ONE of the two signals
    `derive_card_balances` requires — never sufficient on its own, because a
    roster is exactly the thing that goes missing or gets mis-stamped (which
    is why every other card/deposit routing decision reads file content
    instead)."""
    return {r[0] for r in conn.execute(
        "SELECT DISTINCT account_external_id FROM accounts WHERE product = ?",
        (PRODUCT_CARD,))}


def _insert_statement_balance(conn, account_external_id: str, snapshot_at: int,
                              stmt, transactions_covered: bool) -> None:
    """Record one card statement's period balances, and whether that period's
    transactions are in silver at all.

    Keyed on (account, period_end): the same period is re-downloaded by later
    runs and re-parsed on every load, so a period keeps the earliest copy
    passing the most parse gates — the one `_card_statements_in_tree`
    selects — and RE-RECORDS its figures and `snapshot_at` when a better copy
    displaces the held one. Leaving them on the first copy would let a period
    assert one copy's anchors while `transactions_covered` flagged another
    copy's rows as present, which is the unexplainable residual the flag
    exists to prevent. `transactions_covered` moves for a second reason: it
    is also a function of the export seam, which grows as more export history
    lands. Every field written is a deterministic function of the kept copy
    and that seam, so the conflict path is a no-op while neither changes."""
    conn.execute(
        "INSERT INTO statement_balances (account_external_id, "
        "period_start, period_end, opening, closing, snapshot_at, "
        "transactions_covered) VALUES (?,?,?,?,?,?,?) "
        "ON CONFLICT(account_external_id, period_end) DO UPDATE SET "
        "period_start = excluded.period_start, "
        "opening = excluded.opening, closing = excluded.closing, "
        "snapshot_at = excluded.snapshot_at, "
        "transactions_covered = excluded.transactions_covered",
        (account_external_id, _epoch_day(stmt.period_start),
         _epoch_day(stmt.period_end), float(stmt.previous_balance),
         float(stmt.new_balance), snapshot_at, int(transactions_covered)))


def _card_statement_fitid(account_external_id: str, period_end: int,
                          posted_at: int, amount: float,
                          description: str | None, occ: int) -> str:
    """Stable synthetic id for a card statement transaction.

    The BILLING PERIOD is part of the basis, unlike the deposit statement id.
    Two genuinely different transactions can share a transaction date, an
    amount and a descriptor and still be billed in different periods — a
    charge that posts late lands a cycle after an identical one — and the
    occurrence index cannot separate them, because it counts within one
    statement. Keying on the period does, and keeps the index per-statement
    and therefore stable: a statement's ids never move because another
    statement was added to the tree."""
    return _content_hash("stmt_", account_external_id, posted_at, amount,
                         description, f"{period_end}#{occ}")


def _import_card_statement(conn, account_external_id: str, stmt) -> int:
    """Insert a card statement's transactions, converted to silver's
    convention.

    The statement's signs are the INVERSE of the export's — it prints a
    purchase positive (it raises the balance owed) and a payment negative,
    where the export prints spend negative and anything reducing the balance
    owed positive — so every amount is negated here. Silver has one card
    convention, the export's, and this is the only place the other one
    exists. Balances are NOT negated: they stay the printed positive amount
    owed, matching `accounts.balance`.

    Each row keeps the section it was printed under as its `kind`
    (STMT_PURCHASE / STMT_PAYMENT / STMT_FEE / STMT_INTEREST), which is the
    statement era's only statement of what a row was.

    NO PER-ROW BALANCE IS WRITTEN, and re-adding one would be wrong. A
    statement does roll from its own `Previous Balance` to its `New Balance`,
    but it DATES its rows by transaction date while the cycle BILLS them by
    post date: a row transacted before its period opened is printed inside
    that period, carrying its balance, while sitting chronologically inside
    the previous one — which carries a different balance for the same days.
    Ordered by date the eras' two chains therefore contradict each other at
    every such row — and any charge transacted near a cycle boundary can be
    one, so those rows are structural rather than exceptional. The anchors in
    `statement_balances` carry the balance truth for this era instead, which
    is what the design says: the per-row reconstruction is for the export
    era, statement closing balances for the rest."""
    occ_seen: dict[tuple, int] = {}
    period_end = _epoch_day(stmt.period_end)
    n = 0
    for txn in stmt.transactions:
        posted = _epoch_day(txn.posted_at)
        amount = -float(txn.amount)
        desc = txn.description or None
        key = (posted, round(amount, 2), (desc or "").strip())
        occ = occ_seen.get(key, 0)
        occ_seen[key] = occ + 1
        _insert_transaction(conn, account_external_id, {
            "fitid": _card_statement_fitid(account_external_id, period_end,
                                           posted, amount, desc, occ),
            # The TRANSACTION date — the only date the document prints. It
            # fills `posted_at` for want of a post date, which is also why
            # this era carries no per-row balance (see above) and why the
            # transaction gate is per PERIOD rather than per row (see the
            # section comment).
            "posted_at": posted,
            "txn_date": posted,
            "amount": amount, "kind": txn.kind,
            "description": desc, "merchant": desc,
            "category": None, "currency": None, "check_number": None,
            "balance": None,
            "source": SOURCE_STATEMENT,
            "payload": {"description": desc, "basis": "statement_pdf",
                        "posted_at_basis": "transaction_date"},
        })
        n += 1
    return n


def _card_parse_rank(parsed) -> tuple[bool, bool]:
    """How much of a card statement parse holds together: (its summary block
    adds up, its printed rows carry the opening balance to the closing one).
    A tuple so the two gates order as (both) > (summary only) > (neither),
    which is the order `_card_statements_in_tree` keeps copies of one period
    in."""
    return (statement_parser.card_summary_reconciles(parsed),
            statement_parser.card_rows_reconcile(parsed))


def _card_statements_in_tree(bronze_dir: Path, card_ids: set[str]) -> dict:
    """Parse every card statement PDF in the bronze tree, deduped.

    Returns {account_external_id: {period_end date: (snapshot_at, parsed)}}.
    Bronze runs re-download overlapping months, so identical PDFs are skipped
    by content hash, and of the differing copies of one period the earliest
    one passing the most gates is kept (`_card_parse_rank`): a period whose
    first copy mis-rendered would otherwise be refused forever, since a
    later, clean re-download hashes differently and would be discarded behind
    it. Equal ranks keep the earlier run, so nothing moves without cause.
    Every era is parsed — unlike the deposit pass there is no filename
    prefilter, because the period balances of an export-covered month are
    wanted too.

    The content-hash skip is PER CARD, unlike the deposit pool's, because a
    card statement is a per-card document that is never combined (DESIGN.md
    §C): one sha under two card directories would be two cards' statements,
    not one document seen twice, and a pool-wide skip would give the second
    card no anchors at all."""
    out: dict[str, dict] = {}
    seen: set[tuple[str, str]] = set()
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        stmt_dir = run_dir / "statements"
        if not stmt_dir.is_dir():
            continue
        try:
            snapshot_at = bronze.parse_run_ts(run_dir.name)
        except ValueError:                                  # pragma: no cover
            continue
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            if acct_dir.name not in card_ids:
                continue
            for pdf in sorted(acct_dir.glob("*.pdf")):
                sha, _ = bronze.sha256_file(pdf)
                if (acct_dir.name, sha) in seen:
                    continue
                seen.add((acct_dir.name, sha))
                try:
                    parsed = statement_parser.parse_card_statement_pdf(pdf)
                except Exception as exc:        # pragma: no cover — env/poppler
                    log.warning("card statement %s: parse failed (%r); "
                                "skipping", pdf.name, exc)
                    continue
                if parsed.period_start is None or parsed.period_end is None:
                    log.warning("card statement %s: no Opening/Closing Date "
                                "parsed; skipping", pdf.name)
                    continue
                per = out.setdefault(acct_dir.name, {})
                held = per.get(parsed.period_end)
                if held is None or _card_parse_rank(parsed) > \
                        _card_parse_rank(held[1]):
                    # Stamped with the accepted copy's OWN run:
                    # `_insert_statement_balance` re-records the figures and
                    # this timestamp when a better copy displaces the held
                    # one.
                    per[parsed.period_end] = (snapshot_at, parsed)
    return out


def load_card_statements(conn: sqlite3.Connection, bronze_dir: Path) -> int:
    """Parse the card statement PDFs under the bronze tree into
    `statement_balances` (every era) and `transactions` (below the export
    seam only). Returns the number of transactions imported.

    Two independent self-checks decide what lands, and a statement can pass
    one and fail the other:

      * the SUMMARY IDENTITY (Previous + Payment,Credits + Purchases + Cash
        Advances + Balance Transfers + Fees + Interest == New Balance) gates
        the period balances — a summary block that does not add up is a
        mis-parse, and an anchor nobody can trust is worse than no anchor;
      * the ROW IDENTITY (Σ printed rows == New − Previous) gates the
        transactions — it is what proves the parse caught every row of the
        period and invented none.

    A card whose export has not landed yet has no seam, and an absent seam
    bounds nothing: it records the period balances and imports no
    transactions at all, because the export that eventually lands would put
    its own copy of the same rows beside them. The card heals on the load
    after its export arrives. A silver DB built before this gate can already
    hold such a card's statement rows and nothing here withdraws them: it
    must be rebuilt with `load --force` before that card's export lands.

    Every period balance recorded also records whether that period's
    TRANSACTIONS are in silver at all
    (`statement_balances.transactions_covered`). They are not for a card with
    no export seam yet, nor for the one period per card that straddles the
    seam, nor for a period whose row identity failed — and unflagged those
    periods look exactly like any other pair of anchors, so anyone
    reconciling transactions between two anchors is left with an
    unexplainable residual.

    Idempotent: balances upsert on (account, period_end), transactions
    INSERT OR IGNORE on a synthetic id deterministic in the parse."""
    card_ids = card_account_ids_in_tree(bronze_dir)
    if not card_ids:
        return 0
    seams = _export_seams(conn)
    by_acct = _card_statements_in_tree(bronze_dir, card_ids)

    imported = balances = skipped_summary = skipped_rows = straddled = 0
    unseamed = 0
    for ext_id, per_period in sorted(by_acct.items()):
        seam = seams.get(ext_id)
        if seam is None:
            unseamed += 1
            log.info("card statements: %s has no export rows yet, so nothing "
                     "bounds its statement era; its period balances are "
                     "recorded and its transactions wait for an export",
                     ext_id)
        for _, (snapshot_at, stmt) in sorted(per_period.items()):
            if not statement_parser.card_summary_reconciles(stmt):
                log.warning("card statement for the period ending %s: the "
                            "account summary does not add up; no balances "
                            "recorded", stmt.period_end)
                skipped_summary += 1
                continue
            if seam is None:
                # Nothing marks where the export's own rows would begin, so
                # importing here would double every row the first export
                # lands (see the section comment).
                covered = False
            elif _epoch_day(stmt.period_end) >= seam:
                # The export owns this period's rows — unless the period also
                # OPENED before the seam, in which case its pre-seam days
                # belong to neither source. That hole is the price of gating
                # per period rather than per row (see the section comment): it
                # is one period per card and cannot grow, but it is invisible
                # unless said out loud.
                covered = _epoch_day(stmt.period_start) >= seam
                if not covered:
                    given_up = sum(1 for t in stmt.transactions
                                   if _epoch_day(t.posted_at) < seam)
                    log.warning(
                        "card statements: the period %s - %s straddles the "
                        "export seam (%s), so the export owns it and none of "
                        "its statement rows are imported; ~%d row(s) dated "
                        "before the seam are given up rather than risk "
                        "double-counting one. Its balances are recorded with "
                        "transactions_covered = 0",
                        stmt.period_start, stmt.period_end, _iso_day(seam),
                        given_up)
                    straddled += 1
            elif not statement_parser.card_rows_reconcile(stmt):
                log.warning("card statement for the period ending %s: its "
                            "rows do not carry the opening balance to the "
                            "closing one; no transactions imported",
                            stmt.period_end)
                skipped_rows += 1
                covered = False
            else:
                covered = True
                imported += _import_card_statement(conn, ext_id, stmt)
            _insert_statement_balance(conn, ext_id, snapshot_at, stmt, covered)
            balances += 1
    if balances or imported:
        conn.commit()
    if balances or imported or skipped_summary or skipped_rows:
        log.info("card statements: %d period balance(s), %d tx before the "
                 "export seam (%d summary mis-parse(s), %d row mis-parse(s), "
                 "%d period(s) straddling a seam, %d card(s) with no export "
                 "yet)", balances, imported, skipped_summary, skipped_rows,
                 straddled, unseamed)
    return imported


# ============================================================
# The derived per-transaction card balance
# ============================================================
#
# Neither card export carries a running balance, so the one in silver is
# reconstructed. It is only ever reconstructed BETWEEN TWO ANCHORS the
# provider itself asserted — consecutive statement `New Balance` figures, and
# for the newest span the roster's live balance — and a span that does not
# land on its closing anchor is discarded whole rather than written as a
# plausible-looking series. Nothing here invents a balance: a span either
# reproduces the provider's own number or produces none.
#
# Only the EXPORT era gets a per-row balance at all. A statement-era row
# carries none (`_import_card_statement`): those rows are dated by
# transaction date, so no boundary drawn by post date cuts them where the
# statement did, and their era's balance truth is the period anchors
# themselves. A span is walked only when its whole window sits at or after
# the account's export seam, which by construction contains export rows and
# nothing else.

def _card_balance_anchors(conn, account_external_id: str) -> list[tuple]:
    """The account's statement closing balances as (period_end, closing)
    anchors, oldest first."""
    return [(int(pe), float(c)) for pe, c in conn.execute(
        "SELECT period_end, closing FROM statement_balances "
        "WHERE account_external_id = ? AND closing IS NOT NULL "
        "ORDER BY period_end", (account_external_id,))]


def _posted_ledger(conn, account_external_id: str, lo: int | None = None,
                   hi: int | None = None) -> list[tuple[str, float]]:
    """The account's export-sourced rows posted in (lo, hi] — either bound
    open when None — as (fitid, amount), in the deterministic order the roll
    forward walks: post date, then id. Intra-day order is arbitrary either
    way, and the end-of-day balance is exact regardless of it."""
    sql = (f"SELECT fitid, amount FROM transactions WHERE {_EXPORT_SOURCES_SQL} "
           "AND account_external_id = ?")
    params: list = [*EXPORT_SOURCES, account_external_id]
    if lo is not None:
        sql += " AND posted_at > ?"
        params.append(lo)
    if hi is not None:
        sql += " AND posted_at <= ?"
        params.append(hi)
    return [(r[0], float(r[1]))
            for r in conn.execute(sql + " ORDER BY posted_at, fitid", params)]


def _roll_forward(rows: list[tuple[str, float]],
                  opening: float) -> tuple[list[tuple[str, int]], int]:
    """Walk `rows` from `opening`, returning [(fitid, balance in cents)] and
    the closing balance in cents. Arithmetic is in integer cents so a span of
    several hundred rows cannot drift off its anchor by float noise.

    The export signs spend negative and anything reducing the balance owed
    positive, while the balance IS the amount owed — so the balance moves
    against the amount."""
    bal = _cents(opening)
    out = []
    for fitid, amount in rows:
        bal -= _cents(amount)
        out.append((fitid, bal))
    return out, bal


def _apply_derived_balances(conn, derived: dict[str, int | None]) -> None:
    """Write the reconstruction onto the account's rows.

    `derived` covers EVERY export row of the account, mapping the ones a
    landing span reached to their balance in cents and the rest to None. The
    pass is authoritative rather than additive for exactly that reason: an
    anchor arriving in a later run can turn a span that used to land into one
    that does not, and a balance left behind from the load before would then
    be a number no anchor stands behind any more. Each written balance marks
    its payload so the value is never read as a provider-supplied one; a
    cleared row loses the marker with the balance."""
    for fitid, cents in derived.items():
        row = conn.execute("SELECT payload FROM transactions WHERE fitid = ?",
                           (fitid,)).fetchone()
        payload = _payload_dict(row[0] if row else None)
        if cents is None:
            payload.pop("balance_basis", None)
        else:
            payload["balance_basis"] = BALANCE_BASIS_DERIVED
        conn.execute(
            "UPDATE transactions SET balance = ?, payload = ? WHERE fitid = ?",
            (None if cents is None else cents / 100.0,
             silver.canonical_json(payload), fitid))


def _derive_card_account_balances(conn, account_external_id: str,
                                  seam: int) -> tuple[int, int, int]:
    """Reconstruct one card's export-era running balance.

    Returns (rows derived, spans landed, spans discarded). Each consecutive
    pair of statement anchors at or after the seam bounds one span; the newest
    span runs from the last statement anchor to the roster's live balance.
    A span that does not land on its closing anchor keeps no balance at all
    and is logged — that mismatch is the canary for a ledger with a row
    missing, a row too many, or an anchor from a mis-parsed statement."""
    # Every export row starts unbalanced; a landing span fills its own in.
    derived: dict[str, int | None] = {
        fitid: None
        for fitid, _ in _posted_ledger(conn, account_external_id)}
    if not derived:
        return 0, 0, 0
    anchors = [a for a in _card_balance_anchors(conn, account_external_id)
               if a[0] >= seam]
    landed = discarded = 0
    for (lo, opening), (hi, closing) in itertools.pairwise(anchors):
        rows, end = _roll_forward(
            _posted_ledger(conn, account_external_id, lo, hi), opening)
        if end != _cents(closing):
            log.warning("card balance: the ledger between the statements "
                        "ending %s and %s does not carry the first's closing "
                        "balance to the second's; that span keeps no derived "
                        "balance", _iso_day(lo), _iso_day(hi))
            discarded += 1
            continue
        derived.update(rows)
        landed += 1

    if anchors:
        last_end, last_closing = anchors[-1]
        tail, end = _roll_forward(
            _posted_ledger(conn, account_external_id, last_end), last_closing)
        if tail:
            if _tail_lands(conn, account_external_id, end):
                derived.update(tail)
                landed += 1
            else:
                discarded += 1
    _apply_derived_balances(conn, derived)
    return sum(v is not None for v in derived.values()), landed, discarded


def _tail_lands(conn, account_external_id: str, end_cents: int) -> bool:
    """Whether the newest span reconciles with the roster.

    The roster balance is the LIVE amount owed, and it includes activity that
    has not posted yet — which the exports never carry — so the posted ledger
    legitimately trails it. `pending_charges` is exactly that difference, so
    where the roster reported it the gap becomes an identity
    (posted + pending == live) and is checked like any other anchor. Where it
    did not, there is nothing to check the gap against: the span is kept (its
    opening anchor is a statement figure and the roll forward is arithmetic)
    and the implied pending amount is logged UNCONDITIONALLY — including when
    it is zero. Logging only a non-zero gap would make the identity
    observable exactly when it does not hold, so the run that first carries
    the pending field could not be told from the runs that never checked."""
    roster = _latest_account_row(conn, account_external_id)
    live = parse_money(roster.get("balance"))
    if live is None:
        return True                     # no live figure to land on
    pending = parse_money(roster.get("pending_charges"))
    if pending is None:
        log.info("card balance: the newest span lands %.2f below the live "
                 "balance; the roster reported no pending amount to check "
                 "that gap against", (_cents(live) - end_cents) / 100.0)
        return True
    if end_cents + _cents(pending) == _cents(live):
        return True
    log.warning("card balance: the newest span plus the reported pending "
                "amount does not reach the live balance (off by %.2f); that "
                "span keeps no derived balance",
                (end_cents + _cents(pending) - _cents(live)) / 100.0)
    return False


def derive_card_balances(conn: sqlite3.Connection,
                         bronze_dir: Path) -> tuple[int, int, int]:
    """Reconstruct `transactions.balance` for every card's export-era rows.
    Returns (rows derived, spans landed, spans discarded).

    Idempotent — it recomputes the whole reconstruction from the anchors on
    every load rather than tracking which rows already carry one, which is
    also what lets an anchor arriving later withdraw a balance the run before
    had derived.

    The account set is the INTERSECTION of two independent signals: silver's
    roster (`_card_account_ids`) and the export files' own content
    (`card_export_ids_in_tree`). Both are required because this is the ONE
    destructive pass in the loader — it rewrites `transactions.balance`
    authoritatively, clearing every row a landing span did not reach — and a
    deposit row's balance is the provider's own CSV column, which nothing can
    restore. A roster row alone must never authorise that (a mis-stamped or
    corrupted `accounts.product` would otherwise wipe a deposit account's
    provider balances), and neither must a card-shaped export alone. Where
    the two disagree, nothing is rewritten."""
    seams = _export_seams(conn)
    cards = sorted(_card_account_ids(conn)
                   & card_export_ids_in_tree(bronze_dir)
                   & seams.keys())
    rows = landed = discarded = 0
    for ext_id in cards:
        r, ok, bad = _derive_card_account_balances(conn, ext_id, seams[ext_id])
        rows += r
        landed += ok
        discarded += bad
    if cards:
        conn.commit()
    if rows or discarded:
        log.info("card balances: %d row(s) derived over %d span(s) "
                 "(%d span(s) discarded)", rows, landed, discarded)
    return rows, landed, discarded


# ============================================================
# Per-run loader
# ============================================================

def _read_text(path: Path, limit: int | None = None) -> str:
    """A bronze text artefact, or "" when it isn't there. `limit` caps the
    read at that many characters."""
    if not path.is_file():
        return ""
    with path.open(encoding="utf-8", errors="replace") as fh:
        return fh.read() if limit is None else fh.read(limit)


def _export_pairs(tx_dir: Path, limit: int | None = None):
    """Yield (ext_id, qfx text, csv text) per exported account, sorted. An
    account exported in only one format yields "" for the other, so the merge
    still runs on what is there. `limit` reads only that many leading
    characters of each file — enough for `is_card_export`, which looks at the
    CSV's header line and the QFX's message-set tag and nothing else."""
    for ext_id in sorted({p.stem for p in tx_dir.glob("*.qfx")}
                         | {p.stem for p in tx_dir.glob("*.csv")}):
        yield (ext_id,
               _read_text(tx_dir / f"{ext_id}.qfx", limit),
               _read_text(tx_dir / f"{ext_id}.csv", limit))


# The message-set tag sits behind the OFX header and the signon block, a few
# hundred characters in; the CSV header is the first line. 8 KiB is generous
# for both and keeps the routing probe off the multi-megabyte read a whole
# bronze tree's exports would otherwise cost.
_ROUTE_PROBE_CHARS = 8192


def card_export_ids_in_tree(bronze_dir: Path) -> set[str]:
    """Every external id whose EXPORT FILES are card-shaped, anywhere in the
    bronze tree — the file-content signal on its own, with no roster claim
    folded in.

    `is_card_export` is the same test `load_run` routes an export pair on, so
    an account is in this set exactly when its silver rows were keyed by the
    card path. That is what makes it the right second signal for
    `derive_card_balances`, whose rewrite must never be authorised by a
    roster claim: a roster is a per-run assertion that can go missing or
    arrive mis-stamped, while the export file either has the card's header
    and message set or it does not."""
    ids: set[str] = set()
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        tx_dir = run_dir / "transactions"
        if tx_dir.is_dir():
            ids |= {ext for ext, qfx, csv in
                    _export_pairs(tx_dir, limit=_ROUTE_PROBE_CHARS)
                    if is_card_export(qfx, csv)}
    return ids


def card_account_ids_in_tree(bronze_dir: Path) -> set[str]:
    """Every external id that is a card account anywhere in the bronze tree:
    the file-content signal (`card_export_ids_in_tree`) plus every run's
    roster.

    This is the ROUTING set — which statement PDFs go to the card parser
    rather than the deposit one — and there the union is the safe direction.
    A run whose accounts.json is missing or unreadable used to feed a card's
    statement PDFs to the deposit parser, which has a different period line, a
    different sign convention and no card in its balance chain; folding each
    run's roster in as a second POSITIVE signal also catches a card whose
    export failed in every run present. Mis-routing a statement costs a parse,
    and the file signal is all but sufficient on its own anyway.

    `derive_card_balances` deliberately does NOT use this set: it rewrites
    balances, so there a wrong answer costs data and the two signals must
    AGREE rather than either being enough."""
    ids = card_export_ids_in_tree(bronze_dir)
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        ids |= card_account_ids(_read_accounts(run_dir))
    return ids


def load_run(conn: sqlite3.Connection, run_dir: Path) -> bool:
    """Load one bronze run dir. Returns True if ingested, False if skipped
    (non-complete dump, or already loaded). Idempotent."""
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

    # EVERY roster record is ingested, including one from a run whose
    # run.json scores its product's coverage short. DO NOT re-add a coverage
    # gate here. Two reasons:
    #
    #   * Silver ingest is purely ADDITIVE. `accounts` is monotemporal (a new
    #     snapshot row per run, PK (snapshot_at, account_external_id)),
    #     deposit transactions and documents are INSERT OR IGNORE on a stable
    #     key, and the statement pass only ever inserts. The card ledger's
    #     upsert is additive too: it inserts a new id, and on an id already
    #     stored it writes only columns the incoming export both is
    #     authoritative for AND actually carries, so a partial export fills
    #     gaps and never blanks them. Nothing here removes or zeroes anything,
    #     and `--force` deletes the whole DB and replays all of bronze rather
    #     than pruning part of it. A partial run therefore just lands fewer
    #     rows, and the next run completes them — withholding a snapshot buys
    #     no protection, it only loses an observation.
    #   * Withholding actively broke things. Coverage is composite: it also
    #     requires the statement pass, so ONE flaky statement PDF withheld the
    #     whole accounts snapshot — and balances come from the roster, not from
    #     statements, so gold went on projecting the previous run's balance
    #     indefinitely. Worse, a snapshot could be withheld while the product's
    #     transactions were still admitted, and the gold adapter classifies a
    #     transaction by looking its account's product up in `accounts`: with
    #     no roster row there, a card's spend projected into the deposit
    #     ledger.
    #
    # Closure semantics belong to GOLD, which is where "this observation is the
    # complete set" is actually decided (it merges per account, newest
    # observation wins, absence never wins). run.json's `coverage` block only
    # records which product fell short; nothing gates on it.
    accounts = _read_accounts(run_dir)
    for acct in accounts:
        _insert_account(conn, snapshot_at, acct)

    tx_dir = run_dir / "transactions"
    drifted: list[str] = []
    if tx_dir.is_dir():
        for ext_id, qfx_text, csv_text in _export_pairs(tx_dir):
            if is_card_export(qfx_text, csv_text):
                csv_rows = parse_card_csv(csv_text)
                # The card ledger's own writer: each export is authoritative
                # over columns the other does not carry, so a row already in
                # silver is repaired rather than ignored.
                for tx in merge_card_transactions(ext_id, parse_qfx(qfx_text),
                                                  csv_rows):
                    _upsert_card_transaction(conn, ext_id, tx)
            else:
                csv_rows = parse_csv(csv_text)
                for tx in merge_transactions(ext_id, parse_qfx(qfx_text),
                                             csv_rows):
                    _insert_transaction(conn, ext_id, tx)
            if _csv_yielded_nothing(csv_text, csv_rows):
                # Not "the header has drifted": a value-level drift — a post
                # date printed DD/MM/YYYY — parses to nothing just the same,
                # off a header that is perfectly good.
                log.warning("%s: the export CSV carries data lines but parsed "
                            "to no rows — its header or its date format has "
                            "drifted, so this run's ledger for it is QFX-only",
                            ext_id)
                drifted.append(ext_id)

    stmt_dir = run_dir / "statements"
    if stmt_dir.is_dir():
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            for pdf in sorted(acct_dir.glob("*.pdf")):
                _insert_document(conn, snapshot_at, acct_dir.name, pdf)

    # "This run is ingested" and "the tree-wide post passes are owed" commit
    # TOGETHER. The passes are owed by a single flag rather than a marker per
    # run, and a flag written after this commit — even one statement later —
    # leaves a window in which an interrupt strands a run permanently
    # ingested with the passes never owed and nothing left able to notice.
    _write_post_passes_pending(conn, True)
    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version,"
        " run_dir) VALUES (?,?,?)",
        (snapshot_at, silver.current_schema_version(conn), str(run_dir)))
    conn.commit()
    log.info("loaded %s (%d accounts%s)", run_dir.name, len(accounts),
             f", {len(drifted)} export CSV(s) parsed to no rows"
             if drifted else "")
    return True


def _read_accounts(run_dir: Path) -> list[dict]:
    path = run_dir / "accounts.json"
    if not path.is_file():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except ValueError:
        log.warning("%s: accounts.json is not valid JSON; skipping", run_dir.name)
        return []
    if isinstance(data, dict):
        data = data.get("accounts", [])
    return [a for a in data if isinstance(a, dict)]


def _refuse_pre_0002_ids(conn, db_path: Path) -> None:
    """Refuse an incremental load into a silver DB whose export rows still
    carry migration 0001's FITID-derived ids.

    Migration 0002 moved both ledgers onto content-derived ids
    (`<product>:<ext>:<key>:<occ>`) and no migration rewrites the rows already
    stored — ids are loader output, not schema, so `schema_meta` cannot tell
    the two eras apart. INSERT OR IGNORE keys on `fitid` and cannot see that a
    bare FITID and its content-derived id are the same event, so an ordinary
    load would insert every one of those rows a second time and permanently
    double the ledger every spending figure is computed from.

    Checked after the migrations (so `transactions` exists on a first-ever
    load) and outside them (so it still catches a DB that already absorbed
    one bad incremental load). `--force` clears the DB before this runs, which
    is why it is also the repair."""
    stale = conn.execute(
        f"SELECT COUNT(*) FROM transactions WHERE {_EXPORT_SOURCES_SQL} "
        f"AND fitid NOT LIKE '{PRODUCT_DEPOSIT}:%' "
        f"AND fitid NOT LIKE '{PRODUCT_CARD}:%'", EXPORT_SOURCES).fetchone()[0]
    if stale:
        raise SystemExit(
            f"{db_path}: {stale} export row(s) still carry pre-0002 "
            "FITID-derived ids. An incremental load would insert every one of "
            "them again under its content-derived id. Re-run with --force "
            "(silver rebuilds from bronze), then reload gold.")


def _post_passes_pending(conn) -> bool:
    """Whether a previous load left the three tree-wide post passes unfinished
    (`loader_state`, migration 0004)."""
    row = conn.execute(
        "SELECT post_passes_pending FROM loader_state").fetchone()
    return bool(row and row[0])


def _write_post_passes_pending(conn, pending: bool) -> None:
    """Record whether the post passes still owe a run, without committing.

    An upsert on the singleton row rather than a bare UPDATE, so a table
    somehow without its seed row re-seeds itself instead of silently
    swallowing every write and leaving the passes ungated forever.

    Uncommitted, so `load_run` can raise the flag in the same transaction
    that lands the run."""
    conn.execute(
        "INSERT INTO loader_state (id, post_passes_pending) VALUES (1, ?) "
        "ON CONFLICT(id) DO UPDATE SET "
        "post_passes_pending = excluded.post_passes_pending",
        (int(pending),))


def _set_post_passes_pending(conn, pending: bool) -> None:
    """Record whether the post passes still owe a run, and COMMIT it.

    The commit is the point for the CLEARING side: the flag is cleared in a
    transaction of its own, after all three passes have returned, so a crash
    inside one of them leaves the flag raised rather than rolling it back
    together with that pass's own writes. Raising it is `load_run`'s job and
    commits with the run (`_write_post_passes_pending`)."""
    _write_post_passes_pending(conn, pending)
    conn.commit()


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (holds the <UTC-ts>/ run dirs).")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite path. Default: <bronze-dir>/chase.db.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)

    db_path = args.silver_db or (args.bronze_dir / "chase.db")
    if args.force:
        silver.reset(db_path)
    conn = silver.open_db(db_path)
    try:
        silver.apply_migrations(conn, MIGRATIONS_DIR)
        _refuse_pre_0002_ids(conn, db_path)
        # The three tree-wide post passes below are owed by a single flag
        # rather than a marker per run, and `load_run` raises it in the same
        # transaction that lands the run — so there is no instant at which a
        # run is ingested and the passes are not yet owed, whichever run an
        # interrupt lands on.
        loaded = 0
        for run_dir in bronze.iter_run_dirs(args.bronze_dir):
            if load_run(conn, run_dir):
                loaded += 1
        # After the export runs are in, backfill the pre-export tail from the
        # statement PDFs (seam anchored to the export rows just loaded), then
        # reconstruct the card running balance from the anchors that pass
        # just recorded.
        #
        # The flag is cleared only once all three have returned, so a load
        # that dies inside one of them — a poppler failure, an interrupt —
        # re-runs them on the next ordinary load, where before they were gated
        # on `loaded` alone and a tree with nothing new to ingest skipped them
        # forever, leaving silver permanently without its pre-export backfill
        # and its reconstructed card balances. A clean load leaves the flag
        # down, so a no-op reload still skips the PDF parsing they cost.
        # A moved statement parser owes the passes a run just as an
        # unfinished load does, and it is the case nothing else would raise:
        # a parser-only change lands no bronze run, so the pending flag stays
        # down and silver would go on holding rows no parser in the tree
        # produces.
        reparse = silver.stale_generation(conn, STATEMENT_GENERATION_SCOPE,
                                          STATEMENT_GENERATION)
        if _post_passes_pending(conn) or reparse:
            if reparse:
                log.info("the statement parser has changed since silver was "
                         "written; re-deriving the statement rows")
            elif not loaded:
                log.info("the previous load's post passes did not finish; "
                         "re-running them")
            before = _statement_row_count(conn)
            _purge_stale_statement_rows(conn, reparse)
            load_statement_transactions(conn, args.bronze_dir)
            load_card_statements(conn, args.bronze_dir)
            derive_card_balances(conn, args.bronze_dir)
            after = _statement_row_count(conn)
            if reparse and after < before:
                # The re-derivation came back short: a PDF that no longer
                # parses, a segment that stopped reconciling, a bronze tree
                # thinned since. Leaving the generation unstamped is what
                # makes the NEXT load try again instead of committing the
                # shortfall as the new truth — and it settles after that one
                # retry, which has nothing better to compare against.
                log.warning(
                    "statements: re-derivation produced %d row(s) where "
                    "silver held %d; not stamping the parser generation, so "
                    "the next load re-derives again", after, before)
            else:
                # Stamped only once all three have returned, so a pass that
                # dies leaves the older generation stored and the next load
                # re-derives.
                silver.stamp_generation(conn, STATEMENT_GENERATION_SCOPE,
                                        STATEMENT_GENERATION)
            _set_post_passes_pending(conn, False)
        # On every load, not only when the passes above are owed: a cheque
        # can reach the export before the statement that names its payee
        # does, and the pass reads nothing once every cheque is settled.
        annotate_export_cheques(conn, args.bronze_dir)
        log.info("done: %d run(s) ingested into %s", loaded, db_path)
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
