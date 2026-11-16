"""A PDF's text and words, extracted once and cached by content.

Extraction is the slow half of reading a statement, and it depends only on
the document's bytes and on the engine that reads them. Parsing depends on
the parser's code. Kept apart, each can change without redoing the other:
a parser edit re-parses from cached text, and an engine upgrade
re-extracts.

The five extractors each return a ``collectorkit.words.Extracted``, one
``Page`` per page of the document:

* :func:`pdfium` reads PDFium's text layer, and optionally its characters
  merged into words;
* :func:`pdfplumber` reads pdfplumber's text and words, with the keyword
  arguments a parser passes to ``extract_text`` and ``extract_words``;
* :func:`pdftotext_layout` reads poppler's ``pdftotext -layout``;
* :func:`pdftotext_bbox` reads poppler's ``pdftotext -bbox`` word boxes;
* :func:`ocr` rasters each page and reads it with a recogniser.

Each one makes the calls the parsers make today, with the same arguments,
so its text is the text those parsers read. ``Extracted.text()`` joins the
pages the way the legacy extractor of that engine joined them.

:func:`cached` keys an extraction on the document's hash, the engine, the
extractor's arguments, the engine's versions and the extractor's own code.
A parser's fingerprint therefore needs none of them.

The rule for fingerprints: a loader imports this module and a parser
never does, so an edit here re-extracts by the key and re-parses nothing.
The engine packages are imported inside the extractors, so importing this
module never needs them.
"""
from __future__ import annotations

import argparse
import ast
import functools
import gzip
import hashlib
import html
import json
import logging
import os
import re
import subprocess
import sys
import time
from collections.abc import Callable, Mapping, Sequence
from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _dist_version
from pathlib import Path

from collectorkit import pdf as _kitpdf
from collectorkit import pdftotext as _pdftotext
from collectorkit import words as _words
from collectorkit.words import Extracted, Page, Word, params_key

log = logging.getLogger("collectorkit.extract")


# ============================================================
# The extractors
# ============================================================

def pdfium(path, *, words: bool = False) -> Extracted:
    """Every page's text from PDFium's text layer.

    The text is what ``collectorkit.pdf.extract_text_pdfium`` returns,
    page by page: ``PdfTextPage.get_text_bounded()``, an empty page as
    "". With ``words``, each page also carries the words schwab-web's
    tax-form reader builds from the characters (:func:`_pdfium_words`),
    read with no arguments: ``page.words()``."""
    import pypdfium2 as pdfium_mod
    import pypdfium2.raw as pdfium_c

    pages: list[Page] = []
    pdf = pdfium_mod.PdfDocument(str(path))
    try:
        for i in range(len(pdf)):
            page = pdf[i]
            try:
                textpage = page.get_textpage()
                try:
                    text = textpage.get_text_bounded() or ""
                    sets = (((params_key({}),
                              _pdfium_words(textpage, pdfium_c)),)
                            if words else ())
                finally:
                    textpage.close()
            finally:
                page.close()
            pages.append(Page(text, sets))
    finally:
        pdf.close()
    return Extracted("pdfium", tuple(pages))


def _pdfium_words(textpage, pdfium_c) -> tuple[Word, ...]:
    """A page's characters merged into words. A word ends at whitespace,
    at a change of line, or where the next character starts left of it.
    The rule of schwab-web's ``tax_form_parsers._pdf_words``."""
    words: list[list] = []
    cur = None
    for c in range(textpage.count_chars()):
        ch = chr(pdfium_c.FPDFText_GetUnicode(textpage, c))
        if ch.isspace() or not ch.isprintable():
            cur = None
            continue
        left, bottom, right, top = textpage.get_charbox(c)
        if cur is not None and (left < cur[2] - 1 or bottom > cur[4]
                                or top < cur[3]):
            cur = None
        if cur is None:
            cur = [ch, left, right, bottom, top]
            words.append(cur)
        else:
            cur[0] += ch
            cur[2], cur[3], cur[4] = (max(cur[2], right),
                                      min(cur[3], bottom), max(cur[4], top))
    return tuple(Word(w[0], w[1], w[2], w[4], w[3]) for w in words)


