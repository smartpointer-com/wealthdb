"""Tests for tools/parse_diff.py over invented parsers and documents.

Each test builds two small checkouts with a parser module and runs the
tool over a bronze directory of text files. The output may hold counts
and file hashes only: every run checks that no document text and no
path reaches it, with boolean checks so a failure message carries
neither.
"""
from __future__ import annotations

import hashlib
import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest

TOOL = Path(__file__).resolve().parents[1] / "tools" / "parse_diff.py"
_spec = importlib.util.spec_from_file_location("parse_diff", TOOL)
parse_diff = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(parse_diff)

MARKER = "MARKER-51c2"

PARSER = '''
from collectorkit import fixture_kit


def parse(path):
    text = open(path, encoding="utf-8").read()
    if text.startswith("boom"):
        raise ValueError(BOOM + text)
    print(text)  # parser chatter never reaches the tool's output
    return {"length": len(text), "head": text[:5], "tag": TAG,
            "kit": fixture_kit.VERSION}
'''


def _checkout(root: Path, *, tag="t", boom="bad: ", kit=1) -> Path:
    """A checkout with a collector `fake` and a kit module of its own."""
    kit_dir = root / "shared" / "collectorkit" / "collectorkit"
    kit_dir.mkdir(parents=True)
    (kit_dir / "__init__.py").write_text("")
    (kit_dir / "fixture_kit.py").write_text(f"VERSION = {kit}\n")
    collector = root / "collectors" / "fake"
    collector.mkdir(parents=True)
    (collector / "fakeparse.py").write_text(
        f"TAG = {tag!r}\nBOOM = {boom!r}\n" + PARSER)
    return collector


def _sha(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


@pytest.fixture
def bronze(tmp_path):
    root = tmp_path / "bronze"
    docs = {
        "20260101T000000Z/a.pdf": "alpha",
        "20260102T000000Z/a.pdf": "alpha",  # same bytes: runs once
        "20260102T000000Z/c.pdf": f"change {MARKER}",
        "20260102T000000Z/d.pdf": f"boom {MARKER}",
        "20260102T000000Z/skip.txt": "not a document",
    }
    for rel, text in docs.items():
        (root / rel).parent.mkdir(parents=True, exist_ok=True)
        (root / rel).write_text(text)
    return root


def _run(capsys, tmp_path, old, new, bronze, *extra) -> tuple[int, list[str]]:
    rc = parse_diff.main(["--old", str(old), "--new", str(new),
                          "--bronze", str(bronze), *extra, "fakeparse:parse"])
    out, err = capsys.readouterr()
    leaked = any(s in out or s in err for s in (MARKER, "alpha", str(tmp_path)))
    assert not leaked, "a document's text or a path reached the output"
    return rc, out.splitlines()


def test_equal_checkouts_report_no_difference(tmp_path, bronze, capsys):
    old = _checkout(tmp_path / "old")
    new = _checkout(tmp_path / "new")
    assert _run(capsys, tmp_path, old, new, bronze) == (0, [
        "documents: 3 unique of 4 files",
        "fakeparse:parse: 3 same, 0 differ",
    ])


def test_a_changed_result_names_every_file_hash(tmp_path, bronze, capsys):
    old = _checkout(tmp_path / "old")
    new = _checkout(tmp_path / "new", tag="u")
    rc, lines = _run(capsys, tmp_path, old, new, bronze, "--jobs", "2")
    assert rc == 1
    assert lines[1] == "fakeparse:parse: 1 same, 2 differ"
    assert sorted(lines[2:]) == sorted(
        f"  differs: {_sha(t)}" for t in ("alpha", f"change {MARKER}"))


def test_a_changed_exception_text_is_a_difference(tmp_path, bronze, capsys):
    old = _checkout(tmp_path / "old")
    new = _checkout(tmp_path / "new", boom="worse: ")
    assert _run(capsys, tmp_path, old, new, bronze) == (1, [
        "documents: 3 unique of 4 files",
        "fakeparse:parse: 2 same, 1 differ",
        f"  differs: {_sha(f'boom {MARKER}')}",
    ])


def test_each_side_imports_the_kit_of_its_own_checkout(tmp_path, bronze, capsys):
    old = _checkout(tmp_path / "old")
    new = _checkout(tmp_path / "new", kit=2)
    rc, lines = _run(capsys, tmp_path, old, new, bronze)
    assert (rc, lines[1]) == (1, "fakeparse:parse: 1 same, 2 differ")


def test_a_function_that_does_not_load_is_a_usage_error(tmp_path, bronze, capsys):
    old = _checkout(tmp_path / "old")
    new = _checkout(tmp_path / "new")
    (new / "fakeparse.py").write_text("raise ImportError('gone')\n")
    assert _run(capsys, tmp_path, old, new, bronze) == (2, [
        "documents: 3 unique of 4 files"])


def test_usage_errors(tmp_path, bronze):
    old = _checkout(tmp_path / "old")
    for argv in (
        ["--old", str(old), "--new", str(old), "--bronze", str(bronze), "nocolon"],
        ["--old", str(old), "--new", str(tmp_path / "absent"), "--bronze",
         str(bronze), "m:f"],
        ["--old", str(old), "--new", str(old), "--bronze", str(bronze),
         "--jobs", "0", "m:f"],
    ):
        with pytest.raises(SystemExit) as e:
            parse_diff.main(argv)
        assert e.value.code == 2


def test_help():
    out = subprocess.run([sys.executable, "-I", str(TOOL), "--help"],
                         capture_output=True, text=True, check=True).stdout
    assert "MODULE:FUNCTION" in out
