"""Tests for collectorkit.debugcap — the --debug gate's payload.

Two properties carry the weight:

  * a capture NEVER takes down the run it is diagnosing, and
  * a capture NEVER persists a credential.

The second is not hypothetical: fred's API key travels in the query string
(collectors/fred/CLAUDE.md §2), so an unredacted URL trace would write it to
disk — which repo CLAUDE.md §3 forbids outright. Account data in a capture is
fine; bronze is private and already full of it.
"""
from __future__ import annotations

import json
import logging
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from collectorkit import debugcap  # noqa: E402

log = logging.getLogger("test.debugcap")


class RedactUrlTest(unittest.TestCase):
    def test_masks_a_credential_param_but_keeps_its_name(self):
        # That the request carried an api_key is the diagnostic; the value is
        # the secret.
        out = debugcap.redact_url(
            "https://api.example.invalid/x?series_id=DEXSZUS&api_key=SECRET123")
        self.assertIn("series_id=DEXSZUS", out)
        self.assertIn("api_key=", out)
        self.assertNotIn("SECRET123", out)

    def test_masks_every_known_credential_spelling(self):
        for name in ("api_key", "apikey", "token", "access_token", "secret",
                     "client_secret", "password", "sig", "signature",
                     "authorization", "session"):
            out = debugcap.redact_url(f"https://x.invalid/a?{name}=LEAKME")
            self.assertNotIn("LEAKME", out, name)

    def test_is_case_insensitive(self):
        out = debugcap.redact_url("https://x.invalid/a?API_KEY=LEAKME")
        self.assertNotIn("LEAKME", out)

    def test_leaves_an_innocent_url_alone(self):
        url = "https://x.invalid/a/b?page=2&limit=100"
        self.assertEqual(debugcap.redact_url(url), url)

    def test_url_without_a_query_is_unchanged(self):
        url = "https://x.invalid/a/b"
        self.assertEqual(debugcap.redact_url(url), url)

    def test_unparseable_url_is_dropped_not_guessed_at(self):
        # A string we cannot parse is exactly where a naive mask would miss,
        # so drop it whole rather than write something we did not understand.
        self.assertEqual(debugcap.redact_url("http://[oops"), debugcap.REDACTED)


