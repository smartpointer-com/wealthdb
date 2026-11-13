"""One answer to "is this run a finished dump?", over every manifest state.

`prune.classify` with the lenient rule, `bronze.run_is_loadable`,
`bronze.run_skip_reason` and `bronze.manifest_is_complete` must agree on
each state below: a loader reads a run that prune keeps, and a run is
complete only when its manifest reads.
"""
from __future__ import annotations

import os

import pytest

from collectorkit import bronze, prune

C, N, U = prune.COMPLETE, prune.NON_COMPLETE, prune.UNKNOWN

# state: (run.json body, classify state, run_is_loadable, manifest_is_complete)
STATES = {
    "missing": (None, N, False, False),
    "a directory": ("DIR", U, False, False),
    "unreadable": (b'{"status": "complete"}', U, True, False),
    "not utf-8": (b"\xff\xfe{", U, True, False),
    "not json": (b"{not json", U, True, False),
    "not an object": (b"[1, 2]", U, True, False),
    "statusless": (b"{}", C, True, True),
    "status null": (b'{"status": null}', C, True, True),
    "complete": (b'{"status": "complete"}', C, True, True),
    "in-progress": (b'{"status": "in-progress"}', N, False, False),
    "dry-run": (b'{"status": "dry-run"}', N, False, False),
    "unknown status": (b'{"status": "partial"}', N, False, False),
    "status not a string": (b'{"status": 5}', N, False, False),
}


@pytest.mark.parametrize("state", list(STATES))
def test_each_manifest_state_has_one_answer(tmp_path, state):
    body, classified, loadable, complete = STATES[state]
    if state == "unreadable" and os.geteuid() == 0:
        pytest.skip("root reads a file of mode 0")
    run = tmp_path / "20260101T000000Z"
    run.mkdir()
    manifest = run / "run.json"
    if body == "DIR":
        manifest.mkdir()
    elif body is not None:
        manifest.write_bytes(body)
    if state == "unreadable":
        manifest.chmod(0)
    try:
        assert prune.classify(run, prune.LENIENT_CLASSIFY_CFG)[0] == classified
        assert bronze.run_is_loadable(run) is loadable
        assert (bronze.run_skip_reason(run) is None) is loadable
        assert bronze.manifest_is_complete(run) is complete
    finally:
        if state == "unreadable":
            manifest.chmod(0o600)


def test_skip_reason_names_the_cause(tmp_path):
    run = tmp_path / "20260101T000000Z"
    run.mkdir()
    assert bronze.run_skip_reason(run) == "no run.json (still writing?)"
    (run / "run.json").write_bytes(b'{"status": "dry-run"}')
    assert bronze.run_skip_reason(run) == "run.json status='dry-run'"


def test_another_manifest_name(tmp_path):
    run = tmp_path / "20260101T000000Z"
    run.mkdir()
    (run / "manifest.json").write_bytes(b'{"status": "complete"}')
    assert bronze.manifest_is_complete(run, "manifest.json")
    assert not bronze.manifest_is_complete(run)


def _bogus(run_dir, meta):
    return "bogus", "a predicate with a bug"


def test_a_bad_predicate_state_fails_safe(tmp_path):
    run = tmp_path / "20260101T000000Z"
    run.mkdir()
    (run / "run.json").write_bytes(b'{"status": "in-progress"}')
    assert bronze.classify_run(run, _bogus)[0] == bronze.UNKNOWN


def test_prune_keeps_a_run_a_bad_predicate_classifies(tmp_path):
    run = tmp_path / "20200101T000000Z"
    run.mkdir()
    (run / "run.json").write_bytes(b'{"status": "in-progress"}')
    old = 1_000_000_000
    os.utime(run / "run.json", (old, old))
    os.utime(run, (old, old))
    cfg = prune.PruneConfig(is_complete=_bogus)
    assert prune.main(cfg, ["--bronze-dir", str(tmp_path)]) == 0
    assert (run / "run.json").is_file()
