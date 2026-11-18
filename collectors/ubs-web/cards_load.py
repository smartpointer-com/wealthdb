"""The ubs-web card pass: the card roster, its ledger, the invoices
and the statements behind them."""

from __future__ import annotations

import logging
import sqlite3
from pathlib import Path

import card_parsers  # local module
from load_rows import _NEWER_DUMP_LOADED, _read_artefact_json, _written_by_a_newer_dump

log = logging.getLogger("ubs-web.load")


def _load_cards(conn: sqlite3.Connection, snapshot_at: int,
                dump_dir: Path) -> tuple[int, int, int, int]:
    """Load `<dump>/cards/` into the card_* tables.

    A dump with no `cards/` dir — one taken before the card pass existed,
    or with `--no-cards` — loads as zeros rather than as an error, so
    older bronze keeps loading unchanged.

    Nothing is deleted and nothing is zeroed. Every write is an UPSERT
    keyed on an id the source owns, so a run that covered one account, or
    one window, leaves what it did not cover exactly as it was.
    """
    cards_dir = dump_dir / "cards"
    if not cards_dir.is_dir():
        return 0, 0, 0, 0

    # The roster is read FIRST: it states which account each card
    # charges, and the ledger books its rows against the CARD. Without
    # that map a multi-card account's rows key on an id no other card
    # table carries, and they join to nothing.
    roster = _read_card_json(cards_dir / "accounts.json")
    card_to_account = card_parsers.card_account_map(roster)
    stable_accounts = card_parsers.stable_account_ids(roster)

    txn_rows: list[dict] = []
    reserved_counts: dict[str, int] = {}
    for path in sorted(cards_dir.glob("transactions_*.json")):
        rows, page_reserved = card_parsers.parse_transactions(
            _read_card_json(path).get("pages") or [], card_to_account)
        txn_rows.extend(rows)
        for account, count in page_reserved.items():
            reserved_counts[account] = reserved_counts.get(account, 0) + count

    # The billing periods are read once and used twice: the rows go to
    # `card_invoices`, and the statement index needs the same rows to
    # attribute each PDF to its period.
    invoices = _parse_card_invoices(cards_dir, stable_accounts)

    acc_count = _insert_card_accounts(
        conn, snapshot_at, roster, reserved_counts)
    txn_count = _insert_card_transactions(conn, snapshot_at, txn_rows)
    inv_count = _insert_card_invoices(conn, snapshot_at, invoices)
    stmt_count = _insert_card_statements(conn, snapshot_at, cards_dir, invoices)
    _refresh_invoice_coverage(conn)
    _warn_orphan_card_rows(conn)
    return acc_count, txn_count, inv_count, stmt_count


def _warn_orphan_card_rows(conn: sqlite3.Connection) -> None:
    """Report ledger rows whose account has no `card_accounts` row.

    Such a row loads cleanly and then joins to nothing — no account, no
    invoice, and outside gold's spending scope, which selects from the
    accounts table. It is the signature of a ledger keyed on something
    other than the account id, which is exactly what the card-to-account
    map exists to prevent, so it is worth saying out loud rather than
    leaving to be noticed in a report that is quietly short.
    """
    row = conn.execute(
        "SELECT COUNT(*) FROM card_transactions t "
        " WHERE NOT EXISTS (SELECT 1 FROM card_accounts a "
        "                    WHERE a.account_external_id = t.account_external_id)"
    ).fetchone()
    if row and row[0]:
        log.warning("%d card transaction(s) reference an account with no "
                    "card_accounts row; they will not join to an account "
                    "in gold", row[0])


def _read_card_json(path: Path) -> dict:
    """Read one bronze card artefact; an unreadable one reads as empty."""
    payload = _read_artefact_json(path, "card")
    return {} if payload is None else payload


