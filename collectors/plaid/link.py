#!/usr/bin/env python3
"""
Link an institution through Plaid, or renew a link.

A link is made on Plaid's own page (Hosted Link):

* this prints the page's URL;
* the sign-in happens in a browser;
* this polls Plaid until the page has made the Item.

No web server runs and no redirect address exists.

What `--item NAME` does depends on what is stored under that name:

* nothing: a new Item is made and its access token is stored;
* an Item: Link opens in update mode. The sign-in renews that Item, and
  no second one is made;
* a sign-in that was started and never settled: it is settled first. An
  Item it made is claimed; a page still open is waited on again.

A wait for a new Item can stop without one: Ctrl-C, an exit on the page,
or the time running out. Plaid is then asked once more how the sign-in
went. An Item it made is claimed. A page that has closed leaves nothing
behind. A page that is still open can still make an Item, and Plaid
cannot close it early, so its record stays: `login` settles it.

Every call belongs to one environment. Production is the default and
reaches real institutions; `--sandbox` reaches Plaid's test institutions
and sees only the Items made there.

Usage:
    link.py --item NAME [--require PRODUCT] [--sandbox] [--mfa-timeout S]
    link.py --item NAME --sandbox-institution ID [--require PRODUCT]
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime
import hashlib
import logging
import os
import sys
import time
from pathlib import Path

from collectorkit import cli, session

import appkeys
import items
import plaidapi
from appkeys import make_client

log = logging.getLogger("plaid.link")

# How long the sign-in page stays open and this command waits for it.
DEFAULT_WAIT_SECONDS = 3600
POLL_SECONDS = 3.0

DEFAULT_COUNTRY_CODES = "US"

# The product a new Item must support when --require names none.
DEFAULT_REQUIRED = "transactions"

# Rebound by the tests.
_sleep = time.sleep
_monotonic = time.monotonic
_time = time.time


class LinkFailed(Exception):
    """A sign-in ended without the outcome asked for. `str()` is the whole
    account of it for the person at the terminal."""


class TimedOut(LinkFailed):
    """The wait ran its full time and no Item came of it."""


def say(text: str = "") -> None:
    print(text, flush=True)


def add_country_codes(p: argparse.ArgumentParser) -> None:
    p.add_argument(
        "--country-codes", default=None, metavar="CC[,CC]",
        help="Countries whose institutions the page offers. Falls back to "
             f"PLAID_COUNTRY_CODES, then {DEFAULT_COUNTRY_CODES}.")


def country_codes(p: argparse.ArgumentParser, value: str | None) -> list[str]:
    raw = value or os.environ.get("PLAID_COUNTRY_CODES") or DEFAULT_COUNTRY_CODES
    codes = [c.strip().upper() for c in raw.split(",") if c.strip()]
    if not all(len(c) == 2 and c.isalpha() for c in codes):
        p.error(f"country codes are two letters each, not {raw!r}")
    return codes


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter)
    appkeys.add_args(p, "link.py")
    p.add_argument(
        "--item", metavar="NAME",
        help="The local name of the Item to link or renew.")
    p.add_argument(
        "--sandbox-institution", metavar="ID",
        help="Sandbox only: make the test Item at this institution id "
             "without the browser step. Implies --sandbox.")
    p.add_argument(
        "--require", choices=plaidapi.DATA_PRODUCTS, default=None,
        help="The one product the login must support. Plaid offers only "
             "institutions that have it and refuses a login with no account "
             "it fits, so name `investments` for a login that holds only "
             "brokerage accounts. The other data products are requested as "
             f"optional. Default: {DEFAULT_REQUIRED}.")
    add_country_codes(p)
    p.add_argument(
        "--mfa-timeout", type=int, default=None, metavar="SECONDS",
        help="How long the sign-in page stays valid and this command waits "
             f"for the sign-in (default {DEFAULT_WAIT_SECONDS}).")
    # `link` is the half of the fleet's `login` that signs in, and takes
    # the same standard flags.
    cli.add_standard_args(p, verb="login")
    args = p.parse_args(argv)

    if args.sandbox_institution:
        args.sandbox = True
    if not args.item:
        p.error("--item NAME is required: it names the Item to link or "
                "renew")
    try:
        items.check_name(args.item)
    except ValueError as e:
        p.error(str(e))
    if args.mfa_timeout is None:
        args.mfa_timeout = DEFAULT_WAIT_SECONDS
    elif not 60 <= args.mfa_timeout <= plaidapi.MAX_LINK_LIFETIME:
        p.error(f"--mfa-timeout takes 60 to {plaidapi.MAX_LINK_LIFETIME} "
                f"seconds")
    args.country_codes = country_codes(p, args.country_codes)
    return args


# ---- reading a link token's sessions ---------------------------------------

def _added_items(doc: dict) -> list[dict]:
    """Every Item the token's sessions made, one entry per public token.

    One token collects a session per visit to its page. A session can
    report the same Item twice: in the current `results` field and in the
    older `on_success` one. So the entries are keyed on the public token.
    """
    found: dict[str, dict] = {}

    def note(public_token, institution):
        if public_token and public_token not in found:
            institution = institution or {}
            found[public_token] = {
                "public_token": public_token,
                "institution_id": institution.get("institution_id"),
                "institution_name": institution.get("name"),
            }

    for link_session in doc.get("link_sessions") or []:
        results = link_session.get("results") or {}
        for added in results.get("item_add_results") or []:
            note(added.get("public_token"), added.get("institution"))
        legacy = link_session.get("on_success") or {}
        note(legacy.get("public_token"),
             (legacy.get("metadata") or {}).get("institution"))
    return list(found.values())


def _exit_of(link_session: dict) -> dict | None:
    """The exit a session ended with, when it did not end in a link."""
    return link_session.get("exit") or link_session.get("on_exit")


def _exit_text(exit_: dict) -> str:
    """Why a session ended, in Plaid's own words where it gave any."""
    error = exit_.get("error") or {}
    text = error.get("display_message") or error.get("error_message")
    if text:
        code = error.get("error_code")
        return f"{text} ({code})" if code else text
    status = (exit_.get("metadata") or {}).get("status")
    return (f"the page was closed before the sign-in finished"
            f"{f' (status: {status})' if status else ''}")


