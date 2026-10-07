"""Tests for pdf_parsers.

Tests work at the regex / text-assembly layer using synthetic
labels and synthetic pre-extracted text fixtures, so they don't
require a real PDF on disk or pdfplumber's heavy lifting. All
identifiers are placeholders per AGENTS.md §4.
"""
from __future__ import annotations

import json

import pytest

from pdf_parsers import (
    _stmt_security,
    _advice_trx_no,
    _stmt_is_internal_transfer,
    _stmt_split_multi,
    parse_account_statement_text,
    parse_account_statement_transactions_pages,
    parse_capital_call_text,
    parse_contract_note_text,
    parse_label_statement_of_assets,
    parse_maturity_notice_text,
    parse_payment_advice_text,
    parse_statement_of_assets_text,
    statement_of_assets_body_meta,
)


# ============================================================
# Account-Statement MOVEMENT parser
# ============================================================
#
# The movement parser is WORD-based: it disambiguates the Debits vs
# Credits column purely from x-position, so a text fixture isn't
# enough — we build a fake pdfplumber document whose pages return
# synthetic `extract_words()` output at the SAME column geometry the
# real UBS statements use (Debits right-edge ~329, Credits ~414,
# Value date x0 ~432, Balance right-edge ~553). All amounts / dates /
# the IBAN are synthetic placeholders per AGENTS.md §4.

# Synthetic IBAN (all-zero placeholder, valid CH-IBAN shape).
_SYN_IBAN = "CH00 0000 0000 0000 0000 1"
_SYN_COUNTER_IBAN = "CH00 0000 0000 0000 0000 2"

# Ledger column anchor bands (x0, x1) mirroring the real layout.
_BAND = {
    "debit": (289, 329),
    "credit": (374, 414),
    "vdate": (432, 468),
    "bal": (508, 553),
}
_HEADER_TOP = 100.0


def _w(text: str, x0: float, x1: float, top: float) -> dict:
    return {"text": text, "x0": x0, "x1": x1, "top": top}


def _band_word(text: str, band: str, top: float) -> dict:
    x0, x1 = _BAND[band]
    return _w(text, x0, x1, top)


def _header_row(top: float = _HEADER_TOP) -> list[dict]:
    return [
        _w("Date", 42, 62, top), _w("Information", 85, 135, top),
        _w("Debits", 302, 329, top), _w("Credits", 384, 414, top),
        _w("Value", 430, 454, top), _w("date", 457, 476, top),
        _w("Balance", 520, 553, top),
    ]


def _info_words(info: str, top: float) -> list[dict]:
    ws, x = [], 85.0
    for tok in info.split():
        wdt = len(tok) * 6
        ws.append(_w(tok, x, x + wdt, top))
        x += wdt + 4
    return ws


class _FakePage:
    def __init__(self, words: list[dict], text: str):
        self._words = words
        self._text = text

    def extract_words(self, **_kw) -> list[dict]:
        return list(self._words)

    def extract_text(self, **_kw) -> str:
        return self._text


class _FakePDF:
    def __init__(self, pages: list[_FakePage]):
        self.pages = pages


# Page-header text used only for IBAN + currency detection.
_PAGE_TEXT = f"UBS personal account CHF\nIBAN {_SYN_IBAN}\n"


def _build_statement_pdf() -> _FakePDF:
    """A synthetic 2-page CHF statement exercising: credit deposit
    (+ counterparty + counter-IBAN continuation), a debit
    e-banking order, a space-separated debit that drives the balance
    NEGATIVE (trailing-minus), a printed Closing balance, and a
    post-closing 'not included' trailer booking (next period)."""
    # Page 1: opening + two movements.
    p1 = _header_row()
    p1 += [_w("01.10.21", 42, 77, 120), *_info_words("Opening balance", 120),
           _band_word("1000.00", "bal", 120)]
    # 04.10.21 CREDIT +12345.00 -> 13345.00, with continuation lines.
    p1 += [_w("04.10.21", 42, 77, 140), *_info_words("CREDIT", 140),
           _band_word("12345.00", "credit", 140),
           _w("04.10.21", 432, 468, 140),
           _band_word("13345.00", "bal", 140)]
    p1 += [_w("JOHN", 85, 110, 155), _w("DOE", 112, 130, 155)]
    p1 += [_w(_SYN_COUNTER_IBAN, 85, 200, 170)]
    # 10.10.21 E-BANKING PAYMENT ORDER -2345.00 -> 11000.00.
    p1 += [_w("10.10.21", 42, 77, 200),
           *_info_words("E-BANKING PAYMENT ORDER", 200),
           _band_word("2345.00", "debit", 200),
           _w("10.10.21", 432, 468, 200),
           _band_word("11000.00", "bal", 200)]

    # Page 2: a space-separated debit driving the balance negative,
    # the Closing balance, then the post-closing trailer.
    p2 = _header_row()
    # 15.10.21 SHARE debit '11 016.05' (space-separated) -> 16.05-.
    p2 += [_w("15.10.21", 42, 77, 120), *_info_words("SHARE", 120),
           _w("11", 289, 304, 120), _w("016.05", 306, 329, 120),
           _w("15.10.21", 432, 468, 120),
           _w("16.05-", 520, 553, 120)]
    p2 += [_w("31.10.21", 42, 77, 160), *_info_words("Closing balance", 160),
           _w("16.05-", 520, 553, 160)]
    p2 += [_w("The", 42, 55, 180), _w("following", 57, 100, 180),
           _w("bookings", 102, 140, 180)]  # trailer preamble (no date)
    p2 += _header_row(200)
    # 02.11.21 CREDIT +5000.00 (NEXT period; post-closing trailer).
    p2 += [_w("02.11.21", 42, 77, 220), *_info_words("CREDIT", 220),
           _band_word("5000.00", "credit", 220),
           _w("02.11.21", 432, 468, 220),
           _band_word("4983.95", "bal", 220)]

    return _FakePDF([_FakePage(p1, _PAGE_TEXT), _FakePage(p2, _PAGE_TEXT)])


class TestAccountStatementTransactions:
    def _rows(self):
        return parse_account_statement_transactions_pages(
            _build_statement_pdf(), "synthetic-token")

    def test_extracts_all_movements(self):
        rows = self._rows()
        assert len(rows) == 4  # 3 main + 1 trailer

    def test_debit_credit_disambiguation(self):
        rows = self._rows()
        by_kind = {r["description_kind"]: r for r in rows if not r["post_closing"]}
        # CREDIT is a credit-only row.
        assert by_kind["CREDIT"]["amount_credit"] == 12345.00
        assert by_kind["CREDIT"]["amount_debit"] is None
        # E-BANKING PAYMENT ORDER is a debit-only row.
        assert by_kind["E-BANKING PAYMENT ORDER"]["amount_debit"] == 2345.00
        assert by_kind["E-BANKING PAYMENT ORDER"]["amount_credit"] is None

    def test_space_separated_amount_and_negative_balance(self):
        rows = self._rows()
        share = next(r for r in rows if r["description_kind"] == "SHARE")
        assert share["amount_debit"] == 11016.05        # '11 016.05' merged
        assert share["running_balance"] == -16.05        # trailing-minus

    def test_currency_and_account(self):
        rows = self._rows()
        assert rows[0]["currency_iso"] == "CHF"
        assert rows[0]["account_external_id"] == "CH0000000000000000001"

    def test_continuation_counterparty_and_counter_account(self):
        rows = self._rows()
        credit = next(r for r in rows
                      if r["description_kind"] == "CREDIT" and not r["post_closing"])
        assert credit["counterparty"] == "JOHN DOE"
        assert credit["counter_account"] == "CH0000000000000000002"

    def test_post_closing_trailer_flagged(self):
        rows = self._rows()
        trailer = [r for r in rows if r["post_closing"]]
        assert len(trailer) == 1
        # Next-period booking (November), the only trailer row.
        assert trailer[0]["amount_credit"] == 5000.00

    def test_reconciliation_passes(self):
        rows = self._rows()
        # Opening 1000 + 12345 − 2345 − 11016.05 = −16.05 = closing.
        assert all(r["reconciled"] for r in rows)

    def test_booking_dates_and_value_dates(self):
        rows = self._rows()
        from datetime import datetime, timezone
        credit = next(r for r in rows
                      if r["description_kind"] == "CREDIT" and not r["post_closing"])
        d = datetime.fromtimestamp(credit["booking_date"], timezone.utc)
        assert (d.year, d.month, d.day) == (2021, 10, 4)


class TestInternalTransferDetection:
    """Intra-portfolio reshuffles (mandate funding/reduction, book
    transfers) are re-tagged so gold nets them instead of counting
    them as external deposits/withdrawals. Genuine external credits
    keep their booking type."""

    def test_mandate_funding_is_internal(self):
        assert _stmt_is_internal_transfer(
            "CREDIT", ["INCREAS US EQUITY PORTFOLIO", "P. HOLDER"])
        assert _stmt_is_internal_transfer(
            "PAYMENT ORDER BY TELEPHONE",
            ["UEBERTRAG", "REDUKTION US EQUITY MANDAT"])
        assert _stmt_is_internal_transfer(
            "SPECIAL PAYMENT ORDER", ["REDUCTION INVESTMENT PORTFOLIO"])

    def test_external_credit_stays_external(self):
        # Incoming external bank transfer — no internal marker.
        assert not _stmt_is_internal_transfer(
            "CREDIT", ["SOME EXTERNAL BANK", "1 times Incoming SIC-payment"])
        assert not _stmt_is_internal_transfer("CREDIT", ["SALARY PAYER LTD"])

    def test_only_cash_flow_types_eligible(self):
        # A securities settlement referencing a portfolio must NOT be
        # re-tagged (it is already excluded from flows as a buy/sell).
        assert not _stmt_is_internal_transfer("SHARE", ["MANAGE US EQ PORTFOLIO"])
        assert not _stmt_is_internal_transfer("DIVIDEND", ["MANAGE PORTFOLIO"])


