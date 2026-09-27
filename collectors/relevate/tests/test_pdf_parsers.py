"""Tests for the Relevate Quartalsbericht PDF parser.

Exercises the pure-text parser `_parse_quarterly_report_text`
against synthetic layout-mode-text fixtures — no real PDF is
read. Follows the same isolation pattern as the Swissquote
portfolio-performance parser tests.

All identifiers (account, ISINs, security names) are synthetic
per repo AGENTS.md §4.

Run from the collector directory:
    python3 -m unittest discover tests
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

try:
    import pdf_parsers  # noqa: E402
except ImportError:
    pdf_parsers = None


# Synthetic layout-mode text of an English Quartalsbericht. The
# whitespace mimics what pypdf's layout extraction produces: a
# page-1 metadata block, then a 'Portfolio Detail' section with
# one column-header line followed by asset-class subheaders and
# their position rows, finally an end-marker ('Performance').
#
# Numbers use the ASCII apostrophe for the thousands separator;
# the parser tolerates both U+2019 (real PDFs) and ASCII (tests).
SYNTHETIC_QUARTERLY_EN = """\
Quartalsbericht                                                                                                                              Page 1/3

  Reference no.                                       9999.999999.9
  Valuation date                                      31.12.2099

Portfolio Detail
  Asset Class         Security                                          ISIN              Currency      Units     Performance      Allocation     Allocation in CHF

  Stocks
                       Acme Index Equity Fund                            XX0000000001        USD       1'234.567        5.20             45.00         50'000.00
                       Globex Global Equity Fund                         XX0000000002        EUR         500.000        3.10             25.00         30'000.00

  Bonds
                       Initech Bond Fund 2.000% 31.12.2035               XX0000000003        CHF       1'000.000        1.50             15.00         15'000.00
  Accrued interest                                                                                                                          0.10            100.00

  Real Estate
                       Hooli Real Estate Trust                           XX0000000004        CHF         100.000        2.00              8.00          8'000.00

  Liquidity
   Liquidity CHF                                                                              CHF                                           7.00          7'000.00

Performance

Disclaimer
"""

# Synthetic German Quartalsbericht — same data, different section
# labels ('Aktien' / 'Obligationen' / 'Immobilien' / 'Liquidität'
# and 'Bewertungsdatum' / 'Referenz') to exercise the locale-
# fallback paths in _extract_account_external_id /
# _extract_valuation_date / ASSET_CLASS_HEADERS.
SYNTHETIC_QUARTERLY_DE = """\
Quartalsbericht                                                                                                                              Page 1/3

  Referenz Nr.                                        9999.999999.9
  Bewertungsdatum                                     30.09.2099

Portfolio Detail
  Asset Class         Security                                          ISIN              Currency      Units     Performance      Allocation     Allocation in CHF

  Aktien
                       Acme Index Equity Fund                            XX0000000001        USD       1'234.567        5.20             93.00         93'000.00

  Liquidität
   Liquidity CHF                                                                              CHF                                           7.00          7'000.00

Performance
"""

# Fixture that exercises the U+2019 (right single quotation mark)
# thousands separator — the form actually emitted by Relevate's
# PDFs in production.
SYNTHETIC_QUARTERLY_UNICODE_THSEP = """\
  Reference no.                                       9999.999999.9
  Valuation date                                      31.03.2099

Portfolio Detail
  Asset Class         Security                                          ISIN              Currency      Units     Performance      Allocation     Allocation in CHF

  Stocks
                       Acme Index Equity Fund                            XX0000000001        USD       1’234.567        5.20            100.00       100’000.00

