"""
swissfunddata: funds distributed in Switzerland, UBS's among them, from
the annual and semi-annual reports swissfunddata.ch publishes.

- Claim: a fund with a Swiss, Irish or Luxembourg ISIN that the site's
  document list finds alone for that ISIN, with its newest annual and
  semi-annual reports.
  The site shows that list only after a visitor states an investor
  category and agrees to its legal disclaimer: the provider states
  "private investor" and agrees, once a run, in a session the site keeps
  in a cookie. No account, no login.
- Dates: none. The reports are the newest two.
- Fetch and parse: providers/reports.py. A report names its positions,
  with no identifier: gold finds each one's security by its name.

Terms: README.md, swisscanto and swissfunddata.
"""

from __future__ import annotations

import html
import re

import fetcher

from .base import ProviderError, ident
from .reports import ReportProvider

HOST = "https://www.swissfunddata.ch/sfdpub"
ENTRY = f"{HOST}/sfd-entry/private?url=%2Fen%2F"
ROW = re.compile(r'<a href="/sfdpub/en/funds/docs/\d+">\s*(?P<name>[^<]+?)\s*</a>(?P<rest>.*?)</tr>', re.S)
REPORT = re.compile(r'<a href="(?P<href>/sfdpub/docs/[^"]+\.pdf)"[^>]*title="(?P<kind>Annual report|Semi-annual report)'
                    r' as of')
KINDS = {"Annual report": "annual", "Semi-annual report": "semiannual"}


def documents(page: str) -> tuple:
    """The fund a document list names, and its newest annual and
    semi-annual reports as (kind, URL). A list of several funds, or of
    none, names none: a search by ISIN finds one."""
    rows = list(ROW.finditer(page))
    if len(rows) != 1:
        return None, []
    reports = {}
    for r in REPORT.finditer(rows[0].group("rest")):
        reports.setdefault(KINDS[r.group("kind")], "https://www.swissfunddata.ch" + r.group("href"))
    return html.unescape(rows[0].group("name")), sorted(reports.items())


class SwissFundData(ReportProvider):
    name = "swissfunddata"

    def _enter(self, ctx):
        """States the investor category and agrees to the disclaimer, once
        a run. The agreement answers with a redirect to a page the site
        does not have; the session holds the statement all the same."""
        def enter():
            ctx.client.get(ENTRY)
            try:
                ctx.client.post_form(ENTRY, {"_action_set": "I agree"})
            except fetcher.FetchError as e:
                if e.status != 404:
                    raise
            return True
        return ctx.once("swissfunddata_entry", enter)

    def claim(self, fund, ctx):
        isin = ident(fund, "isin")
        if isin[:2] not in ("CH", "IE", "LU"):
            return None
        name, reports = documents(self._documents(isin, ctx))
        if not name or not reports:
            return None
        return {"ref": fund.ref, "isin": isin, "name": name, "reports": reports}

    def _documents(self, text, ctx) -> str:
        """The document list a search finds, once the category is stated."""
        self._enter(ctx)
        resp = ctx.client.get(f"{HOST}/en/funds/documents",
                              params={"text": text, "max": "10", "offset": "0", "sort": "name", "order": "asc"})
        page = resp.body.decode("utf-8", "replace")
        if 'name="_action_set"' in page:
            raise ProviderError("swissfunddata did not take the investor category")
        return page

    def probe(self, ctx) -> str:
        self._documents("", ctx)
        return "swissfunddata took the investor category"
