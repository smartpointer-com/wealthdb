"""Tests for collectorkit.extract: the extractors, the key, the cache.

The engines are not installed in the kit's venv, so each extractor runs
against a stand-in engine that records its calls. The legacy extractor it
replaces runs against the same stand-in, and the two must make the same
calls and return the same text and words: equal by construction.
"""
from __future__ import annotations

import functools
import gzip
import json
import subprocess
import sys
import types
from pathlib import Path

import collector_modules
import pytest

from collectorkit import extract
from collectorkit import pdf as kitpdf
from collectorkit import pdftotext
from collectorkit import words
from collectorkit.words import Extracted, Page, Word, rows

SHA = "ab" * 32


# ------------------------------------------------------------------
# pdfplumber
# ------------------------------------------------------------------

class _PlumberPage:
    def __init__(self, text, words, calls):
        self._text, self._words, self._calls = text, words, calls

    def extract_text(self, **kw):
        self._calls.append(("text", kw))
        return self._text

    def extract_words(self, **kw):
        self._calls.append(("words", kw))
        return [dict(w) for w in self._words]


PLUMBER_PAGES = [
    ("Trade date Booking text\n01.02.2025 Buy", [
        {"text": "Trade", "x0": 10.0, "x1": 30.5, "top": 100.25,
         "bottom": 108.0, "doctop": 100.25, "upright": True},
        {"text": "date", "x0": 31.0, "x1": 45.0, "top": 100.25,
         "bottom": 108.0, "doctop": 100.25, "upright": True}]),
    (None, []),
    ("last page", [{"text": "last", "x0": 1 / 3, "x1": 2 / 3, "top": 0.1,
                    "bottom": 0.7}]),
]


@pytest.fixture
def plumber(monkeypatch):
    calls: list = []

    class _Doc:
        def __init__(self, path):
            calls.append(("open", str(path)))
            self.pages = [_PlumberPage(t, w, calls) for t, w in PLUMBER_PAGES]

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    fake = types.ModuleType("pdfplumber")
    fake.open = _Doc
    monkeypatch.setitem(sys.modules, "pdfplumber", fake)
    return fake, calls


def test_pdfplumber_text_is_the_kits_legacy_text(plumber, tmp_path):
    _, calls = plumber
    legacy = kitpdf.extract_text_pdfplumber(tmp_path / "a.pdf")
    legacy_calls = list(calls)
    calls.clear()
    assert extract.pdfplumber(tmp_path / "a.pdf").text() == legacy
    assert calls == legacy_calls


def test_pdfplumber_is_ubs_webs_reads(plumber, tmp_path):
    fake, calls = plumber
    ubs = collector_modules.load("ubs-web", "pdf_parsers", ("pdfplumber",))
    ubs.pdfplumber = fake
    path = tmp_path / "a.pdf"
    legacy = ubs._pdf_text(path)
    first_two = ubs._pdf_text(path, 2)
    calls.clear()
    doc = extract.pdfplumber(path, **extract.UBS_WEB_PARAMS)
    assert doc.text() == legacy
    assert "\n".join(p.text for p in doc.pages[:2]) == first_two
    assert ("text", {"x_tolerance": 2}) in calls
    assert ("words", {"x_tolerance": 2}) in calls
    assert ("words", {"x_tolerance": 1.5, "keep_blank_chars": False}) in calls
    for page, (_, expected) in zip(doc.pages, PLUMBER_PAGES, strict=True):
        assert page.words(x_tolerance=2) == tuple(
            Word(w["text"], w["x0"], w["x1"], w["top"], w["bottom"])
            for w in expected)


def test_pdfplumber_words_are_fidelity_1099s(plumber, tmp_path):
    fidelity = collector_modules.load("fidelity-web", "pdf_parsers_1099")
    doc = extract.pdfplumber(tmp_path / "a.pdf", word_params=({},))
    with sys.modules["pdfplumber"].open("x") as pdf:
        legacy = [fidelity._page_lines(p) for p in pdf.pages]
    mine = [[[fidelity.Word(w.text, w.x0, w.x1) for w in row.words]
             for row in rows(page.words(), fidelity._LINE_TOLERANCE)]
            for page in doc.pages]
    assert mine == legacy


# ------------------------------------------------------------------
# pdfium
# ------------------------------------------------------------------

