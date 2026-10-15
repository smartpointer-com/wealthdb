"""check_dashboards: a cut-off response is asked once more; a reported
error is not."""

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


if __name__ == "__main__":
    unittest.main()
