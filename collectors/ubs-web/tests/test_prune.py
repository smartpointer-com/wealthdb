"""
Unit tests for prune.py.

tmp_path bronze trees, synthetic artefacts only (no real account
tokens / IBANs / balances). ``debug_subdirs`` names ``screenshots/``
(the ``download --debug`` landmark captures), so prune has two deletion
categories: that subdir from a complete dump, and whole non-complete
run dirs. (The --screenshot-dir / --trace diagnostics land outside
bronze, where prune never sees them.) Covers what ubs-web's configuration
decides; the shared engine's envelope (symlinks, unreadable manifests,
the age guard, --dry-run, target validation) is tested in collectorkit:

  * complete dump: screenshots/ reclaimed, every load input kept
  * a `partial` dump (a `download --only` pass) kept like a complete one
  * prune keeps exactly the dumps `load` reads (`load.skip_reason`)
  * the ubs-web divergence: a statusless legacy manifest is COMPLETE
    only when it is NOT a --dry-run shell — dry_run:true ⇒ deleted,
    dry_run:false ⇒ kept
  * the supplied-documents dir at the bronze root is never a target
  * the one targetable subdir is screenshots/
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default

# Sentinel: build the default "complete" manifest.
_DEFAULT = object()


def make_dump(root: Path, slug: str, manifest=_DEFAULT,
              age_s: float = 0.0, screenshots: bool = True) -> Path:
    """Build a synthetic ubs-web bronze run dir with the standard load
    inputs (positions/ consolidated + per-portfolio CSVs, transactions/
    cash CSV + MT940, documents/ PDF) plus the `--debug` screenshots/
    capture dir.

    ``manifest`` controls run.json: the default sentinel writes a
    terminal ``complete`` manifest; ``None`` writes no run.json at all
    (a crashed walk); any dict is written verbatim (an in-progress
    marker, a dry-run shell, a statusless legacy manifest).

    ``age_s`` backdates every file/dir mtime (and the run dir's own) to
    now-age_s, so the write-activity guard sees an abandoned dump; the
    default (0) leaves it fresh.
    """
    d = root / slug
    (d / "positions").mkdir(parents=True)
    (d / "positions" / "positions.csv").write_text("a;b\n1;2\n")
    (d / "positions" / "positions_0badc0de0badc0de.csv").write_text(
        "a;b\n3;4\n")
    (d / "transactions").mkdir()
    (d / "transactions" / "cash_0badc0de0badc0de_20260101_20260201.csv"
     ).write_text("x;y\n5;6\n")
    (d / "transactions" / "cash_0badc0de0badc0de_20260101_20260201.mt940"
     ).write_text(":20:SYNTHETIC\n")
    (d / "documents").mkdir()
    (d / "documents" / "0badc0de0badc0de.pdf").write_bytes(b"%PDF-1.4 fake")
    if screenshots:
        (d / "screenshots").mkdir()
        (d / "screenshots" / "10-home.html").write_text("<html>home</html>")
        (d / "screenshots" / "10-home.png").write_bytes(b"\x89PNG fake")
    if manifest is _DEFAULT:
        manifest = {"status": "complete", "dry_run": False}
    if manifest is not None:
        (d / "run.json").write_text(json.dumps(manifest))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def assert_inputs_present(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "positions" / "positions.csv").exists()
    assert (d / "positions" / "positions_0badc0de0badc0de.csv").exists()
    assert (d / "transactions"
            / "cash_0badc0de0badc0de_20260101_20260201.csv").exists()
    assert (d / "transactions"
            / "cash_0badc0de0badc0de_20260101_20260201.mt940").exists()
    assert (d / "documents" / "0badc0de0badc0de.pdf").exists()


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


# ============================================================
# Complete dumps: screenshots reclaimed, load inputs kept
# ============================================================

def test_complete_dump_screenshots_pruned_inputs_kept(tmp_path):
    # screenshots/ is the only non-load-input a run dir holds, so it is
    # the only thing a complete dump gives up.
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert_inputs_present(d)


# ============================================================
# Which dumps are kept: exactly the ones `load` reads
# ============================================================

def test_partial_dump_kept_with_its_load_inputs(tmp_path):
    # A `download --only` pass finalises as "partial". `load` reads it,
    # so prune keeps it like a complete dump and takes only screenshots/.
    d = make_dump(tmp_path, OLD_TS,
                  manifest={"status": "partial", "partial_pass": "cards",
                            "dry_run": False},
                  age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert_inputs_present(d)


@pytest.mark.parametrize("manifest", [
    None,
    {"status": "complete", "dry_run": False},
    {"status": "partial", "partial_pass": "cards", "dry_run": False},
    {"status": "in-progress"},
    {"status": "dry-run", "dry_run": True},
    {"status": "unknown-future-status"},
    {"dry_run": False},
    {"dry_run": True},
])
def test_prune_keeps_exactly_what_load_reads(tmp_path, manifest):
    import load
    d = make_dump(tmp_path, OLD_TS, manifest=manifest, age_s=STALE_S)
    loads = load.skip_reason(d) is None
    run_main(tmp_path)
    assert d.exists() == loads


# ============================================================
# Statusless legacy manifests: the ubs-web dry_run divergence
# ============================================================

def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key is written once, at the end of a
    # real walk, so a statusless manifest with dry_run:false means the
    # dump finished: classify COMPLETE, keep its load inputs.
    d = make_dump(tmp_path, OLD_TS, manifest={"dry_run": False},
                  age_s=STALE_S, screenshots=False)
    run_main(tmp_path)
    assert d.exists()
    assert_inputs_present(d)


def test_statusless_dry_run_shell_deleted(tmp_path):
    # The load-bearing ubs-web divergence from fidelity-web: a legacy
    # --dry-run shell also has a full-looking (statusless) manifest, but
    # carries dry_run:true. Bare `m is not None` would wrongly keep it
    # forever; the `not m.get("dry_run")` guard classes it NON_COMPLETE.
    d = make_dump(tmp_path, OLD_TS, manifest={"dry_run": True},
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# Root-level safety and target validation
# ============================================================

def test_the_supplied_documents_dir_survives_a_prune(tmp_path):
    """Bank-delivered PDFs live at the bronze root, beside the run dirs.
    They are load inputs with no dump to belong to, and nothing regenerates
    them — so the one deletion path that takes a whole directory must never
    see this one."""
    make_dump(tmp_path, OLD_TS)
    supplied = tmp_path / "supplied-documents"
    supplied.mkdir()
    (supplied / "statement.pdf").write_bytes(b"%PDF-1.4")
    run_main(tmp_path)
    assert (supplied / "statement.pdf").exists()
    with pytest.raises(SystemExit):
        prune.CONFIG.validate_target(supplied, tmp_path)


def test_validate_target_accepts_screenshots_subdir(tmp_path):
    # The one subdir shape prune may target: the --debug capture dir.
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)
