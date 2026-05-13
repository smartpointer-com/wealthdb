#!/usr/bin/env python3
"""
Schwab Trader API portfolio downloader (read-only).

Fetches account metadata, positions, and transactions from the Schwab
Trader API and writes the raw JSON responses to disk, organised by UTC
timestamp. Performs no write operations against the API.

OAuth tokens are loaded from a local token file. Schwab refresh tokens
expire 7 days after issue and cannot be renewed without an interactive
browser login; run `login.py` (planned) to mint a fresh token when this
script reports refresh failure.

Usage:
    download.py --token-path <file> --dest <dir> \\
                [--client-id <id>] [--client-secret <secret>] \\
                [--since YYYY-MM-DD] [--until YYYY-MM-DD] \\
                [--dry-run]
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

# schwab-py is a thin wrapper over the Schwab Trader API. We import it
# inside main() so that --help works on a fresh checkout without the
# dependency installed.

log = logging.getLogger("schwab-dump")

# Read-only artefact names. Filenames never contain account numbers (plain
# or hashed) so that ls'ing a dest dir does not leak identifiers.
ARTIFACT_ACCOUNT_NUMBERS = "account_numbers.json"
ARTIFACT_USER_PREFERENCE = "user_preference.json"
ARTIFACT_ACCOUNTS_POSITIONS = "accounts_positions.json"
ARTIFACT_TRANSACTIONS_TEMPLATE = "transactions_{n:03d}.json"
ARTIFACT_OPEN_ORDERS = "open_orders.json"

# Schwab caps the transactions endpoint window at 1 year per request. We
# chunk longer ranges into successive sub-ranges to stay within the cap.
TRANSACTION_WINDOW_DAYS = 365

# Order statuses considered "open" — i.e. the order can still execute,
# be cancelled, or be replaced. Excludes terminal states (FILLED,
# CANCELED, REJECTED, EXPIRED, REPLACED). UNKNOWN is kept in case
# Schwab introduces a new status this list does not yet recognise.
OPEN_ORDER_STATUSES = {
    "WORKING",
    "NEW",
    "ACCEPTED",
    "QUEUED",
    "PENDING_ACTIVATION",
    "PENDING_ACKNOWLEDGEMENT",
    "PENDING_CANCEL",
    "PENDING_REPLACE",
    "PENDING_RECALL",
    "AWAITING_PARENT_ORDER",
    "AWAITING_CONDITION",
    "AWAITING_STOP_CONDITION",
    "AWAITING_MANUAL_REVIEW",
    "AWAITING_UR_OUT",
    "AWAITING_RELEASE_TIME",
    "UNKNOWN",
}

# Schwab's orders endpoint caps the entered-datetime window at "not more
# than a year". The span we send is `start@midnight_UTC -> end@end_of_day_UTC`,
# so 365 lookback days yields ~366 elapsed-day units, which Schwab rejects
# with HTTP 400. 364 days plus end-of-day gives just under 365 elapsed days
# and passes the validator. Limit/stop orders rarely live longer than that
# in practice (Schwab GTC is capped at 60 days), so this still reliably
# surfaces every open order.
OPEN_ORDERS_LOOKBACK_DAYS = 364

# Transaction types we ask for. The Schwab enum (per schwab-py) is the
# union of all activity categories; we request all of them so the silver
# layer can classify on its own terms rather than relying on the API to
# pre-filter.
TRANSACTION_TYPES_ALL = [
    "TRADE",
    "RECEIVE_AND_DELIVER",
    "DIVIDEND_OR_INTEREST",
    "ACH_RECEIPT",
    "ACH_DISBURSEMENT",
    "CASH_RECEIPT",
    "CASH_DISBURSEMENT",
    "ELECTRONIC_FUND",
    "WIRE_OUT",
    "WIRE_IN",
    "JOURNAL",
    "MEMORANDUM",
    "MARGIN_CALL",
    "MONEY_MARKET",
    "SMA_ADJUSTMENT",
]


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--token-path",
        required=True,
        type=Path,
        help="Path to the schwab-py token JSON file. The file must already "
             "exist (mint it with login.py first).",
    )
    p.add_argument(
        "--dest",
        required=True,
        type=Path,
        help="Output directory. Each run creates a <UTC-timestamp> "
             "subdirectory under this path.",
    )
    p.add_argument(
        "--client-id",
        default=None,
        help="Schwab OAuth Client ID. Falls back to the SCHWAB_CLIENT_ID "
             "environment variable if omitted.",
    )
    p.add_argument(
        "--client-secret",
        default=None,
        help="Schwab OAuth Client Secret. Falls back to the SCHWAB_CLIENT_SECRET "
             "environment variable if omitted. Avoid passing on the command "
             "line in shared environments — prefer the env var.",
    )
    p.add_argument(
        "--since",
        type=date.fromisoformat,
        default=None,
        help="Earliest transaction date to fetch (YYYY-MM-DD). "
             "Defaults to 1 year before --until.",
    )
    p.add_argument(
        "--until",
        type=date.fromisoformat,
        default=None,
        help="Latest transaction date to fetch (YYYY-MM-DD, inclusive). "
             "Defaults to today (UTC).",
    )
    p.add_argument(
        "--dry-run",
        action="store_true",
        help="Connect, refresh tokens, list account hashes via "
             "/accounts/accountNumbers, and exit. Does not fetch positions "
             "or transactions and does not write artefacts.",
    )
    p.add_argument(
        "-v", "--verbose", action="store_true", help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


def resolve_credential(value: str | None, env_name: str, flag_name: str) -> str:
    """Return the credential value, falling back to the env var.

    Prefers the explicit CLI argument when provided; otherwise reads the
    environment variable. Raises SystemExit if neither is available."""
    if value:
        return value
    env_value = os.environ.get(env_name)
    if env_value:
        return env_value
    raise SystemExit(
        f"Missing credential: pass {flag_name} or set {env_name}. "
        f"Source your Schwab credentials env file before running."
    )


def write_json(path: Path, payload) -> None:
    """Write payload to path as pretty-printed JSON, creating parents."""
    path.parent.mkdir(parents=True, exist_ok=True)
    # Use a tmpfile + rename to avoid partial writes on interruption.
    tmp = path.with_suffix(path.suffix + ".tmp")
    with tmp.open("w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2, sort_keys=True, default=str)
        fh.write("\n")
    tmp.rename(path)
    log.info("Wrote %s (%d bytes)", path, path.stat().st_size)


def schwab_get_json(response):
    """Validate a schwab-py HTTPX response and return its parsed JSON.

    Raises a SystemExit on non-2xx; we deliberately do not retry here.
    Rate-limit / transient handling belongs in a scheduler, not in a
    one-shot CLI."""
    if response.status_code // 100 != 2:
        raise SystemExit(
            f"Schwab API returned HTTP {response.status_code}: "
            f"{response.text[:500]}"
        )
    return response.json()


def fetch_account_numbers(client) -> list[dict]:
    """GET /accounts/accountNumbers. Returns [{accountNumber, hashValue}, ...]."""
    return schwab_get_json(client.get_account_numbers())


def fetch_user_preference(client) -> dict:
    return schwab_get_json(client.get_user_preferences())


def fetch_accounts_with_positions(client) -> list[dict]:
    """GET /accounts?fields=positions. Returns one entry per linked account."""
    fields = client.Account.Fields.POSITIONS
    return schwab_get_json(client.get_accounts(fields=fields))


def iter_transaction_windows(since: date, until: date):
    """Yield (start, end) pairs covering [since, until] in <=365-day chunks."""
    cursor = since
    while cursor <= until:
        end = min(cursor + timedelta(days=TRANSACTION_WINDOW_DAYS - 1), until)
        yield cursor, end
        cursor = end + timedelta(days=1)


def fetch_open_orders(client, since: date, until: date) -> list[dict]:
    """GET /orders, filtered client-side to non-terminal statuses.

    Schwab requires from_entered_datetime/to_entered_datetime; we use the
    last OPEN_ORDERS_LOOKBACK_DAYS as the window since orders entered
    before that have either filled, been cancelled, or expired.

    We call get_orders_for_all_linked_accounts() once without a status
    filter (one request returns all statuses) and then drop any order
    whose status is terminal. This is cheaper than 15 per-status calls
    and produces the same final set."""
    start_dt = datetime.combine(since, datetime.min.time(), tzinfo=timezone.utc)
    end_dt = datetime.combine(until, datetime.max.time(), tzinfo=timezone.utc)
    all_orders = schwab_get_json(
        client.get_orders_for_all_linked_accounts(
            from_entered_datetime=start_dt,
            to_entered_datetime=end_dt,
        )
    )
    return [o for o in all_orders if o.get("status") in OPEN_ORDER_STATUSES]


def fetch_transactions(client, account_hash: str, start: date, end: date) -> list[dict]:
    """GET /accounts/{hash}/transactions for a date range.

    schwab-py expects start_date / end_date as datetimes; we promote
    date -> midnight-UTC start and end-of-day-UTC end so the response
    includes all activity on the boundary days."""
    start_dt = datetime.combine(start, datetime.min.time(), tzinfo=timezone.utc)
    end_dt = datetime.combine(end, datetime.max.time(), tzinfo=timezone.utc)
    types = [client.Transactions.TransactionType[t] for t in TRANSACTION_TYPES_ALL]
    return schwab_get_json(
        client.get_transactions(
            account_hash,
            start_date=start_dt,
            end_date=end_dt,
            transaction_types=types,
        )
    )


def run(args: argparse.Namespace) -> int:
    try:
        import schwab as schwab_pkg  # type: ignore
    except ImportError:
        raise SystemExit(
            "schwab-py is not installed. Run: pip install -r requirements.txt"
        )

    if not args.token_path.is_file():
        raise SystemExit(
            f"Token file {args.token_path} does not exist. "
            f"Run login.py first to mint one (interactive browser flow)."
        )

    client_id = resolve_credential(args.client_id, "SCHWAB_CLIENT_ID", "--client-id")
    client_secret = resolve_credential(args.client_secret, "SCHWAB_CLIENT_SECRET", "--client-secret")

    log.info("Loading client from token at %s", args.token_path)
    # schwab-py's parameter names (api_key, app_secret) are a historical
    # quirk; they accept the OAuth Client ID / Client Secret that Schwab
    # issues in its developer portal.
    client = schwab_pkg.auth.client_from_token_file(
        token_path=str(args.token_path),
        api_key=client_id,
        app_secret=client_secret,
    )

    log.info("Listing account hashes ...")
    account_numbers = fetch_account_numbers(client)
    log.info("Schwab returned %d linked account(s)", len(account_numbers))

    if args.dry_run:
        # Print only hash prefixes — never plaintext numbers — so dry-run
        # output is safe to paste into bug reports.
        for entry in account_numbers:
            h = entry.get("hashValue", "")
            log.info("  account hash %s... (truncated)", h[:8])
        log.info("Dry run complete. No artefacts written.")
        return 0

    # Default the transaction window to the last year if not specified.
    # Schwab caps the transactions endpoint at 365 days per request; we
    # set the default lookback so the entire range fits in a single Schwab
    # call and we don't emit a 1-day trailing chunk on the boundary.
    today = datetime.now(timezone.utc).date()
    until = args.until or today
    since = args.since or (until - timedelta(days=TRANSACTION_WINDOW_DAYS - 1))
    if since > until:
        raise SystemExit(f"--since {since} is after --until {until}")
    log.info("Transaction window: %s -> %s", since, until)

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = args.dest / ts
    log.info("Writing artefacts to %s", run_dir)

    write_json(run_dir / ARTIFACT_ACCOUNT_NUMBERS, account_numbers)

    user_pref = fetch_user_preference(client)
    write_json(run_dir / ARTIFACT_USER_PREFERENCE, user_pref)

    positions = fetch_accounts_with_positions(client)
    write_json(run_dir / ARTIFACT_ACCOUNTS_POSITIONS, positions)

    # Transactions are fetched per account-hash, chunked into <=1y windows
    # to stay under Schwab's request-range cap. Filenames are numbered
    # rather than hash-tagged so they do not leak identifiers; the
    # account-hash dimension lives inside the JSON payload.
    n = 0
    for entry in account_numbers:
        h = entry["hashValue"]
        for start, end in iter_transaction_windows(since, until):
            log.info("Fetching transactions for account %s... %s -> %s",
                     h[:8], start, end)
            txns = fetch_transactions(client, h, start, end)
            payload = {
                "account_hash": h,
                "window_start": start.isoformat(),
                "window_end": end.isoformat(),
                "transactions": txns,
            }
            write_json(run_dir / ARTIFACT_TRANSACTIONS_TEMPLATE.format(n=n), payload)
            n += 1

    # Open orders are fetched once (cross-account, not per-account) and
    # filtered client-side to non-terminal statuses. Closed/cancelled/
    # expired orders are intentionally dropped — for this dump we only
    # care about orders that can still affect the portfolio.
    orders_since = today - timedelta(days=OPEN_ORDERS_LOOKBACK_DAYS)
    log.info("Fetching open orders (entered %s -> %s) ...", orders_since, today)
    open_orders = fetch_open_orders(client, orders_since, today)
    open_orders_payload = {
        "window_start": orders_since.isoformat(),
        "window_end": today.isoformat(),
        "status_filter": sorted(OPEN_ORDER_STATUSES),
        "orders": open_orders,
    }
    write_json(run_dir / ARTIFACT_OPEN_ORDERS, open_orders_payload)
    log.info("Open orders: %d", len(open_orders))

    log.info("Done. Wrote %d transaction window(s) across %d account(s); "
             "%d open order(s).",
             n, len(account_numbers), len(open_orders))
    return 0


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