def _poll(client: plaidapi.Client, link_token: str) -> dict | None:
    """One read of the token's sessions. A fault that can pass is logged
    and returns None, so an hour's wait does not end on one bad minute.
    Such a fault is no answer, a server error or a rate limit."""
    try:
        return client.link_token_get(link_token)
    except plaidapi.TransportError as e:
        log.warning("no answer from Plaid (%s); still waiting", e)
    except plaidapi.PlaidError as e:
        if not e.transient:
            raise
        log.warning("%s; still waiting", e)
    return None


def _sessions(doc: dict) -> list[dict]:
    """The token's sessions, oldest first. Plaid lists them in no fixed
    order; `started_at` is one fixed-width UTC format, so it sorts as
    text."""
    return sorted(doc.get("link_sessions") or [],
                  key=lambda s: s.get("started_at") or "")


def _log_progress(doc: dict, seen: set, ignore: frozenset) -> None:
    """Log what the sign-in page has done since the last read, in Plaid's
    own event names. They say where in the flow a person is, and carry no
    account data, so a pasted log shows how far a failed sign-in got.
    Sessions in `ignore` ended before this run and are not replayed."""
    # Plaid lists a session's events newest first and stamps them to the
    # second; reversing before the stable sort keeps events of one second
    # in the order they happened.
    fresh = [e for s in _sessions(doc)
             if s.get("link_session_id") not in ignore
             for e in reversed(s.get("events") or [])
             if e.get("event_id") not in seen]
    for event in sorted(fresh, key=lambda e: e.get("timestamp") or ""):
        seen.add(event.get("event_id"))
        detail = event.get("event_metadata") or {}
        extra = [str(detail[k]) for k in ("view_name", "error_code",
                                          "exit_status") if detail.get(k)]
        log.info("sign-in page: %s%s", event.get("event_name"),
                 f" ({', '.join(extra)})" if extra else "")


