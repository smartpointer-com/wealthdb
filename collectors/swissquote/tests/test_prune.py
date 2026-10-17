"""
Unit tests for prune.py (swissquote).

tmp_path bronze trees with swissquote's real run-dir layout and
SYNTHETIC content (no real customer IDs, ISINs, or balances). Swissquote
nominates debug_subdirs = ("screenshots",) — the landmark captures
`download --debug` writes. The shared engine's own suite
(shared/collectorkit/tests/test_prune.py) covers the age guard,
symlinks, corrupt manifests and --dry-run. These tests cover what
swissquote's PruneConfig decides:

  * main() reclaims screenshots/ from a complete dump and deletes a
    stale non-complete dump whole; load inputs are kept
  * statusless run.json (legacy dump): kept as complete
  * validate_target accepts screenshots/, never a load input
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, age_s: float = 0.0,
              screenshots: bool = True) -> Path:
    """Build a bronze run dir with swissquote's load inputs.

    ``status`` is the run.json status value (None writes a statusless
    manifest — a legacy dump). ``screenshots`` adds the `--debug`
    capture dir. ``age_s`` backdates every file/dir mtime (and the run
    dir's own) to now-age_s so the write-activity guard sees an
    abandoned dump; the default (0) leaves it fresh.
    """
    d = root / slug
    d.mkdir(parents=True)
    (d / "accounts.json").write_text('[{"account_product": "Trading", '
                                     '"account_external_id": "1000001"}]\n')
    (d / "positions.xls").write_bytes(b"\xd0\xcf\x11\xe0 fake-xls")
    (d / "list_of_assets.xls").write_bytes(b"\xd0\xcf\x11\xe0 fake-xls")
    (d / "position_details.json").write_text('[]\n')
    (d / "transactions_000.csv").write_text("Date;Order #\n")
    (d / "account_overview.pdf").write_bytes(b"%PDF-1.4 fake")
    docs = d / "documents"
    docs.mkdir()
    (docs / "AAAAAAAA-0000-0000-0000-000000000000.pdf").write_bytes(
        b"%PDF-1.4 fake-doc")
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "10_portfolio_page.html").write_text("<html>portfolio</html>")
        (shots / "10_portfolio_page.png").write_bytes(b"\x89PNG fake")
    if run_json:
        meta = {} if status is None else {"status": status}
        (d / "run.json").write_text(json.dumps(meta))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def assert_inputs_present(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "accounts.json").exists()
    assert (d / "positions.xls").exists()
    assert (d / "list_of_assets.xls").exists()
    assert (d / "position_details.json").exists()
    assert (d / "transactions_000.csv").exists()
    assert (d / "documents").is_dir()
    assert next((d / "documents").glob("*.pdf"), None) is not None


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


# ============================================================
# What swissquote's PruneConfig decides
# ============================================================

def test_main_reclaims_screenshots_and_noncomplete_dumps(tmp_path):
    # screenshots/ holds the --debug captures, never a load input, so a
    # complete dump gives it up and keeps the rest. A stale dump with no
    # run.json is not complete and goes whole.
    d = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert_inputs_present(d)
    assert not crashed.exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle.
    # download.py then wrote run.json only once (at the end), so its
    # presence means the dump finished: classify COMPLETE and keep its
    # load inputs. Such a dump carries no screenshots.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S,
                  screenshots=False)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


def test_validate_target_accepts_screenshots_subdir(tmp_path):
    # The one subdir shape prune may target: the --debug capture dir.
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_load_inputs(tmp_path):
    with pytest.raises(SystemExit):
        # A load input inside a run dir is never a valid target — only
        # the debug subdir is, and positions.xls is not it.
        prune.CONFIG.validate_target(tmp_path / OLD_TS / "positions.xls", tmp_path)


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-q"]))