def pdfplumber(path, *, text_params: Mapping | None = None,
               word_params: Sequence[Mapping] = ()) -> Extracted:
    """Every page's text, and words, from pdfplumber.

    ``text_params`` are the keyword arguments of ``page.extract_text``
    (none for ``collectorkit.pdf.extract_text_pdfplumber``, ``x_tolerance=2``
    for ubs-web). Each mapping in ``word_params`` is one call of
    ``page.extract_words``, read back with ``page.words(**mapping)``."""
    import pdfplumber as plumber

    text_kw = dict(text_params or {})
    word_calls = [(params_key(p), dict(p)) for p in word_params]
    pages: list[Page] = []
    with plumber.open(str(path)) as pdf:
        for page in pdf.pages:
            text = page.extract_text(**text_kw) or ""
            sets = tuple(
                (key, tuple(Word(w["text"], w["x0"], w["x1"], w["top"],
                                 w["bottom"])
                            for w in page.extract_words(**kw)))
                for key, kw in word_calls)
            pages.append(Page(text, sets))
    return Extracted("pdfplumber", tuple(pages))


def pdftotext_layout(path, *, timeout: float | None = None) -> Extracted:
    """``pdftotext -layout``, one page per form feed.

    pdftotext ends every page with a form feed, so the last piece is what
    follows the last one (normally ""). Keeping it makes ``text()`` the
    exact string ``collectorkit.pdftotext.layout_text`` returns. Raises
    that module's ExtractionError and ToolMissing."""
    text = _pdftotext.layout_text(path, timeout=timeout)
    return Extracted("pdftotext-layout",
                     tuple(Page(piece) for piece in text.split("\f")),
                     sep="\f")


# `pdftotext -bbox` XHTML, as carta's K-1 reader matches it.
_BBOX_PAGE = re.compile(r"<page\b[^>]*>(.*?)</page>", re.S)
_BBOX_WORD = re.compile(r'<word xMin="([\d.]+)" yMin="([\d.]+)" '
                        r'xMax="([\d.]+)" yMax="([\d.]+)">(.*?)</word>')


def pdftotext_bbox(path, *, timeout: float | None = None) -> Extracted:
    """``pdftotext -bbox`` word boxes, one page per ``<page>``.

    The words are carta's (``k1.bbox_xhtml`` then ``k1.bbox_pages``) with
    the box's bottom kept as well. A page's text is its words joined by
    single spaces, the text carta tests a page by. Raises
    ``collectorkit.pdftotext``'s ExtractionError and ToolMissing."""
    xhtml = _bbox_xhtml(path, timeout=timeout)
    pages = []
    for body in _BBOX_PAGE.findall(xhtml):
        words = tuple(Word(html.unescape(t), float(x0), float(x1),
                           float(y0), float(y1))
                      for x0, y0, x1, y1, t in _BBOX_WORD.findall(body))
        pages.append(Page(" ".join(w.text for w in words),
                          ((params_key({}), words),)))
    return Extracted("pdftotext-bbox", tuple(pages))


