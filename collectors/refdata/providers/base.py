"""
The provider contract. A provider is one source module: it claims what it
can serve, says which dates it has, fetches one date into a directory,
and parses that directory into silver rows. download.py and load.py are
generic; adding a provider is one module and one registry line.

A fund provider:

    claim(fund, ctx)            -> a claim (the provider's own handle on
                                   the fund) or None. Its optional `ids`
                                   ({isin, cusip, symbol}, the symbol a
                                   US ticker only) are what the source
                                   states for the fund; they fill the
                                   fund's gaps before a provider
                                   standing by claims it
    dates(claim, since, until, ctx)
                                -> the dates it can serve in [since,
                                   until], or None when it serves the
                                   current composition only
    latest(claim, ctx)          -> the date of the newest composition it
                                   serves, or None when only a fetch
                                   tells
    fetch(claim, as_of, outdir, ctx)
                                -> writes the raw files into outdir and
                                   returns the day they describe; as_of
                                   None is the current composition
    parse(indir)                -> Parsed rows; reads only what fetch wrote,
                                   so a parser fix re-runs on kept bronze
                                   with `load --force`

A security provider:

    claim_security(security, ctx) -> a claim or None
    fetch_security(claim, outdir, ctx)
    parse(indir)                -> Parsed rows (securities, facts)

Both have probe(ctx), the cheapest answer `login --check` asks for.

fetch writes a meta.json beside the raw files: what was asked, the claim
and anything parse needs that the files do not hold. parse reads it back.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from datetime import date, datetime, timezone
from decimal import Decimal, InvalidOperation
from pathlib import Path

from collectorkit import bronze

META = "meta.json"

# Every silver table's columns (migrations/0001_initial.sql), in order.
COLUMNS = {
    "compositions": ("fund_ref", "as_of", "basis", "provider", "source_kind", "fund_name", "currency",
                     "net_assets", "line_count", "weight_sum", "units_outstanding", "run_at", "payload"),
    "lines": ("fund_ref", "as_of", "basis", "line_no", "isin", "cusip", "figi", "ticker", "exchange", "name",
              "asset_type", "weight", "quantity", "market_value", "currency", "country", "risk_country",
              "sector", "industry", "maturity", "coupon", "long_currency", "long_amount", "short_currency",
              "short_amount", "payload"),
    "attributes": ("fund_ref", "as_of", "domicile", "legal_form", "ucits", "replication", "distribution",
                   "ter", "hedged_currency", "lending", "index_name", "depositary", "run_at", "payload"),
    "counterparties": ("fund_ref", "as_of", "role", "name", "lei", "share", "run_at", "payload"),
    "securities": ("security_key", "as_of", "provider", "isin", "cusip", "figi", "lei", "ticker", "exchange",
                   "name", "security_type", "listing_currency", "run_at", "payload"),
    "security_facts": ("security_key", "field", "as_of", "provider", "value", "scheme", "run_at", "payload"),
}

# A number whose commas separate groups of three digits: 1,234.5.
GROUPED = re.compile(r"^[-+]?\d{1,3}(?:,\d{3})+(?:\.\d*)?$")


class ProviderError(Exception):
    """A provider could not serve a request. The run records it and goes
    on with the next one."""


class Unavailable(ProviderError):
    """The provider has nothing for this request (no file for that date,
    no filing in that period). Not a failure of the run."""


@dataclass
class Context:
    """What a provider gets from the run: the HTTP client, the data dir
    (the inbox lives there), the run's refdata.cfg (config.Config) for a
    provider's own settings, and a cache dict for lookups it repeats
    across funds (a ticker file)."""
    client: object
    data_dir: Path | None = None
    config: object = None
    cache: dict = field(default_factory=dict)

    def once(self, key, load):
        """The result of `load()` for `key`, computed once per run. A load
        that failed fails again without a second request, with its first
        traceback: a file every fund needs is asked for once, not once
        per fund."""
        if key not in self.cache:
            try:
                self.cache[key] = (load(), None, None)
            except Exception as e:  # noqa: BLE001 - re-raised below, and on every later call
                self.cache[key] = (None, e, e.__traceback__)
        value, error, tb = self.cache[key]
        if error is not None:
            raise error.with_traceback(tb)
        return value


@dataclass
class Parsed:
    """Silver rows, keyed by the silver table's column names (COLUMNS).
    Dates are datetime.date; load turns them into the epoch seconds
    silver stores."""
    compositions: list = field(default_factory=list)
    lines: list = field(default_factory=list)
    attributes: list = field(default_factory=list)
    counterparties: list = field(default_factory=list)
    securities: list = field(default_factory=list)
    security_facts: list = field(default_factory=list)


def row(table: str, **values) -> dict:
    """A row of a silver table: every column None and the payload {},
    then the values given. load stamps run_at. A name that is no column
    of the table is an error."""
    out = dict.fromkeys(c for c in COLUMNS[table] if c != "run_at")
    out["payload"] = {}
    stray = values.keys() - out.keys()
    if stray:
        raise KeyError(f"{table} has no column {', '.join(sorted(stray))}")
    out.update(values)
    return out


class Provider:
    """A provider serves funds, securities, or both. Defaults decline."""
    name = ""
    serves_funds = False
    serves_securities = False
    # The SEC asks every client to name a contact; such a provider does not
    # run without one.
    needs_contact = False
    # The refdata.cfg setting a provider reads what to fetch from; it does
    # not run while the setting is empty.
    needs_setting = ""
    # An additive provider serves a fund beside the provider that claims
    # it, never instead of it: the inbox adds what was placed by hand.
    additive = False
    # The wanted fields a security provider can state. A constituent (a
    # security held through funds) is asked only of a provider that
    # answers one of its missing fields: a broad fund names thousands.
    answers = frozenset()

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

    def wants_constituent(self, security) -> bool:
        return bool(self.answers & set(security.missing))

    def fetch_security(self, claim, outdir: Path, ctx):
        raise NotImplementedError

    def parse(self, indir: Path) -> Parsed:
        raise NotImplementedError

    def probe(self, ctx) -> str:
        """The cheapest answer the provider can ask for; returns a short
        description of what answered."""
        return "nothing to probe"


def write_meta(outdir: Path, **meta) -> None:
    bronze.atomic_write_json(outdir / META, meta)


def read_meta(indir: Path) -> dict:
    return json.loads((indir / META).read_text(encoding="utf-8"))


def today_utc() -> date:
    return datetime.now(timezone.utc).date()


def ident(item, scheme: str) -> str:
    """A wanted fund's or security's identifier of one scheme (isin,
    cusip or symbol): its own field, else the reference it is named by
    where that is of the scheme. Upper case; "" where it has none."""
    value = getattr(item, scheme, None)
    if not value:
        name = getattr(item, "ref", None) or getattr(item, "key", "")
        value = name[len(scheme) + 1:] if name.startswith(scheme + ":") else ""
    return value.strip().upper()


def us_symbol(fund) -> str:
    """A fund's ticker where it can name a US fund: a bare ticker names a
    US listing, so a fund with an ISIN of another country has none here
    (a UCITS fund's local ticker can be a US fund's)."""
    isin = ident(fund, "isin")
    return ident(fund, "symbol") if not isin or isin.startswith("US") else ""


def dec(value) -> str | None:
    """A number as silver's decimal string: no exponent, no trailing
    zeros. None for a blank, an unparseable value, or a comma that is no
    thousands mark (0,25 could be a quarter or 25)."""
    if value is None:
        return None
    s = str(value).strip().replace("’", "").replace("'", "")
    if s in ("", "-", "N/A", "n/a", "NA", "--"):
        return None
    if s.endswith("%"):
        s = s[:-1].strip()
    if "," in s:
        if not GROUPED.match(s):
            return None
        s = s.replace(",", "")
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
