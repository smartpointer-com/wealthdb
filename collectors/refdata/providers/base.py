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
(risk_metrics, money_market, characteristics, line_durations,
dividend_calendar; the wanted list, adapters/refdata.md §5): it names
them in `facts`, and is asked for a fund that wants one even where
another provider serves the fund's compositions. Its fetch and parse are
the same calls. The facts it can state for a past day, as a filing does,
it names in `dated_facts` too; the others it states for today only. A
fetch that succeeds without a fact it was asked for (a page that failed
beside a file that did not) names it in meta.json's `facts_missing`
(facts_missing()), so the fact stays due. The facts it states with every
composition it serves, as a holdings file states its lines' durations,
it names in `composition_facts` instead: a request for the fund's
composition answers them, and none is made for them alone.

The dividend calendar is such a fact of a fund, and a fact a security
held directly `wants` (wanted.Security.wants). A provider states every
distribution its source lists for the fund or security, the oldest too,
as a `dividend_events` row: event_row(), with an EVENT_KINDS kind and
the source's own id, else event_id() of its kind and days. The rows an
answer states under one id are one event (merge_events()). A source
that lists none (an accumulating share class) states no row, and the
fetch answers the calendar all the same: it is not asked again within
max_age_days. A document that lists every distribution of the ref from
its oldest one on, as a fund's history does, marks its Parsed
`events_complete`, so load drops what it no longer states; one that
lists only some (the latest, or those a day's list names) leaves it
unmarked, and load adds to what silver holds.

A security provider:

    claim_security(security, ctx) -> a claim or None
    fetch_security(claim, outdir, ctx)
    parse(indir)                -> Parsed rows (securities, facts)

A security provider names the wanted fields and families of figures it
can state, and the facts a security `wants` it can answer, in `answers`.
claim_security sees in the security's `wants` only the facts due from
it this run; a claim names in its `facts` those its fetch asks for. A
provider whose `answers` are such facts alone is asked only where one
is due.

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
                                   identifier such as a CIK), the
                                   families of figures it answers in its
                                   `families`, and the facts of the
                                   securities' `wants` it answers in its
                                   `facts`
    fetch_bulk(claim, outdir, ctx)  writes meta.json with the claim and
                                   `answered`: the keys the answer
                                   settles, those it states a figure or
                                   an event for, or every key asked
                                   where the source lists all it has
                                   for them (finra)
    parse(indir)                -> Parsed rows, for every security the
                                   answer states

Each has probe(ctx), the cheapest answer `login --check` asks for.

fetch writes a meta.json beside the raw files: what was asked, the claim
and anything parse needs that the files do not hold. parse reads it back.
A provider that reads what earlier runs fetched does so through
earlier_answers(), or earlier_docs() and earlier_metas() for the files
those answers kept.
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
    "dividend_events": ("ref", "provider", "event_id", "as_of", "kind", "ex_date", "record_date", "pay_date",
                        "declared_date", "amount", "currency", "status", "run_at", "payload"),
}

# A dividend event's kinds, and the statuses a source may give it.
EVENT_KINDS = frozenset({"dividend", "capital_gain", "return_of_capital", "special", "other"})
EVENT_STATUSES = frozenset({"final", "estimated"})

# The silver columns that hold a flag: 1, 0 or NULL.
FLAGS = frozenset({"ucits", "lending", "is_default", "in_arrears", "paid_in_kind"})

# A number whose commas separate groups of three digits: 1,234.5.
GROUPED = re.compile(r"^[-+]?\d{1,3}(?:,\d{3})+(?:\.\d*)?$")
# An amount as a source writes it, a currency or a mark around it:
# USD 1.6714, $0.25, $.25, 1’234.5.
AMOUNT = re.compile(r"[-+]?\.?\d[\d.,’']*")
CURRENCY_CODE = re.compile(r"\b[A-Z]{3}\b")


class ProviderError(Exception):
    """A provider could not serve a request. The run records it and goes
    on with the next one."""


class Unavailable(ProviderError):
    """The provider has nothing for this request (no file for that date,
    no filing in that period). Not a failure of the run."""


