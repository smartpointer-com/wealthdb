"""Unit tests for tax_form_parsers.py (the 1099-B parser).

Fully synthetic fixtures — invented security names, quantities, and
dollar amounts. No real account data ever enters a tracked file
(repo-root AGENTS.md §4). The fixtures mirror the real Schwab formats:
OFX-2.x XML (the preferred source) and the multi-section composite CSV
(the fallback).
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import tax_form_parsers as tf  # noqa: E402


# ============================================================
# Synthetic XML fixture (OFX 2.x)
# ============================================================
#
# Five lots exercising every branch the parser cares about:
#   1. Covered long-term lot with a real acquisition date + basis.
#   2. "Various" acquisition (DTVAR), short-term.
#   3. Noncovered, basis-not-shown, COSTBASIS=0 → placeholder, nulled.
#   4. Noncovered, basis-not-shown, COSTBASIS>0 → Schwab supplemental
#      basis, preserved.
#   5. Covered lot with a genuine $0 basis + a wash-sale disallowance.

_XML_LOT_TEMPLATE = """\
            <PROCDET_V100>
              <FORM8949CODE>{code}</FORM8949CODE>
              <DTSALE>{dtsale}</DTSALE>
              <SECNAME>{secname}</SECNAME>
              <SALEDESCRIPTION>{desc}</SALEDESCRIPTION>
              <NUMSHRS>{qty}</NUMSHRS>
              <COSTBASIS>{basis}</COSTBASIS>
              <SALESPR>{proceeds}</SALESPR>
              <ACCRUEDMKTDISCOUNT>0.00</ACCRUEDMKTDISCOUNT>
              <LONGSHORT>{term}</LONGSHORT>
{acquired}{wash}              <NONCOVEREDSECURITY>{noncov}</NONCOVEREDSECURITY>
              <BASISNOTSHOWN>{bns}</BASISNOTSHOWN>
            </PROCDET_V100>"""


def _xml_lot(*, code, dtsale, secname, desc, qty, basis, proceeds, term,
             acquired_line="", wash_line="", noncov="N", bns="N") -> str:
    return _XML_LOT_TEMPLATE.format(
        code=code, dtsale=dtsale, secname=secname, desc=desc, qty=qty,
        basis=basis, proceeds=proceeds, term=term,
        acquired=acquired_line, wash=wash_line, noncov=noncov, bns=bns)


def _make_1099b_xml(tax_year: str = "2021") -> str:
    lots = [
        _xml_lot(code="D", dtsale="20211001", secname="SYNTH ALPHA CORP",
                 desc="100.00 SYNTH ALPHA CORP", qty="100.000000",
                 basis="100.00", proceeds="3000.00", term="LONG",
                 acquired_line="              <DTAQD>20100104</DTAQD>\n"),
        _xml_lot(code="B", dtsale="20210615", secname="SYNTH BETA INC",
                 desc="50.00 SYNTH BETA INC", qty="50.000000",
                 basis="500.00", proceeds="1000.00", term="SHORT",
                 acquired_line="              <DTVAR>Y</DTVAR>\n"),
        _xml_lot(code="X", dtsale="20210401", secname="SYNTH GAMMA LLC",
                 desc="10.00 SYNTH GAMMA LLC", qty="10.000000",
                 basis="0.00", proceeds="250.00", term="SHORT",
                 noncov="Y", bns="Y"),
        _xml_lot(code="C", dtsale="20210401", secname="SYNTH DELTA SA",
                 desc="20.00 SYNTH DELTA SA", qty="20.000000",
                 basis="123.45", proceeds="800.00", term="SHORT",
                 noncov="Y", bns="Y"),
        _xml_lot(code="A", dtsale="20211220", secname="SYNTH EPSILON PLC",
                 desc="5.00 SYNTH EPSILON PLC", qty="5.000000",
                 basis="0.00", proceeds="40.00", term="LONG",
                 acquired_line="              <DTAQD>20210101</DTAQD>\n",
                 wash_line="              <WASHSALELOSSDISALLOWED>10.00"
                           "</WASHSALELOSSDISALLOWED>\n"),
    ]
    return (
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<?OFX OFXHEADER="200" VERSION="200" SECURITY="NONE" '
        'OLDFILEUID="NONE" NEWFILEUID="NONE" ?>\n'
        "<OFX>\n  <TAX1099MSGSRSV1>\n    <TAX1099TRNRS>\n"
        "      <TRNUID>SYNTH-UID</TRNUID>\n      <TAX1099RS>\n"
        "        <TAX1099B_V100>\n          <SRVRTID>SYNTH</SRVRTID>\n"
        f"          <TAXYEAR>{tax_year}</TAXYEAR>\n"
        "          <EXTDBINFO_V100>\n"
        + "\n".join(lots)
        + "\n          </EXTDBINFO_V100>\n        </TAX1099B_V100>\n"
        "      </TAX1099RS>\n    </TAX1099TRNRS>\n"
        "  </TAX1099MSGSRSV1>\n</OFX>\n"
    )


class TestParse1099bXml:
    def setup_method(self):
        self.result = tf.parse_1099b_xml(_make_1099b_xml("2021"))
        self.lots = self.result["lots"]
        self.by_name = {lot["security_name"]: lot for lot in self.lots}

    def test_tax_year_and_lot_count(self):
        assert self.result["tax_year"] == 2021
        assert len(self.lots) == 5

    def test_covered_lot_fields(self):
        lot = self.by_name["SYNTH ALPHA CORP"]
        assert lot["kind"] == "Sale"
        assert lot["date"] == "2021-10-01"
        assert lot["date_sold"] == "2021-10-01"
        assert lot["acquired_date"] == "2010-01-04"
        assert lot["quantity"] == 100.0
        assert lot["proceeds"] == 3000.0
        assert lot["amount"] == 3000.0
        assert lot["cost_basis"] == 100.0
        assert lot["term"] == "LONG"
        assert lot["noncovered"] is False
        assert lot["instrument_key"] is None  # no ticker/CUSIP on a 1099-B
        assert lot["symbol"] is None

    def test_various_acquisition(self):
        lot = self.by_name["SYNTH BETA INC"]
        assert lot["acquired_date"] == "Various"
        assert lot["term"] == "SHORT"

    def test_noncovered_zero_basis_is_nulled(self):
        lot = self.by_name["SYNTH GAMMA LLC"]
        assert lot["noncovered"] is True
        assert lot["basis_not_shown"] is True
        assert lot["cost_basis"] is None           # placeholder 0 → None
        assert lot["cost_basis_raw"] == 0.0        # raw preserved in payload
        assert lot["acquired_date"] is None

    def test_noncovered_supplemental_basis_preserved(self):
        lot = self.by_name["SYNTH DELTA SA"]
        assert lot["noncovered"] is True
        assert lot["basis_not_shown"] is True
        assert lot["cost_basis"] == 123.45         # Schwab supplemental basis

    def test_covered_genuine_zero_basis_preserved(self):
        lot = self.by_name["SYNTH EPSILON PLC"]
        assert lot["noncovered"] is False
        assert lot["cost_basis"] == 0.0            # real $0 basis, NOT nulled
        assert lot["wash_sale_disallowed"] == 10.0

    def test_no_b_section_returns_empty(self):
        xml = ('<?xml version="1.0"?>\n<OFX><TAX1099MSGSRSV1>'
               "<TAX1099TRNRS><TAX1099RS></TAX1099RS></TAX1099TRNRS>"
               "</TAX1099MSGSRSV1></OFX>")
        out = tf.parse_1099b_xml(xml)
        assert out == {"tax_year": None, "lots": []}

    def test_tax_year_derived_from_content_when_absent(self):
        xml = _make_1099b_xml("2021").replace(
            "<TAXYEAR>2021</TAXYEAR>", "")
        out = tf.parse_1099b_xml(xml)
        # all sold dates are in 2021 → modal year
        assert out["tax_year"] == 2021


# ============================================================
# Synthetic CSV fixture (multi-section composite)
# ============================================================

def _make_1099b_csv() -> str:
    # Box-number header in the real column order; index 11 = box 5
    # (noncovered), index 18 = box 12 (basis reported to IRS).
    boxes = ["1a", "1b", "1c", "1d", "1e", "1f", "1g", "2", "", "3", "4",
             "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16"]
    labels = ["Description of property (Example 100 sh. XYZ Co.)",
              "Date acquired", "Date sold or disposed", "Proceeds",
              "Cost or other basis", "Accrued market discount",
              "Wash sale loss disallowed",
              "Short-Term gain loss Long-term gain or loss Ordinary"] + [""] * 15

    def row(vals: dict) -> list[str]:
        r = [""] * len(boxes)
        idx = {b: i for i, b in enumerate(boxes) if b}
        for box, v in vals.items():
            r[idx[box]] = v
        return r

    # Rows mirror XML lots 1–3 exactly so the XML/CSV agreement test
    # compares like-for-like.
    covered = row({              # XML lot 1: covered long-term
        "1a": "100.00 SYNTH ALPHA CORP", "1b": "01/04/2010",
        "1c": "10/01/2021", "1d": "3000.00", "1e": "100.00",
        "2": "Long Term", "12": "X",
    })
    various = row({              # XML lot 2: covered, Various acquisition
        "1a": "50.00 SYNTH BETA INC", "1b": "Various", "1c": "06/15/2021",
        "1d": "1000.00", "1e": "500.00", "2": "Short Term", "12": "X",
    })
    noncov = row({               # XML lot 3: noncovered, basis-not-shown
        "1a": "10.00 SYNTH GAMMA LLC", "1c": "04/01/2021",
        "1d": "250.00", "1e": "0.00", "2": "Short Term", "5": "Noncovered",
    })

    import csv as _csv
    import io as _io
    buf = _io.StringIO()
    w = _csv.writer(buf)
    w.writerow([":Account", "XXXX-X999"])
    w.writerow(["Form 1099 DIV"])
    w.writerow(["Box", "Description", "Amount", "Total", "Details"])
    w.writerow(["1a", "Total ordinary dividends", "100.00", "", ""])
    w.writerow([])
    w.writerow(["Form 1099 B"])
    w.writerow(boxes)
    w.writerow(labels)
    w.writerow(covered)
    w.writerow(various)
    w.writerow(noncov)
    w.writerow([])
    return buf.getvalue()


class TestParse1099bCsv:
    def setup_method(self):
        self.result = tf.parse_1099b_csv(_make_1099b_csv())
        self.lots = self.result["lots"]
        self.by_name = {lot["security_name"]: lot for lot in self.lots}

    def test_finds_b_section_only(self):
        # The DIV section above must not leak into the lot list.
        assert len(self.lots) == 3
        assert set(self.by_name) == {
            "SYNTH ALPHA CORP", "SYNTH BETA INC", "SYNTH GAMMA LLC"}

    def test_tax_year_derived_from_sold_dates(self):
        assert self.result["tax_year"] == 2021

    def test_covered_row(self):
        lot = self.by_name["SYNTH ALPHA CORP"]
        assert lot["quantity"] == 100.0
        assert lot["date_sold"] == "2021-10-01"
        assert lot["acquired_date"] == "2010-01-04"
        assert lot["proceeds"] == 3000.0
        assert lot["cost_basis"] == 100.0
        assert lot["term"] == "LONG"
        assert lot["noncovered"] is False
        assert lot["source_format"] == "csv"

    def test_various_acquisition_row(self):
        lot = self.by_name["SYNTH BETA INC"]
        assert lot["acquired_date"] == "Various"
        assert lot["term"] == "SHORT"
        assert lot["cost_basis"] == 500.0
        assert lot["noncovered"] is False

    def test_noncovered_row_nulls_placeholder_basis(self):
        lot = self.by_name["SYNTH GAMMA LLC"]
        assert lot["acquired_date"] is None
        assert lot["term"] == "SHORT"
        assert lot["noncovered"] is True
        assert lot["basis_not_shown"] is True
        assert lot["cost_basis"] is None
        assert lot["cost_basis_raw"] == 0.0


class TestXmlCsvAgree:
    """The XML and CSV twins of the same logical form must agree on the
    facts the gold layer keys on (the loader dedups them to one set of
    rows, so divergence would be silently format-dependent)."""

    def test_overlapping_lots_match(self):
        xml = tf.parse_1099b_xml(_make_1099b_xml("2021"))
        csv = tf.parse_1099b_csv(_make_1099b_csv())
        assert xml["tax_year"] == csv["tax_year"]
        x = {lot["security_name"]: lot for lot in xml["lots"]}
        for name in ("SYNTH ALPHA CORP", "SYNTH BETA INC", "SYNTH GAMMA LLC"):
            c = next(lot for lot in csv["lots"] if lot["security_name"] == name)
            for field in ("date_sold", "acquired_date", "quantity",
                          "proceeds", "cost_basis", "term", "noncovered"):
                assert c[field] == x[name][field], (name, field)


# ============================================================
# Realized-lot reports: Year-End Summary and Gain/Loss Report
# ============================================================
#
# Synthetic page text in the line shapes pypdfium2 extracts from the
# two reports. Invented names, CUSIPs, amounts and dates; XMPL is a
# made-up ticker.

_YES_COLUMNS = (
    "Description OR\n"
    "Option Symbol\n"
    "CUSIP\n"
    "Number Quantity/Par\n"
    "Date\n"
    "Acquired\n"
    " Date\n"
    "Sold Total Proceeds (-)Cost Basis\n"
    "(+)Wash Sale\n"
    "Loss Disallowed\n"
    "(=)Realized\n"
    "Gain or (Loss)\n"
)

_YES_TEXT = (
    "TAX YEAR 2019\n"
    "YEAR-END SUMMARY\n"
    "Short-Term Realized Gain or (Loss). . . . . . . . 3\n"
    "Short-Term Realized Gain or (Loss)\n"
    " \n"
    "This section is for covered securities and corresponds to transactions"
    ' reported on your 1099-B as "cost basis is reported to the IRS."'
    " Report on Form 8949, Part I, with Box A checked.\n"
    + _YES_COLUMNS
    + "EXAMPLE HOLDINGS INC CLASS\n"
    "A\n"
    "000000AA1 10.00 01/05/19 03/10/19 $ 1,200.00 $ 1,000.00 -- $ 200.00 \n"
    "000000AA1 5.00 02/06/19 03/10/19 $ 600.00 $ 650.00 $ 50.00 $ 0.00 \n"
    "Security Subtotal $ 1,800.00 $ 1,650.00\n"
    " \n"
    "$ 50.00 $ 200.00 f\n"
    "XMPL 09/20/2019 30.00 P 3.00S 06/03/19 07/01/19 $ 750.00 $ 120.00t\n"
    " -- $ 630.00\n"
    "Security Subtotal $ 750.00 $ 120.00\n"
    " \n"
    "-- $ 630.00 f\n"
    'Please see the "Endnotes for Your Realized Gain or (Loss)" for an'
    " explanation of the codes and symbols.\n"
    "Account Number\n"
    "0000-0000\n"
    "A SAMPLE HOLDER\n"
    "Page 4 of 9\n"
    "Short-Term Realized Gain or (Loss) (continued)\n"
    "This section is for covered securities and corresponds to transactions"
    ' reported on your 1099-B as "cost basis is reported to the IRS."'
    " Report on Form 8949, Part I, with Box A checked.\n"
    + _YES_COLUMNS
    + "SYNTHETIC WIDGETS CORP CLASS A000000BB2 3.00 04/03/19 05/04/19"
    " $ 90.00 $ 60.00 -- $ 30.00 \n"
    "Security Subtotal $ 90.00 $ 60.00\n"
    " \n"
    "-- $ 30.00 f\n"
    "Total Short-Term (Covered) $ 2,640.00 $ 1,830.00 $ 50.00 $ 860.00 f\n"
    "Total Short-Term $ 2,640.00 $ 1,830.00 $ 50.00 $ 860.00 f\n"
    "Long-Term Realized Gain or (Loss)\n"
    " \n"
    "The transactions in this section are not reported on Form 1099-B or to"
    " the IRS. Report on Form 8949, Part II, with Box F checked.\n"
    + _YES_COLUMNS
    + "EXAMPLE TRUST UNITS 000000CC3 7.00 Various 08/08/19 $ 70.00 Missing -- --\n"
    "Security Subtotal $ 70.00 --\n"
    "EXAMPLE NOTE 4.5%\n"
    "000000DD4 1,000.00 01/02/15 09/01/19 $ 1,000.00 $\n"
    "$\n"
    "990.00\n"
    "Total Long-Term $ 1,070.00 -- -- --\n"
)


class TestYearEndSummary:
    def setup_method(self):
        self.result = tf.parse_year_end_summary_text(_YES_TEXT)
        self.lots = self.result["lots"]

    def test_tax_year_and_lot_count(self):
        assert self.result["tax_year"] == 2019
        assert len(self.lots) == 5
        # The bond line whose amounts scatter over several lines is left
        # out and counted.
        assert self.result["incomplete"] == 1

    def test_covered_lot_with_a_wrapped_description(self):
        lot = self.lots[0]
        assert (lot["security_name"], lot["cusip"], lot["instrument_key"]) == (
            "EXAMPLE HOLDINGS INC CLASS A", "000000AA1", "000000AA1")
        assert (lot["quantity"], lot["acquired_date"], lot["disposed_date"],
                lot["proceeds"], lot["cost_basis"], lot["wash_sale_disallowed"],
                lot["realized_gain_loss"]) == (
            10.0, "2019-01-05", "2019-03-10", 1200.0, 1000.0, None, 200.0)
        assert (lot["term"], lot["covered"], lot["form_8949_box"]) == (
            "SHORT", 1, "A")

    def test_wash_sale_and_description_shared_by_the_next_lot(self):
        lot = self.lots[1]
        # The second lot of a security prints only its CUSIP.
        assert (lot["security_name"], lot["cusip"]) == (None, "000000AA1")
        assert (lot["wash_sale_disallowed"], lot["realized_gain_loss"]) == (50.0, 0.0)

    def test_short_option_lot_with_wrapped_amounts(self):
        lot = self.lots[2]
        assert lot["instrument_key"] == "XMPL 09/20/2019 30.00 P"
        assert lot["cusip"] is None
        assert (lot["quantity"], lot["proceeds"], lot["cost_basis"],
                lot["wash_sale_disallowed"], lot["realized_gain_loss"]) == (
            3.0, 750.0, 120.0, None, 630.0)
        assert lot["footnotes"] == ["S", "t"]
        assert len(lot["raw_lines"]) == 2

    def test_page_furniture_and_glued_cusip(self):
        lot = self.lots[3]
        assert (lot["security_name"], lot["cusip"]) == (
            "SYNTHETIC WIDGETS CORP CLASS A", "000000BB2")
        assert (lot["covered"], lot["form_8949_box"]) == (1, "A")

    def test_not_reported_section_with_missing_basis(self):
        lot = self.lots[4]
        assert (lot["acquired_date"], lot["cost_basis"], lot["realized_gain_loss"]) == (
            "Various", None, None)
        assert (lot["term"], lot["covered"], lot["form_8949_box"]) == ("LONG", None, "F")

    def test_market_discount_column(self):
        text = _YES_TEXT.replace(
            "$ 600.00 $ 650.00 $ 50.00 $ 0.00",
            "$ 600.00 $ 650.00 -- $ 5.00 $ (55.00)")
        lot = tf.parse_year_end_summary_text(text)["lots"][1]
        assert (lot["wash_sale_disallowed"], lot["accrued_market_discount"],
                lot["realized_gain_loss"]) == (None, 5.0, -55.0)

    def test_subtitle_naming_two_boxes(self):
        assert tf._yes_subtitle(
            "The transactions in this section are not reported on Form 1099-B"
            " or to the IRS. Report on Form 8949, in either Part I with Box C"
            " checked or Part II with Box F checked, as appropriate.") == {
            "covered": None, "form_8949_box": "C,F"}
        assert tf._yes_subtitle(
            "This section is for noncovered securities and corresponds to"
            " transactions reported on your 1099-B as \"cost basis is available"
            " but not reported to the IRS.\" Report on Form 8949, Part II, with"
            " Box E checked.") == {"covered": 0, "form_8949_box": "E"}


_GLR_TEXT = (
    "2022 Year-End Schwab Gain/Loss Report\n"
    "Accounting Methods: The default accounting\n"
    "methods used in this report are compliant with IRS\n"
    "Example Account of\n"
    "Report Period\n"
    "Realized Gain or (Loss)\n"
    "Accounting Method \n"
    "Mutual Funds: First In First Out \n"
    "All Other Investments: High Cost \n"
    "Short-Term Quantity/Par\n"
    "Acquired/\n"
    "Opened\n"
    "Sold/\n"
    "Closed Total Proceeds Cost Basis\n"
    "Realized\n"
    "Gain or (Loss)\n"
    "EXAMPLE GROWTH ETF: XMPL 10.0000 01/03/22 02/04/22 $500.00 $650.00 t($150.00)\n"
    "EXAMPLE GROWTH ETF: XMPL 5.0000 01/10/22 02/04/22 $250.00 $200.00 $50.00\n"
    "Security Subtotal $750.00 $850.00 ($100.00)\n"
    "PUT EXAMPLE GROWTH $40 EXP\n"
    "06/17/22: XMPL 220617P00040000 \n"
    "1.0000 S05/02/22 06/01/22 $300.00 $120.00 $180.00\n"
    "Security Subtotal $300.00 $120.00 $180.00\n"
    "Page 3 of 8\n"
    "A SAMPLE HOLDER\n"
    "Realized Gain or (Loss) (continued)\n"
    "Accounting Method \n"
    "Mutual Funds: First In First Out\n"
    "All Other Investments: High Cost \n"
    "Short-Term (continued) Quantity/Par\n"
    "Closed Total Proceeds Cost Basis\n"
    "Realized\n"
    "Gain or (Loss)\n"
    "SYNTHETIC WARRANTS EXP\n"
    "07/01/27: XWRT \n"
    "100.0000 03/01/22 04/01/22 $20.00 $30.00 ($10.00)\n"
    "Total Short-Term $1,070.00 $1,000.00 $70.00\n"
    "Long-Term Quantity/Par\n"
    "Closed Total Proceeds Cost Basis\n"
    "Realized\n"
    "Gain or (Loss)\n"
    "SYNTHETIC BOND FUND: XBND 20.0000 01/04/21 03/01/22 $400.00 $380.00 $20.00\n"
    "Total Long-Term $400.00 $380.00 $20.00\n"
    "Total Realized Gain or (Loss) $1,470.00 $1,380.00 $90.00\n"
    "Contact: Example Desk\n"
)


class TestGainLossReport:
    def setup_method(self):
        self.result = tf.parse_gain_loss_report_text(_GLR_TEXT)
        self.lots = self.result["lots"]

    def test_methods_as_printed(self):
        assert self.result["methods"] == [
            {"asset_class": "Mutual Funds", "method": "First In First Out"},
            {"asset_class": "All Other Investments", "method": "High Cost"},
        ]

    def test_lots_terms_and_totals(self):
        assert self.result["tax_year"] == 2022
        assert self.result["incomplete"] == 0
        assert [(lot["instrument_key"], lot["term"]) for lot in self.lots] == [
            ("XMPL", "SHORT"), ("XMPL", "SHORT"),
            ("XMPL 06/17/2022 40.00 P", "SHORT"),
            ("XWRT", "SHORT"), ("XBND", "LONG")]
        assert sum(lot["proceeds"] for lot in self.lots) == 1470.0
        assert sum(lot["cost_basis"] for lot in self.lots) == 1380.0

    def test_lot_fields(self):
        lot = self.lots[0]
        assert (lot["security_name"], lot["quantity"], lot["acquired_date"],
                lot["disposed_date"], lot["proceeds"], lot["cost_basis"],
                lot["realized_gain_loss"], lot["footnotes"]) == (
            "EXAMPLE GROWTH ETF", 10.0, "2022-01-03", "2022-02-04",
            500.0, 650.0, -150.0, ["t"])
        # The report prints none of these.
        assert (lot["wash_sale_disallowed"], lot["covered"],
                lot["form_8949_box"], lot["cusip"]) == (None, None, None, None)

    def test_short_option_with_a_wrapped_description(self):
        lot = self.lots[2]
        assert lot["security_name"] == "PUT EXAMPLE GROWTH $40 EXP 06/17/22"
        assert (lot["quantity"], lot["acquired_date"], lot["footnotes"]) == (
            1.0, "2022-05-02", ["S"])

    def test_report_without_lots(self):
        assert tf.parse_gain_loss_report_text("2022 Year-End Schwab Gain/Loss Report\n") == {
            "tax_year": 2022, "lots": [], "incomplete": 0, "methods": []}


# ============================================================
# Year-End Summary lots read from the page layout
# ============================================================
#
# A bond lot that prints an adjusted basis stacks a second row of
# amounts under the first, and the text extractor interleaves the two.
# The fixture is a hand-built PDF whose text runs are drawn in that
# interleaved order, so the extracted text scatters the same way.

def _pdf(runs: list[tuple[float, float, str]]) -> bytes:
    """A one-page PDF (792 x 612 points) drawing each (x, y, text) run in
    7-point Helvetica, in the order given."""
    ops = []
    for x, y, text in runs:
        text = text.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")
        ops.append(f"BT /F1 7 Tf {x} {y} Td ({text}) Tj ET")
    stream = "\n".join(ops).encode()
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 792 612]"
        b" /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream",
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for i, body in enumerate(objs, 1):
        offsets.append(len(out))
        out += b"%d 0 obj\n" % i + body + b"\nendobj\n"
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
    out += b"".join(b"%010d 00000 n \n" % o for o in offsets)
    out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (
        len(objs) + 1, xref)
    return bytes(out)


_LAYOUT_HEAD = [
    (36, 570, "TAX YEAR 2023"),
    (36, 470, "Long-Term Realized Gain or (Loss)"),
    (36, 455, "The transactions in this section are not reported on Form"
              " 1099-B or to the IRS. Report on Form 8949, Part II, with Box F"
              " checked."),
    (36, 420, "Description OR"),
    (36, 410, "Option Symbol"),
    (410, 405, "Total Proceeds"),
    (503, 412, "(-)Cost Basis"),
    (518, 402, "Adjusted"),
    (594, 421, "(+)Wash Sale"),
    (582, 412, "Loss Disallowed"),
    (574, 402, "(-)Market Discount"),
    (692, 421, "(=)Realized"),
    (681, 412, "Gain or (Loss)"),
    (700, 402, "Adjusted"),
]


def _bond_runs(y: float, cost: str, adjusted: str, discount: str,
               gain: str, adjusted_gain: str) -> list[tuple[float, float, str]]:
    """A bond lot of 1,000.00 units at row `y` and its second row below,
    in the order the PDF draws them: the second row's amounts between
    the first row's. Amounts sit right-aligned under their headings."""
    low = y - 10

    def right(edge, text):
        return edge - 3.9 * len(text)

    return [
        (36, y, "EXAMPLE TREASURY NOTE"), (36, low, "1.250% MATURED"),
        (182, y, "000000DD4"), (270, y, "1,000.00"), (316, y, "01/02/15"),
        (350, y, "09/01/23"), (383, y, "$"), (430, y, "1,000.00"),
        (467, y, "$"), (467, low, "$"),
        (right(548, cost), y, cost), (right(548, adjusted), low, adjusted),
        (559, low, "$"), (635, y + 1, "--"),
        (right(640, discount), low, discount),
        (650, y, "$"), (650, low, "$"),
        (right(731, gain), y, gain), (right(731, adjusted_gain), low, adjusted_gain),
        (733, low + 2, "b"),
    ]


