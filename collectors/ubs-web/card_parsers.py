#!/usr/bin/env python3
"""
Bronze → silver parsers for the credit-card surface.

Pure functions from the JSON `cards.py` stored to the row dicts
`load.py` writes: no database handle, no filesystem beyond what is
handed in, so each can be tested against a synthetic payload without
standing silver up. The same split `pdf_parsers.py` uses.

What the shapes are, and why the promotions look the way they do, is in
DESIGN.md §5.3–5.4. The three that matter here:

* **A row's identity is minted from its content**, never from the API's
  ``_id`` and never from ``transactionNr``. The latter is a short
  sequence that repeats across hundreds of rows; the former looks
  durable and is not — UBS re-mints every id in the card surface on
  every login (see :func:`stable_account_ids`), so a table keyed on one
  gains a whole second copy of its history per download.
* **``RESERVED`` rows are not transactions.** They carry no id, no value
  date and no posting amount. They are summed onto the account instead.
* **Amounts are stored as UBS signs them** — negative while the card
  owes — which is already the convention gold wants for a liability.
"""

from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime, timezone

# The status that means "this is a settled ledger row". Everything else
# the ledger returns is an authorisation that has not posted.
STATUS_BOOKED = "BOOKED"

# Tolerance for the invoice identity, in the invoice's own currency.
# The figures are printed to the cent, so anything above half a cent is
# a real disagreement rather than float noise.
RECONCILE_TOLERANCE = 0.005

_DATE_ONLY_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def ts(value: str | None) -> int | None:
    """`YYYY-MM-DD` or a full ISO timestamp -> Unix seconds UTC."""
    if not value:
        return None
    try:
        if _DATE_ONLY_RE.match(value):
            dt = datetime.strptime(value, "%Y-%m-%d")
        else:
            dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def money(node) -> tuple[float | None, str | None]:
    """An ``{amount, currency}`` object -> `(amount, currency)`.

    The amount arrives as a string and keeps its sign. A value that will
    not parse yields `(None, currency)` rather than 0.0: a missing figure
    and a zero one mean different things to everything downstream.
    """
    if not isinstance(node, dict):
        return None, None
    raw = node.get("amount")
    currency = node.get("currency") or None
    if raw is None:
        return None, currency
    try:
        return float(str(raw).strip()), currency
    except ValueError:
        return None, currency


def _payload(node) -> str:
    return json.dumps(node, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False)


def _content_id(prefix: str, parts, occurrence: int = 0) -> str:
    """A content-addressed id: `prefix` + a hash of the facts given.

    `occurrence` separates rows the facts cannot: two identical
    purchases on one card on one day are two spends, not one. It is the
    row's index within its own identical group, which is stable because
    every dump re-fetches the whole history — the group has the same
    membership each time, so it hands out the same indices.

    Mirrors `load.py`'s `_stmt_txn_id` for the statement era, which
    solves the same problem for a source that never had an id at all.
    """
    body = "|".join("" if x is None else str(x) for x in (*parts, occurrence))
    return prefix + hashlib.sha256(body.encode("utf-8")).hexdigest()[:16]


def _walk(node, predicate):
    """Every dict anywhere under `node` that satisfies `predicate`.

    The payloads nest — accounts carry related accounts, invoices arrive
    under varying envelopes — and a row missed by reading one fixed path
    is a row silently not loaded.
    """
    found = []
    seen: set[int] = set()

    def visit(value):
        if isinstance(value, dict):
            if predicate(value) and id(value) not in seen:
                seen.add(id(value))
                found.append(value)
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)

    visit(node)
    return found


# ============================================================
# Transactions
# ============================================================

