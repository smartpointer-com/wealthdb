"""
Tests for the loader-side guards that make a dead phase visible.

Two of them, and both exist because the same two-month activity
failure walked past every check that was there:

  * a presence flag must mean the phase PRODUCED something. The walk
    mkdirs each phase directory before its first navigation, so a flag
    read off ``is_dir()`` recorded a 1 for all 55 failed runs — the
    very column an audit would have trusted.
  * a dump whose manifest says it never finished must not be ingested.
    Eleven of the fleet's loaders already refuse one; this loader did
    not, and would have taken a crashed dump's partial artefacts.
"""

from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402


# --------------------------------------------------------- presence flags

def test_an_empty_phase_directory_is_not_presence(tmp_path):
    """The mkdir-before-navigating trap: the directory exists because
    the phase STARTED, which says nothing about what it got."""
    (tmp_path / "activity").mkdir()
    assert load._phase_present(tmp_path, "activity") == 0


def test_a_phase_directory_holding_an_export_is_presence(tmp_path):
    d = tmp_path / "activity"
    d.mkdir()
    (d / "activity_20260901__20260930.csv.zst").write_bytes(b"x")
    assert load._phase_present(tmp_path, "activity") == 1


def test_a_phase_that_never_ran_is_not_presence(tmp_path):
    assert load._phase_present(tmp_path, "activity") == 0


# ------------------------------------------------------- run-status gate

def _run_dir(root: Path, ts: str, status) -> Path:
    d = root / ts
    d.mkdir(parents=True)
    meta = {"cli_config": {"mode": "all"}}
    if status is not None:
        meta["status"] = status
    (d / "run.json").write_text(json.dumps(meta))
    return d


def test_scan_skips_a_dump_that_never_finished(tmp_path):
    _run_dir(tmp_path, "20260101T000000Z", "in-progress")
    _run_dir(tmp_path, "20260102T000000Z", "complete")
    got = [d.name for d in load.scan_bronze(tmp_path)]
    assert got == ["20260102T000000Z"]


def test_scan_keeps_a_manifest_that_predates_the_status_field(tmp_path):
    """A statusless manifest historically meant the walk finished, so
    it stays loadable — the gate must not orphan old bronze."""
    _run_dir(tmp_path, "20260101T000000Z", None)
    assert [d.name for d in load.scan_bronze(tmp_path)] == ["20260101T000000Z"]


def test_scan_keeps_a_complete_dump_whose_phase_came_back_short(tmp_path):
    """A short phase is NOT this gate's business. The dump is
    finished, its other phases are good, and withholding it would
    cost them their load — the mistake this collector must not make."""
    d = _run_dir(tmp_path, "20260101T000000Z", "complete")
    meta = json.loads((d / "run.json").read_text())
    meta["coverage"] = {
        "activity": {"complete": False, "gaps": ["2026-07-01..2026-07-30"]},
        "positions": {"complete": True, "gaps": []},
    }
    (d / "run.json").write_text(json.dumps(meta))
    assert [x.name for x in load.scan_bronze(tmp_path)] == ["20260101T000000Z"]


def test_scan_rejects_a_bronze_dir_that_does_not_exist(tmp_path):
    with pytest.raises(SystemExit):
        load.scan_bronze(tmp_path / "nope")