def _minutes(seconds: float) -> str:
    n = max(1, round(seconds / 60))
    return f"{n} minute{'' if n == 1 else 's'}"


def clock(when: float) -> str:
    """A Unix time as the local clock shows it, date included: a page can
    live for weeks."""
    return (datetime.datetime.fromtimestamp(when).astimezone()
            .strftime("%Y-%m-%d %H:%M %Z"))


def _wait(client: plaidapi.Client, link_token: str, seconds: float,
          outcome, ignore: frozenset = frozenset()):
    """Poll the token's sessions until `outcome(doc)` returns a result
    other than None, and return it. Raises TimedOut when time runs out."""
    deadline = _monotonic() + seconds
    seen: set = set()
    while True:
        doc = _poll(client, link_token)
        if doc is not None:
            _log_progress(doc, seen, ignore)
            result = outcome(doc)
            if result is not None:
                return result
        if _monotonic() >= deadline:
            raise TimedOut(
                f"The sign-in did not finish within {_minutes(seconds)}.")
        _sleep(POLL_SECONDS)


def _wait_for_new_item(client: plaidapi.Client, link_token: str, *,
                       seconds: float, ignore: frozenset) -> list[dict]:
    """Wait until the page has made an Item, and return what it made.

    Plaid reports an Item as soon as the accounts are confirmed, before
    the page's last screens, and it stays made if the person then leaves.
    So a public token ends the wait whatever else its session says.

    Raises LinkFailed when the newest visit ended in an exit and made
    nothing. `ignore` names the sessions that had ended before this wait
    began: an exit from an earlier run says nothing about a visit under
    way now. A tab that is simply closed never ends its session, and a
    session can be marked finished before its result is readable, so
    neither is read as an answer.
    """
    def outcome(doc):
        added = _added_items(doc)
        if added:
            return added
        fresh = [s for s in _sessions(doc)
                 if s.get("link_session_id") not in ignore]
        if fresh and fresh[-1].get("finished_at") and _exit_of(fresh[-1]):
            raise LinkFailed(
                f"Plaid reports: {_exit_text(_exit_of(fresh[-1]))}")
        return None

    return _wait(client, link_token, seconds, outcome, ignore)


def _wait_for_update(client: plaidapi.Client, link_token: str, *,
                     seconds: float) -> None:
    """Wait until an update-mode visit has run to its end. Raises
    LinkFailed once a visit has ended in an exit and none has completed.

    A completed update reports a public token like a new link does. It
    stands for the Item that exists already, whose access token does not
    change, so it is never exchanged.
    """
    def outcome(doc):
        ended = [s for s in _sessions(doc) if s.get("finished_at")]
        if any(not _exit_of(s) for s in ended):
            return True
        if ended:
            raise LinkFailed(
                f"The renewal did not finish. Plaid reports: "
                f"{_exit_text(_exit_of(ended[-1]))}")
        return None

    _wait(client, link_token, seconds, outcome)


# ---- reporting ---------------------------------------------------------------

