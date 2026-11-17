"""A PDF's text and words, extracted once and cached by content.

Extraction is the slow half of reading a statement, and it depends only on
the document's bytes and on the engine that reads them. Parsing depends on
the parser's code. Kept apart, each can change without redoing the other:
a parser edit re-parses from cached text, and an engine upgrade
re-extracts.

The four extractors each return a ``collectorkit.words.Extracted``, one
``Page`` per page of the document:

* :func:`pdfium` reads PDFium's text layer, and optionally its characters
  merged into words;
* :func:`pdfplumber` reads pdfplumber's text and words, with the keyword
  arguments a parser passes to ``extract_text`` and ``extract_words``;
* :func:`pdftotext_layout` reads poppler's ``pdftotext -layout``;
* :func:`pdftotext_bbox` reads poppler's ``pdftotext -bbox`` word boxes.

Each one makes the calls, with the same arguments, of the whole-document
extractor of that engine in ``collectorkit.pdf`` or
``collectorkit.pdftotext``, so ``Extracted.text()`` is the string that
extractor returns.

:func:`cached` keys an extraction on the document's hash, the engine, the
extractor's arguments, the engine's versions and the extractor's own code.
A parser's fingerprint therefore needs none of them.

The rule for fingerprints: a loader imports this module and a parser
never does, so an edit here re-extracts by the key and re-parses nothing.
The engine packages are imported inside the extractors, so importing this
module never needs them.
"""
from __future__ import annotations

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
from collections.abc import Callable, Mapping, Sequence
from pathlib import Path

from collectorkit import srcfp
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
    "". With ``words``, each page also carries its characters merged into
    words (:func:`_pdfium_words`), read with no arguments:
    ``page.words()``."""
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
    at a change of line, or where the next character starts left of the
    one before."""
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
    (none for ``collectorkit.pdf.extract_text_pdfplumber``). Each mapping
    in ``word_params`` is one call of ``page.extract_words``, read back
    with ``page.words(**mapping)``."""
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


# `pdftotext -bbox` XHTML: its pages and word boxes.
_BBOX_PAGE = re.compile(r"<page\b[^>]*>(.*?)</page>", re.S)
_BBOX_WORD = re.compile(r'<word xMin="([\d.]+)" yMin="([\d.]+)" '
                        r'xMax="([\d.]+)" yMax="([\d.]+)">(.*?)</word>')


def pdftotext_bbox(path, *, timeout: float | None = None) -> Extracted:
    """``pdftotext -bbox`` word boxes, one page per ``<page>``.

    Every word box, its bottom included. A page's text is its words
    joined by single spaces. Raises ``collectorkit.pdftotext``'s
    ExtractionError and ToolMissing."""
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


# ============================================================
# The extraction key and the cache
# ============================================================

# Versions are read once per process: a load asks for a key per document.

@functools.cache
def _dists(*names: str) -> tuple[str, ...]:
    return tuple(f"dist:{name}={srcfp.dist_version(name)}" for name in names)


@functools.cache
def _tool(*argv: str) -> tuple[str, ...]:
    """What a binary says its version is, as one line
    (``collectorkit.srcfp.tool_version``)."""
    return (f"tool:{argv[0]}={srcfp.tool_version(argv)}",)


# What each engine's output depends on besides the document and the
# arguments: the installed packages or binaries that read it.
_VERSIONS: dict[str, Callable[[Mapping], tuple[str, ...]]] = {
    "pdfium": lambda _params: _dists("pypdfium2"),
    "pdfplumber": lambda _params: _dists("pdfplumber", "pdfminer.six"),
    "pdftotext_layout": lambda _params: _tool("pdftotext", "-v"),
    "pdftotext_bbox": lambda _params: _tool("pdftotext", "-v"),
}

# The frozen kit modules whose code an extractor runs.
_CODE_FILES = {
    "pdftotext_layout": (_pdftotext,),
    "pdftotext_bbox": (_pdftotext,),
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


def _normalized(tree: ast.AST) -> str:
    srcfp.strip_docstrings(tree)
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
    """A collector's cache root: ``$WEALTHDB_CACHE_DIR`` when set, else
    ``$XDG_CACHE_HOME`` (or ``~/.cache``) ``/wealthdb/<collector>``.
    Derived data, kept outside bronze and outside the secrets directory.

    ``WEALTHDB_CACHE_DIR`` names the root itself. The Docker wrappers set
    it to the in-container mount of the collector's host cache dir, the
    same ``~/.cache/wealthdb/<collector>``, so a cache outlives the
    container."""
    override = os.environ.get("WEALTHDB_CACHE_DIR")
    if override:
        return Path(override)
    base = os.environ.get("XDG_CACHE_HOME") or (Path.home() / ".cache")
    return Path(base) / "wealthdb" / collector


def default_dir(collector: str) -> Path:
    """A collector's extraction-cache directory: ``extract`` under its
    cache root (:func:`cache_root`), beside the parse cache
    (``collectorkit.parsecache.default_dir``)."""
    return cache_root(collector) / "extract"


def make_private_dir(directory) -> None:
    """Create `directory` and each missing parent, mode 0700 whatever the
    umask: a cache holds what a statement says. A directory that exists
    keeps its mode. Raises OSError when one cannot be made."""
    path = Path(directory)
    missing = []
    while not path.exists() and path.parent != path:
        missing.append(path)
        path = path.parent
    for d in reversed(missing):
        try:
            d.mkdir(mode=0o700)
        except FileExistsError:
            continue                    # a concurrent load made it
        os.chmod(d, 0o700)


def usable_cache_dir(directory) -> Path | None:
    """``directory``, or None when there is none or it lies inside a
    secrets directory. A cache never stops a load, so a refused
    directory leaves the caller cache-less, with one warning. The
    secrets directories are compared resolved, so a symlinked one is
    refused too."""
    if directory is None:
        return None
    path = Path(directory)
    resolved = path.expanduser().resolve()
    for secrets in {d.resolve() for d in _SECRETS_DIRS}:
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
        make_private_dir(directory)
        tmp.write_bytes(_dump(extracted))
        tmp.replace(entry)
    except OSError as exc:
        log.debug("extract-cache: could not write %s: %s", entry, exc)
        tmp.unlink(missing_ok=True)
    return extracted


# A cache entry: `<sha>.<extraction key>.json.gz`.
_ENTRY_RE = re.compile(r"^(?P<sha>[^.]+)\.(?P<key>[0-9a-f]{32})\.json\.gz$")


def sweep(directory, keep_keys) -> int:
    """Delete every cache entry in `directory` whose extraction key is not
    in `keep_keys`, and return how many went. Run after a load with the
    keys of the extractors it uses (:func:`extraction_key`): an entry
    under an older key is never read again. Files that are no cache
    entry stay."""
    directory = usable_cache_dir(directory)
    if directory is None or not directory.is_dir():
        return 0
    keep = set(keep_keys)
    removed = 0
    for path in directory.iterdir():
        m = _ENTRY_RE.match(path.name)
        if m is None or m["key"] in keep:
            continue
        try:
            path.unlink()
            removed += 1
        except OSError as exc:
            log.debug("extract-cache: could not remove %s: %s", path, exc)
    return removed
