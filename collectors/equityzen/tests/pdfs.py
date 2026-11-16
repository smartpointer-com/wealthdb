"""Minimal one-page PDFs for the equityzen loader tests: text placed at
given points, which pdftotext reads as any other PDF. Synthetic data only."""
from __future__ import annotations


def pdf_bytes(pages: list[list[tuple[float, float, str]]]) -> bytes:
    """A minimal PDF in one font, Helvetica at 8 points: per page, (x, y,
    text) triples, y measured down from the top of a letter page to the
    baseline. pdftotext reads it as any other PDF."""
    objs = [b"<</Type/Catalog/Pages 2 0 R>>",
            b"<</Type/Pages/Kids[%s]/Count %d>>" % (
                b" ".join(b"%d 0 R" % (4 + 2 * i) for i in range(len(pages))),
                len(pages)),
            b"<</Type/Font/Subtype/Type1/BaseFont/Helvetica"
            b"/Encoding/WinAnsiEncoding>>"]
    for i, words in enumerate(pages):
        body = b"".join(
            b"BT /F1 8 Tf %g %g Td (%s) Tj ET\n" % (
                x, 792 - y, t.encode("cp1252").replace(b"\\", b"\\\\")
                .replace(b"(", b"\\(").replace(b")", b"\\)"))
            for x, y, t in words)
        objs.append(b"<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]"
                    b"/Resources<</Font<</F1 3 0 R>>>>/Contents %d 0 R>>"
                    % (5 + 2 * i))
        objs.append(b"<</Length %d>>stream\n%sendstream" % (len(body), body))
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for n, obj in enumerate(objs, 1):
        offsets.append(len(out))
        out += b"%d 0 obj\n%s\nendobj\n" % (n, obj)
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
    out += b"".join(b"%010d 00000 n \n" % o for o in offsets)
    out += b"trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n" % (
        len(objs) + 1, xref)
    return bytes(out)


def text_page(text: str) -> list[tuple[float, float, str]]:
    """`text` as one PDF page that `pdftotext -layout` gives back line by
    line: each run of characters between two or more spaces is placed at
    its column."""
    out = []
    for i, line in enumerate(text.splitlines()):
        col = 0
        for part in line.split("  "):
            if part.strip():
                out.append((40 + 4.6 * col, 60 + 12 * i, part.strip()))
            col += len(part) + 2
    return out

