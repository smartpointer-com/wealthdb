"""
A fund's holdings as its semi-annual or annual report lists them. The
`swisscanto` and `swissfunddata` providers find the reports; this
module fetches and reads them.

A report is a PDF with an inventory per sub-fund. The held fund's is
the one whose title's words the fund's name holds. Three layouts are
read:

- a schedule of investments (UBS's Irish and Luxembourg funds, in
  English or German): per holding its name, quantity, market value and
  percent of net assets, under industry headings;
- an inventory of fund assets (UBS's Swiss funds): per holding its
  name, currency, quantities and percent of fund assets, in country
  blocks;
- an inventory with an ISIN per line (Swisscanto's funds, in German).

A line's weight is its market value as a share of the lines' total,
scaled to the percent the report prints for them, so a holding the
report rounds to 0.00% keeps its weight. Where a layout's values do not
split into columns, the printed percent stands. Derivatives, cash and
other assets are left out: the composition's remainder holds them.

An industry heading names the issuer's industry, not its sector, and
goes to the line's industry. A country block's TOTAL line names the
country its holdings take.
"""

from __future__ import annotations

import os
import re
import unicodedata
from collections import OrderedDict
from datetime import date
from decimal import Decimal
from pathlib import Path

from collectorkit import bronze

from . import table
from .base import Parsed, Provider, ProviderError, read_meta, row, write_meta

MONTHS = {m: i + 1 for i, m in enumerate(
    ("january", "february", "march", "april", "may", "june", "july", "august", "september", "october",
     "november", "december"))} | {m: i + 1 for i, m in enumerate(
    ("januar", "februar", "märz", "april", "mai", "juni", "juli", "august", "september", "oktober", "november",
     "dezember"))}
DATE_WORDS = re.compile(r"(?:as (?:at|of)|ended|zum|per)\s+(\d{1,2})\.?\s+([A-Za-zä]+)\s+(\d{4})", re.I)
DATE_DOTS = re.compile(r"(?:as (?:at|of)|ended|zum|per)\s+(\d{1,2})\.(\d{1,2})\.(\d{4})", re.I)
ISIN = table.ISIN_RE

# A schedule page's heading states its day; a table of contents names the
# schedules without one.
SCHEDULE = re.compile(r"Schedule of Investments as (?:at|of)|Aufstellung des Wertpapierbestandes zum")
INVENTORY = re.compile(r"Inventory of [Ff]und assets|Inventar des Fondsvermögens")
# Where a schedule's securities end: the derivatives that follow them,
# or the total.
SCHEDULE_END = re.compile(r"^(?:Derivative Finanzinstrumente|Financial Derivative Instruments|TOTAL INVESTMENTS|"
                          r"Total Investments|Forward Foreign Exchange Contracts)", re.I)
INVENTORY_END = re.compile(r"^(?:Total securities and similar instruments|Total Wertpapiere und ähnliche|"
                           r"Bank deposits|Bankguthaben|Derivative financial instruments|Derivative Finanzinstrumente)",
                           re.I)
# The sections a holding sits in: its asset type, as the report writes it.
# Longest first, so a heading opens the longest section it starts with.
SECTIONS = tuple(sorted((
    "common stocks", "preferred stocks", "corporate bonds", "government bonds", "asset and mortgage backed",
    "funds", "investment funds", "rights", "warrants", "stammaktien", "vorzugsaktien", "investmentfonds",
    "bezugsrechte", "optionsscheine", "bearer shares", "registered shares", "participation certificates",
    "other shares", "inhaberaktien", "namenaktien", "partizipationsscheine",
    "aktien und sonstige beteiligungswertpapiere und -rechte"), key=len, reverse=True))
INDUSTRY = re.compile(r"^(?P<h>[^\d(]+?) \(\(?-?[\d.,]+ ?%\)?\)")
# The least percent of a fund the lines read must cover.
MIN_PERCENT = 50
# The words a share class adds to a sub-fund's name.
CLASS_WORDS = {"usd", "chf", "eur", "gbp", "jpy", "cad", "aud", "hkd", "sgd", "sek", "nok", "dkk", "dis", "acc",
               "inc", "dist", "hedged", "class"}
