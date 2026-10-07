"""Schedule K-1 (Form 1065): the partner's tax capital account and gain lines.

A fund's K-1 package is a PDF: a cover letter, the federal face page, the
preparer's supporting statements and any state K-1s. Only the federal face
page is read. It is a printed IRS form with three columns: Parts I and II
with the capital account analysis (item L) on the left, and Part III's
boxes 1-13 and 14-21 in the middle and on the right. A box prints its
number and caption on one line and its amount below or beside it, flush to
the column's right edge.

`pdftotext -layout` folds those columns into one another wherever a line is
dense, so this module reads word boxes instead (`pdftotext -bbox`). A word
belongs to the column its left edge falls in, and to the box whose caption
is the nearest one above it in that column. An item-L amount belongs to the
caption line nearest it vertically.

What is read, as printed:

- item L: beginning capital, capital contributed, current-year net income,
  other increase (decrease), withdrawals and distributions, ending capital;
- box 8 (net short-term capital gain) and box 9a (net long-term);
- box 19, code A (cash and marketable securities) and code C (other
  property).

Amounts are kept as decimal strings with the thousands separators dropped,
a trailing period dropped, and a minus sign where the form prints one or
wraps the amount in parentheses. The withdrawals line is the exception: the
form prints its own parentheses around that field, so the figure inside is
the amount withdrawn. A field the form leaves blank, or fills with a
reference to an attached statement, is None.
"""
from __future__ import annotations

import html
import re
import subprocess
from pathlib import Path
from typing import NamedTuple

from collectorkit import pdftotext


class Word(NamedTuple):
    """One word box from `pdftotext -bbox`, in PDF points (y grows down)."""
    x0: float
    y0: float
    x1: float
    text: str


_PAGE = re.compile(r"<page\b[^>]*>(.*?)</page>", re.S)
_WORD = re.compile(r'<word xMin="([\d.]+)" yMin="([\d.]+)" xMax="([\d.]+)" '
                   r'yMax="[\d.]+">(.*?)</word>')
# A printed amount: digits with optional thousands separators and decimals,
# a leading minus or wrapping parentheses, an optional dollar sign.
_AMOUNT = re.compile(r"^\(?-?\$?\d[\d,]*(?:\.\d*)?\)?$")
_BOX_NUMBER = re.compile(r"^\d{1,2}[a-c]?$")

# Geometry, in PDF points.
# Words on one printed line differ in yMin by a fraction of a point.
_SAME_LINE = 2.0
# A box's codes print a little left of its number, so a column boundary
# sits this far before the column's box numbers.
_COLUMN_MARGIN = 8.0
# A box number or code sits within this distance of the column boundary;
# amounts sit further right, flush to the column's right edge.
_LABEL_SLOT = 16.0
# How far an item-L amount may sit from its caption line. The form's caption
# lines are about 12 points apart; preparers print the amount a few points
# above or below its caption.
_ITEM_L_REACH = 8.0

# Item L's heading. Forms before 2020 print it in sentence case with a
# colon; phrases match without regard to case, a trailing colon or the
# apostrophe's form (_norm).
_ITEM_L_HEADING = "Partner's Capital Account Analysis"
# Item L captions, as the form prints them, by the column each fills. Forms
# before 2020 word two of the lines differently.
_ITEM_L = {
    "beginning_capital": ("Beginning capital account",),
    "contributions": ("Capital contributed during the year",),
    "net_income": ("Current year net income (loss)",
                   "Current year increase (decrease)"),
    "other_change": ("Other increase (decrease)",),
    "distributions": ("Withdrawals and distributions",
                      "Withdrawals & distributions"),
    "ending_capital": ("Ending capital account",),
}
# Part III boxes read, by the column each fills.
_BOXES = {"8": "short_term_gain", "9a": "long_term_gain"}
# Box 19 codes read, by the column each fills.
_DISTRIBUTION_CODES = {"A": "cash_distributions", "C": "property_distributions"}


