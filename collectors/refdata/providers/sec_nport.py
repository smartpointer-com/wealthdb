"""
sec-nport: US registered funds and ETFs, from their N-PORT filings on SEC
EDGAR.

- Claim: the fund's US ticker (none for another country's ISIN), looked
  up in the SEC's ticker file for funds (company_tickers_mf.json: CIK,
  series and class per ticker). A ticker missing there (a UIT ETF has no
  series) is looked up in the company ticker file instead, which gives
  the registrant's CIK alone.
- Dates: the period of every NPORT-P filing of the series. The series'
  filing list (EDGAR's browse feed) names the filings; the registrant's
  submissions file gives each filing's report date, its older pages read
  only as far back as a backfill asks. A fund of a UIT takes every
  NPORT-P of its registrant, and is claimed only when the registrant
  files N-PORT at all.
- Fetch: the filing's primary_doc.xml, never the filing's other
  attachments (one can be a hundred megabytes). Compressed in bronze.
- Parse: every holding with its ISIN, CUSIP, ticker, LEI, balance, value,
  weight (pctVal, already in percent), currency, asset category and the
  issuer's country (invCountry, its legal domicile). A holding in another
  currency states the exchange rate it was valued at, in units of its
  currency per US dollar. A debt holding states its maturity, coupon and
  coupon kind, and whether it is in default, its interest payments in
  arrears, or its coupon paid in kind. A currency forward states its two
  legs; a derivative names its counterparty.
- Risk metrics: a fund with a quarter or more of its net assets in debt
  states its rate and credit-spread sensitivities (fund_risk). dv01 and
  dv100 are what its value moves by for a move in rates of one and of
  one hundred basis points, per currency and tenor, signed as filed:
  positive when the fund loses value as rates rise. The credit-spread
  figures, for a one basis point move of investment-grade and of
  non-investment-grade spreads, are the fund's as a whole, under
  currency XXX.

N-PORT carries no sector. Public filings are quarterly and two to five
months old, and quarter ends differ by fund.

Terms: README.md, sec-nport.
"""

from __future__ import annotations

import logging
import xml.etree.ElementTree as ET
from datetime import date
from decimal import Decimal
from pathlib import Path

from collectorkit import bronze, compress
from collectorkit.parse import iso_date

from . import table
from .base import (Parsed, Provider, ProviderError, Unavailable, blank_to_none, dec, present, read_bytes, read_meta,
                   row, us_symbol, write_meta)

FUND_TICKERS = "https://www.sec.gov/files/company_tickers_mf.json"
COMPANY_TICKERS = "https://www.sec.gov/files/company_tickers.json"
BROWSE = "https://www.sec.gov/cgi-bin/browse-edgar"
SUBMISSIONS = "https://data.sec.gov/submissions/CIK{cik:010d}.json"
SUBMISSIONS_PAGE = "https://data.sec.gov/submissions/{name}"
PRIMARY_DOC = "https://www.sec.gov/Archives/edgar/data/{cik}/{acc}/primary_doc.xml"
DOC = "primary_doc.xml"
ATOM = "{http://www.w3.org/2005/Atom}"
NPORT_FORMS = frozenset({"NPORT-P", "NPORT-P/A"})

# fund_risk's tenors, each with the attribute an N-PORT risk element
# states it in.
TENORS = (("3M", "period3Mon"), ("1Y", "period1Yr"), ("5Y", "period5Yr"), ("10Y", "period10Yr"),
          ("30Y", "period30Yr"))
# ISO 4217's "no currency": the credit-spread rows are the fund's as a whole.
NO_CURRENCY = "XXX"


def company_tickers(ctx):
    """The SEC's company ticker file as served, asked for once a run by
    every provider that reads it."""
    return ctx.once("sec_company_tickers", lambda: ctx.client.get(COMPANY_TICKERS).json())


