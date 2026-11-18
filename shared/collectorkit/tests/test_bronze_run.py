"""The one writer of a download's run dir: `bronze.open_run` and `Run`.

A run dir carries an `in-progress` manifest from its creation and its
terminal status once, at the end. A dry run leaves no run dir, a stamped
one, or a scratch one that is gone after the run. Coverage is recorded
per surface, and a short surface sets the exit code.
"""
from __future__ import annotations

import json

import pytest

from collectorkit import bronze

SLUG = "20260101T000000Z"
META = {"slug": SLUG, "since": "2026-01-01", "series": {"B": 1, "A": 2}}


def _bytes(run) -> bytes:
    return (run.dir / "run.json").read_bytes()


def test_the_manifest_bytes_are_the_sorted_pretty_json_of_its_fields(tmp_path):
    run = bronze.open_run(tmp_path, SLUG, meta=META)
    assert run.dir == tmp_path / SLUG
    assert _bytes(run) == (
        b'{\n  "series": {\n    "A": 2,\n    "B": 1\n  },\n'
        b'  "since": "2026-01-01",\n  "slug": "20260101T000000Z",\n'
        b'  "status": "in-progress"\n}\n')
    run.finish({**META, "rows": 3})
    assert _bytes(run) == (
        b'{\n  "rows": 3,\n  "series": {\n    "A": 2,\n    "B": 1\n  },\n'
        b'  "since": "2026-01-01",\n  "slug": "20260101T000000Z",\n'
        b'  "status": "complete"\n}\n')


def test_the_terminal_manifest_matches_a_hand_written_one(tmp_path):
    # The form every download wrote by hand: the manifest dict with its
    # status set, through atomic_write_json.
    by_hand = tmp_path / "by-hand.json"
    bronze.atomic_write_json(by_hand, {**META, "status": "complete"})
    run = bronze.open_run(tmp_path, SLUG, meta=META)
    run.finish(META)
    assert _bytes(run) == by_hand.read_bytes()


def test_a_fresh_slug_names_the_run_dir_by_default(tmp_path):
    run = bronze.open_run(tmp_path)
    assert bronze.RUN_DIR_RE.match(run.slug)
    assert run.dir == tmp_path / run.slug
    assert json.loads(_bytes(run)) == {"status": "in-progress"}


def test_the_run_is_skipped_until_it_finishes(tmp_path):
    run = bronze.open_run(tmp_path, SLUG, meta=META)
    assert not bronze.run_is_loadable(run.dir)
    run.finish()
    assert bronze.run_is_loadable(run.dir)
    assert json.loads(_bytes(run)) == {**META, "status": "complete"}


def test_update_merges_fields_and_stays_in_progress(tmp_path):
    run = bronze.open_run(tmp_path, SLUG, meta={"slug": SLUG})
    run.update(requests=[], providers=["p"])
    assert json.loads(_bytes(run)) == {
        "slug": SLUG, "requests": [], "providers": ["p"], "status": "in-progress"}
    run.finish()
    assert json.loads(_bytes(run))["status"] == "complete"


def test_finish_takes_another_terminal_status(tmp_path):
    run = bronze.open_run(tmp_path, SLUG)
    run.finish(status="partial")
    assert json.loads(_bytes(run)) == {"status": "partial"}
    assert not bronze.run_is_loadable(run.dir)


def test_writes_land_under_the_run_dir(tmp_path):
    run = bronze.open_run(tmp_path, SLUG)
    assert run.write_json("a/b.json", {"k": 1}) == run.dir / "a" / "b.json"
    assert run.write_bytes("c.bin", b"xyz") == run.dir / "c.bin"
    assert json.loads((run.dir / "a" / "b.json").read_text()) == {"k": 1}
    assert (run.dir / "c.bin").read_bytes() == b"xyz"


def test_a_dry_run_of_policy_none_writes_nothing(tmp_path):
    run = bronze.open_run(tmp_path, SLUG, dry_run=True, meta=META)
    assert run.dir is None and run.slug == SLUG
    assert run.write_json("x.json", {}) is None
    assert run.write_bytes("x.bin", b"") is None
    run.update(rows=1)
    run.finish(META)
    assert list(tmp_path.iterdir()) == []


