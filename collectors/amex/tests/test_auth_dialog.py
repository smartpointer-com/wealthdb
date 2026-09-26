"""Unit tests for the passcode dialog: the delivery picker and the code
prompt, driven through injected input/output so no TTY is needed.

Synthetic values only.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import auth_dialog  # noqa: E402
from amexclient import OTP_DIGITS, ChallengeTarget  # noqa: E402

SMS = ChallengeTarget(value="0", display="Text Message  *******0000",
                      kind="sms")
EMAIL = ChallengeTarget(value="1", display="Email  e****@example.com",
                        kind="email")


def _io(*answers):
    """(input_fn, output_fn, captured_output) over a scripted stdin."""
    it = iter(answers)
    out: list[str] = []

    def input_fn(_prompt=""):
        try:
            return next(it)
        except StopIteration:
            raise EOFError from None
    return input_fn, out.append, out


# ============================================================
# choose_target
# ============================================================

def test_a_single_target_is_chosen_without_prompting():
    input_fn, output_fn, out = _io()          # no input available at all
    assert auth_dialog.choose_target((SMS,), input_fn=input_fn,
                                     output_fn=output_fn) is SMS
    assert any("Text Message" in line for line in out)


def test_several_targets_are_offered_and_the_choice_honoured():
    input_fn, output_fn, out = _io("2")
    assert auth_dialog.choose_target((SMS, EMAIL), input_fn=input_fn,
                                     output_fn=output_fn) is EMAIL
    assert any("1. Text Message" in line for line in out)
    assert any("2. Email" in line for line in out)


def test_a_junk_choice_is_re_prompted_then_accepted():
    input_fn, output_fn, out = _io("x", "9", "1")
    assert auth_dialog.choose_target((SMS, EMAIL), input_fn=input_fn,
                                     output_fn=output_fn) is SMS
    assert sum("between 1 and 2" in line for line in out) == 2


def test_repeated_junk_gives_up():
    input_fn, output_fn, _ = _io("x", "y", "z")
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.choose_target((SMS, EMAIL), input_fn=input_fn,
                                  output_fn=output_fn)


def test_no_stdin_raises_rather_than_hanging():
    # A non-interactive invocation must fail fast, not block on input.
    input_fn, output_fn, _ = _io()
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.choose_target((SMS, EMAIL), input_fn=input_fn,
                                  output_fn=output_fn)


def test_an_empty_menu_raises():
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.choose_target(())


def test_a_target_with_no_value_is_not_usable():
    blank = ChallengeTarget(value="", display="?", kind="other")
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.choose_target((blank,))


# ============================================================
# read_otp
# ============================================================

def test_a_clean_code_is_returned():
    input_fn, output_fn, _ = _io("123456")
    assert auth_dialog.read_otp(input_fn=input_fn,
                                output_fn=output_fn) == "123456"


def test_spaces_and_dashes_are_stripped():
    input_fn, output_fn, _ = _io("12 34-56")
    assert auth_dialog.read_otp(input_fn=input_fn,
                                output_fn=output_fn) == "123456"


def test_a_non_numeric_code_is_re_prompted():
    input_fn, output_fn, out = _io("abc", "123456")
    assert auth_dialog.read_otp(input_fn=input_fn,
                                output_fn=output_fn) == "123456"
    assert any("all digits" in line for line in out)


def test_a_wrong_length_code_is_re_prompted():
    # The six single-digit boxes settle the length, so catching it here
    # beats a half-filled form and a server-side rejection.
    input_fn, output_fn, out = _io("1234", "123456")
    assert auth_dialog.read_otp(input_fn=input_fn,
                                output_fn=output_fn) == "123456"
    assert any(f"{OTP_DIGITS} digits" in line for line in out)


def test_repeated_bad_codes_give_up():
    input_fn, output_fn, _ = _io("1", "2", "3")
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.read_otp(input_fn=input_fn, output_fn=output_fn)


def test_no_stdin_for_the_code_raises():
    input_fn, output_fn, _ = _io()
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.read_otp(input_fn=input_fn, output_fn=output_fn)


# ============================================================
# The dialog holds no secrets and touches nothing
# ============================================================

def test_the_demo_targets_are_synthetic():
    for target in auth_dialog._DEMO_TARGETS:
        assert "*" in target.display or "example" in target.display.lower()


def test_the_module_imports_no_network_or_browser():
    src = Path(auth_dialog.__file__).read_text(encoding="utf-8")
    for forbidden in ("import requests", "playwright", "camoufox",
                      "urllib.request"):
        assert forbidden not in src
