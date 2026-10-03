#!/usr/bin/env python3
"""
Download what Plaid holds for each linked Item into bronze.

Each Item has its own tree, <bronze-dir>/<item>/, and each run one
directory in it, named by its UTC start time:

    run.json                            status, window, what each product held
    item.json                           /item/get: the Item and its update times
    accounts.json                       /accounts/get: accounts and balances
    holdings.json                       /investments/holdings/get
    investment_transactions-NNNN.json   /investments/transactions/get, by page
    transactions-NNNN.json              /transactions/get, by page
    liabilities.json                    /liabilities/get

A tree holds the runs of one Item. A tree that holds anything else is
refused and left as it is.

Without `--refresh`, every read is of the copy Plaid keeps, and none
reaches the institution. An Item is read only for the products it was
linked with: asking for another would add that product to the Item. The
first read of an Item's investment transactions adds Plaid's
subscription for them to the Item; no other read changes it.

`--refresh` first asks Plaid to fetch each Item's investments from the
institution, then waits until Plaid stamps the Item with a fetch, and only
then reads. run.json records the outcome under `refresh`. Plaid bills a
successful refresh on a paid plan. A Production run asks for one only
when plaid.cfg opts in to it; the Trial plan and the Sandbox do not bill.

`--lookback` bounds the two ledgers. Balances, holdings and liabilities
are read whole on every run.

`--dry-run` reads each Item and its accounts, says what a run would read,
and writes nothing. Both reads are free.

Usage:
    download.py --bronze-dir DIR [--item NAME ...] [--sandbox]
                [--lookback PRESET|YYYY-MM-DD] [--refresh] [--dry-run]
                [--debug]
"""

from __future__ import annotations

import argparse
import functools
import logging
import sys
import time
from dataclasses import dataclass
from pathlib import Path

from collectorkit import bronze, cli, debugcap

import appkeys
import config
import items
import plaidapi
import trees
from appkeys import make_client
from trees import (ABSENT, FAILED, FETCHED, NOT_LINKED, NOT_READY, PARTIAL,
                   REFRESH_SETTLED, REFRESHED, REFUSED, SETTLED, UNCONFIRMED)

log = logging.getLogger("plaid.download")

# Rebound by the tests.
_sleep = time.sleep
_monotonic = time.monotonic

# A fault on Plaid's side, or a rate limit, is asked again after these
# waits, in seconds.
TRANSIENT_WAITS = (10.0, 30.0)

# Plaid answers PRODUCT_NOT_READY while it assembles a new Item's data.
# It is asked again at this interval, for at most this long.
NOT_READY_POLL = 20.0
NOT_READY_WAIT = 300.0

# A ledger that changes while it is paged is read at most this many times.
PAGING_ATTEMPTS = 3

# The route `--refresh` asks for. After asking, the Item's update stamps
# are read at this interval, for at most this long. Plaid holds the
# request open while it fetches, so the stamps have usually moved by the
# first read.
REFRESH_ROUTE = "/investments/refresh"
REFRESH_POLL = 10.0
REFRESH_WAIT = 120.0

# Answers to a refresh that come with a hint of what they mean. Plaid's
# page for the route spells the first one in the singular.
REFRESH_UNSUPPORTED = frozenset({"PRODUCT_NOT_SUPPORTED",
                                 "PRODUCTS_NOT_SUPPORTED"})
REFRESH_NOT_ENABLED = frozenset({"INVALID_PRODUCT",
                                 "UNAUTHORIZED_ROUTE_ACCESS"})

# Answers that say the Item has no account the product applies to.
ABSENT_CODES = frozenset({"NO_INVESTMENT_ACCOUNTS", "NO_LIABILITY_ACCOUNTS"})

# Plaid's word that it holds the ledger's whole history. Until then it
# holds the newest rows only.
HISTORY_COMPLETE = "HISTORICAL_UPDATE_COMPLETE"



