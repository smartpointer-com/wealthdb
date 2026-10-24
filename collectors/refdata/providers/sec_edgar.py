"""
sec-edgar: what the SEC knows of a US-listed company, from EDGAR.

- Claim: a security whose ticker the SEC's company ticker file lists.
  A security quoted abroad (an ISIN from outside the US, or no ISIN and
  a price in another currency than US dollars) is claimed only where its
  name and the SEC's agree: a ticker abroad may be another company's in
  New York.
- Fetch: the company's submissions file, and its latest reported common
  shares outstanding (the XBRL concept dei:EntityCommonStockSharesOutstanding).
  For a security held directly, also every period of each figure of
  FIGURES in a family its `missing` names, one XBRL concept
  (companyconcept) a file. A company that states none of the us-gaap
  concepts is asked for their ifrs-full twins, as a 20-F filer reports.
  A concept a company does not report is no error. Only 10-K, 10-Q,
  20-F, 40-F and 6-K filings and their amendments count.
- Calendar: a share held directly that wants its dividend calendar, and
  is no depositary receipt, is asked for the dividends its company's
  10-Q and 10-K filings tag in inline XBRL (sec_calendar): their
  record, pay and declared days and their amount per share. A company
  whose filings tag none is asked again a year later.
- Bulk: the securities held through funds take their figures from XBRL
  frames (sec_frames). One request per concept and calendar period
  answers for every company at once: a broad fund holds thousands.
- Parse: the company's name, tickers and exchanges as an identity row;
  its SIC code, its state or country of incorporation and of its
  business address (EDGAR codes, which gold reads through EDGAR's own
  code map) and its shares outstanding as facts; each figure, period by
  period, as a period fact; each dividend its filings tag as a dividend
  event. A frame gives a period fact to every security whose company it
  lists; it states no form.

Terms: README.md, sec-edgar.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import sec_calendar, sec_frames as frames
from .base import (Parsed, Provider, dec, facts_missing, ident, present, read_json, read_meta, row, today_utc,
                   write_meta)
from .sec_nport import SUBMISSIONS, company_tickers

CONCEPT = "https://data.sec.gov/api/xbrl/companyconcept/CIK{cik:010d}/{taxonomy}/{concept}.json"
SHARES_CONCEPT = "EntityCommonStockSharesOutstanding"
GAAP, IFRS = "us-gaap", "ifrs-full"
SUB_DOC = "submissions.json"
SHARES_DOC = "shares.json"
DEPOSITARY_SIC = "8880"
DEPOSITARY_NAME = re.compile(r"/ADR/?\s*$", re.IGNORECASE)
FILLER = frozenset({"THE", "INC", "CORP", "CO", "LTD", "PLC", "SA", "AG", "NV", "SE", "AB", "ASA", "SPA", "LIMITED",
                    "CORPORATION", "COMPANY", "GROUP", "HOLDINGS", "HOLDING", "CLASS", "CL", "COM", "ORD", "SHS"})
# The forms a concept answer's figure is read from, each with its
# amendment (10-K/A).
FORMS = frozenset({"10-K", "10-Q", "20-F", "40-F", "6-K"})


@dataclass(frozen=True)
class Figure:
    """A reported figure: wealthdb's field, the family the wanted list
    names it by, its unit as the API writes it, whether it is a balance
    on a day (an instant) or a sum over a period, and its concepts.
    Where a figure has several concepts of one taxonomy, the first that
    states a period wins."""
    field: str
    family: str
    unit: str
    instant: bool
    gaap: tuple
    ifrs: tuple

    def concepts(self) -> list:
        """(taxonomy, concept), us-gaap's first."""
        return [(GAAP, c) for c in self.gaap] + [(IFRS, c) for c in self.ifrs]


