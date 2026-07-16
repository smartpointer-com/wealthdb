"""Unit tests for fxprofile.py, the stock-Firefox profile seeder.

The by-hand `login` path signs into a genuine Firefox, so — unlike the
driven profiles — its password manager stays on to autofill the saved
AngelList login. These checks pin that, the AngelList-specific overrides,
and the owner-only profile dir.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import fxprofile  # noqa: E402
from collectorkit import launch  # noqa: E402


@pytest.fixture(autouse=True)
def _isolated_cache_root(tmp_path, monkeypatch):
    # seed() relocates startupCache via launch.prepare_profile_dir, whose
    # cache root defaults to the REAL ~/.cache/wealthdb/startupcache when
    # the env override is unset — these tests run host-side, so without
    # this pin they leak per-test dirs into the real cache.
    monkeypatch.setenv("WEALTHDB_STARTUPCACHE_DIR",
                       str(tmp_path / "startupcache"))


def _prefs(text):
    return dict(re.findall(r'^user_pref\("([^"]+)", (.+)\);$', text, re.M))


def test_password_autofill_restored(tmp_path):
    prefs = _prefs(fxprofile.seed(tmp_path / "p", tmp_path / "d").read_text())
    for key in launch.PASSWORD_MANAGER_ON:
        assert prefs[key] == "true"


def test_angellist_overrides_preserved(tmp_path):
    docs = tmp_path / "d"
    prefs = _prefs(fxprofile.seed(tmp_path / "p", docs).read_text())
    # Session-cookie flush the extraction depends on, and the mounted
    # download dir, both survive the merge onto the shared set.
    assert prefs["browser.startup.page"] == "3"
    assert prefs["browser.download.dir"] == f'"{docs}"'


def test_profile_dir_is_owner_only(tmp_path):
    profile = tmp_path / "p"
    fxprofile.seed(profile, tmp_path / "d")
    assert profile.stat().st_mode & 0o777 == 0o700
