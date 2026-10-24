"""
The dividend calendar of a US company, from the inline XBRL of its 10-Q
and 10-K filings, for sec-edgar. This is no provider of its own:
sec-edgar claims a share's calendar, and fetches and parses it here.

- Due: a company is asked whenever the run asks for its calendar
  (max_age_days), as long as a filing of the last QUIET_DAYS that an
  earlier run read tags a dividend. A company whose filings tag none is
  asked again only QUIET_DAYS after the last run that read them all.
  About one payer in ten tags its dividends' days. Earlier runs'
  meta.json name the filings they read and whether each tags one.
- Fetch: each 10-Q and 10-K in inline XBRL that the company's
  submissions file lists among its recent filings, filed in the last
  FILINGS_MONTHS months, that no earlier run read: its XBRL archive
  (<accession>-xbrl.zip, its inline documents within), else its primary
  document (the inline document itself), compressed.
- Parse: of an archive, the primary document and each other member
  that declares inline XBRL's namespace at its start; an exhibit is
  plain HTML and is not parsed. Then the facts US GAAP tags a
  dividend's record, pay and declared days with and its amount per
  share (DAYS, AMOUNTS), a partnership's distribution to its limited
  partners too, one dividend per member of a dividend axis (else per
  context period). A day is read by its ixt date transform; a fact
  nested in a text block counts as any other. A dividend's amount may
  sit in a context of its own: the amount tagged without dimensions
  for the dividend's period joins it. Preferred stock's dividends are
  left out. US GAAP has no ex day: none is stated.

Terms: README.md, sec-edgar.
"""

from __future__ import annotations

import calendar
import html.entities
import io
import logging
import re
import urllib.parse
import xml.etree.ElementTree as ET
import zipfile
from collections import Counter
from dataclasses import dataclass, field
from datetime import date, timedelta
from decimal import Decimal, InvalidOperation
from pathlib import Path

import fetcher
from collectorkit import bronze, compress

from .base import ProviderError, dec, earlier_metas, event_id, present, read_bytes, row

CALENDAR = "dividend_calendar"
ARCHIVE = "https://www.sec.gov/Archives/edgar/data/{cik}/{folder}/{name}"
FORMS = frozenset({"10-K", "10-Q"})
ACCESSION = re.compile(r"^\d{10}-\d{2}-\d{6}$")
# The filings a company's first request reads: its last 10-K and the
# 10-Qs since, each of which states the dividends of two years.
FILINGS_MONTHS = 15
# A company none of whose filings of the last QUIET_DAYS tags a
# dividend's day is asked again QUIET_DAYS after it was last asked.
QUIET_DAYS = 365
# The largest document of an archive read: a filing's inline document is
# a few megabytes.
MAX_DOCUMENT = 256 << 20
# The bytes at a member's start that tell an inline document: it declares
# inline XBRL's namespace on its root element, in its first kilobyte or two.
SNIFF = 16 << 10

log = logging.getLogger("refdata.sec-edgar")

# ---- inline XBRL -------------------------------------------------------

IX = frozenset({"http://www.xbrl.org/2013/inlineXBRL", "http://www.xbrl.org/2008/inlineXBRL"})
XBRLI = "http://www.xbrl.org/2003/instance"
XBRLDI = "http://xbrl.org/2006/xbrldi"
ISO4217 = "http://www.xbrl.org/2003/iso4217"
# Every year's US GAAP taxonomy has a namespace of its own under this one.
GAAP = "http://fasb.org/us-gaap/"
NIL = "{http://www.w3.org/2001/XMLSchema-instance}nil"
FACT_TAGS = frozenset({"nonNumeric", "nonFraction"})
MEMBER_TAGS = frozenset({f"{{{XBRLDI}}}explicitMember", f"{{{XBRLDI}}}typedMember"})
# XHTML's named characters, which an XML parser knows only by number.
NAMED = re.compile(rb"&([A-Za-z][A-Za-z0-9]*);")
XML_NAMED = frozenset({"amp", "lt", "gt", "quot", "apos"})
MONTHS = {w: i for i, words in enumerate(
    (("january", "jan"), ("february", "feb"), ("march", "mar"), ("april", "apr"), ("may",), ("june", "jun"),
     ("july", "jul"), ("august", "aug"), ("september", "sep", "sept"), ("october", "oct"), ("november", "nov"),
     ("december", "dec")), start=1) for w in words}