def test_a_stamped_dry_run_keeps_its_dir_and_ends_dry_run(tmp_path):
    run = bronze.open_run(tmp_path, SLUG, dry_run=True, dry_policy="stamp",
                          meta=META)
    assert run.dir == tmp_path / SLUG
    assert json.loads(_bytes(run))["status"] == "in-progress"
    run.finish(META, status="complete")
    assert json.loads(_bytes(run))["status"] == "dry-run"
    assert not bronze.run_is_loadable(run.dir)


def test_a_scratch_dry_run_writes_outside_bronze_and_is_removed(tmp_path):
    with bronze.open_run(tmp_path, SLUG, dry_run=True,
                         dry_policy="scratch", meta=META) as run:
        assert run.dir.name == SLUG
        assert not run.dir.is_relative_to(tmp_path)
        run.write_json("x.json", {})
        run.finish(META)
        assert json.loads(_bytes(run))["status"] == "dry-run"
        scratch = run.dir
    assert not scratch.exists()
    assert list(tmp_path.iterdir()) == []


def test_a_crash_leaves_the_in_progress_dir_by_default(tmp_path):
    with pytest.raises(RuntimeError), bronze.open_run(tmp_path, SLUG) as run:
        raise RuntimeError("walk failed")
    assert json.loads(_bytes(run))["status"] == "in-progress"


def test_the_remove_policy_deletes_a_crashed_run_dir(tmp_path):
    with pytest.raises(RuntimeError), bronze.open_run(
            tmp_path, SLUG, on_crash="remove") as run:
        run.write_json("x.json", {})
        raise RuntimeError("walk failed")
    assert not run.dir.exists()


def test_the_remove_policy_keeps_a_finished_run_and_a_clean_exit(tmp_path):
    with pytest.raises(RuntimeError), bronze.open_run(
            tmp_path, SLUG, on_crash="remove") as run:
        run.finish()
        raise RuntimeError("after the manifest")
    assert json.loads(_bytes(run))["status"] == "complete"
    with bronze.open_run(tmp_path, "20260101T000001Z",
                         on_crash="remove") as other:
        pass
    assert json.loads(_bytes(other))["status"] == "in-progress"


@pytest.mark.parametrize("kwargs", [{"dry_policy": "keep"}, {"on_crash": "delete"}])
def test_an_unknown_policy_is_refused(tmp_path, kwargs):
    with pytest.raises(ValueError):
        bronze.open_run(tmp_path, SLUG, **kwargs)
    assert list(tmp_path.iterdir()) == []


def test_no_coverage_key_until_a_surface_is_covered(tmp_path):
    run = bronze.open_run(tmp_path, SLUG, meta=META)
    run.finish(META)
    assert "coverage" not in json.loads(_bytes(run))
    assert run.coverage() == {} and run.exit_code() == 0


def test_coverage_lists_each_surface_and_its_gaps(tmp_path):
    run = bronze.open_run(tmp_path, SLUG)
    run.cover("statements", ok=True)
    run.cover("activity", ok=True)
    run.cover("activity", ok=False, gap="2026-01-01..2026-01-31")
    run.cover("activity", ok=False)
    assert run.coverage() == {
        "statements": {"complete": True, "gaps": []},
        "activity": {"complete": False, "gaps": ["2026-01-01..2026-01-31"]}}
    run.update()
    assert json.loads(_bytes(run))["coverage"] == run.coverage()
    run.finish({"slug": SLUG})
    assert json.loads(_bytes(run)) == {
        "slug": SLUG, "status": "complete", "coverage": run.coverage()}
    assert bronze.run_is_loadable(run.dir)
    assert run.exit_code() == bronze.EXIT_SHORT


def test_a_covered_run_with_no_short_surface_exits_zero(tmp_path):
    run = bronze.open_run(tmp_path, SLUG)
    run.cover("activity", ok=True)
    assert run.exit_code() == 0