def parse_transactions(pages: list[dict],
                       card_to_account: dict | None = None,
                       ) -> tuple[list[dict], dict[str, int]]:
    """Ledger pages -> (booked rows, per-account count of unposted rows).

    `card_to_account` is :func:`card_account_map`. Each row's
    `bookedAccountId` is resolved through it, because on a multi-card
    account that field names the CARD; an unmapped id passes through
    unchanged, which is right for the single-card case where the two
    ids coincide.

    Rows are deduplicated on ``_id``: overlapping fetch windows return
    the same row more than once, and every repeat observed carried
    identical content, so the first wins and the rest are dropped.

    The second return is a COUNT of RESERVED rows per account, not a
    sum. Their magnitude comes from the roster instead
    (:func:`roster_reserved`) — a reserved row carries only the
    merchant-currency figure, so summing them would mix units.
    """
    card_to_account = card_to_account or {}
    rows: dict[str, dict] = {}
    reserved: dict[str, int] = {}

    for page in pages or []:
        for txn in (page.get("_embedded") or {}).get("transactions", []) or []:
            booked_to = txn.get("bookedAccountId")
            account = card_to_account.get(booked_to, booked_to)
            if txn.get("transactionStatus") != STATUS_BOOKED:
                reserved[account] = reserved.get(account, 0) + 1
                continue
            row = _booked_row(txn, account)
            if row is not None and row["_handle"] not in rows:
                rows[row["_handle"]] = row

    return _keyed_by_content(list(rows.values())), reserved


# The facts a card row's identity is built from. Deliberately WITHOUT the
# merchant text: UBS re-labels a merchant between fetches, and free text
# inside an identity plus an upsert-only writer is how a re-label mints a
# second id and doubles the transaction — the defect fidelity-web's
# migration 0005 already paid for on its own feed. The reduced key collides
# where two same-day, same-amount purchases differ only by merchant; the
# occurrence index separates them, exactly as chase's deposit content key
# does. Do not put text back in here to resolve that collision.
_CARD_KEY_FIELDS = ("card_number", "transaction_date", "value_date", "amount",
                    "currency_iso", "original_amount", "original_currency_iso")

# What separates two rows that share the key. Used ONLY to order the group
# before handing out occurrence indices — never `_handle` or `payload`,
# which carry the session id migration 0008 had to purge this table over.
_CARD_TIEBREAK_FIELDS = ("merchant", "merchant_category", "merchant_group_code",
                         "exchange_rate", "settled_in_invoice")


def _keyed_by_content(rows: list[dict]) -> list[dict]:
    """Give each row the id silver stores, and drop the API handle.

    The handle deduped the fetch above — overlapping windows return a
    row more than once, and within one session the same row carries the
    same `_id` — but it cannot survive the session, so it is spent here
    and discarded.

    Order does not enter, and since the merchant left the key it takes
    work to keep it that way: members of a group are no longer identical
    in every stored fact, so which one takes occurrence 0 would otherwise
    follow the order the API happened to page them in. Nothing promises
    that order. Sorting the group on the facts outside the key fixes the
    assignment, which gives the invariant the whole scheme rests on: a
    group of n rows yields ids for occurrences 0..n-1, so the SET of ids
    depends on the group's SIZE alone. No fetch order and no re-label can
    mint an id that did not already exist.
    """
    groups: dict[tuple, list[dict]] = {}
    for row in rows:
        key = tuple(row[f] for f in _CARD_KEY_FIELDS)
        groups.setdefault(key, []).append(row)

    for key, members in groups.items():
        members.sort(key=lambda r: tuple(
            "" if r.get(f) is None else str(r.get(f))
            for f in _CARD_TIEBREAK_FIELDS))
        for n, row in enumerate(members):
            row["transaction_external_id"] = _content_id("card:", key, n)

    for row in rows:
        del row["_handle"]
    return rows