@dataclass(frozen=True)
class XbrlContext:
    """An XBRL context: its period as (start, end), YYYY-MM-DD, an
    instant's start its end (None for forever), and its dimensions as
    sorted (axis, member) pairs, the axis by its local name."""
    period: tuple | None
    dims: tuple


@dataclass(frozen=True)
class Fact:
    concept: str     # a US GAAP concept's local name
    value: object    # a date, or a decimal string
    context: str
    unit: str | None


def _split(tag: str) -> tuple:
    """(namespace, local name) of an element's tag."""
    if tag[:1] == "{":
        uri, _, local = tag[1:].partition("}")
        return uri, local
    return "", tag


def _local(qname) -> str:
    return str(qname or "").rpartition(":")[2]


def _numbered(doc: bytes) -> bytes:
    """A document with each named character XML does not know written by
    its number."""
    def sub(m):
        name = m.group(1).decode()
        point = html.entities.name2codepoint.get(name)
        return m.group(0) if name in XML_NAMED or point is None else b"&#%d;" % point
    return NAMED.sub(sub, doc)


def _concept(el, ns: dict) -> str | None:
    """The US GAAP concept a fact's name is of, by its local name; None
    for a concept of another taxonomy."""
    prefix, _, local = (el.get("name") or "").rpartition(":")
    return local if ns.get(prefix, "").startswith(GAAP) else None


def read(doc: bytes, concepts) -> tuple:
    """An inline XBRL document's contexts (id -> XbrlContext), its units'
    currencies (id -> an ISO 4217 code, or None) and its facts of
    `concepts` (US GAAP local names). A fact nested in another, as in a
    text block, counts as any other. A nil fact, or one whose text its
    format does not read, is none. ET.ParseError where the document is
    no XML."""
    ns, contexts, units, facts, keep = {}, {}, {}, [], 0
    for event, el in ET.iterparse(io.BytesIO(_numbered(doc)), events=("start-ns", "start", "end")):
        if event == "start-ns":
            ns[el[0]] = el[1]
            continue
        uri, local = _split(el.tag)
        held = (uri == XBRLI and local in ("context", "unit")) or (
            uri in IX and local in FACT_TAGS and _concept(el, ns) in concepts)
        if event == "start":
            keep += held
            continue
        if held:
            keep -= 1
            if local == "context":
                contexts[el.get("id")] = _context(el)
            elif local == "unit":
                units[el.get("id")] = _currency(el, ns)
            elif el.get(NIL) != "true":
                text = _text(el)
                value = (_number(text, el.get("format"), el.get("scale"), el.get("sign")) if local == "nonFraction"
                         else _day(text, el.get("format")))
                if value is not None:
                    facts.append(Fact(_concept(el, ns), value, el.get("contextRef"), el.get("unitRef")))
        # Once read, an element's text is let go, unless it is part of a
        # context or a fact still open.
        if not keep:
            el.clear()
    return contexts, units, facts


def _text(el) -> str:
    """An element's text, without what an ix:exclude in it holds, its
    white space collapsed."""
    def parts(e):
        yield e.text or ""
        for c in e:
            uri, local = _split(c.tag)
            if not (uri in IX and local == "exclude"):
                yield from parts(c)
            yield c.tail or ""
    return " ".join("".join(parts(el)).split())


def _context(el) -> XbrlContext:
    """An xbrli:context element as an XbrlContext: its dimensions those
    of its explicit and typed members, wherever it states them."""
    def day(name):
        x = el.find(f".//{{{XBRLI}}}{name}")
        return None if x is None else (x.text or "").strip()[:10] or None
    instant, end = day("instant"), day("endDate")
    period = (instant, instant) if instant else (day("startDate") or end, end) if end else None
    dims = tuple(sorted((_local(m.get("dimension")), " ".join("".join(m.itertext()).split()))
                        for m in el.iter() if m.tag in MEMBER_TAGS))
    return XbrlContext(period, dims)


