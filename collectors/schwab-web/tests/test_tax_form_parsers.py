"""Unit tests for tax_form_parsers.py (the 1099-B parser).

Fully synthetic fixtures — invented security names, quantities, and
dollar amounts. No real account data ever enters a tracked file
(repo-root CLAUDE.md §4). The fixtures mirror the real Schwab formats:
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
