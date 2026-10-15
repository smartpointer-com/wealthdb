"""Shared test helpers: an invented wanted list, a fake provider, fake
sources, and invented workbooks and PDFs."""

import email.message
import http
import io
import json
import urllib.request
import urllib.response
import zipfile
from datetime import date
from xml.sax.saxutils import escape

import fetcher
from providers.base import Parsed, Provider, Unavailable, read_meta, write_meta


def wanted_doc(funds=(), securities=(), as_of="2026-07-15"):
    return {"contract": "refdata-wanted", "version": 1, "generated_at": f"{as_of}T00:00:00Z", "as_of": as_of,
            "settings": {"max_gap_days": 400}, "funds": list(funds), "securities": list(securities)}


def fund(ref, symbol=None, held_from="2026-01-02", held_to=None, compositions=(), roles=("held",)):
    return {"ref": ref, "isin": None, "cusip": None, "symbol": symbol, "name": None, "currency": "USD",
            "roles": list(roles), "held_from": held_from, "held_to": held_to, "compositions": list(compositions)}


def write_wanted(path, doc):
    path.write_text(json.dumps(doc), encoding="utf-8")
    return path


class FakeProvider(Provider):
    """Claims every fund whose symbol it knows; serves the month-ends of
    2026; writes one tiny CSV-shaped file per date. `claimed` and
    `fetched` record what it was asked."""
    name = "fake"
    serves_funds = True

    def __init__(self, known=("FUNDA",), dates_=None, latest_=None):
        self.known, self.claimed, self.fetched = set(known), [], []
        self.dates_ = dates_ if dates_ is not None else [date(2026, m, 28) for m in range(1, 7)]
        self.latest_ = latest_

    def claim(self, f, ctx):
        self.claimed.append(f.ref)
        return {"ref": f.ref} if f.symbol in self.known else None

    def dates(self, claim, since, until, ctx):
        return [d for d in self.dates_ if since <= d <= until]

    def latest(self, claim, ctx):
        return self.latest_

    def fetch(self, claim, as_of, outdir, ctx):
        if as_of is not None and as_of not in self.dates_:
            raise Unavailable("no such date")
        day = as_of or date(2026, 7, 14)
        self.fetched.append((claim["ref"], day))
        outdir.mkdir(parents=True, exist_ok=True)
        (outdir / "holdings.txt").write_text("AAA 100\n")
        write_meta(outdir, provider=self.name, ref=claim["ref"], as_of=day.isoformat())
        return day

    def parse(self, indir):
        meta = read_meta(indir)
        day = date.fromisoformat(meta["as_of"])
        name, weight = (indir / "holdings.txt").read_text().split()
        return Parsed(
            compositions=[{"fund_ref": meta["ref"], "as_of": day, "basis": "holdings", "provider": self.name,
                           "source_kind": "daily_file", "line_count": 1, "weight_sum": weight}],
            lines=[{"fund_ref": meta["ref"], "as_of": day, "basis": "holdings", "line_no": 1, "ticker": name,
                    "name": name, "weight": weight}])


class FakeClient:
    """A stand-in for fetcher.Client. Each route maps a URL substring to
    its answer, the first match winning: bytes, a str, an object sent as
    JSON, an int (an HTTP error of that status), or a callable of the
    request's (url, data) that returns one. A URL no route matches is a
    404. `asked` records every request as (method, url, data): the
    params, the JSON payload or the form fields."""

    def __init__(self, routes=None):
        self.routes, self.asked = dict(routes or {}), []

    def get(self, url, params=None, headers=None):
        return self._answer("GET", url, params)

    def post_json(self, url, payload, headers=None):
        return self._answer("POST", url, payload)

    def post_form(self, url, fields, headers=None):
        return self._answer("POST", url, fields)

    def urls(self, method=None) -> list:
        return [u for m, u, _ in self.asked if method in (None, m)]

    def _answer(self, method, url, data):
        self.asked.append((method, url, data))
        answer = next((a for key, a in self.routes.items() if key in url), 404)
        if callable(answer):
            answer = answer(url, data)
        if isinstance(answer, int):
            raise fetcher.FetchError(url, answer, http.HTTPStatus(answer).phrase)
        body = answer if isinstance(answer, bytes) else answer.encode() if isinstance(answer, str) \
            else json.dumps(answer).encode()
        return fetcher.Response(url, 200, body, "")