@dataclass(frozen=True)
class Product:
    """One thing a run reads beyond the Item and its accounts."""

    name: str                  # its entry in run.json, and its file names
    linked_by: str             # the Item product it belongs to
    read: str                  # the Client method that reads it
    rows: str | None           # the list that holds its rows in an answer
    total: str | None = None   # a paged ledger: the total each page states
    row_id: str | None = None  # a paged ledger: the id of each row
    history: bool = False      # Plaid states how much history it holds

    @property
    def ledger(self) -> bool:
        return self.total is not None

    @property
    def label(self) -> str:
        """Its name as the log writes it."""
        return self.name.replace("_", " ")


# In the order a run reads them.
PRODUCTS = (
    Product("holdings", "investments", "holdings_get", "holdings"),
    Product("investment_transactions", "investments",
            "investment_transactions_get", "investment_transactions",
            "total_investment_transactions",
            trees.LEDGER_IDS["investment_transactions"]),
    Product("transactions", "transactions", "transactions_get",
            "transactions", "total_transactions",
            trees.LEDGER_IDS["transactions"], history=True),
    Product("liabilities", "liabilities", "liabilities_get", None),
)


class ItemFailed(Exception):
    """The Item could not be read at all. `str()` says why and what to
    do about it."""


class ReadFailed(Exception):
    """A read that Plaid answered, and that still gave no usable result."""


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument(
        "--bronze-dir", type=Path, required=True,
        help="Plaid's own data dir. Each Item has its tree of runs in it.")
    appkeys.add_args(p, "download.py")
    p.add_argument(
        "--item", metavar="NAME", action="append",
        help="Read only this Item. Repeat for more. Default: every Item of "
             "the environment.")
    cli.add_standard_args(p, verb="download")
    p.add_argument(
        "--refresh", action="store_true",
        help="First ask Plaid to fetch each Item's investments from the "
             "institution, and wait until it reports the fetch. Plaid bills "
             "a successful refresh on a paid plan, so a Production run asks "
             "only with the opt-in in plaid.cfg. The Trial plan and the "
             "Sandbox do not bill it.")
    p.add_argument(
        "--dry-run", action="store_true",
        help="Read each Item and its accounts, say what a run would read, "
             "and write nothing.")
    p.add_argument(
        "--debug", action="store_true",
        help="Record each HTTP exchange in the run, at "
             "<run>/screenshots/http-trace.jsonl: status, timing and size. "
             "No body, key or token is written. The trace is not a load "
             "input, and `prune` removes it.")
    args = p.parse_args(argv)
    if args.dry_run and args.debug:
        p.error("--debug records into a run, and --dry-run makes none")
    if args.dry_run and args.refresh:
        p.error("--refresh fetches from the institution, and --dry-run "
                "reads only Plaid's copy")
    for name in args.item or []:
        try:
            items.check_name(name)
        except ValueError as e:
            p.error(str(e))
    return args


# ---- one read ------------------------------------------------------------------

def _call(fn, what: str):
    """Make one read, waiting out the answers that pass: a fault on
    Plaid's side, a rate limit, and a product Plaid is still assembling.
    Anything else is raised as Plaid gave it."""
    waits = iter(TRANSIENT_WAITS)
    ready_by = None
    while True:
        try:
            return fn()
        except plaidapi.PlaidError as e:
            if e.error_code == "PRODUCT_NOT_READY":
                if ready_by is None:
                    ready_by = _monotonic() + NOT_READY_WAIT
                    log.info("%s: Plaid is still assembling it; waiting up "
                             "to %.0f minutes", what, NOT_READY_WAIT / 60)
                if _monotonic() < ready_by:
                    _sleep(NOT_READY_POLL)
                    continue
                raise
            wait = next(waits, None) if e.transient else None
            if wait is None:
                raise
            log.warning("%s; asking again in %.0f s", e, wait)
            _sleep(wait)


