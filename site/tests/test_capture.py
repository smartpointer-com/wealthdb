"""Tests for site/capture.py's pure parts; no browser, no Metabase."""

import datetime as dt
import sys
import unittest
import urllib.parse
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import capture  # noqa: E402

AS_OF = dt.date(2026, 10, 30)


class DefaultsQuery(unittest.TestCase):
    def test_every_picker_is_set(self):
        q = capture.defaults_query([
            {"slug": "currency", "type": "string/=", "default": ["USD"]},
            {"slug": "time_range", "type": "date/all-options", "default": "past12months~"},
            {"slug": "as_of_day", "type": "date/single", "default": "thisday"},
            {"slug": "shift", "type": "number/=", "default": [0]},
            {"slug": "source", "type": "string/="},
            {"slug": "section", "type": "string/=", "default": None},
        ], AS_OF)
        self.assertEqual(urllib.parse.parse_qsl(q, keep_blank_values=True), [
            ("currency", "USD"), ("time_range", "2025-10-01~2026-10-30"), ("as_of_day", "2026-10-30"),
            ("shift", "0"), ("source", ""), ("section", "")])

    def test_a_list_default_repeats_the_picker(self):
        q = capture.defaults_query([{"slug": "type", "type": "string/=", "default": ["interest", "coupons"]}], AS_OF)
        self.assertEqual(urllib.parse.parse_qs(q)["type"], ["interest", "coupons"])

    def test_a_relative_string_picker_is_not_a_date(self):
        q = capture.defaults_query([{"slug": "rates", "type": "string/=", "default": "past3months"}], AS_OF)
        self.assertEqual(urllib.parse.parse_qs(q)["rates"], ["past3months"])

    def test_no_pickers(self):
        self.assertEqual(capture.defaults_query([], AS_OF), "")


class PinDate(unittest.TestCase):
    def test_months_with_this_month(self):
        self.assertEqual(capture.pin_date("past12months~", AS_OF), "2025-10-01~2026-10-30")

    def test_months_without_this_month(self):
        self.assertEqual(capture.pin_date("past3months", AS_OF), "2026-07-01~2026-09-30")

    def test_months_across_a_year_start(self):
        self.assertEqual(capture.pin_date("past2months~", dt.date(2027, 1, 15)), "2026-11-01~2027-01-15")

    def test_days(self):
        self.assertEqual(capture.pin_date("past7days~", AS_OF), "2026-10-24~2026-10-30")
        self.assertEqual(capture.pin_date("past7days", AS_OF), "2026-10-23~2026-10-29")

    def test_this_day(self):
        self.assertEqual(capture.pin_date("thisday", AS_OF), "2026-10-30")

    def test_fixed_values_pass(self):
        for v in ("2026-01-01~2026-06-30", "2026-03-31"):
            self.assertEqual(capture.pin_date(v, AS_OF), v)

    def test_unknown_relative_values_stop(self):
        for v in ("thisyear", "next3months", "past2quarters~"):
            with self.assertRaises(SystemExit, msg=v):
                capture.pin_date(v, AS_OF)


class ShotName(unittest.TestCase):
    def test_names_follow_the_page(self):
        self.assertEqual(capture.shot_name("Cash Flow", "light"), "cashflow.png")
        self.assertEqual(capture.shot_name("Wealth Overview", "dark"), "overview-dark.png")

    def test_every_dashboard_has_a_page_image(self):
        page = (Path(__file__).resolve().parent.parent / "public/index.html").read_text()
        for name in capture.DASHBOARDS:
            for scheme in capture.SCHEMES:
                stem = capture.shot_name(name, scheme).removesuffix(".png")
                self.assertIn(f'"shots/{stem}.webp"', page)


if __name__ == "__main__":
    unittest.main()
