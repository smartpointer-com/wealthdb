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

COLLECTORS = Path(__file__).resolve().parents[3] / "collectors"


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


class SecretRedactorTest(unittest.TestCase):
    """A credential reaches the wire encoded, so the redactor has to know
    every spelling. A literal-substring masker once let a percent-encoded
    password through into a debug capture in cleartext."""

    # Punctuation-heavy on purpose: every one of these characters changes
    # under form-urlencoding, which is what defeats a raw-string masker.
    SECRET = "a$b^c%d e&f"
    USER = "example-user"

    def test_masks_the_raw_secret(self):
        redact = debugcap.secret_redactor(self.SECRET)
        self.assertEqual(redact(f"p={self.SECRET}"), f"p={debugcap.REDACTED}")

    def test_masks_the_form_urlencoded_secret(self):
        # The regression: a login POST body percent-encodes the password.
        redact = debugcap.secret_redactor(self.SECRET, self.USER)
        body = ("request_type=login&UserID=example-user"
                "&Password=a%24b%5Ec%25d+e%26f&channel=Web")
        out = redact(body)
        self.assertNotIn("a%24b", out)
        self.assertNotIn("example-user", out)
        self.assertIn("request_type=login", out)

    def test_masks_lower_case_percent_escapes(self):
        # Clients disagree on the case of the hex digits; both decode the
        # same, so both must be masked.
        redact = debugcap.secret_redactor(self.SECRET)
        self.assertNotIn("a%24b", redact("Password=a%24b%5ec%25d+e%26f"))

    def test_masks_the_json_escaped_secret(self):
        redact = debugcap.secret_redactor('pa"ss\\word')
        self.assertNotIn("pa", redact(json.dumps({"p": 'pa"ss\\word'})))

    def test_masks_a_non_ascii_secret_in_both_json_spellings(self):
        redact = debugcap.secret_redactor("pässwörd")
        self.assertNotIn("p\\u00e4", redact(json.dumps({"p": "pässwörd"})))
        self.assertNotIn(
            "pässwörd",
            redact(json.dumps({"p": "pässwörd"}, ensure_ascii=False)))

    def test_no_secrets_is_the_identity(self):
        redact = debugcap.secret_redactor("", None)
        self.assertEqual(redact("nothing to hide"), "nothing to hide")

    def test_falsy_values_pass_through(self):
        redact = debugcap.secret_redactor(self.SECRET)
        self.assertIsNone(redact(None))
        self.assertEqual(redact(""), "")

    def test_variants_are_longest_first(self):
        # A longer spelling must be masked before a shorter one can match
        # inside it and leave a mangled remainder behind.
        variants = debugcap.secret_variants(self.SECRET)
        self.assertEqual(list(variants), sorted(variants, key=len,
                                                reverse=True))

    def test_variants_of_an_empty_secret_are_none(self):
        self.assertEqual(debugcap.secret_variants(""), ())

    def test_alphanumeric_secret_has_one_spelling(self):
        # Nothing to encode, so the spellings coincide and dedupe to one.
        self.assertEqual(debugcap.secret_variants("abc123"), ("abc123",))


class ScrubDomTest(unittest.TestCase):
    """A serialized login form can carry the typed password as a `value`
    attribute — a leak the network redactor never sees."""

    PW_INPUT = ('<input id="p" type="password" name="pw" '
                'value="s3cr3t!" class="x">')

    def test_blanks_a_password_inputs_value(self):
        out = debugcap.scrub_dom(self.PW_INPUT)
        self.assertIn('value=""', out)
        self.assertNotIn("s3cr3t", out)

    def test_blanks_without_knowing_the_credential(self):
        # The --no-prefill case: the human typed it, so no value-based
        # redactor could ever cover this.
        self.assertNotIn("typed-by-hand", debugcap.scrub_dom(
            '<input type="password" value="typed-by-hand">'))

    def test_keeps_other_inputs_intact(self):
        out = debugcap.scrub_dom('<input type="text" value="keep-me">')
        self.assertIn("keep-me", out)

    def test_handles_single_quoted_attributes(self):
        self.assertNotIn("s3cr3t", debugcap.scrub_dom(
            "<input type='password' value='s3cr3t'>"))

    def test_keeps_the_rest_of_the_tag(self):
        # The snapshot is what selectors are pinned from, so only the value
        # is rewritten.
        out = debugcap.scrub_dom(self.PW_INPUT)
        self.assertIn('id="p"', out)
        self.assertIn('name="pw"', out)
        self.assertIn('class="x"', out)

    def test_applies_the_redactor_to_the_rest_of_the_markup(self):
        redact = debugcap.secret_redactor("hunted")
        out = debugcap.scrub_dom('<div data-u="hunted">hunted</div>', redact)
        self.assertNotIn("hunted", out)

    def test_empty_markup_passes_through(self):
        self.assertEqual(debugcap.scrub_dom(""), "")

    def test_a_password_containing_an_angle_bracket_is_still_blanked(self):
        # `>` inside the value closes the tag early for a naive matcher, and
        # a punctuation-heavy password is exactly the kind worth protecting.
        out = debugcap.scrub_dom('<input type="password" value="a>b!" name=p>')
        self.assertNotIn("a>b!", out)
        self.assertIn('value=""', out)

    def test_unquoted_attributes_are_blanked_too(self):
        out = debugcap.scrub_dom("<input type=password value=p@ss>")
        self.assertNotIn("p@ss", out)

    def test_a_non_password_input_keeps_its_value(self):
        # The snapshot is what selectors are pinned from; only the credential
        # is removed.
        markup = '<input type="text" name="userid" value="keepme">'
        self.assertEqual(debugcap.scrub_dom(markup), markup)


