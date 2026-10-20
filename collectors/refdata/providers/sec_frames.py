"""
The SEC's XBRL frames, for sec-edgar: one concept's figure for one
calendar period, for every company that reported it, in one answer.
This is no provider of its own. sec-edgar plans frames for the
securities held through funds, and fetches and parses them here. Its
companyconcept requests share the answer helpers (xbrl, answer_file).

A frame is named by its calendar period: CY2025 a year, CY2025Q2 a
quarter, CY2025Q2I the instant that ends it. The SEC fits each company's
fiscal periods to the calendar periods they lie closest to, and states
one figure per company: the one filed last.

Terms: README.md, sec-edgar.
"""

from __future__ import annotations

import logging
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze, compress

from .base import Parsed, Unavailable, dec, present, read_json, row, today_utc, write_meta

FRAME = "https://data.sec.gov/api/xbrl/frames/{taxonomy}/{concept}/{unit}/{period}.json"

# A quarter's frames are asked for once this many days have passed since
# its end, the deadline of a 10-Q.
DUE_DAYS = 45
# A frame fetched within this many days of its period's end is asked for
# again once it is older than max_age: late filers arrive.
LATE_DAYS = 120
# The quarters asked for where gold holds no period of a family, and the
# furthest back any plan reaches.
LOOKBACK = 8

log = logging.getLogger("refdata.sec-edgar")


def xbrl(ctx, url):
    """An answer of the SEC's XBRL API as (response, its JSON), or None
    where it has none (404): a concept a company does not report, or a
    frame no company fills. An answer that is not JSON fails the
    request."""
    try:
        resp = ctx.client.get(url)
    except fetcher.FetchError as e:
        if e.status != 404:
            raise
        return None
    return resp, resp.json()


def answer_file(taxonomy: str, concept: str) -> str:
    """The file a concept's answer, or its frame, is stored in."""
    return f"{taxonomy}_{concept}.json"


def frame_unit(unit: str) -> str:
    """A unit as a frame's URL writes it: USD/shares as USD-per-shares."""
    return unit.replace("/", "-per-")


def per_shares(unit: str) -> str:
    """A unit as the companyconcept answers write it: a frame's URL and
    answer may write USD/shares as USD-per-shares."""
    return unit.replace("-per-", "/")


def quarter(day: date) -> int:
    """The calendar quarter a day lies in, counted from year 0."""
    return day.year * 4 + (day.month - 1) // 3


def quarter_end(q: int) -> date:
    year, i = divmod(q, 4)
    return date(year, 3 * i + 3, 31 if i in (0, 3) else 30)


def last_due(today: date) -> int:
    """The last quarter that ended DUE_DAYS or more before today."""
    q = quarter(today) - 1
    return q if (today - quarter_end(q)).days >= DUE_DAYS else q - 1


def _quarter_name(q: int) -> str:
    return f"CY{q // 4}Q{q % 4 + 1}"


def periods(start: int, last: int, *, instant: bool) -> list:
    """The frames of the quarters start to last, as (name, end): an
    instant concept's at each quarter's end; a duration concept's for each
    quarter, and for each calendar year those quarters lie in."""
    quarters = range(start, last + 1)
    if instant:
        return [(_quarter_name(q) + "I", quarter_end(q)) for q in quarters]
    years = sorted({q // 4 for q in quarters})
    return [(_quarter_name(q), quarter_end(q)) for q in quarters] + [(f"CY{y}", date(y, 12, 31)) for y in years]


def due(fetched_on: date | None, end: date, today: date, max_age: int) -> bool:
    """Whether a frame is asked for: no run fetched it, or the last one
    did within LATE_DAYS of the period's end and more than max_age days
    ago."""
    return fetched_on is None or ((fetched_on - end).days <= LATE_DAYS and (today - fetched_on).days > max_age)


def fetch(claim: dict, outdir: Path, ctx, *, provider: str) -> None:
    """The claim's frames into outdir, each concept's in a file of its
    own, compressed. meta.json holds the claim with the concepts that
    answered, and in `answered` the claim's keys whose CIK a frame lists.
    A request no frame answers is Unavailable."""
    got, listed = [], set()
    for concept in claim["concepts"]:
        answer = xbrl(ctx, FRAME.format(taxonomy=claim["taxonomy"], concept=concept, unit=claim["unit"],
                                        period=claim["frame"]))
        if answer is None:
            continue
        resp, doc = answer
        listed |= {str(d.get("cik")) for d in doc.get("data") or []
                   if d.get("end") and dec(d.get("val")) is not None}
        path = outdir / answer_file(claim["taxonomy"], concept)
        bronze.atomic_write_bytes(path, resp.body)
        compress.compress_best_effort(path, log)
        got.append(concept)
    if not got:
        raise Unavailable(f"no frame {claim['ref']}")
    answered = [k for cik, keys in claim["keys"].items() if cik in listed for k in keys]
    write_meta(outdir, **{**claim, "concepts": got, "answered": answered}, provider=provider,
               as_of=today_utc().isoformat())


def parse(indir: Path, meta: dict, *, provider: str) -> Parsed:
    """A frame's figures as period_facts rows: one for each security key
    the claim names under a CIK the frame lists. Where a request holds
    several concepts' frames, a company takes its figure from the first
    that lists it. A frame states no fiscal period, form or filing day."""
    out, seen, keys = Parsed(), set(), meta["keys"]
    for concept in meta["concepts"]:
        doc = read_json(indir, answer_file(meta["taxonomy"], concept))
        unit = per_shares(doc.get("uom") or meta["unit"])
        for d in doc.get("data") or []:
            cik, value = str(d.get("cik")), dec(d.get("val"))
            if cik in seen or cik not in keys or not d.get("end") or value is None:
                continue
            seen.add(cik)
            end = date.fromisoformat(d["end"])
            start = date.fromisoformat(d["start"]) if d.get("start") else end
            out.period_facts += [row("period_facts", security_key=key, field=meta["field"], as_of=end,
                                     period_start=start, provider=provider, value=value,
                                     scheme=f"{meta['taxonomy']}:{concept}", unit=unit,
                                     payload=present({"accn": d.get("accn"), "frame": meta["frame"]}))
                                 for key in keys[cik]]
    return out
