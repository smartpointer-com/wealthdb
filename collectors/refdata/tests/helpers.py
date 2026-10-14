"""Shared test helpers: an invented wanted list, a fake provider."""

import json
from datetime import date

from providers.base import Parsed, Provider, Unavailable, write_meta


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
    2026; writes one tiny CSV-shaped file per date."""
    name = "fake"
    serves_funds = True

    def __init__(self, known=("FUNDA",), dates_=None, latest_=None):
        self.known, self.fetched = set(known), []
        self.dates_ = dates_ if dates_ is not None else [date(2026, m, 28) for m in range(1, 7)]
        self.latest_ = latest_

    def claim(self, f, ctx):
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
        from providers.base import read_meta
        meta = read_meta(indir)
        day = date.fromisoformat(meta["as_of"])
        name, weight = (indir / "holdings.txt").read_text().split()
        return Parsed(
            compositions=[{"fund_ref": meta["ref"], "as_of": day, "basis": "holdings", "provider": self.name,
                           "source_kind": "daily_file", "line_count": 1, "weight_sum": weight}],
            lines=[{"fund_ref": meta["ref"], "as_of": day, "basis": "holdings", "line_no": 1, "ticker": name,
                    "name": name, "weight": weight}])


def make_xlsx(rows) -> bytes:
    """An invented one-sheet workbook: every cell an inline string, as
    simple as a spreadsheet gets. A row is a list of cell texts."""
    import io
    import zipfile
    from xml.sax.saxutils import escape

    def col(i):
        s = ""
        i += 1
        while i:
            i, r = divmod(i - 1, 26)
            s = chr(65 + r) + s
        return s

    body = "".join(
        f'<row r="{r + 1}">' + "".join(
            f'<c r="{col(c)}{r + 1}" t="inlineStr"><is><t>{escape(str(v))}</t></is></c>'
            for c, v in enumerate(row) if v != "") + "</row>"
        for r, row in enumerate(rows))
    ns = 'xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"'
    rel = 'xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"'
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("xl/workbook.xml", f'<workbook {ns} {rel}><sheets><sheet name="Holdings" sheetId="1" '
                                      f'r:id="rId1"/></sheets></workbook>')
        z.writestr("xl/_rels/workbook.xml.rels",
                   '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
                   '<Relationship Id="rId1" Target="worksheets/sheet1.xml" '
                   'Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"/>'
                   '</Relationships>')
        z.writestr("xl/worksheets/sheet1.xml", f"<worksheet {ns}><sheetData>{body}</sheetData></worksheet>")
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