# ============================================================
# Bundled payment orders
# ============================================================
#
# The statement books a batch of e-banking payments as ONE movement
# carrying the batch total, with the beneficiaries listed under it and
# a "<N> times <rail>" trailer closing the list. Beneficiaries below
# are invented; the shapes are the statement's.

def _cont(*lines: str) -> list[str]:
    return list(lines)


class TestSplitBundledOrder:
    def test_a_batch_becomes_one_entry_per_payment(self):
        legs = _stmt_split_multi(_cont(
            "EXAMPLE DENTAL AG 111.11",
            "CH 0000 EXAMPLETOWN",
            "NORTHWIND CLINIC 825.26",
            "CH 0000 EXAMPLEBURG",
            "2 times E-Banking CHF domestic",
        ), 936.37)
        assert [x["amount"] for x in legs] == [111.11, 825.26]
        # The amount comes off the name, and the address stays with it.
        assert legs[0]["lines"] == ["EXAMPLE DENTAL AG", "CH 0000 EXAMPLETOWN"]
        assert legs[1]["lines"] == ["NORTHWIND CLINIC", "CH 0000 EXAMPLEBURG"]

    def test_a_beneficiary_block_runs_as_long_as_it_needs(self):
        # The amount sits on the first line of the block, so a name that
        # wraps must not be read as a payment of its own.
        legs = _stmt_split_multi(_cont(
            "EXAMPLE INTERIORS 55 434.17",
            "GMBH",
            "EXAMPLE STREET 35A",
            "AT 0000 EXAMPLESTADT",
            "NORTHWIND CLINIC 12 503.00",
            "CH 0000 EXAMPLEBURG",
            "2 times E-Banking SEPA",
        ), 67937.17)
        assert len(legs) == 2
        assert legs[0]["lines"][:2] == ["EXAMPLE INTERIORS", "GMBH"]
        assert legs[0]["amount"] == 55434.17

    def test_an_address_that_ends_in_a_postcode_opens_no_payment(self):
        # The decimals are what separate an amount from a postcode.
        legs = _stmt_split_multi(_cont(
            "EXAMPLE DENTAL AG 222.22",
            "EXAMPLE STREET 3, 9999",
            "EXAMPLE DENTAL AG 66.66",
            "EXAMPLE STREET 3, 9999",
            "2 times E-Banking CHF domestic",
        ), 288.88)
        assert [x["amount"] for x in legs] == [222.22, 66.66]

    def test_page_furniture_after_the_trailer_belongs_to_no_payment(self):
        legs = _stmt_split_multi(_cont(
            "NORTHWIND CLINIC 44 251.00",
            "CH 0000 EXAMPLEBURG",
            "EXAMPLE DENTAL AG 11 758.13",
            "CH 0000 EXAMPLETOWN",
            "2 times E-Banking SEPA",
            "Form without signature Page 1 / 4",
            "AAAAAA00 / 000000 / EXAMPLE0000000000000 01.01.2098",
        ), 56009.13)
        assert len(legs) == 2
        assert all("Form without signature" not in ln
                   for x in legs for ln in x["lines"])

    def test_a_single_order_is_not_a_batch(self):
        assert _stmt_split_multi(_cont(
            "EXAMPLE DENTAL AG",
            "CH 0000 EXAMPLETOWN",
            "1 times E-Banking CHF domestic",
        ), 111.11) is None

    def test_a_movement_with_no_trailer_is_not_a_batch(self):
        assert _stmt_split_multi(_cont("EXAMPLE DENTAL AG"), 111.11) is None

    def test_a_split_that_cannot_be_proved_is_refused(self):
        # Both of the trailer's claims are checked, because either alone
        # is too weak: the count catches a merged pair, the total catches
        # a misread amount.
        wrong_count = _cont(
            "EXAMPLE DENTAL AG 111.11",
            "NORTHWIND CLINIC 825.26",
            "3 times E-Banking CHF domestic",
        )
        assert _stmt_split_multi(wrong_count, 936.37) is None
        wrong_total = _cont(
            "EXAMPLE DENTAL AG 111.11",
            "NORTHWIND CLINIC 825.26",
            "2 times E-Banking CHF domestic",
        )
        assert _stmt_split_multi(wrong_total, 999.99) is None
        # ... and a movement whose total was never read cannot be proved.
        assert _stmt_split_multi(wrong_total, None) is None

    def test_text_before_the_first_amount_is_an_unknown_shape(self):
        assert _stmt_split_multi(_cont(
            "SOMETHING UNEXPECTED",
            "EXAMPLE DENTAL AG 111.11",
            "NORTHWIND CLINIC 825.26",
            "2 times E-Banking CHF domestic",
        ), 936.37) is None


def _bundle_pdf() -> _FakePDF:
    """A one-page statement whose only movement is a two-payment batch."""
    p1 = _header_row()
    p1 += [_w("01.10.21", 42, 77, 120), *_info_words("Opening balance", 120),
           _band_word("1000.00", "bal", 120)]
    p1 += [_w("04.10.21", 42, 77, 140),
           *_info_words("MULTI E-BANKING ORDER", 140),
           _band_word("300.00", "debit", 140),
           _w("04.10.21", 432, 468, 140),
           _band_word("700.00", "bal", 140)]
    p1 += [_w("EXAMPLE", 85, 130, 155), _w("DENTAL", 132, 170, 155),
           _w("AG", 172, 185, 155), _w("100.00", 300, 329, 155)]
    p1 += [_w(_SYN_COUNTER_IBAN, 85, 200, 168)]
    p1 += [_w("NORTHWIND", 85, 145, 182), _w("CLINIC", 147, 180, 182),
           _w("200.00", 300, 329, 182)]
    p1 += [_w("CH", 85, 95, 195), _w("0000", 97, 120, 195),
           _w("EXAMPLEBURG", 122, 190, 195)]
    p1 += [_w("2", 85, 92, 208), _w("times", 94, 120, 208),
           _w("E-Banking", 122, 170, 208), _w("CHF", 172, 190, 208),
           _w("domestic", 192, 235, 208)]
    p1 += [_w("31.10.21", 42, 77, 230), *_info_words("Closing balance", 230),
           _band_word("700.00", "bal", 230)]
    return _FakePDF([_FakePage(p1, _PAGE_TEXT)])


class TestBundledOrderReachesTheLoaderSplit:
    def _rows(self):
        return parse_account_statement_transactions_pages(
            _bundle_pdf(), "synthetic-token")

    def test_the_batch_row_is_replaced_by_its_payments(self):
        rows = self._rows()
        assert len(rows) == 2
        assert [r["amount_debit"] for r in rows] == [100.00, 200.00]
        # The batch total is gone as a row and survives as the sum.
        assert sum(r["amount_debit"] for r in rows) == 300.00
        assert all(r["description_kind"] == "MULTI E-BANKING ORDER" for r in rows)

    def test_each_payment_names_its_own_beneficiary(self):
        rows = self._rows()
        assert rows[0]["counterparty"] == "EXAMPLE DENTAL AG"
        assert rows[1]["counterparty"] == "NORTHWIND CLINIC"
        # A counter-account belongs to the payment that carries it, not
        # to every payment the batch happened to include.
        assert rows[0]["counter_account"] == "CH0000000000000000002"
        assert rows[1]["counter_account"] is None

    def test_each_payment_carries_only_its_own_narrative(self):
        import json
        rows = self._rows()
        first = json.loads(rows[0]["payload"])
        assert first["continuation"] == ["EXAMPLE DENTAL AG",
                                         _SYN_COUNTER_IBAN]
        assert first["multi_leg"] == {"index": 1, "count": 2,
                                      "rail": "E-Banking CHF domestic"}
        assert json.loads(rows[1]["payload"])["multi_leg"]["index"] == 2

    def test_the_printed_balance_belongs_to_the_last_payment(self):
        # The balances between the payments were never printed, and
        # deriving them would state something the statement does not.
        rows = self._rows()
        assert rows[0]["running_balance"] is None
        assert rows[1]["running_balance"] == 700.00

    def test_the_statement_still_reconciles(self):
        # Reconciliation reads the movement the statement printed, so
        # splitting it must not disturb the chain: 1000 − 300 = 700.
        assert all(r["reconciled"] for r in self._rows())

    def test_the_payments_carry_the_batch_total_for_the_id(self):
        rows = self._rows()
        assert [r["multi_leg_index"] for r in rows] == [1, 2]
        assert all(r["multi_parent_debit"] == 300.00 for r in rows)


# ---- A statement that arrives without its listing row -------------

