"""collectorkit.money — the display form of an amount, read to a number.
Synthetic values only."""
import pytest

from collectorkit import money


@pytest.mark.parametrize("raw, want", [
    ("$1,234.56", 1234.56),
    ("-$5.00", -5.0),
    ("$-2,953", -2953.0),
    ("(1,234.00)", -1234.0),
    ("($12.50)", -12.5),
    ("  42 ", 42.0),
    ("0", 0.0),
    (12.5, 12.5),
    (7, 7.0),
    (None, None),
    ("", None),
    ("   ", None),
    ("()", None),
    ("-", None),
    ("n/a", None),
])
def test_parse_money(raw, want):
    assert money.parse_money(raw) == want


def test_parentheses_negate_whatever_sign_is_inside():
    # The accounting parentheses flip the sign the number carries, so a
    # minus inside them reads positive: the reading every copy it replaced
    # already had.
    assert money.parse_money("(-5.00)") == 5.0


@pytest.mark.parametrize("raw, want", [
    ("$1,234.56", 123456),
    ("(273)", -27300),
    ("$-2,953", -295300),
    ("0", 0),
    ("", None),
    ("—", None),
    (None, None),
])
def test_parse_money_cents(raw, want):
    assert money.parse_money_cents(raw) == want


def test_to_cents_rounds_like_round():
    assert money.to_cents(1.005) == round(1.005 * 100)
    assert money.to_cents(None) is None
