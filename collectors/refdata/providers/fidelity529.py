"""
fidelity-529: the portfolios of a Fidelity-run state plan, from the
plan's annual report. Each portfolio holds Fidelity funds; the SEC's list
of fund series and classes gives each its ticker, so the look-through
reads the funds' own filings (`sec-nport`). The report's URL and the
prefix a broker writes before the plan's portfolios are configured
(refdata.cfg's `fidelity_529_plans`).

- Claim: a portfolio whose name starts with a configured prefix, as
  brokers write it ("XX … PORT"). No request.
- Dates: none. The report states the holdings at the plan's year end.
- Fetch: the plan's annual report, as served, and the SEC's series and
  class list, of which the rows for the funds the portfolio holds are
  kept.
- Parse: the portfolio's schedule of investments, found by its title:
  every fund with its shares and value, its weight the value's share of
  the portfolio's net assets, its asset type the section it is listed
  under, and its ticker where the SEC's list names one fund series so.

The report is asked for once a run, whatever the number of portfolios
it serves, and its pages are read once.

Terms: README.md, fidelity-529.
"""

from __future__ import annotations

import csv
import io
import json
import re
from datetime import datetime
from pathlib import Path

import fetcher
from collectorkit import bronze

from . import table
from .base import Parsed, Provider, ProviderError, dec, read_json, read_meta, row, today_utc, write_meta
from .reports import fetch_pdf, number, own_title, pdf_pages, report_pages, words

SERIES = ("https://www.sec.gov/files/investment/data/other/investment-company-series-class-information/"
          "investment-company-series-class-{year}.csv")
REPORT, SERIES_DOC = "report.pdf", "series.json"
# A broker's short words for the report's.
ABBREVIATIONS = {"port": "portfolio", "indx": "index", "fid": "fidelity"}
MARK = re.compile(r"^Schedule of Investments\s*$")
AS_OF = re.compile(r"Schedule of Investments\s+([A-Z][a-z]+ \d{1,2}, \d{4})")
SECTION = re.compile(r"^(?P<s>[A-Z][A-Za-z -]*?Funds?) [–—-] [\d.]+%$")
HOLDING = re.compile(r"^(?P<text>.*?)\s*(?:\(Cost \$[\d,]+\)\s*)?(?:\d+\.\d+\s+)?(?P<shares>[\d,]+) (?P<value>[\d,]+)$")
NET_ASSETS = re.compile(r"^NET ASSETS [–—-] [\d.]+% \$? ?(?P<v>[\d,]+)$")
TOTAL = re.compile(r"^TOTAL INVESTMENT IN SECURITIES")
TRAILING = re.compile(r"\$? ?(?P<v>[\d,]+)$")
NOT_A_NAME = re.compile(r"^(?:TOTAL|NET |Shares Value|Yield \(%\)|Showing Percentage|Schedule of Investments|Legend|"
                        r"See accompanying|\([a-z]\) )")


def _expand(name: str) -> str:
    return re.sub(r"\b(\w+)\b", lambda m: ABBREVIATIONS.get(m.group(1).lower(), m.group(1)), name)


def plans(ctx) -> dict:
    """The configured plans: a broker's name prefix, and the report."""
    return dict(getattr(ctx.config, "fidelity_529_plans", ()) or ())


class Fidelity529(Provider):
    name = "fidelity-529"
    serves_funds = True
    needs_contact = True
    needs_setting = "fidelity_529_plans"

    def claim(self, fund, ctx):
        name = fund.name or ""
        known = plans(ctx)
        plan = next((p for p in known if name.upper().startswith(p.upper() + " ")), None)
        if plan is None or "portfolio" not in _expand(name).lower():
            return None
        return {"ref": fund.ref, "plan": plan, "report": known[plan], "name": name}

    def fetch(self, claim, as_of, outdir: Path, ctx):
        url = claim["report"]
        fetch_pdf(ctx, url, outdir / REPORT)
        pages = ctx.once(("report_pages", url), lambda: pdf_pages(outdir / REPORT))
        day, holdings, _ = schedule(pages, claim["name"])
        exact, by_words = ctx.once("sec_series", lambda: _series(ctx))
        rows = {h["name"]: exact.get(_exact(h["name"])) or by_words.get(_key(h["name"]), []) for h in holdings}
        bronze.atomic_write_bytes(outdir / SERIES_DOC, json.dumps(rows).encode("utf-8"))
        write_meta(outdir, provider=self.name, ref=claim["ref"], name=claim["name"], url=url, as_of=day.isoformat())
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        return parse_schedule(report_pages(indir / REPORT), meta["name"], meta["ref"], read_json(indir, SERIES_DOC),
                              provider=self.name)

    def probe(self, ctx) -> str:
        known = plans(ctx)
        if not known:
            raise ProviderError("no plan is configured (fidelity_529_plans)")
        plan = min(known)
        resp = ctx.client.get(known[plan])
        kind = "a PDF" if resp.body.startswith(b"%PDF") else "no PDF"
        return f"the {plan} plan's report answered {resp.status} with {kind}"