FIGURES = (
    Figure("eps_diluted", "earnings", "USD/shares", False,
           ("EarningsPerShareDiluted", "EarningsPerShareBasicAndDiluted"), ("DilutedEarningsLossPerShare",)),
    Figure("dividends_per_share", "dividends", "USD/shares", False,
           ("CommonStockDividendsPerShareDeclared", "CommonStockDividendsPerShareCashPaid"),
           ("DividendsRecognisedAsDistributionsToOwnersPerShare",)),
    Figure("net_income", "financials", "USD", False, ("NetIncomeLoss",),
           ("ProfitLossAttributableToOwnersOfParent", "ProfitLoss")),
    Figure("revenue", "financials", "USD", False,
           ("Revenues", "RevenueFromContractWithCustomerExcludingAssessedTax"),
           ("Revenue", "RevenueFromContractsWithCustomers")),
    Figure("operating_income", "financials", "USD", False, ("OperatingIncomeLoss",),
           ("ProfitLossFromOperatingActivities",)),
    Figure("interest_expense", "financials", "USD", False, ("InterestExpense", "InterestExpenseNonoperating"),
           ("InterestExpense",)),
    Figure("long_term_debt", "financials", "USD", True, ("LongTermDebt", "LongTermDebtNoncurrent"),
           ("LongtermBorrowings", "NoncurrentPortionOfNoncurrentBorrowings")),
    Figure("cash", "financials", "USD", True, ("CashAndCashEquivalentsAtCarryingValue",),
           ("CashAndCashEquivalents",)),
    Figure("operating_cash_flow", "financials", "USD", False, ("NetCashProvidedByUsedInOperatingActivities",),
           ("CashFlowsFromUsedInOperatingActivities",)),
    Figure("capex", "financials", "USD", False, ("PaymentsToAcquirePropertyPlantAndEquipment",),
           ("PurchaseOfPropertyPlantAndEquipmentClassifiedAsInvestingActivities",)),
)
FAMILIES = tuple(dict.fromkeys(f.family for f in FIGURES))
# The wanted fields a company lookup answers for any security. For one
# held directly it answers the families too; a security held through
# funds takes them from frames.
LOOKUP_ANSWERS = frozenset({"sector", "country"})


def compact(ticker: str) -> str:
    """A ticker without its share-class punctuation: brokers, the SEC
    and OpenFIGI write XYZB, XYZ-B, XYZ.B and XYZ/B for one share."""
    return re.sub(r"[./\s-]", "", str(ticker or "").upper())


def depositary_registrant(sub: dict) -> bool:
    """Whether a submissions file is a depositary receipt's registrant:
    EDGAR files one under SIC 8880 or names it COMPANY/ADR."""
    return str(sub.get("sic") or "") == DEPOSITARY_SIC or bool(DEPOSITARY_NAME.search(sub.get("name") or ""))


def first_word(name) -> str:
    """The first word of a name that tells the company: no legal form,
    no filler, two letters at least."""
    for w in re.split(r"[^A-Z0-9]+", str(name or "").upper().replace("'", "")):
        if len(w) >= 2 and w not in FILLER:
            return w
    return ""


def names_agree(a, b) -> bool:
    """Whether two names are one company's: the first telling word of
    each, the one the start of the other (QUELLMARK, QUELLMARK INC); a
    word of two letters only where both are equal."""
    x, y = first_word(a), first_word(b)
    if not (x and y):
        return False
    if min(len(x), len(y)) < 3:
        return x == y
    return x.startswith(y) or y.startswith(x)


