"""Tests for collectorkit.pdf — engine selection, the coordinate
normalisation each recogniser needs, the line reconstruction, and how
a recogniser is built.

The extractors themselves are thin wrappers around their backends
(pypdfium2 / pdfplumber / ocrmac / rapidocr), which are per-collector
dependencies this package does not install. What carries logic is the
part that turns a recogniser's own coordinate convention into one
shared one, and the part that rebuilds lines from it — both pure
functions. The reader factories are reached with a stand-in module in
place of each engine, so the whole file runs with no PDF, no OCR
engine and no particular platform.

`TextRun` coordinates are fractions of the page from its TOP left, and
the vertical one is the run's centre.
"""
from __future__ import annotations

import sys
import types

import pytest

from collectorkit.pdf import (
    OCR_ENGINE_RAPIDOCR,
    OCR_ENGINE_VISION,
    TextRun,
    _READERS,
    _rapidocr_reader,
    _run_from_polygon,
    _run_from_vision_box,
    _vision_reader,
    group_ocr_lines,
    resolve_ocr_engine,
)


def run(text, x, centre, height=0.01):
    return TextRun(text, x, centre, height)


# ============================================================
# Engine selection
# ============================================================

def test_an_explicit_engine_is_honoured():
    # A named engine wins over detection. It is checked for being one
    # of the two recognisers, not for being installed, so naming the
    # one a platform's requirements exclude still fails later, at that
    # reader's own import.
    assert resolve_ocr_engine(OCR_ENGINE_RAPIDOCR) == OCR_ENGINE_RAPIDOCR
    assert resolve_ocr_engine(OCR_ENGINE_VISION) == OCR_ENGINE_VISION


def test_an_unknown_engine_says_what_is_supported():
    # The name is hand-typed, so a typo names the two recognisers here
    # rather than surfacing as a KeyError from the reader table two
    # frames on.
    with pytest.raises(ValueError) as excinfo:
        resolve_ocr_engine("visoin")
    assert "visoin" in str(excinfo.value)
    assert OCR_ENGINE_VISION in str(excinfo.value)
    assert OCR_ENGINE_RAPIDOCR in str(excinfo.value)


def test_neither_engine_installed_says_so_by_name(monkeypatch):
    # A caller with no recogniser cannot read a text-layer-less PDF at
    # all, so this fails loudly and names both options rather than
    # returning empty text that looks like an unreadable document.
    monkeypatch.setattr("importlib.util.find_spec", lambda name: None)
    with pytest.raises(ImportError) as excinfo:
        resolve_ocr_engine()
    assert "ocrmac" in str(excinfo.value)
    assert "rapidocr" in str(excinfo.value)


def test_vision_is_preferred_where_it_exists(monkeypatch):
    monkeypatch.setattr("importlib.util.find_spec",
                        lambda name: object() if name in ("ocrmac", "rapidocr") else None)
    assert resolve_ocr_engine() == OCR_ENGINE_VISION


def test_rapidocr_is_the_fallback(monkeypatch):
    monkeypatch.setattr("importlib.util.find_spec",
                        lambda name: object() if name == "rapidocr" else None)
    assert resolve_ocr_engine() == OCR_ENGINE_RAPIDOCR


# ============================================================
# Normalising a recogniser's own coordinates
# ============================================================

def test_a_pixel_polygon_becomes_page_fractions():
    # RapidOCR returns four corners in pixels from the top left.
    polygon = [(100, 50), (300, 50), (300, 70), (100, 70)]
    got = _run_from_polygon("EXAMPLE", polygon, page_width=1000, page_height=500)
    # centre of 50..70px on a 500px page is 60/500 = 0.12
    assert got == TextRun("EXAMPLE", 0.1, 0.12, 0.04)


def test_a_rotated_polygon_uses_its_extremes():
    # A skewed scan gives a polygon that is not axis-aligned; the run's
    # box is its bounding box, so a tilted line still lands on its row.
    polygon = [(110, 52), (300, 48), (302, 70), (112, 74)]
    got = _run_from_polygon("EXAMPLE", polygon, page_width=1000, page_height=500)
    assert got.x == pytest.approx(0.11)
    assert got.centre == pytest.approx(0.122)
    assert got.height == pytest.approx(0.052)


def test_a_degenerate_box_is_dropped_not_divided_by():
    assert _run_from_polygon("x", [(0, 5), (1, 5), (1, 5), (0, 5)], 100, 100) is None
    assert _run_from_polygon("x", [(0, 0), (1, 0), (1, 1), (0, 1)], 0, 100) is None


def test_a_vision_box_flips_to_the_top_origin_frame():
    # Vision measures from the BOTTOM left and y is the box's bottom
    # edge, so 0.80..0.88 from the foot is 0.12..0.20 from the top.
    # The box is deliberately not centred on the page: a centre of 0.16
    # separates the midpoint from either edge and from a missing flip,
    # which would give 0.84.
    got = _run_from_vision_box("EXAMPLE", (0.1, 0.80, 0.2, 0.08))
    assert got.text == "EXAMPLE"
    assert got.x == pytest.approx(0.1)
    assert got.centre == pytest.approx(0.16)
    assert got.height == pytest.approx(0.08)


