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
  issuer's country (invCountry, its legal domicile). A currency forward
  states its two legs; a derivative names its counterparty.

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
from .base import (Parsed, Provider, ProviderError, Unavailable, blank_to_none, dec, read_meta, row, us_symbol,
                   write_meta)

FUND_TICKERS = "https://www.sec.gov/files/company_tickers_mf.json"
COMPANY_TICKERS = "https://www.sec.gov/files/company_tickers.json"
BROWSE = "https://www.sec.gov/cgi-bin/browse-edgar"
SUBMISSIONS = "https://data.sec.gov/submissions/CIK{cik:010d}.json"
SUBMISSIONS_PAGE = "https://data.sec.gov/submissions/{name}"
PRIMARY_DOC = "https://www.sec.gov/Archives/edgar/data/{cik}/{acc}/primary_doc.xml"
DOC = "primary_doc.xml"
ATOM = "{http://www.w3.org/2005/Atom}"

log = logging.getLogger("refdata.sec-nport")


def company_tickers(ctx):
    """The SEC's company ticker file as served, asked for once a run by
    every provider that reads it."""
    return ctx.once("sec_company_tickers", lambda: ctx.client.get(COMPANY_TICKERS).json())


class SecNport(Provider):
    name = "sec-nport"
    serves_funds = True
    needs_contact = True

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
        return ctx.once("nport_mf", load)

    def _company_tickers(self, ctx) -> dict:
        def load():
            doc = company_tickers(ctx)
            return {str(v.get("ticker", "")).upper(): {"cik": int(v["cik_str"]), "series": None, "class": None}
                    for v in (doc.values() if isinstance(doc, dict) else doc) if v.get("ticker")}
        return ctx.once("nport_co", load)

    def claim(self, fund, ctx):
        # N-PORT covers US funds only (base.us_symbol).
        symbol = us_symbol(fund)
        if not symbol:
            return None
        hit = self._fund_tickers(ctx).get(symbol)
        if hit is None:
            # A UIT ETF has no series; its registrant files N-PORT itself.
            # Any other listed company is no fund, and a later provider
            # gets the chance.
            hit = self._company_tickers(ctx).get(symbol)
            if hit is None or not _submission_filings(self._submissions(hit["cik"], ctx)):
                return None
        return {"ref": fund.ref, "symbol": symbol, **hit}

    def _submissions(self, cik, ctx) -> dict:
        return ctx.once(("nport_sub", cik), lambda: ctx.client.get(SUBMISSIONS.format(cik=cik)).json())

    # ---- dates -----------------------------------------------------------

    def _filings(self, claim, ctx, since: date | None = None) -> dict:
        """report date -> the latest filing for it: {accession, filed}.
        The registrant's recent filings, and its older pages as far back
        as `since` asks."""
        sub = self._submissions(claim["cik"], ctx)
        report = list(_submission_filings(sub))
        if since is not None:
            for page in ((sub.get("filings") or {}).get("files") or []):
                if (page.get("filingTo") or "") >= since.isoformat():
                    doc = ctx.once(("nport_page", page["name"]),
                                   lambda name=page["name"]: ctx.client.get(SUBMISSIONS_PAGE.format(name=name)).json())
                    report += _filing_rows(doc)
        if claim["series"]:
            wanted = ctx.once(("nport_series", claim["series"]), lambda: set(_atom_accessions(ctx.client.get(
                BROWSE, params={"action": "getcompany", "CIK": claim["series"], "type": "NPORT-P", "dateb": "",
                                "owner": "include", "count": "100", "output": "atom"}).body)))
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
            raise Unavailable(f"no NPORT-P filing for {claim['symbol']}")
        period = max(filings) if as_of is None else as_of
        f = filings.get(period)
        if f is None:
            raise Unavailable(f"no NPORT-P filing for {claim['symbol']} for {period}")
        acc = f["accession"].replace("-", "")
        resp = ctx.client.get(PRIMARY_DOC.format(cik=claim["cik"], acc=acc))
        if b"edgarSubmission" not in resp.body[:20000]:
            raise ProviderError(f"{resp.url} is not an N-PORT document")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        compress.compress_best_effort(outdir / DOC, log)
        write_meta(outdir, provider=self.name, ref=claim["ref"], symbol=claim["symbol"], cik=claim["cik"],
                   series=claim["series"], accession=f["accession"], filed=f["filed"].isoformat(),
                   period=period.isoformat())
        return period

    # ---- parse -----------------------------------------------------------

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        path = compress.resolve_variant(indir / DOC)
        if path is None:
            raise ProviderError(f"{indir}: no {DOC}")
        with compress.open_bytes(path) as fh:
            return parse_nport(fh.read(), meta["ref"], provider=self.name)

    def probe(self, ctx) -> str:
        n = len(self._fund_tickers(ctx))
        return f"SEC fund ticker file: {n} tickers"


def _submission_filings(sub: dict) -> list:
    """Every NPORT-P filing among a registrant's recent filings, with its
    report date."""
    return _filing_rows((sub.get("filings") or {}).get("recent") or {})