def _insert_card_accounts(conn: sqlite3.Connection, snapshot_at: int,
                          roster: dict, reserved_counts: dict) -> int:
    rows = card_parsers.parse_accounts(roster, reserved_counts)
    for row in rows:
        conn.execute(
            "INSERT OR REPLACE INTO card_accounts ("
            "snapshot_at, account_external_id, account_number, currency_iso, "
            "balance, available, credit_limit, reserved_amount, "
            "reserved_count, product_name, card_type, account_status, "
            "structure_type, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, row["account_external_id"], row["account_number"],
             row["currency_iso"], row["balance"], row["available"],
             row["credit_limit"], row["reserved_amount"],
             row["reserved_count"], row["product_name"], row["card_type"],
             row["account_status"], row["structure_type"], row["payload"]))
    return len(rows)


def _insert_card_transactions(conn: sqlite3.Connection, snapshot_at: int,
                              rows: list[dict]) -> int:
    """UPSERT the ledger, keyed on the content id card_parsers mints.

    `snapshot_at` is deliberately left out of the update clause: the
    column means "the dump that first captured this row", and a later
    dump re-observing it must not rewrite that. So it cannot tell which
    dump wrote a row last, and a dump loaded after a newer one updates
    none.
    """
    for row in rows:
        conn.execute(
            "INSERT INTO card_transactions ("
            "transaction_external_id, account_external_id, snapshot_at, "
            "transaction_date, value_date, amount, currency_iso, "
            "original_amount, original_currency_iso, exchange_rate, "
            "merchant, merchant_category, merchant_group_code, card_number, "
            "settled_in_invoice, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
            "ON CONFLICT(transaction_external_id) DO UPDATE SET "
            "account_external_id=excluded.account_external_id, "
            "transaction_date=excluded.transaction_date, "
            "value_date=excluded.value_date, amount=excluded.amount, "
            "currency_iso=excluded.currency_iso, "
            "original_amount=excluded.original_amount, "
            "original_currency_iso=excluded.original_currency_iso, "
            "exchange_rate=excluded.exchange_rate, "
            "merchant=excluded.merchant, "
            "merchant_category=excluded.merchant_category, "
            "merchant_group_code=excluded.merchant_group_code, "
            "card_number=excluded.card_number, "
            "settled_in_invoice=excluded.settled_in_invoice, "
            "payload=excluded.payload "
            f"WHERE NOT {_NEWER_DUMP_LOADED}",
            (row["transaction_external_id"], row["account_external_id"],
             snapshot_at, row["transaction_date"], row["value_date"],
             row["amount"], row["currency_iso"], row["original_amount"],
             row["original_currency_iso"], row["exchange_rate"],
             row["merchant"], row["merchant_category"],
             row["merchant_group_code"], row["card_number"],
             row["settled_in_invoice"], row["payload"]))
    return len(rows)


def _parse_card_invoices(cards_dir: Path,
                         accounts: dict[str, str]) -> dict[str, list[dict]]:
    """Every account's billing periods, keyed by the account's short id.

    One read and one parse per account: the rows are wanted twice — as
    `card_invoices` rows, and as the periods a statement PDF is
    attributed to — and reading the same two files twice per account
    only invites the two answers to differ.
    """
    out: dict[str, list[dict]] = {}
    for listing_path in sorted(cards_dir.glob("invoices_*.json")):
        short = listing_path.stem[len("invoices_"):]
        details = _read_card_json(
            cards_dir / f"invoice-details_{short}.json").get("invoices") or []
        out[short] = card_parsers.parse_invoices(
            _read_card_json(listing_path), details, accounts=accounts)
    return out


