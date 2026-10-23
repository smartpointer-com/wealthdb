"""
six: the dividends of Swiss and Liechtenstein shares, from SIX Swiss
Exchange's share pages and its ex-dividend notices.

- Claim: a share held directly (public equity, no depositary receipt)
  whose ISIN is Swiss or Liechtenstein's (CH, LI), while its dividend
  calendar is due.
- Fetch: the share page's dividend answer
  (`/sheldon/share_details/v3/<ISIN><CCY>4/share/dividend.json`, CCY the
  share's currency, else CHF): one row per dividend with its ex day and
  amount, every one SIX lists. A share's answer is asked for once a
  month at most (min_age_days), whatever max_age_days says, and a
  request that failed waits the month too. ASKED in the data dir keeps
  the day of each request as it ends (asked()), so a run that dies
  after it still counts it. A share SIX does not list
  (404) answers no dividend. An answer in no layout the parser reads is
  kept with a warning and counts as had all the same: a parser fix
  reads it with `load --force`. The fetch also carries the notices
  earlier complete runs kept for the share (kept_notices), so its parse
  joins them: a later answer keeps the pay days earlier notices stated.
- Bulk: the ex-dividend notices state a dividend's pay day. One search a
  run asks for the whole market's notices of the days since the last
  day an earlier search finished (searched_through()); the first one
  reaches back LOOKBACK_DAYS, and a second run on the same day searches
  nothing. The search answers notices of every type, PAGE_SIZE a page,
  pages numbered from FIRST_PAGE, and `totalCount` of them in all: each
  page is asked for until the pages hold them all, and pages that stop
  short of the count fail the search. It goes through its days in spans
  of SPAN_DAYS, at most MAX_PAGES pages a run (fetch_bulk). A notice
  without an id is never read.
  Only the ex-dividend notices (noticeType EX) count. A notice names its
  share's ISIN, several joined by a slash or a comma, or none. One that
  names a claimed share costs one request more, for its details. One
  that names none costs it only where its title's ex day (Ex
  31.07.2026) is one a claimed share's dividend answer states, and is
  kept for the claimed ISINs its details name. The details' text names
  the issuer's contact person and phone, and a search answer's notice
  its contact: only the four FIELDS and the ISINs are read from a
  detail, each field kept as the text writes it, and nothing else of a
  detail or a search answer is written or logged. A detail that does
  not come fails the search, so the next run asks for the same days
  again; one SIX no longer serves (404) is skipped.
- Parse: the rows of one ex day are one event of kind dividend
  (base.merge_events), their amounts summed; the source's other fields
  (the adjusted amount, the capital repayment) go to the payload. A
  notice joins the event of its share and ex day and gives it its pay
  day. A notice that states its ex day, pay day, gross amount and
  currency is also an event of its own, under the same id: it stands for
  the event until the share's next dividend answer joins it. A share's
  dividend answer lists its whole history, so its events are complete
  (Parsed.events_complete), the notices it carries that join no row
  among them; the notices a search finds add to them. SIX states no
  record day; gold derives it from the ex day.

Terms: README.md, six.
"""

from __future__ import annotations

import html
import logging
import re
from datetime import date, timedelta
from pathlib import Path

import fetcher
from collectorkit import bronze

from . import table
from .base import (Parsed, Provider, ProviderError, dec, direct_share, earlier_docs, earlier_metas, event_id, event_row,
                   ident, latest_through, merge_events, present, read_json, read_meta, read_state, today_utc,
                   write_meta)

