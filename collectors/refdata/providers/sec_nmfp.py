"""
sec-nmfp: US money-market funds, from their N-MFP filings on SEC EDGAR.

- Claim: a fund that wants money-market figures (`money_market`), by its
  US ticker (none for another country's ISIN), looked up in the SEC's
  ticker file for funds (company_tickers_mf.json: CIK, series and class
  per ticker), as sec-nport finds a fund. A series that files no N-MFP
  is no money-market fund and is not claimed.
- Dates: the report date of every N-MFP2 and N-MFP3 filing of the
  series, an amendment winning over the filing it amends. The series'
  filing lists (EDGAR's browse feed, one per form) name the filings; the
  registrant's submissions file gives each filing's report date, its
  older pages read only as far back as a backfill asks.
- Fetch: the filing's primary_doc.xml, never the filing's other
  attachments. Compressed in bronze.
- Parse: the series' weighted average maturity and weighted average
  life, in days, and its 7-day gross yield; the claimed class's 7-day
  net yield. N-MFP3 states a yield for each business day of the month,
  N-MFP2 one for the report date; each row is dated the day it
  describes. A yield is filed as a fraction (0.0425) and reads as
  percent (4.25). A class the filing does not list gives no net yield.
  The portfolio's securities are not read.

N-MFP2 is the form until mid-2024, N-MFP3 since. A fund files within
five business days of the month's end.

Terms: README.md, sec-nmfp.
"""

from __future__ import annotations

import xml.etree.ElementTree as ET
from datetime import date

from collectorkit.parse import iso_date

from .base import Parsed, ProviderError, blank_to_none, dec, percent, row
from .sec_nport import SecNport, child, children, text


class SecNmfp(SecNport):
    """sec-nport's filing list and fetch, of the N-MFP forms, with a
    claim and a parser of its own."""
    name = "sec-nmfp"
    facts = dated_facts = frozenset({"money_market"})
    serves_compositions = False
    FORMS = frozenset({"N-MFP2", "N-MFP2/A", "N-MFP3", "N-MFP3/A"})
    FEED_TYPES = ("N-MFP3", "N-MFP2")
    FILING = DOCUMENT = "N-MFP"

    def claim(self, fund, ctx):
        if "money_market" not in fund.wants:
            return None
        symbol, hit = self._fund_hit(fund, ctx)
        if hit is None or not hit["series"] or not self._series_accessions(hit["series"], ctx):
            return None
        return {"ref": fund.ref, "symbol": symbol, **hit}

    def _parse_document(self, xml: bytes, meta: dict) -> Parsed:
        return parse_nmfp(xml, meta["ref"], meta.get("class"), provider=self.name)


def _daily(el, name: str, report: date) -> list:
    """A figure as [(day, value)]. N-MFP3 lists it per day, each entry
    with its value and date (sevenDayGrossYieldValue and
    sevenDayGrossYieldDate); N-MFP2 states it once, for the report
    date."""
    out = []
    for entry in children(el, name):
        if len(entry):
            day = iso_date(text(entry, name + "Date"))
            if day is not None:
                out.append((day, text(entry, name + "Value")))
        else:
            out.append((report, blank_to_none(entry.text)))
    return out


def parse_nmfp(xml: bytes, ref: str, class_id: str | None, *, provider: str) -> Parsed:
    """One N-MFP2 or N-MFP3 primary_doc.xml as characteristics rows for
    fund `ref`, the share class `class_id` of the filing's series."""
    try:
        root = ET.fromstring(xml)
    except ET.ParseError as e:
        raise ProviderError(f"N-MFP document is not XML: {e}") from e
    data = child(root, "formData")
    gen, series = child(data, "generalInfo"), child(data, "seriesLevelInfo")
    if gen is None or series is None:
        raise ProviderError("N-MFP document has no generalInfo or seriesLevelInfo")
    report = iso_date(text(gen, "reportDate"))
    if report is None:
        raise ProviderError("N-MFP document states no report date")
    series_id = text(gen, "seriesId")
    figures = {}   # (day, field) -> (value, payload); a day listed twice keeps its last entry

    def put(field, day, value, **payload):
        if value is not None:
            figures[(day, field)] = (value, {"series": series_id, **payload})

    put("weighted_average_maturity", report, dec(text(series, "averagePortfolioMaturity")))
    put("weighted_average_life", report, dec(text(series, "averageLifeMaturity")))
    for day, v in _daily(series, "sevenDayGrossYield", report):
        put("seven_day_gross_yield", day, percent(v))
    cls = next((c for c in children(data, "classLevelInfo") if class_id and text(c, "classesId") == class_id), None)
    for day, v in _daily(cls, "sevenDayNetYield", report):
        put("seven_day_yield", day, percent(v), **{"class": class_id})
    return Parsed(characteristics=[
        row("characteristics", fund_ref=ref, as_of=day, provider=provider, field=field, value=value,
            payload=payload)
        for (day, field), (value, payload) in sorted(figures.items())])