@unittest.skipUnless(COLLECTORS.is_dir(), "collectors/ not present")
class CollectorRedactionTest(unittest.TestCase):
    """Every collector masks credentials through the shared redactor.

    The explore harnesses are copy-adapted per source by design, so a
    security primitive inlined in one of them is inlined in all of them —
    and a fix reaches only the copy it was typed into. Credential masking
    is that kind of primitive: the literal-substring version each harness
    once carried wrote a percent-encoded password to a debug capture in
    cleartext."""

    def _collector_sources(self):
        return sorted(COLLECTORS.glob("*/*.py"))

    def test_no_collector_inlines_its_own_credential_masker(self):
        for path in self._collector_sources():
            text = path.read_text(encoding="utf-8")
            self.assertNotIn(
                "secrets_to_redact", text,
                f"{path} inlines a credential masker — use "
                f"collectorkit.debugcap.secret_redactor()")

    def test_every_dom_serialising_module_scrubs(self):
        # Keyed on the BEHAVIOUR — serialising a DOM — not on a file name.
        # Two earlier versions of this guard keyed on names and each missed
        # a real leak: the first on one harness's private function, which
        # skipped every login flow; the second on `login.py`/`explore.py`,
        # which skipped the one collector whose sign-in lives in its
        # download.py. Any module that can serialize a page can serialize a
        # sign-in form, and that markup carries the typed password.
        #
        # File-level, so it proves a module scrubs somewhere rather than at
        # every site. It catches the failure that actually happens — a
        # capture path with no scrubbing at all.
        scanned = 0
        for path in self._collector_sources():
            text = path.read_text(encoding="utf-8")
            if ".content()" not in text:
                continue
            scanned += 1
            self.assertIn(
                "debugcap.scrub_dom(", text,
                f"{path} serialises a DOM without "
                f"collectorkit.debugcap.scrub_dom() — a sign-in form "
                f"captured there carries the typed password")
        self.assertTrue(scanned, "no DOM-serialising module found to scan")

    def test_every_explore_harness_calls_the_shared_redactor(self):
        # Guards the guards above: they scan for an absent anti-pattern, so a
        # scan reaching nothing would pass vacuously. Stated as the invariant
        # every harness must satisfy rather than as a collector count, it
        # holds at any point in history and cannot rot as sources are added.
        harnesses = sorted(COLLECTORS.glob("*/explore.py"))
        self.assertTrue(harnesses, "no explore harness found to scan")
        for path in harnesses:
            self.assertIn(
                "debugcap.secret_redactor(", path.read_text(encoding="utf-8"),
                f"{path} does not route its captures through "
                f"collectorkit.debugcap.secret_redactor()")


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

    def test_a_captured_password_field_is_scrubbed(self):
        # A --debug capture of a sign-in page must not persist the typed
        # credential, whether or not the caller knows what it is.
        p = self._Page('<input type="password" value="s3cr3t">')
        debugcap.capture_page(p, self.run, "signin", log=log)
        out = (self.run / debugcap.SCREENSHOTS_DIR / "signin.html").read_text()
        self.assertNotIn("s3cr3t", out)

    def test_a_redactor_masks_known_credentials_too(self):
        p = self._Page('<div>example-user</div>')
        debugcap.capture_page(p, self.run, "signin", log=log,
                              redact=debugcap.secret_redactor("example-user"))
        out = (self.run / debugcap.SCREENSHOTS_DIR / "signin.html").read_text()
        self.assertNotIn("example-user", out)


class _Resp:
    """The slice of a Playwright Response BodyCapture touches."""

    def __init__(self, url, status=200, ctype="application/json",
                 body='{"ok": true}', text_raises=False):
        self.url = url
        self.status = status
        self.headers = {"content-type": ctype}
        self._body = body
        self._raises = text_raises

    def text(self):
        if self._raises:
            raise RuntimeError("body evicted")
        return self._body


class _Context:
    def __init__(self):
        self.handlers = {}

    def on(self, event, handler):
        self.handlers[event] = handler

    def emit(self, resp):
        self.handlers["response"](resp)