def _exact(name: str) -> str:
    return " ".join(name.lower().split())


def _key(name: str) -> str:
    return " ".join(sorted(words(name)))


def _series(ctx) -> tuple:
    """The SEC's fund series by name, and by the name's words where no
    name is equal: each series' ID and the tickers of its classes. This
    year's list, else last year's while this year's is not out."""
    year = today_utc().year
    try:
        resp = ctx.client.get(SERIES.format(year=year))
    except fetcher.FetchError as e:
        if e.status != 404:
            raise
        resp = ctx.client.get(SERIES.format(year=year - 1))
    exact: dict = {}
    by_words: dict = {}
    for r in csv.DictReader(io.StringIO(resp.body.decode("utf-8-sig", "replace"))):
        name, sid = (r.get("Series Name") or "").strip(), (r.get("Series ID") or "").strip()
        if name and sid:
            for index, key in ((exact, _exact(name)), (by_words, _key(name))):
                index.setdefault(key, {}).setdefault(sid, set()).add((r.get("Class Ticker") or "").strip())
    if not exact:
        raise ProviderError(f"{resp.url} lists no fund series")

    def listed(index):
        return {k: [{"series": sid, "tickers": sorted(t for t in ts if t)} for sid, ts in sorted(v.items())]
                for k, v in index.items()}
    return listed(exact), listed(by_words)


def _portfolios(pages: list) -> dict:
    """Each portfolio's schedule text, by its title: a page's text up to
    the title over its "Schedule of Investments" footer, after any footer
    before it on the page; a schedule that runs over pages is joined."""
    out: dict = {}
    for p in pages:
        lines = [ln.strip() for ln in p.splitlines()]
        start = 0
        for i, ln in enumerate(lines):
            if MARK.match(ln) and i and lines[i - 1]:
                part = lines[start:i - 1]
                if any(AS_OF.search(x) for x in part) or lines[i - 1] in out:
                    out.setdefault(lines[i - 1], []).extend(part)
                start = i + 1
    return out


def schedule(pages: list, fund_name: str) -> tuple:
    """The day, the holdings and the net assets of the portfolio whose
    title the fund's name names (its words, a broker's abbreviations
    spelt out). The holdings must add up to the total the schedule
    prints, or the schedule was not read."""
    portfolios = _portfolios(pages)
    title = own_title(list(portfolios), _expand(fund_name))
    if title is None:
        raise ProviderError(f"the report holds no schedule for {fund_name!r}")
    lines = portfolios[title]
    day = datetime.strptime(next(AS_OF.search(x) for x in lines if AS_OF.search(x)).group(1), "%B %d, %Y").date()
    holdings, section, pending, net, total, in_total = [], None, [], None, None, False
    for ln in lines:
        if in_total and (m := TRAILING.search(ln)):
            total, in_total = number(m.group("v"), "."), False
        if m := SECTION.match(ln):
            section, pending = m.group("s"), []
        elif m := NET_ASSETS.match(ln):
            net = number(m.group("v"), ".")
        elif TOTAL.match(ln):
            # The total's figure ends its line, or the next one.
            m = TRAILING.search(ln)
            total, in_total, pending = (number(m.group("v"), ".") if m else None), m is None, []
        elif re.fullmatch(r"\([a-z]\)", ln):
            continue  # a footnote's mark, between a name and its figures
        elif NOT_A_NAME.match(ln) or not ln:
            pending = []
        elif (m := HOLDING.match(ln)) and (pending or m.group("text")):
            name = " ".join([*pending, m.group("text")]).strip()
            holdings.append({"name": name, "section": section, "shares": number(m.group("shares"), "."),
                             "value": number(m.group("value"), ".")})
            pending = []
        else:
            pending.append(ln)
    if not holdings or not net:
        raise ProviderError(f"the schedule of {title!r} lists no holdings or no net assets")
    read = sum(h["value"] for h in holdings)
    if total is None or abs(read - total) > len(holdings):
        raise ProviderError(f"the holdings read for {title!r} add up to {read}, not the {total} it prints")
    return day, holdings, net


def parse_schedule(pages: list, fund_name: str, ref: str, series: dict, *, provider: str) -> Parsed:
    """One portfolio's schedule as silver rows for `ref`, each fund with
    the ticker of the one fund series its name names."""
    day, holdings, net = schedule(pages, fund_name)
    out = Parsed()
    for h in holdings:
        found = series.get(h["name"]) or []
        tickers = found[0]["tickers"] if len(found) == 1 else []
        out.lines.append(row(
            "lines", fund_ref=ref, as_of=day, basis="holdings", line_no=len(out.lines) + 1,
            ticker=tickers[0] if tickers else None, name=h["name"], asset_type=h["section"],
            weight=dec(round(h["value"] * 100 / net, 8)), quantity=dec(h["shares"]), market_value=dec(h["value"]),
            currency="USD", payload={"series": found[0]["series"]} if len(found) == 1 else {}))
    out.compositions.append(table.composition(ref, day, out.lines, provider=provider, source_kind="report",
                                              fund_name=fund_name, currency="USD", net_assets=dec(net)))
    return out
