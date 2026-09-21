"""PDF text-extraction helpers shared by collector pdf_parsers.

Three extractors over the backends the collectors use:

* ``extract_text_pdfium`` — pypdfium2 (PDFium's C++ core). Layout-
  ordered text, one page per block. Used by the collectors whose
  statement layouts parse cleanly as space-delimited lines.
* ``extract_text_pdfplumber`` — pdfplumber. Per-page text joined
  with newlines, for the collectors whose parsers split on a
  per-page account header.
* ``extract_text_ocr`` — pypdfium2 to raster, then a recogniser to
  read the raster. For the archives whose PDFs are print-stream
  renderings with no text layer at all, where the two extractors
  above return nothing to parse. Two recognisers are supported so
  this is not a macOS-only capability: Apple's Vision framework
  (``ocrmac``) where it exists, and ``rapidocr`` — a pip wheel with
  bundled models — anywhere else.

The backends are **per-collector** dependencies, not installed in
every collector's venv. collectorkit is imported by all of them, so
the backend imports live inside the function bodies: importing this
module never requires any of them, and only the collector that calls a
given extractor needs its backend installed. A collector that OCRs
declares a recogniser for each platform it supports — plus, off macOS,
the inference runtime rapidocr's bundled models run on, which its own
wheel does not pull in. collectors/svb/requirements.txt is the worked
example:

    ocrmac>=1,<2 ; sys_platform == "darwin"
    rapidocr>=3,<4 ; sys_platform != "darwin"
    onnxruntime>=1.17 ; sys_platform != "darwin"
"""
from __future__ import annotations

import functools
import importlib.util
from typing import NamedTuple


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


# OCR raster resolution, as a multiple of 72 dpi. 3 (216 dpi) reads
# statement print cleanly; raising it costs time roughly with the
# square and does not monotonically improve a degraded scan.
OCR_SCALE = 3

# Two text runs belong to the same line when their vertical centres
# are closer than this fraction of the shorter run's height. Tuned
# on a two-column statement layout: it rejoins a label with the
# amount in the column beside it (whose baseline sits a hair off)
# while keeping consecutive ledger rows apart.
OCR_LINE_TOLERANCE = 0.5

# The recognisers, in preference order. Vision is built into macOS —
# nothing to install, nothing to download, and fast — so it leads
# where it exists; RapidOCR is a pip wheel with its own bundled models
# and runs anywhere, so it is what everyone else gets. Both are fixed
# local models, so either reads the same bytes the same way every
# time and a parse cache can memoise the result.
#
# They do NOT produce the same text as each other, so which one ran is
# part of what produced a parse: a cache key that folds in the
# installed extraction stack (collectorkit.srcfp) separates them,
# because each engine's package is present on one platform and absent
# on the other.
OCR_ENGINE_VISION = "vision"
OCR_ENGINE_RAPIDOCR = "rapidocr"


class TextRun(NamedTuple):
    """One recognised run of text, placed on the page.

    Coordinates are fractions of the page, measured from its TOP left,
    which is the order a page is read in. The engines do not agree on
    that — Vision measures from the bottom, RapidOCR from the top in
    pixels — so each adapter normalises to this and everything
    downstream sees one convention.

    The vertical position is the run's CENTRE rather than its top,
    because that is what decides which line it belongs to: a tall run
    and a short one on the same row share a centre but neither edge.
    Carrying the centre also keeps each adapter's arithmetic to a
    single rounding, which an edge plus half a height would not.
    """
    text: str
    x: float        # left edge
    centre: float   # vertical midpoint
    height: float


def resolve_ocr_engine(engine=None) -> str:
    """Return the OCR engine to use: the one asked for, or the best
    available.

    A name is checked against the recognisers this module reads, not
    against what is installed — naming the one a platform's
    requirements exclude still fails later, at that reader's own
    import. An unknown name raises ValueError here, rather than
    surfacing as a KeyError from the reader table two frames on, since
    the name is hand-typed. ImportError names both recognisers when
    neither is installed, since a caller with no recogniser cannot read
    a text-layer-less PDF at all.
    """
    if engine:
        if engine not in _READERS:
            raise ValueError(f"unknown OCR engine {engine!r}; supported: "
                             f"{', '.join(_READERS)}")
        return engine
    for name, module in ((OCR_ENGINE_VISION, "ocrmac"),
                         (OCR_ENGINE_RAPIDOCR, "rapidocr")):
        if importlib.util.find_spec(module) is not None:
            return name
    raise ImportError(
        "no OCR engine installed: ocrmac (macOS, via Apple's Vision "
        "framework) or rapidocr (any platform). Install one of them to "
        "read a PDF that carries no text layer.")