class TestStatementOfAssetsBodyMetadata:
    """UBS serves most Statements of assets through the e-banking
    archive, where a listing row carries the as-of date and the
    portfolio. A statement the bank produces on request is delivered by
    hand and has no listing row at all — the document's own first page
    is then the only place those two facts are written."""

    HEADER = "\n".join([
        "UBS Switzerland AG",
        "Statement of assets",
        "As of 7 March 2024",
        "Portfolio 999-00000000-06, valued in Swiss Franc (CHF)",
    ])

    def test_the_document_names_its_own_date_and_portfolio(self):
        meta = statement_of_assets_body_meta(self.HEADER)
        assert meta is not None
        assert meta["as_of_str"] == "2024-03-07"
        assert meta["account_number_prefix"] == "999-00000000"
        assert meta["portfolio_number"] == "06"

    @pytest.mark.parametrize("drop", [
        "Statement of assets",
        "As of 7 March 2024",
        "Portfolio 999-00000000-06, valued in Swiss Franc (CHF)",
    ])
    def test_all_three_anchors_or_none(self, drop):
        """Half an identification is worse than none: it would file a
        document under a date or a portfolio it never stated."""
        text = "\n".join(ln for ln in self.HEADER.splitlines() if ln != drop)
        assert statement_of_assets_body_meta(text) is None

    def test_a_document_of_another_kind_is_declined(self):
        assert statement_of_assets_body_meta(
            "Account Statement\nAs of 7 March 2024\n"
            "Portfolio 999-00000000-06, valued in Swiss Franc (CHF)") is None

    def test_the_valued_as_of_note_does_not_win(self):
        """Year-end statements print a note dating the VALUATION a day
        or two before the statement. The listing label states the
        header's date, so the header is what the body must read — or
        the same document would land on two different dates depending
        on which road read it."""
        text = ("Statement of assets\n"
                "As of 31 December 2024\n"
                "Portfolio 999-00000000-06, valued in Swiss Franc (CHF)\n"
                "Important notes\n"
                "- Statement of assets valued as of 30.12.2024\n")
        meta = statement_of_assets_body_meta(text)
        assert meta["as_of_str"] == "2024-12-31"

    def test_a_statement_with_no_label_is_walked_from_its_body(self):
        """End to end: the positions walk needs no label when the
        document identifies itself."""
        text = "\n".join([
            self.HEADER,
            "Valued in CHF",
            "Detailed positions",
            "100 Reg.shs Placeholder Equity AG CHF 10.000000 12.50 25.00% 1 250 5.00",
            "Valor 111 - ISIN XX0000000011",
            "Additional information Abbreviations",
        ])
        rows = parse_statement_of_assets_text(text, "<doc-token>", "")
        assert [r["instrument_isin"] for r in rows] == ["XX0000000011"]
        assert rows[0]["portfolio_external_id"] == "0999000000000006"

    def test_a_listing_label_still_wins_where_there_is_one(self):
        """The body is a fallback, not a second opinion: every document
        the archive served must be read exactly as it always was."""
        label = ("\u200d Statement of assets as of 31032026 "
                 "02.04.2026 02 April 2026 P. Placeholder 999-00000000-05 300 KB")
        text = "\n".join([
            self.HEADER,
            "Valued in CHF",
            "Detailed positions",
            "100 Reg.shs Placeholder Equity AG CHF 10.000000 12.50 25.00% 1 250 5.00",
            "Valor 111 - ISIN XX0000000011",
            "Additional information Abbreviations",
        ])
        rows = parse_statement_of_assets_text(text, "<doc-token>", label)
        assert rows[0]["portfolio_external_id"] == "0999000000000005"


# ---- Issue 2: portfolio_external_id length must be 16 -------------

class TestPortfolioExternalId:
    """The PDF parser assembles a PSN-aligned portfolio_external_id
    of the form `<branch:4><base:8><portfolio_no:4>` = 16 chars. UBS
    PDF labels strip the branch's leading zero (rendering as
    `BBB-AAAAAAAA-NN` instead of `BBBB-AAAAAAAA-NN`), so the
    assembly must zfill the branch to 4 chars."""

    # Synthetic digit placeholders — obviously not anyone's real
    # account, but the parser regex requires actual digits.
    BRANCH_3 = "999"
    BRANCH_4 = "9999"
    BASE_8 = "00000000"
    PORTFOLIO_NO = "42"

    @staticmethod
    def _label(branch_disp: str, base: str, portfolio_no: str) -> str:
        """Synthesize a Statement-of-assets listing label in the
        format the docs page emits. branch_disp is what UBS shows
        in the label (without or with leading zero), base is the
        8-digit account base, portfolio_no is the 2-digit suffix."""
        return (
            f"‍ Statement of assets as of 31122024 "
            f"02.01.2025 02 January 2025 P. Placeholder "
            f"{branch_disp}-{base}-{portfolio_no} 300 KB"
        )

    def test_branch_padded_to_four_digits(self):
        """Some labels carry 3-digit branches; the assembled
        ID must still be 16 chars."""
        meta = parse_label_statement_of_assets(
            self._label(self.BRANCH_3, self.BASE_8, self.PORTFOLIO_NO)
        )
        assert meta is not None, "label should parse"
        branch, base = meta["account_number_prefix"].split("-", 1)
        assembled = (
            f"{branch.zfill(4)}{base.zfill(8)}"
            f"{meta['portfolio_number'].zfill(4)}"
        )
        assert len(assembled) == 16, f"got {assembled!r} ({len(assembled)} chars)"

    def test_label_with_4digit_branch_also_works(self):
        """If UBS ever ships labels with non-stripped branches,
        the assembly must still produce 16 chars."""
        meta = parse_label_statement_of_assets(
            self._label(self.BRANCH_4, self.BASE_8, self.PORTFOLIO_NO)
        )
        assert meta is not None
        branch, base = meta["account_number_prefix"].split("-", 1)
        assembled = (
            f"{branch.zfill(4)}{base.zfill(8)}"
            f"{meta['portfolio_number'].zfill(4)}"
        )
        assert len(assembled) == 16

    def test_parse_statement_of_assets_raises_on_length_drift(
            self, monkeypatch):
        """If the assembly ever drifts from 16 chars (regex change,
        upstream label format shift, etc.), the parser must raise
        rather than silently inserting a bad row. Exercised through
        the text helper — the assembly happens there, before any PDF
        content is consulted."""
        # Synthesize a pathological label_meta where the
        # portfolio_number is too long. We monkeypatch the label
        # parser to return it, then assert the assembly raises.
        bad_meta = {
            "as_of_date": 1735603200,
            "as_of_str": "2024-12-31",
            "account_number_prefix": "BBB-AAAAAAAA",
            "portfolio_number": "NNNNN",  # 5 digits, will push total to 17
        }
        monkeypatch.setattr(
            "pdf_parsers.parse_label_statement_of_assets",
            lambda label: bad_meta,
        )
        with pytest.raises(ValueError, match=r"length != 16"):
            parse_statement_of_assets_text("", "<doc-token>", "<label>")


# ---- Statement-of-assets securities: listed + alternatives --------

class TestStatementOfAssetsSecurities:
    """The Statement-of-assets securities walker must capture three
    row shapes off the Valor/ISIN anchor:

      1. Listed securities — the cost/market/gain% triple.
      2. Listed securities whose market price carries a one-letter
         qualifier (e.g. a structured product's "120.00 B 20.00%").
      3. Private-markets / SPV holdings — a single FX rate (or
         "n.a.") in place of the triple; the funded "Outstanding
         Shares" row carries the NAV, the n.a. commitment rows are 0.

    All identifiers are synthetic placeholders per AGENTS.md §4 —
    the ISIN-shaped tokens use the reserved 'XX' prefix and repdigit
    bodies so they are obviously not real instruments."""

    # Synthetic Statement-of-assets listing label (3-digit branch,
    # all-zero base, portfolio 06). Same shape as the docs page emits.
    LABEL = (
        "‍ Statement of assets as of 31032026 "
        "02.04.2026 02 April 2026 P. Placeholder 999-00000000-06 300 KB"
    )

    def _soa_text(self, base_ccy: str = "USD") -> str:
        """A minimal Statement-of-assets text body: a Portfolio-01
        overview block (precious-metals asset-class total, no detail
        page) followed by a Detailed-positions section carrying one
        listed equity, one flagged structured product, one funded
        private-markets holding and one unfunded-commitment row."""
        return "\n".join([
            f"Valued in {base_ccy}",
            "Portfolio 01",
            "Liquidity 11 111 11 111 22.58",
            # market value + (equal) total + %NA, all single-space
            # separated like pdfplumber emits. The 6-digit value is
            # deliberate: its groups line up so a naive capture would
            # slurp both equal columns into one doubled number
            # (987 654 987 654) — the bug this fixture guards against.
            "Precious metals & commodities 987 654 987 654 75.00",
            "Net assets 1 245 678",
            "Detailed positions",
            # 1. listed equity (cost / market / gain% triple)
            "100 Reg.shs Placeholder Equity AG USD 10.000000 12.50 25.00% 1 250 5.00",
            "Financials",
            "Valor 111 - ISIN XX0000000011",
            # 2. structured product with a one-letter price flag ('B')
            "200 Example Structured Note USD 100.000000 120.00 B 20.00% 24 000 10.00",
            "Valor 222 - ISIN XX0000000022",
            # 3. funded private-markets Outstanding Shares (NAV per unit)
            "300 MVPX Placeholder Fund USD 1.2500 375 4.00",
            "Valor 333 - ISIN XX0000000033",
            # 4. unfunded commitment (n.a. price, 0 value) -> skipped
            "400 MVPX Placeholder Fund USD n.a. 0 0.00",
            "Valor 444 - ISIN XX0000000044",
            "Additional information Abbreviations",
        ])

    def _by_isin(self, rows):
        return {r["instrument_isin"]: r for r in rows
                if r["instrument_isin"]}

    def test_listed_equity_still_parses(self):
        """Regression guard: the cost/market/gain% triple path is
        unchanged by the added flag/PM handling."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        eq = self._by_isin(rows)["XX0000000011"]
        assert eq["market_value"] == pytest.approx(1250.0)
        assert eq["cost_price"] == pytest.approx(10.0)
        assert eq["market_price"] == pytest.approx(12.5)
        assert eq["sector"] == "Financials"

    def test_structured_product_with_price_flag(self):
        """The one-letter qualifier after the market price ('120.00 B')
        must not break the headline match."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        amc = self._by_isin(rows)["XX0000000022"]
        assert amc["market_value"] == pytest.approx(24000.0)
        assert amc["cost_price"] == pytest.approx(100.0)

    def test_private_market_outstanding_shares_captured(self):
        """A funded private-markets row (one price figure + NAV, no
        gain%) is captured via the PM-headline fallback. The figure is
        the NAV per unit — units × figure is the market value — so it is
        the market price, not an exchange rate, and no cost is printed."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        pm = self._by_isin(rows)["XX0000000033"]
        assert pm["market_value"] == pytest.approx(375.0)
        assert pm["units"] == pytest.approx(300.0)
        assert pm["market_price"] == pytest.approx(1.25)
        assert pm["current_fx_rate"] is None
        assert pm["cost_price"] is None

    def test_unfunded_commitment_row_skipped(self):
        """The 'n.a.'-priced 0-value commitment row adds no position."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        assert "XX0000000044" not in self._by_isin(rows)

    def test_overview_precious_metals_synthesised_for_usd(self):
        """The precious-metals portfolio has no Detailed-positions
        page; its overview asset-class total is recovered as a
        synthetic position with a non-ISIN-shaped key, attributed to
        portfolio 01."""
        rows = parse_statement_of_assets_text(
            self._soa_text("USD"), "<doc-token>", self.LABEL)
        pm = [r for r in rows
              if r["description"] == "Precious metals & commodities"]
        assert len(pm) == 1
        row = pm[0]
        # Only the Market value column — NOT the doubled mv+total run.
        assert row["market_value"] == pytest.approx(987654.0)
        assert row["market_value"] != pytest.approx(987654987654.0)
        assert row["currency_iso"] == "USD"
        assert row["portfolio_external_id"].endswith("0001")
        # Synthetic key must not look like a real ISIN (len != 12).
        assert len(row["instrument_isin"]) != 12
        assert row["instrument_isin"].startswith("PM-")

    def test_overview_precious_metals_skipped_for_non_usd(self):
        """Only the USD-valued copy emits the synthetic, so the same
        holding (printed once per portfolio currency) collapses to a
        single deterministic row across the relationship's PDFs."""
        rows = parse_statement_of_assets_text(
            self._soa_text("CHF"), "<doc-token>", self.LABEL)
        assert not [r for r in rows
                    if r["description"] == "Precious metals & commodities"]