REPORT_WORDS = re.compile(r"\s*(?:Semi-Annual Report|Annual Report|Halbjahresbericht|Jahresbericht)\b.*$", re.I)
FOOTER = re.compile(r"^(?P<t>Swisscanto .+?) \d+/\d+$")
# A holding line in each layout: a schedule's name, quantity, value and
# percent; an inventory's name, currency, quantities and percent; an
# ISIN inventory's ISIN, name, quantities, currency, price, value and
# percent.
HOLDING = re.compile(r"^(?P<name>.+?) (?P<qty>\(?-?[\d.,]+\)?) (?P<val>\(?-?[\d.,]+\)?) (?P<pct>\(?-?\d+[.,]\d{2}\)?)$")
INVENTORY_LINE = re.compile(r"^(?P<name>.+?)\*? (?P<ccy>[A-Z]{3}) (?P<nums>[\d ]+?) (?P<pct>\d+\.\d{2})(?: [\d ]+)?$")
ISIN_LINE = re.compile(r"^(?P<isin>[A-Z]{2}[A-Z0-9]{9}\d) (?P<name>.+?) (?P<q0>-?[\d']+(?:\.\d+)?) "
                       r"(?P<q1>[\d']+(?:\.\d+)?) (?P<q2>[\d']+(?:\.\d+)?) (?P<qty>-?[\d']+(?:\.\d+)?) "
                       r"(?P<ccy>[A-Z]{3}) (?P<px>[\d']+(?:\.\d+)?) (?P<val>-?[\d']+(?:\.\d+)?) "
                       r"(?P<pct>-?\d+\.\d{2})(?: \S{1,3}\))?$")
# The line that ends an inventory's country block, and names the country.
COUNTRY_TOTAL = re.compile(r"^TOTAL (?P<c>[^\d]+?) [\d ]*\d")
# The reports whose pages parse keeps, by their files' hash: an
# umbrella's report serves each of its sub-funds a run holds.
PAGES_KEPT = 4
_pages: OrderedDict = OrderedDict()


class ReportProvider(Provider):
    """A provider that serves a fund's newest annual and semi-annual
    reports. A claim names the fund as the source does and the reports'
    URLs; fetch stores each PDF as served, and parse reads the fund's
    inventory from each: two compositions, one per report day."""
    serves_funds = True

    def fetch(self, claim, as_of, outdir: Path, ctx):
        files, days = [], []
        for kind, url in claim["reports"]:
            path = outdir / f"{kind}.pdf"
            fetch_pdf(ctx, url, path)
            files.append(path.name)
            days.append(ctx.once(("report_day", url), lambda path=path: report_date(pdf_pages(path, last=5))))
        day = max(filter(None, days), default=None)
        if day is None:
            raise ProviderError("no report states the day of its holdings")
        write_meta(outdir, provider=self.name, ref=claim["ref"], name=claim["name"], files=files,
                   urls=[u for _, u in claim["reports"]], as_of=day.isoformat())
        return day

    def parse(self, indir: Path) -> Parsed:
        """Each report's composition; of two that state the same day, the
        semi-annual report's, so the day's lines are one report's."""
        meta = read_meta(indir)
        by_day, errors = {}, []
        for f in meta["files"]:
            try:
                p = parse_report(report_pages(indir / f), meta["name"], meta["ref"], provider=self.name)
            except ProviderError as e:
                errors.append(f"{f}: {e}")
                continue
            (c,) = p.compositions
            by_day[(c["as_of"], c["basis"])] = p
        if not by_day:
            raise ProviderError("; ".join(errors) or "no report")
        return Parsed(compositions=[c for p in by_day.values() for c in p.compositions],
                      lines=[ln for p in by_day.values() for ln in p.lines])


def save_pdf(resp, path: Path) -> Path:
    if not resp.body.startswith(b"%PDF"):
        raise ProviderError(f"{resp.url} is not a PDF")
    bronze.atomic_write_bytes(path, resp.body)
    return path


def fetch_pdf(ctx, url: str, path: Path) -> None:
    """Stores the PDF at `url` as `path`. A report several funds share is
    asked for once a run, and linked from the first fund's folder into
    the next one's."""
    first = ctx.once(("report", url), lambda: save_pdf(ctx.client.get(url), path))
    if first == path:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        os.link(first, path)
    except OSError:
        bronze.atomic_write_bytes(path, first.read_bytes())


