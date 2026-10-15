"""The provider contract's helpers: silver rows, numbers, identifiers and
the run's cache."""

import traceback

import pytest

import wanted
from providers.base import COLUMNS, Context, dec, ident, row, us_symbol


def test_a_row_holds_every_column_but_run_at():
    r = row("counterparties", fund_ref="isin:XX0000000001", role="swap", name="Example Bank")
    assert set(r) == set(COLUMNS["counterparties"]) - {"run_at"}
    assert (r["lei"], r["payload"], r["name"]) == (None, {}, "Example Bank")
    # Each row has a payload of its own.
    assert row("lines")["payload"] is not row("lines")["payload"]
    with pytest.raises(KeyError, match="no column wieght"):
        row("lines", wieght="10")


@pytest.mark.parametrize("value,want", [
    ("1,234.50", "1234.5"), ("-1,234,567", "-1234567"), ("12.5%", "12.5"), ("1'234.5", "1234.5"),
    ("3.99E-01", "0.399"), ("-0", "0"), (60.0, "60"),
    # A comma that separates no groups of three could be a decimal mark.
    ("0,25", None), ("12,5", None), ("1,23", None), ("1,2345", None),
    ("", None), ("-", None), ("N/A", None), ("abc", None), ("NaN", None), (None, None),
])
def test_dec(value, want):
    assert dec(value) == want


def test_ident_reads_the_field_else_the_reference():
    f = wanted.Fund(ref="isin:xx0000000001 ")
    assert (ident(f, "isin"), ident(f, "cusip"), ident(f, "symbol")) == ("XX0000000001", "", "")
    s = wanted.Security(key="symbol:qabc", isin=" xx0000000002")
    assert (ident(s, "isin"), ident(s, "symbol")) == ("XX0000000002", "QABC")


def test_a_ticker_names_a_us_fund_only():
    assert us_symbol(wanted.Fund(ref="symbol:QABC")) == "QABC"
    assert us_symbol(wanted.Fund(ref="isin:US0000000001", symbol="QABC")) == "QABC"
    assert us_symbol(wanted.Fund(ref="isin:XX0000000001", symbol="QABC")) == ""


def test_once_asks_once_and_fails_again_with_its_first_traceback():
    calls = []

    def load():
        calls.append(1)
        raise ValueError("the list is gone")
    ctx = Context(client=None)
    depths = []
    for _ in range(3):
        try:
            ctx.once("list", load)
        except ValueError as e:
            depths.append(len(traceback.extract_tb(e.__traceback__)))
    assert calls == [1] and depths[0] == depths[1] == depths[2]
    assert ctx.once("other", lambda: 42) == 42 and ctx.once("other", lambda: 0) == 42
