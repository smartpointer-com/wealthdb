"""
Tests for more_detail.parse over invented "More" dialog texts in the
four layouts the site prints. Every value is invented; every CUSIP is
built from an invented base with its check digit computed here.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import more_detail  # noqa: E402


def _check_digit(base: str) -> str:
    """The CUSIP check digit, written apart from the module's: letters
    count 10 to 35, every second value doubles, and the digits of each
    value are summed."""
    def value(ch):
        return int(ch) if ch.isdigit() else ord(ch) - ord("A") + 10
    digits = ""
    for i, ch in enumerate(base):
        digits += str(value(ch) * (2 if i % 2 else 1))
    return str((10 - sum(int(d) for d in digits) % 10) % 10)


def _cusip(base: str = "ZZ9QX12B") -> str:
    return base + _check_digit(base)


def _wrong_cusip(base: str = "ZZ9QX12B") -> str:
    return base + str((int(_check_digit(base)) + 1) % 10)


def _layout_a(security, *, industry_fee=True, accrued=False):
    lines = [
        "Trade Transaction Details",
        "Some page text that is skipped",
        "Print", "Transactions", "Trade Details",
        "Trade Date", "03/04/2031",
        "Settle Date", "03/06/2031",
        *security,
        "Action", "Buy",
        "Quantity", "12",
        "Price", "$45.6700",
        "Principal", "$548.04",
        "Commission", "$0.00",
    ]
    if industry_fee:
        lines += ["Industry Fee", "$0.02"]
    if accrued:
        lines += ["Accrued Interest", "$3.21"]
    lines += ["Total", "$548.06"]
    # Indents and blank lines as the dialog's inner text prints them.
    return "\n\n".join("  " + ln + " " for ln in lines)


def _layout_b(security, *, fee=True, industry_fee=True, second_qty="7",
              text_before_trade_date=True):
    lines = [
        "More Details", "Print",
        "Sell", "SYNTHETIC WIDGET CO",
        "Description", "SYNTHETIC WIDGET CO COM",
        "Quantity", "7",
        "Price", "$12.50",
    ]
    if fee:
        lines += ["Industry fee", "$0.01"]
    lines += ["Total", "$87.49", "Some text", "More text"]
    if text_before_trade_date:
        lines += ["A line before the date"]
    lines += [
        "Trade date", "05/06/2032",
        "Settle date", "05/07/2032",
        "Order id", "987654",
        *security,
        "Quantity", second_qty,
        "Price", "$12.50",
        "Principal", "$87.50",
        "Commission", "$0.00",
    ]
    if industry_fee:
        lines += ["Industry fee", "$0.01"]
    lines += ["footnote", "An invented footnote."]
    return "\n".join(lines)


# ============================================================
# Layout A
# ============================================================

class TestLayoutA:
    def test_fields_keys_and_values_as_printed(self):
        sec = ["SYNTHETIC WIDGET CO", _cusip()]
        got = more_detail.parse(_layout_a(sec))
        assert got == {
            "trade_date": "03/04/2031", "settle_date": "03/06/2031",
            "action": "Buy", "quantity": "12", "price": "$45.6700",
            "principal": "$548.04", "commission": "$0.00",
            "industry_fee": "$0.02", "total": "$548.06",
            "security_lines": sec, "cusip": _cusip(),
        }

    def test_optional_pairs(self):
        sec = ["SYNTHETIC BOND 2.5% 01/01/40", _cusip()]
        absent = more_detail.parse(_layout_a(sec, industry_fee=False))
        assert "industry_fee" not in absent
        assert "accrued_interest" not in absent
        both = more_detail.parse(_layout_a(sec, accrued=True))
        assert both["accrued_interest"] == "$3.21"
        assert both["industry_fee"] == "$0.02"

    @pytest.mark.parametrize("n", [1, 2, 4, 6])
    def test_security_blocks_of_each_length(self, n):
        sec = [f"SYNTHETIC LINE {k}" for k in range(n - 1)] + [_cusip()]
        got = more_detail.parse(_layout_a(sec))
        assert got["security_lines"] == sec
        assert got["cusip"] == _cusip()
        assert got["action"] == "Buy"

    def test_a_block_without_a_cusip_keeps_its_lines(self):
        sec = ["SYNTHETIC WIDGET CO"]
        got = more_detail.parse(_layout_a(sec))
        assert got["security_lines"] == sec
        assert "cusip" not in got

    def test_a_wrong_check_digit_is_no_cusip(self):
        sec = ["SYNTHETIC WIDGET CO", _wrong_cusip()]
        got = more_detail.parse(_layout_a(sec))
        assert got["security_lines"] == sec
        assert "cusip" not in got

    def test_a_cusip_with_letters_in_the_issue(self):
        cusip = _cusip("Q1W2E3R4")
        got = more_detail.parse(_layout_a(["SYNTHETIC", cusip]))
        assert got["cusip"] == cusip


# ============================================================
# Layout B
# ============================================================

class TestLayoutB:
    def test_fields_and_doubled_labels(self):
        sec = ["SYNTHETIC WIDGET CO", "COMMON STOCK", _cusip()]
        got = more_detail.parse(_layout_b(sec))
        assert got == {
            "action": "Sell", "description": "SYNTHETIC WIDGET CO COM",
            "quantity": "7", "price": "$12.50", "industry_fee": "$0.01",
            "total": "$87.49", "trade_date": "05/06/2032",
            "settle_date": "05/07/2032", "order_id": "987654",
            "principal": "$87.50", "commission": "$0.00",
            "security_lines": sec, "cusip": _cusip(),
        }

    def test_a_doubled_label_with_two_values_keeps_both_in_order(self):
        got = more_detail.parse(_layout_b(["SYNTHETIC", _cusip()],
                                          second_qty="7.000"))
        assert got["quantity"] == ["7", "7.000"]
        assert got["price"] == "$12.50"

    def test_optional_fee_pairs(self):
        got = more_detail.parse(_layout_b(["SYNTHETIC", _cusip()],
                                          fee=False, industry_fee=False))
        assert "industry_fee" not in got
        one = more_detail.parse(_layout_b(["SYNTHETIC", _cusip()],
                                          fee=False))
        assert one["industry_fee"] == "$0.01"

    def test_the_trade_date_label_may_start_its_block(self):
        got = more_detail.parse(_layout_b(["SYNTHETIC", _cusip()],
                                          text_before_trade_date=False))
        assert got["trade_date"] == "05/06/2032"

    @pytest.mark.parametrize("n", [1, 2, 4, 6])
    def test_security_blocks_of_each_length(self, n):
        sec = [f"SYNTHETIC LINE {k}" for k in range(n - 1)] + [_cusip()]
        got = more_detail.parse(_layout_b(sec))
        assert got["security_lines"] == sec
        assert got["cusip"] == _cusip()

    def test_a_labelled_action_wins_over_the_line_after_print(self):
        text = _layout_b(["SYNTHETIC", _cusip()]) + "\nAction\nSell Short"
        assert more_detail.parse(text)["action"] == "Sell Short"


# ============================================================
# Dialogs with no data, unknown labels
# ============================================================

class TestNoData:
    def test_wire_dialog_parses_empty(self):
        assert more_detail.parse("Wire Transaction Details\nLoading") == {}

    def test_deposit_dialog_parses_empty(self):
        assert more_detail.parse(
            "Deposit Details\nAn invented line\nAnother line") == {}

    def test_empty_text_parses_empty(self):
        assert more_detail.parse("") == {}

    def test_an_unknown_label_is_never_guessed(self):
        text = _layout_a(["SYNTHETIC", _cusip()]).replace(
            "Commission", "Invented Surcharge")
        got = more_detail.parse(text)
        assert "commission" not in got
        assert "invented_surcharge" not in got
        assert "$0.00" not in got.values()
        assert got["principal"] == "$548.04"
        assert got["industry_fee"] == "$0.02"

    def test_a_label_followed_by_a_label_has_no_value(self):
        got = more_detail.parse("Trade Date\nSettle Date\n01/02/2033")
        assert got == {"settle_date": "01/02/2033"}
