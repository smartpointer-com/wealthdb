"""
jpx: the ex-dividend days of the shares listed in Tokyo, from the Japan
Exchange Group's daily ex-rights list.

- Bulk: while a Japanese share (ISIN JP) held directly wants its
  dividend calendar, the plan reads the list's index page (INDEX). It
  links one workbook per business day, `YYYYMMDDEN.xls`, for the last
  KEEP days. Each linked file no earlier run fetched is one request, the
  oldest first. So a run makes one request for the index and at most
  KEEP for files. A file lists every TSE stock, ETF and REIT whose record
  day falls in the next two weeks, and it leaves the page after KEEP
  business days: the list covers coming days only.
- Parse: every row of a file whose remarks name a dividend or a
  distribution ("Ex-Dividend" and its kin) is one event of kind
  dividend, of a share held or not, under ref `jpx:<code>`, the code as
  the file writes it, with its record and ex days as stated. The list
  states no pay day and no amount. An event is dated by its file's day,
  so a newer file's row of it replaces an older one's. The issue's name,
  market segment, remarks, update mark and any other column go to the
  payload. Gold joins a row to a share by the share's TSE code.
- The file is a legacy Excel workbook (BIFF, .xls), which xlrd reads. A
  date cell and a day written as text (2026/10/23) both read as days. A
  file that is no workbook, or holds no list, is refused: a later run
  asks for it again while the page links it.
- A request's `answered` names the shares whose TSE code (tse_code) its
  file lists with a dividend.

Terms: README.md, jpx.
"""

from __future__ import annotations

import html
import io
import logging
import re
import urllib.parse
from datetime import date, datetime
from pathlib import Path

from collectorkit import bronze

from . import table
from .base import (Parsed, Provider, ProviderError, direct_share, event_row, ident, merge_events, present, read_bytes,
                   read_meta, write_meta)

HOST = "www.jpx.co.jp"
INDEX = f"https://{HOST}/english/listing/others/ex-rights/index.html"
LINK = re.compile(r"""href\s*=\s*["']([^"']*?(\d{8})EN\.xls)["']""", re.IGNORECASE)
# The files the index page keeps, one a business day.
KEEP = 5
REF = "ex-rights:{day}"
LIST_DOC = "ex-rights.xls"
CAL = "dividend_calendar"
# A TSE code: four characters, a letter allowed in the second and fourth
# places; a ticker may add the exchange (1Q01.T, 1Q01 JP).
CODE = re.compile(r"[0-9][0-9A-Z][0-9][0-9A-Z]")
EXCHANGE_SUFFIX = re.compile(r"[.\s:](?:T|JP|JT|TYO|TSE)$")
LISTED_CODE = re.compile(r"[0-9A-Z]{3,6}")
DIVIDEND = re.compile(r"(?i)dividend|distribution")
# A list's columns, by the words their header holds (lower case, a hyphen
# read as a space), in the order they are matched: each claims the first
# header that holds one of its words. A header none claims keeps its own
# text.
COLUMNS = (("record_date", ("record date",)), ("ex_date", ("ex rights date", "ex dividend date", "ex date")),
           ("code", ("code",)), ("name", ("issue name", "name")), ("segment", ("market",)),
           ("remarks", ("remark",)), ("update", ("update",)))
# How far down a file its header may lie, below a title and notes.
HEADER_ROWS = 30

log = logging.getLogger("refdata.jpx")


def wanted_share(security) -> bool:
    """Whether a security is a Japanese share held directly: public
    equity, no depositary receipt, under a Japanese ISIN."""
    return direct_share(security) and ident(security, "isin").startswith("JP")


def tse_code(security) -> str:
    """A share's TSE code, where its symbol is one or a ticker of one;
    "" where it is neither."""
    symbol = EXCHANGE_SUFFIX.sub("", ident(security, "symbol"))
    return symbol if CODE.fullmatch(symbol) else ""


class Jpx(Provider):
    name = "jpx"
    serves_bulk = True
    answers = frozenset({CAL})

    def plan_bulk(self, securities, ctx, today, fetched, max_age) -> list:
        """One request per list the index page links that no earlier run
        fetched, while a Japanese share wants its calendar. Each claim
        names every such share in `keys`, and by TSE code in `codes`."""
        shares = [s for s in securities if CAL in s.wants and wanted_share(s)]
        if not shares:
            return []
        codes = {}
        for s in shares:
            if code := tse_code(s):
                codes.setdefault(code, []).append(s.key)
        linked = ctx.once("jpx_index", lambda: files(ctx.client.get(INDEX).text()))
        if not linked:
            log.warning("JPX's ex-rights index links no list")
        keys = [s.key for s in shares]
        return [(ref, {"ref": ref, "url": url, "day": day.isoformat(), "keys": keys, "codes": codes, "facts": [CAL]})
                for day, url in linked if (ref := REF.format(day=day.isoformat())) not in fetched]

    def fetch_bulk(self, claim, outdir: Path, ctx):
        resp = ctx.client.get(claim["url"])
        rows = list_rows(sheet_rows(resp.body))
        if rows is None:
            raise ProviderError(f"{resp.url} holds no ex-rights list")
        bronze.atomic_write_bytes(outdir / LIST_DOC, resp.body)
        stated = {e["ref"].removeprefix("jpx:") for e in events(rows, date.fromisoformat(claim["day"]),
                                                                  provider=self.name)}
        answered = sorted({k for code, keys in claim["codes"].items() if code in stated for k in keys})
        write_meta(outdir, **claim, provider=self.name, as_of=claim["day"], answered=answered)

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        rows = list_rows(sheet_rows(read_bytes(indir, LIST_DOC))) or []
        return Parsed(dividend_events=events(rows, date.fromisoformat(meta["as_of"]), provider=self.name))

    def probe(self, ctx) -> str:
        return f"JPX ex-rights index: {len(files(ctx.client.get(INDEX).text()))} lists"


