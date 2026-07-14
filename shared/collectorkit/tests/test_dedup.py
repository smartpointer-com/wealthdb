"""Unit tests for collectorkit.dedup — the storage-dedup engine.

Safety-focused, mirroring the prune suite: byte-identical files across
complete quiescent dumps are shared (hardlink default / clone opt-in),
non-complete / fresh / symlinked dumps are skipped, unique files and tiny
files are left alone, symlinked intermediate dirs are never followed, the
sweep is idempotent, and load-input content is never altered.
"""
from __future__ import annotations

import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

from collectorkit import bronze, dedup

OLD_A = "20260101T010000Z"
OLD_B = "20260102T010000Z"
BIG = b"%PDF-1.4 " + b"x" * 20000        # >= DEFAULT_MIN_SIZE
BIG2 = b"%PDF-1.4 " + b"y" * 20000
STALE = time.time() - 6 * 3600


def _run(root: Path, slug: str, files: dict[str, bytes],
         status: str | None = "complete") -> Path:
    d = root / slug
    (d / "documents").mkdir(parents=True)
    for name, body in files.items():
        (d / name).write_bytes(body)
    meta: dict = {"portfolios": []}
    if status is not None:
        meta["status"] = status
    (d / "run.json").write_text(json.dumps(meta))
    return d


def _backdate(root: Path):
    for p in list(root.rglob("*")) + [root]:
        os.utime(p, (STALE, STALE), follow_symlinks=False)


def _ino(p: Path) -> int:
    return p.stat().st_ino


def _sweep(root: Path, **kw):
    return dedup.run(root, dry_run=kw.get("dry_run", False),
                     strategy=kw.get("strategy", "hardlink"),
                     min_age_hours=kw.get("min_age_hours", 1.0),
                     min_size=kw.get("min_size", dedup.DEFAULT_MIN_SIZE))


# ============================================================
# Core: cross-run byte-identical files are shared
# ============================================================

def test_hardlink_shares_identical_across_runs(tmp_path):
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {"documents/x.pdf": BIG})
    _backdate(tmp_path)
    assert _ino(a / "documents/x.pdf") != _ino(b / "documents/x.pdf")

    reclaimed, n = _sweep(tmp_path)

    # Same inode now, content byte-identical, one file's worth reclaimed.
    assert _ino(a / "documents/x.pdf") == _ino(b / "documents/x.pdf")
    assert (b / "documents/x.pdf").read_bytes() == BIG
    assert n == 1 and reclaimed == len(BIG)


def test_distinct_content_not_shared(tmp_path):
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {"documents/x.pdf": BIG2})
    _backdate(tmp_path)
    _sweep(tmp_path)
    assert _ino(a / "documents/x.pdf") != _ino(b / "documents/x.pdf")


def test_unique_size_never_hashed_or_shared(tmp_path):
    # Different sizes can't collide; nothing to share.
    _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    _run(tmp_path, OLD_B, {"documents/x.pdf": BIG + b"z"})
    _backdate(tmp_path)
    _, n = _sweep(tmp_path)
    assert n == 0


def test_tiny_files_below_min_size_ignored(tmp_path):
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": b"tiny"})
    b = _run(tmp_path, OLD_B, {"documents/x.pdf": b"tiny"})
    _backdate(tmp_path)
    _, n = _sweep(tmp_path)
    assert n == 0
    assert _ino(a / "documents/x.pdf") != _ino(b / "documents/x.pdf")


# ============================================================
# Idempotence + coexistence
# ============================================================

def test_idempotent_second_run_reclaims_nothing(tmp_path):
    _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    _run(tmp_path, OLD_B, {"documents/x.pdf": BIG})
    _backdate(tmp_path)
    _sweep(tmp_path)
    reclaimed, n = _sweep(tmp_path)          # already shared
    assert reclaimed == 0 and n == 0


