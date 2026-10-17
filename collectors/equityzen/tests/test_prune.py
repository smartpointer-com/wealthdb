"""
Unit tests for prune.py's equityzen configuration.

The shared engine's own suite (shared/collectorkit/tests/test_prune.py)
covers the generic cases: the in-flight age guard, unreadable manifests,
bad slugs, symlinks, --dry-run and target validation. These cover what
equityzen's PruneConfig decides, over tmp_path bronze trees in equityzen's
run-dir layout (investments.json + offerings/<slug>/detail.json +
documents/<slug>/*.pdf + screenshots/ + run.json):

  * a complete dump keeps every load input, the document blobs included,
    and loses screenshots/, the captures `download --debug` writes
  * a statusless manifest counts as complete and is kept whole
  * `main()` wires that config: a dump with no run.json or a status other
    than complete is deleted, and the silver DB at the bronze root is
    never touched

Synthetic slugs / ids / bytes only — no real deal ids, company names,
share counts, or amounts.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402

OLD_TS = "20260101T010000Z"

# Synthetic deal-slug / doc-slug: sha256(id)[:16]-shaped hex, no real id.
DEAL_SLUG = "0123456789abcdef"
DOC_SLUG = "fedcba9876543210"


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, screenshots: bool = True) -> Path:
    """Build a synthetic equityzen bronze run dir with a document blob.
    ``run_json=False`` models a walk that crashed before the in-progress
    marker; ``status`` sets the run.json status field (``None`` writes a
    statusless manifest); ``screenshots`` adds the ``--debug`` capture
    dir."""
    d = root / slug
    (d / "offerings" / DEAL_SLUG).mkdir(parents=True)
    (d / "investments.json").write_text(json.dumps(
        {"ONGOING": {"data": {"buyer": {"buyerDeals": {"edges": []}}}}}))
    (d / "offerings" / DEAL_SLUG / "detail.json").write_text(json.dumps(
        {"data": {"buyer": {"buyerDeals": {"edges": []}}}}))
    (d / "documents" / DEAL_SLUG).mkdir(parents=True)
    (d / "documents" / DEAL_SLUG / f"{DOC_SLUG}.pdf").write_bytes(
        b"%PDF-1.4 fake")
    if screenshots:
        (d / "screenshots").mkdir(parents=True)
        (d / "screenshots" / "10-portfolio.html").write_text(
            "<html>portfolio</html>")
        (d / "screenshots" / "10-portfolio.png").write_bytes(b"\x89PNG fake")
    if run_json:
        (d / "run.json").write_text(json.dumps(
            {"status": status} if status is not None else {"source": "equityzen"}))
    return d


def assert_inputs_intact(d: Path) -> None:
    assert (d / "investments.json").exists()
    assert (d / "offerings" / DEAL_SLUG / "detail.json").exists()
    assert (d / "documents" / DEAL_SLUG / f"{DOC_SLUG}.pdf").exists()
    assert (d / "run.json").exists()


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
    (tmp_path / "equityzen.db").write_bytes(b"sqlite fake")
    assert prune.main(["--bronze-dir", str(tmp_path),
                       "--min-age-hours", "0"]) == 0
    assert_inputs_intact(complete)
    assert not crashed.exists()
    assert not unmarked.exists()
    assert (tmp_path / "equityzen.db").exists()