def _bbox_xhtml(path, *, timeout: float | None = None) -> str:
    """One PDF as ``pdftotext -bbox`` XHTML, with the failures of
    ``collectorkit.pdftotext.layout_text``."""
    try:
        proc = subprocess.run(["pdftotext", "-bbox", str(path), "-"],
                              capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise _pdftotext.ExtractionError(
            f"pdftotext timed out after {timeout:g}s") from exc
    except OSError as exc:
        raise _pdftotext.ToolMissing(
            f"pdftotext (poppler-utils) cannot be run: {exc}") from exc
    if proc.returncode != 0:
        detail = proc.stderr.decode("utf-8", errors="replace").strip()[:200]
        raise _pdftotext.ExtractionError(
            f"pdftotext exited {proc.returncode}: {detail}")
    return proc.stdout.decode("utf-8", errors="replace")


def ocr(path, *, scale=_kitpdf.OCR_SCALE,
        line_tolerance=_kitpdf.OCR_LINE_TOLERANCE, engine=None) -> Extracted:
    """Every page rastered with PDFium and read by a recogniser: the calls
    of ``collectorkit.pdf.extract_text_ocr``, kept per page.

    A page's text is its lines joined by newlines. ``text()`` leaves out
    a page with no text, as the legacy extractor adds nothing for a page
    with no line. The one case the two still differ is a page whose only
    line is empty. The engine name carries the recogniser, since the two
    read the same raster differently."""
    import pypdfium2 as pdfium_mod

    name = _kitpdf.resolve_ocr_engine(engine)
    reader = _kitpdf._READERS[name]()
    pages: list[Page] = []
    pdf = pdfium_mod.PdfDocument(str(path))
    try:
        for i in range(len(pdf)):
            page = pdf[i]
            try:
                image = page.render(scale=scale).to_pil()
            finally:
                page.close()
            lines = _kitpdf.group_ocr_lines(reader(image), line_tolerance)
            pages.append(Page("\n".join(lines)))
    finally:
        pdf.close()
    return Extracted(f"ocr-{name}", tuple(pages), skip_empty=True)


# ============================================================
# The extraction key and the cache
# ============================================================

# Versions are read once per process: a load asks for a key per document.

@functools.cache
def _dists(*names: str) -> tuple[str, ...]:
    out = []
    for name in names:
        try:
            out.append(f"dist:{name}={_dist_version(name)}")
        except PackageNotFoundError:
            out.append(f"dist:{name}=unavailable")
    return tuple(out)


@functools.cache
def _tool(*argv: str) -> tuple[str, ...]:
    """What a binary says its version is, as one line. A missing or
    failing binary is "unavailable", which is itself a value."""
    try:
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return (f"tool:{argv[0]}=unavailable",)
    banner = (proc.stdout + proc.stderr).strip().splitlines()
    return (f"tool:{argv[0]}={banner[0].strip() if banner else 'unavailable'}",)


def _ocr_versions(params: Mapping) -> tuple[str, ...]:
    name = _kitpdf.resolve_ocr_engine(params.get("engine"))
    stack = {_kitpdf.OCR_ENGINE_VISION: ("ocrmac",),
             _kitpdf.OCR_ENGINE_RAPIDOCR: ("rapidocr", "onnxruntime")}[name]
    return (f"recogniser:{name}", *_dists("pypdfium2", *stack))


# What each engine's output depends on besides the document and the
# arguments: the installed packages or binaries that read it.
_VERSIONS: dict[str, Callable[[Mapping], tuple[str, ...]]] = {
    "pdfium": lambda _params: _dists("pypdfium2"),
    "pdfplumber": lambda _params: _dists("pdfplumber", "pdfminer.six"),
    "pdftotext_layout": lambda _params: _tool("pdftotext", "-v"),
    "pdftotext_bbox": lambda _params: _tool("pdftotext", "-v"),
    "ocr": _ocr_versions,
}

# The frozen kit modules whose code an extractor runs.
_CODE_FILES = {
    "pdftotext_layout": (_pdftotext,),
    "pdftotext_bbox": (_pdftotext,),
    "ocr": (_kitpdf,),
}

# Arguments that bound an extraction's time but not its result.
_NOT_IN_KEY = frozenset({"timeout"})


def _unwrap(extractor) -> tuple[str, dict]:
    """(extractor name, keyword arguments) of one of this module's
    extractors, called bare or through ``functools.partial``."""
    func, params = extractor, {}
    if isinstance(extractor, functools.partial):
        if extractor.args:
            raise ValueError("pass an extractor's arguments by keyword")
        func, params = extractor.func, dict(extractor.keywords)
    name = getattr(func, "__name__", None)
    if name not in _VERSIONS or globals().get(name) is not func:
        raise ValueError(f"not an extractor of collectorkit.extract: "
                         f"{extractor!r}")
    return name, params


def _strip_docstrings(tree: ast.AST) -> None:
    for node in ast.walk(tree):
        if isinstance(node, (ast.Module, ast.ClassDef, ast.FunctionDef,
                             ast.AsyncFunctionDef)):
            body = node.body
            if (body and isinstance(body[0], ast.Expr)
                    and isinstance(body[0].value, ast.Constant)
                    and isinstance(body[0].value.value, str)):
                node.body = body[1:]


def _normalized(tree: ast.AST) -> str:
    _strip_docstrings(tree)
    return ast.dump(tree, annotate_fields=True, include_attributes=False)


# The definitions a cache entry is made of: the document types, which
# the codec writes, and the codec itself.
_ENTRY_TYPES = ("Word", "Page", "Extracted")
_CODEC = ("_encode", "_decode")


@functools.cache
def _code_version(name: str) -> str:
    """A hash of the code an extractor runs and of the cache entry's
    form: the extractor and what it reaches in this module, the codec,
    the document types in ``collectorkit.words``, and the frozen kit
    modules it calls into. The geometry in ``collectorkit.words`` is
    reached by none of them, so an edit to it re-extracts nothing."""
    parts = [_code_hash(Path(__file__).read_bytes(), (name, *_CODEC)),
             _code_hash(Path(_words.__file__).read_bytes(), _ENTRY_TYPES)]
    for module in _CODE_FILES.get(name, ()):
        parts.append(_normalized(ast.parse(Path(module.__file__).read_bytes())))
    return hashlib.sha256("\n".join(parts).encode("utf-8")).hexdigest()


def _code_hash(source: bytes, roots: Sequence[str]) -> str:
    """A hash of the top-level definitions ``roots`` in ``source`` and of
    every top-level definition they reach by name. Comments and
    docstrings do not count, as in ``collectorkit.srcfp``."""
    tree = ast.parse(source)
    defs: dict[str, ast.AST] = {}
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.ClassDef)):
            defs[node.name] = node
        elif isinstance(node, (ast.Assign, ast.AnnAssign)):
            targets = node.targets if isinstance(node, ast.Assign) \
                else [node.target]
            for target in targets:
                for leaf in ast.walk(target):
                    if isinstance(leaf, ast.Name):
                        defs[leaf.id] = node
    reached: dict[int, ast.AST] = {}
    todo = list(roots)
    while todo:
        node = defs.get(todo.pop())
        if node is None or id(node) in reached:
            continue
        reached[id(node)] = node
        todo += [leaf.id for leaf in ast.walk(node)
                 if isinstance(leaf, ast.Name)]
    parts = sorted(_normalized(node) for node in reached.values())
    return hashlib.sha256("\n".join(parts).encode("utf-8")).hexdigest()


