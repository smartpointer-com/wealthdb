"""Words on a page, the extracted document, and word geometry.

A parser reads a document as an :class:`Extracted`: one :class:`Page` per
page, each with its text and, for the engines that read them, its
:class:`Word` boxes. :func:`rows` groups words into printed lines, and
:func:`columns` and :func:`cell_of` place a word in a column, with each
reader's own tolerance, anchor and rule as parameters.

The rule for fingerprints: a parser imports this module and never
``collectorkit.extract``, so an edit here re-parses its importers and an
edit to an engine re-extracts without re-parsing. That is why it imports
nothing but the standard library.
"""
from __future__ import annotations

import json
from collections.abc import Iterable, Mapping, Sequence
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
    there ``top`` is the larger number. OCR runs carry no right edge, so
    ``collectorkit.extract.ocr`` reports no words."""
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

    ``sep`` is how that engine's legacy extractor joined its pages, and
    ``skip_empty`` whether it left an empty page out of the join. OCR is
    the one that did: ``collectorkit.pdf.extract_text_ocr`` joins lines,
    not pages, so a page that yields no line adds nothing."""
    engine: str
    pages: tuple[Page, ...]
    sep: str = "\n"
    skip_empty: bool = False

    def text(self, sep: str | None = None) -> str:
        """Every page's text, joined. With no ``sep``, the string the
        engine's legacy extractor returned for the same document."""
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


_ROW_ANCHORS = ("first", "mean")
_ROW_POSITIONS = {"top": lambda w: w.top, "mid": lambda w: w.mid}


def rows(words: Iterable[Word], tolerance: float, anchor: str = "first", *,
         by: str = "top", relative: bool = False) -> list[Row]:
    """Group words into printed lines, top to bottom, each left to right.

    Words are taken in order of (``by`` position, ``x0``). A word joins
    the current line when its position lies within ``tolerance`` of the
    line's anchor; otherwise it opens the next line. The anchor is the
    line's first word (``"first"``) or the running mean of its words
    (``"mean"``). With ``relative``, the tolerance is a fraction of the
    shorter height: the word's, or the shortest in the line so far.

    Comparing with the last line only is the same as testing every line:
    words come in position order, and a line opens only beyond the
    tolerance of the one before it.

    The readers' parameters: ubs-web's ledger ``(3.0, "first")``,
    fidelity-web's 1099 and carta's K-1 ``(2.0, "first")``, and
    ``collectorkit.pdf.group_ocr_lines`` ``(0.5, "mean", by="mid",
    relative=True)``. Words with an equal ``x0`` keep their input order;
    group_ocr_lines orders those by text.

    ``by="top"`` reads a top-down frame (pdfplumber, ``pdftotext
    -bbox``); PDFium's words measure up from the bottom."""
    if anchor not in _ROW_ANCHORS:
        raise ValueError(f"anchor must be one of {_ROW_ANCHORS}: {anchor!r}")
    position = _ROW_POSITIONS[by]
    out: list[Row] = []
    current: list[Word] = []
    y = height = 0.0
    for w in sorted(words, key=lambda w: (position(w), w.x0)):
        wy = position(w)
        w_height = w.bottom - w.top
        if current:
            limit = (tolerance * min(height, w_height) if relative
                     else tolerance)
            if abs(wy - y) > limit:
                out.append(Row(y, tuple(sorted(current, key=lambda w: w.x0))))
                current = []
        if current:
            if anchor == "mean":
                y = (y * len(current) + wy) / (len(current) + 1)
            height = min(height, w_height)
        else:
            y, height = wy, w_height
        current.append(w)
    if current:
        out.append(Row(y, tuple(sorted(current, key=lambda w: w.x0))))
    return out


_COLUMN_EDGES = ("left", "right", "centre")


def columns(header_row: Sequence[Word], labels: Mapping[str, str],
            edge: str = "right", *, ordered: bool = False
            ) -> dict[str, float] | None:
    """Each column's edge, read off a header line: ``{name: x}`` in the
    order of ``labels``, or None when a label is not on the line.

    A label is one or more words, matched as consecutive words of the
    line, its first occurrence from the left. ``edge`` is the label's
    left edge (its first word's ``x0``), its right edge (its last word's
    ``x1``) or its centre (the middle of the two). With ``ordered``, each
    label is looked for from where the one before it was found, which
    tells apart labels that share a word ("Transaction price",
    "Transaction gain")."""
    if edge not in _COLUMN_EDGES:
        raise ValueError(f"edge must be one of {_COLUMN_EDGES}: {edge!r}")
    words = list(header_row)
    out: dict[str, float] = {}
    start = 0
    for name, label in labels.items():
        want = label.split()
        at = next((k for k in range(start if ordered else 0,
                                    len(words) - len(want) + 1)
                   if [w.text for w in words[k:k + len(want)]] == want),
                  None)
        if at is None:
            return None
        first, last = words[at], words[at + len(want) - 1]
        out[name] = {"left": first.x0, "right": last.x1,
                     "centre": (first.x0 + last.x1) / 2}[edge]
        start = at
    return out


_CELL_RULES = ("nearest", "first_reaching", "last_passed")
_CELL_POSITIONS = {"x0": lambda w: w.x0, "x1": lambda w: w.x1,
                   "centre": lambda w: (w.x0 + w.x1) / 2}


def cell_of(word: Word, columns: Mapping[str, float | None],
            slack: float | None = None, *, by: str = "x1",
            rule: str = "nearest", default: str | None = None) -> str | None:
    """The column a word sits in, by one of its edges (``by``: ``"x0"``,
    ``"x1"``, ``"centre"``) against the columns' edges.

    * ``"nearest"``: the column whose edge is closest, the first of equals;
      beyond ``slack`` (when given) it is ``default``. A column whose edge
      is None is skipped.
    * ``"first_reaching"``: the first column, in order, whose edge plus
      ``slack`` is at or past the word.
    * ``"last_passed"``: the last column, in order, whose edge minus
      ``slack`` the word is at or past.

    The readers' parameters: ubs-web's ledger ``("centre", "nearest")``;
    schwab-web's year-end lots ``("x1", "nearest", slack=12.0)``; ubs-web's
    transaction list ``("x1", "first_reaching", slack=3.0,
    default="G")``; fidelity-web's 1099 ``("x1", "last_passed")`` over
    columns sorted by edge; carta's K-1 ``("x0", "last_passed")``."""
    if rule not in _CELL_RULES:
        raise ValueError(f"rule must be one of {_CELL_RULES}: {rule!r}")
    x = _CELL_POSITIONS[by](word)
    if rule == "nearest":
        best, best_d = None, float("inf")
        for name, at in columns.items():
            if at is None:
                continue
            d = abs(x - at)
            if d < best_d:
                best, best_d = name, d
        if best is None or (slack is not None and best_d > slack):
            return default
        return best
    margin = slack or 0.0
    if rule == "first_reaching":
        return next((name for name, at in columns.items()
                     if at is not None and x <= at + margin), default)
    found = default
    for name, at in columns.items():
        if at is not None and x >= at - margin:
            found = name
    return found