def _insert_card_invoices(conn: sqlite3.Connection, snapshot_at: int,
                          invoices: dict[str, list[dict]]) -> int:
    """UPSERT the billing periods. `transactions_covered` is left to
    :func:`_refresh_invoice_coverage`, which needs the whole ledger, and
    is carried across the upsert meanwhile."""
    count = 0
    for rows in invoices.values():
        for row in rows:
            if _written_by_a_newer_dump(conn, "card_invoices", {
                    "account_external_id": row["account_external_id"],
                    "period_end": row["period_end"]}, snapshot_at):
                continue
            conn.execute(
                "INSERT OR REPLACE INTO card_invoices ("
                "account_external_id, period_end, period_start, "
                "invoice_external_id, snapshot_at, invoicing_date, "
                "debiting_date, due_on, due_amount, minimal_due_amount, "
                "currency_iso, balance_forward, total_debit, total_credit, "
                "reconciles, statement_type, payment_method, invoice_status, "
                "transactions_covered, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, "
                "?, ?, COALESCE((SELECT transactions_covered FROM "
                "card_invoices WHERE account_external_id = ? AND "
                "period_end = ?), 0), ?)",
                (row["account_external_id"], row["period_end"],
                 row["period_start"], row["invoice_external_id"], snapshot_at,
                 row["invoicing_date"], row["debiting_date"], row["due_on"],
                 row["due_amount"], row["minimal_due_amount"],
                 row["currency_iso"], row["balance_forward"],
                 row["total_debit"], row["total_credit"], row["reconciles"],
                 row["statement_type"], row["payment_method"],
                 row["invoice_status"], row["account_external_id"],
                 row["period_end"], row["payload"]))
            count += 1
    return count


def _insert_card_statements(conn: sqlite3.Connection, snapshot_at: int,
                            cards_dir: Path,
                            invoices: dict[str, list[dict]]) -> int:
    """Index the statement PDFs against the periods they belong to.

    A statement is content-addressed, so its filename says nothing about
    which period produced it; the capture writes that mapping beside it.
    A file with no mapping is skipped rather than guessed at — a
    statement filed under the wrong period is worse than one not filed.
    """
    stmt_dir = cards_dir / "statements"
    if not stmt_dir.is_dir():
        return 0
    attribution = _statement_attribution(cards_dir, invoices)
    count = 0
    for pdf in sorted(stmt_dir.glob("*.pdf")):
        meta = attribution.get(pdf.name)
        if meta is None:
            log.warning("card statement %s… has no invoice attribution; "
                        "not indexed", pdf.name[:12])
            continue
        if _written_by_a_newer_dump(conn, "card_statements",
                                    {"content_sha256": pdf.stem}, snapshot_at):
            continue
        conn.execute(
            "INSERT OR REPLACE INTO card_statements ("
            "content_sha256, account_external_id, invoice_external_id, "
            "period_end, file_path, size_bytes, snapshot_at"
            ") VALUES (?, ?, ?, ?, ?, ?, ?)",
            (pdf.stem, meta["account_external_id"],
             meta["invoice_external_id"], meta["period_end"],
             str(pdf.resolve()), pdf.stat().st_size, snapshot_at))
        count += 1
    return count


def _statement_attribution(cards_dir: Path,
                           invoices: dict[str, list[dict]]) -> dict:
    """Statement filename -> the invoice row it belongs to.

    A statement is content-addressed, so its name carries no invoice id;
    the capture writes the mapping beside it — in the API's own invoice
    ids, which is why the lookup is on the parse's `_handle` and not on
    the content id the row is stored under.
    """
    mapping: dict[str, dict] = {}
    for stmts_path in sorted(cards_dir.glob("statements_*.json")):
        short = stmts_path.stem[len("statements_"):]
        by_id = {r["_handle"]: r for r in invoices.get(short, ())}
        for name, invoice_id in (
                _read_card_json(stmts_path).get("files") or {}).items():
            row = by_id.get(invoice_id)
            if row is not None:
                mapping[name] = row
    return mapping


def _refresh_invoice_coverage(conn: sqlite3.Connection) -> None:
    """Recompute `card_invoices.transactions_covered` across the table.

    A period counts as covered when the account's loaded ledger reaches
    past both of its edges. Reaching past both is what stops a period
    only half-covered by a narrow window from claiming to be whole.

    Recomputed rather than accumulated: coverage is a function of how
    much ledger has landed, which grows with every load, so an answer
    kept from an earlier load goes stale in the direction of claiming
    more than is there.
    """
    conn.execute(
        "UPDATE card_invoices SET transactions_covered = ("
        "  SELECT CASE WHEN COUNT(*) > 0"
        "              AND MIN(t.value_date) <= card_invoices.period_start"
        "              AND MAX(t.value_date) >= card_invoices.period_end"
        "         THEN 1 ELSE 0 END"
        "  FROM card_transactions t"
        "  WHERE t.account_external_id = card_invoices.account_external_id"
        ")")