def test_the_two_engines_place_the_same_run_alike():
    # One run described in each recogniser's own frame — RapidOCR in
    # pixels from the top, Vision in fractions from the bottom — must
    # reach line grouping as the same placement. That agreement is what
    # lets one parser read either engine's output.
    rapid = _run_from_polygon("EXAMPLE", [(100, 60), (300, 60), (300, 100), (100, 100)],
                              page_width=1000, page_height=500)
    vision = _run_from_vision_box("EXAMPLE", (0.1, 0.80, 0.2, 0.08))
    assert vision.x == pytest.approx(rapid.x)
    assert vision.centre == pytest.approx(rapid.centre)
    assert vision.height == pytest.approx(rapid.height)


def test_a_degenerate_vision_box_is_dropped():
    # A zero-height run would collapse its row's tolerance — which
    # scales with the shorter run's height — and split that row.
    assert _run_from_vision_box("x", (0.1, 0.5, 0.2, 0.0)) is None


# ============================================================
# Line reconstruction
# ============================================================

def test_reads_top_to_bottom_then_left_to_right():
    # Deliberately shuffled: the grouping must impose the order, not
    # inherit it.
    lines = group_ocr_lines([
        run("Balance", 0.7, 0.10),
        run("Date", 0.1, 0.10),
        run("second row", 0.1, 0.20),
    ])
    assert lines == ["Date Balance", "second row"]


def test_a_taller_run_shares_a_line_with_a_shorter_one():
    # A heading and the small print beside it sit on one row but share
    # neither a top nor a bottom edge — only a centre.
    lines = group_ocr_lines([
        run("HEADING", 0.1, 0.10, height=0.03),
        run("note", 0.6, 0.10, height=0.01),
    ])
    assert lines == ["HEADING note"]


def test_consecutive_rows_stay_apart():
    lines = group_ocr_lines([
        run("01-03 first", 0.1, 0.200),
        run("01-04 second", 0.1, 0.215),
    ])
    assert lines == ["01-03 first", "01-04 second"]


def test_a_baseline_wobble_rejoins_the_row():
    # The label and the amount in the column beside it are printed on
    # the same row but land a fraction apart; they must rejoin, or a
    # summary line loses its figure.
    lines = group_ocr_lines([
        run("(-) Withdrawals", 0.1, 0.5000),
        run("$1,234.56", 0.7, 0.5002),
    ])
    assert lines == ["(-) Withdrawals $1,234.56"]


def test_tolerance_scales_with_the_smaller_run():
    # The same drift means different things at different type sizes:
    # between two lines of 6-point print it is a row boundary, within
    # one heading it is a wobble.
    drift = 0.005
    small = [run("upper", 0.1, 0.500, height=0.004),
             run("lower", 0.5, 0.500 + drift, height=0.004)]
    assert group_ocr_lines(small) == ["upper", "lower"]
    large = [run("upper", 0.1, 0.500, height=0.020),
             run("lower", 0.5, 0.500 + drift, height=0.020)]
    assert group_ocr_lines(large) == ["upper lower"]


def test_empty_page_yields_no_lines():
    assert group_ocr_lines([]) == []


# ============================================================
# Building a recogniser
# ============================================================

@pytest.fixture
def fake_engines(monkeypatch):
    """Both recognisers, stood in for in ``sys.modules``.

    Each factory imports its engine inside the function body, so a
    stand-in injected after this module was imported is what the
    factory picks up — which is also what keeps neither engine
    imported until a document needs OCR. Yields the record of RapidOCR
    constructions, the cost the caching exists to avoid.
    """
    builds = []

    vision = types.ModuleType("ocrmac.ocrmac")
    vision.text_from_image = lambda image, language_preference=None: []
    monkeypatch.setitem(sys.modules, "ocrmac", types.ModuleType("ocrmac"))
    monkeypatch.setitem(sys.modules, "ocrmac.ocrmac", vision)

    class RapidOCR:
        def __init__(self):
            builds.append(self)

        def __call__(self, array):
            return None

    rapidocr = types.ModuleType("rapidocr")
    rapidocr.RapidOCR = RapidOCR
    monkeypatch.setitem(sys.modules, "rapidocr", rapidocr)
    numpy = types.ModuleType("numpy")
    numpy.asarray = lambda image: image
    monkeypatch.setitem(sys.modules, "numpy", numpy)

    # The readers are cached per process, so a reader closed over a
    # stand-in must not outlive the test that installed it.
    _vision_reader.cache_clear()
    _rapidocr_reader.cache_clear()
    yield builds
    _vision_reader.cache_clear()
    _rapidocr_reader.cache_clear()


def test_a_reader_is_built_once_per_process(fake_engines):
    # RapidOCR builds its inference sessions in its constructor, and a
    # cold pass over an archive OCRs many documents in one process, so
    # the engine is built once rather than once per document.
    assert _rapidocr_reader() is _rapidocr_reader()
    assert len(fake_engines) == 1
    assert _vision_reader() is _vision_reader()


def test_each_engine_keeps_its_own_reader(fake_engines):
    # The cache is per adapter, so alternating the engine between calls
    # still reaches the recogniser that was named.
    assert _READERS[resolve_ocr_engine(OCR_ENGINE_VISION)]() is _vision_reader()
    assert _READERS[resolve_ocr_engine(OCR_ENGINE_RAPIDOCR)]() is _rapidocr_reader()
    assert _vision_reader() is not _rapidocr_reader()