def report_pages(path: Path) -> tuple:
    """pdf_pages(path), kept for the last PAGES_KEPT reports read by their
    files' hash: the copies of one report read once."""
    key = bronze.sha256_file(path)[0]
    if key in _pages:
        _pages.move_to_end(key)
    else:
        _pages[key] = tuple(pdf_pages(path))
        if len(_pages) > PAGES_KEPT:
            _pages.popitem(last=False)
    return _pages[key]


def pdf_pages(path: Path, last: int | None = None) -> list:
    """Each page's text, in layout order; the first `last` pages only
    where given. collectorkit.pdf joins the pages; a report is read page
    by page."""
    import pypdfium2 as pdfium

    pdf = pdfium.PdfDocument(str(path))
    try:
        out = []
        for i in range(min(len(pdf), last or len(pdf))):
            page = pdf[i]
            try:
                tp = page.get_textpage()
                try:
                    out.append(tp.get_text_bounded() or "")
                finally:
                    tp.close()
            finally:
                page.close()
        return out
    finally:
        pdf.close()


def words(s: str) -> set:
    """A title's or a name's words: lower-cased, marks and punctuation
    dropped. A ligature's letters fold to their first (fi and fl to f),
    on both sides alike: a report's text may lose the second (Qualifed)."""
    s = unicodedata.normalize("NFKC", s).lower().replace("&", " and ").replace("fi", "f").replace("fl", "f")
    return {w for w in re.split(r"[^0-9a-zäöüéèàç]+", s) if w}


def own_title(titles, fund_name: str) -> str | None:
    """The sub-fund title a fund's name names: of the titles of three
    words or more whose every word the name holds, the one with the most
    words. A source may order a name's words otherwise (Index Fund Q -
    Equity Fund for Index Equity Fund)."""
    name = words(fund_name)
    fits = [t for t in set(titles) if len(words(t)) >= 3 and words(t) <= name]
    return max(fits, key=lambda t: (len(words(t)), t)) if fits else None


def report_date(pages: list) -> date | None:
    """The day a report states its holdings for, from its first pages."""
    text = " ".join(" ".join(p.split()) for p in pages[:5])
    for m in DATE_WORDS.finditer(text):
        if m.group(2).lower() in MONTHS:
            return date(int(m.group(3)), MONTHS[m.group(2).lower()], int(m.group(1)))
    m = DATE_DOTS.search(text)
    return date(int(m.group(3)), int(m.group(2)), int(m.group(1))) if m else None


def number(s: str, decimal: str) -> Decimal | None:
    """A printed number: thousands marks (',', '.', ' ', "'") dropped and
    the decimal mark read as such; a value in parentheses is negative."""
    s = s.strip()
    neg = s.startswith("(") and s.endswith(")") or s.startswith("-")
    s = s.strip("()-")
    thousands = {",": ".", ".": ","}.get(decimal, "")
    s = s.replace(thousands, "").replace(" ", "").replace("'", "").replace(decimal, ".")
    try:
        v = Decimal(s)
    except ArithmeticError:
        return None
    return -v if neg else v


def parse_report(pages: list, fund_name: str, ref: str, *, provider: str) -> Parsed:
    """The held fund's inventory in one report as silver rows for `ref`.
    A word of the fund's name that its sub-fund's title lacks must be the
    umbrella's (the cover's, or the start of one: SOL for SOLUTIONS), its
    class's (a line that starts with the title, or a line of the
    sub-fund's own pages other than a holding) or a currency's; any
    other (SRI where the title says Socially Responsible) means the title
    found is another sub-fund's, and the report is refused."""
    as_of = report_date(pages)
    if as_of is None:
        raise ProviderError("the report states no day")
    if any(INVENTORY.search(p) for p in pages):
        isin_lines = any(ISIN.match(ln.split(" ", 1)[0]) for p in pages if INVENTORY.search(p)
                         for ln in p.splitlines() if " " in ln)
        title, rows, mine = _isin_inventory(pages, fund_name) if isin_lines else _inventory(pages, fund_name)
    elif any(SCHEDULE.search(p) for p in pages):
        title, rows, mine = _schedule(pages, fund_name)
    else:
        raise ProviderError("the report holds no inventory")
    if not rows:
        raise ProviderError(f"the report holds no inventory for {fund_name!r}")
    # A layout read in part (a bond inventory's lines, a fund line among
    # them read) covers a small share of the fund: not its composition.
    printed = sum((r["pct"] for r in rows), Decimal(0))
    if printed < MIN_PERCENT:
        raise ProviderError(f"the lines read for {title!r} cover {printed}% of the fund")
    cover = words(pages[0])
    told = cover | CLASS_WORDS | words(" ".join(
        ln for p in pages for ln in p.splitlines() if " ".join(ln.split()).lower().startswith(title.lower())))
    told |= words(" ".join(ln for p in mine for ln in p.splitlines() if not _row_line(ln.strip())))
    unexplained = {w for w in words(fund_name) - words(title) - told
                   if len(w) > 1 and not (len(w) >= 3 and any(t.startswith(w) for t in cover))}
    if unexplained:
        raise ProviderError(f"{fund_name!r} names {sorted(unexplained)}, which neither the sub-fund {title!r} "
                            "nor the report names")
    lns = _lines(rows, ref, as_of)
    return Parsed(lines=lns, compositions=[table.composition(ref, as_of, lns, provider=provider, source_kind="report",
                                                             fund_name=fund_name, payload={"sub_fund": title})])