def report_item(client: plaidapi.Client, item: items.Item) -> bool:
    """Ask Plaid for the Item's state, print it, and return whether the
    Item is usable. This is the proof a token works: one free read."""
    try:
        doc = client.item_get(item.access_token)
    except plaidapi.PlaidError as e:
        say(f"{item.name}: Plaid refuses the Item: {e}")
        hint = items.remedy(item, e.error_code, e.error_type)
        if hint:
            say(f"  {hint}")
        return False
    except plaidapi.TransportError as e:
        say(f"{item.name}: no answer from Plaid: {e}")
        return False
    state = doc.get("item") or {}
    error = state.get("error")
    if not error:
        say(f"{item.name}: ok")
    elif error.get("error_type") == "ITEM_ERROR":
        say(f"{item.name}: needs a new sign-in")
    else:
        say(f"{item.name}: Plaid reports an error on the Item")

    def row(label: str, value) -> None:
        say(f"  {label:<16} {value}")

    if error:
        row("Plaid reports", f"{error.get('error_code')}: "
                             f"{error.get('error_message')}")
        hint = items.remedy(item, error.get("error_code"),
                            error.get("error_type"))
        if hint:
            row("what to do", hint)
    row("institution",
        " ".join(str(v) for v in (
            state.get("institution_name") or item.institution_name,
            state.get("institution_id") or item.institution_id) if v) or "-")
    row("environment", item.environment)
    row("products", ", ".join(state.get("products") or []) or "-")
    row("consent expires",
        state.get("consent_expiration_time") or "no date reported")
    status = doc.get("status") or {}
    for product in ("transactions", "investments"):
        updated = (status.get(product) or {}).get("last_successful_update")
        if updated:
            row(product, f"updated {updated}")
    return not error


# ---- storing -----------------------------------------------------------------

def _free_name(secrets_dir: Path, wanted: str, item_id: str) -> str:
    """`wanted`, or the nearest name no stored Item has. A token is never
    left unstored for want of a name, so the last resort is a name derived
    from the item id, which is always valid."""
    candidates = [wanted] + [f"{wanted}-{n}" for n in range(2, 100)]
    for name in candidates:
        if (items.ITEM_NAME_RE.match(name)
                and not items.token_path(secrets_dir, name).exists()):
            return name
    return "item-" + hashlib.sha256(item_id.encode()).hexdigest()[:12]


def _claim(client: plaidapi.Client, secrets_dir: Path, name: str,
           added: list[dict]) -> list[items.Item]:
    """Exchange each public token and store the Item it stands for.

    An item id that is stored already was claimed by an earlier run of the
    same sign-in and is left as it is. A token that cannot be written is
    revoked at Plaid, so no access exists that this deployment has no
    record of.
    """
    known = {i.item_id: i
             for i in items.of_environment(secrets_dir, client.environment)}
    claimed, lost = [], []
    for entry in added:
        try:
            exchanged = client.exchange_public_token(entry["public_token"])
        except plaidapi.PlaidError as e:
            if e.transient:
                raise
            # Plaid will not exchange it: a public token lives half an
            # hour, and a sign-in settled later than that made an Item
            # nothing can claim any more.
            lost.append((entry.get("institution_name") or "an institution",
                         e))
            continue
        if exchanged["item_id"] in known:
            claimed.append(known[exchanged["item_id"]])
            continue
        item = items.Item(
            name=_free_name(secrets_dir, name, exchanged["item_id"]),
            environment=client.environment,
            access_token=exchanged["access_token"],
            item_id=exchanged["item_id"],
            institution_id=entry.get("institution_id"),
            institution_name=entry.get("institution_name"),
            linked_at=session.iso_now(),
        )
        try:
            items.save_item(secrets_dir, item)
        except OSError as e:
            raise LinkFailed(
                f"The new Item's token could not be written "
                f"({type(e).__name__}: {e.strerror or e}). "
                f"{_revoke(client, item.access_token)}") from e
        known[item.item_id] = item
        claimed.append(item)
    for institution, error in lost:
        say(f"An earlier sign-in made an Item at {institution} that can no "
            f"longer be claimed. Plaid reports: {error}")
        say("On a Trial plan that Item still counts. The institution lists "
            "the access under its connected apps, where it can be revoked.")
    if lost and not claimed:
        # Nothing more can come of this sign-in, so its record goes: kept,
        # it would be found and fail the same way on every later run.
        items.clear_pending(secrets_dir, name)
        raise LinkFailed(
            f"No Item was stored. "
            f"{items.command(f'link --item {name}', client.environment)} "
            f"links afresh.")
    return claimed


def _revoke(client: plaidapi.Client, access_token: str) -> str:
    try:
        client.item_remove(access_token)
    except (plaidapi.PlaidError, plaidapi.TransportError) as e:
        return (f"Plaid did not revoke it either ({e}), so the institution "
                f"still lists the access; revoke it there.")
    return ("Plaid has revoked it, so no access exists without a record. "
            "On a Trial plan the Item still counts.")


