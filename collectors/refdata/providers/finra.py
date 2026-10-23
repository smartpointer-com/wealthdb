"""
finra: the dividend calendar of the depositary receipts traded over the
counter, from FINRA's OTC Daily List API. The daily list states each
distribution of an OTC security: its ex, record, pay and declaration
days, and for a receipt the gross and net rate per receipt in US
dollars, the depositary's fee and the tax relief.

- Credential: FINRA's terms require one for every request. The client
  id comes from --client-id or FINRA_CLIENT_ID, the secret from
  FINRA_CLIENT_SECRET alone (config.with_finra_credential); the wrapper
  sources both from <secrets-dir>/finra.env. Without both, or with a
  control character in either, the provider plans, fetches and probes
  nothing. With them it takes an OAuth token (client credentials, HTTP
  Basic) from FINRA's token endpoint and sends it as a bearer token with
  each request to api.finra.org, never one without it. A token that is
  not one word of printable ASCII is refused before any header holds
  it, so no error quotes it. The token stays in the run's memory
  (Context.cache, as a Token, whose repr hides it) and is taken again
  after TOKEN_SECONDS; nothing writes it anywhere, and no error or log
  line names it or the secret.
- Plan: one bulk request for the receipts held directly that want their
  calendar and have a ticker, where it is due: the newer of gold's
  calendar day for the receipt and the last day an earlier answer
  covered for it (covered()) is older than max_age_days. The receipts
  with neither day ask for their history from HISTORY_START; the others
  together, from the oldest of their days, that day included, since its
  list may have grown after the earlier run.
- Fetch: per CHUNK tickers, a POST of a JSON filter: only the FIELDS
  read, the tickers (newSymbolCode) and the days (calendarDay); PAGE
  rows a page, the next page by offset. Each page is kept as FINRA sent
  it. meta.json names every key asked as answered, events or none: a
  receipt listed on an exchange is not on the OTC list.
- Budget: FINRA caps a Public credential at 10 GB a month. A tally of
  the bytes api.finra.org's answers brought (fetcher's count), a failed
  answer's too, per calendar month in UTC, lives in the data dir (TALLY,
  0600): each data dir keeps its own. A request counts at REQUEST_MAX,
  the most one may bring, before it goes: one that could pass BUDGET is
  not sent, and the fetch stops.
- Parse: per ticker and event id (dividendMasterID) the daily list's
  latest entry, by its day and time: FINRA revises an estimate
  (commentText APX) to the final rate (FNL) under one id, and states
  the pay day only once it is final. The gross rate (ADRGrossRate) is
  the amount, in US dollars; the net rate, fee and tax relief go to the
  payload. The kind comes from dividendTypeCode (KINDS). A ticker's other
  entries on the list (a name change) state no distribution day and give
  no event.

Terms: README.md, finra.
"""

from __future__ import annotations

import base64
import json
import logging
import re
import time
from datetime import date
from pathlib import Path

import config
from collectorkit import bronze
from collectorkit.parse import iso_date

from .base import (Parsed, Provider, ProviderError, blank_to_none, dec, earlier_metas, event_id, ident, present,
                   read_bytes, read_meta, read_state, row, through_day, today_utc, write_meta)

TOKEN_URL = "https://ews.fip.finra.org/fip/rest/ews/oauth2/access_token?grant_type=client_credentials"
API_HOST = "api.finra.org"
DATA_URL = f"https://{API_HOST}/data/group/otcMarket/name/otcDailyList"
REF = "otc-daily-list"
CAL = "dividend_calendar"
# The first day a receipt's history is asked from.
HISTORY_START = date(2015, 1, 1)
# How long a token is used before a new one is taken.
TOKEN_SECONDS = 30 * 60
TOKEN_KEY = "finra-token"
# Rows a page asks for: half the 5,000 records a request may bring, so a
# page also stays well under the 3 MB a request may bring.
PAGE = 2500
# Tickers a request names.
CHUNK = 100
MB, GB = 10**6, 10**9
REQUEST_MAX = 3 * MB
BUDGET = 8 * GB
TALLY = "finra-usage.json"
# The fields a request asks for: those parse reads.
FIELDS = ("newSymbolCode", "calendarDay", "dailyListDatetime", "dividendMasterID", "dividendTypeCode",
          "commentText", "exDate", "recordDate", "paymentDate", "declarationDate", "ADRGrossRate", "ADRNetRate",
          "ADRFeeAmount", "ADRTaxReliefAmount")