def _currency(el, ns: dict) -> str | None:
    """The currency of a unit: its first measure of ISO 4217's (USD of
    USD per share)."""
    for m in el.iter(f"{{{XBRLI}}}measure"):
        prefix, _, code = (m.text or "").strip().rpartition(":")
        if ns.get(prefix) == ISO4217:
            return code
    return None


def _order(fmt: str) -> str | None:
    """The order a date transform writes a day, a month and a year in:
    mdy, dmy or ymd. The registries name it by its parts
    (date-monthname-day-year-en, datemonthdayyearen), or as a US or a
    European form (dateslashus, datelongeu). None for a transform of no
    whole day (date-month-year, a quarter's end)."""
    name = fmt.rpartition(":")[2].lower().replace("-", "")
    if not name.startswith("date"):
        return None
    parts = sorted((name.find(w), w[0]) for w in ("day", "month", "year") if w in name)
    if parts:
        order = "".join(c for _, c in parts)
        return order if len(order) == 3 else None
    return "mdy" if name.endswith("us") else "dmy" if name.endswith(("eu", "uk")) else None


def _day(text: str, fmt: str | None) -> date | None:
    """The day a date fact states: its text read by its format, an ixt
    date transform (November 20, 2025; 20/11/2025), a month by its
    English name or number and a year of two digits in this century;
    without a format, as XML writes a date (2025-11-20). None where the
    text states no such day."""
    try:
        if not fmt:
            return date.fromisoformat(text.strip()[:10])
        order = _order(fmt)
        words = re.findall(r"[A-Za-z]+|\d+", text)
        months = [MONTHS[w.lower()] for w in words if w.lower() in MONTHS]
        numbers = [int(w) for w in words if w.isdigit()]
        if order is None or len(months) > 1:
            return None
        if months and len(numbers) == 2:
            parts = {**dict(zip(order.replace("m", ""), numbers, strict=True)), "m": months[0]}
        elif not months and len(numbers) == 3:
            parts = dict(zip(order, numbers, strict=True))
        else:
            return None
        return date(parts["y"] + 2000 if parts["y"] < 100 else parts["y"], parts["m"], parts["d"])
    except ValueError:
        return None


def _number(text: str, fmt: str | None, scale, sign) -> str | None:
    """The number a numeric fact states: its text read by its format, an
    ixt number transform (a dot or a comma its decimal mark, or a dash
    for zero), times ten to its scale, and signed."""
    name = (fmt or "").rpartition(":")[2].lower().replace("-", "")
    if name in ("fixedzero", "zerodash"):
        value = "0"
    else:
        digits = re.sub(r"[^\d,.]", "", text)
        comma = name.endswith("comma") or "commadecimal" in name
        value = dec(digits.replace(".", "").replace(",", ".") if comma else digits.replace(",", ""))
    if value is None:
        return None
    try:
        d = Decimal(value).scaleb(int(scale or 0))
    except (ValueError, InvalidOperation):
        return None
    return dec(-d if sign == "-" else d)


# ---- dividends ---------------------------------------------------------

# The concepts a filing tags a dividend's days with, each with the event
# column it fills: a corporation's dividend, then a partnership's
# distribution to its limited partners.
DAYS = {"DividendsPayableDateOfRecordDayMonthAndYear": "record_date",
        "DividendPayableDateToBePaidDayMonthAndYear": "pay_date",
        "DividendsPayableDateDeclaredDayMonthAndYear": "declared_date",
        "DistributionMadeToLimitedPartnerDateOfRecord": "record_date",
        "DistributionMadeToLimitedPartnerDistributionDate": "pay_date",
        "DistributionMadeToLimitedPartnerDeclarationDate": "declared_date"}
# The concepts of its amount per share or unit, the first a dividend
# states counting: the amount payable, then the amount declared.
AMOUNTS = ("DividendsPayableAmountPerShare", "CommonStockDividendsPerShareDeclared",
           "DistributionMadeToLimitedPartnerDistributionsDeclaredPerUnit")