# Each page: characters as (char, left, bottom, right, top), PDF points
# from the bottom-left. A space ends a word, so does a step left, so does
# a change of line.
PDFIUM_PAGES = [
    ("Total Proceeds\n12.50", [
        ("T", 10, 700, 15, 710), ("o", 15, 700, 20, 710), (" ", 20, 700, 22, 710),
        ("P", 22, 700, 27, 711), ("r", 27, 699.5, 30, 710),
        ("1", 100, 690, 104, 699), ("2", 104, 690, 108, 699),
        ("x", 50, 690, 54, 699), ("\x02", 54, 690, 55, 699),
        ("y", 55, 600, 58, 609)]),
    ("", []),
]


@pytest.fixture
def pdfium(monkeypatch):
    calls: list = []

    class _TextPage:
        def __init__(self, text, chars):
            self._text, self._chars = text, chars

        def get_text_bounded(self):
            return self._text or None

        def count_chars(self):
            return len(self._chars)

        def get_charbox(self, c):
            _, left, bottom, right, top = self._chars[c]
            return left, bottom, right, top

        def close(self):
            calls.append("textpage.close")

    class _Page:
        def __init__(self, index):
            self._index = index

        def get_textpage(self):
            return _TextPage(*PDFIUM_PAGES[self._index])

        def render(self, scale):
            calls.append(("render", scale))
            return types.SimpleNamespace(to_pil=lambda: ("image", self._index))

        def close(self):
            calls.append("page.close")

    class _Doc:
        def __init__(self, path):
            calls.append(("open", path))

        def __len__(self):
            return len(PDFIUM_PAGES)

        def __getitem__(self, i):
            return _Page(i)

        def close(self):
            calls.append("doc.close")

    fake = types.ModuleType("pypdfium2")
    fake.PdfDocument = _Doc
    raw = types.ModuleType("pypdfium2.raw")
    raw.FPDFText_GetUnicode = lambda textpage, c: ord(textpage._chars[c][0])
    fake.raw = raw
    monkeypatch.setitem(sys.modules, "pypdfium2", fake)
    monkeypatch.setitem(sys.modules, "pypdfium2.raw", raw)
    return calls


def test_pdfium_text_is_the_kits_legacy_text(pdfium, tmp_path):
    legacy = kitpdf.extract_text_pdfium(tmp_path / "a.pdf")
    assert extract.pdfium(tmp_path / "a.pdf").text() == legacy
    assert legacy == "Total Proceeds\n12.50\n"


def test_pdfium_words_are_schwab_webs(pdfium, tmp_path):
    schwab = collector_modules.load("schwab-web", "tax_form_parsers")
    legacy = schwab._pdf_words(tmp_path / "a.pdf")
    doc = extract.pdfium(tmp_path / "a.pdf", words=True)
    mine = [[(w.text, w.x0, w.x1, w.bottom, w.top) for w in page.words()]
            for page in doc.pages]
    assert mine == [[tuple(w) for w in page] for page in legacy]
    assert [w.text for w in doc.pages[0].words()] == ["To", "Pr", "12", "x", "y"]


def test_ocr_text_is_the_kits_legacy_text(pdfium, tmp_path, monkeypatch):
    lines = {0: [kitpdf.TextRun("b", 0.5, 0.25, 0.125),
                 kitpdf.TextRun("a", 0.25, 0.25, 0.125),
                 kitpdf.TextRun("c", 0.25, 0.75, 0.125)],
             1: []}
    monkeypatch.setitem(kitpdf._READERS, kitpdf.OCR_ENGINE_VISION,
                        lambda: lambda image: lines[image[1]])
    legacy = kitpdf.extract_text_ocr(tmp_path / "a.pdf", engine="vision")
    doc = extract.ocr(tmp_path / "a.pdf", engine="vision")
    assert doc.text() == legacy == "a b\nc"
    assert doc.engine == "ocr-vision"
    assert ("render", kitpdf.OCR_SCALE) in pdfium


# ------------------------------------------------------------------
# pdftotext
# ------------------------------------------------------------------

def _fake_run(stdout=b"", returncode=0, exc=None, seen=None):
    def run(argv, **kw):
        if seen is not None:
            seen.append((argv, kw))
        if exc is not None:
            raise exc
        return subprocess.CompletedProcess(argv, returncode, stdout, b"bad pdf")
    return run


def test_pdftotext_layout_is_layout_text(monkeypatch, tmp_path):
    seen: list = []
    monkeypatch.setattr(subprocess, "run",
                        _fake_run("one\f\ftwo é\f".encode(), seen=seen))
    legacy = pdftotext.layout_text(tmp_path / "a.pdf", timeout=30)
    doc = extract.pdftotext_layout(tmp_path / "a.pdf", timeout=30)
    assert doc.text() == legacy
    assert [p.text for p in doc.pages] == ["one", "", "two é", ""]
    assert seen[0] == seen[1]