def _row_line(ln: str) -> bool:
    """Whether a line reads as a holding in any layout."""
    return bool(HOLDING.match(ln) or INVENTORY_LINE.match(ln) or ISIN.match(ln.split(" ", 1)[0]))


def _lines(rows: list, ref: str, as_of: date) -> list:
    """Silver lines from parsed rows, each weighted by its market value
    where every row has one."""
    printed = sum((r["pct"] for r in rows), Decimal(0))
    values = [r.get("value") for r in rows]
    total = sum(values, Decimal(0)) if all(v is not None for v in values) else Decimal(0)
    out = []
    for r in rows:
        weight = r["pct"] if not total else r["value"] * printed / total
        out.append(row("lines", fund_ref=ref, as_of=as_of, basis="holdings", line_no=len(out) + 1, isin=r.get("isin"),
                       name=r["name"], asset_type=r.get("section"), weight=table.dec(round(weight, 8)),
                       quantity=table.dec(r.get("quantity")), market_value=table.dec(r.get("value")),
                       currency=r.get("currency"), country=r.get("country"), industry=r.get("industry")))
    return out


def _section(line: str) -> str | None:
    """The asset-type section a heading line opens, as the report writes
    it. A heading may carry its own first letters again (a drop cap)."""
    low = " ".join(line.lower().split())
    for s in SECTIONS:
        if low == s or low.startswith(s + " "):
            return line.strip()[:len(s)]
    return None


def _own_pages(pages: list, fund_name: str, titles_of) -> tuple:
    """The held fund's sub-fund title and the pages of its part of a
    report. titles_of gives the sub-fund titles a page names, or None for
    a page that continues the one before. A title that names only what
    the fund's name puts before its dash names the umbrella, not a
    sub-fund."""
    titled = [titles_of(p) for p in pages]
    umbrella = words(re.split(r"\s[-–]\s", fund_name, maxsplit=1)[0]) if re.search(r"\s[-–]\s", fund_name) else set()
    own = own_title([t for ts in titled if ts for t in ts if not words(t) <= umbrella], fund_name)
    out, current = [], False
    for p, ts in zip(pages, titled, strict=True):
        if ts is not None:
            current = own in ts
        if current and own:
            out.append(p)
    return own, out


def _schedule(pages: list, fund_name: str) -> tuple:
    """A schedule of investments: holding lines of name, quantity, market
    value and percent, from the held fund's pages up to its derivatives.
    A page names its sub-fund at its top, or beside the heading that
    states its day: on the line before, or the first line after with no
    figure. A page with that heading and no title continues the one
    before. A holding counts once a section heading has opened."""
    def titles_of(p):
        lines = [ln.strip() for ln in p.splitlines() if ln.strip()]
        found = []
        for i, ln in enumerate(lines):
            if SCHEDULE.search(ln):
                after = next((t for t in lines[i + 1:i + 4] if not re.search(r"\d", t)), "")
                found += [lines[0], lines[i - 1] if i else "", after]
        found = [t for t in found if not re.search(r"\d", t) and len(words(t)) >= 3]
        return found or None

    own, mine = _own_pages(pages, fund_name, titles_of)
    rows, section, industry, done = [], None, None, False
    for p in mine:
        for ln in p.splitlines():
            ln = ln.strip()
            if done or not ln:
                continue
            if SCHEDULE_END.match(ln):
                done = True
                continue
            if not re.search(r"\d", ln) and (s := _section(ln)):
                section = s
                continue
            if m := INDUSTRY.match(ln):
                industry = m.group("h").strip()
                continue
            holding = _holding(ln) if section else None
            if holding:
                rows.append({**holding, "section": section, "industry": industry})
    return own, rows, mine