# An event's kind by its dividendTypeCode: a cash dividend (CD), or one
# with a stock dividend beside it (CSD), whose gross rate is the cash.
# Any other code (a stock dividend, a split, a spin-off) is `other`, its
# code kept in the payload.
KINDS = {"CD": "dividend", "CSD": "dividend"}
# An event's status by a word of its commentText, which FINRA writes in
# many forms: "FNL", "(Final)", "Fnl", "APX", "(Approximate)".
STATUSES = {"FNL": "final", "FINAL": "final", "APX": "estimated", "APPROX": "estimated",
            "APPROXIMATE": "estimated"}
# The day FINRA writes for a date it does not know.
UNKNOWN_DAY = date(9999, 9, 9)
# A token a header can carry: one word of printable ASCII.
TOKEN_CHARS = re.compile(r"[\x21-\x7e]+")

log = logging.getLogger("refdata.finra")


class Token:
    """A bearer token and the clock's reading when it was taken. Its repr
    hides the token, so no repr of the run's context shows it."""
    __slots__ = ("value", "taken")

    def __init__(self, value: str, taken: float):
        self.value, self.taken = value, taken

    def __repr__(self) -> str:
        return "Token(<hidden>)"


def credential(ctx) -> tuple[str, str]:
    """The run's FINRA client id and secret; a plain refusal, naming
    neither value, where either is not set or holds a control
    character."""
    cid = getattr(ctx.config, "finra_client_id", "")
    secret = getattr(ctx.config, "finra_client_secret", "")
    values = ((config.FINRA_CLIENT_ID, cid), (config.FINRA_CLIENT_SECRET, secret))
    unset = [name for name, value in values if not value]
    if unset:
        raise ProviderError(f"FINRA's API needs its credential, and {' and '.join(unset)} "
                            f"{'is' if len(unset) == 1 else 'are'} not set: the wrapper sources both from "
                            f"<secrets-dir>/finra.env (--client-id gives the id). Nothing is asked of FINRA.")
    marred = [name for name, value in values if not value.isprintable()]
    if marred:
        raise ProviderError(f"{' and '.join(marred)} {'holds' if len(marred) == 1 else 'hold'} a control "
                            f"character; check <secrets-dir>/finra.env. Nothing is asked of FINRA.")
    return cid, secret


def size(n: int) -> str:
    return f"{n / GB:.2f} GB" if n >= GB else f"{n / MB:.1f} MB"


def read_tally(path: Path) -> dict:
    """The tally of bytes received, YYYY-MM -> bytes; empty where there
    is none yet. One that does not read is an error: the budget is not
    guessed (base.read_state)."""
    return read_state(path, "FINRA's budget", _bytes)


def _bytes(value) -> int:
    """A month's count in the tally: a whole number of bytes."""
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError("no tally of bytes by month")
    return value


class Tally:
    """The month's tally of the bytes api.finra.org's answers brought, kept
    in the data dir's TALLY after every request."""

    def __init__(self, path: Path, month: str):
        self.path, self.month, self.months = path, month, read_tally(path)

    @property
    def used(self) -> int:
        return self.months.get(self.month, 0)

    def admit(self) -> None:
        """Refuses a request that could pass BUDGET, counted at REQUEST_MAX."""
        if self.used + REQUEST_MAX > BUDGET:
            raise ProviderError(f"stopped: FINRA's answers brought {size(self.used)} in {self.month}, and one "
                                f"more request may bring {size(REQUEST_MAX)}, past this month's budget of "
                                f"{size(BUDGET)}. No request is sent before next month.")

    def add(self, n: int) -> None:
        if n > 0:
            self.months[self.month] = self.used + n
            bronze.atomic_write_json(self.path, self.months)


def covered(data_dir: Path | None) -> dict:
    """key -> the last day an earlier answer covered for it: of the data
    dir's complete runs, each finra answer's `through`, for every key its
    `answered` names."""
    out = {}
    for _, meta in earlier_metas(data_dir, Finra.name):
        through = through_day(meta)
        if through is None:
            continue
        for key in meta.get("answered") or ():
            out[key] = max(out.get(key, through), through)
    return out


