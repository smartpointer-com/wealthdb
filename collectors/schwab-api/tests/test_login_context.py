"""Unit test for login.py's browser context, built on
collectorkit.launch.camoufox_context: the profile dir and the launch
options this collector passes. The guarded close is collectorkit's and
is tested there.

No browser: `camoufox.sync_api` is stubbed into sys.modules (the real
package isn't installed in the host venv — login runs in a container).
"""
from __future__ import annotations

import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402


class _StubCamoufox:
    """The slice of camoufox.sync_api.Camoufox the context manager uses:
    it records its launch options, and enter yields a context object."""

    last = None

    def __init__(self, **kwargs):
        self.kwargs = kwargs
        _StubCamoufox.last = self

    def __enter__(self):
        return object()

    def __exit__(self, *exc):
        return None


@pytest.fixture(autouse=True)
def _stub_camoufox(monkeypatch):
    mod = types.ModuleType("camoufox.sync_api")
    mod.Camoufox = _StubCamoufox
    pkg = types.ModuleType("camoufox")
    pkg.sync_api = mod
    monkeypatch.setitem(sys.modules, "camoufox", pkg)
    monkeypatch.setitem(sys.modules, "camoufox.sync_api", mod)


def test_profile_dir_and_hardening_reach_the_launch(tmp_path):
    with login.open_camoufox_context(tmp_path, trace=False):
        pass
    kwargs = _StubCamoufox.last.kwargs
    assert kwargs["user_data_dir"] == str(tmp_path)
    assert kwargs["persistent_context"] is True
    assert kwargs["firefox_user_prefs"]["browser.cache.disk.enable"] is False
    # Schwab's sign-in runs without the humanized cursor and the geoip
    # fingerprint.
    assert kwargs["humanize"] is False
    assert kwargs["geoip"] is False
