"""Unit tests for login.py's authenticator-code reader (no browser).

A typed code goes through the shared `cli.read_code`: separators are
stripped and a malformed entry is re-prompted a bounded number of times,
so a typo does not end a login whose credentials were already accepted.
A `--totp` value is checked once. Synthetic codes only.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402


def _typed(*lines: str):
    """An input_fn answering each prompt with the next line, then EOF."""
    answers = iter(lines)

    def input_fn(prompt: str) -> str:
        try:
            return next(answers)
        except StopIteration:
            raise EOFError from None
    return input_fn


def test_a_typed_code_is_returned_without_its_separators():
    assert login._read_totp(None, input_fn=_typed("123 456")) == "123456"
    assert login._read_totp(None, input_fn=_typed("123-456")) == "123456"


def test_a_malformed_typed_code_is_asked_for_again(capsys):
    assert login._read_totp(None, input_fn=_typed("12345", "12a456",
                                                  "654321")) == "654321"
    out = capsys.readouterr().out
    assert "6 digits (got 5)" in out
    assert "all digits" in out


def test_three_malformed_codes_stop_the_login():
    with pytest.raises(SystemExit, match="no valid code after 3 attempts"):
        login._read_totp(None, input_fn=_typed("1", "12", "123"))


def test_no_input_stops_the_login():
    with pytest.raises(SystemExit, match="no input on stdin"):
        login._read_totp(None, input_fn=_typed())


def test_a_supplied_code_is_checked_once():
    assert login._read_totp(" 123456 ") == "123456"
    with pytest.raises(SystemExit, match="exactly 6 digits"):
        login._read_totp("123 456")
