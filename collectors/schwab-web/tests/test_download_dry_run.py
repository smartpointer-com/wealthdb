"""
Regression tests for `download --dry-run` — it must persist NOTHING
under the bronze root (root CLAUDE.md §2).

Two layers:
  - `_open_run_dir`: the run-dir-target helper directly (fast, no page).
  - `walk(..., dry_run=True)`: the full dry-run download path with a
    MagicMock Playwright page (same session-mock style as the other
    tests), asserting the bronze dest stays empty afterwards.
"""

from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock

import pytest  # noqa: F401

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


# ============================================================
# _open_run_dir helper
# ============================================================

def test_open_run_dir_real_run_creates_under_dest(tmp_path):
    dest = tmp_path / "bronze"
    dest.mkdir()
    with download._open_run_dir(dest, "20260706T000000Z", dry_run=False) as rd:
        assert rd == dest / "20260706T000000Z"
        assert rd.is_dir()
        (rd / "run.json").write_text("{}", encoding="utf-8")
    # A real run leaves its dump in place under the bronze root.
    assert (dest / "20260706T000000Z" / "run.json").exists()


def test_open_run_dir_dry_run_persists_nothing_under_dest(tmp_path):
    dest = tmp_path / "bronze"
    dest.mkdir()
    with download._open_run_dir(dest, "20260706T000000Z", dry_run=True) as rd:
        # The scratch run dir is NOT under the bronze root.
        assert dest not in rd.parents
        assert rd.is_dir()
        # Writes inside the walk still work — they just go to scratch.
        (rd / "run.json").write_text("{}", encoding="utf-8")
    # Nothing under the bronze root, and the scratch tree is gone.
    assert list(dest.iterdir()) == []
    assert not rd.exists()


# ============================================================
# walk(dry_run=True) end-to-end
# ============================================================

def _mock_page_no_accounts():
    """A Playwright-page stand-in whose account selector yields no
    entries — enough to drive walk() through its read-only preamble
    (navigate, enumerate) and out via the empty-accounts branch."""
    page = MagicMock()
    # enumerate_accounts() reads page.locator(...).all(); [] -> no accounts.
    page.locator.return_value.all.return_value = []
    return page


def test_walk_dry_run_writes_nothing_to_bronze(tmp_path):
    dest = tmp_path / "bronze"
    dest.mkdir()

    summary = download.walk(
        _mock_page_no_accounts(),
        dest,
        mode="all",
        dry_run=True,
        screenshot_dir=None,
    )

    assert summary["dry_run"] is True
    # The invariant: a dry-run leaves no run dir / no file under --bronze-dir.
    assert list(dest.iterdir()) == []
