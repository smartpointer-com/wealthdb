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


class BrokenClicks(unittest.TestCase):
    """A click to a dashboard lands where it is there and carries each
    picker the click names."""

    @staticmethod
    def dash(name, params, clicks=()):
        return {"name": name, "parameters": [{"id": p} for p in params],
                "dashcards": [{"card": {"name": card}, "visualization_settings": {"click_behavior": {
                    "type": "link", "linkType": "dashboard", "targetId": target,
                    "parameterMapping": {t: {"id": t, "source": {"type": "parameter", "id": s}}
                                         for t, s in mapping.items()}}}}
                              for card, target, mapping in clicks]}

    def test_a_click_lands_where_both_sides_carry_its_pickers(self):
        dashes = {1: self.dash("Overview", ["ccy"], [("Figure", 2, {"ccy-2": "ccy"})]),
                  2: self.dash("Home", ["ccy-2"])}
        self.assertEqual(check_dashboards.broken_clicks(dashes), [])

    def test_a_missing_dashboard_or_picker_is_named(self):
        dashes = {1: self.dash("Overview", ["ccy"], [("Gone", 9, {}), ("Lost", 2, {"nope": "ccy"}),
                                                    ("Stray", 2, {"ccy-2": "nope"})]),
                  2: self.dash("Home", ["ccy-2"])}
        self.assertEqual([(c, why.split(",")[0]) for _d, c, why in check_dashboards.broken_clicks(dashes)],
                         [("Gone", "opens dashboard 9"), ("Lost", "sets nope"), ("Stray", "passes nope")])

    def test_a_column_source_needs_no_picker_of_its_own(self):
        dashes = {1: self.dash("Overview", [], [("Year", 2, {})]), 2: self.dash("Returns", ["year"])}
        dashes[1]["dashcards"][0]["card"]["result_metadata"] = [{"name": "start_year"}]
        dashes[1]["dashcards"][0]["visualization_settings"]["click_behavior"]["parameterMapping"] = {
            "year": {"id": "year", "source": {"type": "column", "id": "start_year"}}}
        self.assertEqual(check_dashboards.broken_clicks(dashes), [])

    def test_a_card_that_states_no_columns_yet_is_not_flagged(self):
        dashes = {1: self.dash("Overview", [], [("Year", 2, {})]), 2: self.dash("Returns", ["year"])}
        dashes[1]["dashcards"][0]["visualization_settings"]["click_behavior"]["parameterMapping"] = {
            "year": {"id": "year", "source": {"type": "column", "id": "start_year"}}}
        self.assertEqual(check_dashboards.broken_clicks(dashes), [])

    def test_a_column_the_card_does_not_return_is_named(self):
        dashes = {1: self.dash("Overview", [], [("Mix", 2, {})]), 2: self.dash("Allocation", ["day"])}
        dashes[1]["dashcards"][0]["card"]["result_metadata"] = [{"name": "share_pct"}]
        dashes[1]["dashcards"][0]["visualization_settings"]["click_behavior"]["parameterMapping"] = {
            "day": {"id": "day", "source": {"type": "column", "id": "as_of_day"}}}
        self.assertEqual([(c, why) for _d, c, why in check_dashboards.broken_clicks(dashes)],
                         [("Mix", "passes column as_of_day, which the card does not return")])

    def test_the_column_sources_of_a_card_that_has_not_run_are_counted(self):
        dashes = {1: self.dash("Overview", ["ccy"], [("Year", 2, {"ccy-2": "ccy"}), ("Mix", 2, {})]),
                  2: self.dash("Returns", ["ccy-2", "year", "day"])}
        ran, fresh = dashes[1]["dashcards"]
        ran["card"]["result_metadata"] = [{"name": "start_year"}]
        for dc, tid, col in ((ran, "year", "start_year"), (fresh, "day", "as_of_day")):
            dc["visualization_settings"]["click_behavior"]["parameterMapping"][tid] = {
                "id": tid, "source": {"type": "column", "id": col}}
        self.assertEqual(check_dashboards.broken_clicks(dashes), [])
        self.assertEqual(check_dashboards.unchecked_columns(dashes), 1)


if __name__ == "__main__":
    unittest.main()