@dataclass
class Context:
    """What a provider gets from the run: the HTTP client
    (fetcher.Client: a request may carry the provider's own
    Authorization header, and received(host) counts the bytes a host's
    answers brought), the data dir (the inbox lives there), the run's
    refdata.cfg (config.Config) for a provider's own settings, the names
    of the providers the run asks, and a cache dict for lookups it
    repeats across funds (a ticker file). A secret the cache holds, as
    finra's token, is an object whose repr hides it."""
    client: object
    data_dir: Path | None = None
    config: object = None
    providers: frozenset = frozenset()
    cache: dict = field(default_factory=dict)

    def asks(self, name: str) -> bool:
        """Whether the run asks the provider `name`: one of `providers`,
        those refdata.cfg enables, narrowed by --provider where given
        (download.enabled_providers). A provider looks a fund up at
        another's source only then: enabling a provider accepts its
        source's terms, and a run narrowed to one provider asks no other
        source."""
        return name in self.providers

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
    silver stores. `events_complete` marks dividend events that are every
    one the source lists for each ref they name, from the oldest day it
    lists on: load then drops the provider's other rows of the ref from
    that day on (load.write)."""
    compositions: list = field(default_factory=list)
    lines: list = field(default_factory=list)
    attributes: list = field(default_factory=list)
    counterparties: list = field(default_factory=list)
    securities: list = field(default_factory=list)
    security_facts: list = field(default_factory=list)
    fund_risk: list = field(default_factory=list)
    characteristics: list = field(default_factory=list)
    period_facts: list = field(default_factory=list)
    dividend_events: list = field(default_factory=list)
    events_complete: bool = False


def row(table: str, **values) -> dict:
    """A row of a silver table: every column None, the payload {} and a
    characteristic's label "", then the values given. load stamps run_at.
    A name that is no column of the table is an error, and so is a
    dividend event's kind or status that EVENT_KINDS or EVENT_STATUSES
    does not hold."""
    out = dict.fromkeys(c for c in COLUMNS[table] if c != "run_at")
    out["payload"] = {}
    if table == "characteristics":
        out["label"] = ""
    stray = values.keys() - out.keys()
    if stray:
        raise KeyError(f"{table} has no column {', '.join(sorted(stray))}")
    out.update(values)
    if table == "dividend_events" and (out["kind"] not in EVENT_KINDS
                                       or out["status"] not in (None, *EVENT_STATUSES)):
        raise ValueError(f"no dividend event is of kind {out['kind']!r} and status {out['status']!r}")
    return out


def event_id(kind: str, ex=None, record=None, pay=None) -> str:
    """A dividend event's id where its source gives none: its kind and
    its ex, record and pay days (dates or YYYY-MM-DD), joined by `|`, a
    day the source does not state left blank."""
    return "|".join([kind, *(d.isoformat() if isinstance(d, date) else (d or "") for d in (ex, record, pay))])


def event_row(ref: str, provider: str, day: date, kind: str, days: dict, **cols) -> dict:
    """A dividend_events row of `ref` and `provider`, dated `day`: its
    days (ex_date, record_date, pay_date, declared_date, those stated)
    and its other columns `cols`. Its id is the one `cols` gives, else
    event_id() of its kind and days."""
    if "event_id" not in cols:
        cols["event_id"] = event_id(kind, days.get("ex_date"), days.get("record_date"), days.get("pay_date"))
    return row("dividend_events", ref=ref, provider=provider, as_of=day, kind=kind, **days, **cols)


def merge_events(rows) -> list:
    """dividend_events rows, one per ref, provider and event id, in the
    order each first comes. The rows an answer states under one id are
    the parts of one event, as a regular and a special distribution of
    the same days are; a row stated twice, the same in every column, is
    one part. The parts' amounts are summed where each states one and
    they state one currency, else the first part's stands. The event
    keeps the first part's other columns, and its payload lists every
    part, each with its amount and currency."""
    groups = {}
    for r in rows:
        parts = groups.setdefault((r["ref"], r["provider"], r["event_id"]), [])
        if r not in parts:
            parts.append(r)
    out = []
    for parts in groups.values():
        first = parts[0]
        if len(parts) == 1:
            out.append(first)
            continue
        amounts, codes = [p["amount"] for p in parts], {p["currency"] for p in parts} - {None}
        summed = None not in amounts and len(codes) <= 1
        out.append({**first, "amount": dec(sum(map(Decimal, amounts))) if summed else first["amount"],
                    "currency": next(iter(codes), None) if summed else first["currency"],
                    "payload": {"parts": [{**present({"amount": p["amount"], "currency": p["currency"]}),
                                           **p["payload"]} for p in parts]}})
    return out


def kinds_paid(parts: dict, kinds) -> dict:
    """kind -> the amount (a decimal string) a distribution pays of it:
    `parts` maps each part's name to its amount, None where it pays
    none, and `kinds` names each part's kind as (name, kind) pairs. The
    parts of one kind are summed, as a short- and a long-term gain are
    one capital gain. Empty where no part pays."""
    sums = {}
    for name, kind in kinds:
        if parts.get(name) is not None:
            sums[kind] = sums.get(kind, Decimal(0)) + Decimal(parts[name])
    return {kind: dec(total) for kind, total in sums.items()}


def facts_missing(facts) -> dict:
    """meta.json's `facts_missing` for the facts a fetch was asked for
    and did not get: none where it got every one."""
    facts = list(facts)
    return {"facts_missing": facts} if facts else {}


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
    # The wanted fields a security or bulk provider can state, and the
    # facts of a security's `wants` it answers (dividend_calendar). A
    # constituent (a security held through funds) is asked only of a
    # provider that answers one of its missing fields: a broad fund names
    # thousands.
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
    # The fewest days between two security requests to the provider for
    # one security, whatever max_age_days says, a request that failed
    # counted too: a source that allows one a month.
    min_age_days = 0

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

    def asked(self, ctx) -> dict:
        """security key -> the day of the provider's last request for it,
        as the provider keeps it for itself, a request that failed too;
        for one that waits min_age_days. So a run that dies after its
        requests still counts them. ProviderError where the record does
        not read."""
        return {}

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

    def note(self) -> str | None:
        """A line on what this run read through the provider, logged once
        at the run's end; None for nothing to say."""
        return None