class TestStatementOfAssetsHoldingDetail:
    """The lines a holding prints below its headline: the average buy
    and current exchange rates (line 2), the cost value (line 3) and the
    last purchase date (line 4). Each is read from the right-hand end of
    its line, since the wrapped description and the distribution notes
    share the left-hand side. Every figure is synthetic, and each block's
    figures agree with each other the way a real statement's do: cost
    value = units × cost price × buy rate, and the unrealized P/L is the
    market value over the cost value."""

    LABEL = TestStatementOfAssetsSecurities.LABEL

    def _rows(self, *block: str) -> dict:
        text = "\n".join(["Valued in USD", "Detailed positions", *block,
                          "Additional information Abbreviations"])
        rows = parse_statement_of_assets_text(text, "<doc-token>", self.LABEL)
        return {r["instrument_isin"]: r for r in rows}

    # A GBP holding in a USD portfolio, its line 3 carrying a market-price
    # date between the cost value and the P/L.
    FOREIGN = (
        "1 000 Reg.shs Example Equity AG GBP 20.000000 25.00 25.00% 32 500 4.50",
        "(XMPL) All sectors 1.250000 1.300000 4.00%",
        "Distribution: 01.06.2030 25 000 30.06.2030 30.00%",
        "Distribution amount: GBP 0.5 2.00% DY 15.03.2030",
        "Valor 555 - ISIN XX0000000055",
    )

    def test_a_foreign_currency_holding_states_both_rates(self):
        row = self._rows(*self.FOREIGN)["XX0000000055"]
        assert row["acquisition_fx_rate"] == pytest.approx(1.25)
        assert row["current_fx_rate"] == pytest.approx(1.3)
        assert row["cost_basis"] == pytest.approx(25000.0)
        assert row["last_purchase_date"] == 1899763200     # 15.03.2030

    def test_a_holding_in_the_portfolio_currency_states_no_rate(self):
        row = self._rows(
            "500 Example ETF USD 10.000000 12.00 20.00% 6 000 1.00",
            "Example ETF Class A All sectors",
            "Distribution: 01.06.2030 5 000 20.00%",
            "Distribution amount: USD 0.1 1.00% DY 15.02.2030",
            "Valor 666 - ISIN XX0000000066",
        )["XX0000000066"]
        assert row["acquisition_fx_rate"] is None
        assert row["current_fx_rate"] is None
        assert row["cost_basis"] == pytest.approx(5000.0)
        assert row["last_purchase_date"] == 1897344000     # 15.02.2030

    def test_a_figure_set_against_the_cost_value_is_not_read_into_it(self):
        # The distribution amount 'USD 2' sits one space before the cost
        # value '450 000', so the line reads '2 450 000'. Only the shorter
        # reading agrees with the printed P/L (360 000 / 450 000 - 1).
        row = self._rows(
            "3 000 Reg.shs Example Holding AG USD 150.000000 120.00 -20.00% 360 000 9.00",
            "Distribution: 01.05.2030 Consumer staples",
            "Distribution amount: USD 2 450 000 -20.00%",
            "3.00% DY 20.01.2030",
            "Valor 777 - ISIN XX0000000077",
        )["XX0000000077"]
        assert row["cost_basis"] == pytest.approx(450000.0)
        assert row["last_purchase_date"] == 1895097600     # 20.01.2030

    def test_a_cost_value_no_reading_agrees_with_is_left_unread(self):
        row = self._rows(
            "100 Reg.shs Example Equity AG USD 10.000000 12.00 20.00% 1 200 1.00",
            "All sectors",
            "9 999 20.00%",
            "Valor 999 - ISIN XX0000000099",
        )["XX0000000099"]
        assert row["cost_basis"] is None

    def test_a_distribution_date_is_not_a_purchase(self):
        row = self._rows(
            "200 Example Bond Fund USD 50.000000 55.00 10.00% 11 000 2.00",
            "All sectors",
            "10 000 10.00%",
            "Distribution: 01.07.2030",
            "Valor 888 - ISIN XX0000000088",
        )["XX0000000088"]
        assert row["cost_basis"] == pytest.approx(10000.0)
        assert row["last_purchase_date"] is None

    def test_a_private_market_holding_reads_its_last_purchase_below_the_nav_date(self):
        row = self._rows(
            "300 MVPX Placeholder Fund USD 1.2500 375 4.00",
            "Placeholder Fund 9",
            "Outstanding Shares 31.03.2030",
            "Strategy: Private markets - Others 15.01.2030",
            "Valor 333 - ISIN XX0000000033",
        )["XX0000000033"]
        assert row["market_price"] == pytest.approx(1.25)
        assert row["last_purchase_date"] == 1894665600     # 15.01.2030
        assert row["cost_basis"] is None
        assert row["acquisition_fx_rate"] is None

    def test_a_cash_line_keeps_its_rate_as_the_current_one(self):
        rows = parse_statement_of_assets_text("\n".join([
            "Valued in USD", "Detailed positions",
            "GBP 1 000.00 Example Cash Account GBP 0.00 1.3000 1 300 0.10",
            "CH00 0000 0000 0000 0000 A",
            "Additional information Abbreviations",
        ]), "<doc-token>", self.LABEL)
        assert rows[0]["current_fx_rate"] == pytest.approx(1.3)
        assert rows[0]["acquisition_fx_rate"] is None

    def test_a_holding_whose_headline_does_not_parse_takes_no_other(self):
        # The second headline prints an integer cost price, which the
        # headline pattern does not read. Its Valor line is within reach
        # of the first holding's headline, which must not be lent to it.
        rows = self._rows(
            *self.FOREIGN,
            "Country of custody Switzerland",
            "50 Reg.shs Example Other AG USD 148 150.5 1.69% 7 525 1.00",
            "All sectors",
            "Valor 444 - ISIN XX0000000044",
        )
        assert "XX0000000055" in rows
        assert "XX0000000044" not in rows


# ---- Statement of assets: the transaction list ---------------------
#
# Word-based, like the Account-Statement ledger: a figure's meaning is
# its column and its row within the booking, so the fixtures place words
# at the column geometry the list prints (right edges 266 / 527 / 598 /
# 682 / 782, description from 271, booking text from 96, a 10pt row
# pitch). Every name, figure and identifier is synthetic.

_TL_HEADER_ROWS = (
    [("Trade", 39, 61), ("date", 63, 80), ("Booking", 96, 127),
     ("text", 130, 144), ("Number/Amount", 201, 266),
     ("Description", 271, 314), ("Cost/Purchase", 452, 506),
     ("price", 508, 527), ("Transaction", 533, 577), ("price", 579, 598),
     ("Transaction", 619, 663), ("gain", 666, 682),
     ("Transaction", 715, 759), ("value", 762, 782)],
    [("Trade", 39, 61), ("time", 63, 80), ("Tax", 253, 266),
     ("Custody", 271, 303), ("account", 305, 336), ("Exchange", 473, 509),
     ("rate", 512, 527), ("Exchange", 543, 580), ("rate", 582, 597),
     ("Exchange", 627, 664), ("gain", 666, 683), ("Accrued", 719, 751),
     ("interest", 753, 782)],
    [("Value", 39, 61), ("date", 63, 80), ("Various", 237, 266),
     ("Account", 271, 303), ("Cost", 487, 504), ("value", 507, 527),
     ("Realized", 637, 669), ("P/L", 671, 682), ("Settlement", 708, 749),
     ("amount", 752, 782)],
    [("Brokerage", 227, 266), ("Place", 457, 476), ("of", 479, 487),
     ("execution", 489, 526), ("in", 707, 714), ("account", 716, 747),
     ("currency", 749, 782)],
    [("Stock", 206, 227), ("exchange", 229, 266)],
    [("Third-party", 181, 223), ("executions", 225, 266)],
    [("Foreign", 202, 230), ("Financial", 233, 266)],
    [("Transaction", 206, 250), ("Tax", 253, 266)],
)
_TL_TOP = 124.0
_TL_RIGHT = {"B": 266, "D": 527, "E": 598, "F": 682, "G": 782}
_TL_LEFT = {"A": 39, "text": 96, "C": 271}


def _tl_cell(col: str, text: str, top: float) -> list[dict]:
    """Words of one cell: left-aligned in A, C and the booking text,
    right-aligned in the figure columns. 5pt per character, 3pt gaps."""
    toks = text.split()
    widths = [5 * len(t) for t in toks]
    span = sum(widths) + 3 * (len(toks) - 1)
    x = _TL_LEFT[col] if col in _TL_LEFT else _TL_RIGHT[col] - span
    out = []
    for tok, wdt in zip(toks, widths, strict=True):
        out.append(_w(tok, x, x + wdt, top))
        x += wdt + 3
    return out


def _tl_header() -> list[dict]:
    return [_w(t, x0, x1, _TL_TOP + 10 * i)
            for i, row in enumerate(_TL_HEADER_ROWS) for t, x0, x1 in row]


def _tl_booking(start: float, rows: dict[int, dict[str, str]]) -> list[dict]:
    """A booking's words: {row number: {column: text}}."""
    return [w for k, cells in rows.items() for col, text in cells.items()
            for w in _tl_cell(col, text, start + 10 * k)]


