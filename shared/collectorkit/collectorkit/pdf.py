"""PDF text-extraction helpers shared by collector pdf_parsers.

Two extractors, one per backend the collectors use:

* ``extract_text_pdfium`` — pypdfium2 (PDFium's C++ core). Layout-
  ordered text, one page per block. Used by the collectors whose
  statement layouts parse cleanly as space-delimited lines.
* ``extract_text_pdfplumber`` — pdfplumber. Per-page text joined
  with newlines, for the collectors whose parsers split on a
  per-page account header.

``pypdfium2`` and ``pdfplumber`` are **per-collector** dependencies,
not installed in every collector's venv. collectorkit is imported by
all of them, so the backend imports live inside the function bodies:
importing this module never requires either dependency, and only the
collector that calls a given extractor needs its backend installed.
"""
from __future__ import annotations


def extract_text_pdfium(path) -> str:
    """Extract every page's text via pypdfium2, joined with ``\\n``.

    The text is layout-ordered (top-to-bottom, left-to-right within
    each page), which is what the line-anchored parsers expect. Each
    page's text is taken via ``PdfPage.get_textpage()`` and
    ``PdfTextPage.get_text_bounded()`` — the latter returns the full
    text without coordinate filtering (calling it directly, rather
    than the deprecated ``get_text_range()`` redirect, avoids a
    per-PDF UserWarning).

    Resources are released explicitly (textpage/page/document
    ``close()``) — PDFium handles are C pointers and Python GC isn't
    deterministic enough to rely on across hundreds of PDFs.
    """
    import pypdfium2 as pdfium
    parts: list[str] = []
    pdf = pdfium.PdfDocument(str(path))
    try:
        for i in range(len(pdf)):
            page = pdf[i]
            try:
                textpage = page.get_textpage()
                try:
                    parts.append(textpage.get_text_bounded() or "")
                finally:
                    textpage.close()
            finally:
                page.close()
    finally:
        pdf.close()
    return "\n".join(parts)


def extract_text_pdfplumber(path) -> str:
    """Concatenate every page's text via pdfplumber, joined with
    newlines so per-account parsers can split on a per-page account
    header regardless of which page it lands on."""
    import pdfplumber
    with pdfplumber.open(str(path)) as pdf:
        return "\n".join(
            (page.extract_text() or "") for page in pdf.pages
        )