NAME = "six"
HOST = "https://www.six-group.com"
DIVIDENDS = HOST + "/sheldon/share_details/v3/{isin}{currency}4/share/dividend.json"
SEARCH = HOST + "/sheldon/official_notices/v2/find.json"
DETAILS = HOST + "/sheldon/official_notices/v2/details/{id}.json"
CAL = "dividend_calendar"
NOTICES_REF = "ex-dividend-notices"
DIVIDEND_DOC = "dividend.json"
NOTICES_DOC = "notices.json"
# How far back the first search reaches: the pay days of a year's
# dividends.
LOOKBACK_DAYS = 366
DEFAULT_CURRENCY = "CHF"
SHARE_ISIN = re.compile(r"(?:CH|LI)[A-Z0-9]{9}\d")
ANY_ISIN = re.compile(r"\b[A-Z]{2}[A-Z0-9]{9}\d\b")
NOTICE_ID = re.compile(r"[A-Za-z0-9_-]{1,64}")
# The keys a search answer names a notice's id by.
ID_KEYS = ("noticeId", "id")
# The search's page: the notices it holds, and its number, from 0; and
# the count of every notice the search finds.
PAGE_SIZE = 1000
PAGE_PARAMS = ("pageSize", "pageNumber")
FIRST_PAGE = 0
COUNT_KEY = "totalCount"
# A search answer's notice: its type (EX for an ex-dividend notice), its
# share's ISIN, several joined by a slash or a comma, or none ("NONE"),
# and its title, which writes its ex day ("Ex 31.07.2026").
TYPE_KEY, EX_TYPE, ISIN_KEY, TITLE_KEY = "noticeType", "EX", "isin", "title"
ISIN_SEPARATOR = re.compile(r"\s*[/,]\s*")
TITLE_EX_DAY = re.compile(r"(?i)\bex\b\D*(\d{1,2}\.\d{1,2}\.\d{4})")
# This run's dividend answers' ex days, by ISIN, in the run's cache.
RUN_DAYS = "six_run_ex_days"
# The day of each share's last dividend request, kept in the data dir
# (0600): security key -> YYYY-MM-DD.
ASKED = "six-asked.json"
# The search goes through its days in spans of about a month, asking for
# at most MAX_PAGES pages a run: a first search of a year takes a few
# dozen.
SPAN_DAYS = 31
MAX_PAGES = 40
# A dividend row's own fields; any other goes to the payload.
EX_KEY, VALUE_KEY, CURRENCY_KEY = "exDividendDate", "value", "currency"

# The fields a notice's details are read for, by the label their text
# gives each, with the form of its value, the one group each captures: a
# day, an amount (after a currency code, where the text writes one) and
# a currency code.
EX, PAY, GROSS, CURRENCY = "Ex Date", "Payment Date", "Gross amount payable per security", "Currency Code"
DAY = r"(\d{1,2}\.\d{1,2}\.\d{4}|\d{4}-\d{2}-\d{2}|\d{8})(?!\d)"
FIELDS = {EX: DAY, PAY: DAY, GROSS: r"(?:[A-Z]{3}\s*)?(\d[\d'’]*(?:\.\d+)?)", CURRENCY: r"([A-Z]{3})\b"}
FIELD_PATTERNS = {label: re.compile(r"(?i:" + r"[\s-]+".join(map(re.escape, label.split())) + r")\b[\s:]*" + token)
                  for label, token in FIELDS.items()}
BLOCK_END = re.compile(r"(?i)<br\s*/?>|</(?:p|div|tr|li|h\d)>")
CELL_END = re.compile(r"(?i)</t[dh]>")
TAG = re.compile(r"<[^>]+>")

log = logging.getLogger("refdata.six")


def claims(security) -> bool:
    """Whether six serves a share: held directly, public equity and no
    depositary receipt, under a Swiss or Liechtenstein ISIN."""
    return direct_share(security) and bool(SHARE_ISIN.fullmatch(ident(security, "isin")))


def currency_of(security) -> str:
    """The currency SIX lists a share's dividends under: the share's,
    else CHF."""
    code = (security.currency or "").strip().upper()
    return code if re.fullmatch(r"[A-Z]{3}", code) else DEFAULT_CURRENCY


