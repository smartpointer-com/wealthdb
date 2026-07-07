"""Unit tests for collectorkit.recompress — the backlog-recompression engine.

The engine rewrites load inputs, so the coverage mirrors the prune
suite's safety focus:

  * complete quiescent dump: patterned files become verified .zst
    twins, originals gone, content and mtime preserved
  * non-complete / UNKNOWN / symlinked / fresh run dirs untouched
  * already-converted runs drop out (idempotence)
  * interrupted attempt (twin + original both present): a verifying
    twin is adopted, a corrupt twin is recompressed — either way the
    run converges
  * --dry-run rewrites nothing
  * files outside the patterns (run.json) are never touched
  * validate_target refuses non-run-dir paths and symlinks
"""
from __future__ import annotations

import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

from collectorkit import bronze, compress, prune, recompress

zstandard = pytest.importorskip("zstandard")

OLD_TS = "20260101T010000Z"
BODY = b'"Type","Buy","Cur."\n"Trade","0.5","BTC"\n' * 200


def _is_complete(run_dir, meta):
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CFG = recompress.RecompressConfig(
    patterns=("cu_*/*.csv",),
    is_complete=_is_complete,
    level=3,   # tests care about correctness, not ratio
)


def _seed_run(root: Path, slug: str = OLD_TS, status: str | None = "complete",
              manifest: bool = True) -> Path:
    run_dir = root / slug
    sub = run_dir / "cu_1"
    sub.mkdir(parents=True, exist_ok=True)
    (sub / "trades.csv").write_bytes(BODY)
    (sub / "balance.csv").write_bytes(BODY[:100])
    if manifest:
        meta: dict = {"portfolios": []}
        if status is not None:
            meta["status"] = status
        (run_dir / "run.json").write_text(json.dumps(meta), encoding="utf-8")
    # Backdate everything so the quiescence guard passes.
    past = time.time() - 6 * 3600
    for p in [run_dir, sub, *sub.iterdir(),
              *([run_dir / "run.json"] if manifest else [])]:
        os.utime(p, (past, past))
    return run_dir


def _run(root: Path, **kw) -> int:
    return recompress.run(CFG, root, dry_run=kw.get("dry_run", False),
                          min_age_hours=kw.get("min_age_hours", 1.0))


def _backdate(*paths: Path) -> None:
    """Push a fixture artefact past the quiescence guard (planting a
    twin after _seed_run gives it a fresh mtime, which the guard —
    correctly — reads as an active walk)."""
    past = time.time() - 6 * 3600
    for p in paths:
        os.utime(p, (past, past))
        os.utime(p.parent, (past, past))


# ============================================================
# The happy path
# ============================================================

def test_complete_run_is_converted_and_verified(tmp_path):
    run_dir = _seed_run(tmp_path)
    assert _run(tmp_path) == 0

    sub = run_dir / "cu_1"
    assert not (sub / "trades.csv").exists()
    assert not (sub / "balance.csv").exists()
    for name in ("trades.csv", "balance.csv"):
        twin = sub / (name + ".zst")
        assert twin.is_file()
        with compress.open_bytes(twin) as fh:
            content = fh.read()
        assert content == (BODY if name == "trades.csv" else BODY[:100])
    # run.json is not a patterned file — untouched.
    assert (run_dir / "run.json").is_file()


def test_converted_run_drops_out_of_next_plan(tmp_path):
    _seed_run(tmp_path)
    _run(tmp_path)
    targets, _ = recompress.plan_recompress(tmp_path, CFG, min_age_s=0)
    assert targets == []


def test_mtime_carried_over(tmp_path):
    run_dir = _seed_run(tmp_path)
    before = (run_dir / "cu_1" / "trades.csv").stat().st_mtime
    _run(tmp_path)
    assert int((run_dir / "cu_1" / "trades.csv.zst").stat().st_mtime) \
        == int(before)


# ============================================================
# Safety envelope
# ============================================================

@pytest.mark.parametrize("status", ["in-progress", "dry-run"])
def test_non_complete_runs_untouched(tmp_path, status):
    run_dir = _seed_run(tmp_path, status=status)
    _run(tmp_path)
    assert (run_dir / "cu_1" / "trades.csv").read_bytes() == BODY
    assert not (run_dir / "cu_1" / "trades.csv.zst").exists()


def test_unknown_manifest_untouched(tmp_path):
    run_dir = _seed_run(tmp_path)
    (run_dir / "run.json").write_text("{corrupt", encoding="utf-8")
    _run(tmp_path)
    assert (run_dir / "cu_1" / "trades.csv").read_bytes() == BODY


def test_fresh_run_skipped_by_quiescence_guard(tmp_path):
    slug = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = _seed_run(tmp_path, slug=slug)  # slug itself is fresh
    _run(tmp_path)
    assert (run_dir / "cu_1" / "trades.csv").read_bytes() == BODY


def test_symlinked_run_dir_skipped(tmp_path):
    real = _seed_run(tmp_path / "elsewhere")
    (tmp_path / OLD_TS).symlink_to(real)
    _run(tmp_path)
    assert (real / "cu_1" / "trades.csv").read_bytes() == BODY


def test_dry_run_rewrites_nothing(tmp_path):
    run_dir = _seed_run(tmp_path)
    assert _run(tmp_path, dry_run=True) == 0
    assert (run_dir / "cu_1" / "trades.csv").read_bytes() == BODY
    assert not (run_dir / "cu_1" / "trades.csv.zst").exists()


def test_validate_target_refuses_foreign_paths(tmp_path):
    _seed_run(tmp_path)
    stray = tmp_path / "known_portfolios.json"
    stray.write_text("{}", encoding="utf-8")
    with pytest.raises(SystemExit):
        recompress.validate_target(stray, tmp_path)
    outside = tmp_path.parent / "outside.csv"
    outside.write_text("x", encoding="utf-8")
    with pytest.raises(SystemExit):
        recompress.validate_target(outside, tmp_path)


# ============================================================
# Interrupted-attempt convergence
# ============================================================

def test_adopts_verified_twin_from_interrupted_attempt(tmp_path):
    run_dir = _seed_run(tmp_path)
    orig = run_dir / "cu_1" / "trades.csv"
    # A prior attempt compressed but crashed before unlinking.
    twin = compress.compress_file(orig, remove_original=False, level=3)
    _backdate(twin)
    twin_bytes = twin.read_bytes()

    _run(tmp_path)
    assert not orig.exists()
    # Adopted, not recompressed: identical bytes.
    assert (run_dir / "cu_1" / "trades.csv.zst").read_bytes() == twin_bytes


def test_recompresses_corrupt_twin(tmp_path):
    run_dir = _seed_run(tmp_path)
    twin = run_dir / "cu_1" / "trades.csv.zst"
    twin.write_bytes(zstandard.ZstdCompressor(level=3).compress(b"stale"))
    _backdate(twin)

    _run(tmp_path)
    assert not (run_dir / "cu_1" / "trades.csv").exists()
    with compress.open_bytes(twin) as fh:
        assert fh.read() == BODY
