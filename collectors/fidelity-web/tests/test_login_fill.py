"""
Tests for the signin fill — that a credential which does not land is
never submitted.

The failure these exist to stop: Fidelity renamed the username input
and turned its `autocomplete` into a token list, so every candidate
selector missed and the code fell through to a last-resort "first
visible text input" fill that verified nothing. When the PVD component
re-rendered under that fill the field ended up empty, the form
submitted blank credentials, and the run reported only "neither 2FA
input nor post-auth URL within 15s" — fifteen seconds and a wrong
diagnosis away from the real cause.

A blank submit is not a harmless no-op either: Fidelity counts it as a
failed sign-in attempt, so a nightly that makes one walks the account
towards a lockout.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


class FakeInput:
    """An input whose value can be wiped `wipes` times before it
    sticks — a component re-rendering under the fill."""

    def __init__(self, wipes=0, visible=True, count=1):
        self.wipes, self._value = wipes, ""
        self._visible, self._count = visible, count
        self.fills = 0

    # -- locator surface
    def count(self):
        return self._count

    def is_visible(self, timeout=None):
        return self._visible

    @property
    def first(self):
        return self

    def fill(self, value):
        self.fills += 1
        if self.wipes > 0:
            self.wipes -= 1
            self._value = ""
        else:
            self._value = value

    def input_value(self, timeout=None):
        return self._value

    def click(self, timeout=None):
        self.clicked = True


def test_a_value_that_lands_is_accepted_on_the_first_try():
    box = FakeInput()
    assert download._fill_verified(box, "secret-value", "password") is True
    assert box.fills == 1


def test_a_value_wiped_by_a_re_render_is_refilled():
    box = FakeInput(wipes=1)
    assert download._fill_verified(box, "secret-value", "password") is True
    assert box.fills == 2


def test_a_value_that_never_lands_is_reported_rather_than_submitted():
    box = FakeInput(wipes=99)
    assert download._fill_verified(box, "secret-value", "password") is False


def test_the_fill_never_logs_the_value(caplog):
    """It runs on a username and a password; only lengths may be
    logged."""
    caplog.set_level("DEBUG")
    download._fill_verified(FakeInput(wipes=99), "hunter2-secret", "password")
    assert "hunter2-secret" not in caplog.text


def test_a_password_that_will_not_hold_aborts_before_submitting():
    class Page:
        def locator(self, sel):
            return FakeInput(wipes=99)
    with pytest.raises(SystemExit):
        download.fill_password(Page(), "secret-value")


def test_the_form_is_never_submitted_with_an_empty_field():
    """The lockout guard: a blank submit counts against the account."""
    empty, button = FakeInput(), FakeInput()
    empty._value = ""

    class Page:
        def locator(self, sel):
            if sel == download.SEL_LOGIN_BUTTON:
                return button
            return empty
    with pytest.raises(SystemExit) as e:
        download.click_login(Page())
    assert "empty" in str(e.value)
    assert not hasattr(button, "clicked")


def test_a_filled_form_does_submit():
    """Proves the guard above is not vacuous."""
    filled, button = FakeInput(), FakeInput()
    filled._value = "something"

    class Page:
        def locator(self, sel):
            if sel == download.SEL_LOGIN_BUTTON:
                return button
            return filled
    download.click_login(Page())
    assert button.clicked is True


def test_the_username_selectors_match_a_tokenised_autocomplete():
    """`autocomplete="username webauthn"` is a space-separated token
    list, so `=` misses it and `~=` does not. That one operator would
    have survived the rename on its own."""
    cands = download.SEL_USERNAME_TEXT_INPUT_CANDIDATES
    assert "input#dom-username-input" in cands
    assert any("autocomplete~=username" in c for c in cands)
    assert not any("autocomplete=username]" in c for c in cands)
