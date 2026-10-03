#!/usr/bin/env python3
"""
Settle the sign-ins `link` left open, or probe what is linked.

`login` opens no sign-in page and makes no new link. A `link` run that
stopped can leave a sign-in page open, and the page can still make an
Item. `login` asks Plaid once about each sign-in left open:

* it made an Item: the Item is claimed and its token stored;
* it made nothing and its page has closed: its record is removed;
* its page is still open: it is left as it is.

A sign-in that a `link` run is still waiting on is left to that run.
With no sign-in left open, `login` does nothing and asks Plaid nothing.

`--check` reads only. It asks Plaid whether it accepts the app keys, then
asks for each stored Item's state, and lists the sign-ins left open. Its
calls are free and reach no institution.

Every call belongs to one environment. Production is the default;
`--sandbox` sees only the Items and sign-ins made in the Sandbox.

Usage:
    login.py [--item NAME] [--sandbox]
    login.py --check [--item NAME] [--sandbox]
"""

from __future__ import annotations

import argparse
import sys

from collectorkit import cli

import appkeys
import items
import link
import plaidapi
from appkeys import make_client
from link import say


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter)
    appkeys.add_args(p, "login.py")
    p.add_argument(
        "--item", metavar="NAME",
        help="Settle or check only the Item of this name.")
    p.add_argument(
        "--check", action="store_true",
        help="Probe the app keys and the stored Items, and change nothing. "
             "Exit 0 when Plaid accepts the keys and every Item is healthy.")
    link.add_country_codes(p)
    cli.add_standard_args(p, verb="login")
    args = p.parse_args(argv)

    if args.item:
        try:
            items.check_name(args.item)
        except ValueError as e:
            p.error(str(e))
    if args.country_codes and not args.check:
        p.error("--country-codes applies to --check only")
    args.country_codes = link.country_codes(p, args.country_codes)
    return args


def settle_all(args: argparse.Namespace, environment: str) -> int:
    """`login`: settle every sign-in `link` left open. The client is made
    only once there is a sign-in to ask about, so a deployment with none
    needs no keys for this."""
    names = [args.item] if args.item else None
    left, unreadable = items.open_sign_ins(args.secrets_dir, environment,
                                           names)
    for e in unreadable:
        say(str(e))
    if not left:
        if args.item:
            say(f"No sign-in of {args.item!r} is left open. "
                f"{items.command(f'link --item {args.item}', environment)} "
                f"links or renews it.")
        else:
            say(f"No {environment} sign-in is left open.")
        return 1 if unreadable else 0

    client = make_client(environment, args.client_id)
    status = 1 if unreadable else 0
    for found in left:
        name = found.name
        with items.held(args.secrets_dir, name) as mine:
            if not mine:
                say(f"{name}: a link run is waiting on this sign-in, and it "
                    f"is left to that run.")
                continue
            try:
                # Read again under the hold: a run that ended a moment ago
                # may have settled it.
                pending = items.load_pending(args.secrets_dir, name)
                if pending is None:
                    continue
                settled = link.settle(
                    client, args.secrets_dir, pending,
                    drop=items.load_item(args.secrets_dir, name) is not None)
            except (link.LinkFailed, items.ItemStoreError) as e:
                say(f"{name}: {e}")
                status = 1
                continue
            except (plaidapi.PlaidError, plaidapi.TransportError) as e:
                say(f"{name}: Plaid could not say how the sign-in ended "
                    f"({e}). Its record stays.")
                status = 1
                continue
        if settled.status is not None:
            status = max(status, settled.status)
        elif settled.pending is None:
            say(f"{name}: the sign-in made no Item. Its record is removed.")
        else:
            say(f"{name}: the sign-in page is open until "
                f"{link.clock(settled.pending.expires_at)}. An Item made "
                f"there is claimed by the next "
                f"{items.command('login', environment)}.")
    return status


def check(args: argparse.Namespace, client: plaidapi.Client) -> int:
    """`login --check`: the app keys, then each Item, in one environment,
    then the sign-ins left open."""
    environment = client.environment
    try:
        client.institutions_first(args.country_codes)
    except plaidapi.PlaidError as e:
        say(f"{environment}: Plaid rejects the app keys: {e}")
        return 1
    except plaidapi.TransportError as e:
        say(f"{environment}: no answer from Plaid: {e}")
        return 1
    say(f"{environment}: Plaid accepts the app keys")
    names = [args.item] if args.item else None
    linked, unreadable = items.select(args.secrets_dir, environment, names)
    for e in unreadable:
        say(str(e))
    if not linked and not unreadable:
        say(f"no {environment} Item is linked; "
            f"{items.command('link --item NAME', environment)} links one")
    # Every Item is reported, including the ones after a failing one.
    healthy = all([link.report_item(client, i) for i in linked])
    # A sign-in left open is no fault of an Item, so it is listed and
    # leaves the exit status alone.
    left, damaged = items.open_sign_ins(args.secrets_dir, environment, names)
    for e in damaged:
        say(str(e))
    for pending in left:
        say(f"{pending.name}: a sign-in started {pending.created_at} is left "
            f"open; {items.command('login', environment)} settles it")
    return 0 if linked and healthy and not unreadable else 1


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    appkeys.source_env_file(args.env_file)
    environment = appkeys.environment(args)
    try:
        if args.check:
            return check(args, make_client(environment, args.client_id))
        return settle_all(args, environment)
    except items.ItemStoreError as e:
        say(str(e))
        return 1
    except KeyboardInterrupt:
        say()
        say("Stopped.")
        return 130


if __name__ == "__main__":
    sys.exit(main())
