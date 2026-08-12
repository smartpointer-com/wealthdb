"""Unit tests for mdsui.activate — the activation-strategy ordering for
Chase's MDS controls (zero-box host, open shadow root). No browser: the
strategies are stubbed.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import mdsui  # noqa: E402


class _Inner:
    """A boxed clickable descendant inside the host's open shadow root."""

    def __init__(self, count=1, visible=True, calls=None):
        self._count = count
        self._visible = visible
        self._calls = calls

    @property
    def first(self):
        return self

    def count(self):
        return self._count

    def is_visible(self):
        return self._visible

    def click(self, timeout=None):
        if self._calls is not None:
            self._calls.append("inner-click")


class _Host:
    def __init__(self, calls, inner=None):
        self._calls = calls
        self._inner = inner if inner is not None else _Inner(count=0)

    def locator(self, selector):
        return self._inner


def test_activate_host_click_wins(monkeypatch):
    monkeypatch.setattr(mdsui, "click", lambda *a, **k: True)
    monkeypatch.setattr(mdsui, "_shadow_click",
                        lambda *a, **k: pytest.fail("should not reach shadow"))
    assert mdsui.activate(object(), "#x") is True


def test_activate_shadow_click_wins(monkeypatch):
    # The item's handler is on an element inside the open shadow — clicking it
    # (native .click()) comes before the boxed-descendant path.
    monkeypatch.setattr(mdsui, "click", lambda *a, **k: False)
    monkeypatch.setattr(mdsui, "_shadow_click", lambda *a, **k: True)
    monkeypatch.setattr(mdsui, "first_in_frames",
                        lambda *a, **k: pytest.fail("should not reach host"))
    assert mdsui.activate(object(), "#sms") is True


def test_activate_clicks_boxed_descendant(monkeypatch):
    calls = []
    monkeypatch.setattr(mdsui, "click", lambda *a, **k: False)
    monkeypatch.setattr(mdsui, "_shadow_click", lambda *a, **k: False)
    monkeypatch.setattr(mdsui, "first_in_frames",
                        lambda *a, **k: _Host(calls, _Inner(count=1, calls=calls)))
    assert mdsui.activate(object(), "#sms") is True
    assert calls == ["inner-click"]


def test_activate_none_without_host(monkeypatch):
    monkeypatch.setattr(mdsui, "click", lambda *a, **k: False)
    monkeypatch.setattr(mdsui, "_shadow_click", lambda *a, **k: False)
    monkeypatch.setattr(mdsui, "first_in_frames", lambda *a, **k: None)
    assert mdsui.activate(object(), "#x") is False


class _EvalFrame:
    def __init__(self, url, result):
        self.url = url
        self._result = result

    def evaluate(self, js, arg=None):
        return self._result


class _EvalPage:
    def __init__(self, frames):
        self.frames = frames


def test_deep_click_text_clicks_in_chase_frame():
    # The deep-click JS returns a descriptor string when it clicked; truthy
    # → True. Only chase frames are consulted.
    page = _EvalPage([_EvalFrame("https://secure05c.chase.com/x", "a[role=link]")])
    assert mdsui.deep_click_text(page, "Get a text") is True


def test_deep_click_text_skips_foreign_and_reports_miss():
    assert mdsui.deep_click_text(
        _EvalPage([_EvalFrame("https://evil.example/x", "hit")]), "t") is False
    assert mdsui.deep_click_text(
        _EvalPage([_EvalFrame("https://secure.chase.com/x", None)]), "t") is False


class _RoleFrame:
    def __init__(self, url, count):
        self.url = url
        self._count = count
        self.clicked = False

    def get_by_role(self, role, name=None, exact=False):
        return self

    @property
    def first(self):
        return self

    def count(self):
        return self._count

    def scroll_into_view_if_needed(self, timeout=None):
        pass

    def click(self, timeout=None):
        self.clicked = True


def test_click_role_real_clicks_the_anchor():
    # The proven path: a real Playwright click on get_by_role in the chase
    # frame that has a match.
    hit = _RoleFrame("https://secure05c.chase.com/x", 1)
    page = _EvalPage([_RoleFrame("https://secure.chase.com/x", 0), hit])
    assert mdsui.click_role(page, "link", "Confirm using our mobile app") is True
    assert hit.clicked is True


def test_click_role_skips_foreign_and_missing():
    assert mdsui.click_role(
        _EvalPage([_RoleFrame("https://evil.example/x", 1)]), "link", "x") is False
    assert mdsui.click_role(
        _EvalPage([_RoleFrame("https://secure.chase.com/x", 0)]), "link", "x") is False


def test_chase_frames_filters_by_origin():
    page = _EvalPage([
        _EvalFrame("https://widgets.example.net/x", None),      # foreign
        _EvalFrame("https://secure.chase.com/x", None),         # kept
        _EvalFrame("https://secure05c.chase.com/y", None),      # kept
    ])
    hosts = [f.url for f in mdsui.chase_frames(page)]
    assert hosts == ["https://secure.chase.com/x", "https://secure05c.chase.com/y"]
