"""Words on a page, the extracted document, and word geometry.

A parser reads a document as an :class:`Extracted`: one :class:`Page` per
page, each with its text and, for the engines that read them, its
:class:`Word` boxes. :func:`rows` groups words into printed lines, and
:func:`cell_of` places a word in the nearest column.

The rule for fingerprints: a parser imports this module and never
``collectorkit.extract``, so an edit here re-parses its importers and an
edit to an engine re-extracts without re-parsing. That is why it imports
nothing but the standard library.
"""
from __future__ import annotations

import json
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from typing import NamedTuple


# ============================================================
# The extracted document
# ============================================================

class Word(NamedTuple):
    """One word on a page and its box, in PDF points.

    The coordinates are the engine's own. pdfplumber and ``pdftotext
    -bbox`` measure down from the page's top, so ``top`` is below
    ``bottom`` in number. PDFium measures up from the page's bottom, so
    there ``top`` is the larger number."""
    text: str
    x0: float
    x1: float
    top: float
    bottom: float

    @property
    def mid(self) -> float:
        """The vertical middle of the box."""
        return (self.top + self.bottom) / 2


def params_key(params: Mapping) -> str:
    """One set of keyword arguments as a stable string."""
    return json.dumps(dict(params), sort_keys=True, separators=(",", ":"))


@dataclass(frozen=True)
class Page:
    """One page: its text, and its words for each set of word arguments
    the extractor was asked for."""
    text: str
    word_sets: tuple[tuple[str, tuple[Word, ...]], ...] = ()

    def words(self, **params) -> tuple[Word, ...]:
        """The page's words, as read with exactly these word arguments.
        Raises KeyError when the extraction did not read them."""
        key = params_key(params)
        for have, words in self.word_sets:
            if have == key:
                return words
        raise KeyError(f"no words extracted with {key}; extracted: "
                       f"{[have for have, _ in self.word_sets]}")


@dataclass(frozen=True)
class Extracted:
    """A whole document as one engine read it.

    ``sep`` is how the engine's whole-document extractor joins its pages,
    and ``skip_empty`` whether it leaves an empty page out of the join.
    No extractor of ``collectorkit.extract`` sets it; it is part of the
    cache entry's form."""
    engine: str
    pages: tuple[Page, ...]
    sep: str = "\n"
    skip_empty: bool = False

    def text(self, sep: str | None = None) -> str:
        """Every page's text, joined. With no ``sep``, the string the
        engine's whole-document extractor returns for the same
        document."""
        joiner = self.sep if sep is None else sep
        return joiner.join(p.text for p in self.pages
                           if not (self.skip_empty and not p.text))


# ============================================================
# Word geometry
# ============================================================

class Row(NamedTuple):
    """One printed line: its vertical anchor and its words, left to
    right."""
    top: float
    words: tuple[Word, ...]


def rows(words: Iterable[Word], tolerance: float,
         anchor: str = "first") -> list[Row]:
    """Group words into printed lines, top to bottom, each left to right.

    Words are taken in order of (``top``, ``x0``). A word joins the
    current line when its ``top`` lies within ``tolerance`` of the line's
    anchor, its first word's ``top`` (``anchor`` is ``"first"``, the one
    anchor); otherwise it opens the next line. Words with an equal ``x0``
    keep their input order.

    Comparing with the last line only is the same as testing every line:
    words come in ``top`` order, and a line opens only beyond the
    tolerance of the one before it.

    The frame is top-down (pdfplumber, ``pdftotext -bbox``); PDFium's
    words measure up from the bottom. Callers: ubs-web's ledger and
    transaction list ``(3.0, "first")``, fidelity-web's 1099 and
    ``collectorkit.k1`` ``(2.0, "first")``."""
    if anchor != "first":
        raise ValueError(f"anchor must be 'first': {anchor!r}")
    out: list[Row] = []
    current: list[Word] = []
    y = 0.0
    for w in sorted(words, key=lambda w: (w.top, w.x0)):
        if current and abs(w.top - y) > tolerance:
            out.append(Row(y, tuple(sorted(current, key=lambda w: w.x0))))
            current = []
        if not current:
            y = w.top
        current.append(w)
    if current:
        out.append(Row(y, tuple(sorted(current, key=lambda w: w.x0))))
    return out


def cell_of(word: Word, columns: Mapping[str, float | None],
            slack: float | None = None) -> str | None:
    """The column whose edge is nearest the word's right edge (``x1``),
    the first of equals, or None beyond ``slack`` (when given) or when no
    column has an edge. A column whose edge is None is skipped.

    schwab-web's year-end lots call it with ``slack=12.0``."""
    best, best_d = None, float("inf")
    for name, at in columns.items():
        if at is None:
            continue
        d = abs(word.x1 - at)
        if d < best_d:
            best, best_d = name, d
    if best is None or (slack is not None and best_d > slack):
        return None
    return best