class Six(Provider):
    name = NAME
    serves_securities = True
    serves_bulk = True
    answers = frozenset({CAL})
    # A share's dividend answer is asked for once a month at most.
    min_age_days = 30

    def claim_security(self, security, ctx):
        if CAL not in security.wants or not claims(security):
            return None
        return {"key": security.key, "isin": ident(security, "isin"), "currency": currency_of(security),
                "facts": [CAL]}

    def asked(self, ctx) -> dict:
        """security key -> the day of the last request for its dividends,
        as ASKED in the data dir keeps it (base.Provider.asked)."""
        return read_asked(Path(ctx.data_dir) / ASKED) if ctx is not None and ctx.data_dir else {}

    def fetch_security(self, claim, outdir: Path, ctx):
        listed, resp = True, None
        days = self.asked(ctx)
        try:
            resp = ctx.client.get(DIVIDENDS.format(isin=claim["isin"], currency=claim["currency"]))
        except fetcher.FetchError as e:
            if e.status != 404:
                raise
            log.warning("%s: SIX lists no share %s in %s", claim["key"], claim["isin"], claim["currency"])
            listed = False
        finally:
            # Kept as each request ends, whatever its answer, so a run
            # that dies after it still counts it.
            if ctx.data_dir:
                bronze.atomic_write_json(Path(ctx.data_dir) / ASKED,
                                         {**{k: d.isoformat() for k, d in days.items()},
                                          claim["key"]: today_utc().isoformat()})
        if resp is not None:
            doc = resp.json()  # an answer that is not JSON fails the fetch
            bronze.atomic_write_bytes(outdir / DIVIDEND_DOC, resp.body)
            # Kept all the same, and not asked for again before max_age:
            # asking again would not change the layout, and a parser fix
            # reads the kept answer with `load --force`.
            if records(doc, EX_KEY) is None:
                log.warning("%s: its dividends matched no known layout", claim["key"])
            ctx.cache.setdefault(RUN_DAYS, {}).setdefault(claim["isin"], set()).update(ex_days(doc))
        notices = sorted(kept_notices(ctx).get(claim["isin"], {}).values(), key=_order)
        if notices:
            bronze.atomic_write_json(outdir / NOTICES_DOC, notices)
        write_meta(outdir, provider=self.name, key=claim["key"], isin=claim["isin"], currency=claim["currency"],
                   listed=listed, as_of=today_utc().isoformat())

    def plan_bulk(self, securities, ctx, today, fetched, max_age) -> list:
        """One search a run for the ex-dividend notices, while a share is
        claimed. It covers the days from the last day an earlier search
        finished (searched_through()), else that of the last search, which
        it asks for again as notices appear through a day, else from
        LOOKBACK_DAYS back, to today. A run on that day plans none. The
        claim names each claimed ISIN's security keys."""
        isins = {}
        for s in securities:
            if CAL in s.wants and claims(s):
                isins.setdefault(ident(s, "isin"), []).append(s.key)
        last = searched_through(getattr(ctx, "data_dir", None)) or fetched.get(NOTICES_REF)
        if not isins or (last is not None and last >= today):
            return []
        first = last or today - timedelta(days=LOOKBACK_DAYS)
        return [(NOTICES_REF, {"ref": NOTICES_REF, "first": first.isoformat(), "last": today.isoformat(),
                               "isins": isins, "keys": [k for keys in isins.values() for k in keys],
                               "facts": [CAL]})]

    def _search(self, first: date, last: date, ctx, pages: list) -> list:
        """Every notice with an id the search of the days first to last
        lists, of every type, page by page until the pages hold the
        `totalCount` the answer states. `pages` holds the pages the run
        may still ask for, and each page asked takes one. A page in no
        known layout, pages that stop short of the count, or a search the
        cap cuts short fail the search."""
        items, ids, held, page = [], set(), 0, FIRST_PAGE
        while True:
            if pages[0] <= 0:
                raise ProviderError(f"the search reached its cap of {MAX_PAGES} pages a run")
            pages[0] -= 1
            resp = ctx.client.get(SEARCH, params=search_params(first, last, page))
            doc = resp.json()
            got = records(doc, *ID_KEYS)
            if got is None:
                raise ProviderError(f"{resp.url}: the notices matched no known layout")
            named = [(nid, x) for x in got if (nid := notice_id(x)) is not None]
            new = [x for nid, x in named if nid not in ids]
            ids.update(nid for nid, _ in named)
            items += new
            held += len(new) + len(got) - len(named)
            count = doc.get(COUNT_KEY) if isinstance(doc, dict) else None
            if not isinstance(count, int) or isinstance(count, bool) or held >= count:
                return items
            if not got or (named and not new):
                raise ProviderError(f"the search lists {count} notices, and its pages hold {held}")
            page += 1

    def _notices(self, items, claimed, due, asked: set, ctx) -> list:
        """The kept notices of a search's notices (fetch_bulk), each asked
        for once a search: `asked` holds the ids asked for already."""
        kept = []
        for item in items:
            nid, named = notice_id(item), notice_isins(item)
            if item.get(TYPE_KEY) != EX_TYPE or nid in asked or (named and not named & claimed) or (
                    not named and title_ex_day(item) not in due):
                continue
            asked.add(nid)
            got = self._details(nid, ctx)
            if got is not None:
                fields, stated = got
                kept += [{"id": nid, "isin": isin, "fields": fields} for isin in sorted((named or stated) & claimed)]
        return kept

    def fetch_bulk(self, claim, outdir: Path, ctx):
        """The search's ex-dividend notices of the claimed shares, each
        with the fields its details state (NOTICES_DOC), span by span of
        SPAN_DAYS, the oldest first, within MAX_PAGES pages a run. A
        notice that names a claimed ISIN is asked for its details. One
        that names none is asked only where its title's ex day is one a
        claimed share's dividend answer states (dividend_days()), and kept
        for the claimed ISINs its details name. A span that does not
        finish ends the search: meta.json's `through` names the last day
        of the last span that did, and the next run goes on from it. Where
        none did, the search fails. Nothing else of a search answer is
        written or logged."""
        claimed = claim["isins"].keys()
        days = dividend_days(ctx)
        due = {day for isin in claimed for day in days.get(isin, ())}
        kept, listed, through, pages, asked = [], 0, None, [MAX_PAGES], set()
        for first, last in spans(date.fromisoformat(claim["first"]), date.fromisoformat(claim["last"])):
            try:
                items = self._search(first, last, ctx, pages)
                found = self._notices(items, claimed, due, asked, ctx)
            except (ProviderError, fetcher.FetchError) as e:
                if through is None:
                    raise
                log.warning("ex-dividend notices: the search of %s to %s did not finish (%s); the next run goes "
                            "on from %s", first, last, e, through)
                break
            kept += found
            listed += sum(x.get(TYPE_KEY) == EX_TYPE for x in items)
            through = last
        bronze.atomic_write_json(outdir / NOTICES_DOC, kept)
        answered = sorted({k for n in kept if standalone(notice_event(n)) for k in claim["isins"][n["isin"]]})
        write_meta(outdir, **claim, provider=self.name, listed=listed, answered=answered,
                   through=through.isoformat(), as_of=today_utc().isoformat())

    def _details(self, nid: str, ctx) -> tuple | None:
        """(the fields a notice's details state (notice_fields), the ISINs
        they name); None where SIX no longer serves them. The details
        themselves go nowhere."""
        try:
            doc = ctx.client.get(DETAILS.format(id=nid)).json()
        except fetcher.FetchError as e:
            if e.status != 404:
                raise
            log.warning("ex-dividend notice %s: SIX no longer serves it", nid)
            return None
        fields = notice_fields(doc)
        if EX not in fields:
            log.warning("ex-dividend notice %s: its details state no ex day", nid)
        return fields, {m for s in _strings(doc) for m in ANY_ISIN.findall(s.upper())}

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        day = date.fromisoformat(meta["as_of"])
        notices = read_json(indir, NOTICES_DOC) if (indir / NOTICES_DOC).is_file() else []
        if meta.get("ref") == NOTICES_REF:
            return Parsed(dividend_events=notice_events(notices, meta["isins"], day, provider=self.name))
        doc = read_json(indir, DIVIDEND_DOC) if (indir / DIVIDEND_DOC).is_file() else None
        rows = records(doc, EX_KEY) if doc is not None else None
        return Parsed(dividend_events=dividend_events(rows or [], notices, meta["key"], meta.get("currency"), day,
                                                      provider=self.name),
                      events_complete=rows is not None)

    def probe(self, ctx) -> str:
        """The first page of today's notices: how many of them there are,
        and how many of those it holds are ex-dividend notices."""
        today = today_utc()
        doc = ctx.client.get(SEARCH, params=search_params(today, today)).json()
        items = records(doc, *ID_KEYS) or []
        count = doc.get(COUNT_KEY) if isinstance(doc, dict) else None
        if not isinstance(count, int) or isinstance(count, bool):
            count = len(items)
        return f"SIX notices: {count} for today, {sum(x.get(TYPE_KEY) == EX_TYPE for x in items)} ex-dividend"


