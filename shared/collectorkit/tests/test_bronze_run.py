"""The one writer of a download's run dir: `bronze.open_run` and `Run`.

A run dir carries an `in-progress` manifest from its creation and its
terminal status once, at the end. A dry run leaves no run dir or a
stamped one.
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


@pytest.mark.parametrize("policy", ["keep", "scratch"])
def test_an_unknown_policy_is_refused(tmp_path, policy):
    with pytest.raises(ValueError):
        bronze.open_run(tmp_path, SLUG, dry_run=True, dry_policy=policy)
    assert list(tmp_path.iterdir()) == []
