"""How schwab-web reads its PDFs: PDFium's text and, for a Year-End
Summary, its words (``collectorkit.extract.pdfium``), extracted once per
document and cached by content while a load names a cache.

The parse_*_pdf wrappers call this module and no parser does, so no
parser fingerprint holds the extraction code. The loader folds
:data:`EXTRACTORS`' extraction keys into its parser generations
instead, so an engine upgrade re-derives what the engine read.
"""
from __future__ import annotations

import functools
from pathlib import Path

from collectorkit import bronze, extract
from collectorkit.words import Extracted

TEXT = extract.pdfium
WORDS = functools.partial(extract.pdfium, words=True)
EXTRACTORS = (TEXT, WORDS)

# Where this process caches extractions: None for no cache. A load sets it
# for its own process and hands it to each parse worker.
CACHE_DIR: Path | None = None


def pdfium(path, *, words: bool = False) -> Extracted:
    """The PDF at `path` as PDFium reads it, with every page's words
    when `words`. Under a CACHE_DIR the extraction is read from, or
    stored in, that cache, by the file's content."""
    extractor = WORDS if words else TEXT
    if CACHE_DIR is None:
        return extractor(path)
    return extract.cached(extractor, path, bronze.sha256_file(path)[0],
                          CACHE_DIR)