def search_params(first: date, last: date, page: int = FIRST_PAGE) -> dict:
    """The search for the notices of the days first to last, its page
    `page`. showExDividend keeps the ex-dividend notices among the
    others; it leaves out none."""
    size, number = PAGE_PARAMS
    return {"firstDate": first.strftime("%Y%m%d"), "lastDate": last.strftime("%Y%m%d"), "showExDividend": "true",
            size: PAGE_SIZE, number: page}


def read_asked(path: Path) -> dict:
    """security key -> the day ASKED keeps for it; empty where there is
    no file yet. One that does not read is an error: SIX's monthly ask is
    not guessed (base.read_state)."""
    return read_state(path, "SIX's monthly ask", date.fromisoformat)


def spans(first: date, last: date) -> list:
    """The days first to last as spans of SPAN_DAYS at most, the oldest
    first, each (its first day, its last day)."""
    out = []
    while first <= last:
        end = min(first + timedelta(days=SPAN_DAYS - 1), last)
        out.append((first, end))
        first = end + timedelta(days=1)
    return out


def searched_through(data_dir) -> date | None:
    """The last day an earlier complete run's search finished: the latest
    `through` of the searches it kept; None where none states one."""
    return latest_through(data_dir, NAME, "bulk")


def notice_isins(item: dict) -> set:
    """The ISINs a search answer's notice names in its ISIN field: one,
    several joined by a slash or a comma, or none ("NONE", null)."""
    return {s for s in ISIN_SEPARATOR.split(str(item.get(ISIN_KEY) or "").upper()) if ANY_ISIN.fullmatch(s)}


