"""
Unit tests for prune.py's angellist configuration.

The shared engine's own suite (shared/collectorkit/tests/test_prune.py)
covers the generic cases: the in-flight age guard, unreadable manifests,
bad slugs, symlinks, --dry-run and target validation. These cover what
angellist's PruneConfig decides, over tmp_path bronze trees in angellist's
layout:
  * a complete dump keeps its load inputs (captures.jsonl, run.json) and
    viewer.json, and loses screenshots/, the per-route captures
    `download --debug` writes
  * a statusless manifest counts as complete and is kept whole
  * `main()` wires that config: a dump with no run.json or a status other
    than complete is deleted, while the bronze-root siblings
    angellist-documents/ (a K-1/PDF load input) and the silver
    angellist.db are never touched
"""

from __future__ import annotations

import json
from pathlib import Path

import prune

OLD_TS = "20260101T010000Z"

# One synthetic captured GraphQL exchange — the primary load input. No
# real IDs/balances/names: a placeholder op with an empty data object.
CAPTURE_LINE = json.dumps(
    {"op": "PortfolioDashboardQuery", "variables": {}, "data": {}}) + "\n"
# Synthetic identity blob (viewer.json) — kept, never a prune target.
VIEWER_BLOB = json.dumps({"currentUser": {"slug": "synthetic-user"}})


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, screenshots: bool = True) -> Path:
    """Build a bronze run dir with angellist's artefacts. ``status=None``
    with ``run_json=True`` writes a statusless manifest. ``screenshots``
    adds the `--debug` capture dir."""
    d = root / slug
    d.mkdir(parents=True)
    (d / "captures.jsonl").write_text(CAPTURE_LINE)
    (d / "viewer.json").write_text(VIEWER_BLOB)
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "10-bootstrap.html").write_text("<html>bootstrap</html>")
        (shots / "20-acct1-portfolio.png").write_bytes(b"\x89PNG fake")
    if run_json:
        payload = ({"status": status} if status is not None
                   else {"source": "angellist", "ops": {}, "had_errors": False})
        (d / "run.json").write_text(json.dumps(payload))
    return d


def assert_inputs_intact(d: Path) -> None:
    assert (d / "captures.jsonl").exists()
    assert (d / "run.json").exists()
    assert (d / "viewer.json").exists()


def test_complete_dump_screenshots_pruned_inputs_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert prune.main(["--bronze-dir", str(tmp_path)]) == 0
    assert not (d / "screenshots").exists()
    assert_inputs_intact(d)


def test_statusless_manifest_kept_as_complete(tmp_path):
    # A download that writes no status writes its manifest only once the
    # walk finished, so a statusless run.json means the dump is complete.
    d = make_dump(tmp_path, OLD_TS, status=None, screenshots=False)
    assert prune.main(["--bronze-dir", str(tmp_path),
                       "--min-age-hours", "0"]) == 0
    assert_inputs_intact(d)


def test_validate_target_accepts_screenshots_subdir(tmp_path):
    # The one subdir shape prune may target: the --debug capture dir.
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_main_deletes_unfinished_dumps_and_nothing_at_the_root(tmp_path):
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", status="in-progress")
    unmarked = make_dump(tmp_path, "20260103T010000Z", run_json=False)
    # angellist-documents/ is a load input (K-1 CSVs/PDFs feed
    # load_documents) that sits at the bronze root beside the run dirs,
    # as does the silver DB.
    docs = tmp_path / "angellist-documents"
    docs.mkdir()
    (docs / "2097 Schedule K-1 Synthetic SPV.csv").write_text("a,b\n1,2\n")
    (docs / "2097 Schedule K-1 Synthetic SPV.pdf").write_bytes(b"%PDF-1.4 fake")
    (tmp_path / "angellist.db").write_bytes(b"sqlite fake")
    assert prune.main(["--bronze-dir", str(tmp_path),
                       "--min-age-hours", "0"]) == 0
    assert_inputs_intact(complete)
    assert not crashed.exists()
    assert not unmarked.exists()
    assert (docs / "2097 Schedule K-1 Synthetic SPV.csv").exists()
    assert (docs / "2097 Schedule K-1 Synthetic SPV.pdf").exists()
    assert (tmp_path / "angellist.db").exists()
