"""The ids the ubs-web load gives export rows: one bank number on
several movements (`_assign_export_txn_ids`), and the portfolio
transaction list's ids and accounts."""

from __future__ import annotations

import hashlib
import json
import logging
import re
import sqlite3

log = logging.getLogger("ubs-web.load")


def portfolio_account_canonical(product: str | None) -> str | None:
    """Canonical safekeeping-account id from the export's "Product".

    The export writes the spaced, dotted form (`1234 00000001.S5`);
    the PSN feed keys the same account `12340000000001S5`,
    zero-padding the middle group to ten digits. Both feeds naming one
    account the same way is what lets the adapter tell a trade it
    already holds from a new one, so a shape this cannot convert
    returns None rather than a near-miss id."""
    if not product:
        return None
    m = _PRODUCT_RE.fullmatch(product.strip())
    if not m:
        return None
    head, body, suffix = m.group(1), m.group(2), m.group(3)
    return f"{head}{body.zfill(10)}{suffix}"


_PRODUCT_RE = re.compile(r"(\d{4})\s+(\d{1,10})\.([A-Z0-9]{1,6})")
_PORTFOLIO_RE = re.compile(r"(\d{4})\s+(\d{1,10})\s+([A-Z0-9]{1,6})")


def portfolio_id_canonical(portfolio: str | None) -> str | None:
    """Canonical portfolio id from the export's "Portfolio" column.

    `1234 00000001 0006` is the same identifier the sibling feed keys
    portfolios by as `1234000000010006`. Unlike an account id (above),
    a portfolio id is NOT zero-padded: the two identifier spaces look
    alike and pad differently, and a padded portfolio id joins to
    nothing."""
    if not portfolio:
        return None
    m = _PORTFOLIO_RE.fullmatch(portfolio.strip())
    if not m:
        return None
    return "".join(m.groups())


def portfolio_txn_id(account: str, row: dict) -> str:
    """Stable id for one export row.

    Every actual trade carries UBS's own "External reference", which is
    what the id is built from. Corporate actions and FX legs are
    published without one, so those hash the columns the bank does fill
    — enough of them that two distinct bookings of the same type on the
    same security and day do not collide.

    Only columns that state the BOOKING may enter the hash. The
    valuation is not one of them: each scope values the same booking in
    its own reporting currency, so "Trans. value" (and the currency
    beside it) differ between a portfolio's own export and a
    consolidated view's, and hashing them would give one booking two
    ids — which defeats the dedupe below and lets a trade reach the
    ledger twice."""
    ref = (row.get("External reference") or "").strip()
    if ref:
        return f"ptx:{ref}"
    parts = "|".join(str(row.get(k, "") or "") for k in (
        "Product", "Trade date", "Trade time", "Booking", "Value date",
        "Description 1", "Description 2", "Valor", "ISIN",
        "Number/Amt.", "Order no.",
    ))
    digest = hashlib.sha256(f"{account}|{parts}".encode()).hexdigest()
    return f"ptx:{digest[:16]}"


def names_a_portfolio(portfolio_external_id: str | None) -> bool:
    """Whether an id names one of the numbered portfolios.

    UBS files consolidated views beside the real portfolios and offers
    them in the same chooser. A consolidated view reports every row it
    covers under its own id, which is lettered rather than numbered
    (`…R001`) and belongs to no portfolio — so a row carrying one
    states which custody account moved but not which cash account
    settled, and the pair that answers that is portfolio + currency."""
    return bool(portfolio_external_id) and portfolio_external_id.isdigit()


# UBS's "Transaction no." is not one per movement. It is one per
# BOOKING EVENT as the bank models it, and the bank sometimes models
# several movements as one:
#
#   * a deposit product (a call deposit, a fixed-term deposit) stamps
#     every increase, decrease, repayment and monthly interest payment
#     with the number derived from the product's own serial, so the
#     whole life of the product shares ONE number;
#   * a cross-border payment carries the correspondent bank's
#     third-party charge under the number of the payment it belongs to,
#     so a payment and its fee share one number.
#
# Keyed on the bare number, the rows of such a group would overwrite
# each other, and an ON CONFLICT upsert cannot tell that from a re-load
# of the same row: no parse fails, nothing is logged, and the survivor
# looks like a complete account. A deposit product's whole ledger
# reduces to whichever row the export printed last.
#
# So the ids follow the statement era, which suffixes the rows of a
# split movement (`_stmt_txn_id`). One row of the group keeps
# UBS's bare number — the advice pass needs it, since an advice is
# keyed by the number that carries it to its twin leg (see
# `_DOCUMENT_PASS_DELETES` in documents_load.py) — and the rest take a
# suffix.
#
# Which row keeps it is decided against silver, not against the dump.
# A row that already holds the bare number keeps it, whatever else this
# window carries; a group whose holder this window does not cover
# leaves that number alone and suffixes every member present; and only
# a group nobody holds yet picks, there by largest absolute amount, so
# the PAYMENT takes the number and its fee the suffix, which is the
# pairing the advice pass wants (`_bare_number_holders`).
#
# The suffix is derived from the row's own content rather than its
# position in the file — bar the ordinal that separates two rows
# identical in every movement field — so a dump whose window covers a
# different slice of the same group still mints the same ids.


def _export_row_fingerprint(row: dict) -> str:
    """The movement fields that distinguish two rows sharing a
    transaction number. Raw cell text, not parsed values, so a
    formatting change in how an amount is rendered cannot move an id."""
    return "|".join((_export_movement_fingerprint(row), row["description1"]))


