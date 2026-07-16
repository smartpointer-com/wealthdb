"""Unit test for login.py's guarded browser context.

Regression for a masked failure: the browser crashed right after launch,
every page call in the with-body raised TargetClosedError, and then the
context cleanup raised its own TargetClosedError (`from None`) — burying
the body's exception, the actual diagnosis. The guarded close must let
the body's exception propagate and only log the cleanup failure.

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
    enter yields a context object; exit always raises, the way Playwright
    does when the browser already died."""

    last = None

    def __init__(self, **kwargs):
        self.kwargs = kwargs
        self.exited = False
        _StubCamoufox.last = self

    def __enter__(self):
        return object()

    def __exit__(self, *exc):
        self.exited = True
        raise RuntimeError(
            "BrowserContext.close: Target page, context or browser "
            "has been closed")


@pytest.fixture(autouse=True)
def _stub_camoufox(monkeypatch):
    mod = types.ModuleType("camoufox.sync_api")
    mod.Camoufox = _StubCamoufox
    pkg = types.ModuleType("camoufox")
    pkg.sync_api = mod
    monkeypatch.setitem(sys.modules, "camoufox", pkg)
    monkeypatch.setitem(sys.modules, "camoufox.sync_api", mod)


def test_body_error_survives_a_failing_close(tmp_path):
    with pytest.raises(ValueError, match="the real error"):
        with login.open_camoufox_context(tmp_path, trace=False):
            raise ValueError("the real error")
    assert _StubCamoufox.last.exited          # cleanup ran, just guarded


def test_clean_body_still_surfaces_nothing_but_a_warning(tmp_path, caplog):
    # A close failure after a *successful* body must not raise either —
    # the flow's result is already decided by then.
    with login.open_camoufox_context(tmp_path, trace=False):
        pass
    assert _StubCamoufox.last.exited
    assert any("browser cleanup failed" in r.message for r in caplog.records)


def test_profile_dir_and_hardening_reach_the_launch(tmp_path):
    with login.open_camoufox_context(tmp_path, trace=False):
        pass
    kwargs = _StubCamoufox.last.kwargs
    assert kwargs["user_data_dir"] == str(tmp_path)
    assert kwargs["persistent_context"] is True
    assert kwargs["firefox_user_prefs"]["browser.cache.disk.enable"] is False
