"""Unit tests for the Donor-Advised Fund (Fidelity Charitable) phase.

All fixtures are synthesised from scratch — no real Fidelity charitable
account numbers, fund names, charities, or amounts (root AGENTS.md §4).
The tests drive the DAF helpers with a fake Playwright page whose
``request`` context serves fabricated JSON / CSV / PDF bodies, so no
live session or Camoufox import is needed (mirrors test_download_dryrun).
"""

from __future__ import annotations

import json
import sys
from datetime import date
from pathlib import Path
from urllib.parse import urlparse, parse_qs

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402

# Synthetic ids — 7-digit DAF account, obviously fake.
DAF_ACCT = "9990001"
PARTY_ID = "8880002"


# ---------------------------------------------------------------------------
# Fake Playwright request/response plumbing
# ---------------------------------------------------------------------------

class FakeResponse:
    def __init__(self, status, headers, body_bytes):
        self.status = status
        self.headers = headers
        self._body = body_bytes

    @property
    def ok(self):
        return 200 <= self.status < 300

    def body(self):
        return self._body

    def json(self):
        return json.loads(self._body)


class FakeRequestCtx:
    """Routes GET/POST by path to fabricated bodies. ``routes`` maps a
    path substring to a callable(query_dict) -> (status, headers, body).
    """

    def __init__(self, routes):
        self.routes = routes
        self.calls = []

    def _route(self, url):
        u = urlparse(url)
        for frag, fn in self.routes.items():
            if frag in u.path:
                return fn(parse_qs(u.query))
        return (404, {}, b"")

    def get(self, url, headers=None, timeout=None):
        self.calls.append(("GET", url))
        status, hdrs, body = self._route(url)
        return FakeResponse(status, hdrs, body)

    def post(self, url, data=None, headers=None, timeout=None):
        self.calls.append(("POST", url))
        status, hdrs, body = self._route(url)
        return FakeResponse(status, hdrs, body)


class FakePage:
    def __init__(self, request_ctx, url=None):
        self.request = request_ctx
        self._url = url or download.DAF_SPA_URL

    def goto(self, url, wait_until=None, timeout=None):
        self._url = url

    def evaluate(self, js, *args):
        if "location.href" in js:
            return self._url
        return None

    @property
    def url(self):
        return self._url

    def content(self):
        return "<html></html>"

    def screenshot(self, **k):
        pass

    def wait_for_selector(self, *a, **k):
        pass


# ---------------------------------------------------------------------------
# Pure helpers
# ---------------------------------------------------------------------------

def test_login_has_daf_charitable_label():
    dims = [
        {"account_id": "100000001", "portfolio": "Education"},
        {"account_id": DAF_ACCT, "portfolio": "Fidelity Charitable® Giving"},
    ]
    assert download._login_has_daf(dims) is True


def test_login_has_daf_short_id_fallback():
    # No charitable label, but a non-9-digit id is the DAF tell.
    dims = [{"account_id": DAF_ACCT, "portfolio": None}]
    assert download._login_has_daf(dims) is True


def test_login_has_daf_retail_only_false():
    dims = [
        {"account_id": "100000001", "portfolio": "Education"},
        {"account_id": "200000002", "portfolio": "Brokerage"},
    ]
    assert download._login_has_daf(dims) is False


def test_parse_establish_date_formats():
    assert download._daf_parse_establish_date(
        {"establishDate": "2014-03-07"}) == date(2014, 3, 7)
    assert download._daf_parse_establish_date(
        {"establishDate": "03/07/2014"}) == date(2014, 3, 7)
    assert download._daf_parse_establish_date(
        {"establishDate": "2014-03-07T00:00:00"}) == date(2014, 3, 7)
    assert download._daf_parse_establish_date({}) is None
    assert download._daf_parse_establish_date(
        {"establishDate": "garbage"}) is None


def test_year_span_since_inception_uses_establish():
    span = download._daf_year_span(None, date(2014, 3, 7))
    assert span[0] == 2014
    assert span[-1] >= 2026  # up to the current year


def test_year_span_windowed_clamps_to_establish():
    # A since older than establish should not probe pre-establish years.
    span = download._daf_year_span(date(2010, 1, 1), date(2014, 3, 7))
    assert span[0] == 2014


