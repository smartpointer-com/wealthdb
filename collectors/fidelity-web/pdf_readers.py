#!/usr/bin/env python3
"""
fidelity-web: the statement PDFs, extracted and parsed.

Each document kind's parser module reads an extracted document
(``parse(extracted, path=...)``, over ``collectorkit.words``) and
imports nothing that extracts, so its fingerprint is its own code. This
module holds the other half:

* the engine and the arguments each kind is extracted with
  (``collectorkit.extract``). Their extraction key is part of the parse
  cache's namespace in load.py, so an engine upgrade re-parses;
* ``parse_*_pdf(path)``, which extracts one document and parses it,
  from the extraction cache when the loader passes one;
* the 1099 identity read, which extracts a page only when the parser
  asks for it.

Run on its own, it prints what one kind reads from PDFs, as one JSON
array:

    python pdf_readers.py {statement,supplied,daf,1099} FILE.pdf ...
        [--json-out PATH]
"""

from __future__ import annotations

import functools
import sys

from collectorkit import extract, parser_cli
from collectorkit.words import Page, Word, params_key

import pdf_parsers
import pdf_parsers_1099
import pdf_parsers_daf
import pdf_parsers_supplied

# Every page's text, the 529, DAF and supplied statements' extraction:
# what collectorkit.pdf.extract_text_pdfplumber reads.
STATEMENT_TEXT = extract.pdfplumber

# Every page's words, as pdfplumber's extract_words() reads them with no
# arguments: the Consolidated 1099's extraction.
FORM_1099_WORDS = functools.partial(extract.pdfplumber, word_params=({},))


def _extracted(extractor, path, sha, cache_dir):
    """``path`` as ``extractor`` reads it. With a ``cache_dir``, from the
    extraction cache there, keyed on ``sha``, the document's sha256."""
    if cache_dir is None:
        return extractor(path)
    return extract.cached(extractor, path, sha, cache_dir)


def parse_statement_pdf(path, *, sha=None, cache_dir=None):
    """One 529 statement PDF, extracted and parsed
    (``pdf_parsers.parse``)."""
    return pdf_parsers.parse(
        _extracted(STATEMENT_TEXT, path, sha, cache_dir), path=path)


def parse_daf_statement_pdf(path, *, sha=None, cache_dir=None):
    """One DAF statement PDF, extracted and parsed
    (``pdf_parsers_daf.parse``)."""
    return pdf_parsers_daf.parse(
        _extracted(STATEMENT_TEXT, path, sha, cache_dir), path=path)


def parse_supplied_statement_pdf(path, *, expected_signature=None, sha=None,
                                 cache_dir=None):
    """One supplied statement PDF, extracted and parsed
    (``pdf_parsers_supplied.parse``)."""
    return pdf_parsers_supplied.parse(
        _extracted(STATEMENT_TEXT, path, sha, cache_dir), path=path,
        expected_signature=expected_signature)


def parse_consolidated_1099_pdf(path, *, sha=None, cache_dir=None):
    """One Consolidated Form 1099 PDF, extracted and parsed
    (``pdf_parsers_1099.parse``)."""
    return pdf_parsers_1099.parse(
        _extracted(FORM_1099_WORDS, path, sha, cache_dir), path=path)


def _plumber_pages(pdf):
    """Each page of an open pdfplumber document as ``FORM_1099_WORDS``
    reads it, its words only, a page at a time."""
    key = params_key({})
    for page in pdf.pages:
        yield Page("", ((key, tuple(
            Word(w["text"], w["x0"], w["x1"], w["top"], w["bottom"])
            for w in page.extract_words())),))


def read_consolidated_1099_identity(path):
    """Which form a Consolidated 1099 PDF is
    (``pdf_parsers_1099.read_identity``).

    Most copies are never parsed in full, so this read is not cached as
    an extraction: it opens the pages one at a time, only as far as the
    parser reads."""
    import pdfplumber

    with pdfplumber.open(str(path)) as pdf:
        return pdf_parsers_1099.read_identity(_plumber_pages(pdf), path=path)


_KINDS = {
    "statement": (
        parse_statement_pdf,
        "Extract per-account Holdings rows from one or more Fidelity 529 "
        "statement PDFs and emit JSON."),
    "supplied": (
        parse_supplied_statement_pdf,
        "Extract per-account Holdings rows from one or more legacy "
        "supplied statement PDFs and emit JSON."),
    "daf": (
        parse_daf_statement_pdf,
        "Extract the pool holdings from one or more Fidelity Charitable "
        "statement PDFs and emit JSON."),
    "1099": (
        parse_consolidated_1099_pdf,
        "Extract the Form 1099-B lots from one or more Consolidated Form "
        "1099 PDFs and emit JSON."),
}


def main(argv) -> int:
    if not argv or argv[0] not in _KINDS:
        print(f"usage: pdf_readers.py {{{','.join(_KINDS)}}} PDF [PDF ...] "
              "[--json-out PATH]", file=sys.stderr)
        return 2
    parse, description = _KINDS[argv[0]]
    return parser_cli.dump_json(argv[1:], parse, description=description)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
