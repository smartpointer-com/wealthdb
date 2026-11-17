"""Tiny PDFs for the loader tests: text placed where a test says.

Courier at a fixed size, each piece of text drawn at its own position,
so pdfplumber reads back the lines and the word boxes a test lays out.
No real document is used.
"""
from __future__ import annotations

PAGE_WIDTH, PAGE_HEIGHT = 595, 842
SIZE = 6


def _escape(text: str) -> str:
    return text.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")


def _stream(words: list[tuple[float, float, str]]) -> bytes:
    """One page's content: each (x, top, text) drawn with its left edge
    at x and its line `top` points below the page's top edge."""
    ops = []
    for x, top, text in words:
        baseline = PAGE_HEIGHT - top - SIZE
        ops.append(f"BT /F1 {SIZE} Tf {x:.2f} {baseline:.2f} Td "
                   f"({_escape(text)}) Tj ET")
    return "\n".join(ops).encode("cp1252")


def pdf_bytes(pages: list[list[tuple[float, float, str]]]) -> bytes:
    """The bytes of a PDF with one page per list of (x, top, text)."""
    n = len(pages)
    font = 3 + 2 * n
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [%s] /Count %d >>" % (
            b" ".join(b"%d 0 R" % (3 + 2 * i) for i in range(n)), n),
    ]
    for i, words in enumerate(pages):
        stream = _stream(words)
        objects.append(
            b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] "
            b"/Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>"
            % (PAGE_WIDTH, PAGE_HEIGHT, 4 + 2 * i, font))
        objects.append(b"<< /Length %d >>\nstream\n" % len(stream) + stream
                       + b"\nendstream")
    objects.append(b"<< /Type /Font /Subtype /Type1 /BaseFont /Courier "
                   b"/Encoding /WinAnsiEncoding >>")
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


def lines_pdf(*pages: list[str]) -> bytes:
    """A PDF whose pages print these lines, one under the other."""
    return pdf_bytes([[(20.0, 20.0 + 10 * i, line)
                       for i, line in enumerate(lines)] for lines in pages])
