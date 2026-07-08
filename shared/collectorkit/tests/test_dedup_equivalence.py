"""Unit tests for collectorkit.dedup's equivalence-dedup API
(`plan_equivalence` / `collapse_equiv_groups`): collapse files that share an
injected equivalence KEY — not necessarily byte-identical — onto the oldest
copy, under the same safety envelope as the byte sweep.

No PDFs: the key function is a stub. Each fixture file's bytes are
``b"<marker>|<render-noise>"``; the stub key is the ``<marker>`` before the
``|`` and the noise after it models the per-download re-render, so two files
with the same marker but different bytes are the parse-equivalent case the real
schwab-web verb collapses.
"""
from __future__ import annotations

import json
import os
import time
from pathlib import Path

from collectorkit import dedup

OLD_A = "20260101T010000Z"
OLD_B = "20260102T010000Z"
OLD_C = "20260103T010000Z"
STALE = time.time() - 6 * 3600


def _run(root: Path, slug: str, files: dict[str, bytes],
         status: str | None = "complete") -> Path:
    d = root / slug
    for rel, body in files.items():
        p = d / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(body)
    meta = {"status": status} if status is not None else {}
    (d / "run.json").write_text(json.dumps(meta))
    return d


def _backdate(root: Path):
    for p in list(root.rglob("*")) + [root]:
        os.utime(p, (STALE, STALE), follow_symlinks=False)


def _ino(p: Path) -> int:
    return p.stat().st_ino


# key = the marker before the first '|' (the "parsed content"); the bytes after
# it are per-render noise that the key ignores.
def _key(path) -> str:
    return Path(path).read_bytes().split(b"|", 1)[0].decode()


def _gid(run_dir, path):
    return path.name if path.suffix == ".pdf" else None


def _plan(root, **kw):
    return dedup.plan_equivalence(
        root, group_of=_gid, key_of=_key,
        min_age_s=kw.get("min_age_hours", 1.0) * 3600.0,
        min_size=kw.get("min_size", 1))


# ============================================================
# The core case: parse-equal but byte-different copies collapse
# ============================================================

def test_parse_equal_bytes_differ_collapse_onto_oldest(tmp_path):
    a = _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|old-render"})
    b = _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|new-render-longer"})
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    assert not divergent
    assert len(groups) == 1
    g = groups[0]
    assert g.group_id == "s.pdf" and g.equiv_key == "STMT-A"
    assert len(g.dups) == 1
    # oldest run (lexicographically smallest path) is the canonical keep.
    assert g.canonical.path == a / "statements/9999/s.pdf"
    # reclaim is the DUP's own size (members are not byte-equal → sizes differ).
    assert g.reclaim == len(b"STMT-A|new-render-longer")

    reclaimed, n = dedup.collapse_equiv_groups(groups, tmp_path, key_of=_key)
    assert n == 1 and reclaimed == len(b"STMT-A|new-render-longer")
    # now one inode; the canonical's bytes back both (lossy at byte level).
    assert _ino(a / "statements/9999/s.pdf") == _ino(b / "statements/9999/s.pdf")
    assert (b / "statements/9999/s.pdf").read_bytes() == b"STMT-A|old-render"


def test_divergent_group_never_collapsed(tmp_path):
    _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-B|r2"})  # differs
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    assert groups == []
    assert len(divergent) == 1 and divergent[0].group_id == "s.pdf"
    reclaimed, n = dedup.collapse_equiv_groups(groups, tmp_path, key_of=_key)
    assert n == 0


def test_lone_copy_is_never_keyed(tmp_path):
    # A statement present in only ONE run cannot collapse, so it is never
    # parsed (the expensive step is skipped for singletons).
    _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    _backdate(tmp_path)
    calls: list = []

    def key_spy(p):
        calls.append(str(p))
        return _key(p)

    groups, divergent, skipped = dedup.plan_equivalence(
        tmp_path, group_of=_gid, key_of=key_spy, min_age_s=3600.0, min_size=1)
    assert groups == [] and divergent == []
    assert calls == []


def test_parse_error_member_is_divergent(tmp_path):
    _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|r2"})
    _backdate(tmp_path)

    def flaky(p):
        if OLD_B in str(p):
            raise ValueError("boom")
        return _key(p)

    groups, divergent, skipped = dedup.plan_equivalence(
        tmp_path, group_of=_gid, key_of=flaky, min_age_s=3600.0, min_size=1)
    assert groups == []
    assert len(divergent) == 1


