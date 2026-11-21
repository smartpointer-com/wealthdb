"""Small PDFs for tests: text drawn where a test says, in one Type 1 font.

A loader or parser test needs a PDF that the extractors read as any
other: pdftotext, pdfplumber, pypdf and PDFium. A real document cannot
be used, so a test draws its own. The builder writes the smallest file
they all accept: a catalog, the page tree, one font and, per page, the
page and its content stream.

A page's content is one of two forms:

* :func:`runs`: each ``(x, y, text)`` run drawn on its own, its
  baseline at ``y`` points above the page's bottom edge, in the order
  given;
* :func:`lines`: one text object whose lines run down the page from a
  start point, a fixed leading apart.

:func:`from_top` builds the whole PDF from runs whose ``y`` is measured
down from the page's top edge, the way a test lays out a page.

Text is written in the WinAnsi encoding (cp1252). The bytes depend only
on the arguments, so a test may hash them.

Test support only: imported as ``from collectorkit import testpdf`` by a
collector's tests, never by a loader or a parser.
"""
from __future__ import annotations

from collections.abc import Iterable, Sequence

LETTER = (612, 792)
A4 = (595, 842)


def _string(text: str) -> bytes:
    """`text` as a PDF literal string, in parentheses."""
    escaped = (text.encode("cp1252").replace(b"\\", b"\\\\")
               .replace(b"(", b"\\(").replace(b")", b"\\)"))
    return b"(" + escaped + b")"


def runs(placed: Iterable[tuple[float, float, str]], *,
         size: float) -> bytes:
    """A content stream drawing each ``(x, y, text)`` run at `size`
    points, its baseline at height `y`."""
    return b"".join(b"BT /F1 %g Tf %g %g Td %s Tj ET\n" % (size, x, y,
                                                         _string(t))
                    for x, y, t in placed)


def lines(texts: Iterable[str], *, size: float, leading: float,
          x: float, y: float) -> bytes:
    """A content stream drawing `texts` one under the other at `size`
    points, the first baseline at ``(x, y)``, each next one `leading`
    points lower."""
    body = b"".join(_string(t) + b" Tj T*\n" for t in texts)
    return (b"BT /F1 %g Tf %g TL %g %g Td\n" % (size, leading, x, y)
            + body + b"ET\n")


def from_top(pages: Iterable[Iterable[tuple[float, float, str]]], *,
             size: float, font: str = "Helvetica",
             page: tuple[float, float] = LETTER) -> bytes:
    """A :func:`pdf` with one page of :func:`runs` per entry of `pages`,
    each run's ``y`` measured down from the page's top edge to its
    baseline."""
    return pdf([runs([(x, page[1] - y, t) for x, y, t in placed], size=size)
                for placed in pages], font=font, page=page)


def pdf(contents: Sequence[bytes], *, font: str = "Helvetica",
        page: tuple[float, float] = LETTER) -> bytes:
    """A PDF with one page per content stream of `contents`, each page
    `page` points wide and high, its text in the standard Type 1 `font`
    (Helvetica, Courier, ...)."""
    n = len(contents)
    font_obj = 3 + 2 * n
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [%s] /Count %d >>" % (
            b" ".join(b"%d 0 R" % (3 + 2 * i) for i in range(n)), n),
    ]
    for i, stream in enumerate(contents):
        objects.append(
            b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %g %g] "
            b"/Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>"
            % (page[0], page[1], 4 + 2 * i, font_obj))
        objects.append(b"<< /Length %d >>\nstream\n" % len(stream) + stream
                       + b"\nendstream")
    objects.append(b"<< /Type /Font /Subtype /Type1 /BaseFont /%s "
                   b"/Encoding /WinAnsiEncoding >>" % font.encode("ascii"))
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for number, body in enumerate(objects, 1):
        offsets.append(len(out))
        out += b"%d 0 obj\n" % number + body + b"\nendobj\n"
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objects) + 1)
    out += b"".join(b"%010d 00000 n \n" % offset for offset in offsets)
    out += (b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n"
            % (len(objects) + 1, xref))
    return bytes(out)