BBOX = ('<html><body><doc><page width="612" height="792">'
        '<word xMin="10.5" yMin="20.25" xMax="30.0" yMax="28.0">A&amp;B</word>'
        '<word xMin="40" yMin="20.5" xMax="60" yMax="28.5">Box</word>'
        '<word xMin="1" yMin="2" xMax="3">no-ymax</word>'
        '</page><page width="612" height="792"></page></doc></body></html>')


def test_pdftotext_bbox_is_cartas(monkeypatch, tmp_path):
    carta = collector_modules.load("carta", "k1")
    seen: list = []
    monkeypatch.setattr(subprocess, "run", _fake_run(BBOX.encode(), seen=seen))
    legacy = carta.bbox_pages(carta.bbox_xhtml(tmp_path / "a.pdf", timeout=60))
    doc = extract.pdftotext_bbox(tmp_path / "a.pdf", timeout=60)
    assert seen[0] == seen[1]
    assert [[(w.x0, w.top, w.x1, w.text) for w in p.words()]
            for p in doc.pages] == [[tuple(w) for w in page] for page in legacy]
    assert [p.text for p in doc.pages] == \
        [" ".join(w.text for w in page) for page in legacy]
    assert doc.pages[0].words()[0].bottom == 28.0


@pytest.mark.parametrize("fake,error", [
    (_fake_run(returncode=1), pdftotext.ExtractionError),
    (_fake_run(exc=OSError("missing")), pdftotext.ToolMissing),
    (_fake_run(exc=subprocess.TimeoutExpired(["pdftotext"], 5)),
     pdftotext.ExtractionError),
])
def test_pdftotext_bbox_fails_as_carta_does(monkeypatch, tmp_path, fake, error):
    carta = collector_modules.load("carta", "k1")
    monkeypatch.setattr(subprocess, "run", fake)
    with pytest.raises(error) as theirs:
        carta.bbox_xhtml(tmp_path / "a.pdf", timeout=5)
    with pytest.raises(error) as ours:
        extract.pdftotext_bbox(tmp_path / "a.pdf", timeout=5)
    assert str(ours.value) == str(theirs.value)


# ------------------------------------------------------------------
# The extraction key
# ------------------------------------------------------------------

def test_the_key_names_engine_arguments_and_versions(monkeypatch):
    monkeypatch.setattr(extract, "_tool", lambda *argv: ("tool:pdftotext=1",))
    plumber = functools.partial(extract.pdfplumber, **extract.UBS_WEB_PARAMS)
    keys = {extract.extraction_key(e) for e in (
        extract.pdfium, functools.partial(extract.pdfium, words=True),
        extract.pdfplumber, plumber, extract.pdftotext_layout,
        extract.pdftotext_bbox)}
    assert len(keys) == 6
    assert all(len(k) == 32 for k in keys)
    assert extract.extraction_key(plumber) == extract.extraction_key(
        functools.partial(extract.pdfplumber, **extract.UBS_WEB_PARAMS))
    # A timeout bounds the time, not the text.
    assert extract.extraction_key(extract.pdftotext_layout) == \
        extract.extraction_key(functools.partial(extract.pdftotext_layout,
                                                 timeout=30))
    monkeypatch.setattr(extract, "_tool", lambda *argv: ("tool:pdftotext=2",))
    assert extract.extraction_key(extract.pdftotext_layout) not in keys


def test_the_key_refuses_what_is_not_an_extractor():
    with pytest.raises(ValueError):
        extract.extraction_key(len)
    with pytest.raises(ValueError):
        extract.extraction_key(functools.partial(extract.pdfium, "a.pdf"))


def _hashes(source: str) -> dict[str, str]:
    return {name: extract._code_hash(source.encode(), (name, *extract._CODEC))
            for name in ("pdfium", "pdfplumber", "pdftotext_bbox")}


def test_the_code_hash_follows_what_an_extractor_runs():
    source = Path(extract.__file__).read_text(encoding="utf-8")
    base = _hashes(source)
    assert len(set(base.values())) == 3

    def edited(old, new):
        assert old in source
        return _hashes(source.replace(old, new, 1))

    # Comments and docstrings move nothing.
    assert edited("# The frozen kit modules",
                  "# A comment.\n# The frozen kit modules") == base
    assert edited('"""Every page\'s text from PDFium',
                  '"""Each page\'s text from PDFium') == base
    # A helper moves the extractor that calls it, and only that one.
    moved = edited("left < cur[2] - 1", "left < cur[2] - 2")
    assert moved["pdfium"] != base["pdfium"]
    assert moved["pdfplumber"] == base["pdfplumber"]
    moved = edited('xMax="([\\d.]+)" yMax', 'xMax="([\\d.]*)" yMax')
    assert moved["pdftotext_bbox"] != base["pdftotext_bbox"]
    assert moved["pdfium"] == base["pdfium"]
    # The codec moves every one.
    moved = edited('"skip_empty": extracted.skip_empty,', "")
    assert all(moved[k] != base[k] for k in base)


