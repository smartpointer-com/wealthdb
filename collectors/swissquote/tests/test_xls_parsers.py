"""
Fixture tests for the Positions and List of Assets XLS parsers.

Workbooks are synthesised with xlwt at test time (no binary fixtures
in the repo), with obviously fake values. The header gates must:
  - accept both header variants the export engine has produced —
    with and without a trailing blank cell (the 2026-08-19 positions
    export dropped its historical trailing blank);
  - still fail loud on any change to a labelled column.
Row handling: section headers set asset_class, subtotal/Total rows
drop, position rows land with their named fields.
"""

from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

import xlwt

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import load  # noqa: E402

POSITIONS_LABELS = load.POSITIONS_EXPECTED_HEADER
LOA_LABELS = load.LOA_EXPECTED_HEADER


def _write_xls(path: Path, rows: list[list]) -> None:
    wb = xlwt.Workbook()
    sheet = wb.add_sheet("export")
    for r, row in enumerate(rows):
        for c, val in enumerate(row):
            sheet.write(r, c, val)
    wb.save(str(path))


# A minimal positions sheet: one section header, one position row,
# a subtotal, a second section with one row, and the grand Total.
_POSITIONS_BODY = [
    ["ETFs"],
    ["", "FAKEETF", 10.0, 100.0, 1000.0, 1.0, 0.1, 100.5, "CHF",
     5.0, 0.5, 1000.0, 60.0],
    ["", "Subtotal ETFs", "", "", 1000.0],
    ["Equities"],
    ["", "FAKEEQ", 2.0, 300.0, 600.0, -1.0, -0.2, 305.0, "USD",
     12.0, 2.0, 660.0, 40.0],
    ["", "Total", "", "", "", "", "", "", "", "", "", 1660.0, 100.0],
]

_LOA_BODY = [
    ["CHF", 1.0, 111.0, 1000.0, 1111.0, 1111.0, 62.5],
    ["USD", 0.9, 0.0, 600.0, 600.0, 660.0, 37.5],
    ["Total CHF", "", "", "", "", 1771.0, 100.0],
]


class PositionsXlsTests(unittest.TestCase):
    def _parse(self, header: list) -> list[dict]:
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / "positions.xls"
            _write_xls(p, [header] + _POSITIONS_BODY)
            return load.parse_positions_xls(p)

    def _check_rows(self, rows: list[dict]) -> None:
        self.assertEqual(len(rows), 2)
        etf, eq = rows
        self.assertEqual(etf["asset_class"], "ETFs")
        self.assertEqual(etf["symbol"], "FAKEETF")
        self.assertEqual(etf["quantity"], 10.0)
        self.assertEqual(etf["currency"], "CHF")
        self.assertEqual(etf["positions_pct"], 60.0)
        self.assertEqual(eq["asset_class"], "Equities")
        self.assertEqual(eq["symbol"], "FAKEEQ")
        self.assertEqual(eq["currency"], "USD")
        self.assertEqual(eq["total_value_chf"], 660.0)

    def test_header_without_trailing_blank(self):
        # 13-cell variant, first seen 2026-08-19.
        self._check_rows(self._parse(list(POSITIONS_LABELS)))

    def test_header_with_trailing_blank(self):
        # 14-cell variant produced by exports up to 2026-08-15.
        self._check_rows(self._parse(list(POSITIONS_LABELS) + [""]))

    def test_renamed_labelled_column_fails(self):
        bad = list(POSITIONS_LABELS)
        bad[bad.index("Quantity")] = "Qty"
        with self.assertRaises(SystemExit):
            self._parse(bad)

    def test_reordered_labelled_columns_fail(self):
        bad = list(POSITIONS_LABELS)
        bad[1], bad[2] = bad[2], bad[1]
        with self.assertRaises(SystemExit):
            self._parse(bad)

    def test_extra_labelled_column_fails(self):
        with self.assertRaises(SystemExit):
            self._parse(list(POSITIONS_LABELS) + ["Extra"])


class ListOfAssetsXlsTests(unittest.TestCase):
    def _parse(self, header: list) -> list[dict]:
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / "list_of_assets.xls"
            _write_xls(p, [header] + _LOA_BODY)
            return load.parse_list_of_assets_xls(p)

    def _check_rows(self, rows: list[dict]) -> None:
        # "Total CHF" footer dropped; per-currency rows kept.
        self.assertEqual([r["currency"] for r in rows], ["CHF", "USD"])
        self.assertEqual(rows[0]["cash_balance"], 111.0)
        self.assertEqual(rows[1]["valuation_chf"], 660.0)

    def test_header_exact(self):
        self._check_rows(self._parse(list(LOA_LABELS)))

    def test_header_with_trailing_blank(self):
        # Not yet observed for this sheet, but the same export engine
        # dropped/added a trailing blank on positions.xls.
        self._check_rows(self._parse(list(LOA_LABELS) + [""]))

    def test_renamed_labelled_column_fails(self):
        bad = list(LOA_LABELS)
        bad[0] = "Ccy"
        with self.assertRaises(SystemExit):
            self._parse(bad)


class TrimTrailingBlanksTests(unittest.TestCase):
    def test_trims_only_trailing(self):
        self.assertEqual(
            load._trim_trailing_blanks(["", "A", "", "B", "", ""]),
            ["", "A", "", "B"],
        )
        self.assertEqual(load._trim_trailing_blanks([]), [])
        self.assertEqual(load._trim_trailing_blanks(["", ""]), [])


if __name__ == "__main__":
    unittest.main()
