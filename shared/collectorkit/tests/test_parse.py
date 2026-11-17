"""Tests for the CUSIP check digit of `collectorkit.parse`. Every base is
invented, apart from one widely published issuer CUSIP."""
from __future__ import annotations

import string

import pytest

from collectorkit import parse


def _oracle(base: str) -> str:
    """The check digit, written apart from the module's: each character
    is valued (digits as themselves, A-Z 10 to 35, then *, @ and # 36 to
    38), every second value is doubled, and all the digits of the values
    are summed."""
    extra = {"*": 36, "@": 37, "#": 38}

    def value(ch):
        if ch.isdigit():
            return int(ch)
        return extra[ch] if ch in extra else ord(ch) - ord("A") + 10

    digits = "".join(str(value(ch) * (2 if i % 2 else 1))
                     for i, ch in enumerate(base))
    return str((10 - sum(int(d) for d in digits) % 10) % 10)


ALPHABET = string.digits + string.ascii_uppercase + "*@#"


@pytest.mark.parametrize("base", [
    "ZZ9QX12B", "00000000", "99999999", "A*B@C#D1", "########", "12AB34CD",
    *(ch * 8 for ch in ALPHABET),
    *(ALPHABET[i:i + 8] for i in range(0, len(ALPHABET) - 8)),
])
def test_the_check_digit_agrees_with_an_independent_oracle(base):
    assert parse.cusip_check_digit(base) == _oracle(base)
    assert parse.is_cusip(base + _oracle(base))
    wrong = str((int(_oracle(base)) + 1) % 10)
    assert not parse.is_cusip(base + wrong)


def test_a_published_cusip_checks():
    assert parse.is_cusip("037833100")


@pytest.mark.parametrize("value", [
    "", "1234567", "123456789", "abcdefgh", "1234567-", "ZZ9QX12",
])
def test_what_is_no_base_has_no_check_digit(value):
    assert parse.cusip_check_digit(value) is None


@pytest.mark.parametrize("value", ["", "03783310", "0378331000",
                                   "037833lOO", "abcdefgh1"])
def test_what_is_no_cusip(value):
    assert not parse.is_cusip(value)
