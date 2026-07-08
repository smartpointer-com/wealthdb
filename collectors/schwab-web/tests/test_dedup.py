"""Unit tests for schwab-web dedup.py — parse-equivalence statement dedup.

No real PDFs / pypdfium2: `pdf_parsers.parse_statement_pdf` is stubbed with a
fake that derives the parsed dict from a content marker in the file
(``b"<marker>|<render-noise>"``), so the catalog + equivalence + collapse are
exercised end-to-end without the image. Synthetic account suffixes / filenames
only.
"""
from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import dedup  # noqa: E402
import pdf_parsers  # noqa: E402

OLD_A = "20260101T010000Z"
OLD_B = "20260102T010000Z"
STALE = time.time() - 6 * 3600


def _stub_parse(path, statement_year=None):
    """Fake parse: the marker before '|' is the 'content'; the bytes after it
    are per-render noise the parse ignores. Includes the volatile ``path`` field
    the real parser emits, so parse_key's stripping is exercised."""
    marker = Path(path).read_bytes().split(b"|", 1)[0].decode()
    return {"path": str(path), "period_start": None, "period_end": marker,
            "transactions": [], "positions": {}, "cash_summary": {},
            "account_registration": {}}


def _mk_run(root, slug, docs, files, status="complete"):
    """docs: list of (suffix, filename, type, date). files: rel -> bytes."""
    d = root / slug
    for rel, body in files.items():
        p = d / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(body)
    by_suffix: dict = {}
    for suffix, fn, typ, date in docs:
        by_suffix.setdefault(suffix, []).append(
            {"type": typ, "filename": fn, "date": date, "format": "PDF",
             "sha256": "0" * 64,
             "size": len(files.get(f"statements/{suffix}/{fn}", b""))})
    meta = {"status": status,
            "statements": [{"suffix": s, "documents": ds}
                           for s, ds in by_suffix.items()]}
    (d / "run.json").write_text(json.dumps(meta))
    return d


def _backdate(root: Path):
    for p in list(root.rglob("*")) + [root]:
        os.utime(p, (STALE, STALE), follow_symlinks=False)


def _ino(p: Path) -> int:
    return p.stat().st_ino


# ============================================================
# Catalog — only true statement PDFs, with the year hint
# ============================================================

def test_catalog_selects_only_statement_pdfs(tmp_path):
    d = _mk_run(tmp_path, OLD_A,
                docs=[("9999", "stmt.pdf", "Statements", "01/31/2024"),
                      ("9999", "tax.pdf", "Tax Forms", "12/31/2023"),   # wrong kind
                      ("9999", "stmt.xml", "Statements", "01/31/2024"),  # wrong fmt
                      ("9999", "undated.pdf", "Statements", "")],        # no date
                files={"statements/9999/stmt.pdf": b"JAN|r",
                       "statements/9999/tax.pdf": b"x",
                       "statements/9999/stmt.xml": b"x",
                       "statements/9999/undated.pdf": b"x"})
    # A statement PDF with NO manifest sha256 is excluded — load skips it too.
    meta = json.loads((d / "run.json").read_text())
    meta["statements"][0]["documents"].append(
        {"type": "Statements", "filename": "nosha.pdf", "date": "02/29/2024",
         "format": "PDF", "size": 3})
    (d / "run.json").write_text(json.dumps(meta))
    (d / "statements/9999/nosha.pdf").write_bytes(b"x")

    cat = dedup.build_statement_catalog(tmp_path)
    assert {Path(k).name for k in cat} == {"stmt.pdf"}
    (gid, year), = cat.values()
    # logical id is load's key: <suffix>/<doc_date-epoch>/<filename>.
    assert gid.startswith("9999/") and gid.endswith("/stmt.pdf")
    assert year == 2024


def test_parse_key_ignores_path_and_render_noise(tmp_path, monkeypatch):
    monkeypatch.setattr(pdf_parsers, "parse_statement_pdf", _stub_parse)
    f1 = tmp_path / "a.pdf"; f1.write_bytes(b"JAN|noise-1")
    f2 = tmp_path / "b.pdf"; f2.write_bytes(b"JAN|noise-2-longer")
    f3 = tmp_path / "c.pdf"; f3.write_bytes(b"FEB|noise")
    assert dedup.parse_key(f1, 2024) == dedup.parse_key(f2, 2024)  # same content
    assert dedup.parse_key(f1, 2024) != dedup.parse_key(f3, 2024)  # differs


# ============================================================
# End-to-end via main()
# ============================================================

def test_collapse_equivalent_statements_end_to_end(tmp_path, monkeypatch):
    monkeypatch.setattr(pdf_parsers, "parse_statement_pdf", _stub_parse)
    a = _mk_run(tmp_path, OLD_A,
                [("9999", "stmt.pdf", "Statements", "01/31/2024")],
                {"statements/9999/stmt.pdf": b"JAN|render-old"})
    b = _mk_run(tmp_path, OLD_B,
                [("9999", "stmt.pdf", "Statements", "01/31/2024")],
                {"statements/9999/stmt.pdf": b"JAN|render-new-and-longer"})
    _backdate(tmp_path)
    assert dedup.main(["--bronze-dir", str(tmp_path)]) == 0
    # collapsed onto the oldest copy's bytes (lossy at byte level, silver-safe).
    assert _ino(a / "statements/9999/stmt.pdf") == _ino(b / "statements/9999/stmt.pdf")
    assert (b / "statements/9999/stmt.pdf").read_bytes() == b"JAN|render-old"


def test_divergent_statements_not_collapsed(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(pdf_parsers, "parse_statement_pdf", _stub_parse)
    a = _mk_run(tmp_path, OLD_A,
                [("9999", "stmt.pdf", "Statements", "01/31/2024")],
                {"statements/9999/stmt.pdf": b"JAN|r"})
    b = _mk_run(tmp_path, OLD_B,
                [("9999", "stmt.pdf", "Statements", "01/31/2024")],
                {"statements/9999/stmt.pdf": b"FEB|r"})   # parses different
    _backdate(tmp_path)
    dedup.main(["--bronze-dir", str(tmp_path)])
    assert _ino(a / "statements/9999/stmt.pdf") != _ino(b / "statements/9999/stmt.pdf")
    assert "DIVERGENT" in capsys.readouterr().out


def test_dry_run_collapses_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr(pdf_parsers, "parse_statement_pdf", _stub_parse)
    a = _mk_run(tmp_path, OLD_A,
                [("9999", "stmt.pdf", "Statements", "01/31/2024")],
                {"statements/9999/stmt.pdf": b"JAN|r1"})
    b = _mk_run(tmp_path, OLD_B,
                [("9999", "stmt.pdf", "Statements", "01/31/2024")],
                {"statements/9999/stmt.pdf": b"JAN|r2"})
    _backdate(tmp_path)
    ia, ib = (_ino(a / "statements/9999/stmt.pdf"),
              _ino(b / "statements/9999/stmt.pdf"))
    dedup.main(["--bronze-dir", str(tmp_path), "--dry-run"])
    assert _ino(a / "statements/9999/stmt.pdf") == ia
    assert _ino(b / "statements/9999/stmt.pdf") == ib
