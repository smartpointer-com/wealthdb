"""Tests for collectorkit.srcfp — the parser source fingerprint."""
from __future__ import annotations

import importlib
import sys


from collectorkit import srcfp


def _write(path, text):
    path.write_text(text, encoding="utf-8")


def _import_from(dirpath, name):
    """Import (or reimport) module `name` from `dirpath`."""
    sys.path.insert(0, str(dirpath))
    try:
        sys.modules.pop(name, None)
        return importlib.import_module(name)
    finally:
        sys.path.remove(str(dirpath))


BASE = '''
"""Original docstring."""
import re  # noqa

THRESHOLD = 3  # a comment


def parse(text):
    """Return a count."""
    return text.count("x") + THRESHOLD
'''


def test_normalized_hash_ignores_comments_formatting_docstrings(tmp_path):
    a = tmp_path / "a.py"
    _write(a, BASE)
    h0 = srcfp.normalized_source_hash(a)

    # comment-only edit
    _write(a, BASE.replace("# a comment", "# a totally different comment"))
    assert srcfp.normalized_source_hash(a) == h0

    # docstring edits (module + function)
    _write(a, BASE.replace("Original docstring.", "Rewritten.").replace(
        "Return a count.", "Counts things differently in prose."))
    assert srcfp.normalized_source_hash(a) == h0

    # formatting: extra whitespace/blank lines + quote style (same AST)
    _write(a, BASE.replace("THRESHOLD = 3", "THRESHOLD  =  3\n\n").replace(
        'count("x")', "count('x')"))
    assert srcfp.normalized_source_hash(a) == h0

    # a real code change (different numeric literal) must change the hash
    _write(a, BASE.replace("THRESHOLD = 3", "THRESHOLD = 4"))
    assert srcfp.normalized_source_hash(a) != h0

    # a real code change (different string literal) must change the hash
    _write(a, BASE.replace('count("x")', 'count("y")'))
    assert srcfp.normalized_source_hash(a) != h0


def test_unparseable_source_falls_back_to_raw_bytes(tmp_path):
    a = tmp_path / "broken.py"
    _write(a, "def (:  not python")
    h0 = srcfp.normalized_source_hash(a)
    _write(a, "def (:  not python  # trailing change")
    assert srcfp.normalized_source_hash(a) != h0  # conservative: any byte change invalidates


def test_first_party_origin_membership(tmp_path):
    roots = {tmp_path}
    assert srcfp._first_party_origin("os", roots) is None
    assert srcfp._first_party_origin("re", roots) is None
    ck = srcfp._first_party_origin("collectorkit.compress", roots)
    assert ck is not None and ck.name == "compress.py"
    _write(tmp_path / "sibling.py", "x = 1\n")
    sib = srcfp._first_party_origin("sibling", roots)
    assert sib is not None and sib.name == "sibling.py"


def test_parser_fingerprint_follows_first_party_imports(tmp_path):
    _write(tmp_path / "helper_mod.py", 'FIELD = "alpha"  # tag\n')
    _write(tmp_path / "root_mod.py",
           "from helper_mod import FIELD\n\n\ndef parse():\n    return FIELD\n")
    root = _import_from(tmp_path, "root_mod")
    fp0 = srcfp.parser_fingerprint([root])

    # comment-only edit to the imported helper: fingerprint unchanged
    _write(tmp_path / "helper_mod.py", 'FIELD = "alpha"  # a different tag\n')
    assert srcfp.parser_fingerprint([root]) == fp0

    # code edit to the imported helper: fingerprint MUST change (closure works)
    _write(tmp_path / "helper_mod.py", 'FIELD = "beta"\n')
    assert srcfp.parser_fingerprint([root]) != fp0

    sys.modules.pop("root_mod", None)
    sys.modules.pop("helper_mod", None)