class SecNport(Provider):
    """A fund's filings of one kind on EDGAR. A provider of another kind
    (sec-nmfp) sets the forms and the document and reads the same ticker
    and submissions files, under the same keys of the run's cache, so a
    run that asks both fetches each file once."""
    name = "sec-nport"
    serves_funds = True
    needs_contact = True
    facts = dated_facts = frozenset({"risk_metrics"})
    # The forms a submissions file names the filings by, amendments
    # included, and the types the series' browse feed is asked for, each
    # of which takes in its amendment.
    FORMS = NPORT_FORMS
    FEED_TYPES = ("NPORT-P",)
    # The filing and its document, as an error names them.
    FILING, DOCUMENT = "NPORT-P", "N-PORT"

    # ---- claim -----------------------------------------------------------

    def _fund_tickers(self, ctx) -> dict:
        def load():
            doc = ctx.client.get(FUND_TICKERS).json()
            idx = {f: i for i, f in enumerate(doc.get("fields") or [])}
            out = {}
            for r in doc.get("data") or []:
                sym = str(r[idx["symbol"]] or "").strip().upper()
                if sym:
                    out[sym] = {"cik": int(r[idx["cik"]]), "series": r[idx["seriesId"]] or None,
                                "class": r[idx["classId"]] or None}
            return out
        return ctx.once("sec_fund_tickers", load)

    def _company_tickers(self, ctx) -> dict:
        def load():
            doc = company_tickers(ctx)
            return {str(v.get("ticker", "")).upper(): {"cik": int(v["cik_str"]), "series": None, "class": None}
                    for v in (doc.values() if isinstance(doc, dict) else doc) if v.get("ticker")}
        return ctx.once("nport_co", load)

    def _fund_hit(self, fund, ctx) -> tuple:
        """The fund's US ticker and its entry in the SEC's ticker file for
        funds ({cik, series, class}): "" and None for a fund with no US
        ticker, as EDGAR's fund filings cover US funds only
        (base.us_symbol); the entry None for a ticker the file lacks."""
        symbol = us_symbol(fund)
        return symbol, (self._fund_tickers(ctx).get(symbol) if symbol else None)

    def claim(self, fund, ctx):
        symbol, hit = self._fund_hit(fund, ctx)
        if not symbol:
            return None
        if hit is None:
            # A UIT ETF has no series; its registrant files N-PORT itself.
            # Any other listed company is no fund, and a later provider
            # gets the chance.
            hit = self._company_tickers(ctx).get(symbol)
            if hit is None or not _submission_filings(self._submissions(hit["cik"], ctx), self.FORMS):
                return None
        return {"ref": fund.ref, "symbol": symbol, **hit}

    def _submissions(self, cik, ctx) -> dict:
        return ctx.once(("sec_submissions", cik), lambda: ctx.client.get(SUBMISSIONS.format(cik=cik)).json())

    def _series_accessions(self, series: str, ctx) -> set:
        """The accession numbers of the series' filings, as its browse
        feed lists them, one feed per type."""
        out = set()
        for kind in self.FEED_TYPES:
            out |= ctx.once(("sec_series", series, kind), lambda kind=kind: set(_atom_accessions(ctx.client.get(
                BROWSE, params={"action": "getcompany", "CIK": series, "type": kind, "dateb": "",
                                "owner": "include", "count": "100", "output": "atom"}).body)))
        return out

    # ---- dates -----------------------------------------------------------

    def _filings(self, claim, ctx, since: date | None = None) -> dict:
        """report date -> the latest filing for it: {accession, form,
        filed, period}. The registrant's recent filings, and its older
        pages as far back as `since` asks; of a series, those its browse
        feed lists."""
        sub = self._submissions(claim["cik"], ctx)
        report = _submission_filings(sub, self.FORMS)
        if since is not None:
            for page in ((sub.get("filings") or {}).get("files") or []):
                if (page.get("filingTo") or "") >= since.isoformat():
                    doc = ctx.once(("sec_submissions_page", page["name"]),
                                   lambda name=page["name"]: ctx.client.get(SUBMISSIONS_PAGE.format(name=name)).json())
                    report += _filing_rows(doc, self.FORMS)
        if claim["series"]:
            wanted = self._series_accessions(claim["series"], ctx)
            report = [f for f in report if f["accession"] in wanted]
        out = {}
        for f in sorted(report, key=lambda f: (f["filed"], f["accession"])):
            if f["period"]:
                out[f["period"]] = f
        return out

    def latest(self, claim, ctx):
        filings = self._filings(claim, ctx)
        return max(filings) if filings else None

    def dates(self, claim, since, until, ctx):
        return sorted(d for d in self._filings(claim, ctx, since) if since <= d <= until)

    # ---- fetch -----------------------------------------------------------

    def fetch(self, claim, as_of, outdir: Path, ctx):
        filings = self._filings(claim, ctx, as_of)
        if not filings:
            raise Unavailable(f"no {self.FILING} filing for {claim['symbol']}")
        period = max(filings) if as_of is None else as_of
        f = filings.get(period)
        if f is None:
            raise Unavailable(f"no {self.FILING} filing for {claim['symbol']} for {period}")
        acc = f["accession"].replace("-", "")
        resp = ctx.client.get(PRIMARY_DOC.format(cik=claim["cik"], acc=acc))
        if b"edgarSubmission" not in resp.body[:20000]:
            raise ProviderError(f"{resp.url} is not an {self.DOCUMENT} document")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        compress.compress_best_effort(outdir / DOC, logging.getLogger(f"refdata.{self.name}"))
        write_meta(outdir, provider=self.name, ref=claim["ref"], symbol=claim["symbol"], cik=claim["cik"],
                   series=claim["series"], **{"class": claim["class"]}, accession=f["accession"], form=f["form"],
                   filed=f["filed"].isoformat(), period=period.isoformat())
        return period

    # ---- parse -----------------------------------------------------------

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        return self._parse_document(read_bytes(indir, DOC), meta)

    def _parse_document(self, xml: bytes, meta: dict) -> Parsed:
        """The rows of one filing's document, read with its meta.json."""
        return parse_nport(xml, meta["ref"], provider=self.name)

    def probe(self, ctx) -> str:
        n = len(self._fund_tickers(ctx))
        return f"SEC fund ticker file: {n} tickers"