def title_ex_day(item: dict) -> date | None:
    """The ex day a search answer's notice writes in its title (Ex
    31.07.2026)."""
    m = TITLE_EX_DAY.search(str(item.get(TITLE_KEY) or ""))
    return table.parse_day(m.group(1)) if m else None


def ex_days(doc) -> set:
    """The ex days a share's dividend answer states."""
    return {d for r in records(doc, EX_KEY) or () if (d := table.parse_day(r.get(EX_KEY)))}


def dividend_days(ctx) -> dict:
    """ISIN -> the ex days the share's dividend answers state: those the
    requests of earlier complete runs in the data dir kept, read once a
    run, and those this run's requests brought (RUN_DAYS)."""
    def load():
        out = {}
        for folder, meta in earlier_metas(ctx.data_dir, NAME, "security"):
            try:
                doc = read_json(folder, DIVIDEND_DOC)
            except (ProviderError, OSError, ValueError):
                continue
            out.setdefault(meta.get("isin"), set()).update(ex_days(doc))
        return out
    earlier, now = ctx.once("six_dividend_days", load), ctx.cache.get(RUN_DAYS, {})
    return {isin: earlier.get(isin, set()) | now.get(isin, set()) for isin in {*earlier, *now}}


def _lists(doc):
    """Every list of objects an answer holds, depth first."""
    if isinstance(doc, list):
        if all(isinstance(x, dict) for x in doc):
            yield doc
        for x in doc:
            yield from _lists(x)
    elif isinstance(doc, dict):
        for v in doc.values():
            yield from _lists(v)


def records(doc, *keys) -> list | None:
    """The objects of the first list an answer holds whose objects name
    one of `keys`, wherever the answer holds it: SIX's rows or notices.
    [] where it holds none but empty lists, as for a share that paid no
    dividend; None where it holds others, a layout this provider does not
    read."""
    lists = list(_lists(doc))
    for found in lists:
        if any(k in x for x in found for k in keys):
            return found
    return None if any(lists) else []


def _strings(doc):
    """Every string an answer holds, a number's text too, depth first."""
    if isinstance(doc, dict):
        for v in doc.values():
            yield from _strings(v)
    elif isinstance(doc, list):
        for v in doc:
            yield from _strings(v)
    elif isinstance(doc, (str, int)) and not isinstance(doc, bool):
        yield str(doc)


def notice_id(item: dict) -> str | None:
    """A search answer's notice id, where it is one a details URL can
    name."""
    nid = next((str(item[k]) for k in ID_KEYS if item.get(k) not in (None, "")), None)
    return nid if nid is not None and NOTICE_ID.fullmatch(nid) else None


def notice_fields(doc) -> dict:
    """The FIELDS a notice's details state, label -> the value as the
    text writes it, and nothing else of them. The text may be plain or
    HTML: a break or a block ends a line, a table cell a field. A value
    follows its label, past spaces, a colon or a line break."""
    text = "\n".join(html.unescape(TAG.sub("", CELL_END.sub("\t", BLOCK_END.sub("\n", s)))) for s in _strings(doc))
    out = {}
    for label, pattern in FIELD_PATTERNS.items():
        m = pattern.search(text)
        if m:
            out[label] = m.group(1)
    return out