def _paged(read, product: Product, what: str) -> list[dict]:
    """Every page of a paged ledger, in order.

    Plaid pages by offset, so a row that arrives or leaves mid-read
    shifts the pages after it. The read starts again when a page states
    another total, when a row comes back a second time, or when the pages
    do not add up to the total. One change shows in none of these: a row
    that leaves a page already read while another arrives in a page not
    read yet. The next run reads the window again and makes up for it.
    """
    for _ in range(PAGING_ATTEMPTS):
        pages: list[dict] = []
        seen: set = set()
        total = None
        while True:
            page = _call(functools.partial(read, len(seen)), what)
            ids = [row.get(product.row_id)
                   for row in page.get(product.rows) or []]
            if total is None:
                total = page.get(product.total) or 0
            if (page.get(product.total) or 0) != total \
                    or len(set(ids)) != len(ids) or seen.intersection(ids):
                break
            seen.update(ids)
            pages.append(page)
            if len(seen) >= total:
                if len(seen) == total:
                    return pages
                break
            if not ids:
                break
        log.info("%s changed while it was read; reading it again", what)
    raise ReadFailed(f"the {what} changed while it was read, "
                     f"{PAGING_ATTEMPTS} times over")


# ---- one product ----------------------------------------------------------------

def _read_product(client: plaidapi.Client, token: str, product: Product,
                  since, until) -> tuple[list[tuple[str, dict]], str | None]:
    """The files one product writes, as (name, answer) pairs, and for the
    bank and card ledger Plaid's word on how much history it holds."""
    what = product.label
    read = getattr(client, product.read)
    if not product.ledger:
        return [(f"{product.name}.json",
                 _call(functools.partial(read, token), what))], None
    history = None
    if product.history:
        # Asked before the pages, so a history Plaid completes mid-read
        # is never claimed for rows read before it was complete.
        history = _call(
            functools.partial(client.ledger_history_status, token),
            f"{what} history")
    pages = _paged(lambda offset: read(token, start=since, end=until,
                                       offset=offset), product, what)
    return [(f"{product.name}-{n:04d}.json", page)
            for n, page in enumerate(pages, 1)], history


def _count(product: Product, docs) -> int:
    if product.rows is None:
        # Liabilities: one list per kind of account.
        return sum(len(rows or []) for doc in docs
                   for rows in (doc.get("liabilities") or {}).values())
    return sum(len(doc.get(product.rows) or []) for doc in docs)


def _outcome(error: Exception) -> dict:
    """What run.json records for a product whose read did not succeed."""
    if not isinstance(error, plaidapi.PlaidError):
        return {"status": FAILED, "error": str(error)}
    entry = {"error_code": error.error_code,
             "error_message": error.error_message}
    if error.error_code in ABSENT_CODES:
        return {"status": ABSENT, **entry}
    if error.error_code == "PRODUCT_NOT_READY":
        return {"status": NOT_READY, **entry}
    return {"status": FAILED, **entry}


# ---- one Item ---------------------------------------------------------------------

def _no_item_error(state: dict) -> None:
    error = (state.get("item") or {}).get("error")
    if error:
        raise plaidapi.PlaidError("/item/get", 200, error)


def _read_item(client: plaidapi.Client, item: items.Item, *,
               refresh=None) -> tuple[dict, dict]:
    """The Item and its accounts. Raises ItemFailed when either cannot be
    read, since nothing else means anything without the accounts. Raises
    ItemFailed too when Plaid reports an error on the Item. Plaid answers
    every read of such an Item with that error, and update mode clears it.

    `refresh`, when given, runs between the two reads. It takes the Item's
    state and returns the state after the refresh, so the accounts read
    after it."""
    token = item.access_token
    try:
        state = _call(functools.partial(client.item_get, token), "the Item")
        _no_item_error(state)
        if refresh is not None:
            state = refresh(state)
            _no_item_error(state)
        accounts = _call(functools.partial(client.accounts_get, token),
                         "accounts")
    except plaidapi.PlaidError as e:
        raise ItemFailed(
            f"{item.name}: Plaid reports: {e}. "
            f"{items.remedy(item, e.error_code, e.error_type)}".rstrip()
        ) from e
    except plaidapi.TransportError as e:
        raise ItemFailed(f"{item.name}: no answer from Plaid: {e}") from e
    return state, accounts