def _finish(client: plaidapi.Client, secrets_dir: Path, name: str,
            added: list[dict]) -> int:
    claimed = _claim(client, secrets_dir, name, added)
    items.clear_pending(secrets_dir, name)
    healthy = True
    for item in claimed:
        say()
        say(f"Linked {item.institution_name or 'the institution'} as "
            f"{item.name!r}. Token file: "
            f"{items.token_path(secrets_dir, item.name)}")
        healthy = report_item(client, item) and healthy
    stored = items.of_environment(secrets_dir, client.environment)
    institutions = [i.institution_id for i in stored if i.institution_id]
    if len(institutions) != len(set(institutions)):
        say()
        say("Two stored Items are at the same institution. Some "
            "institutions keep only the newest link of a login; "
            f"{items.command('login --check', client.environment)} shows "
            f"which Items still answer.")
    if client.environment == "production":
        say()
        say(f"{len(stored)} production Item(s) are stored here. A Trial "
            f"plan allows ten, and removing one does not return its slot.")
    return 0 if healthy else 1


# ---- settling a sign-in ------------------------------------------------------

@dataclasses.dataclass(frozen=True)
class Settled:
    """What became of a sign-in. `status` is the exit status of claiming
    the Item it made. `pending` is its record while the page is open, and
    `ended` the visits to that page that have ended. A sign-in with
    neither a status nor a record was dropped."""
    status: int | None = None
    pending: items.PendingLink | None = None
    ended: frozenset = frozenset()


def _with_expiry(pending: items.PendingLink, doc: dict,
                 secrets_dir: Path) -> items.PendingLink:
    """`pending` with the time its page stops accepting a sign-in: the one
    its file records, else the `expiration` Plaid states for its link
    token. Raises ItemStoreError when neither states one. The file itself
    is left as it is."""
    if isinstance(pending.expires_at, (int, float)) and not isinstance(
            pending.expires_at, bool):
        return pending
    try:
        stated = plaidapi.instant(doc.get("expiration"))
    except (TypeError, ValueError):
        stated = None
    if stated is None:
        raise items.ItemStoreError(
            f"{items.pending_path(secrets_dir, pending.name)} records no "
            f"usable expiry, and Plaid states none for its sign-in either. "
            f"Nothing was changed. If no sign-in on that page is still under "
            f"way, move the file out of the secrets dir and run link again.")
    return dataclasses.replace(pending, expires_at=stated)


def settle(client: plaidapi.Client, secrets_dir: Path,
           pending: items.PendingLink, *, drop: bool = False,
           closed: bool = False) -> Settled:
    """Settle a sign-in as far as Plaid's answer allows.

    An Item the sign-in made is claimed, even after its page has closed.
    With nothing to claim, its record goes once nothing more can come of
    it: Plaid no longer knows its link token, or the page has closed, by
    its expiry or by `closed`. `drop` says an Item holds the name already,
    and the record goes then too. Otherwise the record stays.
    """
    try:
        doc = client.link_token_get(pending.link_token)
    except plaidapi.PlaidError as e:
        # Only Plaid's word that the link token is gone ends a saved
        # sign-in. Any other error says nothing about its page.
        if e.error_code != "INVALID_LINK_TOKEN":
            raise
        log.info("Plaid no longer knows the sign-in (%s)", e)
        doc = None
    if doc is not None and (added := _added_items(doc)):
        return Settled(status=_finish(client, secrets_dir, pending.name,
                                      added))
    if doc is not None and not drop:
        pending = _with_expiry(pending, doc, secrets_dir)
    if doc is None or drop or closed or pending.expires_at <= _time():
        items.clear_pending(secrets_dir, pending.name)
        return Settled()
    return Settled(pending=pending, ended=frozenset(
        s.get("link_session_id") for s in _sessions(doc)
        if s.get("finished_at")))


