"""How schwab-web reads its PDFs: PDFium's text and, for a Year-End
Summary, its words (``collectorkit.extract.pdfium``), extracted once per
document and cached by content when the caller names a cache.

The parse_*_pdf wrappers call this module and no parser does, so no
parser fingerprint holds the extraction code. The loader folds
:data:`EXTRACTORS`' extraction keys into its parser generations
instead, so an engine upgrade re-derives what the engine read.
"""
from __future__ import annotations

import functools

from collectorkit import extract
from collectorkit.words import Extracted

TEXT = extract.pdfium
WORDS = functools.partial(extract.pdfium, words=True)
EXTRACTORS = (TEXT, WORDS)


def pdfium(path, sha256: str | None = None, cache_dir=None, *,
           words: bool = False) -> Extracted:
    """The PDF at `path` as PDFium reads it, with every page's words
    when `words`. With its `sha256` and a `cache_dir` the extraction is
    read from, or stored in, that cache."""
    extractor = WORDS if words else TEXT
    if sha256 is None:
        return extractor(path)
    return extract.cached(extractor, path, sha256, cache_dir)