def _submission_filings(sub: dict, forms) -> list:
    """Every filing of these forms among a registrant's recent filings,
    with its report date."""
    return _filing_rows((sub.get("filings") or {}).get("recent") or {}, forms)


def _filing_rows(cols: dict, forms) -> list:
    """The filings of these forms on one columnar page of a submissions
    file: the recent block, or an older page."""
    out = []
    for i, form in enumerate(cols.get("form") or []):
        if form not in forms:
            continue
        out.append({"accession": cols["accessionNumber"][i], "form": form,
                    "filed": date.fromisoformat(cols["filingDate"][i]),
                    "period": date.fromisoformat(cols["reportDate"][i]) if cols["reportDate"][i] else None})
    return out


def _atom_accessions(body: bytes) -> list:
    """The accession numbers an EDGAR browse feed lists."""
    try:
        root = ET.fromstring(body)
    except ET.ParseError as e:
        raise ProviderError(f"EDGAR filing feed is not XML: {e}") from e
    out = []
    for entry in root.iter(f"{ATOM}entry"):
        for el in entry.iter():
            if local(el.tag) == "accession-number" and el.text:
                out.append(el.text.strip())
    return out


# An SEC filing's XML read by local names, whatever namespace the form's
# schema puts its elements in.

def local(tag: str) -> str:
    """A tag without its namespace."""
    return tag.rsplit("}", 1)[-1]


def child(el, name):
    """The first child with this local name; None where `el` is None or
    has no such child."""
    for c in el if el is not None else ():
        if local(c.tag) == name:
            return c
    return None


def children(el, name) -> list:
    """The children with this local name; none where `el` is None."""
    return [c for c in el if local(c.tag) == name] if el is not None else []


def text(el, name):
    """The text of the first child with this local name, None where it
    is blank or missing."""
    c = child(el, name)
    return blank_to_none(c.text) if c is not None else None


def find(el, name):
    """The first descendant with this local name."""
    for c in el.iter():
        if local(c.tag) == name:
            return c
    return None