CONCEPTS = frozenset(DAYS) | frozenset(AMOUNTS)
# The axes whose members tell one dividend from another.
AXES = frozenset({"DividendsAxis", "DistributionMadeToLimitedPartnerByDistributionAxis"})
# A member that names preferred stock: its dividends are not the share's.
PREFERRED = re.compile(r"Preferred|Preference")


@dataclass
class _Group:
    """The facts of one dividend: its axis member, or the period its
    facts share; the periods of their contexts; concept -> its values, a
    day or (amount, currency)."""
    member: str | None
    period: tuple | None
    periods: set = field(default_factory=set)
    values: dict = field(default_factory=dict)


def dividends(doc: bytes) -> list:
    """The dividends an inline XBRL document tags, each a dict of its
    record_date, pay_date and declared_date, its amount as (amount,
    currency, concept) or None, and its axis member or the period of
    its facts. Facts join into one dividend per member of a dividend
    axis (AXES), and the others per context period and dimensions. A
    dividend states a record or a pay day; facts that state two days of
    one kind are no dividend (two tagged in one context). One whose
    facts state no amount takes the amount tagged without dimensions for
    one of its periods, where no other dividend has that period: the
    amount of a quarter's declaration, which the table of days may tag
    apart. A fact of a member that names preferred stock is none."""
    contexts, units, facts = read(doc, CONCEPTS)
    groups, plain = {}, {}
    for f in facts:
        c = contexts.get(f.context)
        if c is None or c.period is None or any(PREFERRED.search(_local(m)) for _, m in c.dims):
            continue
        value = f.value if f.concept in DAYS else (f.value, units.get(f.unit))
        if f.concept in AMOUNTS and not c.dims:
            plain.setdefault(c.period, {}).setdefault(f.concept, set()).add(value)
        member = next((m for axis, m in c.dims if axis in AXES), None)
        g = groups.setdefault((member,) if member else (None, c.period, c.dims),
                              _Group(member, None if member else c.period))
        g.periods.add(c.period)
        g.values.setdefault(f.concept, set()).add(value)
    dated = []
    for g in groups.values():
        days = {}
        for concept, column in DAYS.items():
            days.setdefault(column, set()).update(g.values.get(concept, ()))
        if any(len(v) > 1 for v in days.values()):
            continue
        days = {column: next(iter(v), None) for column, v in days.items()}
        if days["record_date"] or days["pay_date"]:
            dated.append((g, days))
    claims = Counter(p for g, _ in dated for p in g.periods)
    out = []
    for g, days in dated:
        if any(c in g.values for c in AMOUNTS):
            amount = _amount(g.values)
        else:
            joined = [p for p in sorted(g.periods) if claims[p] == 1 and p in plain]
            amount = _amount(plain[joined[0]]) if len(joined) == 1 else None
        out.append({**days, "amount": amount, "member": g.member, "period": g.period})
    return out


def _amount(values: dict):
    """(amount, currency, concept) of the first of AMOUNTS that `values`
    (concept -> {(amount, currency)}) states; None where it states none,
    or two amounts for it."""
    for concept in AMOUNTS:
        if concept in values:
            got = values[concept]
            return (*next(iter(got)), concept) if len(got) == 1 else None
    return None


@dataclass
class Reads:
    """What a run read of the filings: the archives opened, the documents
    parsed and the HTML members of an archive left unparsed (its
    exhibits)."""
    archives: int = 0
    documents: int = 0
    skipped: int = 0

    def line(self) -> str | None:
        """The run's one line on it; None where it read no filing."""
        if not (self.archives or self.documents):
            return None
        return (f"calendar: archives read {self.archives}, documents parsed {self.documents}, "
                f"members skipped {self.skipped}")


READS = Reads()


def inline(head: bytes) -> bool:
    """Whether an archive member is an inline XBRL document, by its first
    SNIFF bytes: they declare inline XBRL's namespace."""
    return any(uri.encode() in head for uri in IX)