def _holding(ln: str) -> dict | None:
    m = HOLDING.match(ln)
    if not m or re.match(r"(?:Total|Summe)\b", m.group("name")) or m.group("name").endswith("insgesamt"):
        return None
    decimal = m.group("pct").strip("()-")[-3]
    return {"name": m.group("name"), "quantity": number(m.group("qty"), decimal),
            "value": number(m.group("val"), decimal), "pct": number(m.group("pct"), decimal)}


def _inventory(pages: list, fund_name: str) -> tuple:
    """An inventory of fund assets: each page names its sub-fund in its
    running header, its first line with letters. A country block ends in
    a TOTAL line that names the country, which its holdings take. Its
    numbers, grouped by spaces, do not split into columns, so the
    printed percent stands."""
    def titles_of(p):
        first = next((ln for ln in p.splitlines()[:3] if re.search(r"[A-Za-z]", ln)), "")
        title = re.sub(r"\s\d{1,3}\s", " ", REPORT_WORDS.sub("", first))
        return [title.split(" – ")[-1].strip()] if title.strip() else None

    own, mine = _own_pages(pages, fund_name, titles_of)
    rows, block, section, started = [], [], None, False
    for p in mine:
        if INVENTORY.search(p):
            started = True
        if not started:
            continue
        for ln in p.splitlines()[1:]:
            ln = ln.strip()
            if INVENTORY_END.match(ln):
                return own, rows + block, mine
            if not re.search(r"\d", ln) and (s := _section(ln)):
                section = s
                continue
            if m := COUNTRY_TOTAL.match(ln):
                rows += [{**r, "country": m.group("c").strip()} for r in block]
                block = []
                continue
            m = INVENTORY_LINE.match(ln)
            if m and not ln.upper().startswith("TOTAL"):
                block.append({"name": m.group("name").strip(), "currency": m.group("ccy"),
                              "pct": Decimal(m.group("pct")), "section": section})
    return own, rows + block, mine


def _isin_inventory(pages: list, fund_name: str) -> tuple:
    """An inventory with an ISIN per line; each page names its sub-fund
    in its footer. A position sold out during the period is left out."""
    # Where a page's text comes out column by column (ISINs first, figures
    # after), its lines do not read: the composition's remainder holds
    # them.
    def titles_of(p):
        found = [m.group("t") for ln in p.splitlines() if (m := FOOTER.match(ln.strip()))]
        return found or None

    own, mine = _own_pages(pages, fund_name, titles_of)
    rows, section, started = [], None, False
    for p in mine:
        if INVENTORY.search(p):
            started = True
        if not started:
            continue
        lines = [ln.strip() for ln in p.splitlines()]
        for i, ln in enumerate(lines):
            if s := _section(re.sub(r"(\s+-?[\d'.]+)+$", "", ln)):
                section = s
                continue
            if not ISIN.match(ln.split(" ", 1)[0]):
                continue
            # A long name wraps: its figures follow on the next lines, up to
            # the next position, section or total.
            joined, m = ln, ISIN_LINE.match(ln)
            for nxt in lines[i + 1:i + 3]:
                if m or not nxt or ISIN.match(nxt.split(" ", 1)[0]) or _section(nxt) or nxt.lower().startswith("total"):
                    break
                joined += " " + nxt
                m = ISIN_LINE.match(joined)
            if not m:
                continue
            qty, value = number(m.group("qty"), "."), number(m.group("val"), ".")
            if not qty and not value:
                continue
            rows.append({"name": m.group("name"), "isin": m.group("isin"), "quantity": qty, "value": value,
                         "currency": m.group("ccy"), "pct": number(m.group("pct"), "."), "section": section})
    return own, rows, mine