def test_package_init_reexports_are_not_spidered(tmp_path):
    """A re-exporting package __init__ must not pull the whole package into the
    closure: a sibling the __init__ imports but the parser never uses should
    not affect the fingerprint, while a module the parser actually imports
    should."""
    pkg = tmp_path / "pkgx"
    pkg.mkdir()
    _write(pkg / "__init__.py", "from pkgx.used import U\nfrom pkgx.unused import Z\n")
    _write(pkg / "used.py", "U = 1\n")
    _write(pkg / "unused.py", "Z = 1\n")
    _write(tmp_path / "top.py", "from pkgx.used import U\n\n\ndef parse():\n    return U\n")
    top = _import_from(tmp_path, "top")

    fp0 = srcfp.parser_fingerprint([top])
    # editing the unused sibling (only reachable via __init__ re-export) is inert
    _write(pkg / "unused.py", "Z = 999\n")
    assert srcfp.parser_fingerprint([top]) == fp0
    # editing the used sibling changes the fingerprint
    _write(pkg / "used.py", "U = 2\n")
    assert srcfp.parser_fingerprint([top]) != fp0

    for m in ("top", "pkgx", "pkgx.used", "pkgx.unused"):
        sys.modules.pop(m, None)


def test_parser_fingerprint_folds_dists_and_python(tmp_path, monkeypatch):
    _write(tmp_path / "solo.py", "def parse():\n    return 1\n")
    root = _import_from(tmp_path, "solo")

    monkeypatch.setattr(srcfp, "_dist_version", lambda d: "1.0.0")
    fp_a = srcfp.parser_fingerprint([root], extra_dists=("pdfplumber",))
    monkeypatch.setattr(srcfp, "_dist_version", lambda d: "1.0.1")
    fp_b = srcfp.parser_fingerprint([root], extra_dists=("pdfplumber",))
    assert fp_a != fp_b  # a library version bump invalidates

    # declaring a dist changes the key vs not declaring it
    assert srcfp.parser_fingerprint([root]) != fp_b

    monkeypatch.setattr(srcfp, "_dist_version",
                        lambda d: (_ for _ in ()).throw(srcfp.PackageNotFoundError()))
    # missing metadata is non-fatal
    assert isinstance(srcfp.parser_fingerprint([root], extra_dists=("nope",)), str)

    sys.modules.pop("solo", None)


def test_fingerprint_is_hex_and_stable(tmp_path):
    _write(tmp_path / "stable.py", "def parse():\n    return 42\n")
    root = _import_from(tmp_path, "stable")
    fp = srcfp.parser_fingerprint([root])
    assert len(fp) == 32 and all(c in "0123456789abcdef" for c in fp)
    assert fp == srcfp.parser_fingerprint([root])  # deterministic within a run
    sys.modules.pop("stable", None)


def test_an_extraction_binary_moves_the_fingerprint(tmp_path, monkeypatch):
    """A collector that shells out to `pdftotext` has no Python dist to pin,
    so the binary's own version is what stands in for it. Without this a
    poppler upgrade re-renders every column while the fingerprint holds
    still — the drift such a collector is least able to see."""
    _write(tmp_path / "shellout.py", "def parse():\n    return 1\n")
    root = _import_from(tmp_path, "shellout")
    tool = (("pdftotext", "-v"),)

    monkeypatch.setattr(srcfp, "_tool_version", lambda argv: "poppler 1.0.0")
    fp_a = srcfp.parser_fingerprint([root], extra_tools=tool)
    monkeypatch.setattr(srcfp, "_tool_version", lambda argv: "poppler 1.0.1")
    fp_b = srcfp.parser_fingerprint([root], extra_tools=tool)
    assert fp_a != fp_b

    # Declaring a tool changes the key against not declaring one, so a
    # collector that starts pinning its extractor re-derives once.
    assert srcfp.parser_fingerprint([root]) != fp_b

    sys.modules.pop("shellout", None)


def test_a_missing_extraction_binary_is_a_value_not_an_error(tmp_path):
    """Refusing to compute a fingerprint would fail the load over a
    diagnostic. An absent binary reads as "unavailable", which is itself a
    value — so a tool that disappears between runs still moves the key."""
    _write(tmp_path / "noexe.py", "def parse():\n    return 1\n")
    root = _import_from(tmp_path, "noexe")

    assert srcfp._tool_version(("definitely-not-on-this-path", "-v")) == "unavailable"
    fp = srcfp.parser_fingerprint(
        [root], extra_tools=(("definitely-not-on-this-path", "-v"),))
    assert len(fp) == 32

    sys.modules.pop("noexe", None)