def bbox_pages(xhtml: str) -> list[list[Word]]:
    """`pdftotext -bbox` output as one list of words per page."""
    return [[Word(float(x0), float(y0), float(x1), html.unescape(t))
             for x0, y0, x1, t in _WORD.findall(body)]
            for body in _PAGE.findall(xhtml)]


def _lines(words: list[Word]) -> list[list[Word]]:
    """Words grouped into printed lines, top to bottom, each left to right."""
    out: list[list[Word]] = []
    for w in sorted(words, key=lambda w: (w.y0, w.x0)):
        if out and abs(out[-1][0].y0 - w.y0) <= _SAME_LINE:
            out[-1].append(w)
        else:
            out.append([w])
    return [sorted(line, key=lambda w: w.x0) for line in out]


def _norm(text: str) -> str:
    """A word as phrases match it: case-folded, a typographic apostrophe
    made straight, a trailing colon dropped."""
    return text.casefold().replace("\u2019", "'").removesuffix(":")


def _find(lines: list[list[Word]], phrase: str) -> tuple[int, int] | None:
    """(line index, word index) where `phrase` starts, or None. Words
    compare as _norm leaves them."""
    want = [_norm(t) for t in phrase.split()]
    for li, line in enumerate(lines):
        texts = [_norm(w.text) for w in line]
        for wi in range(len(texts) - len(want) + 1):
            if texts[wi:wi + len(want)] == want:
                return li, wi
    return None


def _decimal(raw: str, *, magnitude: bool = False) -> str | None:
    """A printed amount as a decimal string, or None if it is not one."""
    if not _AMOUNT.match(raw):
        return None
    neg = raw.startswith("-") or (raw.startswith("(") and raw.endswith(")"))
    digits = raw.strip("()").lstrip("-").lstrip("$").replace(",", "")
    digits = digits.rstrip(".")
    return digits if magnitude or not neg else "-" + digits


def is_face_page(words: list[Word]) -> bool:
    """Whether a page is the federal Schedule K-1 (Form 1065) face page."""
    text = " ".join(w.text for w in words)
    lines = _lines(words)
    return ("Schedule K-1" in text and _find(lines, _ITEM_L_HEADING) is not None
            and _find(lines, "Ordinary business income") is not None)


def parse_face_page(words: list[Word]) -> dict | None:
    """The figures the face page prints, or None when the page lacks the
    form's column anchors. Keys: `tax_year` (int or None), the six item-L
    figures, `short_term_gain`, `long_term_gain`, `cash_distributions`,
    `property_distributions` (decimal strings or None), and `printed`, the
    raw text of every figure read."""
    lines = _lines(words)
    box1 = _find(lines, "Ordinary business income")
    box14 = _find(lines, "Self-employment earnings")
    if box1 is None or box14 is None or box1[1] == 0 or box14[1] == 0:
        return None
    # Each column starts at its box numbers: the word before each caption.
    mid = lines[box1[0]][box1[1] - 1].x0 - _COLUMN_MARGIN
    right = lines[box14[0]][box14[1] - 1].x0 - _COLUMN_MARGIN

    out: dict = {"tax_year": None, "printed": {}}
    for line in lines:
        m = re.search(r"calendar year (\d{4})", " ".join(w.text for w in line))
        if m:
            out["tax_year"] = int(m.group(1))
            break

    item_l = _item_l_amounts(lines, mid)
    for col in _ITEM_L:
        _put(out, col, item_l.get(col))
    boxes = _boxes(lines, mid, right)
    for box, col in _BOXES.items():
        _put(out, col, _first_amount(boxes.get(("mid", box), [])))
    for code, col in _DISTRIBUTION_CODES.items():
        _put(out, col, _coded_amount(boxes.get(("right", "19"), []), code, right))
    return out


def _put(out: dict, col: str, raw: str | None) -> None:
    """Store one figure read from the form, and its printed text."""
    out[col] = None
    if raw is not None:
        out["printed"][col] = raw
        out[col] = _decimal(raw, magnitude=col == "distributions")


