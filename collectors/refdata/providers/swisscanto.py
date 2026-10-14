"""
swisscanto: Swisscanto's funds, from their annual and semi-annual reports.

- Claim: a fund with a Swiss ISIN whose name names Swisscanto, as
  Swisscanto's product API knows it (institutional classes first, then
  retail): its full name and the links to its umbrella's newest annual
  and semi-annual reports, in English where there is one, else German.
- Dates: none. The reports are the newest two.
- Fetch and parse: providers/reports.py. Each report lists every
  position with its ISIN.

Terms: Swisscanto's legal notice prohibits accessing the site's
contents systematically "to compile a collection, a database or a
directory". The reports are the funds' own publications, read for the
funds held, at most once in max_age_days.
"""

from __future__ import annotations

import fetcher

from .base import fund_isin
from .reports import ReportProvider

API = "https://products.swisscanto.com/products/api"
KINDS = {"funddetail.publication.FYRREP": "annual", "funddetail.publication.HYRREP": "semiannual"}


class Swisscanto(ReportProvider):
    name = "swisscanto"

    def _details(self, isin, ctx):
        """The fund's details, as an institutional class, else a retail
        one: each segment knows only its own classes."""
        for segment in ("INSTITUTIONAL", "RETAIL"):
            try:
                doc = ctx.client.get(f"{API}/funds/{isin}",
                                     params={"segment": segment, "country": "CH", "language": "en"},
                                     headers={"Accept": "application/json"}).json()
            except fetcher.FetchError as e:
                if e.status != 404:
                    raise
                continue
            if doc.get("fundDetails"):
                return doc["fundDetails"]
        return None

    def claim(self, fund, ctx):
        isin = fund_isin(fund)
        if not isin.startswith("CH") or "swisscanto" not in (fund.name or "").lower():
            return None
        details = self._details(isin, ctx)
        if not details:
            return None
        reports = {}
        for section in details.get("sections") or []:
            for pub in section.get("publications") or []:
                kind = KINDS.get((pub.get("name") or {}).get("value"))
                files = {str(lang).lower(): f.get("href") for f in pub.get("files") or []
                         for lang in f.get("fileLanguages") or []}
                if kind and kind not in reports and (href := files.get("en") or files.get("de")):
                    reports[kind] = href
        name = (details.get("summary") or {}).get("fullName")
        if not reports or not name:
            return None
        return {"ref": fund.ref, "isin": isin, "name": name, "reports": sorted(reports.items())}

    def probe(self, ctx) -> str:
        resp = ctx.client.get(f"{API}/translations", params={"language": "en", "country": "CH", "segment": "RETAIL"})
        return f"Swisscanto product API answered {resp.status}"
