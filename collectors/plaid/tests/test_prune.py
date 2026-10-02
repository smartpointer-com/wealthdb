"""Tests for prune: the shared engine, run once per Item tree. Only debug
traces of complete runs and whole runs that did not complete may go, and
nothing goes when the dir holds runs that are not plaid's."""
from __future__ import annotations

import json
import os
import time

import pytest

import prune


def age(path, hours=3.0):
    stamp = time.time() - hours * 3600
    for p in [path, *path.rglob("*")]:
        os.utime(p, (stamp, stamp))


def make_run(tree, slug, status, *, trace=False, old=True):
    """A run of the Item whose tree holds it, as download writes one."""
    run_dir = tree / slug
    run_dir.mkdir(parents=True)
    (run_dir / "run.json").write_text(json.dumps(
        {"status": status, "item": tree.name}))
    (run_dir / "accounts.json").write_text("{}")
    if trace:
        (run_dir / "screenshots").mkdir()
        (run_dir / "screenshots" / "http-trace.jsonl").write_text("{}\n")
    if old:
        age(run_dir)
    return run_dir


def snapshot(root):
    return sorted((str(p.relative_to(root)),
                   p.read_bytes() if p.is_file() else None)
                  for p in root.rglob("*"))


@pytest.fixture
def bronze(tmp_path):
    root = tmp_path / "plaid"
    bank = root / "bank"
    make_run(bank, "20260101T000000Z", "complete", trace=True)
    make_run(bank, "20260102T000000Z", "in-progress")
    make_run(bank, "20260103T000000Z", "failed")
    stopped = bank / "20260104T000000Z"     # stopped at its first write
    stopped.mkdir()
    (stopped / "run.json.tmp").write_text("{")
    age(stopped)
    make_run(bank, "20260105T000000Z", "in-progress", old=False)
    (bank / "bank.db").write_text("silver")
    make_run(root / "broker", "20260101T000000Z", "complete")
    (root / "notes").write_text("not an Item tree")
    (root / "Not An Item").mkdir()
    return root


def test_prune_removes_traces_and_runs_that_did_not_complete(bronze, capsys):
    assert prune.main(["--bronze-dir", str(bronze)]) == 0

    bank = bronze / "bank"
    assert sorted(p.name for p in bank.iterdir()) == [
        "20260101T000000Z", "20260105T000000Z", "bank.db"]
    # The complete run keeps every load input and loses only its trace.
    assert sorted(p.name for p in (bank / "20260101T000000Z").iterdir()) == [
        "accounts.json", "run.json"]
    assert (bank / "bank.db").read_text() == "silver"
    assert (bronze / "broker" / "20260101T000000Z" / "run.json").exists()
    assert (bronze / "notes").exists() and (bronze / "Not An Item").exists()
    out = capsys.readouterr().out
    assert "== bank" in out and "== broker" in out
    assert "status='failed'" in out
    assert "stopped before its first run.json" in out


def test_a_dry_run_removes_nothing(bronze, capsys):
    before = snapshot(bronze)
    assert prune.main(["--bronze-dir", str(bronze), "--dry-run"]) == 0
    assert snapshot(bronze) == before
    assert "would delete" in capsys.readouterr().out


def test_a_run_in_flight_is_left_alone(bronze):
    prune.main(["--bronze-dir", str(bronze)])
    assert (bronze / "bank" / "20260105T000000Z").exists()


@pytest.mark.parametrize("hours,slug,kept", [
    ("0", "20260105T000000Z", False),   # a fresh run goes with no guard
    ("4", "20260102T000000Z", True),    # a three-hour-old run stays
])
def test_min_age_hours_sets_the_guard_both_ways(bronze, hours, slug, kept):
    # Either case differs from the one-hour default, so each fails if the
    # flag does not reach the engine.
    assert prune.main(["--bronze-dir", str(bronze),
                       "--min-age-hours", hours]) == 0
    assert (bronze / "bank" / slug).exists() is kept


