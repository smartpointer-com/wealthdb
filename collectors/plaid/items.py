"""
The Items one deployment has linked, and the files that hold them.

An Item is one login at one institution. Each has a local name, chosen at
`link`. A name has these files in the secrets dir:

    plaid-token-<name>.json   the Item's access token, its ids and its
                              environment. Written once, when the Item is
                              made; update mode never changes the token.
    plaid-link-<name>.json    a Hosted Link session that has not been
                              settled yet. Written before its URL is
                              shown, removed once the sign-in is settled.
    plaid-link-<name>.lock    held by the one run that works on that name's
                              sign-in, so two runs never claim one Item.
                              Removed when that run ends.

One token file per Item, so linking one never rewrites the token of
another. A token cannot be fetched again: Plaid shows it once, at the
exchange, and a Trial plan does not return the slot of an Item whose token
is lost. So a file that exists and cannot be read is an error here, never
"no such Item". Read as absent, it would lead the caller to link a second
Item.
"""

from __future__ import annotations

import argparse
import fcntl
import json
import logging
import os
import re
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field, fields
from pathlib import Path

from collectorkit import session

import plaidapi
from appkeys import SECRET_ENV

log = logging.getLogger("plaid.items")

ENVIRONMENTS = tuple(plaidapi.HOSTS)

# A name is a directory, part of several file names, and a fair gold
# source id, so it stays inside what all three accept.
ITEM_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,39}$")

_TOKEN_PREFIX = "plaid-token-"
_LINK_PREFIX = "plaid-link-"


class ItemStoreError(Exception):
    """A credential file is present and cannot be used as it stands."""


def check_name(name: str) -> str:
    """`name` when it is a valid Item name, else ValueError."""
    if not ITEM_NAME_RE.match(name or ""):
        raise ValueError(
            f"{name!r} is not a valid Item name: use lower-case letters, "
            f"digits, '-' and '_', starting with a letter or a digit, at "
            f"most 40 characters")
    return name


def item_name(value: str) -> str:
    """argparse `type` for --item: the name, or argparse's error saying
    why it is not one."""
    try:
        return check_name(value)
    except ValueError as e:
        raise argparse.ArgumentTypeError(str(e)) from None


@dataclass
class Item:
    name: str
    environment: str
    # Kept out of repr(): an Item printed into a log must not carry it.
    access_token: str = field(repr=False)
    item_id: str
    institution_id: str | None = None
    institution_name: str | None = None
    linked_at: str | None = None


@dataclass
class PendingLink:
    name: str
    environment: str
    link_token: str = field(repr=False)
    hosted_link_url: str = field(repr=False)
    required: str
    created_at: str
    # When the page stops accepting a sign-in, in Unix seconds.
    expires_at: int


def token_path(secrets_dir: Path, name: str) -> Path:
    return Path(secrets_dir) / f"{_TOKEN_PREFIX}{check_name(name)}.json"


def pending_path(secrets_dir: Path, name: str) -> Path:
    return Path(secrets_dir) / f"{_LINK_PREFIX}{check_name(name)}.json"


def lock_path(secrets_dir: Path, name: str) -> Path:
    return Path(secrets_dir) / f"{_LINK_PREFIX}{check_name(name)}.lock"


def _read(path: Path, kind: type, prefix: str):
    """The record in `path` as a `kind`, or ItemStoreError. A field the
    file lacks reads as None. An error never quotes the file: it holds a
    credential."""
    try:
        if path.stat().st_mode & 0o077:
            session.secure_file(path)
        doc = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as e:
        raise ItemStoreError(
            f"{path} exists but cannot be read ({type(e).__name__}). "
            f"Nothing was changed. Restore the file from a backup, or "
            f"move it aside to start over.") from e
    names = [f.name for f in fields(kind)]
    if not isinstance(doc, dict) or any(
            not isinstance(doc.get(n), str) or not doc[n]
            for n in ("name", "environment")):
        raise ItemStoreError(f"{path} does not hold a {kind.__name__} record.")
    record = kind(**{n: doc.get(n) for n in names})
    expected = path.name[len(prefix):-len(".json")]
    if record.name != expected:
        raise ItemStoreError(
            f"{path} holds the record of {record.name!r}, not of "
            f"{expected!r}. A credential file is named after its Item.")
    if record.environment not in ENVIRONMENTS:
        raise ItemStoreError(
            f"{path} names an unknown environment: {record.environment!r}.")
    return record


def load_item(secrets_dir: Path, name: str) -> Item | None:
    """The stored Item called `name`, or None when no file exists. A link
    whose target is gone is a file that cannot be read."""
    path = token_path(secrets_dir, name)
    if not os.path.lexists(path):
        return None
    item = _read(path, Item, _TOKEN_PREFIX)
    if not all(isinstance(v, str) and v
               for v in (item.access_token, item.item_id)):
        raise ItemStoreError(f"{path} holds no access token or no item id.")
    # A Plaid token states its environment: access-<environment>-<uuid>.
    if not item.access_token.startswith(f"access-{item.environment}-"):
        raise ItemStoreError(
            f"{path} records the {item.environment} environment, and its "
            f"access token belongs to another one.")
    return item


def save_item(secrets_dir: Path, item: Item) -> None:
    session.save_state(token_path(secrets_dir, item.name), asdict(item))


def _stored_names(secrets_dir: Path, prefix: str = _TOKEN_PREFIX
                  ) -> list[str]:
    """The name of every record under `prefix`: by default the stored
    Items. A file under the prefix whose name is not an Item name is some
    other file and is passed over."""
    names = []
    for path in sorted(Path(secrets_dir).glob(f"{prefix}*.json")):
        name = path.name[len(prefix):-len(".json")]
        if not ITEM_NAME_RE.match(name):
            log.warning("ignoring %s: its name is not an Item name", path.name)
            continue
        names.append(name)
    return names