class SecEdgar(Provider):
    name = "sec-edgar"
    serves_securities = True
    serves_bulk = True
    needs_contact = True
    answers = LOOKUP_ANSWERS | frozenset(FAMILIES) | {sec_calendar.CALENDAR}

    def _tickers(self, ctx) -> tuple:
        """ticker -> CIK as the SEC writes it, share-class tickers (XYZ-B)
        by their compact form (XYZB) besides, and CIK -> company name."""
        def load():
            doc = company_tickers(ctx)
            exact, classes, titles = {}, {}, {}
            for v in (doc.values() if isinstance(doc, dict) else doc):
                t = str(v.get("ticker") or "").upper()
                if not t:
                    continue
                cik = int(v["cik_str"])
                exact.setdefault(t, cik)
                titles.setdefault(cik, v.get("title") or "")
                if compact(t) != t:
                    classes.setdefault(compact(t), cik)
            return exact, classes, titles
        return ctx.once("edgar_tickers", load)

    def wants_constituent(self, security) -> bool:
        """A security held through funds is looked up company by company
        for a sector or a country only: its figures come by frame."""
        return not LOOKUP_ANSWERS.isdisjoint(security.missing)

    def claim_security(self, security, ctx):
        """The security's company, and the families of figures its fetch
        asks for: those the `missing` of a security held directly names,
        none for a security held through funds. A share that wants its
        dividend calendar, and is no depositary receipt, has it in the
        claim's `facts` where its company is due (sec_calendar.due), with
        the filings earlier runs read in `read`."""
        symbol = ident(security, "symbol")
        exact, classes, titles = self._tickers(ctx)
        # The ticker as written first; a share class's compact form only
        # matches a share-class ticker, never a plain one it collides with.
        cik = (exact.get(symbol) or classes.get(compact(symbol))) if symbol else None
        if not cik:
            return None
        isin = ident(security, "isin")
        abroad = not isin.startswith("US") if isin else (security.currency or "USD").upper() != "USD"
        if abroad and security.name and not names_agree(security.name, titles.get(cik)):
            return None
        families = [f for f in FAMILIES if f in security.missing] if security.scope == "direct" else []
        claim = {"key": security.key, "symbol": symbol, "cik": cik, "families": families}
        if sec_calendar.CALENDAR in security.wants and not security.receipt:
            seen = sec_calendar.seen(ctx, self.name).get(cik)
            if sec_calendar.due(seen, today_utc()):
                claim.update(facts=[sec_calendar.CALENDAR], read=sorted(seen.read) if seen else [])
        return claim

    def fetch_security(self, claim, outdir: Path, ctx):
        cik, today = claim["cik"], today_utc()
        sub = ctx.client.get(SUBMISSIONS.format(cik=cik))
        doc = sub.json()  # an answer that is not JSON fails the fetch
        bronze.atomic_write_bytes(outdir / SUB_DOC, sub.body)
        shares = frames.xbrl(ctx, CONCEPT.format(cik=cik, taxonomy="dei", concept=SHARES_CONCEPT))
        if shares is not None:
            bronze.atomic_write_bytes(outdir / SHARES_DOC, shares[0].body)
        figures = [f for f in FIGURES if f.family in claim.get("families", ())]
        # A company reports under one taxonomy: the IFRS twins are asked
        # for only where it states none of the us-gaap concepts.
        if figures and not fetch_concepts(ctx, cik, outdir, GAAP, [c for f in figures for c in f.gaap]):
            fetch_concepts(ctx, cik, outdir, IFRS, [c for f in figures for c in f.ifrs])
        dividends = {}
        if sec_calendar.CALENDAR in claim.get("facts", ()):
            got, missing = sec_calendar.fetch(ctx, cik, outdir, doc, today, set(claim.get("read", ())))
            dividends = {"calendar": got, **facts_missing([sec_calendar.CALENDAR] if missing else [])}
        write_meta(outdir, provider=self.name, key=claim["key"], cik=cik, families=claim.get("families", []),
                   as_of=today.isoformat(), **dividends)

    def plan_bulk(self, securities, ctx, today, fetched, max_age) -> list:
        """The frames the securities held through funds want. Each such
        security whose company the ticker file names (claim_security)
        wants the figures of every family its `missing` names. A family's
        frames run from the quarter of the newest period end gold holds
        of the least current such security (its `periods`), or LOOKBACK
        quarters back where one security holds none, and never further
        back, to the last quarter due. Each frame names the keys of its
        family's securities by CIK, and that family in its `families`. A
        frame an earlier run fetched is asked for again only where
        sec_frames.due says so. The newest frames come first, so a run
        cut short by --limit has asked for the latest."""
        last = frames.last_due(today)
        floor = last - frames.LOOKBACK + 1
        starts, keys = {}, {}
        for s in securities:
            wants = [f for f in FAMILIES if f in s.missing] if s.scope == "constituent" else []
            claim = self.claim_security(s, ctx) if wants else None
            if claim is None:
                continue
            for family in wants:
                start = frames.quarter(s.periods[family]) if s.periods.get(family) else floor
                starts[family] = min(starts.get(family, start), start)
                keys.setdefault(family, {}).setdefault(str(claim["cik"]), []).append(s.key)
        planned = []
        for i, fig in enumerate(FIGURES):
            if fig.family not in starts:
                continue
            unit = frames.frame_unit(fig.unit)
            for period, end in frames.periods(max(starts[fig.family], floor), last, instant=fig.instant):
                ref = f"frame:{GAAP}:{fig.gaap[0]}:{unit}:{period}"
                if frames.due(fetched.get(ref), end, today, max_age):
                    planned.append((end, i, ref, {"ref": ref, "field": fig.field, "families": [fig.family],
                                                  "taxonomy": GAAP, "concepts": list(fig.gaap), "unit": unit,
                                                  "frame": period, "keys": keys[fig.family]}))
        # A year still running counts at its last quarter due; at one end,
        # the figures in FIGURES' order, a quarter before its year.
        newest = frames.quarter_end(last)
        planned.sort(key=lambda p: (-min(p[0], newest).toordinal(), p[1], "Q" not in p[3]["frame"]))
        return [(ref, claim) for _, _, ref, claim in planned]

    def fetch_bulk(self, claim, outdir: Path, ctx):
        frames.fetch(claim, outdir, ctx, provider=self.name)

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        if "frame" in meta:
            return frames.parse(indir, meta, provider=self.name)
        sub = read_json(indir, SUB_DOC)
        shares = read_json(indir, SHARES_DOC) if (indir / SHARES_DOC).is_file() else None
        out = parse_company(sub, shares, meta["key"], date.fromisoformat(meta["as_of"]), provider=self.name)
        answers = {(t, c): read_json(indir, frames.answer_file(t, c)) for fig in FIGURES for t, c in fig.concepts()
                   if (indir / frames.answer_file(t, c)).is_file()}
        out.period_facts = [r for fig in FIGURES for r in figure_rows(fig, answers, meta["key"], provider=self.name)]
        out.dividend_events = sec_calendar.parse(indir, meta, provider=self.name)
        return out

    def probe(self, ctx) -> str:
        return f"SEC company ticker file: {len(self._tickers(ctx)[0])} tickers"

    def note(self) -> str | None:
        return sec_calendar.READS.line()