def _booked_row(txn: dict, account: str | None) -> dict | None:
    """One BOOKED ledger row, or None when it cannot be keyed or dated.

    `_handle` is the API's per-session `_id`, carried only far enough to
    dedup the fetch; :func:`_keyed_by_content` replaces it with the id
    silver stores. A booked row always carries a handle and a value date
    in practice; refusing the ones that do not is what keeps a malformed
    payload from landing a row that nothing can join or order.
    """
    handle = txn.get("_id")
    value_date = ts(txn.get("valueDate"))
    if not handle or value_date is None or not account:
        return None
    amount, currency = money(txn.get("postingAmount"))
    if amount is None:
        return None
    original_amount, original_currency = money(txn.get("originalAmount"))
    settled = txn.get("settledInInvoice")
    return {
        "_handle": handle,
        "account_external_id": account,
        "transaction_date": ts(txn.get("transactionDate")),
        "value_date": value_date,
        "amount": amount,
        "currency_iso": currency,
        "original_amount": original_amount,
        "original_currency_iso": original_currency,
        "exchange_rate": _float(txn.get("exchangeRate")),
        # `details` is the merchant; `merchantName` is the CATEGORY. The
        # API's naming is the wrong way round and the promotion is where
        # that gets corrected once, for everything downstream.
        "merchant": txn.get("details"),
        "merchant_category": txn.get("merchantName"),
        "merchant_group_code": txn.get("merchantGroupCode"),
        "card_number": txn.get("cardNr"),
        "settled_in_invoice": None if settled is None else int(bool(settled)),
        "payload": _payload(txn),
    }


def _scalar(value, key: str):
    """A field that may arrive as a scalar or as a small object.

    Returns the scalar, or `key` out of the object. UBS wraps some
    single-valued fields in an envelope, and a column bound to the
    envelope raises at insert time rather than storing anything.
    """
    if isinstance(value, dict):
        return value.get(key)
    return value


def _float(value) -> float | None:
    if value is None:
        return None
    try:
        return float(str(value).strip())
    except ValueError:
        return None


# ============================================================
# Accounts
# ============================================================

def parse_accounts(roster: dict, reserved_counts: dict | None = None,
                   ) -> list[dict]:
    """The roster -> one row per card account.

    Deduplicated on the account id, first occurrence winning, exactly as
    :func:`account_ids` is: the roster reaches the same account by more
    than one path (a top-level entry and again under a sibling's related
    accounts), and `_walk` dedups on object identity rather than on the
    id. Without this the two readings of one payload disagree, and the
    insert's last-writer-wins would let an abbreviated copy overwrite a
    full one.

    The reserved figure comes from the roster (:func:`roster_reserved`);
    `reserved_counts` is the ledger's count of unposted rows per
    account, which is currency-free and so safe to take from there.
    """
    reserved_counts = reserved_counts or {}
    reserved_amounts = roster_reserved(roster)
    stable = stable_account_ids(roster)
    rows = []
    seen: set[str] = set()
    for node in _walk(roster, _is_account_node):
        account = node["id"]
        if account in seen:
            continue
        seen.add(account)
        balance, currency = money(node.get("balance"))
        available, _ = money(node.get("available"))
        limit, _ = money(node.get("limit"))
        rows.append({
            "account_external_id": stable[account],
            "account_number": node.get("accountNumber"),
            "currency_iso": currency or node.get("currency"),
            # As the roster reports it, and it already INCLUDES the
            # account's authorised-but-unposted spend: an account's
            # `balance` matches its card's `balanceIncludingReserved`,
            # not the card's `balance`. `reserved_amount` beside it is
            # the part of this figure that has not booked, recorded so a
            # reader can tell the two apart — never to be added back.
            "balance": balance,
            "available": available,
            "credit_limit": limit,
            "reserved_amount": reserved_amounts.get(stable[account]),
            "reserved_count": reserved_counts.get(stable[account]),
            "product_name": node.get("productName"),
            "card_type": node.get("cardType"),
            "account_status": node.get("accountStatus")
            or node.get("cardStatus"),
            "structure_type": node.get("structureType"),
            "payload": _payload(node),
        })
    return rows


# ============================================================
# Invoices
# ============================================================