def _stamps(state: dict) -> tuple:
    """Plaid's last successful and last failed investments update of the
    Item, as Plaid states them. Either may be absent."""
    investments = (state.get("status") or {}).get("investments") or {}
    return (investments.get("last_successful_update"),
            investments.get("last_failed_update"))


def _refusal_hint(item: items.Item, error: plaidapi.PlaidError) -> str:
    if error.error_code == "PRODUCT_NOT_READY":
        return ("Plaid is still assembling the Item's investments. A later "
                "run can refresh them.")
    if error.error_code in REFRESH_UNSUPPORTED:
        return ("The institution does not support a refresh. Plaid updates "
                "investments on its own at least once each market day.")
    if error.error_code in REFRESH_NOT_ENABLED:
        return ("Investments Refresh is not enabled for these app keys. The "
                "Plaid Dashboard's Products page shows what is.")
    if error.status == 429:
        return ("Plaid allows one refresh a minute, ten an hour and twenty "
                "a day for each Item.")
    return items.remedy(item, error.error_code, error.error_type)


def _refresh(client: plaidapi.Client, item: items.Item, manifest: dict,
             state: dict) -> dict:
    """Ask Plaid to fetch the Item's investments from the institution now,
    and wait until Plaid reports a fetch. Returns the Item's state after
    the wait, and records the outcome in the manifest under `refresh`.

    The request is made once: Plaid bills each successful one on a paid
    plan, so a fault is never answered by asking again. Plaid stamps the
    Item each time it reaches the institution, whether or not anything
    changed. A stamp that moves after the request is the sign that a fetch
    ran. No webhook is set up to say so.
    """
    if "investments" not in ((state.get("item") or {}).get("products")
                             or []):
        manifest["refresh"] = {"status": NOT_LINKED}
        log.info("%s: refresh: the Item was not linked with investments",
                 item.name)
        return state
    before = _stamps(state)
    entry = {"route": REFRESH_ROUTE, "status": UNCONFIRMED}
    manifest["refresh"] = entry
    log.info("%s: asking Plaid to refresh investments from the institution",
             item.name)
    answered = "accepted the request"
    try:
        entry["request_id"] = client.investments_refresh(
            item.access_token).get("request_id")
    except plaidapi.PlaidError as e:
        entry.update(error_code=e.error_code, error_message=e.error_message,
                     request_id=e.request_id)
        if e.error_code in ABSENT_CODES:
            entry["status"] = ABSENT
            log.info("%s: refresh: none (%s)", item.name, e.error_code)
            return state
        if 400 <= e.status < 500:
            entry["status"] = REFUSED
            hint = _refusal_hint(item, e)
            log.error("%s: refresh refused: %s.%s The run reads Plaid's "
                      "stored copy.", item.name, e,
                      f" {hint}" if hint else "")
            return state
        # Any other answer, such as a fault on Plaid's side, says nothing
        # of the fetch; the stamps do.
        answered = f"answered {e}"
    except plaidapi.TransportError as e:
        entry["error"] = str(e)
        answered = f"gave no answer ({e})"
    deadline = _monotonic() + REFRESH_WAIT
    while True:
        state = _call(functools.partial(client.item_get, item.access_token),
                      "the Item")
        success, failure = _stamps(state)
        entry.update(last_successful_update=success,
                     last_failed_update=failure)
        if (state.get("item") or {}).get("error"):
            # The run fails on the Item's error, with its remedy, as soon
            # as this returns.
            entry["status"] = FAILED
            return state
        if success != before[0]:
            entry["status"] = REFRESHED
            log.info("%s: refreshed: Plaid fetched from the institution "
                     "(%s)", item.name, success)
            return state
        if failure != before[1]:
            entry["status"] = FAILED
            log.error("%s: refresh: Plaid could not fetch from the "
                      "institution (%s). The run reads Plaid's stored copy.",
                      item.name, failure)
            return state
        if _monotonic() >= deadline:
            log.warning("%s: refresh: Plaid %s, and reported no fetch within "
                        "%.0f s. The run reads what Plaid holds.", item.name,
                        answered, REFRESH_WAIT)
            return state
        _sleep(REFRESH_POLL)


