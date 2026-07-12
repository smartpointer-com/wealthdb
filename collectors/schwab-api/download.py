#!/usr/bin/env python3
"""
Schwab Trader API portfolio downloader (read-only).

Fetches account metadata, positions, and transactions from the Schwab
Trader API and writes the raw JSON responses to disk, organised by UTC
timestamp. Performs no write operations against the API.

Each bronze DATA artefact is zstd-compressed in place as it lands
(`accounts_positions.json` → `accounts_positions.json.zst`, and likewise
for account_numbers / user_preference / transactions_NNN / open_orders /
instruments), via `collectorkit.compress` with a decompress-and-verify
pass before the plain file is removed. Compression is best-effort: on
failure the plain file stays and the run still succeeds — `load`
resolves either form. The `run.json` status-lifecycle manifest is
NEVER compressed (it is the forward completeness signal `prune` and
`load` read directly and must stay greppable).

OAuth tokens are loaded from a local token file. Schwab refresh tokens
expire 7 days after issue and cannot be renewed without an interactive
browser login; run `login.py` to mint a fresh token when this script
reports refresh failure.

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
import time
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import _txartefacts
from collectorkit import cli, compress

# schwab-py is a thin wrapper over the Schwab Trader API. We import it
# inside main() so that --help works on a fresh checkout without the
# dependency installed.

log = logging.getLogger("schwab-api")

# Read-only artefact names. Filenames never contain account numbers (plain
# or hashed) so that ls'ing a dest dir does not leak identifiers.
ARTIFACT_ACCOUNT_NUMBERS = "account_numbers.json"
ARTIFACT_USER_PREFERENCE = "user_preference.json"
ARTIFACT_ACCOUNTS_POSITIONS = "accounts_positions.json"
ARTIFACT_TRANSACTIONS_TEMPLATE = "transactions_{n:03d}.json"
ARTIFACT_OPEN_ORDERS = "open_orders.json"
ARTIFACT_INSTRUMENTS = "instruments.json"

# Per-run manifest carrying the normalized status lifecycle
# ("in-progress" at run-dir creation, atomically overwritten with
# "complete" at the end). It is the forward completeness signal `prune`
# keys on; `load` never reads it (its artefact set is
# account_numbers/user_preference/accounts_positions/transactions_*/
# open_orders/instruments), so adding it is inert to silver.
RUN_MANIFEST = "run.json"

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

# HTTP resilience. schwab-py sets a flat 30s timeout on its httpx client;
# Schwab's /transactions and /instruments endpoints regularly take longer,
# so we raise the read timeout and retry transient transport faults
# (read/connect timeouts, dropped connections) with exponential backoff.
# HTTP *status* errors are never retried — see schwab_get_json.
DEFAULT_READ_TIMEOUT_S = 60.0
MAX_TRANSIENT_RETRIES = 4
RETRY_BASE_DELAY_S = 2.0


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--token-path",
        type=Path,
        default=Path.home() / ".secrets" / "schwab-api-token.json",
        help="Path to the schwab-py token JSON file. The file must already "
             "exist (mint it with login.py first). Default: "
             "~/.secrets/schwab-api-token.json.",
    )
    p.add_argument(
        "--dest",
        type=Path,
        default=cli.default_data_root() / "schwab-api",
        help="Output directory (default: %(default)s). Each run "
             "creates a <UTC-timestamp> subdirectory under this path.",
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
    # --since / --until / --lookback — shared contract. No
    # --documents-* (Schwab API has no document archive surface).
    # Schwab caps each transactions call at 1 year; the loader
    # chunks longer ranges automatically via TRANSACTION_WINDOW_DAYS.
    cli.add_lookback_args(p, has_documents=False)
    p.add_argument(
        "--with-instruments",
        action="store_true",
        help="After fetching positions and transactions, look up "
             "metadata (symbol, cusip, description, exchange, type, "
             "assetType) for every instrument that appeared in either, "
             "via /marketdata/v1/instruments. Writes a separate "
             "instruments.json artefact. Off by default — instrument "
             "metadata changes rarely, so this is typically run on a "
             "reduced schedule.",
    )
    p.add_argument(
        "--dry-run",
        action="store_true",
        help="Connect, refresh tokens, list account hashes via "
             "/accounts/accountNumbers, and exit. Does not fetch positions "
             "or transactions and does not write artefacts.",
    )
    p.add_argument(
        "--read-timeout", type=float, default=DEFAULT_READ_TIMEOUT_S,
        metavar="SECONDS",
        help="Per-request read timeout in seconds (default: %(default)s). "
             "schwab-py defaults to a flat 30s; Schwab's /transactions and "
             "/instruments endpoints are often slower, so we raise it and "
             "retry transient timeouts. Bump this if downloads keep hitting "
             "read timeouts.",
    )
    p.add_argument(
        "--debug", action="store_true",
        help="Gate bronze-resident debug artefacts, for parity with the "
             "fleet-wide debug convention. This is a pure REST collector "
             "that writes none — the browser-flow page captures / traces "
             "belong to login.py and land outside the bronze tree — so the "
             "flag currently gates nothing. Off by default. (Use --verbose "
             "for DEBUG-level logging.)",
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


def oauth_error_types() -> tuple[type, ...]:
    """authlib OAuth error classes to catch, or () if authlib is absent.

    Imported lazily so --help works on a checkout without dependencies
    installed (authlib ships transitively with schwab-py)."""
    types: list[type] = []
    try:
        from authlib.integrations.base_client.errors import OAuthError
        types.append(OAuthError)
    except ImportError:
        pass
    try:
        from authlib.oauth2.rfc6749.errors import OAuth2Error
        types.append(OAuth2Error)
    except ImportError:
        pass
    return tuple(types)


def explain_token_failure(token_path: Path, err: Exception) -> str:
    """Turn an authlib OAuth refresh failure into an actionable message.

    `str(err)` carries Schwab's OAuth response (e.g. invalid_grant /
    'Refresh token is invalid, expired or revoked') — a generic OAuth
    error with no account data, safe to surface verbatim."""
    detail = str(err).strip()
    lines = [
        "Schwab rejected the stored OAuth token — cannot authenticate.",
        f"  Schwab said: {detail}",
    ]
    if "invalid_client" in detail.lower():
        lines.append(
            "This is a client-credential problem, not the token: check "
            "SCHWAB_CLIENT_ID / SCHWAB_CLIENT_SECRET (or --client-id / "
            "--client-secret)."
        )
    else:
        lines += [
            "The refresh token has expired or been revoked. Schwab refresh "
            "tokens live 7 days from the last interactive login and cannot "
            "be renewed programmatically.",
            "Fix: mint a new one with login.py, then re-run this download:",
            f"    login.py --token-path {token_path}",
        ]
    return "\n".join(lines)


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


def compress_export(path: Path) -> Path:
    """Compress a freshly written bronze DATA artefact in place
    (`accounts_positions.json` → `accounts_positions.json.zst`),
    returning the on-disk path.

    Best-effort by design: `load` resolves either form, so a compression
    failure (disk full, missing codec) downgrades to a warning and the
    plain file stays — never a lost artefact. `compress.compress_file`
    decompress-and-sha256-verifies the twin before unlinking the
    original, so the window in which data could be lost is nil.
    `run.json` is NEVER routed here (see `write_data_artefact`): the
    status-lifecycle manifest must stay uncompressed and greppable."""
    try:
        final = compress.compress_file(path)
        log.info("Compressed %s → %s (%d bytes)", path.name, final.name,
                 final.stat().st_size)
        return final
    except Exception as exc:  # noqa: BLE001 — best-effort by design
        log.warning("Could not compress %s (%s); keeping the plain file",
                    path.name, exc)
        return path


def write_data_artefact(path: Path, payload) -> Path:
    """Write a bronze DATA artefact as JSON, then zstd-compress it in
    place (best-effort). Returns the on-disk path.

    Deliberately distinct from `write_json`: the `run.json` manifest is
    written via `write_json` directly and so is never compressed, while
    every data artefact goes through here — the separation makes the
    run.json exclusion self-evident rather than a per-call omission."""
    write_json(path, payload)
    return compress_export(path)


def schwab_get_json(response):
    """Validate a schwab-py HTTPX response and return its parsed JSON.

    Raises a SystemExit on non-2xx; we deliberately do not retry HTTP
    *status* errors here (rate-limit / 5xx handling belongs in a
    scheduler). Transient *transport* faults are retried a level up in
    _request_json."""
    if response.status_code // 100 != 2:
        raise SystemExit(
            f"Schwab API returned HTTP {response.status_code}: "
            f"{response.text[:500]}"
        )
    return response.json()


def configure_timeout(client, read_timeout: float) -> None:
    """Raise the schwab-py client's HTTP timeouts above its flat 30s
    default. Reads get a generous window (Schwab's /transactions and
    /instruments endpoints are slow); connect / pool stay short so an
    unreachable host still fails fast rather than hanging."""
    import httpx  # transitive schwab-py dep; import lazily like the rest
    client.set_timeout(httpx.Timeout(
        connect=10.0, read=read_timeout, write=30.0, pool=10.0,
    ))


def _request_json(what: str, call, max_retries: int = MAX_TRANSIENT_RETRIES):
    """Run a schwab-py request (`call` returns the HTTPX response), then
    validate + parse it. Transient transport faults — read/connect
    timeouts and dropped connections — are retried with exponential
    backoff; every GET here is idempotent so a retry is safe. HTTP status
    errors surface immediately via schwab_get_json (not retried)."""
    import httpx
    import httpcore
    # Catch both layers: httpx normally maps httpcore faults to its own
    # exceptions, but an un-mapped httpcore.ReadTimeout can still leak out.
    retryable = (
        httpx.TransportError,          # incl. httpx.TimeoutException
        httpcore.TimeoutException,
        httpcore.NetworkError,
    )
    for attempt in range(1, max_retries + 1):
        try:
            response = call()
            break
        except retryable as e:
            if attempt >= max_retries:
                raise SystemExit(
                    f"Schwab API {what} failed after {max_retries} attempt(s) "
                    f"({type(e).__name__}: {e}). Schwab was slow or "
                    f"unreachable — retry, or raise --read-timeout."
                )
            delay = RETRY_BASE_DELAY_S * (2 ** (attempt - 1))
            log.warning(
                "Schwab API %s: %s (attempt %d/%d); retrying in %.0fs",
                what, type(e).__name__, attempt, max_retries, delay,
            )
            time.sleep(delay)
    return schwab_get_json(response)


def fetch_account_numbers(client) -> list[dict]:
    """GET /accounts/accountNumbers. Returns [{accountNumber, hashValue}, ...]."""
    return _request_json("account numbers", client.get_account_numbers)


def fetch_user_preference(client) -> dict:
    return _request_json("user preferences", client.get_user_preferences)


def fetch_accounts_with_positions(client) -> list[dict]:
    """GET /accounts?fields=positions. Returns one entry per linked account."""
    fields = client.Account.Fields.POSITIONS
    return _request_json(
        "accounts + positions", lambda: client.get_accounts(fields=fields))


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
    all_orders = _request_json(
        "open orders",
        lambda: client.get_orders_for_all_linked_accounts(
            from_entered_datetime=start_dt,
            to_entered_datetime=end_dt,
        ),
    )
    return [o for o in all_orders if o.get("status") in OPEN_ORDER_STATUSES]


def collect_instrument_symbols_from_run(run_dir: Path) -> list[str]:
    """Scan the bronze artefacts in `run_dir` for every distinct symbol.

    Walks accounts_positions.json and every transactions_*.json file,
    extracting `instrument.symbol` from positions and from each
    transferItems entry. The result is the de-duplicated, sorted list of
    symbols we will look up via /instruments. Each artefact is resolved
    to its on-disk variant and decompressed in memory — by the time this
    runs the data artefacts are already zstd-compressed in place."""
    symbols: set[str] = set()

    ap_path = compress.resolve_variant(run_dir / ARTIFACT_ACCOUNTS_POSITIONS)
    if ap_path is not None:
        with compress.open_text(ap_path) as f:
            for wrapper in json.load(f):
                sa = wrapper.get("securitiesAccount") or {}
                for pos in sa.get("positions") or []:
                    sym = (pos.get("instrument") or {}).get("symbol")
                    if sym:
                        symbols.add(sym)

    for txn_path in _txartefacts.transaction_files(run_dir):
        with compress.open_text(txn_path) as f:
            data = json.load(f)
            for txn in data.get("transactions") or []:
                for item in txn.get("transferItems") or []:
                    sym = (item.get("instrument") or {}).get("symbol")
                    if sym:
                        symbols.add(sym)

    return sorted(symbols)


def fetch_instruments(client, symbols: list[str]) -> dict:
    """GET /instruments?projection=symbol-search with class-share normalisation.

    Schwab is inconsistent across its own endpoints on class-share
    tickers: /accounts and /transactions emit the dot form (BRK.B),
    while /instruments only indexes the slash form (BRK/B). For any
    sent symbol containing '.', we include *both* forms in the same
    request. Schwab returns whichever variant it actually knows about;
    we then rewrite '/' back to '.' on the returned `symbol` field so
    the bronze file is symbol-consistent with sibling artefacts and
    silver joins cleanly downstream.

    Symbols that Schwab still does not recognise after this fallback
    are logged for visibility but not retried. They are typically:
    OCC-style option contracts (occ symbols are not indexed here),
    bond CUSIPs (use /instruments/{cusip}, not symbol-search), and
    internal placeholders like CURRENCY_USD."""
    lookup: list[str] = []
    for s in symbols:
        lookup.append(s)
        if "." in s:
            lookup.append(s.replace(".", "/"))

    proj = client.Instrument.Projection.SYMBOL_SEARCH
    response = _request_json(
        "instruments",
        lambda: client.get_instruments(symbols=lookup, projection=proj),
    )

    # Normalise '/'  ->  '.' on returned symbols, and dedup in the rare
    # case where Schwab returned both variants for one ticker.
    seen: set[str] = set()
    normalised: list[dict] = []
    for inst in response.get("instruments") or []:
        sym = inst.get("symbol") or ""
        if "/" in sym:
            sym = sym.replace("/", ".")
            inst["symbol"] = sym
        if sym and sym not in seen:
            seen.add(sym)
            normalised.append(inst)
    response["instruments"] = normalised

    missing = [s for s in symbols if s not in seen]
    if missing:
        preview = missing[:5] + (["..."] if len(missing) > 5 else [])
        log.info("Schwab /instruments did not match %d sent symbol(s): %s",
                 len(missing), preview)
    return response


def fetch_transactions(client, account_hash: str, start: date, end: date) -> list[dict]:
    """GET /accounts/{hash}/transactions for a date range.

    schwab-py expects start_date / end_date as datetimes; we promote
    date -> midnight-UTC start and end-of-day-UTC end so the response
    includes all activity on the boundary days."""
    start_dt = datetime.combine(start, datetime.min.time(), tzinfo=timezone.utc)
    end_dt = datetime.combine(end, datetime.max.time(), tzinfo=timezone.utc)
    types = [client.Transactions.TransactionType[t] for t in TRANSACTION_TYPES_ALL]
    return _request_json(
        "transactions",
        lambda: client.get_transactions(
            account_hash,
            start_date=start_dt,
            end_date=end_dt,
            transaction_types=types,
        ),
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

    # schwab-py's parameter names (api_key, app_secret) are a historical
    # quirk; they accept the OAuth Client ID / Client Secret that Schwab
    # issues in its developer portal. Both building the client and the
    # first request force a token refresh, which is where an expired or
    # revoked refresh token surfaces — catch the authlib error and explain
    # it rather than letting a raw traceback escape.
    oauth_errors = oauth_error_types()
    try:
        log.info("Loading client from token at %s", args.token_path)
        client = schwab_pkg.auth.client_from_token_file(
            token_path=str(args.token_path),
            api_key=client_id,
            app_secret=client_secret,
        )
        configure_timeout(client, args.read_timeout)
        log.info("Listing account hashes ...")
        account_numbers = fetch_account_numbers(client)
    except oauth_errors as e:
        raise SystemExit(explain_token_failure(args.token_path, e))
    log.info("Schwab returned %d linked account(s)", len(account_numbers))

    if args.dry_run:
        # Print only hash prefixes — never plaintext numbers — so dry-run
        # output is safe to paste into bug reports.
        for entry in account_numbers:
            h = entry.get("hashValue", "")
            log.info("  account hash %s... (truncated)", h[:8])
        log.info("Dry run complete. No artefacts written.")
        return 0

    # Default the transaction window to 90 days if not specified —
    # collectorkit's shared default lookback (resolve_lookback). Wider
    # explicit windows are split into <=365-day chunks downstream to
    # respect Schwab's per-request cap.
    today = datetime.now(timezone.utc).date()
    since, until, _, _ = cli.resolve_lookback(args, has_documents=False)
    log.info("Transaction window: %s -> %s", since, until)

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = args.dest / ts
    log.info("Writing artefacts to %s", run_dir)

    # Drop an "in-progress" manifest up front and overwrite it with the
    # terminal status at the end. This makes the run's state legible to
    # `prune` while the walk is still running (a crashed walk leaves
    # status="in-progress" — a non-complete dump prune can reclaim once
    # it goes quiescent) and closes the ambiguity of a run dir with no
    # manifest at all. The first write also mkdirs the run dir.
    write_json(run_dir / RUN_MANIFEST, {"status": "in-progress"})

    write_data_artefact(run_dir / ARTIFACT_ACCOUNT_NUMBERS, account_numbers)

    user_pref = fetch_user_preference(client)
    write_data_artefact(run_dir / ARTIFACT_USER_PREFERENCE, user_pref)

    positions = fetch_accounts_with_positions(client)
    write_data_artefact(run_dir / ARTIFACT_ACCOUNTS_POSITIONS, positions)

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
            write_data_artefact(
                run_dir / ARTIFACT_TRANSACTIONS_TEMPLATE.format(n=n), payload)
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
    write_data_artefact(run_dir / ARTIFACT_OPEN_ORDERS, open_orders_payload)
    log.info("Open orders: %d", len(open_orders))

    # Optional instrument-metadata fetch. Schwab omits `description` on
    # equity positions/transactions but populates it via /instruments
    # (any projection). We harvest every symbol that appeared in the
    # state/event artefacts we just wrote and look them up in one batch.
    instruments_count = None
    if args.with_instruments:
        symbols = collect_instrument_symbols_from_run(run_dir)
        log.info("Fetching instrument metadata for %d unique symbol(s) ...",
                 len(symbols))
        instruments_response = (
            fetch_instruments(client, symbols) if symbols else {"instruments": []}
        )
        write_data_artefact(run_dir / ARTIFACT_INSTRUMENTS, instruments_response)
        instruments_count = len(instruments_response.get("instruments") or [])
        log.info("Instruments: %d returned", instruments_count)

    # Terminal manifest: overwrite the in-progress marker with the
    # finished status + a few counts (no account identifiers). This
    # atomic overwrite is what `prune` reads to classify the dump
    # COMPLETE and keep its load inputs.
    write_json(run_dir / RUN_MANIFEST, {
        "status": "complete",
        "accounts": len(account_numbers),
        "transaction_windows": n,
        "open_orders": len(open_orders),
        "instruments": instruments_count,
    })

    summary = (
        f"Done. Wrote {n} transaction window(s) across "
        f"{len(account_numbers)} account(s); {len(open_orders)} open order(s)"
    )
    if instruments_count is not None:
        summary += f"; {instruments_count} instrument(s)"
    log.info("%s.", summary)
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