def parse_invoices(listing: dict,
                   details: list[dict] | None = None,
                   account_external_id: str | None = None,
                   accounts: dict[str, str] | None = None) -> list[dict]:
    """Billing periods -> one row per (account, period end).

    The period list carries the dates and the closing figure; the
    per-invoice detail carries the opening balance and the turnover. They
    are merged on the invoice id, and the identity

        balance_forward + total_debit + total_credit == due_amount

    is checked here rather than at insert time, so `reconciles` is a
    property of the parse and testable without a database.

    `account_external_id` is the account the listing was fetched for. It
    is the fallback for a detail-only period: the detail payload does not
    repeat the account, so without it such a row could not be keyed.
    `accounts` is :func:`stable_account_ids`, which turns the session
    handle each node names into the identity silver stores.

    The merge runs on the API's invoice id and the stored id is minted
    after it: the two payloads agree on the handle within one session,
    which is all the merge needs, and neither the handle nor the account
    it names survives to the table.
    """
    accounts = accounts or {}
    by_id: dict[str, dict] = {}
    for node in _walk(listing, _is_invoice_node):
        row = _invoice_row(node, account_external_id, accounts)
        if row is not None:
            by_id[row["invoice_external_id"]] = row

    for detail in details or []:
        for node in _walk(detail, _is_invoice_node):
            invoice_id = node.get("id")
            row = by_id.get(invoice_id)
            if row is None:
                row = _invoice_row(node, account_external_id, accounts)
                if row is None:
                    continue
                by_id[invoice_id] = row
            _merge_detail(row, node)

    rows = [r for r in by_id.values() if r["account_external_id"]]
    for row in rows:
        # `_handle` is not stored. The statement capture writes its
        # filename -> invoice mapping in the API's own ids, so the PDF
        # index still has to be able to find a period by the handle it
        # was captured under.
        row["_handle"] = row["invoice_external_id"]
        row["invoice_external_id"] = _content_id("cardinv:", (
            row["account_external_id"], row["period_start"],
            row["period_end"], row["statement_type"]))
    return rows


def _is_invoice_node(node: dict) -> bool:
    return "periodFrom" in node and isinstance(node.get("id"), str)


def _is_card_node(node: dict) -> bool:
    """A node describing one physical card under an account.

    Identified by the pair that makes it useful — its own id and the
    account it charges — rather than by a type discriminator, which card
    nodes do not carry.
    """
    return (isinstance(node.get("_id"), str)
            and isinstance(node.get("liableAccountId"), str))


def card_account_map(roster: dict) -> dict[str, str]:
    """card id -> the account id that card charges.

    The ledger books a row against the CARD (`bookedAccountId` names a
    card, not the account, on any account holding more than one), while
    every other card table is keyed by the account the roster
    enumerates. Without this map those rows key on an id nothing else
    has, and they join to nothing — no account row, no invoice, and
    outside gold's spending scope, which selects from `accounts`.

    The roster states the relation itself: each card node carries
    `liableAccountId`. That is a session handle, so it is resolved to
    the identity silver stores (:func:`stable_account_ids`) here rather
    than left for the insert to translate.

    Every ACCOUNT handle maps too, to its own stable id. A single-card
    account books against the account itself, and such a row would
    otherwise pass through carrying a handle while its account row, its
    invoices and the rest of the ledger carry the number — joining to
    nothing, which is the very thing this map exists to prevent.
    """
    stable = stable_account_ids(roster)
    mapping = dict(stable)
    mapping.update(
        {n["_id"]: stable.get(n["liableAccountId"], n["liableAccountId"])
         for n in _walk(roster, _is_card_node)})
    return mapping


def roster_reserved(roster: dict) -> dict[str, float]:
    """account id -> its authorised-but-unposted amount, in the account's
    own currency.

    Keyed, like everything this module returns, by the identity silver
    stores rather than by the session handle the roster names.

    Taken from the roster rather than summed from the ledger's RESERVED
    rows, for two reasons. A reserved row carries only `originalAmount`,
    which is the merchant's currency, so summing them mixes units. And
    each card states the figure directly: `balanceIncludingReserved`
    less `balance`, both in the card's currency.
    """
    stable = stable_account_ids(roster)
    out: dict[str, float] = {}
    for node in _walk(roster, _is_card_node):
        account = stable.get(node["liableAccountId"], node["liableAccountId"])
        with_reserved, _ = money(node.get("balanceIncludingReserved"))
        booked, _ = money(node.get("balance"))
        if with_reserved is None or booked is None:
            continue
        out[account] = out.get(account, 0.0) + (with_reserved - booked)
    return out


def _is_account_node(node: dict) -> bool:
    return (node.get("accountType") == "CREDIT_CARD_ACCOUNT"
            and isinstance(node.get("id"), str))