_TL_PAGE_TEXT = ("From 01.01.2030 to 31.03.2030 Statement of assets as of 31 March 2030\n"
                 "Valued in USD\n"
                 "Trade date Booking text Number/Amount Description\n")

# A purchase in a currency other than the reporting one, with three
# charges; the settlement is the gross amount plus them.
_TL_PURCHASE = {
    0: {"A": "02.01.2030", "text": "Purchase", "B": "1 000",
        "C": "Reg.shs Example AG", "D": "GBP 20.000000", "G": "25 000.00"},
    1: {"A": "10:00:00", "text": "Spot", "D": "1.250000"},
    2: {"A": "04.01.2030", "B": "GBP -50.00",
        "C": "Settlement no.: XX00000000001", "D": "25 000"},
    3: {"C": "Valor 555 - ISIN XX0000000055", "D": "London",
        "G": "GBP -20 070.00"},
    4: {"C": "999-0000000.S1"},
    5: {"C": "CH00 0000 0000 0000 0000 A"},
    6: {"B": "GBP -20.00"},
}
# A sale in the reporting currency: the average cost, the cost value and
# the realized P/L.
_TL_SALE = {
    0: {"A": "15.02.2030", "text": "Sale", "B": "-100",
        "C": "Shs Example Two", "D": "USD 50.000000", "E": "60.00",
        "F": "20.00%", "G": "-6 000.00"},
    1: {"A": "11:00:00", "text": "Spot",
        "C": "Valor 666 - ISIN XX0000000066"},
    2: {"A": "19.02.2030", "B": "USD -10.00",
        "C": "Settlement no.: XX00000000002", "D": "5 000",
        "F": "20.00%"},
    3: {"D": "SIX SWX", "G": "USD 5 990.00"},
}
# A corporate action: no price, a booking text on two rows.
_TL_SPIN_OFF = {
    0: {"A": "20.03.2030", "text": "Incoming from", "B": "50",
        "C": "Reg.shs Example Spin", "D": "CHF"},
    1: {"text": "spin-off"},
    2: {"A": "22.03.2030", "C": "Valor 777 - ISIN XX0000000077"},
}
# A booking text on three rows: every cell below the first row prints a
# row lower than its header label, except the trade time. The transaction
# rate (header row 1), the "Various" charge (row 2), the value date (row
# 2), the place and the settlement amount (row 3) and the foreign
# financial transaction tax (row 6) each print one row down.
_TL_WRAPPED = {
    0: {"A": "05.03.2030", "text": "Purchase from", "B": "100",
        "C": "Shs Example Three", "D": "EUR", "E": "40.00",
        "G": "4 400.00"},
    1: {"A": "09:30:00", "text": "subscription"},
    2: {"text": "rights", "E": "1.100000"},
    3: {"A": "07.03.2030", "B": "EUR -5.00",
        "C": "Settlement no.: XX00000000003"},
    4: {"C": "Valor 787 - ISIN XX0000000087", "D": "Paris",
        "G": "EUR -4 012.00"},
    5: {"C": "999-0000000.S1"},
    6: {"C": "CH00 0000 0000 0000 0000 A"},
    7: {"B": "EUR -7.00"},
}


def _tl_pdf(*pages: list[dict]) -> _FakePDF:
    return _FakePDF([_FakePage(words, _TL_PAGE_TEXT) for words in pages])


class TestTransactionList:
    LABEL = TestStatementOfAssetsSecurities.LABEL

    def _trades(self, *pages: list[dict]) -> list[dict]:
        from pdf_parsers import parse_statement_of_assets_pages
        _positions, trades = parse_statement_of_assets_pages(
            _tl_pdf(*pages), "<doc-token>", self.LABEL)
        return trades

    def _page(self) -> list[dict]:
        return (_tl_header()
                + _tl_booking(217, _TL_PURCHASE)
                + _tl_booking(298, _TL_SALE)
                + _tl_booking(348, _TL_SPIN_OFF)
                + _tl_cell("A", "Subtotal inflows incl. accrued interest", 390)
                + _tl_cell("G", "0.00", 390)
                + _tl_cell("A", "AAAA0000/000000/XXXXXXXXXXXX", 555))

    def test_each_booking_is_one_row_in_list_order(self):
        trades = self._trades(self._page())
        assert [t["seq"] for t in trades] == [1, 2, 3]
        assert [t["isin"] for t in trades] == [
            "XX0000000055", "XX0000000066", "XX0000000077"]
        assert {t["source_doc_token"] for t in trades} == {"<doc-token>"}
        assert {t["reporting_currency_iso"] for t in trades} == {"USD"}
        assert {(t["period_start"], t["period_end"]) for t in trades} == {
            (1893456000, 1901145600)}                  # 01.01.2030, 31.03.2030

    def test_a_purchase_reads_its_price_rate_and_charges(self):
        t = self._trades(self._page())[0]
        assert t["booking_text"] == "Purchase Spot"
        assert t["trade_date"] == 1893542400            # 02.01.2030
        assert t["trade_time"] == "10:00:00"
        assert t["quantity"] == pytest.approx(1000.0)
        assert t["currency_iso"] == "GBP"
        assert t["cost_price"] == pytest.approx(20.0)
        assert t["acquisition_fx_rate"] == pytest.approx(1.25)
        assert t["cost_basis"] == pytest.approx(25000.0)
        assert t["transaction_value"] == pytest.approx(25000.0)
        assert t["transaction_price"] is None
        assert t["fees"] == pytest.approx(-50.0)
        assert t["financial_transaction_tax"] == pytest.approx(-20.0)
        assert t["taxes"] is None and t["commission"] is None
        assert t["charges_currency_iso"] == "GBP"
        assert t["settlement_amount"] == pytest.approx(-20070.0)
        assert t["settlement_currency_iso"] == "GBP"
        assert t["place_of_execution"] == "London"
        assert t["security_name"] == "Reg.shs Example AG"
        assert t["settlement_no"] == "XX00000000001"
        assert t["valor"] == "555"
        assert t["custody_account"] == "999-0000000.S1"
        assert t["account_iban"] == "CH000000000000000000A"

    def test_a_sale_reads_its_cost_and_realized_result(self):
        t = self._trades(self._page())[1]
        assert t["booking_text"] == "Sale Spot"
        assert t["quantity"] == pytest.approx(-100.0)
        assert t["cost_price"] == pytest.approx(50.0)
        assert t["transaction_price"] == pytest.approx(60.0)
        assert t["transaction_gain_pct"] == pytest.approx(20.0)
        assert t["cost_basis"] == pytest.approx(5000.0)
        assert t["realized_pl_pct"] == pytest.approx(20.0)
        assert t["transaction_value"] == pytest.approx(-6000.0)
        assert t["acquisition_fx_rate"] is None
        assert t["settlement_amount"] == pytest.approx(5990.0)
        # The settlement number printed below the Valor line is still
        # its own, and neither enters the description.
        assert t["settlement_no"] == "XX00000000002"
        assert t["security_name"] == "Shs Example Two"

    def test_a_corporate_action_joins_its_booking_text(self):
        t = self._trades(self._page())[2]
        assert t["booking_text"] == "Incoming from spin-off"
        assert t["quantity"] == pytest.approx(50.0)
        assert t["currency_iso"] == "CHF"
        assert t["cost_price"] is None
        assert t["trade_time"] is None
        assert t["value_date"] == 1900368000             # 22.03.2030

    def test_a_booking_text_on_three_rows_moves_the_rows_below_it(self):
        t = self._trades(_tl_header() + _tl_booking(217, _TL_WRAPPED))[0]
        assert t["booking_text"] == "Purchase from subscription rights"
        assert t["trade_date"] == 1898899200            # 05.03.2030
        assert t["trade_time"] == "09:30:00"
        assert t["value_date"] == 1899072000            # 07.03.2030
        assert t["transaction_price"] == pytest.approx(40.0)
        assert t["transaction_fx_rate"] == pytest.approx(1.1)
        assert t["fees"] == pytest.approx(-5.0)
        assert t["commission"] is None
        assert t["financial_transaction_tax"] == pytest.approx(-7.0)
        assert t["charges_currency_iso"] == "EUR"
        assert t["place_of_execution"] == "Paris"
        assert t["settlement_amount"] == pytest.approx(-4012.0)
        assert t["settlement_currency_iso"] == "EUR"
        assert t["cost_basis"] is None and t["acquisition_fx_rate"] is None
        assert t["realized_pl_pct"] is None
        assert t["isin"] == "XX0000000087"
        assert t["account_iban"] == "CH000000000000000000A"
        # The payload keeps each cell where it prints.
        payload = json.loads(t["payload"])
        assert payload["B3"] == "EUR -5.00" and payload["B7"] == "EUR -7.00"

    def test_the_closing_totals_are_not_a_booking(self):
        trades = self._trades(self._page())
        assert trades[2]["transaction_value"] is None
        assert "Subtotal" not in trades[2]["payload"]

    def test_a_charge_printed_left_of_the_quantity_label_is_a_charge(self):
        # Older lists set a charge's currency further left than the
        # Number/Amount label starts. It is right-aligned with the
        # column all the same, and is not booking text.
        booking = {**_TL_SALE, 2: {**_TL_SALE[2], "B": "USD -1 234 567.00"}}
        t = self._trades(_tl_header() + _tl_booking(217, booking))[0]
        assert t["booking_text"] == "Sale Spot"
        assert t["fees"] == pytest.approx(-1234567.0)

    def test_the_list_continues_across_pages(self):
        trades = self._trades(
            _tl_header() + _tl_booking(217, _TL_PURCHASE),
            _tl_header() + _tl_booking(217, _TL_SALE))
        assert [(t["seq"], t["isin"]) for t in trades] == [
            (1, "XX0000000055"), (2, "XX0000000066")]

    def test_a_page_without_the_list_header_adds_nothing(self):
        pdf = _FakePDF([_FakePage(_tl_booking(217, _TL_SALE), "Valued in USD\n")])
        from pdf_parsers import parse_statement_of_assets_pages
        assert parse_statement_of_assets_pages(
            pdf, "<doc-token>", self.LABEL) == ([], [])