def test_doc_stem_is_pii_free():
    row = {"documentType": "STATEMENT", "correspondenceDate": "2026-06-30",
           "id": DAF_ACCT}
    stem = download._daf_doc_stem(row, "STATEMENT")
    assert "STATEMENT" in stem and "2026" in stem
    assert DAF_ACCT not in stem  # the account/document id never in the name


# ---------------------------------------------------------------------------
# Pagination
# ---------------------------------------------------------------------------

def test_fetch_paged_follows_pages_and_stops_at_total():
    # 3 items total across 2 pages of size 100 (synthetic: page 1 has 100,
    # page 2 has the remaining — but we fake small counts and rely on the
    # total to stop).
    def grants_route(q):
        page_no = int(q.get("page", ["1"])[0])
        if page_no == 1:
            items = [{"grantId": f"g{i}"} for i in range(2)]
        elif page_no == 2:
            items = [{"grantId": "g2"}]
        else:
            items = []
        body = json.dumps({
            "totalItems": 3, "itemsPerPage": 100,
            "currentItemCount": len(items),
            "startIndex": (page_no - 1) * 100 + 1, "items": items,
        }).encode()
        return (200, {}, body)

    page = FakePage(FakeRequestCtx({"/transactionHistory/grants": grants_route}))
    items = download._daf_fetch_paged(
        page, f"{download.DAF_API}/transactionHistory/grants?accountId=x", None)
    assert len(items) == 3
    # exactly two page fetches (stopped once total reached)
    assert len(page.request.calls) == 2


def test_fetch_paged_stops_on_empty_page():
    def route(q):
        return (200, {}, json.dumps(
            {"totalItems": 999, "items": []}).encode())

    page = FakePage(FakeRequestCtx({"/transactionHistory/contributions": route}))
    items = download._daf_fetch_paged(
        page,
        f"{download.DAF_API}/transactionHistory/contributions?accountId=x",
        None)
    assert items == []
    assert len(page.request.calls) == 1


# ---------------------------------------------------------------------------
# Documents: PDF guard + content-hash dedup
# ---------------------------------------------------------------------------

_PDF_A = b"%PDF-1.4\nAAAA\n%%EOF\n"


def test_scrape_documents_dedups_and_guards(tmp_path):
    # Listing: two STATEMENT rows (distinct keys) + one GRANT row.
    # Downloads: row1 -> PDF_A, row2 -> PDF_A (duplicate bytes -> deduped),
    # grant row -> a non-PDF body (must be rejected by the %PDF guard).
    listing = {
        "STATEMENT": [
            {"legacyDocumentKey": "k1", "documentType": "STATEMENT",
             "documentName": "Q1 Statement (pdf)",
             "correspondenceDate": "2026-03-31"},
            {"legacyDocumentKey": "k2", "documentType": "STATEMENT",
             "documentName": "Q2 Statement (pdf)",
             "correspondenceDate": "2026-06-30"},
        ],
        "GRANT": [
            {"legacyDocumentKey": "k3", "documentType": "GRANT",
             "documentName": "Grant confirm (pdf)",
             "correspondenceDate": "2026-05-01"},
        ],
        "CONTRIBUTION": [],
        "FORM_8283": [],
    }
    dl_bodies = {"k1": _PDF_A, "k2": _PDF_A, "k3": b"<html>not a pdf</html>"}

    def doc_list_route(q):
        dtype = q.get("documentType", [""])[0]
        return (200, {}, json.dumps(listing.get(dtype, [])).encode())

    def doc_download_route(q):
        key = q.get("legacyDocumentKey", [""])[0]
        return (200, {"content-type": "application/pdf"},
                dl_bodies.get(key, b""))

    # Order matters: /document/download is the more specific path.
    routes = {
        "/document/download": doc_download_route,
        "/document": doc_list_route,
    }
    page = FakePage(FakeRequestCtx(routes))
    acct_dir = tmp_path / "acct"
    res = download._daf_scrape_documents(
        page, "jwt", DAF_ACCT, date(2019, 1, 1), acct_dir)

    assert res["listed"] == 3          # all three rows indexed
    assert res["saved_pdfs"] == 1      # PDF_A once (k1==k2 bytes deduped)
    assert res["errors"] == ["Grant confirm (pdf)"]  # non-PDF rejected
    pdfs = list((acct_dir / "documents").glob("*.pdf"))
    assert len(pdfs) == 1
    # index written at the account dir
    index = json.loads((acct_dir / "documents_index.json").read_text())
    assert len(index) == 3