def _linked(state: dict) -> list[Product]:
    linked = set((state.get("item") or {}).get("products") or [])
    return [p for p in PRODUCTS if p.linked_by in linked]


def _again(item: items.Item, lookback: str | None) -> str:
    """The command that reads the Item over the same window again."""
    return items.command(
        f"download --item {item.name}"
        + (f" --lookback {lookback}" if lookback else ""), item.environment)


def _tree_ok(tree: Path, item: items.Item) -> bool:
    try:
        trees.check(tree, item)
    except trees.TreeError as e:
        log.error("%s", e)
        return False
    except OSError as e:
        log.error("%s: cannot read %s: %s", item.name, tree, e)
        return False
    return True


def dry_run(client: plaidapi.Client, item: items.Item, root: Path, since,
            until) -> bool:
    if not _tree_ok(root / item.name, item):
        return False
    try:
        state, accounts = _read_item(client, item)
    except ItemFailed as e:
        log.error("%s", e)
        return False
    log.info("%s: %d accounts; a run would read accounts, %s; ledgers from "
             "%s to %s", item.name, len(accounts.get("accounts") or []),
             ", ".join(p.label for p in _linked(state))
             or "nothing else", since, until)
    return True


def download_item(client: plaidapi.Client, item: items.Item, root: Path,
                  since, until, *, debug: bool, refresh: bool = False,
                  lookback: str | None = None) -> bool:
    """Read one Item into a new run. Returns False unless the Item and
    every product it was linked with were read in full, and, with
    `refresh`, unless Plaid reported the fetch it was asked for.

    A run whose Item cannot be read ends as `failed`, with the reason. A
    run stopped by a write error or by Ctrl-C stays `in-progress`. Neither
    is a load input, and `prune` removes both."""
    tree = root / item.name
    if not _tree_ok(tree, item):
        return False
    slug = bronze.ts_slug()
    run = bronze.run_dir(tree, slug)
    manifest = {
        "status": trees.IN_PROGRESS, "slug": slug, "item": item.name,
        "environment": item.environment, "item_id": item.item_id,
        "api_version": plaidapi.API_VERSION,
        "since": since.isoformat(), "until": until.isoformat(),
        "products": {},
    }
    try:
        bronze.atomic_write_json(run / trees.RUN_FILE, manifest)
        trace = debugcap.HttpTrace(run, log=log, enabled=debug)
        client.on_exchange = (
            lambda url, status, elapsed, size, error: trace.record(
                "POST", url, status=status, elapsed_ms=elapsed,
                bytes_=size, error=error))
        try:
            state, accounts = _read_item(
                client, item,
                refresh=functools.partial(_refresh, client, item, manifest)
                if refresh else None)
        except ItemFailed as e:
            log.error("%s", e)
            manifest.update(status=trees.RUN_FAILED, reason=str(e))
            bronze.atomic_write_json(run / trees.RUN_FILE, manifest)
            return False
        bronze.atomic_write_json(run / trees.ITEM_FILE, state)
        bronze.atomic_write_json(run / "accounts.json", accounts)
        manifest["products"]["accounts"] = {
            "status": FETCHED, "files": ["accounts.json"],
            "rows": len(accounts.get("accounts") or [])}
        healthy = ("refresh" not in manifest
                   or manifest["refresh"]["status"] in REFRESH_SETTLED)
        linked = _linked(state)
        for product in PRODUCTS:
            entry = (_download_product(client, item, product, run, since,
                                       until)
                     if product in linked else {"status": NOT_LINKED})
            manifest["products"][product.name] = entry
            _log_product(item, product, entry, _again(item, lookback))
            healthy = healthy and entry["status"] in SETTLED
        # A write recreates a run dir that was removed under the run, so a
        # run pruned mid-way would otherwise end complete with files gone.
        missing = trees.missing_files(run, manifest["products"])
        if missing:
            log.error("%s: %s lost %s while it was written; it stays "
                      "in-progress", item.name, run, ", ".join(missing))
            return False
        manifest["status"] = trees.COMPLETE
        bronze.atomic_write_json(run / trees.RUN_FILE, manifest)
    except OSError as e:
        log.error("%s: cannot write the run: %s. %s stays in-progress.",
                  item.name, e, run)
        return False
    finally:
        client.on_exchange = None
    log.info("%s: wrote %s", item.name, run)
    return healthy