class TestCapitalCall:
    """A capital call: the UBS cover page titled "Capital Call", then the
    administrator's notice. All names, figures and identifiers are
    synthetic."""

    COVER = [
        "Capital Call",
        "Produced on 10 March 2030",
        "Please find enclosed a capital call notice in relation to your",
        "investment in Example Fund 9.",
    ]

    def _text(self, *, isin_line: str = "ISIN - XX0000000011",
              investing: str = "Investing in Example LP",
              total: str = "Total 1,234,567.00 12,345.67",
              amount: str = "12,345.67") -> str:
        return "\n".join([
            *self.COVER,
            "8 March 2030",
            "Example Fund 9 (“EF 9”) - Capital Call No. 23",
            *([investing] if investing else []),
            isin_line,
            "Dear Investor",
            "In accordance with the terms of the Supplement, EF 9 will now make Capital Call No. 23 of",
            f"USD {amount}, which represents 7.25% of your Net Commitment of USD 170,285.10.",
            "A breakdown of the Capital Call is provided below.",
            "Fund (USD) Investor Amount (USD)",
            "Investments 1,234,567.00 12,345.67",
            total,
            "Please ensure that your Client Advisor makes the amount of USD 12,345.67 available in your account",
            "by value date 20",
            "March 2030.",
        ])

    def test_the_call_is_read(self):
        [row] = parse_capital_call_text(self._text(), "<doc-token>")
        assert row["kind"] == "capital_call"
        assert row["instrument_isin"] == "XX0000000011"
        assert row["currency_iso"] == "USD"
        assert row["amount"] == pytest.approx(12345.67)
        assert row["settlement_amount"] == pytest.approx(12345.67)
        assert row["value_date"] == 1900195200             # 20.03.2030
        assert row["doc_date"] == 1899331200               # 10.03.2030
        assert row["title"] == (
            "Example Fund 9 (“EF 9”) - Capital Call No. 23 "
            "Investing in Example LP")
        assert row["quantity"] is None and row["price"] is None
        assert json.loads(row["payload"])["isin"] == "ISIN - XX0000000011"

    def test_the_cash_taken_includes_what_is_charged_on_top_of_the_call(self):
        # A late closing's equalisation interest is in the breakdown total
        # and in the cash, never in the called amount.
        [row] = parse_capital_call_text(
            self._text(total="Total 1,234,567.00 12,596.17"), "<doc-token>")
        assert row["amount"] == pytest.approx(12345.67)
        assert row["settlement_amount"] == pytest.approx(12596.17)

    def test_an_isin_set_at_the_end_of_the_investing_line_is_read(self):
        [row] = parse_capital_call_text(
            self._text(investing="", isin_line="Investing in ISIN – XX0000000011"),
            "<doc-token>")
        assert row["instrument_isin"] == "XX0000000011"
        assert row["title"] == "Example Fund 9 (“EF 9”) - Capital Call No. 23"

    def test_a_letter_that_is_not_a_call_yields_nothing(self):
        text = self._text().replace("Capital Call\n", "Quarterly Report\n", 1)
        assert parse_capital_call_text(text, "<doc-token>") == []


class TestContractNote:
    """A contract note for a purchase outside the exchange. All names,
    figures and identifiers are synthetic."""

    NEW_ISSUE = "\n".join([
        "Contract note",
        "Produced on 30 March 2030",
        "New issue purchase",
        "Trade date: 15.03.2030 Place of transaction Issuer",
        "Settlement date: 30.03.2030",
        "Quantity Security 1234567 ISIN XX0000000022 Price",
        "800.5 Example Fund SICAV USD 100.00",
        "E-USD-capitalisation",
        "Market value in trading currency USD 80 050.00",
        "Minus your prepayment USD 80 000.00",
        "Placement Fee USD 800.00",
        "Swiss federal stamp duty USD 120.00",
        "To the debit of account 0000 00000000.XX USD Value date 30.03.2030 USD 970.00",
        "For USD /CHF conversions, we have used the following rate: 0.900000.",
    ])

    PREPAYMENT = "\n".join([
        "Contract note",
        "Produced on 1 March 2030",
        "Prepayment for fund subscription",
        "Trade date: 01.03.2030 Place of transaction Issuer",
        "Settlement date: 02.03.2030",
        "Prepayment for Subscription of",
        "Example Fund SICAV -",
        "E-USD-capitalisation",
        "XX0000000033",
        "Market value in trading currency USD 80 000.00",
        "USD / CHF at 0.90000",
        "To the debit of account 0000 00000000.XX USD Value date 02.03.2030 USD 80 000.00",
    ])

    def test_a_new_issue_purchase_is_read(self):
        [row] = parse_contract_note_text(self.NEW_ISSUE, "<doc-token>")
        assert row["kind"] == "contract_note"
        assert row["title"] == "New issue purchase"
        assert (row["valor"], row["instrument_isin"]) == ("1234567", "XX0000000022")
        assert row["security_name"] == "Example Fund SICAV E-USD-capitalisation"
        assert row["quantity"] == pytest.approx(800.5)
        assert row["price"] == pytest.approx(100.0)
        assert row["currency_iso"] == "USD"
        assert row["amount"] == pytest.approx(80050.0)
        assert row["prepayment"] == pytest.approx(80000.0)
        assert row["placement_fee"] == pytest.approx(800.0)
        assert row["stamp_duty"] == pytest.approx(120.0)
        assert row["settlement_amount"] == pytest.approx(970.0)
        assert row["settlement_currency_iso"] == "USD"
        assert row["fx_rate"] == pytest.approx(0.9)
        assert row["fx_rate_pair"] == "USD/CHF"
        assert row["trade_date"] == 1899763200             # 15.03.2030
        assert row["value_date"] == 1901059200             # 30.03.2030
        assert row["doc_date"] == 1901059200

    def test_a_prepayment_names_its_fund_above_a_bare_isin(self):
        [row] = parse_contract_note_text(self.PREPAYMENT, "<doc-token>")
        assert row["instrument_isin"] == "XX0000000033"
        assert row["valor"] is None
        assert row["security_name"] == "Example Fund SICAV - E-USD-capitalisation"
        assert row["quantity"] is None and row["price"] is None
        assert row["amount"] == pytest.approx(80000.0)
        assert row["placement_fee"] is None and row["stamp_duty"] is None
        assert row["fx_rate"] == pytest.approx(0.9)
        assert row["fx_rate_pair"] == "USD/CHF"

    def test_a_document_that_is_not_a_contract_note_yields_nothing(self):
        text = self.NEW_ISSUE.replace("Contract note", "Order confirmation", 1)
        assert parse_contract_note_text(text, "<doc-token>") == []


# ---- Issue 1: opening/closing/total balances must be extracted ---

class TestAccountStatementBalances:
    """The Account-Statement parser must populate opening_balance,
    closing_balance, total_debits, total_credits from the squished
    PDF text. Earlier the four regexes required `\\s+` between label
    and value, which never matches the whitespace-stripped `flat`
    text — every row landed with all four numeric columns NULL."""

    # Synthetic placeholder IBAN: CH + 2-digit checksum + 17
    # alphanumerics. Not a real IBAN — the regex only validates
    # shape, so any matching string works for the parser layer.
    IBAN = "CH00000000000000000000A"[:21]  # 21 chars
    PERIOD = "01.04.2026 - 30.04.2026"
    HEADER = "UBS personal account EUR"

    # Synthetic placeholder values. Deliberately obvious-fake
    # repdigits so a casual reader can see they are not anyone's
    # actual balances. Matching the assertions to these constants
    # makes it easy to confirm none of the numbers were lifted from
    # a real probed statement.
    OPENING_VAL_STR = "1 111.11"
    CREDITS_VAL_STR = "2 222.22"
    DEBITS_VAL_STR = "3 333.33"
    CLOSING_VAL_STR = "4 444.44"

    def _statement_with_summary(self) -> str:
        """Text shape of a full monthly EUR statement (opening +
        closing + totals all present). Whitespace and line breaks
        mirror what pdfplumber emits before the squish-pass."""
        return (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"Account Statement\n"
            f"{self.PERIOD} / Monthly: number 4\n"
            "Your account at a glance Debits Credits Balance\n"
            f"Opening balance {self.OPENING_VAL_STR}\n"
            f"Total credits {self.CREDITS_VAL_STR}\n"
            f"Total debits {self.DEBITS_VAL_STR}\n"
            f"Closing balance {self.CLOSING_VAL_STR}\n"
        )

    def _statement_without_summary(self) -> str:
        """Text shape of a low-activity statement that omits the
        summary block and only carries the in-table opening line."""
        return (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"Account Statement\n"
            f"{self.PERIOD}\n"
            "Date Information Debits Credits Value date Balance\n"
            f"01.04.26 Opening balance {self.OPENING_VAL_STR}\n"
        )

    def test_summary_block_populates_all_four_columns(self):
        rows = parse_account_statement_text(
            self._statement_with_summary(), "<doc-token>"
        )
        assert len(rows) == 1
        row = rows[0]
        assert row["opening_balance"] == pytest.approx(1111.11)
        assert row["closing_balance"] == pytest.approx(4444.44)
        assert row["total_credits"] == pytest.approx(2222.22)
        assert row["total_debits"] == pytest.approx(3333.33)
        assert row["currency_iso"] == "EUR"
        assert row["account_external_id"] == self.IBAN

    def test_missing_summary_leaves_totals_null(self):
        """When the summary block is absent (low-activity month),
        opening_balance must still come from the in-table line and
        the totals must remain NULL — never 0.0, which would be
        indistinguishable from a real zero-flow month."""
        rows = parse_account_statement_text(
            self._statement_without_summary(), "<doc-token>"
        )
        assert len(rows) == 1
        row = rows[0]
        assert row["opening_balance"] == pytest.approx(1111.11)
        assert row["closing_balance"] is None
        assert row["total_credits"] is None
        assert row["total_debits"] is None

    def test_zero_balance_without_decimal_parses(self):
        """Closed / zero-activity accounts render balances as a bare
        `0` rather than `0.00`. The decimal portion of the value
        regex must be optional or the row drops to NULL."""
        text = (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"{self.PERIOD}\n"
            "Date Information Debits Credits Value date Balance\n"
            "01.01.24 Opening balance 0\n"
            "Turnover total 0 0\n"
            "31.12.24 Closing balance 0\n"
        )
        rows = parse_account_statement_text(text, "<doc-token>")
        assert len(rows) == 1
        assert rows[0]["opening_balance"] == 0.0
        assert rows[0]["closing_balance"] == 0.0

    def test_negative_value_parses(self):
        """Overdraft / debit-side opening balances render with a
        leading minus sign in the source PDF; the value regex must
        accept it."""
        text = (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"{self.PERIOD}\n"
            f"Opening balance -{self.OPENING_VAL_STR}\n"
            f"Closing balance -{self.CLOSING_VAL_STR}\n"
        )
        rows = parse_account_statement_text(text, "<doc-token>")
        assert len(rows) == 1
        assert rows[0]["opening_balance"] == pytest.approx(-1111.11)
        assert rows[0]["closing_balance"] == pytest.approx(-4444.44)