class FakeHttps(urllib.request.BaseHandler):
    """Stands in for the network beneath a real fetcher.Client. Each route
    maps a URL to (status, body, headers), or to an exception it raises;
    any other URL answers `default`. `seen` records every request the
    client's opener sends."""

    def __init__(self, routes=None, default=(404, b"", {})):
        self.routes, self.default, self.seen = dict(routes or {}), default, []

    def https_open(self, req):
        self.seen.append(req)
        answer = self.routes.get(req.full_url, self.default)
        if isinstance(answer, Exception):
            raise answer
        status, body, headers = answer
        msg = email.message.Message()
        for k, v in headers.items():
            msg[k] = v
        resp = urllib.response.addinfourl(io.BytesIO(body), msg, req.full_url, status)
        resp.msg = http.HTTPStatus(status).phrase
        return resp


def make_xlsx(rows=(), *, sheet=None, shared=None, target="worksheets/sheet1.xml") -> bytes:
    """An invented one-sheet workbook. By default every cell of `rows` is
    an inline string, as simple as a spreadsheet gets; `sheet` gives the
    sheet's rows as XML instead, `shared` its shared strings, and
    `target` where the workbook says the sheet lies."""
    def col(i):
        s = ""
        i += 1
        while i:
            i, r = divmod(i - 1, 26)
            s = chr(65 + r) + s
        return s

    if sheet is None:
        sheet = "".join(
            f'<row r="{r + 1}">' + "".join(
                f'<c r="{col(c)}{r + 1}" t="inlineStr"><is><t>{escape(str(v))}</t></is></c>'
                for c, v in enumerate(cells) if v != "") + "</row>"
            for r, cells in enumerate(rows))
    ns = 'xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"'
    rel = 'xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"'
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("xl/workbook.xml", f'<workbook {ns} {rel}><sheets><sheet name="Holdings" sheetId="1" '
                                      f'r:id="rId1"/></sheets></workbook>')
        z.writestr("xl/_rels/workbook.xml.rels",
                   '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
                   f'<Relationship Id="rId1" Target="{target}" '
                   'Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"/>'
                   '</Relationships>')
        if shared is not None:
            z.writestr("xl/sharedStrings.xml", f"<sst {ns}>" + "".join(f"<si><t>{escape(s)}</t></si>" for s in shared)
                       + "</sst>")
        path = target.lstrip("/")
        z.writestr(path if path.startswith("xl/") else f"xl/{path}",
                   f"<worksheet {ns}><sheetData>{sheet}</sheetData></worksheet>")
    return buf.getvalue()


def pdf(pages) -> bytes:
    """A PDF whose pages hold the given lines of text, in Helvetica: the
    smallest file a text extractor reads as a report does."""
    objs = {1: b"<< /Type /Catalog /Pages 2 0 R >>",
            3: b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"}
    kids = []
    for i, lines in enumerate(pages):
        page, content = 4 + 2 * i, 5 + 2 * i
        text = b"".join(b"(" + ln.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)").encode("cp1252")
                        + b") Tj T* " for ln in lines)
        stream = b"BT /F1 9 Tf 12 TL 36 800 Td " + text + b"ET"
        objs[content] = b"<< /Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream"
        objs[page] = (b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 3 0 R >> >>"
                      b" /Contents %d 0 R >>" % content)
        kids.append(b"%d 0 R" % page)
    objs[2] = b"<< /Type /Pages /Kids [" + b" ".join(kids) + b"] /Count %d >>" % len(kids)
    out, offsets = bytearray(b"%PDF-1.4\n"), {}
    for n in sorted(objs):
        offsets[n] = len(out)
        out += b"%d 0 obj\n" % n + objs[n] + b"\nendobj\n"
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
    out += b"".join(b"%010d 00000 n \n" % offsets[n] for n in sorted(objs))
    out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, xref)
    return bytes(out)