def test_the_code_hash_follows_the_entry_types_not_the_geometry():
    source = Path(words.__file__).read_text(encoding="utf-8")

    def entry_hash(text):
        return extract._code_hash(text.encode(), extract._ENTRY_TYPES)

    base = entry_hash(source)
    for old, new in (("if abs(wy - y) > limit:", "if abs(wy - y) >= limit:"),
                     ('_CELL_RULES = ("nearest"', '_CELL_RULES = ("closest"')):
        assert old in source
        assert entry_hash(source.replace(old, new, 1)) == base
    old = "skip_empty: bool = False"
    assert old in source
    assert entry_hash(source.replace(old, "skip_empty: bool = True", 1)) != base


# ------------------------------------------------------------------
# The cache
# ------------------------------------------------------------------

@pytest.fixture
def counted(monkeypatch):
    """Stand in for the pdfium extractor, counting its calls."""
    calls: list = []

    def pdfium(path, *, words=False):
        calls.append(path)
        if Path(path).name == "broken.pdf":
            raise pdftotext.ExtractionError("unreadable")
        return Extracted("pdfium", (
            Page("text é", (("{}", (Word("w", 0.1, 0.2, 1 / 3, 2.5),)),)),
            Page("")))

    monkeypatch.setattr(extract, "pdfium", pdfium)
    monkeypatch.setattr(extract, "_dists", lambda *names: ("dist:x=1",))
    return calls


def test_cached_extracts_once(tmp_path, counted):
    first = extract.cached(extract.pdfium, "a.pdf", SHA, tmp_path / "c")
    second = extract.cached(extract.pdfium, "a.pdf", SHA, tmp_path / "c")
    assert first == second
    assert counted == ["a.pdf"]
    (entry,) = (tmp_path / "c").iterdir()
    assert entry.name.startswith(SHA + ".") and entry.name.endswith(".json.gz")
    assert entry.read_bytes() == extract._dump(first)


def test_an_error_is_not_cached(tmp_path, counted):
    for _ in range(2):
        with pytest.raises(pdftotext.ExtractionError):
            extract.cached(extract.pdfium, "broken.pdf", SHA, tmp_path)
    assert len(counted) == 2
    assert not list(tmp_path.iterdir())


def test_an_unreadable_entry_is_extracted_again(tmp_path, counted):
    extract.cached(extract.pdfium, "a.pdf", SHA, tmp_path)
    (entry,) = tmp_path.iterdir()
    entry.write_bytes(gzip.compress(b"{")[:-3])
    again = extract.cached(extract.pdfium, "a.pdf", SHA, tmp_path)
    assert again.pages[0].text == "text é"
    assert len(counted) == 2


def test_no_or_a_refused_directory_is_a_plain_call(tmp_path, counted,
                                                   monkeypatch):
    extract.cached(extract.pdfium, "a.pdf", SHA, None)
    monkeypatch.setattr(extract, "_SECRETS_DIRS", (tmp_path,))
    extract.cached(extract.pdfium, "a.pdf", SHA, tmp_path / "cache")
    assert len(counted) == 2
    assert not list(tmp_path.iterdir())


def test_the_cache_root_is_xdg(monkeypatch, tmp_path):
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
    assert extract.cache_root("ubs-web") == tmp_path / "wealthdb" / "ubs-web"


def test_measure_counts_and_writes_nothing(tmp_path, counted):
    docs = tmp_path / "docs"
    (docs / "sub").mkdir(parents=True)
    for name in ("a.pdf", "sub/b.pdf", "broken.pdf", "c.txt"):
        (docs / name).write_bytes(b"%PDF")
    before = sorted(p.name for p in docs.rglob("*"))
    out = extract.measure(docs, extract.pdfium)
    assert sorted(p.name for p in docs.rglob("*")) == before
    assert (out["documents"], out["failed"], out["pages"]) == (2, 1, 4)
    assert out["words"] == {"{}": 2}
    assert out["text_bytes"] == 2 * len("text é".encode())
    assert out["entry_bytes"] > 0
    assert "text" not in json.dumps(out).replace("text_bytes", "")