def test_a_run_json_that_cannot_be_read_is_kept(bronze):
    run_dir = bronze / "bank" / "20260106T000000Z"
    run_dir.mkdir()
    (run_dir / "run.json").write_text("{")
    age(run_dir)
    assert prune.main(["--bronze-dir", str(bronze)]) == 0
    assert (run_dir / "run.json").read_text() == "{"


def test_named_items_only(bronze):
    assert prune.main(["--bronze-dir", str(bronze), "--item", "broker"]) == 0
    assert (bronze / "bank" / "20260102T000000Z").exists()


def test_an_unknown_item_is_refused(bronze):
    with pytest.raises(SystemExit, match="no Item tree"):
        prune.main(["--bronze-dir", str(bronze), "--item", "absent"])


# ---- a dir that is not plaid's own -------------------------------------------------

def other_collectors(root):
    """A data root as other collectors leave it: runs with no status,
    runs with no run.json, and a complete run with a debug capture."""
    chase = root / "chase" / "20250101T000000Z"
    chase.mkdir(parents=True)
    (chase / "run.json").write_text(json.dumps({"source": "other"}))
    psn = root / "ubs-psn" / "20250102T000000Z"
    psn.mkdir(parents=True)
    (psn / "report.zip").write_bytes(b"PK")
    web = root / "schwab-web" / "20250103T000000Z"
    (web / "screenshots").mkdir(parents=True)
    (web / "run.json").write_text(json.dumps({"status": "complete"}))
    (web / "screenshots" / "page.png").write_bytes(b"png")
    for tree in ("chase", "ubs-psn", "schwab-web"):
        age(root / tree)


@pytest.mark.parametrize("argv", [[], ["--dry-run"], ["--item", "plaid"]])
def test_the_data_root_itself_is_refused_and_left_as_it_is(tmp_path, argv):
    root = tmp_path / "data"
    other_collectors(root)
    make_run(root / "plaid" / "bank", "20260102T000000Z", "in-progress")
    before = snapshot(root)
    with pytest.raises(SystemExit, match="holds runs that are not plaid's"):
        prune.main(["--bronze-dir", str(root), *argv])
    assert snapshot(root) == before


def test_a_run_naming_another_tree_is_refused(bronze):
    make_run(bronze / "broker", "20260102T000000Z", "in-progress")
    moved = bronze / "bank" / "20260106T000000Z"
    (bronze / "broker" / "20260102T000000Z").rename(moved)
    before = snapshot(bronze)
    with pytest.raises(SystemExit, match="names no plaid run of 'bank'"):
        prune.main(["--bronze-dir", str(bronze)])
    assert snapshot(bronze) == before


# ---- flags and edges ---------------------------------------------------------------

def test_the_debug_dir_flag_does_not_apply(bronze):
    with pytest.raises(SystemExit) as caught:
        prune.main(["--bronze-dir", str(bronze), "--debug-dir",
                    str(bronze.parent / "debug")])
    assert caught.value.code == 2


def test_help_does_not_offer_the_debug_dir_flag(capsys):
    with pytest.raises(SystemExit) as caught:
        prune.main(["--help"])
    assert caught.value.code == 0
    assert "--debug-dir" not in capsys.readouterr().out


def test_a_missing_bronze_dir_is_an_error(tmp_path):
    with pytest.raises(SystemExit, match="does not exist"):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


def test_an_empty_bronze_dir_has_nothing_to_prune(tmp_path, capsys):
    assert prune.main(["--bronze-dir", str(tmp_path)]) == 0
    assert "nothing to prune" in capsys.readouterr().out


def test_a_symlinked_item_tree_is_not_followed(tmp_path):
    root = tmp_path / "plaid"
    outside = tmp_path / "outside" / "bank"
    make_run(outside, "20260102T000000Z", "in-progress")
    root.mkdir()
    (root / "bank").symlink_to(outside, target_is_directory=True)
    assert prune.main(["--bronze-dir", str(root)]) == 0
    assert (outside / "20260102T000000Z").exists()


def test_the_validator_refuses_a_load_input(bronze):
    with pytest.raises(SystemExit, match="refusing to delete"):
        prune.validate_target(
            bronze / "bank" / "20260101T000000Z" / "accounts.json",
            bronze / "bank")