# ---- Maturity Notice (mortgage interest-roll) PDFs ----------------

class TestMaturityNotice:
    """Parses the per-mortgage quarterly maturity-notice PDFs.
    Fixture text mirrors the line structure pdfplumber produces; the
    bold-rendered headers ('AAss aatt …') are the actual on-page
    artefact pdfplumber's character-level extraction emits."""

    # Synthetic placeholder mortgage account number — NOT real.
    ACCT_LINE = "Account no. 999-12345678.MMM 0001"
    # Synthetic placeholder address.
    COLLATERAL_LINE = "Category EXAMPLE ROAD 1, 0000 EXAMPLECITY"

    def test_fixed_rate_quarter_end(self):
        text = (
            "UBS Fixed-Rate Mortgage CHF\n"
            f"{self.ACCT_LINE}\n"
            f"{self.COLLATERAL_LINE}\n"
            "MMaattuurriittyy nnoottiiccee\n"
            "AAss aatt 3300..0099..22002233\n"
            "General information Amount in CHF\n"
            "Current debt capital 1 234 567.89\n"
        )
        rows = parse_maturity_notice_text(text, "<doc-token>")
        assert len(rows) == 1
        row = rows[0]
        # Branch zfill 3 → 4, base zfill 8 → 8, hyphen → space.
        assert row["account_external_id"] == "0999 12345678.MMM 0001"
        assert row["currency_iso"] == "CHF"
        # Outstanding goes in as a liability (negative sign).
        assert row["outstanding_balance"] == pytest.approx(-1234567.89)
        assert row["product_name"] == "UBS Fixed-Rate Mortgage"
        assert row["rate_type"] == "fixed"
        assert row["collateral_description"] == \
            "EXAMPLE ROAD 1, 0000 EXAMPLECITY"
        # 2023-09-30 UTC midnight.
        from datetime import datetime, timezone
        assert datetime.fromtimestamp(
            row["as_of_date"], timezone.utc).date() \
            == datetime(2023, 9, 30).date()

    def test_saron_classified_as_variable(self):
        """SARON Mortgage is UBS's variable-rate product; rate_type
        should map to 'variable', not 'saron', so the canonical
        taxonomy stays rate-basis."""
        text = (
            "UBS SARON Mortgage CHF\n"
            f"{self.ACCT_LINE}\n"
            f"{self.COLLATERAL_LINE}\n"
            "MMaattuurriittyy nnoottiiccee\n"
            "AAss aatt 3311..1122..22002233\n"
            "Current debt capital 500 000.00\n"
        )
        rows = parse_maturity_notice_text(text, "<doc-token>")
        assert len(rows) == 1
        assert rows[0]["product_name"] == "UBS SARON Mortgage"
        assert rows[0]["rate_type"] == "variable"

    def test_non_mortgage_maturity_notice_returns_empty(self):
        """The doc_type 'Maturity notice' is also UBS-side used for
        bond / time-deposit notices that aren't mortgages. Without a
        recognised product line + Account no. + debt-capital line
        the parser must return [], not invent a row."""
        text = (
            "Some Other Notice\n"
            "Account no. unrelated\n"
            "As at 30.09.2023\n"
            "Total amount due 100.00\n"
        )
        assert parse_maturity_notice_text(text, "<doc-token>") == []

    def test_undouble_passthrough_when_not_bold(self):
        """Normal (non-doubled) header lines must still match."""
        text = (
            "UBS Fixed-Rate Mortgage CHF\n"
            f"{self.ACCT_LINE}\n"
            f"{self.COLLATERAL_LINE}\n"
            "As at 30.09.2023\n"
            "Current debt capital 100 000.00\n"
        )
        rows = parse_maturity_notice_text(text, "<doc-token>")
        assert len(rows) == 1
        from datetime import datetime, timezone
        assert datetime.fromtimestamp(
            rows[0]["as_of_date"], timezone.utc).date() \
            == datetime(2023, 9, 30).date()


# ============================================================
# Credit / Debit Advice PDFs (per-movement payment advices)
# ============================================================
#
# UBS issues one advice per side of a payment, each addressed to its
# own account and both printing the SAME transaction number. The
# fixtures below are the two layouts the archive holds — the current
# one, which sets the account block on its own lines, and the older
# one, whose two columns extract_text merges into a single line — plus
# the two documents that wear the same label without being payments.
# All identifiers, amounts and addresses synthetic per AGENTS.md §4.

_ADV_IBAN_FROM = "CH00 0000 0000 0000 0000 1"
_ADV_IBAN_TO = "CH00 0000 0000 0000 0000 2"


def _advice_pdf_text(direction: str, iban: str, *, trx: str,
                     amount: str = "12 345.67", ccy: str = "CHF",
                     purpose: str | None = "Increase Example Mandate",
                     value_line: str = "Val. 01.01.2020") -> str:
    """One payment advice in the current layout. `direction` is the
    headline UBS prints ('Credit Advice' / 'Debit Advice')."""
    party = ("By order of" if direction.startswith("Credit")
             else "Order of 01.01.2020\nBeneficiary")
    details = f"Details of payment\n{purpose}\n" if purpose else ""
    return (
        "aUBS UBS Switzerland AG\n"
        "Postfach, 8098 Zürich\n"
        "www.ubs.com\n"
        "For information:\n"
        "Ms Example Adviser\n"
        "Tel. +41-00-000 00 00\n"
        f"Cash Account for investment solutions {ccy}\n"
        f"IBAN {iban}\n"
        "Herr\n"
        "Account no. 000-000000.001\n"
        "A. Example u/o B. Example\n"
        "Category EXAMPLE CATEGORY\n"
        "Example Street 1\n"
        "Client no. 000-000000\n"
        "0000 Example Town\n"
        "BIC EXAMPLEXXXX\n"
        "VAT number CHE-000.000.000 MWST\n"
        f"{direction}\n"
        "Produced on 2 January 2020\n"
        "Information/References\n"
        f"TRX-No. {trx}\n"
        "Bookkeeping entry date 1 January 2020\n"
        "Description\n"
        f"{party}\n"
        "A. Example u/o B. Example Additional information of the ordering bank\n"
        f"Example Street 1 Original amount {ccy} {amount}\n"
        "CH 0000 Example Town\n"
        f"{details}"
        "Currency Amount\n"
        f"Total amount {ccy} {amount}\n"
        f"{value_line}\n"
        "Yours sincerely,\n"
        "UBS Switzerland AG\n"
        "Form without signature Page 1 / 1\n"
        "XX000X00/000000/EXAMPLE000000000000 01.01.2020\n"
    )


# The older layout: the addressee column and the account column share
# each printed line, so the IBAN is not at the start of one.
_ADV_TWO_COLUMN_TEXT = (
    "aUBS UBS Switzerland AG\n"
    "Postfach, 8098 Zürich\n"
    "www.ubs.com\n"
    "Cash Account for investment solutions CHF\n"
    f"Herr IBAN {_ADV_IBAN_TO}\n"
    "A. Example u/o Account no. 000-000000.001\n"
    "B. Example\n"
    "Category EXAMPLE CATEGORY\n"
    "Example Street 1\n"
    "Client no. 000-000000\n"
    "0000 Example Town\n"
    "BIC EXAMPLEXXXX\n"
    "Credit Advice\n"
    "Produced on 3 January 2020\n"
    "Information/References\n"
    "TRX-No. 0000 024 ED 0000456 ABC/ABC\n"
    "Bookkeeping entry date 2 January 2020\n"
    "Description\n"
    "By order of\n"
    "A. Example u/o B. Example\n"
    "Details of payment\n"
    "Funding Example Managed Account\n"
    "Currency Amount\n"
    "Total amount CHF 98 765.43\n"
    "Val. 02.01.2020\n"
)

# A safe-deposit-box rental bill. UBS labels it a Debit Advice and it
# is not a payment: no transaction number, and its total is printed in
# a shape of its own.
_ADV_SAFE_BOX_TEXT = (
    "aUBS UBS Switzerland AG\n"
    "UBS safe deposit box\n"
    "Herr Box no. 000-00-000\n"
    "A. Example u/o Client no. 000-000000\n"
    "B. Example\n"
    "Account no. 000-000000.001\n"
    "Debit Advice\n"
    "Produced on 4 January 2020\n"
    "Rental fee from 01.01.2020 to 31.12.2020 CHF 11.11\n"
    "Total CHF 11.11\n"
    "0.00 % VAT (on CHF 11.11) CHF 2.22\n"
    "Total Val. 03.01.2020 CHF 13.33\n"
)

# A mortgage interest settlement, labelled 'Debit advice'. It names an
# account and an amount but no transaction number, so there is no id
# to key a movement row by.
_ADV_MORTGAGE_TEXT = (
    "aUBS UBS Switzerland AG\n"
    "UBS personal account CHF\n"
    f"IBAN {_ADV_IBAN_FROM}\n"
    "Herr\n"
    "Account no. 000-000000.001\n"
    "Debit advice\n"
    "Produced on 5 January 2020\n"
    "General information\n"
    "We will debit the following:\n"
    "Settlement\n"
    "UBS SARON Mortgage\n"
    "Account no. 000-000000.AAA 0000\n"
    "Description Value date Amount in CHF\n"
    "Interest 04.01.2020 44.44\n"
)