class BodyCaptureTest(unittest.TestCase):
    def setUp(self):
        import tempfile
        self._tmp = tempfile.TemporaryDirectory()
        self.out = Path(self._tmp.name) / "captures"

    def tearDown(self):
        self._tmp.cleanup()

    def _capture(self, out_dir="default"):
        return debugcap.BodyCapture(
            self.out if out_dir == "default" else out_dir,
            host_markers=("sws-gateway",), log=log)

    def test_records_only_matching_hosts_and_flushes_bodies(self):
        cap, ctx = self._capture(), _Context()
        cap.attach(ctx)
        ctx.emit(_Resp("https://sws-gateway.example.invalid/api/v1/notice"))
        ctx.emit(_Resp("https://cdn.example.invalid/app.js"))
        self.assertEqual(cap.flush(), 1)
        files = list(self.out.glob("body-*.json"))
        self.assertEqual(len(files), 1)
        blob = json.loads(files[0].read_text())
        self.assertEqual(blob["status"], 200)
        self.assertEqual(blob["body"], '{"ok": true}')

    def test_redacts_the_url_it_writes(self):
        cap, ctx = self._capture(), _Context()
        cap.attach(ctx)
        ctx.emit(_Resp(
            "https://sws-gateway.example.invalid/a?session=SECRET123"))
        cap.flush()
        blob = next(self.out.glob("body-*.json")).read_text()
        self.assertNotIn("SECRET123", blob)

    def test_non_text_bodies_are_skipped(self):
        cap, ctx = self._capture(), _Context()
        cap.attach(ctx)
        ctx.emit(_Resp("https://sws-gateway.example.invalid/logo",
                       ctype="image/png"))
        self.assertEqual(cap.flush(), 0)

    def test_a_gone_body_is_skipped_not_fatal(self):
        cap, ctx = self._capture(), _Context()
        cap.attach(ctx)
        ctx.emit(_Resp("https://sws-gateway.example.invalid/a",
                       text_raises=True))
        ctx.emit(_Resp("https://sws-gateway.example.invalid/b"))
        self.assertEqual(cap.flush(), 1)

    def test_disabled_attaches_and_flushes_as_a_no_op(self):
        cap, ctx = self._capture(out_dir=None), _Context()
        cap.attach(ctx)
        self.assertNotIn("response", ctx.handlers)   # no listener at all
        self.assertEqual(cap.flush(), 0)

    def test_flush_drains_the_buffer(self):
        cap, ctx = self._capture(), _Context()
        cap.attach(ctx)
        ctx.emit(_Resp("https://sws-gateway.example.invalid/a"))
        self.assertEqual(cap.flush(), 1)
        self.assertEqual(cap.flush(), 0)   # nothing written twice


if __name__ == "__main__":
    unittest.main()


class SafeErrorTest(unittest.TestCase):
    """A transport client's exception carries the whole request in its
    text. Anything that persists it persists a credential."""

    def test_keeps_the_diagnostic_and_drops_the_call_log(self):
        exc = RuntimeError(
            "APIRequestContext.get: Timeout 60000ms exceeded.\n"
            "Call log:\n"
            "  - -> GET https://bank.test/api/x\n"
            "    - apikey: SECRETKEY\n"
            "    - cookie: session=SECRETSESSION")
        out = debugcap.safe_error(exc)
        self.assertIn("Timeout 60000ms exceeded", out)
        self.assertIn("RuntimeError", out)
        self.assertNotIn("SECRETKEY", out)
        self.assertNotIn("SECRETSESSION", out)
        self.assertNotIn("Call log", out)

    def test_masks_a_secret_carried_in_a_url(self):
        out = debugcap.safe_error(
            ValueError("failed: https://bank.test/f?apikey=SECRETKEY&x=1"))
        self.assertNotIn("SECRETKEY", out)
        self.assertIn("x=1", out)

    def test_a_call_log_on_the_first_line_is_still_cut(self):
        out = debugcap.safe_error(
            RuntimeError("boom Call log: - apikey: SECRETKEY"))
        self.assertNotIn("SECRETKEY", out)

    def test_an_empty_message_still_names_the_class(self):
        self.assertEqual(debugcap.safe_error(TimeoutError()), "TimeoutError")


class RedactHeadersTest(unittest.TestCase):
    """The complement of redact_url: a credential whose value is only
    knowable from the header it arrived in."""

    def test_masks_by_name_keeping_the_name(self):
        out = debugcap.redact_headers({
            "apikey": "SECRET", "Cookie": "session=SECRET",
            "authorization": "Bearer SECRET", "accept": "application/json"})
        self.assertEqual(out["apikey"], debugcap.REDACTED)
        self.assertEqual(out["Cookie"], debugcap.REDACTED)
        self.assertEqual(out["authorization"], debugcap.REDACTED)
        self.assertEqual(out["accept"], "application/json")

    def test_empty_and_unmappable_inputs_are_safe(self):
        self.assertEqual(debugcap.redact_headers(None), {})
        self.assertEqual(debugcap.redact_headers({}), {})
        self.assertEqual(debugcap.redact_headers("not a mapping"), {})
