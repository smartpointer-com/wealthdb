"""check_dashboards: a cut-off response is asked once more; a reported
error is not; each picker is varied alone around the defaults."""

import datetime as dt
import pathlib
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import check_dashboards  # noqa: E402

DASH, DC = {"id": 1}, {"id": 2, "card_id": 3, "parameter_mappings": []}
DONE = (202, {"status": "completed", "data": {"rows": [[1]]}})


class RunCard(unittest.TestCase):
    def run_with(self, *answers):
        calls = mock.Mock(side_effect=list(answers))
        with mock.patch.object(check_dashboards, "call", calls):
            ok, rows, err, _params = check_dashboards.run_card("http://x", {}, DASH, DC, {}, {})
        return ok, rows, err, calls.call_count

    def test_a_cut_off_response_is_asked_once_more(self):
        self.assertEqual(self.run_with((202, None), DONE), (True, [[1]], None, 2))

    def test_two_cut_off_responses_fail(self):
        ok, _rows, err, n = self.run_with((202, None), (202, None))
        self.assertEqual((ok, err, n), (False, "no result", 2))

    def test_a_reported_error_fails_at_once(self):
        ok, _rows, err, n = self.run_with((202, {"status": "failed", "error": "bad column"}))
        self.assertEqual((ok, err, n), (False, "bad column", 1))


class Combos(unittest.TestCase):
    """The picker values a dashboard's cards run under."""

    PARAMS = [{"id": slug, "slug": slug, "default": default} for slug, default in (
        ("currency", ["USD"]), ("time_range", "past12months~"), ("source", None),
        ("rates", ["constant"]), ("maturing", ["cash"]), ("shift", [0]), ("horizon", ["12"]),
        ("window", ["this year"]))]

    def combos(self):
        return dict(check_dashboards.combos(self.PARAMS, ["src-1"], ["USD", "CHF"], 2024,
                                            dt.date(2026, 10, 21)))

    def test_the_projection_pickers_are_varied_one_at_a_time(self):
        defaults = self.combos()["defaults"]
        for label, slug, value in (("rates ['forward']", "rates", ["forward"]),
                                   ("maturing ['idle']", "maturing", ["idle"]),
                                   ("maturing ['roll']", "maturing", ["roll"]),
                                   ("shift [-100]", "shift", [-100]),
                                   ("horizon ['36']", "horizon", ["36"]),
                                   ("window ['last year']", "window", ["last year"]),
                                   ("window ['next months']", "window", ["next months"])):
            self.assertEqual(self.combos()[label], {**defaults, slug: value}, label)

    def test_the_existing_variations_stay(self):
        combos = self.combos()
        self.assertEqual(combos["defaults"]["shift"], [0])
        self.assertIn("currency ['CHF']", combos)
        self.assertIn("source src-1", combos)
        self.assertIn("window 2024-01-01~2026-10-21", combos)

    def test_a_picker_the_dashboard_lacks_is_not_varied(self):
        combos = dict(check_dashboards.combos(self.PARAMS[:3], [], ["USD"], 2024, dt.date(2026, 10, 21)))
        self.assertFalse([label for label in combos if label.startswith(("rates", "shift", "window ["))])


class EmptyAtDefaults(unittest.TestCase):
    def test_an_empty_card_at_the_defaults_fails_but_the_calendar(self):
        empties = {("defaults", "Income", "Expected payments"), ("defaults", "Income", "Expected income"),
                   ("defaults", "Income (privacy)", "Expected payments (privacy)"),
                   ("window past7days~", "Income", "Net income")}
        self.assertEqual(check_dashboards.empty_at_defaults(empties), [("Income", "Expected income")])


if __name__ == "__main__":
    unittest.main()
