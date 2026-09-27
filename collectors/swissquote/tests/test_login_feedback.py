"""
The MFA page's API-call recorder.

login.py logs what the wait page requested, and how long each call was held,
because the approval channel it used to ride has moved once already: a route
that was a genuine long poll in June answered 404 by September, and what
replaced it is a short poll. Recording the page's own calls — id-masked, so a
line can be pasted into a report, and ordered by duration, because a held
call is the one worth polling — is what makes the next move diagnosable
instead of guessable.

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import login  # noqa: E402

class UrlShapeTests(unittest.TestCase):
    """The MFA page's API calls are logged so a moved route can be re-derived.

    They are logged as SHAPES: the path is the diagnostic, while the ids in
    it are session-scoped and the whole point is that the line can be pasted
    into a bug report.
    """

    def test_a_session_id_is_elided_but_the_path_survives(self):
        got = login.url_shape(
            "https://trade.swissquote.ch/sq-thirdlevel-plugin/api/thirdlevel"
            "/smartL3/feedback/listen/0123456789abcdef0123456789abcdef"
            "?cache=false&timeout=20000"
        )
        self.assertNotIn("0123456789abcdef", got)
        self.assertIn("<id>", got)
        # The parts that identify the endpoint are all still readable.
        for keep in ("/api/thirdlevel/", "smartL3", "feedback/listen",
                     "timeout=20000"):
            self.assertIn(keep, got)

    def test_a_credential_query_parameter_is_redacted(self):
        got = login.url_shape(
            "https://trade.swissquote.ch/x/api/y?token=s3cret&cache=false")
        self.assertNotIn("s3cret", got)
        self.assertIn("cache=false", got)

    def test_short_hex_like_words_are_left_alone(self):
        # "feedback"/"added" are not long hex runs; eliding them would
        # destroy the very path the caller needs to read.
        got = login.url_shape("https://h/api/feedback/listen/abcdef")
        self.assertIn("feedback/listen", got)
        self.assertNotIn("<id>", got)


class SampleApiCallsTests(unittest.TestCase):
    class _Page:
        def __init__(self, result):
            self._result = result

        def evaluate(self, _script):
            if isinstance(self._result, Exception):
                raise self._result
            return self._result

    def test_returns_the_entries_as_given(self):
        self.assertEqual(
            login.sample_api_calls(self._Page([["a", 12]])), [["a", 12]])

    def test_a_page_mid_navigation_yields_nothing_rather_than_raising(self):
        # Diagnostics must never take down the login they are diagnosing.
        self.assertEqual(
            login.sample_api_calls(self._Page(RuntimeError("navigating"))), [])

    def test_a_null_result_is_tolerated(self):
        self.assertEqual(login.sample_api_calls(self._Page(None)), [])


class MergeApiSampleTests(unittest.TestCase):
    LONG = "https://h/api/thirdlevel/smartL3/check-challenge/0123456789abcdef"
    SHORT = "https://h/api/thirdlevel?urlId=0123456789abcdef"

    def test_counts_repeated_calls_to_one_shape(self):
        seen = login.merge_api_sample(
            {}, [[self.SHORT, 5], [self.SHORT, 7], [self.LONG, 20001]])
        short = seen[login.url_shape(self.SHORT)]
        self.assertEqual(short["calls"], 2)
        self.assertEqual(short["ms"], 7)

    def test_repeated_samples_do_not_multiply_the_same_entries(self):
        # The timing buffer is cumulative, so each sample re-reports every
        # earlier entry; summing across samples would inflate the counts.
        sample = [[self.SHORT, 5], [self.SHORT, 7]]
        seen = {}
        for _ in range(4):
            login.merge_api_sample(seen, sample)
        self.assertEqual(seen[login.url_shape(self.SHORT)]["calls"], 2)

    def test_a_growing_buffer_raises_the_count(self):
        seen = {}
        login.merge_api_sample(seen, [[self.SHORT, 5]])
        login.merge_api_sample(seen, [[self.SHORT, 5], [self.SHORT, 6]])
        self.assertEqual(seen[login.url_shape(self.SHORT)]["calls"], 2)

    def test_the_longest_duration_survives(self):
        seen = {}
        login.merge_api_sample(seen, [[self.LONG, 19998]])
        login.merge_api_sample(seen, [[self.LONG, 3]])
        self.assertEqual(seen[login.url_shape(self.LONG)]["ms"], 19998)

    def test_ids_are_elided_so_one_shape_aggregates_every_call(self):
        seen = login.merge_api_sample({}, [
            ["https://h/api/x/0123456789abcdef", 1],
            ["https://h/api/x/fedcba9876543210", 1],
        ])
        self.assertEqual(len(seen), 1)
        self.assertEqual(next(iter(seen.values()))["calls"], 2)

    def test_a_missing_duration_is_treated_as_zero(self):
        seen = login.merge_api_sample({}, [[self.SHORT, None]])
        self.assertEqual(seen[login.url_shape(self.SHORT)]["ms"], 0)


class ReportApiCallsTests(unittest.TestCase):
    def test_the_longest_held_call_is_reported_first(self):
        # That is the call the page is waiting on, which is the whole point
        # of the listing: it identifies the endpoint worth polling.
        import tempfile
        seen = login.merge_api_sample({}, [
            ["https://h/api/quick/0123456789abcdef", 8],
            ["https://h/api/held/fedcba9876543210", 20001],
        ])
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            login.report_api_calls(seen, out)
            lines = (out / "mfa_api_calls.txt").read_text().splitlines()
        self.assertIn("/api/held/", lines[0])
        self.assertIn("20001ms", lines[0])
        self.assertTrue(all("0123456789abcdef" not in ln for ln in lines))
        self.assertTrue(all("<id>" in ln for ln in lines))

    def test_nothing_observed_writes_no_file(self):
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            login.report_api_calls({}, out)
            self.assertFalse((out / "mfa_api_calls.txt").exists())

    def test_no_debug_dir_still_logs_without_raising(self):
        login.report_api_calls(
            login.merge_api_sample({}, [["https://h/api/a", 1]]), None)
