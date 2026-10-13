"""
The provider contract. A provider is one source module: it claims what it
can serve, says which dates it has, fetches one date into a directory,
and parses that directory into silver rows. download.py and load.py are
generic; adding a provider is one module and one registry line.

A fund provider:

    claim(fund, ctx)            -> a claim (the provider's own handle on
                                   the fund) or None
    dates(claim, since, until, ctx)
                                -> the dates it can serve in [since,
                                   until], or None when it serves the
                                   current composition only
    latest(claim, ctx)          -> the date of the newest composition it
                                   serves, or None when only a fetch
                                   tells
    fetch(claim, as_of, outdir, ctx)
                                -> writes the raw files into outdir;
                                   as_of None is the current composition
    parse(indir)                -> Parsed rows; reads only what fetch wrote,
                                   so a parser fix re-runs on kept bronze
                                   with `load --force`

A security provider:

    claim_security(security, ctx) -> a claim or None
    fetch_security(claim, outdir, ctx)
    parse(indir)                -> Parsed rows (securities, facts)

Both have probe(ctx), one cheap request that `login --check` makes.

fetch writes a meta.json beside the raw files: what was asked, the claim
and anything parse needs that the files do not hold. parse reads it back.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import date, datetime, timezone
from pathlib import Path

META = "meta.json"


class ProviderError(Exception):
    """A provider could not serve a request. The run records it and goes
    on with the next one."""


class Unavailable(ProviderError):
    """The provider has nothing for this request (no file for that date,
    no filing in that period). Not a failure of the run."""


@dataclass
class Context:
    """What a provider gets from the run: the HTTP client, the contact for
    the User-Agent, the data dir (the inbox lives there) and a cache dict
    for lookups it repeats across funds (a ticker file)."""
    client: object
    contact: str = ""
    data_dir: Path | None = None
    cache: dict = field(default_factory=dict)

    def once(self, key, load):
        """The result of `load()` for `key`, computed once per run. A load
        that failed fails again without a second request: a file every
        fund needs is asked for once, not once per fund."""
        if key not in self.cache:
            try:
                self.cache[key] = (load(), None)
            except Exception as e:  # noqa: BLE001 - re-raised below, and on every later call
                self.cache[key] = (None, e)
        value, error = self.cache[key]
        if error is not None:
            raise error
        return value


@dataclass
class Parsed:
    """Silver rows, keyed by the silver table's column names
    (migrations/0001_initial.sql). Dates are datetime.date; load turns
    them into the epoch seconds silver stores."""
    funds: list = field(default_factory=list)
    compositions: list = field(default_factory=list)
    lines: list = field(default_factory=list)
    attributes: list = field(default_factory=list)
    counterparties: list = field(default_factory=list)
    securities: list = field(default_factory=list)
    security_facts: list = field(default_factory=list)


class Provider:
    """A provider serves funds, securities, or both. Defaults decline."""
    name = ""
    serves_funds = False
    serves_securities = False
    # The SEC asks every client to name a contact; such a provider does not
    # run without one.
    needs_contact = False
    # An additive provider serves a fund beside the provider that claims
    # it, never instead of it: the inbox adds what was placed by hand.
    additive = False

    def claim(self, fund, ctx):
        return None

    def dates(self, claim, since, until, ctx):
        return None

    def latest(self, claim, ctx):
        return None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        raise NotImplementedError

    def claim_security(self, security, ctx):
        return None

    # The wanted fields a security provider can state. A constituent (a
    # security held through funds) is asked only of a provider that
    # answers one of its missing fields: a broad fund names thousands.
    answers = frozenset()

    def wants_constituent(self, security) -> bool:
        return bool(self.answers & set(security.missing))

    def fetch_security(self, claim, outdir: Path, ctx):
        raise NotImplementedError

    def parse(self, indir: Path) -> Parsed:
        raise NotImplementedError

    def probe(self, ctx) -> str:
        """One cheap request; returns a short description of what answered."""
        return "nothing to probe"


def write_meta(outdir: Path, **meta) -> None:
    from collectorkit import bronze
    outdir.mkdir(parents=True, exist_ok=True)
    bronze.atomic_write_json(outdir / META, meta)


def read_meta(indir: Path) -> dict:
    return json.loads((indir / META).read_text(encoding="utf-8"))


def today_utc() -> date:
    return datetime.now(timezone.utc).date()


def dec(value) -> str | None:
    """A number as silver's decimal string: None for blank or unparseable,
    no exponent, no trailing zeros."""
    from decimal import Decimal, InvalidOperation
    if value is None:
        return None
    s = str(value).strip().replace(",", "").replace("’", "").replace("'", "")
    if s in ("", "-", "N/A", "n/a", "NA", "--"):
        return None
    if s.endswith("%"):
        s = s[:-1]
    try:
        d = Decimal(s)
    except InvalidOperation:
        return None
    if not d.is_finite():
        return None
    out = format(d.normalize(), "f")
    return "0" if out in ("-0", "") else out


def blank_to_none(value):
    if value is None:
        return None
    s = str(value).strip()
    return s or None
