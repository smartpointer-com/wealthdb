"""
Tests for download.py's run.json status lifecycle.

The SFTP transport (connect + the per-order-type sftp.get loop) is
mocked out; these exercise only the additive metadata wrapper the
prune convention introduced:

  * an "in-progress" run.json is written at run-dir creation, before
    the pull walks
  * a finished pull (>=1 zip) overwrites it with status="complete"
  * a pull that fetched nothing removes the whole shell (rmtree, since
    the run.json makes the dir non-empty)
  * --dry-run creates no run dir (and never walks)
  * --debug is accepted (uniform flag; gates nothing here)

Synthetic client id / zip bytes only — no network, no real UBS data.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402
from collectorkit import bronze  # noqa: E402


def _run_main(tmp_path, monkeypatch, download_all_impl, *extra_argv):
    """Invoke download.main() with connect() + download_all() mocked."""
    client = mock.MagicMock()
    client.open_sftp.return_value = mock.MagicMock()
    monkeypatch.setattr(download, "connect", lambda args: client)
    monkeypatch.setattr(download, "download_all", download_all_impl)
    monkeypatch.setattr(
        sys, "argv",
        ["download.py", "--client-id", "CH000000",
         "--dest", str(tmp_path), *extra_argv])
    return download.main()


def _run_dirs(root: Path):
    return [p for p in root.iterdir()
            if p.is_dir() and bronze.RUN_DIR_RE.match(p.name)]


def test_complete_pull_writes_complete_status(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        (run_dir / "Z40.zip").write_bytes(b"PK\x03\x04")
        return 2, 1

    assert _run_main(tmp_path, monkeypatch, dl) == 0
    runs = _run_dirs(tmp_path)
    assert len(runs) == 1
    d = runs[0]
    assert (d / "ZAH.zip").exists()
    meta = json.loads((d / "run.json").read_text())
    assert meta["status"] == "complete"
    assert meta["downloaded"] == 2
    assert meta["empty"] == 1


def test_in_progress_marker_present_during_walk(tmp_path, monkeypatch):
    seen = {}

    def dl(sftp, run_dir, verbose=False):
        seen["mid"] = json.loads((run_dir / "run.json").read_text())
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    _run_main(tmp_path, monkeypatch, dl)
    assert seen["mid"]["status"] == "in-progress"


def test_zero_zip_pull_removes_shell(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        return 0, 40

    assert _run_main(tmp_path, monkeypatch, dl) == 0
    # rmtree removed the in-progress-only shell — no run dir survives.
    assert _run_dirs(tmp_path) == []


def test_dry_run_creates_no_run_dir(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        raise AssertionError("download_all must not run under --dry-run")

    assert _run_main(tmp_path, monkeypatch, dl, "--dry-run") == 0
    assert _run_dirs(tmp_path) == []


def test_debug_flag_accepted(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    assert _run_main(tmp_path, monkeypatch, dl, "--debug") == 0