def answer_rows(body: bytes) -> list:
    """The rows of an answer: a JSON list; none where the body is empty,
    as FINRA answers a filter that matches nothing."""
    if not body.strip():
        return []
    rows = json.loads(body.decode("utf-8-sig"))
    if not isinstance(rows, list):
        raise ProviderError("FINRA's answer is no list of rows")
    return rows


def day(value) -> date | None:
    """A date field's day: YYYY-MM-DD, as a date or the start of a
    timestamp; None for a blank, another text or UNKNOWN_DAY."""
    d = iso_date(str(value or "").strip())
    return None if d == UNKNOWN_DAY else d


def kind_of(code) -> str:
    return KINDS.get(str(code or "").strip().upper(), "other")


def status_of(comment) -> str | None:
    words = re.findall(r"[A-Z]+", str(comment or "").upper())
    return next((STATUSES[w] for w in words if w in STATUSES), None)


def events(rows, keys: dict, as_of: date, *, provider: str) -> list:
    """The dividend_events rows of an answer's rows, for each key of the
    ticker (`keys`: ticker -> keys): one per event, from its latest
    entry, by day, time and order in the answer. An entry is dated by its
    calendarDay, else its time's day, else `as_of`."""
    latest = {}
    for n, r in enumerate(rows):
        if not isinstance(r, dict):
            continue
        symbol = str(r.get("newSymbolCode") or "").strip().upper()
        ex, record, pay = day(r.get("exDate")), day(r.get("recordDate")), day(r.get("paymentDate"))
        if symbol not in keys or not (ex or record or pay):
            continue
        kind = kind_of(r.get("dividendTypeCode"))
        eid = blank_to_none(r.get("dividendMasterID")) or event_id(kind, ex, record, pay)
        stated = day(r.get("calendarDay")) or day(r.get("dailyListDatetime")) or as_of
        order = (stated, str(r.get("dailyListDatetime") or ""), n)
        if (symbol, eid) not in latest or order >= latest[(symbol, eid)][0]:
            latest[(symbol, eid)] = (order, r, kind, ex, record, pay)
    out = []
    for (symbol, eid), ((stated, _, _), r, kind, ex, record, pay) in sorted(latest.items()):
        amount = dec(r.get("ADRGrossRate"))
        payload = present({"symbol": symbol, "dividendTypeCode": blank_to_none(r.get("dividendTypeCode")),
                           "commentText": blank_to_none(r.get("commentText")),
                           "dailyListDatetime": blank_to_none(r.get("dailyListDatetime")),
                           "ADRNetRate": dec(r.get("ADRNetRate")), "ADRFeeAmount": dec(r.get("ADRFeeAmount")),
                           "ADRTaxReliefAmount": dec(r.get("ADRTaxReliefAmount"))})
        out += [row("dividend_events", ref=key, provider=provider, event_id=eid, as_of=stated, kind=kind,
                    ex_date=ex, record_date=record, pay_date=pay, declared_date=day(r.get("declarationDate")),
                    amount=amount, currency="USD" if amount is not None else None,
                    status=status_of(r.get("commentText")), payload=payload)
                for key in keys[symbol]]
    return out