def test_scrape_documents_dry_run_lists_only(tmp_path):
    # acct_dir=None → dry-run: listing counted, nothing downloaded/written.
    listing = {"STATEMENT": [
        {"legacyDocumentKey": "k1", "documentType": "STATEMENT",
         "documentName": "Q1 Statement (pdf)",
         "correspondenceDate": "2026-03-31"}],
        "GRANT": [], "CONTRIBUTION": [], "FORM_8283": []}

    def doc_list_route(q):
        return (200, {}, json.dumps(
            listing.get(q.get("documentType", [""])[0], [])).encode())

    page = FakePage(FakeRequestCtx({"/document": doc_list_route}))
    res = download._daf_scrape_documents(
        page, "jwt", DAF_ACCT, date(2019, 1, 1), None)

    assert res["listed"] == 1
    assert res["saved_pdfs"] == 0
    assert list(tmp_path.rglob("*")) == []      # nothing written
    # never hit the download endpoint
    assert not any("/document/download" in url for _m, url in page.request.calls)


# ---------------------------------------------------------------------------
# scrape_daf: no-DAF short-circuits
# ---------------------------------------------------------------------------

def _full_daf_routes():
    """Synthetic route table covering the whole DAF read surface, so a
    dry-run enumeration resolves every endpoint it touches."""
    roster = [{"accountNbr": DAF_ACCT, "gaName": "Test Family DAF"}]
    master = {"accountNbr": DAF_ACCT, "gaBalance": 1000.0,
              "establishDate": "2014-03-07"}
    pools = [{"poolPriceDate": "2026-09-01", "totalMarketValue": 1000.0,
              "poolInfoList": [{"poolId": "P1", "poolName": "Pool One",
                                "marketValue": 1000.0}]}]
    empty_env = {"totalItems": 0, "items": []}

    def paged(_q):
        return (200, {}, json.dumps(empty_env).encode())

    return {
        "/identity/self": lambda q: (
            200, {"fid-cgf-auth-jwt": "jwt-token"},
            json.dumps({"loginKeyId": 1, "partyId": PARTY_ID}).encode()),
        "/user/": lambda q: (200, {}, json.dumps(roster).encode()),
        "/givingAccounts/": lambda q: (200, {}, json.dumps(master).encode()),
        "/poolBalances": lambda q: (200, {}, json.dumps(pools).encode()),
        "/transactionHistory/grants": paged,
        "/transactionHistory/contributions": paged,
        "/transactionHistory/adjustment": paged,
        "/gift": paged,
        "/poolExchange": lambda q: (200, {}, json.dumps([]).encode()),
        "/document": lambda q: (200, {}, json.dumps([]).encode()),
    }


def test_scrape_daf_dry_run_reads_but_writes_nothing(tmp_path):
    page = FakePage(FakeRequestCtx(_full_daf_routes()))

    # Land the SSO hop on the DAF host.
    def _hop(url, wait_until=None, timeout=None):
        page._url = download.DAF_SPA_URL
    page.goto = _hop

    res = download.scrape_daf(page, tmp_path, None, None, dry_run=True)

    assert res["status"] == "dry-run"
    assert res["accounts"] == 1
    acct = next(iter(res["per_account"].values()))
    assert acct["status"] == "dry-run"
    assert acct["pools"] == 1
    # Nothing written under bronze.
    assert list(tmp_path.rglob("*")) == []
    # No export/download endpoint was ever hit (export-nothing contract).
    assert not any("/download" in url for _m, url in page.request.calls)


def test_scrape_daf_no_daf_when_sso_bounces(tmp_path):
    # SSO nav lands back on the signin page -> _daf_navigate_sso False ->
    # scrape_daf returns no-daf and writes nothing.
    page = FakePage(FakeRequestCtx({}),
                    url="https://digital.fidelity.com/prgw/digital/signin/")

    # goto keeps us on a non-DAF host; navigate_sso should give up.
    def _stay(url, wait_until=None, timeout=None):
        page._url = "https://digital.fidelity.com/prgw/digital/signin/"
    page.goto = _stay

    res = download.scrape_daf(page, tmp_path, None, None)
    assert res["status"] == "no-daf"
    assert not (tmp_path / "daf").exists()