def _stopped(client: plaidapi.Client, secrets_dir: Path,
             pending: items.PendingLink, stop: BaseException, *,
             closed: bool) -> int:
    """A wait for a new Item stopped without one. Ask Plaid once more, so
    that an Item the page made is claimed, and the record stays only while
    the page can still make one."""
    say()
    say(str(stop) if isinstance(stop, LinkFailed) else "Stopped.")
    status = 1 if isinstance(stop, LinkFailed) else 130
    settles = items.command("login", client.environment)
    try:
        settled = settle(client, secrets_dir, pending, closed=closed)
    except (plaidapi.PlaidError, plaidapi.TransportError) as e:
        say(f"Plaid could not say how the sign-in ended ({e}). Its record "
            f"stays, and {settles} settles it.")
        return status
    if settled.status is not None:
        return settled.status
    if settled.pending is None:
        say("Nothing was linked, and nothing is left behind.")
    else:
        say(f"Nothing was linked. The page stays open until "
            f"{clock(settled.pending.expires_at)}. A sign-in finished there "
            f"still makes an Item, so the sign-in's record stays. {settles} "
            f"claims such an Item, and removes the record once the page "
            f"has closed.")
    return status


# ---- the three ways a link starts ------------------------------------------

def _announce(url: str, environment: str, seconds: float, *,
              renew: bool) -> None:
    say()
    say("Open this page in a browser and sign in at the institution"
        + (" again:" if renew else ":"))
    say()
    say(f"    {url}")
    say()
    if environment == "sandbox":
        say("Sandbox: the test login is user_good / pass_good.")
    say(f"The page stays valid for {_minutes(seconds)}. Waiting for the "
        f"sign-in to finish.")
    say("Ctrl-C stops the wait.")


def _start(client: plaidapi.Client, secrets_dir: Path,
           args: argparse.Namespace) -> items.PendingLink:
    """Ask Plaid for a sign-in page and write the session down before its
    URL is shown. From that point the page can make an Item whether or not
    this process still runs. The link token is then the one handle that
    can ask Plaid what happened."""
    required = args.require or DEFAULT_REQUIRED
    doc = client.link_token_for_new_item(
        required=required, country_codes=args.country_codes,
        lifetime_seconds=args.mfa_timeout)
    if not doc.get("hosted_link_url"):
        raise LinkFailed("Plaid returned no sign-in page for the link.")
    pending = items.PendingLink(
        name=args.item, environment=client.environment,
        link_token=doc["link_token"], hosted_link_url=doc["hosted_link_url"],
        required=required, created_at=session.iso_now(),
        expires_at=int(_time()) + args.mfa_timeout)
    items.save_pending(secrets_dir, pending)
    return pending


def _renew(client: plaidapi.Client, item: items.Item,
           args: argparse.Namespace) -> int:
    say(f"Item {item.name!r} is linked already. Link opens in update mode: "
        f"the sign-in renews this Item and makes no new one.")
    try:
        doc = client.link_token_for_update(
            access_token=item.access_token, country_codes=args.country_codes,
            lifetime_seconds=args.mfa_timeout)
    except plaidapi.PlaidError as e:
        # Update mode needs a token Plaid accepts. For the two answers
        # that say it does not, the way on is not another renewal.
        if e.error_code not in ("ITEM_NOT_FOUND", "INVALID_ACCESS_TOKEN"):
            raise
        raise LinkFailed(f"Plaid reports: {e}. "
                         f"{items.remedy(item, e.error_code, e.error_type)}"
                         ) from e
    if not doc.get("hosted_link_url"):
        raise LinkFailed("Plaid returned no sign-in page for the renewal.")
    _announce(doc["hosted_link_url"], item.environment, args.mfa_timeout,
              renew=True)
    _wait_for_update(client, doc["link_token"], seconds=args.mfa_timeout)
    say()
    return 0 if report_item(client, item) else 1