def fetch_concepts(ctx, cik: int, outdir: Path, taxonomy: str, concepts) -> int:
    """Each concept's answer for one company into outdir, a file each;
    the number of them the company reports."""
    n = 0
    for concept in concepts:
        answer = frames.xbrl(ctx, CONCEPT.format(cik=cik, taxonomy=taxonomy, concept=concept))
        if answer is not None:
            bronze.atomic_write_bytes(outdir / frames.answer_file(taxonomy, concept), answer[0].body)
            n += 1
    return n


def figure_rows(fig: Figure, answers: dict, key: str, *, provider: str) -> list:
    """One figure's period_facts rows for security `key`, from its
    concepts' answers ((taxonomy, concept) -> companyconcept answer): one
    row per period. Where several filings state a period, the figure's
    first concept that states it wins, then the filing filed last, then
    the later accession, then the figure's own unit. The fiscal year and
    period are those of the first filing that states the period: a later
    one restates it as a comparative under its own."""
    best, first = {}, {}
    for rank, (taxonomy, concept) in enumerate(fig.concepts()):
        for unit, facts in ((answers.get((taxonomy, concept)) or {}).get("units") or {}).items():
            for f in facts:
                if not f.get("end") or dec(f.get("val")) is None \
                        or str(f.get("form") or "").removesuffix("/A") not in FORMS:
                    continue
                period = (f.get("start") or f["end"], f["end"])
                filing = (f.get("filed") or "", f.get("accn") or "")
                if period not in first or filing < first[period][0]:
                    first[period] = (filing, f)
                order = (-rank, filing, unit == fig.unit)
                if period not in best or order > best[period][0]:
                    best[period] = (order, f, f"{taxonomy}:{concept}", unit)
    rows = []
    for period, (_, f, scheme, unit) in sorted(best.items()):
        fy, fp = first[period][1].get("fy"), first[period][1].get("fp")
        rows.append(row("period_facts", security_key=key, field=fig.field, as_of=date.fromisoformat(period[1]),
                        period_start=date.fromisoformat(period[0]), provider=provider, value=dec(f["val"]),
                        scheme=scheme, unit=unit, fiscal_year=int(fy) if fy else None, fiscal_period=fp or None,
                        form=f.get("form"), filed=date.fromisoformat(f["filed"]) if f.get("filed") else None,
                        payload=present({"accn": f.get("accn"), "frame": f.get("frame")})))
    return rows


def latest_shares(doc: dict | None):
    """The latest reported shares outstanding and the day they stand for;
    where two filings state the same day, the later filing wins. The API
    carries the company-wide value only, so a company that reports its
    share classes one by one states none."""
    if not doc:
        return None, None
    facts = [f for unit in (doc.get("units") or {}).values() for f in unit
             if f.get("end") and f.get("val") is not None]
    if not facts:
        return None, None
    last = max(facts, key=lambda f: (f["end"], f.get("filed") or "", f.get("accn") or ""))
    return last["val"], last["end"]


def parse_company(sub: dict, shares: dict | None, key: str, as_of: date, *, provider: str) -> Parsed:
    """One company's submissions file, and its shares outstanding, as
    silver rows for security `key`."""
    tickers = sub.get("tickers") or []
    exchanges = sub.get("exchanges") or []
    out = Parsed()
    out.securities.append(row(
        "securities", security_key=key, as_of=as_of, provider=provider, lei=sub.get("lei") or None,
        ticker=tickers[0] if tickers else None, exchange=exchanges[0] if exchanges else None, name=sub.get("name"),
        security_type=sub.get("entityType"),
        payload=present({"cik": sub.get("cik"), "tickers": tickers or None, "exchanges": exchanges or None,
                         "sic_description": sub.get("sicDescription")})))

    def fact(field, value, scheme, **payload):
        if value not in (None, ""):
            out.security_facts.append(row("security_facts", security_key=key, field=field, as_of=as_of,
                                          provider=provider, value=str(value), scheme=scheme, payload=payload))

    fact("sic", sub.get("sic"), "sic", description=sub.get("sicDescription"))
    fact("incorporation", sub.get("stateOfIncorporation"), "edgar")
    # A US address states its state; one abroad states its EDGAR country
    # code apart. A depositary receipt's registrant files from its
    # depositary's address, which says nothing of the company.
    business = (sub.get("addresses") or {}).get("business") or {}
    if not depositary_registrant(sub):
        fact("hq_country", business.get("stateOrCountry") or business.get("countryCode"), "edgar")
    value, end = latest_shares(shares)
    fact("shares_outstanding", value, "count", end=end)
    return out