def list_items(secrets_dir: Path) -> list[Item]:
    """Every stored Item, by name. Raises ItemStoreError on the first file
    that cannot be read."""
    # A file removed since the listing is passed over.
    return [item for name in _stored_names(secrets_dir)
            if (item := load_item(secrets_dir, name)) is not None]


def of_environment(secrets_dir: Path, environment: str) -> list[Item]:
    """Every stored Item of one environment. Raises ItemStoreError on a
    file that cannot be read, so a caller about to store a new Item never
    misses one that exists."""
    return [i for i in list_items(secrets_dir) if i.environment == environment]


def command(text: str, environment: str) -> str:
    """A command to suggest, quoted, as it runs in `environment`: a
    Sandbox one carries --sandbox, so it never reaches Production."""
    return f"`{text}{' --sandbox' if environment == 'sandbox' else ''}`"


def remedy(item: Item, error_code: str | None,
           error_type: str | None) -> str:
    """What to do about an Item that Plaid refuses, or reports a problem
    with. Empty when Plaid's own message is all there is to go on."""
    if error_code == "ITEM_NOT_FOUND":
        return (f"Plaid no longer has this Item, and update mode cannot "
                f"renew it. The only way back is a new link under a new "
                f"name, {command('link --item NEW-NAME', item.environment)};"
                f" on a Trial plan it uses one more of the ten Items. Every "
                f"run reports this Item while "
                f"{_TOKEN_PREFIX}{item.name}.json is in the secrets dir.")
    if error_code == "INVALID_ACCESS_TOKEN":
        return (f"Plaid does not accept the stored token with these app "
                f"keys. PLAID_CLIENT_ID and {SECRET_ENV[item.environment]} "
                f"must be the keys of the Plaid team that linked the Item, "
                f"and {_TOKEN_PREFIX}{item.name}.json must match its "
                f"backup. A new link does not repair this.")
    if error_type == "ITEM_ERROR":
        return (f"{command(f'link --item {item.name}', item.environment)} "
                f"renews the sign-in.")
    return ""


def require_environment(name: str, environment: str, wanted: str) -> None:
    """Refuse a record of one environment on a run of the other: an access
    token only works with the secret of the environment that made it."""
    if environment != wanted:
        raise SystemExit(
            f"{name!r} belongs to the {environment} environment; "
            f"{'pass' if environment == 'sandbox' else 'drop'} --sandbox.")


def select(secrets_dir: Path, environment: str, names: list[str] | None
           ) -> tuple[list[Item], list[ItemStoreError]]:
    """The stored Items a run reads, and the token files it could not
    read.

    With `names`, those Items, in that order and each once; only their own
    files are read, and one that cannot be read raises. Without, every
    Item of the environment; a file that cannot be read is returned beside
    the others, so one damaged file does not stop every Item's run."""
    if names:
        chosen: list[Item] = []
        for name in dict.fromkeys(names):
            item = load_item(secrets_dir, name)
            if item is None:
                raise SystemExit(
                    f"no Item is named {name!r}; "
                    f"{command(f'link --item {name}', environment)} links "
                    f"one")
            require_environment(name, item.environment, environment)
            chosen.append(item)
        return chosen, []
    chosen, unreadable = [], []
    for name in _stored_names(secrets_dir):
        try:
            item = load_item(secrets_dir, name)
        except ItemStoreError as e:
            unreadable.append(e)
            continue
        if item is not None and item.environment == environment:
            chosen.append(item)
    return chosen, unreadable


def load_pending(secrets_dir: Path, name: str) -> PendingLink | None:
    path = pending_path(secrets_dir, name)
    if not os.path.lexists(path):
        return None
    pending = _read(path, PendingLink, _LINK_PREFIX)
    if not pending.link_token:
        raise ItemStoreError(f"{path} holds no link token.")
    return pending


def save_pending(secrets_dir: Path, pending: PendingLink) -> None:
    session.save_state(pending_path(secrets_dir, pending.name),
                       asdict(pending))


def clear_pending(secrets_dir: Path, name: str) -> None:
    pending_path(secrets_dir, name).unlink(missing_ok=True)


def open_sign_ins(secrets_dir: Path, environment: str,
                  names: list[str] | None = None
                  ) -> tuple[list[PendingLink], list[ItemStoreError]]:
    """The sign-ins of one environment that are not settled yet, and the
    records that could not be read. With `names`, only theirs."""
    found, unreadable = [], []
    for name in _stored_names(secrets_dir, _LINK_PREFIX):
        if names and name not in names:
            continue
        try:
            pending = load_pending(secrets_dir, name)
        except ItemStoreError as e:
            unreadable.append(e)
            continue
        if pending is not None and pending.environment == environment:
            found.append(pending)
    return found, unreadable


@contextmanager
def held(secrets_dir: Path, name: str) -> Iterator[bool]:
    """Hold the sign-in of `name` while this run works on it. Yields False,
    and holds nothing, when another run holds it already. The lock's file
    goes when the run ends. A run that crashes leaves the file, and the
    next run takes it over: the system releases the lock of a process
    that has ended."""
    path = lock_path(secrets_dir, name)
    while True:
        fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            yield False
            return
        # The run that held the lock removes its file as it ends. A lock
        # on that removed file guards nothing, so this run starts over.
        try:
            if os.path.samestat(os.fstat(fd), os.stat(path)):
                break
        except FileNotFoundError:
            pass
        os.close(fd)
    try:
        yield True
    finally:
        path.unlink(missing_ok=True)
        os.close(fd)