def _sandbox_item(client: plaidapi.Client,
                  args: argparse.Namespace) -> list[dict]:
    """Make a test Item at a Sandbox institution with no browser step. The
    products follow the same rule as a real link: the required one must be
    offered, the others are added where the institution has them."""
    required = args.require or DEFAULT_REQUIRED
    institution = client.institution_get_by_id(
        args.sandbox_institution, args.country_codes)["institution"]
    offered = institution.get("products") or []
    if required not in offered:
        raise LinkFailed(
            f"{institution.get('name')} does not offer {required}; it "
            f"offers: {', '.join(sorted(offered))}")
    products = [required] + [p for p in plaidapi.DATA_PRODUCTS
                             if p != required and p in offered]
    doc = client.sandbox_public_token_create(
        args.sandbox_institution, products)
    return [{"public_token": doc["public_token"],
             "institution_id": institution.get("institution_id"),
             "institution_name": institution.get("name")}]


def link(args: argparse.Namespace, client: plaidapi.Client) -> int:
    """`link --item NAME`: settle, renew or make the Item of that name.
    One run at a time works on a name's sign-in."""
    with items.held(args.secrets_dir, args.item) as mine:
        if not mine:
            raise LinkFailed(
                f"Another run is working on the sign-in of {args.item!r}. "
                f"Run link again once it has ended.")
        return _link(args, client)


def _link(args: argparse.Namespace, client: plaidapi.Client) -> int:
    secrets_dir, name = args.secrets_dir, args.item
    existing = items.load_item(secrets_dir, name)
    pending = items.load_pending(secrets_dir, name)
    for record in (existing, pending):
        if record is not None:
            items.require_environment(name, record.environment,
                                      client.environment)
    if existing is not None and (args.require or args.sandbox_institution):
        raise SystemExit(
            f"Item {name!r} exists and keeps the products it was linked "
            f"with; --require and --sandbox-institution apply to a new "
            f"Item only.")

    # A sign-in an earlier run started comes first: it may have made an
    # Item nothing here has claimed yet.
    ended: frozenset = frozenset()
    if pending is not None:
        settled = settle(client, secrets_dir, pending,
                         drop=existing is not None)
        if settled.status is not None:
            return settled.status
        pending, ended = settled.pending, settled.ended

    if existing is not None:
        return _renew(client, existing, args)
    if args.sandbox_institution:
        return _finish(client, secrets_dir, name, _sandbox_item(client, args))
    if pending is None:
        pending = _start(client, secrets_dir, args)
    remaining = max(1, pending.expires_at - _time())
    seconds = min(args.mfa_timeout, remaining)
    _announce(pending.hosted_link_url, client.environment, seconds,
              renew=False)
    try:
        added = _wait_for_new_item(client, pending.link_token,
                                   seconds=seconds, ignore=ended)
    except (LinkFailed, KeyboardInterrupt) as stop:
        # A wait as long as the page's life ends when the page closes.
        return _stopped(client, secrets_dir, pending, stop,
                        closed=isinstance(stop, TimedOut)
                        and seconds >= remaining)
    status = _finish(client, secrets_dir, name, added)
    # Plaid reports the Item before the page's last screens, so the page
    # is usually still open at this point.
    say()
    say("The link is stored. The rest of the page can be finished or "
        "closed.")
    return status


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    appkeys.source_env_file(args.env_file)
    client = make_client(appkeys.environment(args), args.client_id)
    try:
        return link(args, client)
    except LinkFailed as e:
        say()
        say(str(e))
        return 1
    except items.ItemStoreError as e:
        say(str(e))
        return 1
    except plaidapi.PlaidError as e:
        say(f"Plaid reports: {e}")
        return 1
    except plaidapi.TransportError as e:
        say(f"No answer from Plaid: {e}")
        return 1
    except KeyboardInterrupt:
        say()
        say("Stopped.")
        if items.pending_path(args.secrets_dir, args.item).exists():
            say(f"The sign-in's record stays. "
                f"{items.command('login', client.environment)} settles it.")
        return 130


if __name__ == "__main__":
    sys.exit(main())
