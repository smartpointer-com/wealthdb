"""Unit tests for the refusal exits of login._cli_two_factor: Chase answers
the challenge invocation with an error status, so no push or code is on its
way and the run ends at once. The page, the menu and the waits are stubs;
no browser, credentials or live navigation.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import auth_dialog  # noqa: E402
import login  # noqa: E402


@pytest.fixture
def flow(monkeypatch):
    """Stub every browser step of _cli_two_factor and record its captures.
    A wait evaluates its predicate once, as a wait whose condition already
    holds (or never will) ends."""
    captures = []
    monkeypatch.setattr(login, "wait_for", lambda pred, _page, _timeout: bool(pred()))
    monkeypatch.setattr(login, "_session_authed", lambda _page, _watch: False)
    monkeypatch.setattr(login, "_capture", lambda _page, _args, name: captures.append(name))
    monkeypatch.setattr(login, "_click_factor", lambda _page, _factor: True)
    monkeypatch.setattr(login, "_click_next", lambda _page: True)
    monkeypatch.setattr(login, "_first_in_frames", lambda _page, _selector: None)
    monkeypatch.setattr(login, "_locate", lambda _page, _selector: None)

    def no_otp():
        raise AssertionError("read_otp prompted after a refused invocation")

    monkeypatch.setattr(auth_dialog, "read_otp", no_otp)
    monkeypatch.setattr(auth_dialog, "parse_challenge_options",
                        lambda _body: auth_dialog.ChallengeMenu(token="t", factors=()))

    def run(factor):
        monkeypatch.setattr(auth_dialog, "choose_factor", lambda _menu: factor)
        watch = login._AuthWatch()
        watch.challenge_options = {}
        watch.invocation_status = 403
        return login._cli_two_factor(object(), watch, object()), captures

    return run


def test_a_refused_push_ends_the_run(flow):
    rc, captures = flow(auth_dialog.FACTOR_INAPP)
    assert rc == 1
    assert captures[-1] == "push-refused"


@pytest.mark.parametrize("factor", [auth_dialog.FACTOR_OTP_SMS, auth_dialog.FACTOR_OTP_VOICE])
def test_a_refused_code_ends_the_run_before_the_prompt(flow, factor):
    rc, captures = flow(factor)
    assert rc == 1
    assert captures[-1] == "code-refused"
    assert "before-otp" not in captures