def log_notes(providers, log) -> None:
    """Each provider's line on what the run read through it
    (Provider.note)."""
    for p in providers:
        if (note := p.note()) is not None:
            log.info("%s: %s", p.name, note)


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


def read_state(path: Path, kept: str, value) -> dict:
    """The object a provider keeps across runs in a file of the data dir,
    each value as `value` converts it; empty where there is no file yet.
    A file that does not read, or a value that `value` rejects (a
    ValueError or a TypeError), is an error that names the path and
    `kept`, what cannot be kept without it: such state is not guessed."""
    try:
        doc = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(doc, dict):
            raise ValueError("no object")
        return {str(k): value(v) for k, v in doc.items()}
    except FileNotFoundError:
        return {}
    except (OSError, ValueError, TypeError) as e:
        raise ProviderError(f"{path} does not read ({e}); {kept} cannot be kept without it") from e


def complete_runs(data_dir):
    """(run dir, manifest) of each complete run in the data dir, oldest
    first; none without a data dir. A run left incomplete counts for
    nothing, as load reads none of it."""
    for run in bronze.iter_run_dirs(data_dir) if data_dir else ():
        manifest = bronze.read_manifest(run / "run.json")
        if manifest is not None and manifest.get("status") == "complete":
            yield run, manifest


def earlier_answers(data_dir, provider: str, kind: str | None = None):
    """(run dir, manifest entry, folder) of each request `provider`
    served (status ok, a folder) in the complete runs of the data dir,
    oldest first; of `kind` (fund, security, bulk) alone where given."""
    for run, manifest in complete_runs(data_dir):
        for e in manifest.get("requests") or ():
            if (e.get("provider") == provider and e.get("status") == "ok" and e.get("dir")
                    and kind in (None, e.get("kind"))):
                yield run, e, run / e["dir"]


def earlier_docs(data_dir, provider: str, name: str = META, kind: str | None = None):
    """(folder, document) of each earlier answer of `provider`
    (earlier_answers) whose JSON file `name`, its meta.json by default,
    reads; oldest first. One that does not read is passed over."""
    for _, _, folder in earlier_answers(data_dir, provider, kind):
        try:
            doc = read_json(folder, name)
        except (ProviderError, OSError, ValueError):
            continue
        yield folder, doc


def earlier_metas(data_dir, provider: str, kind: str | None = None):
    """(folder, meta) of each earlier answer of `provider` whose
    meta.json reads as an object (earlier_docs)."""
    return ((folder, meta) for folder, meta in earlier_docs(data_dir, provider, kind=kind) if isinstance(meta, dict))


def through_day(meta: dict) -> date | None:
    """The last day an answer covered, as its meta.json's `through`
    states it; None where it states no such day."""
    try:
        return date.fromisoformat(meta["through"])
    except (KeyError, TypeError, ValueError):
        return None


def latest_through(data_dir, provider: str, kind: str | None = None) -> date | None:
    """The latest day an earlier answer of `provider` covered
    (through_day); None where none states one."""
    return max(filter(None, (through_day(meta) for _, meta in earlier_metas(data_dir, provider, kind))),
               default=None)


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


def direct_share(security) -> bool:
    """Whether a security is a share held directly: public equity, and
    no depositary receipt."""
    return security.scope == "direct" and security.asset_class == "public_equity" and not security.receipt


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


def amount(*values) -> tuple:
    """(amount, currency) of an amount a source writes in one value or
    more, as a cell's raw value and its text: the first number a value
    holds, as silver's decimal string (dec(): the value as it stands,
    .25 and 1.2E-3 too, else the first number its text writes), and the
    three-letter code a text names, else USD where it writes $. Both None
    where no value holds a number, or its number is zero."""
    number = None
    for value in values:
        if isinstance(value, bool) or value is None:
            continue
        number = dec(value)
        if number is None and not isinstance(value, int | float | Decimal) and (m := AMOUNT.search(str(value))):
            number = dec(m.group().rstrip(".,"))
        if number is not None:
            break
    if number is None or Decimal(number) == 0:
        return None, None
    words = " ".join(v for v in values if isinstance(v, str))
    code = CURRENCY_CODE.search(words)
    return number, code.group() if code else "USD" if "$" in words else None


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