def test_preexisting_hardlink_skipped(tmp_path):
    # Mirrors viac's download-time hardlinks: already-shared → not counted.
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {})
    os.link(a / "documents/x.pdf", b / "documents/x.pdf")
    _backdate(tmp_path)
    reclaimed, n = _sweep(tmp_path)
    assert reclaimed == 0 and n == 0


def test_reclaim_counts_per_freed_inode_not_per_path(tmp_path):
    # Independent canonical (inode X) + two dups already sharing ONE inode Y
    # (the viac pattern: newer runs hardlinked together, an older run holds
    # an independent copy). Only Y's blocks free, once → reclaim = size×1 in
    # BOTH the dry-run preview and the real run, never size×2.
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})   # inode X = canonical
    b = _run(tmp_path, OLD_B, {"documents/x.pdf": BIG})   # inode Y
    c = _run(tmp_path, "20260103T010000Z", {})
    os.link(b / "documents/x.pdf", c / "documents/x.pdf")  # C shares Y with B
    _backdate(tmp_path)

    dry, dn = _sweep(tmp_path, dry_run=True)
    assert dry == len(BIG) and dn == 2          # 2 dup paths, 1 freed inode
    real, rn = _sweep(tmp_path)
    assert real == len(BIG) and rn == 2         # NOT 2 × len(BIG)
    ix = _ino(a / "documents/x.pdf")
    assert _ino(b / "documents/x.pdf") == ix
    assert _ino(c / "documents/x.pdf") == ix    # all three collapsed onto X


# ============================================================
# Safety envelope
# ============================================================

@pytest.mark.parametrize("status", ["in-progress", "dry-run"])
def test_non_complete_dumps_skipped(tmp_path, status):
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {"documents/x.pdf": BIG}, status=status)
    _backdate(tmp_path)
    _sweep(tmp_path)
    # b is non-complete → never touched, so no sharing happened.
    assert _ino(a / "documents/x.pdf") != _ino(b / "documents/x.pdf")


def test_fresh_dump_skipped_by_quiescence(tmp_path):
    slug = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, slug, {"documents/x.pdf": BIG})
    _backdate(a)                              # a stale, b fresh
    _, n = _sweep(tmp_path)
    assert n == 0


def test_dry_run_changes_nothing(tmp_path):
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {"documents/x.pdf": BIG})
    _backdate(tmp_path)
    reclaimed, n = _sweep(tmp_path, dry_run=True)
    assert reclaimed == len(BIG) and n == 1          # reported
    assert _ino(a / "documents/x.pdf") != _ino(b / "documents/x.pdf")  # untouched


def test_symlinked_file_not_shared(tmp_path):
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {})
    (b / "documents/x.pdf").symlink_to(a / "documents/x.pdf")
    _backdate(tmp_path)
    _sweep(tmp_path)
    assert (b / "documents/x.pdf").is_symlink()       # left as-is


def test_symlinked_intermediate_dir_not_followed(tmp_path):
    external = tmp_path / "external"
    external.mkdir()
    (external / "x.pdf").write_bytes(BIG)
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {})
    # b/documents/sub -> external (a symlinked dir the walk must not enter)
    (b / "documents" / "sub").symlink_to(external, target_is_directory=True)
    _backdate(tmp_path)
    ext_ino = _ino(external / "x.pdf")
    _sweep(tmp_path)
    # The external file behind the symlinked dir is untouched.
    assert _ino(external / "x.pdf") == ext_ino
    assert (external / "x.pdf").read_bytes() == BIG


def test_validate_refuses_paths_outside_run_dir(tmp_path):
    _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    stray = tmp_path / "loose.pdf"
    stray.write_bytes(BIG)
    with pytest.raises(SystemExit):
        dedup._validate(stray, tmp_path)
    outside = tmp_path.parent / "outside.pdf"
    outside.write_bytes(BIG)
    with pytest.raises(SystemExit):
        dedup._validate(outside, tmp_path)