def extract_text_ocr(path, *, scale=OCR_SCALE,
                     line_tolerance=OCR_LINE_TOLERANCE, engine=None) -> str:
    """OCR every page of a text-layer-less PDF into layout-ordered
    lines, joined with ``\\n`` — the same shape the two extractors
    above return, so a parser can treat all three alike.

    Each page is rastered with pypdfium2 and read by whichever
    recogniser :func:`resolve_ocr_engine` picks. Neither returns
    lines; both return placed runs of text, which
    :func:`group_ocr_lines` rebuilds lines from.

    What the two engines read is NOT identical, so a parser meant to
    survive both has to be gated on something other than the exact
    characters — the document's own arithmetic, a checksum, a total it
    states. Whichever ran is folded into the parse-cache key, so
    moving between platforms re-parses rather than replaying the other
    engine's text.
    """
    import pypdfium2 as pdfium

    reader = _READERS[resolve_ocr_engine(engine)]()
    lines: list[str] = []
    pdf = pdfium.PdfDocument(str(path))
    try:
        for i in range(len(pdf)):
            page = pdf[i]
            try:
                image = page.render(scale=scale).to_pil()
            finally:
                page.close()
            lines.extend(group_ocr_lines(reader(image), line_tolerance))
    finally:
        pdf.close()
    return "\n".join(lines)


def group_ocr_lines(runs, line_tolerance=OCR_LINE_TOLERANCE) -> list[str]:
    """Rebuild page lines from placed :class:`TextRun`s, top to bottom
    then left to right.

    Runs are grouped by their vertical CENTRE rather than by an edge —
    a tall run and a short one on the same row share a centre but
    neither a top nor a bottom — and the tolerance scales with the
    shorter run's own height, so the same number works for a heading
    and for 6-point print.

    Pure: takes the runs, not an image, so it is testable without a
    PDF, an OCR engine, or a particular platform.
    """
    placed = sorted(((run.centre, run.x, run.height, run.text)
                     for run in runs), key=lambda r: (r[0], r[1]))
    lines: list[list[tuple[float, str]]] = []
    current: list[tuple[float, str]] = []
    centre = height = 0.0
    for run_centre, x, run_height, text in placed:
        if current and abs(run_centre - centre) > line_tolerance * min(height, run_height):
            lines.append(current)
            current = []
        if current:
            # Track the running mean, so a line that drifts across a
            # wide row is compared against the whole row, not its first
            # run.
            centre = (centre * len(current) + run_centre) / (len(current) + 1)
            height = min(height, run_height)
        else:
            centre, height = run_centre, run_height
        current.append((x, text))
    if current:
        lines.append(current)
    return [" ".join(text for _x, text in sorted(line)) for line in lines]


# Each adapter is built once per process. RapidOCR's constructor
# builds its inference sessions, which a cold pass over an archive
# would otherwise rebuild for every document; Vision costs nothing
# either way and is cached alongside it so the two stay symmetric.
# Caching the factory, not the module, keeps each engine's import
# inside the body it belongs to: nothing is imported until a document
# actually needs OCR.
@functools.lru_cache(maxsize=None)
def _vision_reader():
    """Apple's Vision framework, through ocrmac."""
    from ocrmac.ocrmac import text_from_image

    def read(image):
        return [
            run for run in (
                _run_from_vision_box(text, box)
                for text, _confidence, box
                in text_from_image(image, language_preference=["en-US"]))
            if run is not None
        ]
    return read


@functools.lru_cache(maxsize=None)
def _rapidocr_reader():
    """RapidOCR (PaddleOCR's models on onnxruntime). Returns a
    four-point polygon per run in PIXELS from the top left, so the
    page's own size is what turns them into fractions."""
    import numpy
    from rapidocr import RapidOCR

    engine = RapidOCR()

    def read(image):
        array = numpy.asarray(image.convert("RGB"))
        page_height, page_width = array.shape[0], array.shape[1]
        result = engine(array)
        if result is None or result.boxes is None or not result.txts:
            return []
        return [
            run for run in (
                _run_from_polygon(text, polygon, page_width, page_height)
                for text, polygon in zip(result.txts, result.boxes))
            if run is not None
        ]
    return read


def _run_from_vision_box(text, box):
    """One Vision bounding box as a page-fraction :class:`TextRun`.
    Vision gives ``(x, y, width, height)`` already as fractions but
    measures them from the page's BOTTOM left, so only the vertical
    axis turns over: the centre from the top is one minus the midpoint
    of the run's own span. A degenerate box is dropped, as on the
    RapidOCR side — a height of zero would collapse its row's grouping
    tolerance and split the row it belongs to."""
    x, y, _w, h = box
    if h <= 0:
        return None
    return TextRun(text, x, 1.0 - (y + h / 2), h)


def _run_from_polygon(text, polygon, page_width, page_height):
    """One pixel-space polygon as a page-fraction :class:`TextRun`.
    A recogniser can return a degenerate box; those are dropped rather
    than divided by zero."""
    if not page_width or not page_height:
        return None
    xs = [float(point[0]) for point in polygon]
    ys = [float(point[1]) for point in polygon]
    top, bottom = min(ys), max(ys)
    if bottom <= top:
        return None
    return TextRun(text, min(xs) / page_width,
                   (top + bottom) / 2 / page_height,
                   (bottom - top) / page_height)


_READERS = {
    OCR_ENGINE_VISION: _vision_reader,
    OCR_ENGINE_RAPIDOCR: _rapidocr_reader,
}