def _export_movement_fingerprint(row: dict) -> str:
    """`_export_row_fingerprint` without the name the row is booked
    under. The bank restates a security's name on rows it has already
    exported (a company renamed, a share class retitled), so the same
    movement can come back under a different Description1; this is what
    still recognises it."""
    return "|".join((
        row["booking_date_raw"], row["value_date_raw"],
        row["debit_raw"], row["credit_raw"],
        row["description_kind"] or "",
    ))


def _bare_number_holders(conn: sqlite3.Connection,
                         rows: list[dict]) -> dict[tuple[str, str], dict]:
    """(account, transaction no.) -> the movement fields of the row
    silver ALREADY holds under the bare number, for the groups this dump
    touches.

    Which row of a group keeps the bank's bare number cannot be decided
    from the dump alone. UBS clamps its transactions UI and the
    collector's own `--lookback` is a window, so a later run routinely
    sees only PART of a group — and a rule that picks the largest member
    of whatever it can see hands the number to a different row each
    time. That is not a cosmetic churn: the row holding the number is
    overwritten with the newcomer's data and the newcomer is stored a
    second time under its suffix, which is the silent loss this whole id
    scheme exists to prevent, arriving by the back door.

    So the question is asked of the union of what is stored and what
    arrived: whoever holds the number keeps it, and only a group with no
    holder yet picks one. Advice rows are ignored — they carry the bare
    number by design (see `_insert_advice_transactions`) and state four
    facts where an export row states all of them, so an export row takes
    the number from one rather than yielding to it."""
    keys = {(r["account_external_id"], r["txn_no"]) for r in rows}
    if not keys:
        return {}
    held: dict[tuple[str, str], dict] = {}
    accounts = sorted({a for a, _ in keys})
    numbers = sorted({n for _, n in keys})
    q = ("SELECT account_external_id, transaction_external_id, payload"
         "  FROM transactions"
         f" WHERE account_external_id IN ({','.join('?' * len(accounts))})"
         f" AND transaction_external_id IN ({','.join('?' * len(numbers))})")
    for account, _txn_no, payload in conn.execute(q, (*accounts, *numbers)):
        key = (account, _txn_no)
        if key not in keys:
            continue
        try:
            cells = json.loads(payload)
        except (TypeError, ValueError):
            continue
        if not isinstance(cells, dict) or "payment_advice_pdf" in str(payload):
            continue
        held[key] = {
            "booking_date_raw": (cells.get("Booking date") or "").strip(),
            "value_date_raw": ((cells.get("Value date") or "").strip()
                               or (cells.get("Trade date") or "").strip()),
            "debit_raw": (cells.get("Debit") or "").strip(),
            "credit_raw": (cells.get("Credit") or "").strip(),
            "description_kind": (cells.get("Description2") or "").strip() or None,
            "description1": (cells.get("Description1") or "").strip(),
        }
    return held


def _assign_export_txn_ids(rows: list[dict],
                           held: dict[tuple[str, str], dict] | None = None) -> list[str]:
    """Return one id per row, positionally aligned with `rows`.

    A transaction number used once is its row's id unchanged, which is
    every row the export has ever loaded but the collided ones."""
    held = held or {}
    groups: dict[tuple[str, str], list[int]] = {}
    for i, row in enumerate(rows):
        groups.setdefault((row["account_external_id"], row["txn_no"]), []).append(i)

    ids: list[str] = [""] * len(rows)
    for key, members in groups.items():
        txn_no = key[1]
        if len(members) == 1 and key not in held:
            ids[members[0]] = txn_no
            continue
        # Whoever already holds the bare number keeps it, whatever else
        # this dump happens to carry. Only a group nobody holds yet
        # picks one, and then the largest movement takes it: the advice
        # pass is keyed by the number and an advice names a payment
        # rather than the fee beside it.
        holder = held.get(key)
        incumbent = [i for i in members if holder is not None
                     and _export_row_fingerprint(rows[i])
                     == _export_row_fingerprint(holder)]
        if not incumbent and holder is not None:
            # The holder may have come back under a restated name. The
            # one member whose movement is the holder's IS the holder;
            # suffixed, it would be stored a second time beside itself.
            # Two such members cannot be told apart, so neither claims.
            restated = [i for i in members
                        if _export_movement_fingerprint(rows[i])
                        == _export_movement_fingerprint(holder)]
            if len(restated) == 1:
                incumbent = restated
        if incumbent:
            primary = incumbent[0]
        elif key in held:
            # The holder is a row this window does not cover. It is not
            # ours to move, and nothing here may take its number.
            primary = None
        else:
            primary = max(members, key=lambda i: (
                abs(rows[i]["amount_debit"] or 0.0) + abs(rows[i]["amount_credit"] or 0.0),
                _export_row_fingerprint(rows[i]),
            ))
        # Two rows of a group that are identical in every movement
        # field hash alike; an ordinal keeps them apart rather than
        # letting one eat the other, which is the whole defect.
        used: dict[str, int] = {}
        for i in members:
            if primary is not None and i == primary:
                ids[i] = txn_no
                continue
            digest = hashlib.sha256(
                _export_row_fingerprint(rows[i]).encode("utf-8")).hexdigest()[:8]
            seen = used.get(digest, 0)
            used[digest] = seen + 1
            ids[i] = f"{txn_no}#{digest}" + (f".{seen}" if seen else "")
        if len(members) > 1:
            log.info("transaction no. %s names %d movements on %s; "
                     "%d kept under suffixed ids",
                     txn_no, len(members), key[0],
                     len(members) - (1 if primary is not None else 0))
    return ids