# ============================================================
# Accounting + idempotence
# ============================================================

def test_reclaim_counts_per_freed_inode_not_per_path(tmp_path):
    a = _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|canon"})
    b = _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|dupdup"})
    c = _run(tmp_path, OLD_C, {"statements/9999/s.pdf": b"placeholder"})
    # B and C pre-share one inode (e.g. a prior sweep); reclaim counts it once.
    (c / "statements/9999/s.pdf").unlink()
    os.link(b / "statements/9999/s.pdf", c / "statements/9999/s.pdf")
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    assert len(groups) == 1
    g = groups[0]
    assert len(g.dups) == 2                     # two dup PATHS (B, C)
    assert g.reclaim == len(b"STMT-A|dupdup")   # one dup INODE
    reclaimed, n = dedup.collapse_equiv_groups(groups, tmp_path, key_of=_key)
    assert reclaimed == len(b"STMT-A|dupdup")
    assert (_ino(a / "statements/9999/s.pdf")
            == _ino(b / "statements/9999/s.pdf")
            == _ino(c / "statements/9999/s.pdf"))


def test_already_shared_inode_is_not_a_dup(tmp_path):
    # If the newer copy already shares the canonical inode, there is nothing
    # to reclaim (idempotent — a second run does nothing).
    a = _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    b = _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"placeholder"})
    (b / "statements/9999/s.pdf").unlink()
    os.link(a / "statements/9999/s.pdf", b / "statements/9999/s.pdf")
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    assert groups == [] and divergent == []


# ============================================================
# Safety envelope
# ============================================================

def test_plan_mutates_nothing(tmp_path):
    a = _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    b = _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|r2"})
    _backdate(tmp_path)
    ia, ib = (_ino(a / "statements/9999/s.pdf"),
              _ino(b / "statements/9999/s.pdf"))
    _plan(tmp_path)
    assert _ino(a / "statements/9999/s.pdf") == ia
    assert _ino(b / "statements/9999/s.pdf") == ib


def test_non_complete_dump_excluded(tmp_path):
    _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|r2"},
         status="in-progress")
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    # only OLD_A is eligible → singleton → nothing collapses.
    assert groups == [] and divergent == []


def test_fresh_dump_skipped_by_quiescence(tmp_path):
    _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    b = _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|r2"})
    _backdate(tmp_path)
    now = time.time()
    for p in list(b.rglob("*")) + [b]:
        os.utime(p, (now, now))
    groups, divergent, skipped = _plan(tmp_path, min_age_hours=1.0)
    assert groups == []          # OLD_B in flight → OLD_A singleton


def test_symlinked_statement_not_walked(tmp_path):
    a = _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    b = _run(tmp_path, OLD_B, {"statements/9999/other": b"x"})
    (b / "statements/9999/s.pdf").symlink_to(a / "statements/9999/s.pdf")
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    assert groups == []          # the symlinked copy is never walked


def test_collapse_skips_drifted_member(tmp_path):
    a = _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    b = _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|r2"})
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    # the dup changes on disk AFTER planning → drift guard skips it.
    (b / "statements/9999/s.pdf").write_bytes(b"STMT-A|r2-CHANGED")
    reclaimed, n = dedup.collapse_equiv_groups(groups, tmp_path, key_of=_key)
    assert n == 0
    assert _ino(a / "statements/9999/s.pdf") != _ino(b / "statements/9999/s.pdf")


def test_collapse_reverify_key_mismatch_skips(tmp_path):
    # If the equivalence key at collapse time disagrees with the plan, the
    # member is skipped — the last-moment re-check before the replace.
    a = _run(tmp_path, OLD_A, {"statements/9999/s.pdf": b"STMT-A|r1"})
    b = _run(tmp_path, OLD_B, {"statements/9999/s.pdf": b"STMT-A|r2"})
    _backdate(tmp_path)
    groups, divergent, skipped = _plan(tmp_path)
    reclaimed, n = dedup.collapse_equiv_groups(
        groups, tmp_path, key_of=lambda p: str(p))   # every path distinct
    assert n == 0
    assert _ino(a / "statements/9999/s.pdf") != _ino(b / "statements/9999/s.pdf")