def extraction_key(extractor) -> str:
    """The cache key of an extractor, apart from the document: the
    engine, its arguments, the versions of what reads the bytes, the
    Python minor version (pdfminer is pure Python, and PDFium's words
    test characters with ``str`` predicates) and the extractor's own
    code. 32 hex characters."""
    name, params = _unwrap(extractor)
    kept = {k: v for k, v in params.items() if k not in _NOT_IN_KEY}
    parts = [f"engine:{name}", f"params:{params_key(kept)}",
             *_VERSIONS[name](params),
             "py:{}.{}".format(*sys.version_info[:2]),
             f"code:{_code_version(name)}"]
    return hashlib.sha256("\n".join(parts).encode("utf-8")).hexdigest()[:32]


def _encode(extracted: Extracted) -> dict:
    return {
        "engine": extracted.engine,
        "sep": extracted.sep,
        "skip_empty": extracted.skip_empty,
        "pages": [{"text": p.text,
                   "words": [[key, [list(w) for w in ws]]
                             for key, ws in p.word_sets]}
                  for p in extracted.pages],
    }


def _decode(data: dict) -> Extracted:
    return Extracted(
        data["engine"],
        tuple(Page(p["text"],
                   tuple((key, tuple(Word(*w) for w in ws))
                         for key, ws in p["words"]))
              for p in data["pages"]),
        data["sep"], data["skip_empty"])


def _dump(extracted: Extracted) -> bytes:
    """One cache entry's bytes. gzip with no timestamp, so the same
    extraction always writes the same bytes."""
    raw = json.dumps(_encode(extracted), ensure_ascii=False,
                     separators=(",", ":")).encode("utf-8")
    return gzip.compress(raw, mtime=0)


# Where a secret lives: a cache never goes there.
_SECRETS_DIRS = (Path.home() / ".secrets", Path("/secrets"))
_warned_dirs: set[Path] = set()


def cache_root(collector: str) -> Path:
    """A collector's cache root: ``$XDG_CACHE_HOME`` (or ``~/.cache``)
    ``/wealthdb/<collector>``. Derived data, kept outside bronze and
    outside the secrets directory."""
    base = os.environ.get("XDG_CACHE_HOME") or (Path.home() / ".cache")
    return Path(base) / "wealthdb" / collector


def usable_cache_dir(directory) -> Path | None:
    """``directory``, or None when there is none or it lies inside a
    secrets directory. A cache never stops a load, so a refused
    directory leaves the caller cache-less, with one warning."""
    if directory is None:
        return None
    path = Path(directory)
    resolved = path.expanduser().resolve()
    for secrets in _SECRETS_DIRS:
        if resolved == secrets or secrets in resolved.parents:
            if resolved not in _warned_dirs:
                _warned_dirs.add(resolved)
                log.warning("cache: %s is inside a secrets directory; "
                            "running without a cache", path)
            return None
    return path