def documents(data: bytes, name: str, primary: str | None) -> list:
    """The inline XBRL documents of a filing's stored file: of its XBRL
    archive, the members that are inline documents (inline()) or the
    filing's primary document; else the primary document itself. Any
    other member, an exhibit, is not parsed. zipfile.BadZipFile where
    the archive is none."""
    if not name.endswith(".zip"):
        READS.documents += 1
        return [data]
    out, primary = [], (primary or "").rpartition("/")[2] or None
    with zipfile.ZipFile(io.BytesIO(data)) as z:
        READS.archives += 1
        for i in z.infolist():
            if not i.filename.lower().endswith((".htm", ".html", ".xhtml")) or i.file_size > MAX_DOCUMENT:
                continue
            with z.open(i) as member:
                head = member.read(SNIFF)
                if i.filename.rpartition("/")[2] == primary or inline(head):
                    out.append(head + member.read())
                else:
                    READS.skipped += 1
    READS.documents += len(out)
    return out


def filing_dividends(data: bytes, name: str, primary: str | None) -> list:
    """The dividends of a filing's stored file, of all its inline
    documents (documents(); `primary` the primary document's name, or None). A
    file or a document that does not read states none, and a warning
    names it."""
    try:
        docs = documents(data, name, primary)
    except zipfile.BadZipFile as e:
        log.warning("%s: %s", name, e)
        return []
    out = []
    for doc in docs:
        try:
            out += dividends(doc)
        except ET.ParseError as e:
            log.warning("%s: not XML: %s", name, e)
    return out


# ---- which filings, and when -------------------------------------------


def months_back(day: date, n: int) -> date:
    """The day n months before `day`, its month's last where that month
    is shorter."""
    year, month = divmod(day.year * 12 + day.month - 1 - n, 12)
    return date(year, month + 1, min(day.day, calendar.monthrange(year, month + 1)[1]))


def filings(sub: dict, since: date, read) -> list:
    """The filings a request reads: each 10-Q and 10-K in inline XBRL
    among a submissions file's recent filings (a year of them at least),
    filed on `since` or later, whose accession is not in `read`; oldest
    first."""
    cols = (sub.get("filings") or {}).get("recent") or {}

    def col(name, i):
        values = cols.get(name) or []
        return values[i] if i < len(values) else None

    out = []
    for i, form in enumerate(cols.get("form") or []):
        acc, filed = col("accessionNumber", i), col("filingDate", i)
        if (form in FORMS and ACCESSION.match(acc or "") and filed and filed >= since.isoformat()
                and acc not in read and col("isInlineXBRL", i) != 0):
            out.append({"accession": acc, "form": form, "filed": filed, "report": col("reportDate", i) or None,
                        "document": col("primaryDocument", i) or None})
    return sorted(out, key=lambda f: (f["filed"], f["accession"]))


@dataclass
class Seen:
    """What earlier runs read of one company's filings: accession ->
    (the day it was filed, whether it tags a dividend), and the last day
    a run read every filing it asked for (None: none did)."""
    read: dict = field(default_factory=dict)
    checked: date | None = None


def seen(ctx, provider: str) -> dict:
    """CIK -> Seen, from the meta.json of each request of `provider` that
    read a company's filings in an earlier complete run (ctx.data_dir):
    worked out once a run. A run left incomplete counts for nothing, as
    load reads none of it."""
    def load():
        out = {}
        for _, meta in earlier_metas(ctx.data_dir, provider, "security"):
            cal = meta.get("calendar")
            if not isinstance(cal, dict):
                continue
            try:
                read = {f["accession"]: (date.fromisoformat(f["filed"]), bool(f.get("tagged")))
                        for f in cal.get("filings") or []}
                whole = CALENDAR not in (meta.get("facts_missing") or ())
                cik, day = int(meta["cik"]), date.fromisoformat(meta["as_of"])
            except (KeyError, TypeError, ValueError):
                continue
            s = out.setdefault(cik, Seen())
            s.read.update(read)
            if whole:
                s.checked = max(s.checked or day, day)
        return out
    return ctx.once(("sec_calendar_seen", provider), load)


