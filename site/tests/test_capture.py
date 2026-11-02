"""Tests for site/capture.py's pure parts; no browser, no Metabase."""

import datetime as dt
import re
import sys
import unittest
import urllib.parse
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import capture  # noqa: E402

AS_OF = dt.date(2026, 10, 30)
PAGE = (Path(__file__).resolve().parent.parent / "public/index.html").read_text()


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
        self.assertEqual(capture.pin_date("past7days~", AS_OF), "2026-10-23~2026-10-30")
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
        for name in capture.DASHBOARDS:
            for scheme in capture.SCHEMES:
                stem = capture.shot_name(name, scheme).removesuffix(".png")
                self.assertIn(f'"shots/{stem}.webp"', PAGE)

    def test_every_page_image_is_a_shot(self):
        stems = {capture.shot_name(n, s).removesuffix(".png") for n in capture.DASHBOARDS for s in capture.SCHEMES}
        named = set(re.findall(r"shots/([\w-]+)\.webp", PAGE))
        self.assertTrue(named)
        self.assertLessEqual(named, stems)


class FindDashboards(unittest.TestCase):
    """find_dashboards against a fake Metabase API."""

    def fake(self, collections, dashboards):
        calls = []

        def call(base, path, method="GET", body=None, session=None):
            calls.append(path)
            if path == "/api/collection":
                return collections
            if path.startswith("/api/collection/"):
                return {"data": [{"name": n, "id": i} for i, n in enumerate(dashboards, 1)]}
            return {"id": int(path.rsplit("/", 1)[1])}

        old, capture.call = capture.call, call
        self.addCleanup(setattr, capture, "call", old)
        return calls

    def test_finds_every_dashboard_in_the_live_collection(self):
        calls = self.fake([{"id": 4, "name": capture.provision.COLLECTION_NAME, "archived": True},
                           {"id": 5, "name": capture.provision.COLLECTION_NAME}],
                          [*capture.DASHBOARDS, "Something else"])
        found = capture.find_dashboards("http://mb", "s")
        self.assertEqual(sorted(found), sorted(capture.DASHBOARDS))
        self.assertIn("/api/collection/5/items?models=dashboard", calls)
        self.assertNotIn("/api/dashboard/9", calls)

    def test_a_missing_dashboard_stops(self):
        self.fake([{"id": 5, "name": capture.provision.COLLECTION_NAME}], list(capture.DASHBOARDS)[1:])
        with self.assertRaises(SystemExit) as e:
            capture.find_dashboards("http://mb", "s")
        self.assertIn(next(iter(capture.DASHBOARDS)), str(e.exception))

    def test_a_missing_collection_stops(self):
        self.fake([{"id": 4, "name": capture.provision.COLLECTION_NAME, "archived": True}], [])
        with self.assertRaises(SystemExit):
            capture.find_dashboards("http://mb", "s")


if __name__ == "__main__":
    unittest.main()