class Finra(Provider):
    name = "finra"
    serves_bulk = True
    answers = frozenset({CAL})
    # The clock a token's age is read on.
    clock = staticmethod(time.monotonic)

    def plan_bulk(self, securities, ctx, today, fetched, max_age) -> list:
        """One request, for every receipt whose calendar is due (the
        module's Plan): `keys` maps each ticker to its keys, `groups`
        the tickers by the day they are asked from, `through` today."""
        receipts = [(s, symbol) for s in securities
                    if s.receipt and CAL in s.wants and (symbol := ident(s, "symbol"))]
        if not receipts:
            return []
        credential(ctx)
        last_covered = covered(ctx.data_dir)
        keys, starts = {}, {}
        for s, symbol in receipts:
            known = max(filter(None, (s.calendar, last_covered.get(s.key))), default=None)
            if known is not None and (today - known).days <= max_age:
                continue
            start = known or HISTORY_START
            keys.setdefault(symbol, []).append(s.key)
            starts[symbol] = min(starts.get(symbol, start), start)
        if not keys:
            return []
        history = sorted(t for t, d in starts.items() if d == HISTORY_START)
        later = sorted(t for t, d in starts.items() if d != HISTORY_START)
        groups = [{"from": HISTORY_START.isoformat(), "symbols": history}] if history else []
        if later:
            groups.append({"from": min(starts[t] for t in later).isoformat(), "symbols": later})
        return [(REF, {"ref": REF, "keys": keys, "facts": [CAL], "groups": groups, "through": today.isoformat()})]

    def _token(self, ctx) -> str:
        """The run's bearer token: the one taken within TOKEN_SECONDS, else
        a new one. One a header cannot carry is refused unquoted."""
        held = ctx.cache.get(TOKEN_KEY)
        if held is not None and self.clock() - held.taken < TOKEN_SECONDS:
            return held.value
        cid, secret = credential(ctx)
        basic = base64.b64encode(f"{cid}:{secret}".encode()).decode("ascii")
        doc = ctx.client.post_form(TOKEN_URL, {}, headers={"Authorization": f"Basic {basic}"}).json()
        token = doc.get("access_token") if isinstance(doc, dict) else None
        if not token:
            raise ProviderError("FINRA's token endpoint answered without an access_token")
        if not isinstance(token, str) or not TOKEN_CHARS.fullmatch(token):
            raise ProviderError("FINRA's token endpoint answered a token that is not one word of printable "
                                "ASCII, which no header can carry; nothing more is asked of FINRA")
        ctx.cache[TOKEN_KEY] = Token(token, self.clock())
        return token

    def _page(self, ctx, tally: Tally, symbols, since: str, through: str, offset: int) -> bytes:
        """One page of the daily list for the tickers and days, counted in
        the tally whether it comes or fails."""
        tally.admit()
        token = self._token(ctx)
        payload = {"limit": PAGE, "offset": offset, "fields": list(FIELDS),
                   "domainFilters": [{"fieldName": "newSymbolCode", "values": list(symbols)}],
                   "dateRangeFilters": [{"fieldName": "calendarDay", "startDate": since, "endDate": through}]}
        before = ctx.client.received(API_HOST)
        try:
            return ctx.client.post_json(DATA_URL, payload, headers={"Authorization": f"Bearer {token}",
                                                                    "Accept": "application/json"}).body
        finally:
            tally.add(ctx.client.received(API_HOST) - before)

    def fetch_bulk(self, claim, outdir: Path, ctx):
        """Every page of the claim's groups into outdir, then meta.json
        with every key asked as answered."""
        credential(ctx)
        if ctx.data_dir is None:
            raise ProviderError("FINRA's budget is kept in the data dir, and the run names none")
        tally = Tally(ctx.data_dir / TALLY, today_utc().strftime("%Y-%m"))
        files = []
        for group in claim["groups"]:
            for i in range(0, len(group["symbols"]), CHUNK):
                offset = 0
                while True:
                    body = self._page(ctx, tally, group["symbols"][i:i + CHUNK], group["from"], claim["through"],
                                      offset)
                    rows = answer_rows(body)
                    files.append(f"page-{len(files) + 1:03d}.json")
                    bronze.atomic_write_bytes(outdir / files[-1], body)
                    if len(rows) < PAGE:
                        break
                    offset += PAGE
        write_meta(outdir, **claim, provider=self.name, as_of=claim["through"], files=files,
                   answered=sorted(k for keys in claim["keys"].values() for k in keys))
        log.info("finra: %d pages; FINRA's answers brought %s in %s", len(files), size(tally.used), tally.month)

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        rows = [r for name in meta["files"] for r in answer_rows(read_bytes(indir, name))]
        return Parsed(dividend_events=events(rows, meta["keys"], date.fromisoformat(meta["as_of"]),
                                             provider=self.name))

    def probe(self, ctx) -> str:
        """A token, and the month's tally; no request for data."""
        self._token(ctx)
        month = today_utc().strftime("%Y-%m")
        if ctx.data_dir is None:
            return "a token taken; no data dir, so no tally"
        used = read_tally(ctx.data_dir / TALLY).get(month, 0)
        return f"a token taken; {month}: FINRA's answers brought {size(used)} of the {size(BUDGET)} budget"