def files(page: str) -> list:
    """(day, URL) of each list the index page links on JPX's own host, by
    the day its name states, the newest KEEP, oldest first."""
    found = {}
    for href, digits in LINK.findall(page):
        url = urllib.parse.urljoin(INDEX, html.unescape(href))
        parts = urllib.parse.urlsplit(url)
        if parts.scheme != "https" or parts.hostname != HOST:
            continue
        try:
            day = datetime.strptime(digits, "%Y%m%d").date()
        except ValueError:
            continue
        found.setdefault(day, url)
    return sorted(found.items())[-KEEP:]


def sheet_rows(data: bytes) -> list:
    """The first sheet of a workbook as rows of cells: a date cell as a
    date, a number as its text (42, not 42.0), text stripped, any
    other cell "". xlrd is imported here: no other provider needs it."""
    import xlrd

    try:
        book = xlrd.open_workbook(file_contents=data, logfile=io.StringIO())
        sheet = book.sheet_by_index(0)
    except Exception as e:  # noqa: BLE001 - xlrd raises many kinds of error on a file it cannot read
        raise ProviderError(f"no workbook xlrd reads: {e}") from e

    def cell(c):
        if c.ctype == xlrd.XL_CELL_DATE:
            try:
                return xlrd.xldate_as_datetime(c.value, book.datemode).date()
            except (ValueError, OverflowError):
                return ""
        if c.ctype == xlrd.XL_CELL_NUMBER:
            return str(int(c.value)) if float(c.value).is_integer() else str(c.value)
        return c.value.strip() if c.ctype == xlrd.XL_CELL_TEXT else ""
    return [[cell(c) for c in sheet.row(r)] for r in range(sheet.nrows)]


def _norm(text) -> str:
    return re.sub(r"[\s-]+", " ", str(text)).strip().lower()


def header(cells: list) -> dict:
    """column -> the field of COLUMNS its header names, else the header's
    own text; a column without a header is none."""
    out, taken = {}, set()
    for j, cell in enumerate(cells):
        text = _norm(cell) if not isinstance(cell, date) else ""
        if not text:
            continue
        field = next((f for f, words in COLUMNS if f not in taken and any(w in text for w in words)), None)
        if field:
            taken.add(field)
        out[j] = field or " ".join(str(cell).split())
    return out


def list_rows(rows: list) -> list | None:
    """The rows below a list's header, each as name (header()) -> cell,
    blank cells left out; None where no row of the first HEADER_ROWS names
    both a code and a record date."""
    start = next((i for i, cells in enumerate(rows[:HEADER_ROWS])
                  if {"code", "record_date"} <= set(header(cells).values())), None)
    if start is None:
        return None
    names = header(rows[start])
    return [{names[j]: c for j, c in enumerate(cells) if j in names and c != ""} for cells in rows[start + 1:]]


def _day(value) -> date | None:
    """A list's day: a date cell, or text as JPX writes it (2026/10/23)
    or as table.parse_day reads it."""
    if isinstance(value, date):
        return value
    s = str(value or "").strip()
    for fmt in ("%Y/%m/%d", "%Y.%m.%d"):
        try:
            return datetime.strptime(s, fmt).date()
        except ValueError:
            continue
    return table.parse_day(re.sub(r"^([A-Za-z]{3})\.", r"\1", s))


def events(rows: list, day: date, *, provider: str) -> list:
    """The dividend_events rows of a list's rows, dated `day`: one per
    row whose remarks name a dividend or a distribution and that states a
    code and a record or an ex day, the rows of one code and days one
    event (base.merge_events). Its other cells go to the payload."""
    out = []
    for r in rows:
        code = str(r.get("code") or "").strip().upper()
        if not LISTED_CODE.fullmatch(code) or not DIVIDEND.search(str(r.get("remarks") or "")):
            continue
        record, ex = _day(r.get("record_date")), _day(r.get("ex_date"))
        if not (record or ex):
            continue
        payload = present({k: v.isoformat() if isinstance(v, date) else v for k, v in r.items()
                           if k not in ("code", "record_date", "ex_date")})
        out.append(event_row(f"jpx:{code}", provider, day, "dividend", {"ex_date": ex, "record_date": record},
                             payload=payload))
    return merge_events(out)
