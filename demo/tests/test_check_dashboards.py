"""check_dashboards: a cut-off response is asked once more; a reported
error is not; each picker is varied alone around the defaults, a listed
one over the values the dashboard offers; the rows a run saves compare
with the next run's, and a tile that got slower is listed."""

import contextlib
import datetime as dt
import io
import pathlib
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import check_dashboards  # noqa: E402
# check_dashboards puts web/ on the path.
import metabase  # noqa: E402
import pickers  # noqa: E402
import provision  # noqa: E402

DASH, DC = {"id": 1}, {"id": 2, "card_id": 3, "parameter_mappings": []}
DONE = (202, {"status": "completed", "data": {"rows": [[1]]}})


class RunCard(unittest.TestCase):
    def run_with(self, *answers):
        client = mock.Mock()
        client.post.side_effect = list(answers)
        ok, rows, err, _params = check_dashboards.run_card(client, DASH, DC, {}, {})
        return ok, rows, err, client.post.call_count

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
        return provision.dashboard_parameters(dashboard)

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
        self.assertEqual(combos["defaults"][pickers.SHIFT_PARAM_ID], [0])
        self.assertEqual(combos["shift [-100]"], {**combos["defaults"], pickers.SHIFT_PARAM_ID: [-100]})

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


class FakeMetabase(metabase.Client):
    """Recorded answers of a Metabase with one dashboard of two tiles
    and no pickers, in the pre-defined collection unless `listed` is
    False. `rows` is card id -> the rows its query answers."""

    def __init__(self, rows, listed=True):
        super().__init__("http://mb")
        self.rows, self.listed = rows, listed

    def call(self, path, method="GET", data=None, timeout=None):
        if path == "/api/session":
            return 200, {"id": "sess"}
        if path == "/api/permissions/group":
            return 200, [{"id": 2, "name": "Administrators"}]
        if path == "/api/api-key":
            return 200, {"id": 5, "unmasked_key": "mb_synthetic"}
        if path.startswith("/api/api-key/"):
            return 204, None
        if path == "/api/collection":
            return 200, [{"id": 7, "name": provision.COLLECTION_NAME}] if self.listed else []
        if path.startswith("/api/collection/7/items"):
            return 200, {"data": [{"id": 1, "name": "Board"}]}
        if path == "/api/dashboard/1":
            return 200, {"id": 1, "name": "Board", "parameters": [], "dashcards": [
                {"id": 10 + c, "card_id": c, "parameter_mappings": [],
                 "card": {"name": f"Tile {c}", "display": "table"}} for c in sorted(self.rows)]}
        card = int(path.rsplit("/card/", 1)[1].split("/")[0])
        return 202, {"status": "completed", "data": {"rows": self.rows[card]}}


class SavedRows(unittest.TestCase):
    """--save-rows keeps every run's rows; --compare names each run that
    differs, floats within the bar, and fails on any."""

    def run_main(self, rows, *flags, fake=None):
        with tempfile.TemporaryDirectory() as d:
            pw = pathlib.Path(d, "pw")
            pw.write_text("synthetic\n")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                rc = check_dashboards.main(["--password-file", str(pw), *flags], fake or FakeMetabase(rows))
        return rc, out.getvalue()

    def test_saved_rows_compare_equal_and_name_a_difference(self):
        with tempfile.TemporaryDirectory() as d:
            saved = str(pathlib.Path(d, "rows.json"))
            rows = {3: [["a", 1.5], ["b", 2.0]], 4: [[7]]}
            rc, _ = self.run_main(rows, "--save-rows", saved)
            self.assertEqual(rc, 0)
            runs = check_dashboards.load_runs(saved)
            self.assertEqual(sorted(runs), [("Board", "Tile 3", "defaults"), ("Board", "Tile 4", "defaults")])
            self.assertEqual(runs[("Board", "Tile 3", "defaults")]["rows"], [["a", 1.5], ["b", 2.0]])

            rc, out = self.run_main({3: [["a", 1.5 * (1 + 1e-15)], ["b", 2.0]], 4: [[7]]}, "--compare", saved)
            self.assertEqual(rc, 0, out)
            self.assertIn("runs that differ from", out)
            self.assertNotIn("DIFF", out)

            rc, out = self.run_main({3: [["a", 1.5], ["b", 2.5]], 4: [[7], [8]]}, "--compare", saved)
            self.assertEqual(rc, 1)
            self.assertIn("DIFF Board | Tile 3 | defaults | 1 of 2 rows differ, the first is row 1", out)
            self.assertIn("DIFF Board | Tile 4 | defaults | 1 rows saved, 2 now", out)

    def test_a_missing_collection_stops(self):
        with self.assertRaises(SystemExit) as e:
            self.run_main({}, fake=FakeMetabase({}, listed=False))
        self.assertIn("collection not found", str(e.exception))

    def test_a_failed_listing_stops_with_its_status(self):
        class Failing(FakeMetabase):
            def call(self, path, method="GET", data=None, timeout=None):
                if path == "/api/collection":
                    return 500, {"message": "down"}
                return super().call(path, method, data, timeout)
        with self.assertRaises(SystemExit) as e:
            self.run_main({}, fake=Failing({}))
        self.assertEqual(str(e.exception), "could not list collections (500): {'message': 'down'}")


class CompareRuns(unittest.TestCase):
    @staticmethod
    def runs(*records):
        return {(d, c, label): {"ok": ok, "rows": rows, "seconds": s} for d, c, label, ok, rows, s in records}

    def test_floats_match_within_the_bar_and_nothing_else_does(self):
        same = check_dashboards.same_value
        self.assertTrue(same([1.0, "x", None], [1.0 + 1e-14, "x", None]))
        self.assertTrue(same([0.0], [5e-10]))
        self.assertTrue(same([3], [3.0]))
        self.assertFalse(same([1.0], [1.0 + 1e-9]))
        self.assertFalse(same(["1"], [1]))
        self.assertFalse(same([True], [1]))
        self.assertFalse(same([1, 2], [1]))

    def test_a_status_change_and_a_run_on_one_side_are_named(self):
        old = self.runs(("D", "a", "defaults", True, [[1]], 0.1), ("D", "b", "defaults", True, [], 0.1))
        new = self.runs(("D", "a", "defaults", False, [], 0.1), ("D", "c", "defaults", True, [], 0.1))
        self.assertEqual(check_dashboards.compare_runs(old, new),
                         [("D", "a", "defaults", "now fails"), ("D", "b", "defaults", "only in the saved runs"),
                          ("D", "c", "defaults", "not in the saved runs")])

    def test_a_tile_whose_runs_took_longer_in_all_is_slower(self):
        old = self.runs(("D", "a", "x", True, [], 0.4), ("D", "a", "y", True, [], 0.4),
                        ("D", "b", "x", True, [], 0.01), ("D", "c", "x", True, [], 2.0))
        new = self.runs(("D", "a", "x", True, [], 0.9), ("D", "a", "y", True, [], 0.4),
                        ("D", "b", "x", True, [], 0.2), ("D", "c", "x", True, [], 2.5))
        self.assertEqual(check_dashboards.slower_tiles(old, new), [("D", "a", 0.8, 1.3)])


if __name__ == "__main__":
    unittest.main()
