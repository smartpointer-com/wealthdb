"""check_dashboards: a cut-off response is asked once more; a reported
error is not; each picker is varied alone around the defaults, a listed
one over the values the dashboard offers."""

import datetime as dt
import pathlib
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import check_dashboards  # noqa: E402
# check_dashboards puts web/ on the path.
import provision  # noqa: E402

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
    """The picker values a dashboard's cards run under, over the pickers
    provisioning gives it."""

    def params(self, dashboard):
        models = {name: i for i, name in enumerate(provision.report_models(), start=1)}
        return provision.dashboard_parameters(models, "range", dashboard)

    def combos(self, dashboard="Income"):
        return dict(check_dashboards.combos(self.params(dashboard), ["src-1"], ["USD", "CHF"], 2024,
                                            dt.date(2026, 10, 21)))

    def test_a_listed_picker_runs_over_each_value_but_its_default(self):
        combos = self.combos()
        listed = [p for p in self.params("Income") if p.get("values_source_type") == "static-list"]
        self.assertEqual({p["slug"] for p in listed}, {"currency", "rates", "maturing", "horizon"})
        for p in listed:
            (default,) = p["default"]
            self.assertNotIn(f"{p['slug']} {[default]}", combos)
            for v in p["values_source_config"]["values"]:
                if v != default:
                    label = f"{p['slug']} {[v]}"
                    self.assertEqual(combos[label], {**combos["defaults"], p["id"]: [v]}, label)
        for label in ("rates ['forward']", "maturing ['idle']", "maturing ['roll']", "horizon ['24']",
                      "horizon ['36']", "currency ['CHF']"):
            self.assertIn(label, combos)

    def test_the_shift_runs_100_basis_points_lower(self):
        combos = self.combos()
        self.assertEqual(combos["defaults"][provision.SHIFT_PARAM_ID], [0])
        self.assertEqual(combos["shift [-100]"], {**combos["defaults"], provision.SHIFT_PARAM_ID: [-100]})

    def test_the_time_range_and_source_variations(self):
        combos = self.combos()
        self.assertIn("source src-1", combos)
        self.assertIn("time_range past7days~", combos)
        self.assertIn("time_range past3years~", combos)
        self.assertIn("time_range 2024-01-01~2026-10-21", combos)

    def test_a_picker_the_dashboard_lacks_is_not_varied(self):
        combos = self.combos("Exposure")
        self.assertIn("currency ['CHF']", combos)
        self.assertFalse([label for label in combos
                          if label.startswith(("rates", "maturing", "shift", "horizon"))])


class EmptyAtDefaults(unittest.TestCase):
    def test_an_empty_card_at_the_defaults_fails_but_the_calendar(self):
        empties = {("defaults", "Income", "Expected payments"), ("defaults", "Income", "Income in the window"),
                   ("defaults", "Income (privacy)", "Expected payments (privacy)"),
                   ("time_range past7days~", "Income", "Net income")}
        self.assertEqual(check_dashboards.empty_at_defaults(empties), [("Income", "Income in the window")])


class NullAtDefaults(unittest.TestCase):
    def test_an_income_figure_null_at_the_defaults_fails(self):
        figures = [("defaults", "Income", "Withholding", [[None]]),
                   ("defaults", "Income (privacy)", "Withholding (privacy)", [[0]]),
                   ("source src-1", "Income", "Received income", [[None]]),
                   ("defaults", "Returns", "Return (TWR)", [[None]])]
        self.assertEqual(check_dashboards.null_at_defaults(figures), [("Income", "Withholding")])


if __name__ == "__main__":
    unittest.main()