_LAYOUT_TAIL = [
    (36, 330, "SYNTH OMEGA LTD"), (182, 330, "000000OO9"), (290, 330, "4.00"),
    (316, 330, "01/04/10"), (350, 330, "10/01/23"), (383, 330, "$"),
    (440, 330, "80.00"), (467, 330, "$"), (530, 330, "20.00"),
    (635, 331, "--"), (650, 330, "$"), (712, 330, "60.00"),
    (36, 310, "Total Long-Term"),
    (36, 80, 'Please see the "Endnotes for Your Realized Gain or (Loss)"'),
]


class TestYearEndSummaryLayout:
    @staticmethod
    def _parse(tmp_path, *bonds, extra=()):
        path = tmp_path / "Year-End-Summary.PDF"
        path.write_bytes(_pdf([*_LAYOUT_HEAD, *(r for b in bonds for r in b),
                               *extra, *_LAYOUT_TAIL]))
        return tf.parse_realized_report_pdf(path, "year_end_summary")

    _BOND = _bond_runs(385, "990.00", "995.00", "4.00", "10.00", "6.00")

    def test_a_bond_lot_whose_amounts_scatter_is_read_from_the_page(self, tmp_path):
        result = self._parse(tmp_path, self._BOND)
        assert result["incomplete"] == 0
        lot = result["lots"][0]
        assert (lot["security_name"], lot["cusip"], lot["quantity"],
                lot["acquired_date"], lot["disposed_date"]) == (
            "EXAMPLE TREASURY NOTE 1.250% MATURED", "000000DD4", 1000.0,
            "2015-01-02", "2023-09-01")
        assert (lot["proceeds"], lot["cost_basis"], lot["wash_sale_disallowed"],
                lot["accrued_market_discount"], lot["realized_gain_loss"]) == (
            1000.0, 990.0, None, 4.0, 10.0)
        assert (lot["adjusted_cost_basis"], lot["adjusted_gain_loss"]) == (995.0, 6.0)
        assert (lot["term"], lot["covered"], lot["form_8949_box"],
                lot["footnotes"], lot["tax_year"]) == ("LONG", None, "F", ["b"], 2023)
        # The scattered fragments are kept as the lot's raw lines.
        assert len(lot["raw_lines"]) > 1

    def test_the_scattered_fragments_stay_out_of_the_next_lot(self, tmp_path):
        lot = self._parse(tmp_path, self._BOND)["lots"][1]
        assert (lot["security_name"], lot["proceeds"], lot["realized_gain_loss"]) == (
            "SYNTH OMEGA LTD", 80.0, 60.0)
        # A lot read from the text prints no adjusted figures.
        assert "adjusted_cost_basis" not in lot

    def test_lots_with_the_same_dates_and_quantity_keep_print_order(self, tmp_path):
        result = self._parse(
            tmp_path, self._BOND,
            _bond_runs(355, "980.00", "985.00", "3.00", "20.00", "17.00"))
        assert result["incomplete"] == 0
        assert [(lot["cost_basis"], lot["adjusted_gain_loss"])
                for lot in result["lots"][:2]] == [(990.0, 6.0), (980.0, 17.0)]

    def test_a_lot_whose_rows_do_not_read_is_counted(self, tmp_path):
        # A third row of amounts under the lot: no column says which
        # field it is.
        result = self._parse(tmp_path, self._BOND, extra=[(720, 365, "1.00")])
        assert result["incomplete"] == 1
        assert [lot["security_name"] for lot in result["lots"]] == ["SYNTH OMEGA LTD"]
