"""
The MFA page's API-call report.

login.py logs what the wait page requested, and how long each call was held,
through the shared `collectorkit.debugcap` recorder, because the page's
approval channel is its own status endpoint, whose route can move. Recording
the page's own calls — id-masked, so a line can be pasted into a report, and
ordered by duration, because a held call is the one worth polling — makes
such a move diagnosable instead of guessable. collectorkit's own suite
covers the recorder; these tests cover login.py's report.

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
from collectorkit import debugcap  # noqa: E402


class ReportApiCallsTests(unittest.TestCase):
    def test_the_longest_held_call_is_reported_first(self):
        # That is the call the page is waiting on, which is the whole point
        # of the listing: it identifies the endpoint worth polling.
        import tempfile
        seen = debugcap.merge_api_sample({}, [
            ["https://h/api/quick/0123456789abcdef", 8],
            ["https://h/api/held/fedcba9876543210", 20001],
        ])
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            login.report_mfa_api_calls(seen, out)
            lines = (out / "mfa_api_calls.txt").read_text().splitlines()
        self.assertIn("/api/held/", lines[0])
        self.assertIn("20001ms", lines[0])
        self.assertTrue(all("0123456789abcdef" not in ln for ln in lines))
        self.assertTrue(all("<id>" in ln for ln in lines))

    def test_nothing_observed_writes_no_file(self):
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            login.report_mfa_api_calls({}, out)
            self.assertFalse((out / "mfa_api_calls.txt").exists())

    def test_no_debug_dir_still_logs_without_raising(self):
        login.report_mfa_api_calls(
            debugcap.merge_api_sample({}, [["https://h/api/a", 1]]), None)