def stable_account_ids(roster: dict) -> dict[str, str]:
    """The API's account handle -> the identity silver stores.

    UBS's card surface hands out session-scoped handles: the account
    `id`, a card's `_id`, a ledger row's `_id` and an invoice's `id` are
    all re-minted at every login, and two dumps taken a day apart shared
    NOT ONE ledger id. A handle is therefore the right thing
    to drive the fetch with and the wrong thing to key a table on — key
    on one and every download appends a second copy of the history.

    `accountNumber` is the identity that does hold still. An account
    without one keeps its handle: the row is worth having on the old
    terms, and the orphan check will say so if the ledger disagrees.
    """
    return {node["id"]: node.get("accountNumber") or node["id"]
            for node in _walk(roster, _is_account_node)}


def account_ids(roster: dict) -> list[str]:
    """Every card-account id in a roster payload, in document order.

    Exported because the capture needs the same reading the parse does:
    an id the walk misses is an account silently not fetched, and one the
    parse misses is an account silently not loaded. One definition means
    the two cannot drift apart.
    """
    return _dedup(node["id"] for node in _walk(roster, _is_account_node))


def invoice_ids(payload: dict) -> list[str]:
    """Every invoice id in a period listing, in document order."""
    return _dedup(node["id"] for node in _walk(payload, _is_invoice_node))


def _dedup(values) -> list[str]:
    """Order-preserving de-duplication."""
    seen: set[str] = set()
    out: list[str] = []
    for v in values:
        if v not in seen:
            seen.add(v)
            out.append(v)
    return out


def _invoice_row(node: dict, account_external_id: str | None,
                 accounts: dict[str, str]) -> dict | None:
    period_start = ts(node.get("periodFrom"))
    period_end = ts(node.get("periodTo"))
    if period_start is None or period_end is None:
        return None
    due_amount, currency = money(node.get("dueAmount"))
    minimal, _ = money(node.get("minimalDueAmount"))
    account = node.get("creditCardAccountId") or account_external_id
    return {
        "account_external_id": accounts.get(account, account),
        "period_end": period_end,
        "period_start": period_start,
        "invoice_external_id": node["id"],
        "invoicing_date": ts(node.get("invoicingDate")),
        "debiting_date": ts(node.get("debitingDate")),
        "due_on": ts(node.get("dueOn")),
        "due_amount": due_amount,
        "minimal_due_amount": minimal,
        "currency_iso": currency,
        "balance_forward": None,
        "total_debit": None,
        "total_credit": None,
        "reconciles": None,
        "statement_type": node.get("statementType"),
        "payment_method": node.get("paymentMethod"),
        # An object (`{"statusCode": …}`), not a scalar — binding it
        # straight into a TEXT column raises and takes the whole dump's
        # load down with it.
        "invoice_status": _scalar(node.get("invoiceStatus"), "statusCode"),
        "payload": _payload(node),
    }


def _merge_detail(row: dict, node: dict) -> None:
    """Fold a period's totals onto its row and settle `reconciles`."""
    balance_forward, _ = money(node.get("balanceForward"))
    total_debit, _ = money(node.get("totalDebit"))
    total_credit, _ = money(node.get("totalCredit"))
    row["balance_forward"] = balance_forward
    row["total_debit"] = total_debit
    row["total_credit"] = total_credit
    # The detail is the fuller record, so its payload replaces the
    # listing's; the listing's own columns are already promoted above.
    row["payload"] = _payload(node)
    row["reconciles"] = reconciles(balance_forward, total_debit,
                                   total_credit, row.get("due_amount"))


def reconciles(balance_forward: float | None, total_debit: float | None,
               total_credit: float | None,
               due_amount: float | None) -> int | None:
    """Whether a period's four figures satisfy the identity.

    None when any of them is missing — there was nothing to check, which
    is a different answer from "checked and disagreed".
    """
    parts = (balance_forward, total_debit, total_credit, due_amount)
    if any(p is None for p in parts):
        return None
    total = balance_forward + total_debit + total_credit
    return int(abs(total - due_amount) <= RECONCILE_TOLERANCE)