def _download_product(client: plaidapi.Client, item: items.Item,
                      product: Product, run: Path, since, until) -> dict:
    """Read one product into the run, and return its run.json entry. Its
    files are written only once every page is in."""
    try:
        files, history = _read_product(client, item.access_token, product,
                                       since, until)
    except (plaidapi.PlaidError, plaidapi.TransportError, ReadFailed) as e:
        return _outcome(e)
    for name, doc in files:
        bronze.atomic_write_json(run / name, doc)
    entry = {"status": FETCHED, "files": [name for name, _ in files],
             "rows": _count(product, [doc for _, doc in files])}
    if product.ledger:
        entry.update(since=since.isoformat(), until=until.isoformat())
    if product.history:
        entry["history"] = history
        if history != HISTORY_COMPLETE:
            entry["status"] = PARTIAL
    return entry


def _log_product(item: items.Item, product: Product, entry: dict,
                 again: str) -> None:
    what = product.label
    status = entry["status"]
    if status == FETCHED:
        log.info("%s: %s: %d rows", item.name, what, entry["rows"])
    elif status == PARTIAL:
        log.warning("%s: %s: %d rows, and Plaid holds only part of the "
                    "history so far (%s). Once it holds the rest, %s reads "
                    "it.", item.name, what, entry["rows"], entry["history"],
                    again)
    elif status == NOT_LINKED:
        log.info("%s: %s: the Item was not linked with it", item.name, what)
    elif status == ABSENT:
        log.info("%s: %s: none (%s)", item.name, what, entry["error_code"])
    elif status == NOT_READY:
        log.warning("%s: %s: Plaid was still assembling it after %.0f "
                    "minutes. Once it is ready, %s reads it.", item.name,
                    what, NOT_READY_WAIT / 60, again)
    else:
        log.error("%s: %s: not read: %s", item.name, what,
                  entry.get("error_message") or entry.get("error"))


# ---- the run ------------------------------------------------------------------------

def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    appkeys.source_env_file(args.env_file)
    environment = appkeys.environment(args)
    try:
        chosen, unreadable = items.select(args.secrets_dir, environment,
                                          args.item)
    except items.ItemStoreError as e:
        log.error("%s", e)
        return 1
    for e in unreadable:
        log.error("%s", e)
    if not chosen:
        if not unreadable:
            log.error("no %s Item is linked; %s links one", environment,
                      items.command("link --item NAME", environment))
        return 1
    since, until = cli.resolve_lookback(args)
    billed = frozenset()
    if args.refresh and environment == "production":
        try:
            billed = config.load().billed_reads
        except config.ConfigError as e:
            log.error("%s", e)
            return 1
        if REFRESH_ROUTE not in billed:
            log.error("--refresh asks Plaid for %s, which Plaid bills per "
                      "successful call on a paid plan. The Trial plan and "
                      "the Sandbox do not bill it. A production run asks for "
                      "it only with this opt-in in %s: "
                      '{"billed_reads": ["%s"]}', REFRESH_ROUTE,
                      config.path(), REFRESH_ROUTE)
            return 1
    client = make_client(environment, args.client_id)
    client.billed_reads = billed
    healthy = not unreadable
    try:
        for item in chosen:
            if args.dry_run:
                ok = dry_run(client, item, args.bronze_dir, since, until)
            else:
                ok = download_item(client, item, args.bronze_dir, since,
                                   until, debug=args.debug,
                                   refresh=args.refresh,
                                   lookback=args.lookback)
            healthy = healthy and ok
    except KeyboardInterrupt:
        log.warning("stopped. A run under way stays in-progress; `prune` "
                    "removes it.")
        return 130
    return 0 if healthy else 1


if __name__ == "__main__":
    sys.exit(main())