def test_validate_refuses_symlinked_intermediate_dir(tmp_path):
    # TOCTOU guard: a file reached through a symlinked intermediate dir
    # (swapped in after planning) resolves to a regular file, but the write
    # would land outside the tree — _validate must refuse it.
    run = tmp_path / OLD_A
    (run / "documents").mkdir(parents=True)
    external = tmp_path / "external"
    external.mkdir()
    (external / "x.pdf").write_bytes(BIG)
    (run / "documents" / "sub").symlink_to(external, target_is_directory=True)
    victim = run / "documents" / "sub" / "x.pdf"
    assert victim.is_file()                      # resolves through the symlink
    with pytest.raises(SystemExit):
        dedup._validate(victim, tmp_path)


def test_manifestless_dump_skip_reason_is_honest(tmp_path, capsys):
    # A dump with no run.json (schwab-api / ubs-psn legacy shape) is skipped
    # without implying a crash — it may be perfectly complete.
    d = tmp_path / OLD_A
    (d / "documents").mkdir(parents=True)
    (d / "documents" / "x.pdf").write_bytes(BIG)   # no run.json
    _backdate(tmp_path)
    _sweep(tmp_path)
    out = capsys.readouterr().out
    assert "no run.json" in out
    assert "crash" not in out.lower()


# ============================================================
# Clone strategy (copy-on-write reflink; skips where unsupported)
# ============================================================

def test_clone_strategy_shares_and_verifies(tmp_path):
    a = _run(tmp_path, OLD_A, {"documents/x.pdf": BIG})
    b = _run(tmp_path, OLD_B, {"documents/x.pdf": BIG})
    _backdate(tmp_path)
    try:
        reclaimed, n = _sweep(tmp_path, strategy="clone")
    except Exception:
        pytest.skip("copy-on-write clone not supported on this filesystem")
    # Clones are independent inodes but byte-identical content.
    assert (b / "documents/x.pdf").read_bytes() == BIG
    assert (a / "documents/x.pdf").read_bytes() == BIG
    assert n == 1 and reclaimed == len(BIG)


# ============================================================
# CLI subtree discovery
# ============================================================

def test_collector_subtrees_discovers_bronze_trees(tmp_path):
    (tmp_path / "cointracking").mkdir()
    _run(tmp_path / "cointracking", OLD_A, {"documents/x.pdf": BIG})
    (tmp_path / "not-a-collector").mkdir()          # no run dirs
    (tmp_path / "loose.db").write_bytes(b"x")
    trees = dedup._collector_subtrees(tmp_path, None)
    assert [t.name for t in trees] == ["cointracking"]
    # --source narrows to one
    assert dedup._collector_subtrees(tmp_path, "cointracking")[0].name \
        == "cointracking"
    assert dedup._collector_subtrees(tmp_path, "nope") == []


def test_source_honours_per_collector_data_dir_override(tmp_path, monkeypatch):
    # ${PREFIX}_DATA_DIR points a collector's bronze outside --data-dir (the
    # wrappers honour it first). A --source sweep must follow it there (F5),
    # not sweep <data-dir>/<source>. Prefix = source upper, hyphens -> _.
    override = tmp_path / "custom-viac-root"
    override.mkdir()
    _run(override, OLD_A, {"documents/x.pdf": BIG})
    monkeypatch.setenv("VIAC_DATA_DIR", str(override))
    trees = dedup._collector_subtrees(tmp_path / "elsewhere", "viac")
    assert trees == [override]
    # Without the override it falls back to <data-dir>/<source>.
    monkeypatch.delenv("VIAC_DATA_DIR", raising=False)
    assert dedup._collector_subtrees(tmp_path / "elsewhere", "viac") == []


def test_source_requires_real_tree_with_run_dirs(tmp_path):
    (tmp_path / "empty").mkdir()                      # a dir with no run dirs
    assert dedup._collector_subtrees(tmp_path, "empty") == []
    real = tmp_path / "carta"
    real.mkdir()
    _run(real, OLD_A, {"documents/x.pdf": BIG})
    (tmp_path / "link").symlink_to(real, target_is_directory=True)
    assert dedup._collector_subtrees(tmp_path, "link") == []   # symlink refused