def _item_l_amounts(lines: list[list[Word]], mid: float) -> dict[str, str]:
    """The printed item-L amounts, by column. Each left-column amount within
    reach of an item-L caption belongs to the caption line nearest it; the
    first amount a caption gets is its figure."""
    heading = _find(lines, _ITEM_L_HEADING)
    captions: dict[str, Word] = {}
    for col, phrases in _ITEM_L.items():
        for phrase in phrases:
            hit = _find(lines, phrase)
            if hit and (heading is None or hit[0] > heading[0]):
                captions[col] = lines[hit[0]][hit[1]]
                break
    out: dict[str, str] = {}
    for line in lines:
        for w in line:
            if w.x0 >= mid or _decimal(w.text) is None or not captions:
                continue
            col, cap = min(captions.items(), key=lambda kv: abs(kv[1].y0 - w.y0))
            if abs(cap.y0 - w.y0) <= _ITEM_L_REACH and w.x0 > cap.x1:
                out.setdefault(col, w.text)
    return out


def _boxes(lines: list[list[Word]], mid: float,
           right: float) -> dict[tuple[str, str], list[list[Word]]]:
    """Part III's words, by (column, box number), as lines. A box runs from
    its caption line down to the next caption in the same column. A word on
    a caption line that precedes more caption text is part of the caption
    (a section number such as the 1231 in box 10), not an amount."""
    current: dict[str, str | None] = {"mid": None, "right": None}
    out: dict[tuple[str, str], list[list[Word]]] = {}
    for line in lines:
        per_col: dict[str, list[Word]] = {"mid": [], "right": []}
        for w in line:
            if w.x0 >= right:
                per_col["right"].append(w)
            elif w.x0 >= mid:
                per_col["mid"].append(w)
        for col, ws in per_col.items():
            if not ws:
                continue
            edge = mid if col == "mid" else right
            first = ws[0]
            if (first.x0 - edge < _LABEL_SLOT and _BOX_NUMBER.match(first.text)
                    and len(ws) > 1 and ws[1].text[:1].isalpha()):
                current[col] = first.text
                ws = _caption_tail(ws[1:])
            if current[col] is not None and ws:
                out.setdefault((col, current[col]), []).append(ws)
    return out


def _caption_tail(ws: list[Word]) -> list[Word]:
    """The words after a caption's last alphabetic word: what the line
    prints beside the caption rather than in it."""
    last_alpha = max((i for i, w in enumerate(ws) if re.search(r"[A-Za-z]", w.text)),
                     default=-1)
    return ws[last_alpha + 1:]


def _first_amount(box_lines: list[list[Word]]) -> str | None:
    """The first printed amount in a box, top to bottom; None if blank."""
    return next((w.text for ws in box_lines for w in ws
                 if _decimal(w.text) is not None), None)


def _coded_amount(box_lines: list[list[Word]], code: str,
                  edge: float) -> str | None:
    """The amount printed on the line of `code` in a coded box (box 19)."""
    for ws in box_lines:
        if ws[0].text == code and ws[0].x0 - edge < _LABEL_SLOT:
            return _first_amount([ws[1:]])
    return None


def parse_bbox(xhtml: str) -> dict | None:
    """The first federal face page of a K-1 package, parsed; None for a
    document that has none (a 1042-S, a state-only schedule)."""
    for words in bbox_pages(xhtml):
        if is_face_page(words):
            return parse_face_page(words)
    return None


def bbox_xhtml(path: Path | str, *, timeout: float | None = None) -> str:
    """One PDF as `pdftotext -bbox` XHTML. Raises the same errors as
    collectorkit.pdftotext.layout_text."""
    try:
        proc = subprocess.run(["pdftotext", "-bbox", str(path), "-"],
                              capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise pdftotext.ExtractionError(
            f"pdftotext timed out after {timeout:g}s") from exc
    except OSError as exc:
        raise pdftotext.ToolMissing(
            f"pdftotext (poppler-utils) cannot be run: {exc}") from exc
    if proc.returncode != 0:
        detail = proc.stderr.decode("utf-8", errors="replace").strip()[:200]
        raise pdftotext.ExtractionError(
            f"pdftotext exited {proc.returncode}: {detail}")
    return proc.stdout.decode("utf-8", errors="replace")