def _filing_rows(cols: dict) -> list:
    """The NPORT-P filings of one columnar page of a submissions file:
    the recent block, or an older page."""
    out = []
    for i, form in enumerate(cols.get("form") or []):
        if form not in ("NPORT-P", "NPORT-P/A"):
            continue
        out.append({"accession": cols["accessionNumber"][i],
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
            if _local(el.tag) == "accession-number" and el.text:
                out.append(el.text.strip())
    return out


def _local(tag: str) -> str:
    return tag.rsplit("}", 1)[-1]


def _child(el, name):
    for c in el:
        if _local(c.tag) == name:
            return c
    return None


def _text(el, name):
    c = _child(el, name) if el is not None else None
    return blank_to_none(c.text) if c is not None else None


def _find(el, name):
    """The first descendant with this local name."""
    for c in el.iter():
        if _local(c.tag) == name:
            return c
    return None


def parse_nport(xml: bytes, ref: str, *, provider: str) -> Parsed:
    """One N-PORT primary_doc.xml as silver rows for fund `ref`."""
    try:
        root = ET.fromstring(xml)
    except ET.ParseError as e:
        raise ProviderError(f"N-PORT document is not XML: {e}") from e
    gen = _find(root, "genInfo")
    fund_info = _find(root, "fundInfo")
    if gen is None:
        raise ProviderError("N-PORT document has no genInfo")
    period = _text(gen, "repPdDate") or _text(gen, "repPdEnd")
    if not period:
        raise ProviderError("N-PORT document states no report date")
    as_of = date.fromisoformat(period)
    out = Parsed()
    cps: dict = {}
    for n, sec in enumerate((s for s in root.iter() if _local(s.tag) == "invstOrSec"), start=1):
        ids = _child(sec, "identifiers")
        isin = ticker = None
        other = {}
        if ids is not None:
            for i in ids:
                kind = _local(i.tag)
                if kind == "isin":
                    isin = blank_to_none(i.get("value"))
                elif kind == "ticker":
                    ticker = blank_to_none(i.get("value"))
                elif kind == "other":
                    other[i.get("otherDesc") or "other"] = i.get("value")
        cur = _text(sec, "curCd")
        if cur is None:
            cond = _child(sec, "currencyConditional")
            cur = blank_to_none(cond.get("curCd")) if cond is not None else None
        cat = _text(sec, "assetCat")
        if cat is None:
            cond = _child(sec, "assetConditional")
            cat = blank_to_none(cond.get("assetCat")) if cond is not None else None
        line = row(
            "lines", fund_ref=ref, as_of=as_of, basis="holdings", line_no=n, isin=isin,
            cusip=_cusip(_text(sec, "cusip")), ticker=ticker, name=_text(sec, "name") or _text(sec, "title"),
            asset_type=cat, weight=dec(_text(sec, "pctVal")), quantity=dec(_text(sec, "balance")),
            market_value=dec(_text(sec, "valUSD")), currency=cur, country=_country(_text(sec, "invCountry")),
            payload={k: v for k, v in {
                "lei": _lei(_text(sec, "lei")), "title": _text(sec, "title"), "units": _text(sec, "units"),
                "payoff": _text(sec, "payoffProfile"), "issuer_cat": _text(sec, "issuerCat"),
                "fair_value_level": _text(sec, "fairValLevel"), "other_ids": other or None}.items() if v})
        debt = _find(sec, "debtSec")
        if debt is not None:
            maturity = iso_date(_text(debt, "maturityDt"))
            line["maturity"] = maturity.isoformat() if maturity else None
            line["coupon"] = dec(_text(debt, "annualizedRt"))
        deriv = _find(sec, "derivativeInfo")
        if deriv is not None:
            fwd = _find(deriv, "fwdDeriv")
            if fwd is not None:
                line["long_currency"] = _text(fwd, "curPur")
                line["long_amount"] = dec(_text(fwd, "amtCurPur"))
                line["short_currency"] = _text(fwd, "curSold")
                line["short_amount"] = dec(_text(fwd, "amtCurSold"))
            role = "swap" if _find(deriv, "swapDeriv") is not None else "derivative"
            for cp in (c for c in deriv.iter() if _local(c.tag) == "counterparties"):
                name = _text(cp, "counterpartyName")
                if name:
                    entry = cps.setdefault((role, name), {"lei": _lei(_text(cp, "counterpartyLei")),
                                                          "share": Decimal(0)})
                    if line["weight"] is not None:
                        entry["share"] += abs(Decimal(line["weight"]))
        out.lines.append(line)
    out.compositions.append(table.composition(
        ref, as_of, out.lines, provider=provider, source_kind="filing",
        fund_name=_text(gen, "seriesName") or _text(gen, "regName"), currency="USD",
        net_assets=dec(_text(fund_info, "netAssets")),
        payload={k: v for k, v in {"series": _text(gen, "seriesId"), "registrant": _text(gen, "regName"),
                                   "period_end": _text(gen, "repPdEnd")}.items() if v}))
    out.counterparties = [row("counterparties", fund_ref=ref, as_of=as_of, role=role, name=name, lei=v["lei"],
                              share=dec(v["share"]) if v["share"] else None)
                          for (role, name), v in sorted(cps.items())]
    return out


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