def cached(extractor, path, sha256: str, cache_dir) -> Extracted:
    """``extractor(path)``, read from ``cache_dir`` when this document was
    extracted the same way before, and stored there when not.

    An extraction that raises stores nothing, so the next run tries
    again. The cache only ever saves time: an unreadable entry is read
    again from the PDF, and a directory that cannot be written leaves
    the result unstored (logged at debug). With ``cache_dir`` None it is
    a plain call."""
    directory = usable_cache_dir(cache_dir)
    if directory is None:
        return extractor(path)
    entry = directory / f"{sha256}.{extraction_key(extractor)}.json.gz"
    if entry.is_file():
        try:
            return _decode(json.loads(gzip.decompress(entry.read_bytes())))
        except (OSError, EOFError, ValueError, KeyError, TypeError) as exc:
            log.debug("extract-cache: unreadable %s: %s", entry, exc)
    extracted = extractor(path)
    tmp = entry.with_name(entry.name + ".tmp")
    try:
        directory.mkdir(parents=True, exist_ok=True)
        tmp.write_bytes(_dump(extracted))
        tmp.replace(entry)
    except OSError as exc:
        log.debug("extract-cache: could not write %s: %s", entry, exc)
        tmp.unlink(missing_ok=True)
    return extracted


# ============================================================
# Measure first: how large a document directory's cache would be
# ============================================================

# ubs-web's reads: page text at x_tolerance=2, the transaction list's words
# at x_tolerance=2, the ledger's at x_tolerance=1.5.
UBS_WEB_PARAMS = {
    "text_params": {"x_tolerance": 2},
    "word_params": ({"x_tolerance": 2},
                    {"x_tolerance": 1.5, "keep_blank_chars": False}),
}


def measure(doc_dir, extractor) -> dict:
    """What caching every PDF under ``doc_dir`` with ``extractor`` would
    store, in counts and bytes only: no text, no names, no paths. Writes
    nothing. ``word_bytes`` is the JSON of the words alone, and
    ``entry_bytes`` the gzip entries :func:`cached` would write."""
    out = {"documents": 0, "failed": 0, "pages": 0, "words": {},
           "text_bytes": 0, "word_bytes": 0, "entry_bytes": 0,
           "largest_entry_bytes": 0, "seconds": 0.0}
    for path in sorted(Path(doc_dir).rglob("*.pdf")):
        started = time.monotonic()
        try:
            extracted = extractor(path)
        except Exception:                       # noqa: BLE001 — counted
            out["failed"] += 1
            continue
        finally:
            out["seconds"] += time.monotonic() - started
        out["documents"] += 1
        out["pages"] += len(extracted.pages)
        for page in extracted.pages:
            out["text_bytes"] += len(page.text.encode("utf-8"))
            for key, words in page.word_sets:
                out["words"][key] = out["words"].get(key, 0) + len(words)
                out["word_bytes"] += len(json.dumps(
                    [list(w) for w in words], ensure_ascii=False,
                    separators=(",", ":")).encode("utf-8"))
        size = len(_dump(extracted))
        out["entry_bytes"] += size
        out["largest_entry_bytes"] = max(out["largest_entry_bytes"], size)
    out["seconds"] = round(out["seconds"], 1)
    return out


def _main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(
        prog="python -m collectorkit.extract",
        description="Report what caching one directory's PDFs would store, "
                    "in counts and bytes only.")
    sub = p.add_subparsers(dest="verb", required=True)
    m = sub.add_parser("measure")
    m.add_argument("doc_dir", type=Path)
    m.add_argument("--engine", choices=sorted(_VERSIONS),
                   default="pdfplumber")
    m.add_argument("--params", type=json.loads, default=None,
                   help="the extractor's keyword arguments as a JSON "
                        "object; default for pdfplumber: ubs-web's reads")
    args = p.parse_args(argv)
    params = args.params
    if params is None:
        params = UBS_WEB_PARAMS if args.engine == "pdfplumber" else {}
    extractor = functools.partial(globals()[args.engine], **params)
    print(json.dumps(measure(args.doc_dir, extractor), indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(_main(sys.argv[1:]))