class HttpTraceTest(unittest.TestCase):
    def setUp(self):
        import tempfile
        self._tmp = tempfile.TemporaryDirectory()
        self.run = Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def _lines(self):
        p = self.run / debugcap.SCREENSHOTS_DIR / debugcap.HttpTrace.FILENAME
        return [json.loads(x) for x in p.read_text().splitlines()] if p.exists() else []

    def test_records_under_the_prune_reclaimed_subdir(self):
        # The dir name must match what every collector's prune nominates as
        # debug_subdirs, or captures would survive a prune forever.
        t = debugcap.HttpTrace(self.run, log=log)
        t.record("GET", "https://x.invalid/a", status=200, elapsed_ms=12.34,
                 bytes_=99)
        self.assertTrue(
            (self.run / debugcap.SCREENSHOTS_DIR / "http-trace.jsonl").exists())
        e = self._lines()[0]
        self.assertEqual((e["method"], e["status"], e["bytes"]), ("GET", 200, 99))

    def test_redacts_the_url_it_writes(self):
        t = debugcap.HttpTrace(self.run, log=log)
        t.record("GET", "https://x.invalid/a?api_key=SECRET123", status=200)
        self.assertNotIn("SECRET123", json.dumps(self._lines()))

    def test_keeps_only_whitelisted_response_headers(self):
        # A blocklist forgets exactly the header that matters; this is a
        # whitelist, so a Set-Cookie can never ride along.
        t = debugcap.HttpTrace(self.run, log=log)
        t.record("GET", "https://x.invalid/a", status=429, headers={
            "Content-Type": "application/json",
            "Retry-After": "30",
            "Set-Cookie": "session=SECRETCOOKIE",
            "Authorization": "Bearer SECRETTOKEN",
        })
        blob = json.dumps(self._lines())
        self.assertNotIn("SECRETCOOKIE", blob)
        self.assertNotIn("SECRETTOKEN", blob)
        h = self._lines()[0]["headers"]
        self.assertEqual(h.get("retry-after"), "30")      # the useful one
        self.assertEqual(h.get("content-type"), "application/json")

    def test_records_an_error_without_a_status(self):
        t = debugcap.HttpTrace(self.run, log=log)
        t.record("GET", "https://x.invalid/a", error="connection reset")
        e = self._lines()[0]
        self.assertEqual(e["error"], "connection reset")
        self.assertNotIn("status", e)

    def test_appends_rather_than_truncating(self):
        t = debugcap.HttpTrace(self.run, log=log)
        for i in range(3):
            t.record("GET", f"https://x.invalid/{i}", status=200)
        self.assertEqual(len(self._lines()), 3)

    def test_disabled_writes_nothing_and_still_accepts_calls(self):
        # Call sites record unconditionally; the gate lives here.
        t = debugcap.HttpTrace(self.run, log=log, enabled=False)
        t.record("GET", "https://x.invalid/a", status=200)
        self.assertFalse((self.run / debugcap.SCREENSHOTS_DIR).exists())

    def test_no_run_dir_is_a_clean_no_op(self):
        t = debugcap.HttpTrace(None, log=log)
        t.record("GET", "https://x.invalid/a", status=200)   # must not raise

    def test_a_write_failure_never_propagates(self):
        # The run must survive its own diagnostics.
        t = debugcap.HttpTrace(self.run, log=log)
        t.record("GET", "https://x.invalid/a", status=200)
        path = self.run / debugcap.SCREENSHOTS_DIR / "http-trace.jsonl"
        path.unlink()
        path.parent.chmod(0o500)          # read-only dir -> open() raises
        try:
            with self.assertLogs(log, level="WARNING"):
                t.record("GET", "https://x.invalid/b", status=200)
        finally:
            path.parent.chmod(0o700)


class CapturePageTest(unittest.TestCase):
    """The browser capture: two independent artefacts, neither fatal."""

    def setUp(self):
        import tempfile
        self._tmp = tempfile.TemporaryDirectory()
        self.run = Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    class _Page:
        def __init__(self, html="<html>ok</html>", content_raises=False,
                     shot_raises=False):
            self._html, self._c, self._s = html, content_raises, shot_raises
            self.shot_path = None

        def content(self):
            if self._c:
                raise RuntimeError("target closed")
            return self._html

        def screenshot(self, path=None, full_page=False):
            if self._s:
                raise RuntimeError("timeout")
            self.shot_path = path
            Path(path).write_bytes(b"\x89PNG")

    def test_writes_dom_and_screenshot(self):
        p = self._Page()
        debugcap.capture_page(p, self.run, "landing", log=log)
        d = self.run / debugcap.SCREENSHOTS_DIR
        self.assertEqual((d / "landing.html").read_text(), "<html>ok</html>")
        self.assertTrue((d / "landing.png").exists())

    def test_a_screenshot_failure_still_leaves_the_dom(self):
        # The DOM is what explains a selector that found nothing; it must not
        # be lost because a screenshot timed out on a busy page.
        p = self._Page(shot_raises=True)
        with self.assertLogs(log, level="WARNING"):
            debugcap.capture_page(p, self.run, "landing", log=log)
        self.assertTrue((self.run / debugcap.SCREENSHOTS_DIR / "landing.html").exists())

    def test_a_dom_failure_never_propagates(self):
        p = self._Page(content_raises=True)
        with self.assertLogs(log, level="WARNING"):
            debugcap.capture_page(p, self.run, "landing", log=log)   # no raise

    def test_png_can_be_skipped(self):
        p = self._Page()
        debugcap.capture_page(p, self.run, "landing", log=log, png=False)
        d = self.run / debugcap.SCREENSHOTS_DIR
        self.assertTrue((d / "landing.html").exists())
        self.assertFalse((d / "landing.png").exists())


if __name__ == "__main__":
    unittest.main()
