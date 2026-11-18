"""The ubs-web statement ledger: each movement a statement prints, and
each movement an advice states, under the ids the load gives them."""

from __future__ import annotations

import hashlib
import logging
import sqlite3

import landmarks as ubs  # local module: the export's shared vocabulary
from load_rows import _TXN_INSERT, _TXN_NOT_OLDER

log = logging.getLogger("ubs-web.load")


def _stmt_batch_txn_id(account: str, r: dict) -> str:
    """Content-stable transaction id for the MOVEMENT a PDF row came
    from. Same booking on the monthly AND the annual statement (and in
    a statement's post-closing trailer) hashes identically, so the
    ON CONFLICT upsert dedups the overlap; the per-statement occurrence
    index keeps genuinely-distinct identical same-day bookings apart.

    A split bundle's legs all hash to this same value — they are the
    one movement the statement printed — which is what lets a leg name
    the movement a statement printed whole, and the UBS adapter keep the
    legs in its place."""
    parts = "|".join(str(x) for x in (
        account, r["booking_date"], r["value_date"],
        # A leg carries its own share in amount_debit/amount_credit;
        # the movement is identified by the batch total the statement
        # printed, so the hash reads that where a leg has one.
        r.get("multi_parent_debit", r.get("amount_debit")),
        r.get("multi_parent_credit", r.get("amount_credit")),
        r.get("description_kind") or "", r.get("occurrence", 0),
    ))
    return "stmt:" + hashlib.sha256(parts.encode("utf-8")).hexdigest()[:16]


def _stmt_txn_id(account: str, r: dict) -> str:
    """The row's own id: the movement id, suffixed with the leg's
    position when the movement was a bundle the parser split. An
    unsplit row keeps the bare movement id: the id a statement that
    prints the movement whole gives it, and the one a leg
    (`<id>#<n>`) names."""
    tid = _stmt_batch_txn_id(account, r)
    leg = r.get("multi_leg_index")
    return f"{tid}#{leg}" if leg else tid


def _insert_hist_transactions(conn: sqlite3.Connection, snapshot_at: int,
                              rows: list[dict]) -> tuple[int, int]:
    """Insert PDF-derived movement rows into the silver `transactions`
    table under their content ids (`_stmt_txn_id`), so the same booking
    printed on two statements is one row. Returns (rows_inserted,
    statements_rejected).

    A statement whose movements failed the running-balance
    reconciliation is rejected wholesale (its column assignment is
    not trustworthy)."""
    if not rows:
        return 0, 0
    if not rows[0].get("reconciled", False):
        return 0, 1  # whole statement rejected
    inserted = 0
    for r in rows:
        account = ubs.iban_canonical(r.get("account_external_id"))
        if not account:
            continue  # non-cash / non-IBAN statement header
        txn_id = _stmt_txn_id(account, r)
        try:
            conn.execute(
                _TXN_INSERT +
                "DO UPDATE SET "
                "snapshot_at = excluded.snapshot_at, "
                "booking_date = excluded.booking_date, "
                "value_date = excluded.value_date, "
                "currency_iso = excluded.currency_iso, "
                "amount_debit = excluded.amount_debit, "
                "amount_credit = excluded.amount_credit, "
                "counterparty = excluded.counterparty, "
                "description_kind = excluded.description_kind, "
                "payload = excluded.payload "
                + _TXN_NOT_OLDER,
                (
                    txn_id, account, snapshot_at,
                    None,  # trade_date: no statement equivalent
                    r["booking_date"], r["value_date"],
                    r.get("currency_iso") or "",
                    r.get("amount_debit"), r.get("amount_credit"),
                    r.get("counterparty"), r.get("description_kind"),
                    r.get("payload") or "{}",
                ),
            )
            inserted += 1
        except sqlite3.IntegrityError as e:
            log.debug("hist transaction insert failed: %s", e)
    return inserted, 0


def _insert_advice_transactions(conn: sqlite3.Connection, snapshot_at: int,
                                rows: list[dict]) -> int:
    """Insert the movement a Credit/Debit Advice states into the silver
    `transactions` table, under UBS's own transaction number. Returns the
    number of rows actually written.

    Why these rows carry no minted id. The advice prints the very
    "Transaction no." the CSV export puts in the id column, and UBS stamps
    BOTH sides of an inter-account transfer with one number — which is
    precisely why silver's primary key is the compound
    (transaction_external_id, account_external_id) in the first place
    (migration 0001). Storing the advice's leg under the bare number and
    its OWN account is therefore not a new id scheme but the shape that key
    was designed to hold: the leg the export never saw lands beside the leg
    it did, and a consumer that pairs legs on a shared transaction number
    finds both.

    Why ON CONFLICT DO NOTHING rather than DO UPDATE. An advice states four
    facts — direction, amount, currency and the two dates — and nothing
    else: no booking type, no counter account, no trade date. The export
    states all of those for the same movement, under the same key. An
    upsert would overwrite a full row with a thinner one every time the
    document pass ran, and the loss would be invisible. So the export's
    row stays. Re-derivation when the parser moves comes from the
    pass-level delete instead (`_DOCUMENT_PASS_DELETES`), which drops the
    advice rows by their payload marker so the next run writes them afresh
    — an export row, having no such marker, is not touched.

    Every other advice is written, also where a statement prints the same
    movement under its own minted id: which record of the booking gold
    keeps is the UBS adapter's decision, made where every era is in hand.
    """
    inserted = 0
    for r in rows:
        account = ubs.iban_canonical(r.get("account_external_id"))
        txn_id = r.get("transaction_external_id")
        if not account or not txn_id:
            continue
        cur = conn.execute(
            _TXN_INSERT +
            "DO NOTHING",
            (
                txn_id, account, snapshot_at,
                None,  # trade_date: an advice prints no trade date
                r["booking_date"], r["value_date"],
                r.get("currency_iso") or "",
                r.get("amount_debit"), r.get("amount_credit"),
                r.get("counterparty"), r.get("description_kind"),
                r.get("payload") or "{}",
            ),
        )
        # 0 when the key already holds a row (the export's, or this
        # advice's from an earlier walk); 1 when the advice wrote a row
        # of its own.
        if cur.rowcount > 0:
            inserted += 1
    return inserted