def notice_event(notice: dict) -> dict:
    """A kept notice's id, ex day, pay day, gross amount and currency, read
    from the fields it keeps; {} where it states no ex day."""
    f = notice.get("fields") or {}
    ex = table.parse_day(f.get(EX))
    if ex is None:
        return {}
    return {"id": str(notice.get("id")), "ex": ex, "pay": table.parse_day(f.get(PAY)), "amount": dec(f.get(GROSS)),
            "currency": (f.get(CURRENCY) or "").upper() or None}


def standalone(event: dict) -> bool:
    """Whether a notice is an event of its own: it states its ex day, pay
    day, gross amount and currency. One that lacks any of them only gives
    its pay day to its share's dividend row, which it would otherwise
    replace with less."""
    return bool(event) and all(event[k] is not None for k in ("pay", "amount", "currency"))


def _order(notice: dict):
    """Notices in the order SIX numbers them, so a later notice of an ex
    day replaces an earlier one."""
    nid = str(notice.get("id"))
    return (0, int(nid), "") if nid.isdigit() else (1, 0, nid)


def by_ex_day(notices) -> dict:
    """ex day -> the event (notice_event) of the latest notice that states
    it."""
    out = {}
    for n in sorted(notices, key=_order):
        e = notice_event(n)
        if e:
            out[e["ex"]] = e
    return out


def notice_events(notices: list, isins: dict, day: date, *, provider: str) -> list:
    """The dividend_events rows of notices, dated `day`: per ISIN and ex
    day the latest notice that is an event of its own (standalone), for
    every security key its ISIN names, under the id of the dividend row
    it joins."""
    latest = {}
    for n in sorted(notices, key=_order):
        e = notice_event(n)
        if standalone(e):
            latest[(n.get("isin"), e["ex"])] = e
    # The id of the dividend row it joins: its ex day's alone.
    return [event_row(key, provider, day, "dividend", {"ex_date": e["ex"], "pay_date": e["pay"]},
                      event_id=event_id("dividend", e["ex"]), amount=e["amount"], currency=e["currency"],
                      payload={"notice": e["id"]})
            for (isin, _), e in latest.items() for key in isins.get(isin, ())]


def dividend_events(rows: list, notices: list, key: str, currency: str | None, day: date, *, provider: str) -> list:
    """The dividend_events rows of a share's dividend answer, dated `day`:
    one per ex day, its rows merged (base.merge_events): their amounts
    summed where each states one and they state one currency, else the
    first row's. Its currency is the rows', else its notice's, else the
    one it was asked under. The notice of its ex day (by_ex_day) gives
    its pay day, and its amount where the rows state none. A row that
    states no ex day is none. A notice that is an event of its own
    (standalone) and joins no row is one too, as a search states it."""
    parts = []
    for r in rows:
        ex = table.parse_day(r.get(EX_KEY))
        if ex is not None:
            parts.append(event_row(key, provider, day, "dividend", {"ex_date": ex}, amount=dec(r.get(VALUE_KEY)),
                                   currency=str(r.get(CURRENCY_KEY) or "").upper() or None,
                                   payload=present({k: v for k, v in r.items()
                                                    if k not in (EX_KEY, VALUE_KEY, CURRENCY_KEY)})))
    joins, out = by_ex_day(notices), []
    for e in sorted(merge_events(parts), key=lambda e: e["ex_date"]):
        notice = joins.get(e["ex_date"], {})
        out.append({**e, "pay_date": notice.get("pay"),
                    "amount": e["amount"] if e["amount"] is not None else notice.get("amount"),
                    "currency": e["currency"] or notice.get("currency") or currency,
                    "payload": present({**e["payload"], "notice": notice.get("id")})})
    listed = {e["ex_date"] for e in out}
    out += [e for e in notice_events(notices, {n.get("isin"): [key] for n in notices}, day, provider=provider)
            if e["ex_date"] not in listed]
    return out


def kept_notices(ctx) -> dict:
    """ISIN -> notice id -> the notice, as the search requests of earlier
    complete runs in the data dir kept it (NOTICES_DOC), a later run's
    copy of a notice winning. Read once a run. A share's dividend fetch
    carries its ISIN's into its folder."""
    def load():
        out = {}
        for _, notices in earlier_docs(ctx.data_dir, NAME, NOTICES_DOC, "bulk"):
            for n in notices if isinstance(notices, list) else ():
                if isinstance(n, dict) and n.get("isin") and n.get("id") is not None:
                    out.setdefault(n["isin"], {})[str(n["id"])] = n
        return out
    return ctx.once("six_notices", load)