def parse_nport(xml: bytes, ref: str, *, provider: str) -> Parsed:
    """One N-PORT primary_doc.xml as silver rows for fund `ref`."""
    try:
        root = ET.fromstring(xml)
    except ET.ParseError as e:
        raise ProviderError(f"N-PORT document is not XML: {e}") from e
    gen = find(root, "genInfo")
    fund_info = find(root, "fundInfo")
    if gen is None:
        raise ProviderError("N-PORT document has no genInfo")
    period = text(gen, "repPdDate") or text(gen, "repPdEnd")
    if not period:
        raise ProviderError("N-PORT document states no report date")
    as_of = date.fromisoformat(period)
    out = Parsed()
    cps: dict = {}
    for n, sec in enumerate((s for s in root.iter() if local(s.tag) == "invstOrSec"), start=1):
        ids = child(sec, "identifiers")
        isin = ticker = None
        other = {}
        if ids is not None:
            for i in ids:
                kind = local(i.tag)
                if kind == "isin":
                    isin = blank_to_none(i.get("value"))
                elif kind == "ticker":
                    ticker = blank_to_none(i.get("value"))
                elif kind == "other":
                    other[i.get("otherDesc") or "other"] = i.get("value")
        # A holding in another currency states it, and the rate it was
        # valued at, in currencyConditional.
        fx = child(sec, "currencyConditional")
        cur = text(sec, "curCd")
        if cur is None and fx is not None:
            cur = blank_to_none(fx.get("curCd"))
        cat = text(sec, "assetCat")
        if cat is None:
            cond = child(sec, "assetConditional")
            cat = blank_to_none(cond.get("assetCat")) if cond is not None else None
        line = row(
            "lines", fund_ref=ref, as_of=as_of, basis="holdings", line_no=n, isin=isin,
            cusip=_cusip(text(sec, "cusip")), ticker=ticker, name=text(sec, "name") or text(sec, "title"),
            asset_type=cat, weight=dec(text(sec, "pctVal")), quantity=dec(text(sec, "balance")),
            market_value=dec(text(sec, "valUSD")), currency=cur, country=_country(text(sec, "invCountry")),
            exchange_rate=dec(fx.get("exchangeRt")) if fx is not None else None,
            payload=present({
                "lei": _lei(text(sec, "lei")), "title": text(sec, "title"), "units": text(sec, "units"),
                "payoff": text(sec, "payoffProfile"), "issuer_cat": text(sec, "issuerCat"),
                "fair_value_level": text(sec, "fairValLevel"), "other_ids": other or None}))
        debt = find(sec, "debtSec")
        if debt is not None:
            maturity = iso_date(text(debt, "maturityDt"))
            line["maturity"] = maturity.isoformat() if maturity else None
            line["coupon"] = dec(text(debt, "annualizedRt"))
            line["coupon_kind"] = text(debt, "couponKind")
            line["is_default"] = _flag(text(debt, "isDefault"))
            line["in_arrears"] = _flag(text(debt, "areIntrstPmntsInArrs"))
            line["paid_in_kind"] = _flag(text(debt, "isPaidKind"))
        deriv = find(sec, "derivativeInfo")
        if deriv is not None:
            fwd = find(deriv, "fwdDeriv")
            if fwd is not None:
                line["long_currency"] = text(fwd, "curPur")
                line["long_amount"] = dec(text(fwd, "amtCurPur"))
                line["short_currency"] = text(fwd, "curSold")
                line["short_amount"] = dec(text(fwd, "amtCurSold"))
            role = "swap" if find(deriv, "swapDeriv") is not None else "derivative"
            for cp in (c for c in deriv.iter() if local(c.tag) == "counterparties"):
                name = text(cp, "counterpartyName")
                if name:
                    entry = cps.setdefault((role, name), {"lei": _lei(text(cp, "counterpartyLei")),
                                                          "share": Decimal(0)})
                    if line["weight"] is not None:
                        entry["share"] += abs(Decimal(line["weight"]))
        out.lines.append(line)
    out.compositions.append(table.composition(
        ref, as_of, out.lines, provider=provider, source_kind="filing",
        fund_name=text(gen, "seriesName") or text(gen, "regName"), currency="USD",
        net_assets=dec(text(fund_info, "netAssets")),
        payload=present({"series": text(gen, "seriesId"), "registrant": text(gen, "regName"),
                         "period_end": text(gen, "repPdEnd")})))
    out.counterparties = [row("counterparties", fund_ref=ref, as_of=as_of, role=role, name=name, lei=v["lei"],
                              share=dec(v["share"]) if v["share"] else None)
                          for (role, name), v in sorted(cps.items())]
    out.fund_risk = _fund_risk(fund_info, ref, as_of)
    return out


def _fund_risk(fund_info, ref: str, as_of: date) -> list:
    """The fund's rate and credit-spread sensitivities as fund_risk rows:
    dv01 and dv100 per currency a curMetric names, and the credit-spread
    figures under NO_CURRENCY. A filing without them has none."""
    if fund_info is None:
        return []
    out = []
    for metric in children(find(fund_info, "curMetrics"), "curMetric"):
        cur = text(metric, "curCd")
        if cur is not None:
            out += _tenors(ref, as_of, cur, dv01=child(metric, "intrstRtRiskdv01"),
                           dv100=child(metric, "intrstRtRiskdv100"))
    out += _tenors(ref, as_of, NO_CURRENCY, credit_ig_dv01=child(fund_info, "creditSprdRiskInvstGrade"),
                   credit_hy_dv01=child(fund_info, "creditSprdRiskNonInvstGrade"))
    return out


def _tenors(ref: str, as_of: date, currency: str, **elements) -> list:
    """One fund_risk row per tenor for one currency, each column from the
    risk element given for it. A tenor none of the elements states has no
    row."""
    out = []
    for tenor, attr in TENORS:
        values = {col: dec(el.get(attr)) if el is not None else None for col, el in elements.items()}
        if any(v is not None for v in values.values()):
            out.append(row("fund_risk", fund_ref=ref, as_of=as_of, currency=currency, tenor=tenor, **values))
    return out


def _flag(v):
    """N-PORT's Y or N as a flag; anything else states nothing."""
    return {"Y": True, "N": False}.get(v)


def _country(v):
    """N-PORT writes XX or N/A where it knows no country."""
    if v is None or v.upper() in ("XX", "N/A", "NA"):
        return None
    return v


def _lei(v):
    """N-PORT writes N/A where a party has no LEI."""
    if v is None or v.upper() in ("N/A", "NA", "NONE"):
        return None
    return v


def _cusip(v):
    """N-PORT writes N/A or zeros where a holding has no CUSIP."""
    if v is None or v.upper() in ("N/A", "NA", "000000000"):
        return None
    return v
