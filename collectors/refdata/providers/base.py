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

A fund provider may also answer some of the facts a fund `wants`
(risk_metrics, money_market, characteristics, line_durations; the
wanted list, adapters/refdata.md §5): it names them in `facts`, and is
asked for a fund that wants one even where another provider serves the
fund's compositions. Its fetch and parse are the same calls. The facts
it can state for a past day, as a filing does, it names in
`dated_facts` too; the others it states for today only. A fetch that
succeeds without a fact it was asked for (a page that failed beside a
file that did not) names it in meta.json's `facts_missing`, so the fact
stays due. The facts it states with every composition it serves, as a
holdings file states its lines' durations, it names in
`composition_facts` instead: a request for the fund's composition
answers them, and none is made for them alone.

A security provider:

    claim_security(security, ctx) -> a claim or None
    fetch_security(claim, outdir, ctx)
    parse(indir)                -> Parsed rows (securities, facts)

A bulk provider serves many securities from one request, such as a
table of every company's figure for one period:

    plan_bulk(securities, ctx, today, fetched, max_age)
                                -> [(ref, claim)]: the requests a run
                                   makes for these securities (the
                                   wanted list's, in security_order).
                                   ref names the request and its folder;
                                   `fetched` maps the ref of each bulk
                                   request an earlier run fetched to the
                                   day of that run. A claim names the
                                   securities it asks about in its
                                   `keys` (a list, or lists by an
                                   identifier such as a CIK), and the
                                   wanted families it answers in its
                                   `families`
    fetch_bulk(claim, outdir, ctx)  writes meta.json with the claim and
                                   `answered`, the keys the answer
                                   states a figure for
    parse(indir)                -> Parsed rows, for every security the
                                   answer states

Each has probe(ctx), the cheapest answer `login --check` asks for.

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

from collectorkit import bronze, compress

META = "meta.json"

# Every silver table's columns (migrations/), in order.
COLUMNS = {
    "compositions": ("fund_ref", "as_of", "basis", "provider", "source_kind", "fund_name", "currency",
                     "net_assets", "line_count", "weight_sum", "units_outstanding", "run_at", "payload"),
    "lines": ("fund_ref", "as_of", "basis", "line_no", "isin", "cusip", "figi", "ticker", "exchange", "name",
              "asset_type", "weight", "quantity", "market_value", "currency", "country", "risk_country",
              "sector", "industry", "maturity", "coupon", "long_currency", "long_amount", "short_currency",
              "short_amount", "payload", "coupon_kind", "is_default", "in_arrears", "paid_in_kind", "exchange_rate",
              "duration", "modified_duration", "yield_to_maturity", "yield_to_worst", "yield_to_call",
              "real_duration", "real_yield", "accrual_date"),
    "attributes": ("fund_ref", "as_of", "domicile", "legal_form", "ucits", "replication", "distribution",
                   "ter", "hedged_currency", "lending", "index_name", "depositary", "run_at", "payload"),
    "counterparties": ("fund_ref", "as_of", "role", "name", "lei", "share", "run_at", "payload"),
    "securities": ("security_key", "as_of", "provider", "isin", "cusip", "figi", "lei", "ticker", "exchange",
                   "name", "security_type", "listing_currency", "run_at", "payload"),
    "security_facts": ("security_key", "field", "as_of", "provider", "value", "scheme", "run_at", "payload"),
    "fund_risk": ("fund_ref", "as_of", "currency", "tenor", "dv01", "dv100", "credit_ig_dv01", "credit_hy_dv01",
                  "run_at", "payload"),
    "characteristics": ("fund_ref", "as_of", "provider", "field", "label", "value", "run_at", "payload"),
    "period_facts": ("security_key", "field", "as_of", "period_start", "provider", "value", "scheme", "unit",
                     "fiscal_year", "fiscal_period", "form", "filed", "run_at", "payload"),
}

# The silver columns that hold a flag: 1, 0 or NULL.
FLAGS = frozenset({"ucits", "lending", "is_default", "in_arrears", "paid_in_kind"})

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
    fund_risk: list = field(default_factory=list)
    characteristics: list = field(default_factory=list)
    period_facts: list = field(default_factory=list)


def row(table: str, **values) -> dict:
    """A row of a silver table: every column None, the payload {} and a
    characteristic's label "", then the values given. load stamps run_at.
    A name that is no column of the table is an error."""
    out = dict.fromkeys(c for c in COLUMNS[table] if c != "run_at")
    out["payload"] = {}
    if table == "characteristics":
        out["label"] = ""
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
    serves_bulk = False
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
    # The facts of a fund's `wants` a fund provider answers beside
    # compositions (`facts`), those it can state for a past day
    # (`dated_facts`), and those its compositions state
    # (`composition_facts`).
    facts = frozenset()
    dated_facts = frozenset()
    composition_facts = frozenset()
    # A fund provider whose documents state a fund's facts and no holdings
    # (a money-market filing) is never offered a fund's compositions.
    serves_compositions = True

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

    def plan_bulk(self, securities, ctx, today, fetched, max_age) -> list:
        return []

    def fetch_bulk(self, claim, outdir: Path, ctx):
        raise NotImplementedError

    def parse(self, indir: Path) -> Parsed:
        raise NotImplementedError

    def probe(self, ctx) -> str:
        """The cheapest answer the provider can ask for; returns a short
        description of what answered."""
        return "nothing to probe"


def write_meta(outdir: Path, **meta) -> None:
    bronze.atomic_write_json(outdir / META, meta)


def read_bytes(indir: Path, name: str) -> bytes:
    """A file of a request's folder, as stored or compressed (collectorkit
    compress)."""
    path = compress.resolve_variant(indir / name)
    if path is None:
        raise ProviderError(f"{indir}: no {name}")
    with compress.open_bytes(path) as fh:
        return fh.read()


def read_json(indir: Path, name: str):
    """A JSON file of a request's folder, a byte order mark allowed: a
    source's answer as stored or compressed, or a file the provider
    wrote."""
    return json.loads(read_bytes(indir, name).decode("utf-8-sig"))


def read_meta(indir: Path) -> dict:
    return read_json(indir, META)


def folder_name(ref: str) -> str:
    """A fund reference or security key as a folder name: `isin_XX…`, as
    the inbox writes it. A reference with other characters a folder name
    cannot hold gets a short hash of itself, so XYZ-B and XYZ/B never
    share a folder."""
    name = ref.replace(":", "_", 1)
    stem = bronze.safe_stem(name)
    return stem if stem == name else f"{stem}-{bronze.short_token(ref, 8)}"


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


def listed(index: dict, fund, us_site: str):
    """An issuer's product list's entries for a fund, as (site, entry):
    by its ISIN, then its CUSIP, then its ticker, which matches a fund of
    the US site only (us_symbol)."""
    for key, us_only in ((ident(fund, "isin"), False), (ident(fund, "cusip"), False), (us_symbol(fund), True)):
        hit = index.get(key) if key else None
        if hit is not None and not (us_only and hit[0] != us_site):
            yield hit


def dec(value) -> str | None:
    """A number as silver's decimal string: no exponent, no trailing
    zeros. None for a blank, an unparseable value, or a comma that is no
    thousands mark (0,25 could be a quarter or 25)."""
    if value is None:
        return None
    s = str(value).strip().replace("’", "").replace("'", "")
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
    return "0" if out == "-0" else out


def percent(fraction) -> str | None:
    """A fraction as silver's decimal string in percent: 0.25 is 25."""
    d = dec(fraction)
    return dec(Decimal(d) * 100) if d is not None else None


def present(values: dict, *, keep_falsy: bool = False) -> dict:
    """A payload's entries that hold a value: none that is None or "",
    and, unless keep_falsy, no other falsy one (False, 0, an empty
    list)."""
    if keep_falsy:
        return {k: v for k, v in values.items() if v not in (None, "")}
    return {k: v for k, v in values.items() if v}


def blank_to_none(value):
    if value is None:
        return None
    s = str(value).strip()
    return s or None