Performance
"""


@unittest.skipIf(pdf_parsers is None, "pdf_parsers module unavailable")
class QuarterlyReportParserTest(unittest.TestCase):
    def test_english_full_fixture(self):
        """Page-1 metadata, four positions across three asset classes,
        cash + accrued interest — full happy path."""
        result = pdf_parsers._parse_quarterly_report_text(
            SYNTHETIC_QUARTERLY_EN, source_sha256="abc123",
        )

        self.assertEqual(result["account_external_id"], "9999.999999.9")
        self.assertEqual(result["currency"], "CHF")
        self.assertEqual(result["source_sha256"], "abc123")

        # 31.12.2099 UTC midnight → 4'102'358'400
        self.assertEqual(result["as_of_date"], 4102358400)

        self.assertEqual(len(result["positions"]), 4)
        by_isin = {p["isin"]: p for p in result["positions"]}

        # Asset-class assignment via section headers.
        self.assertEqual(by_isin["XX0000000001"]["asset_class"], "Stocks")
        self.assertEqual(by_isin["XX0000000002"]["asset_class"], "Stocks")
        self.assertEqual(by_isin["XX0000000003"]["asset_class"], "Bonds")
        self.assertEqual(by_isin["XX0000000004"]["asset_class"], "Real Estate")

        # Position numeric extraction.
        p1 = by_isin["XX0000000001"]
        self.assertEqual(p1["security_name"], "Acme Index Equity Fund")
        self.assertEqual(p1["currency"], "CHF")                # market_value denom
        self.assertEqual(p1["instrument_currency"], "USD")     # trading ccy
        self.assertAlmostEqual(p1["units"], 1234.567)
        self.assertAlmostEqual(p1["allocation_pct"], 0.45)     # 45.00 → 0.45
        self.assertAlmostEqual(p1["market_value"], 50000.00)

        # Cash + accrued interest.
        self.assertEqual(len(result["cash"]), 2)
        kinds = {c["balance_kind"]: c for c in result["cash"]}
        self.assertIn("cash", kinds)
        self.assertIn("accrued_interest", kinds)
        self.assertEqual(kinds["cash"]["currency"], "CHF")
        self.assertAlmostEqual(kinds["cash"]["amount"], 7000.00)
        self.assertAlmostEqual(kinds["cash"]["allocation_pct"], 0.07)
        self.assertEqual(kinds["accrued_interest"]["currency"], "CHF")
        self.assertAlmostEqual(kinds["accrued_interest"]["amount"], 100.00)

    def test_german_locale_fixture(self):
        """'Aktien' / 'Liquidität' / 'Referenz' / 'Bewertungsdatum'
        all resolve via fallback paths."""
        result = pdf_parsers._parse_quarterly_report_text(
            SYNTHETIC_QUARTERLY_DE, source_sha256="def456",
        )

        self.assertEqual(result["account_external_id"], "9999.999999.9")
        # 30.09.2099 UTC midnight → 4'094'409'600
        self.assertEqual(result["as_of_date"], 4094409600)

        self.assertEqual(len(result["positions"]), 1)
        self.assertEqual(result["positions"][0]["asset_class"], "Stocks")
        self.assertEqual(len(result["cash"]), 1)
        self.assertEqual(result["cash"][0]["balance_kind"], "cash")

    def test_unicode_thousands_separator(self):
        """U+2019 in numerics (the form pypdf actually extracts from
        Relevate's PDFs) parses identically to ASCII apostrophe."""
        result = pdf_parsers._parse_quarterly_report_text(
            SYNTHETIC_QUARTERLY_UNICODE_THSEP, source_sha256="ghi789",
        )

        self.assertEqual(len(result["positions"]), 1)
        p = result["positions"][0]
        self.assertAlmostEqual(p["units"], 1234.567)
        self.assertAlmostEqual(p["market_value"], 100000.00)

    def test_missing_reference_no_raises(self):
        text = "Valuation date 31.12.2099\nPortfolio Detail\nPerformance\n"
        with self.assertRaises(ValueError):
            pdf_parsers._parse_quarterly_report_text(text, source_sha256="x")

    def test_missing_valuation_date_raises(self):
        text = "Reference no. 9999.999999.9\nPortfolio Detail\nPerformance\n"
        with self.assertRaises(ValueError):
            pdf_parsers._parse_quarterly_report_text(text, source_sha256="x")


@unittest.skipIf(pdf_parsers is None, "pdf_parsers module unavailable")
class SwissNumberTest(unittest.TestCase):
    """The Swiss thousands-separator helper must tolerate both the
    typographic right-single-quote (U+2019) the PDFs emit and the
    ASCII apostrophe tests use."""

    def test_swiss_num_ascii_apostrophe(self):
        self.assertAlmostEqual(pdf_parsers._swiss_num("1'234.56"), 1234.56)
        self.assertAlmostEqual(pdf_parsers._swiss_num("1'234'567.89"), 1234567.89)

    def test_swiss_num_unicode_apostrophe(self):
        self.assertAlmostEqual(
            pdf_parsers._swiss_num("1’234.56"), 1234.56,
        )
        self.assertAlmostEqual(
            pdf_parsers._swiss_num("1’234’567.89"), 1234567.89,
        )

    def test_swiss_num_negative(self):
        self.assertAlmostEqual(pdf_parsers._swiss_num("-12.34"), -12.34)


# ============================================================
# Credit-note (Gutschriftsanzeige) parser tests
# ============================================================

# Synthetic default-mode text for a Gutschriftsanzeige. The
# Gutschriftsanzeige is visually two-column (labels left, values
# right); pypdf's default extraction concatenates the right column
# verbatim after the left, so the values arrive in this fixed
# order: amount → Valuta → Stiftung → Referenznummer → PII fields.
#
# All numerics and identifiers are synthetic.
SYNTHETIC_CREDIT_NOTE = """\
the relevate-way to manage your money
Gutschriftsanzeige Seite 1/1
Gutschriftsanzeige
Stiftung
Referenznummer
Kundenangaben
Vorname
Nachname
E-Mail
Telefon
Adresse
PLZ / Ort
Geburtsdatum
AHV-Nr.
Gerne bestätigen wir Ihnen, dass wir folgenden Betrag auf Ihrem Portfolio
gutgeschrieben haben:
Betrag:
Valuta:
CHF 1'234.56
15.07.2099
PensFree
9999.999999.9
Jane
Doe
EMAIL
+##########
Example Street 1
9999 Example City
01.01.2000
756.0000.0000.00
"""


@unittest.skipIf(pdf_parsers is None, "pdf_parsers module unavailable")
class CreditNoteParserTest(unittest.TestCase):
    def test_happy_path(self):
        """Anchor on the 'CHF <amount>' line; pick the next DD.MM.YYYY
        as Valuta; the birthdate further down must NOT confuse it."""
        r = pdf_parsers._parse_credit_note_text(
            SYNTHETIC_CREDIT_NOTE, source_sha256="cn-sha",
        )
        self.assertEqual(r["account_external_id"], "9999.999999.9")
        self.assertEqual(r["currency"], "CHF")
        self.assertAlmostEqual(r["amount"], 1234.56)
        self.assertEqual(r["kind"], "contribution")
        self.assertEqual(r["source_sha256"], "cn-sha")

        # 15.07.2099 UTC midnight
        import datetime as _dt
        expected = int(_dt.datetime(
            2099, 7, 15, tzinfo=_dt.timezone.utc,
        ).timestamp())
        self.assertEqual(r["occurred_at"], expected)

    def test_unicode_thousands_separator(self):
        """U+2019 in the amount line — what pypdf actually emits."""
        text = SYNTHETIC_CREDIT_NOTE.replace(
            "CHF 1'234.56", "CHF 1’234’567.89",
        )
        r = pdf_parsers._parse_credit_note_text(text, source_sha256="x")
        self.assertAlmostEqual(r["amount"], 1234567.89)

    def test_missing_amount_raises(self):
        text = SYNTHETIC_CREDIT_NOTE.replace("CHF 1'234.56", "")
        with self.assertRaises(ValueError):
            pdf_parsers._parse_credit_note_text(text, source_sha256="x")

    def test_missing_account_raises(self):
        text = SYNTHETIC_CREDIT_NOTE.replace("9999.999999.9", "")
        with self.assertRaises(ValueError):
            pdf_parsers._parse_credit_note_text(text, source_sha256="x")

    def test_missing_valuta_raises(self):
        # Strip both DD.MM.YYYY occurrences (Valuta + birthdate).
        import re as _re
        text = _re.sub(r"\b\d{2}\.\d{2}\.\d{4}\b", "", SYNTHETIC_CREDIT_NOTE)
        with self.assertRaises(ValueError):
            pdf_parsers._parse_credit_note_text(text, source_sha256="x")


if __name__ == "__main__":
    unittest.main()