def _advice_row(direction: str, iban: str, **kw) -> dict:
    rows = parse_payment_advice_text(
        _advice_pdf_text(direction, iban, **kw), "synthetic-token")
    assert len(rows) == 1
    return rows[0]


class TestPaymentAdvice:
    def test_a_credit_advice_becomes_a_credit_on_the_receiving_account(self):
        r = _advice_row("Credit Advice", _ADV_IBAN_TO,
                        trx="0000 036 ED 0000123 ABC/ABC")
        assert r["account_external_id"] == "CH0000000000000000002"
        assert r["amount_credit"] == 12345.67
        assert r["amount_debit"] is None
        assert r["currency_iso"] == "CHF"
        assert json.loads(r["payload"])["advice_direction"] == "credit"

    def test_a_debit_advice_becomes_a_debit_on_the_paying_account(self):
        r = _advice_row("Debit Advice", _ADV_IBAN_FROM,
                        trx="0000 036 ED 0000123 ABC/ABC")
        assert r["account_external_id"] == "CH0000000000000000001"
        assert r["amount_debit"] == 12345.67
        assert r["amount_credit"] is None
        assert json.loads(r["payload"])["advice_direction"] == "debit"

    def test_the_amount_is_the_figure_printed_not_a_signed_one(self):
        # DESIGN.md §3.6: the PDF eras state the direction with the
        # column, never with the sign, and a consumer reads it there.
        debit = _advice_row("Debit Advice", _ADV_IBAN_FROM,
                            trx="0000 036 ED 0000123")
        assert debit["amount_debit"] > 0

    def test_both_legs_of_one_transfer_carry_the_same_id(self):
        # The pairing the whole parser exists for: UBS stamps one
        # transaction number on both sides, and silver's compound key
        # keeps them apart by account while the id brings them together.
        credit = _advice_row("Credit Advice", _ADV_IBAN_TO,
                             trx="0000 036 ED 0000123 ABC/ABC")
        debit = _advice_row("Debit Advice", _ADV_IBAN_FROM,
                            trx="0000 036 ED 0000123 ABC/ABC")
        assert (credit["transaction_external_id"]
                == debit["transaction_external_id"])
        assert (credit["account_external_id"]
                != debit["account_external_id"])

    def test_the_dates_are_the_booking_and_the_value_date(self):
        from datetime import datetime, timezone
        r = _advice_row("Credit Advice", _ADV_IBAN_TO,
                        trx="0000 036 ED 0000123 ABC/ABC")
        booked = datetime.fromtimestamp(r["booking_date"], timezone.utc)
        value = datetime.fromtimestamp(r["value_date"], timezone.utc)
        assert (booked.year, booked.month, booked.day) == (2020, 1, 1)
        assert (value.year, value.month, value.day) == (2020, 1, 1)

    def test_an_advice_without_a_value_date_books_on_its_entry_date(self):
        r = _advice_row("Credit Advice", _ADV_IBAN_TO,
                        trx="0000 036 ED 0000123", value_line="")
        assert r["value_date"] == r["booking_date"]

    def test_the_purpose_line_is_the_counterparty_not_the_holder(self):
        # The "By order of" block on an advice for a move between the
        # holder's own accounts is the holder; the purpose is the only
        # text that says anything about the movement.
        r = _advice_row("Credit Advice", _ADV_IBAN_TO,
                        trx="0000 036 ED 0000123 ABC/ABC")
        assert r["counterparty"] == "Increase Example Mandate"
        assert json.loads(r["payload"])["continuation"] == [
            "Increase Example Mandate"]

    def test_a_mandate_purpose_is_marked_an_internal_transfer(self):
        internal = _advice_row("Credit Advice", _ADV_IBAN_TO,
                               trx="0000 036 ED 0000123",
                               purpose="Uebertrag Example Portfolio")
        assert json.loads(internal["payload"])["internal_transfer"]
        external = _advice_row("Debit Advice", _ADV_IBAN_FROM,
                               trx="0000 036 ED 0000123",
                               purpose="YOUR PAYMENT ORDER DATED 01012020")
        assert not json.loads(external["payload"])["internal_transfer"]

    def test_no_booking_type_and_no_counter_account_are_invented(self):
        # Gold promotes a row to external capital off the booking type,
        # so a type this document never printed would fabricate capital.
        r = _advice_row("Credit Advice", _ADV_IBAN_TO,
                        trx="0000 036 ED 0000123")
        assert r["description_kind"] is None
        assert r["counter_account"] is None

    def test_the_row_is_marked_as_the_pdf_rail_and_as_an_advice(self):
        # `source` names the rail gold dates the MT940 feed against;
        # `document` is what the loader's re-derivation delete keys on.
        p = json.loads(_advice_row("Credit Advice", _ADV_IBAN_TO,
                                   trx="0000 036 ED 0000123")["payload"])
        assert p["source"] == "account_statement_pdf"
        assert p["document"] == "payment_advice_pdf"

    def test_the_older_two_column_layout_still_finds_its_account(self):
        rows = parse_payment_advice_text(_ADV_TWO_COLUMN_TEXT, "synthetic-token")
        assert len(rows) == 1
        assert rows[0]["account_external_id"] == "CH0000000000000000002"
        assert rows[0]["amount_credit"] == 98765.43
        assert rows[0]["transaction_external_id"] == "0000024ED0000456"

    def test_a_headline_sharing_its_line_with_the_date_still_reads(self):
        # Whether the headline and the production date arrive as one
        # printed line depends on how the page's columns resolve. The
        # headline is the only place the document says which way the
        # money went, so a direction lost to a merge would drop the
        # movement without a word.
        text = _advice_pdf_text("Credit Advice", _ADV_IBAN_TO,
                                trx="0000 036 ED 0000123").replace(
            "Credit Advice\nProduced on 2 January 2020",
            "Credit Advice Produced on 2 January 2020")
        rows = parse_payment_advice_text(text, "synthetic-token")
        assert len(rows) == 1
        assert rows[0]["amount_credit"] == 12345.67

    def test_a_document_that_is_not_a_payment_yields_nothing(self):
        # Both wear an advice label and neither is a movement: a
        # document that does not state the id its row would be keyed by
        # cannot be placed, and is left to the documents catalog alone.
        assert parse_payment_advice_text(_ADV_SAFE_BOX_TEXT, "t") == []
        assert parse_payment_advice_text(_ADV_MORTGAGE_TEXT, "t") == []


class TestAdviceTransactionNumber:
    """The advice prints the transaction number in space-separated
    groups; the CSV export spells the same number without them. The
    two have to normalise to one string or the legs never meet."""

    def test_the_printed_groups_join_into_the_exports_spelling(self):
        assert _advice_trx_no("0000 036 ED 0000123") == "0000036ED0000123"

    def test_the_booking_desks_initials_are_not_part_of_the_number(self):
        assert _advice_trx_no("0000 036 ED 0000123 ABC/ABC") == "0000036ED0000123"
        assert _advice_trx_no("ZZ00 068 ED 0000123 AB1/CD2") == "ZZ00068ED0000123"

    def test_a_line_that_is_not_a_number_is_refused(self):
        # A wrong id would key a row onto a transfer it has nothing to
        # do with, so anything but bare alphanumeric groups is rejected
        # rather than salvaged.
        assert _advice_trx_no("see enclosed advice") is None
        assert _advice_trx_no("0000 036") is None


def test_stmt_security_reads_the_valor_that_closes_a_caption():
    """A trade's continuation names the instrument and closes with the
    Swiss valor. The lines beside it — the order reference, the turnover
    trailer — do not end in a bare run of digits, which is the test."""
    caption, valor = _stmt_security([
        "A 28.09.2023 AA123980",
        "EXAMPLEETF WORLD 1234567",
        "Turnover total 1 111 111.11 1 111 111.11",
    ])
    assert (caption, valor) == ("EXAMPLEETF WORLD", "1234567")


def test_stmt_security_skips_the_settlement_reference_above_it():
    """A trade's settlement reference is a letter, a date and a NUMBER,
    and it comes first — so a scan taking the earliest match takes the
    reference and never reaches the security below it. The number is not
    a valor, so nothing was ever mis-stamped; the row simply stayed
    unresolved."""
    caption, valor = _stmt_security([
        "V 01.01.2020 16000000",
        "EXAMPLE SP 400 US 1234567",
    ])
    assert (caption, valor) == ("EXAMPLE SP 400 US", "1234567")


def test_stmt_security_declines_what_is_not_a_security_line():
    for lines in (
        [],
        ["A 01.01.2020 AB000000"],                      # an order reference
        ["V 01.01.2020 16000000"],                      # a settlement reference
        ["Turnover total 1 111 111.11 1 111 111.11"],   # an amount, not a valor
        ["1234567"],                                    # a number with no caption
        ["Payment to Example Payee"],                   # ordinary narrative
    ):
        assert _stmt_security(lines) == (None, None), lines


def test_stmt_security_reads_a_valor_printed_hard_against_its_caption():
    """A long caption leaves no room for the space and the statement
    prints the valor against it. The spaced form is tried first, so an
    ordinary line is unaffected."""
    assert _stmt_security(["Example Group Rg199999991"]) == (
        "Example Group Rg", "199999991")
    assert _stmt_security(["Exmp Index USD-2D-199999992"]) == (
        "Exmp Index USD-2D-", "199999992")
    # A spaced line still parses spaced — the glued pattern would split
    # it differently and must never get the chance.
    assert _stmt_security(["EXAMPLEETF WORLD 1234567"]) == (
        "EXAMPLEETF WORLD", "1234567")
    # And the shapes that are not valors stay rejected: a turnover
    # trailer, a bare date, a short trailing number in a name.
    for line in ("Turnover total 1 111 111.11", "10.04.2025", "Example Index 500"):
        assert _stmt_security([line]) == (None, None), line