def due(s: Seen | None, today: date) -> bool:
    """Whether a company's calendar is asked for: no earlier run read all
    the filings it asked for, a filing of the last QUIET_DAYS tags a
    dividend, or the last run that read them all is QUIET_DAYS old."""
    if s is None or s.checked is None:
        return True
    quiet = today - timedelta(days=QUIET_DAYS)
    return s.checked <= quiet or any(tagged and filed > quiet for filed, tagged in s.read.values())


# ---- fetch and parse ---------------------------------------------------


def fetch(ctx, cik: int, outdir: Path, sub: dict, today: date, read) -> tuple:
    """The filings of a company's calendar into outdir (filings()): each
    its XBRL archive, else, where there is none, its primary document,
    compressed. Returns meta.json's `calendar`, the filings read with
    the file each is stored in and whether it tags a dividend, and
    whether a filing could not be had: it is asked for again."""
    got, missing = [], False
    for f in filings(sub, months_back(today, FILINGS_MONTHS), read):
        try:
            name = _fetch_filing(ctx, cik, f, outdir)
        except (fetcher.FetchError, ProviderError) as e:
            log.warning("CIK %s: filing %s: %s", cik, f["accession"], e)
            missing = True
            continue
        tagged = bool(filing_dividends(read_bytes(outdir, name), name, f["document"]))
        got.append({**f, "file": name, "tagged": tagged})
    return {"filings": got}, missing


def _fetch_filing(ctx, cik: int, f: dict, outdir: Path) -> str:
    """One filing into outdir; the name of the file it is stored in."""
    folder, archive = f["accession"].replace("-", ""), f"{f['accession']}-xbrl.zip"
    try:
        body = ctx.client.get(ARCHIVE.format(cik=cik, folder=folder, name=archive)).body
        if zipfile.is_zipfile(io.BytesIO(body)):
            bronze.atomic_write_bytes(outdir / archive, body)
            return archive
    except fetcher.FetchError as e:
        if e.status != 404:
            raise
    if not f["document"]:
        raise ProviderError("no XBRL archive and no primary document")
    body = ctx.client.get(ARCHIVE.format(cik=cik, folder=folder, name=urllib.parse.quote(f["document"]))).body
    name = f"{f['accession']}.htm"
    bronze.atomic_write_bytes(outdir / name, body)
    compress.compress_best_effort(outdir / name, log)
    return name


def parse(indir: Path, meta: dict, *, provider: str) -> list:
    """The dividend_events rows of a request's filings, for the security
    its meta.json names: one per dividend, its event id its kind and its
    record and pay days (no member name lasts from one filing to the
    next). A dividend that several filings state takes each column from
    the last filed that states it, and is dated by the day that one was
    filed; its member or period is the last one's. Every dividend is
    final and has no ex day."""
    merged = {}
    for f in sorted((meta.get("calendar") or {}).get("filings") or [], key=lambda f: (f["filed"], f["accession"])):
        for d in filing_dividends(read_bytes(indir, f["file"]), f["file"], f.get("document")):
            e = merged.setdefault(event_id("dividend", None, d["record_date"], d["pay_date"]), {})
            e.update({k: v for k, v in d.items() if v is not None and k not in ("member", "period")})
            e.update(member=d["member"], period=d["period"], filing=f)
    out = []
    for eid, e in sorted(merged.items()):
        f = e["filing"]
        amount, currency, concept = e.get("amount") or (None, None, None)
        out.append(row("dividend_events", ref=meta["key"], provider=provider, event_id=eid,
                       as_of=date.fromisoformat(f["filed"]), kind="dividend", record_date=e.get("record_date"),
                       pay_date=e.get("pay_date"), declared_date=e.get("declared_date"), amount=amount,
                       currency=currency, status="final",
                       payload=present({"accn": f["accession"], "form": f["form"], "member": e["member"],
                                        "period": "/".join(e["period"]) if e["period"] else None,
                                        "amount_concept": f"us-gaap:{concept}" if concept else None})))
    return out
