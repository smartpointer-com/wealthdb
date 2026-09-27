"""Tests for collectorkit.debugcap — the --debug gate's payload.

Two properties carry the weight:

  * a capture NEVER takes down the run it is diagnosing, and
  * a capture NEVER persists a credential.

The second is not hypothetical: fred's API key travels in the query string
(collectors/fred/AGENTS.md §2), so an unredacted URL trace would write it to
disk — which repo AGENTS.md §3 forbids outright. Account data in a capture is
fine; bronze is private and already full of it.
"""
from __future__ import annotations

import html
import json
import logging
import re
import sys
import unittest
from unittest import mock
import urllib.parse
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from collectorkit import debugcap  # noqa: E402

log = logging.getLogger("test.debugcap")

COLLECTORS = Path(__file__).resolve().parents[3] / "collectors"
# The shared half of every explore harness, scanned with the collectors'.
KIT_EXPLORE = Path(__file__).resolve().parents[1] / "collectorkit" / "explore.py"

# Stated as literals, never imported from the module: a name removed from
# debugcap._SECRET_PARAMS has to fail a test, and a test that reads the set
# it is checking would follow the removal instead of catching it.
KNOWN_PARAMS = (
    "api_key", "apikey", "key", "token", "access_token", "refresh_token",
    "id_token", "secret", "client_secret", "password", "passwd", "pwd",
    "sig", "signature", "auth", "authorization", "session", "sessionid",
)
# Credential-bearing header names that are NOT also query parameters.
HEADER_ONLY = (
    "cookie", "set-cookie", "x-api-key", "x-auth-token",
    "proxy-authorization", "x-csrf-token", "x-xsrf-token",
)


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
        for name in KNOWN_PARAMS:
            with self.subTest(name):
                out = debugcap.redact_url(f"https://x.invalid/a?{name}=LEAKME")
                self.assertNotIn("LEAKME", out)

    def test_the_credential_parameter_set_is_the_one_stated_here(self):
        # Pins both directions: a name dropped from the module fails the
        # loop above, and one added without a test here fails this.
        self.assertEqual(frozenset(KNOWN_PARAMS), debugcap._SECRET_PARAMS)

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
    # The space carries two spellings of its own — `+` from a form POST,
    # `%20` from encodeURIComponent — and both are exercised below.
    SECRET = "a$b^c%d e&f"
    USER = "example-user"

    def test_masks_the_raw_secret(self):
        redact = debugcap.secret_redactor(self.SECRET)
        self.assertEqual(redact(f"p={self.SECRET}"), f"p={debugcap.REDACTED}")

    def test_masks_the_form_urlencoded_secret(self):
        # The regression: a login POST body percent-encodes the password.
        # Both spellings of the space, because clients disagree: a form POST
        # sends `+`, encodeURIComponent sends `%20`.
        redact = debugcap.secret_redactor(self.SECRET, self.USER)
        for pw in ("a%24b%5Ec%25d+e%26f", "a%24b%5Ec%25d%20e%26f"):
            with self.subTest(pw):
                out = redact(f"request_type=login&UserID=example-user"
                             f"&Password={pw}&channel=Web")
                self.assertNotIn("a%24b", out)
                self.assertNotIn("example-user", out)
                self.assertIn("request_type=login", out)

    def test_masks_lower_case_percent_escapes(self):
        # Clients disagree on the case of the hex digits; both decode the
        # same, so both must be masked.
        redact = debugcap.secret_redactor(self.SECRET)
        for pw in ("a%24b%5ec%25d+e%26f", "a%24b%5ec%25d%20e%26f"):
            with self.subTest(pw):
                self.assertNotIn("a%24b", redact(f"Password={pw}"))

    def test_masks_the_json_escaped_secret(self):
        redact = debugcap.secret_redactor('pa"ss\\word')
        self.assertNotIn("pa", redact(json.dumps({"p": 'pa"ss\\word'})))

    def test_masks_a_non_ascii_secret_in_both_json_spellings(self):
        redact = debugcap.secret_redactor("pässwörd")
        self.assertNotIn("p\\u00e4", redact(json.dumps({"p": "pässwörd"})))
        self.assertNotIn(
            "pässwörd",
            redact(json.dumps({"p": "pässwörd"}, ensure_ascii=False)))

    def test_masks_the_unescaped_json_spelling_of_a_non_ascii_secret(self):
        # A secret with a quote AND a non-ASCII character is the case where
        # all three JSON spellings differ, so the ensure_ascii=False one is
        # not covered for free by the raw string.
        secret = 'p\u00e4"ss'
        redact = debugcap.secret_redactor(secret)
        out = redact(json.dumps({"p": secret}, ensure_ascii=False))
        self.assertNotIn("ss", out)
        self.assertIn(debugcap.REDACTED, out)

    def test_masks_the_percent_20_spelling(self):
        # encodeURIComponent spells a space `%20`, not `+`.
        redact = debugcap.secret_redactor(self.SECRET)
        body = "pw=" + urllib.parse.quote(self.SECRET, safe="")
        self.assertEqual(redact(body), "pw=" + debugcap.REDACTED)

    def test_masks_the_html_entity_spelling(self):
        # A serialized DOM is the artefact that always spells it this way.
        self.assertIn(html.escape(self.SECRET, quote=False),
                      debugcap.secret_variants(self.SECRET))

    def test_the_wire_spellings_are_the_ones_stated_here(self):
        # Literals, not a re-derivation: an encoder dropped from
        # secret_variants has to fail here rather than change both sides.
        self.assertEqual(set(debugcap.secret_variants(self.SECRET)), {
            "a$b^c%d e&f",                 # raw (and both JSON spellings)
            "a%24b%5Ec%25d+e%26f",         # form POST
            "a%24b%5ec%25d+e%26f",         # ...lower-case hex
            "a%24b%5Ec%25d%20e%26f",       # encodeURIComponent
            "a%24b%5ec%25d%20e%26f",       # ...lower-case hex
            "a$b^c%d e&amp;f",             # serialized DOM
        })
        self.assertEqual(set(debugcap.secret_variants('p\u00e4"s d')), {
            'p\u00e4"s d',
            "p%C3%A4%22s%20d", "p%c3%a4%22s%20d",
            "p%C3%A4%22s+d", "p%c3%a4%22s+d",
            'p\\u00e4\\"s d',              # json.dumps
            'p\u00e4\\"s d',               # json.dumps(ensure_ascii=False)
            'p\u00e4&quot;s d',            # DOM attribute value
        })

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

    def test_a_hyphenated_attribute_is_not_mistaken_for_the_value(self):
        # `data-value` is a selector hook, not the credential; blanking it
        # would cost the snapshot the very anchor it is kept for.
        out = debugcap.scrub_dom(
            '<input type="password" data-value="hook" value="pw">')
        self.assertIn('data-value="hook"', out)
        self.assertIn('value=""', out)
        self.assertNotIn('"pw"', out)

    # A serialized DOM entity-escapes what it carries, and a punctuated
    # secret is exactly the kind that changes shape on the way in.
    ENTITY_SECRET = 'a&b"c<d>e'

    def _entity_scrub(self, markup):
        return debugcap.scrub_dom(
            markup, debugcap.secret_redactor(self.ENTITY_SECRET))

    def test_masks_an_entity_escaped_attribute_value(self):
        # Two spellings, because serializers disagree on whether `<`/`>` are
        # escaped inside an attribute.
        for value in ('a&amp;b&quot;c<d>e', 'a&amp;b&quot;c&lt;d&gt;e'):
            with self.subTest(value):
                out = self._entity_scrub(
                    f'<input type="text" name="u" value="{value}">')
                self.assertNotIn("a&amp;b", out)
                self.assertIn(debugcap.REDACTED, out)

    def test_masks_an_entity_escaped_hidden_field(self):
        # type="hidden" is not type="password", so only the redactor covers
        # it — and the markup spells the `&` as an entity.
        out = self._entity_scrub(
            '<input type="hidden" name="u" value="a&amp;b&quot;c<d>e">')
        self.assertNotIn("a&amp;b", out)

    def test_masks_an_entity_escaped_text_node(self):
        # A text node escapes the angle brackets but never the quote.
        out = self._entity_scrub('<div>a&amp;b"c&lt;d&gt;e</div>')
        self.assertEqual(out, f"<div>{debugcap.REDACTED}</div>")


@unittest.skipUnless(COLLECTORS.is_dir(), "collectors/ not present")
class CollectorRedactionTest(unittest.TestCase):
    """Every collector masks credentials through the shared redactor.

    A security primitive inlined in one collector tends to be inlined in
    all of them, and a fix then reaches only the copy it was typed into.
    Credential masking is that kind of primitive: the literal-substring
    version each explore harness once carried wrote a percent-encoded
    password to a debug capture in cleartext. The harnesses' recording
    half now lives in collectorkit.explore, scanned here with them."""

    def _collector_sources(self):
        return sorted(COLLECTORS.glob("*/*.py")) + [KIT_EXPLORE]

    def test_no_collector_inlines_its_own_credential_masker(self):
        for path in self._collector_sources():
            text = path.read_text(encoding="utf-8")
            self.assertNotIn(
                "secrets_to_redact", text,
                f"{path} inlines a credential masker — use "
                f"collectorkit.debugcap.secret_redactor()")

    # Every way the fleet turns a live page into markup. Three earlier
    # versions of this guard keyed too narrowly and each left a real leak
    # invisible: the first on one harness\'s private function, which skipped
    # every login flow; the second on `login.py`/`explore.py`, which skipped
    # the one collector whose sign-in lives in its download.py; the third on
    # `.content()`, which a capture written with the `outerHTML` fallback —
    # the spelling three SPA collectors already use for a page whose
    # `content()` times out — would slip straight past.
    DOM_SERIALISERS = (".content()", "outerHTML", "inner_html(", "innerHTML")

    def test_every_dom_serialising_module_scrubs(self):
        # Keyed on the BEHAVIOUR — serialising a DOM — not on a file name.
        # Any module that can serialize a page can serialize a sign-in form,
        # and that markup carries the typed password.
        #
        # File-level, so it proves a module scrubs somewhere rather than at
        # every site. It catches the failure that actually happens — a
        # capture path with no scrubbing at all.
        scanned = 0
        for path in self._collector_sources():
            text = path.read_text(encoding="utf-8")
            if not any(k in text for k in self.DOM_SERIALISERS):
                continue
            scanned += 1
            self.assertIn(
                "debugcap.scrub_dom(", text,
                f"{path} serialises a DOM (content() / outerHTML / "
                f"innerHTML) without collectorkit.debugcap.scrub_dom() — a "
                f"sign-in form captured there carries the typed password")
        self.assertTrue(scanned, "no DOM-serialising module found to scan")

    def test_the_serialiser_set_reaches_past_content(self):
        # The scan above is only as wide as this tuple, and every file it
        # currently reaches happens to spell `.content()` somewhere too —
        # so narrowing the tuple back to that one spelling would leave the
        # scan green while the guard stopped covering anything. Drive the
        # predicate over synthetic text instead, where each spelling is on
        # its own and the narrowing is visible.
        for spelling in ("html = page.content()",
                         "html = page.evaluate("
                         "'document.documentElement.outerHTML')",
                         "html = frame.inner_html('body')",
                         "html = await el.innerHTML"):
            with self.subTest(spelling):
                self.assertTrue(
                    any(k in spelling for k in self.DOM_SERIALISERS),
                    "a DOM serialiser this set no longer recognises")
        self.assertFalse(
            any(k in "page.goto(url)" for k in self.DOM_SERIALISERS),
            "the set matches a line that serialises nothing")

    def test_every_har_recording_harness_redacts_it(self):
        # Playwright writes the HAR itself and writes it whole — the login
        # POST body as text AND as parsed params, every header, the cookie
        # jar. Nothing passed at record time narrows that, so a harness
        # that records one must also clean it after the close.
        #
        # No exemptions: the scan is over every harness that records a
        # HAR, and one harness's private scrubber is exactly the shape
        # this guard exists to prevent — a fix reaches only the copy it
        # was typed into, and the exemption that names the copy outlives
        # the reason for it.
        # A harness that records through explore.Session gets the scrub the
        # session queues at construction — the shared route, scanned here
        # with the rest.
        scanned = 0
        for path in sorted(COLLECTORS.glob("*/explore.py")) + [KIT_EXPLORE]:
            text = path.read_text(encoding="utf-8")
            if "record_har_path" not in text:
                continue
            scanned += 1
            self.assertTrue(
                "debugcap.redact_har(" in text
                or ("explore.Session" in text and path != KIT_EXPLORE),
                f"{path} records a HAR without "
                f"collectorkit.debugcap.redact_har() — the file holds the "
                f"login POST body and the cookie jar")
        self.assertTrue(scanned, "no HAR-recording harness found to scan")

    @staticmethod
    def _capture_page_calls(text):
        """Every `capture_page(...)` call in `text`, as (line, arguments).

        Parens are balanced rather than matched to the first `)`, so a
        call whose arguments contain one — a lambda, a nested call — is
        read whole instead of being cut in half.
        """
        for m in re.finditer(r"capture_page\(", text):
            depth, i = 1, m.end()
            while i < len(text) and depth:
                if text[i] == "(":
                    depth += 1
                elif text[i] == ")":
                    depth -= 1
                i += 1
            yield text.count("\n", 0, m.start()) + 1, text[m.end():i - 1]

    def test_every_capture_page_call_passes_a_redactor(self):
        # capture_page() blanks password inputs on its own, which covers
        # the credential a form is holding at the moment of capture and
        # nothing else. A credential the page carries anywhere else — a
        # hidden field, a bootstrap script, the session in a meta tag —
        # is only reached by the `redact` the call site passes, and a
        # site that passes none writes it to disk.
        #
        # Per CALL, not per file: the failure this catches is a harness
        # that routes some of its captures and forgets the rest, which a
        # file-level scan reads as clean.
        scanned = 0
        for path in self._collector_sources():
            for line, args in self._capture_page_calls(
                    path.read_text(encoding="utf-8")):
                scanned += 1
                self.assertIn(
                    "redact=", args,
                    f"{path}:{line} captures a page without a redactor — "
                    f"pass redact= (debugcap.secret_redactor for a run "
                    f"holding credentials, debugcap.session_redactor / "
                    f"SessionMask for one holding only a session)")
        self.assertTrue(scanned, "no capture_page call found to scan")

    def test_the_capture_page_scan_reads_a_call_whole(self):
        # The scan above is only as good as its paren matching, and a
        # matcher that stopped at the first `)` would read the call below
        # as ending inside the lambda and miss the redactor after it —
        # passing the guard while covering nothing. Drive it over
        # synthetic text, where the shape is visible.
        calls = list(self._capture_page_calls(
            "debugcap.capture_page(page, d, n, log=log,\n"
            "                      redact=lambda h: mask(blank(h)))\n"))
        self.assertEqual(len(calls), 1)
        self.assertIn("redact=", calls[0][1])
        self.assertNotIn(
            "redact=",
            list(self._capture_page_calls(
                "debugcap.capture_page(page, d, n, log=log)"))[0][1])

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

    def _capture(self, out_dir="default", redact=None):
        return debugcap.BodyCapture(
            self.out if out_dir == "default" else out_dir,
            host_markers=("sws-gateway",), log=log, redact=redact)

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

    def test_the_redactor_reaches_the_body_and_the_file_name(self):
        # The file is a JSON re-encoding of the body, so the redactor has to
        # run on the RAW text: a secret carrying a quote reaches the file
        # doubly escaped, in a spelling no variant knows. And the name is
        # built from the URL path, which the query-only redact_url never
        # touches.
        cap, ctx = self._capture(
            redact=debugcap.secret_redactor('EXAMPLEPW"X', "EXAMPLEID")), \
            _Context()
        cap.attach(ctx)
        ctx.emit(_Resp(
            "https://sws-gateway.example.invalid/session/EXAMPLEID/whoami",
            body=json.dumps({"pw": 'EXAMPLEPW"X', "id": "EXAMPLEID"})))
        self.assertEqual(cap.flush(), 1)
        written = next(self.out.glob("body-*.json"))
        self.assertNotIn("EXAMPLEID", written.name)
        blob = written.read_text()
        self.assertNotIn("EXAMPLEPW", blob)
        self.assertNotIn("EXAMPLEID", blob)

    def test_flush_drains_the_buffer(self):
        cap, ctx = self._capture(), _Context()
        cap.attach(ctx)
        ctx.emit(_Resp("https://sws-gateway.example.invalid/a"))
        self.assertEqual(cap.flush(), 1)
        self.assertEqual(cap.flush(), 0)   # nothing written twice


class RedactHarTest(unittest.TestCase):
    """Playwright writes the HAR itself, whole: the login POST as text AND
    as parsed params, every header, the cookie jar, the query string. It is
    JSON, so it is cleaned after the context close that produced it."""

    SECRET = "a$b^c%d e&f"

    def setUp(self):
        import tempfile
        self._tmp = tempfile.TemporaryDirectory()
        self.har = Path(self._tmp.name) / "network.har"

    def tearDown(self):
        self._tmp.cleanup()

    def _write(self, entry):
        self.har.write_text(json.dumps({"log": {"version": "1.2",
                                                "entries": [entry]}}),
                            encoding="utf-8")

    ENTRY = {
        "request": {
            "method": "POST",
            "url": "https://x.invalid/login?api_key=EXAMPLEAPIKEY&page=2",
            "queryString": [{"name": "api_key", "value": "EXAMPLEAPIKEY"},
                            {"name": "page", "value": "2"}],
            "headers": [{"name": "Cookie", "value": "sid=EXAMPLESESSION"},
                        {"name": "X-CSRF-Token", "value": "EXAMPLECSRF"},
                        {"name": "Accept", "value": "application/json"}],
            "cookies": [{"name": "sid", "value": "EXAMPLESESSION"}],
            "postData": {
                "mimeType": "application/x-www-form-urlencoded",
                "text": "user=example-user&password=a%24b%5Ec%25d+e%26f",
                "params": [{"name": "user", "value": "example-user"},
                           {"name": "password",
                            "value": "a%24b%5Ec%25d+e%26f"}],
            },
        },
        "response": {
            "status": 200,
            "headers": [{"name": "Set-Cookie",
                         "value": "sid=EXAMPLESESSION; Path=/"}],
            "cookies": [{"name": "sid", "value": "EXAMPLESESSION"}],
            "content": {"mimeType": "application/json",
                        "text": '{"who": "a$b^c%d e&f"}'},
        },
    }

    # A JSON / GraphQL sign-in — the normal shape for the SPAs these
    # harnesses target. Playwright fills postData.params for a form-encoded
    # body only, so this one arrives as `text` alone.
    JSON_ENTRY = {
        "request": {
            "method": "POST",
            "url": "https://x.invalid/graphql",
            "postData": {
                "mimeType": "application/json",
                "text": '{"op": "SignIn", "vars": {"user": "example-user", '
                        '"password": "EXAMPLEPASSWORD"}}',
            },
        },
        "response": {"status": 200},
    }

    # A password shorter than the echo pass's length floor.
    SHORT_PW_ENTRY = {
        "request": {
            "method": "POST",
            "url": "https://x.invalid/login",
            "postData": {
                "mimeType": "application/x-www-form-urlencoded",
                "text": "user=example-user&password=shortpw",
                "params": [{"name": "user", "value": "example-user"},
                           {"name": "password", "value": "shortpw"}],
            },
        },
        "response": {"status": 200},
    }

    def _redacted(self, redact=None, entry=None):
        import copy
        self._write(copy.deepcopy(self.ENTRY if entry is None else entry))
        self.assertTrue(debugcap.redact_har(self.har, redact, log=log))
        return self.har.read_text()

    def test_masks_the_credentials_it_was_given(self):
        blob = self._redacted(debugcap.secret_redactor(self.SECRET,
                                                       "example-user"))
        self.assertNotIn("a%24b", blob)       # the parsed param
        self.assertNotIn("a$b^c", blob)       # the echoed response body
        self.assertNotIn("example-user", blob)

    def test_masks_by_name_what_no_redactor_could_know(self):
        # The session the server minted, the key the SPA was issued: their
        # values are knowable only from the name they arrived under.
        blob = self._redacted()
        self.assertNotIn("EXAMPLESESSION", blob)
        self.assertNotIn("EXAMPLEAPIKEY", blob)
        self.assertNotIn("EXAMPLECSRF", blob)

    def test_masks_the_password_param_even_unknown(self):
        # The --no-prefill case again: nobody passed the password in, but
        # the parameter it sits in names it.
        self.assertNotIn("a%24b", self._redacted())

    def test_keeps_what_diagnoses(self):
        blob = self._redacted()
        self.assertIn("page=2", blob)
        self.assertIn("application/json", blob)
        self.assertIn("api_key", blob)        # that one rode along is the
        self.assertIn("Cookie", blob)         # diagnostic; the value is not

    def test_masks_a_json_body_password_nobody_passed_in(self):
        # Same --no-prefill case as above, in the shape a parsed-parameter
        # pass cannot see: the key the value nests under still names it.
        blob = self._redacted(entry=self.JSON_ENTRY)
        self.assertNotIn("EXAMPLEPASSWORD", blob)
        self.assertIn("SignIn", blob)      # what diagnoses survives
        self.assertIn("password", blob)    # the name is the diagnostic

    def test_a_short_named_password_is_masked_by_its_field_name(self):
        # Too short for the echo pass to chase, and it does not need to
        # be chased: the field it was posted in names it, so the body is
        # masked by name and the floor never comes into it.
        self.assertNotIn("shortpw", self._redacted(entry=self.SHORT_PW_ENTRY))

    def test_a_short_named_value_is_not_chased_through_the_bodies(self):
        # The regression the floor exists to stop. `key` is a credential
        # name, so its value is masked where it sits — but chasing a
        # one-character value through the payload beside it would blank
        # every matching character and leave a capture that reads as
        # redacted while showing nothing.
        entry = {
            "request": {
                "method": "GET",
                "url": "https://x.invalid/a?key=1&page=2",
                "queryString": [{"name": "key", "value": "1"},
                                {"name": "page", "value": "2"}],
            },
            "response": {"status": 200,
                         "content": {"mimeType": "application/json",
                                     "text": '{"rows": 1, "pages": 1}'}},
        }
        out = json.loads(self._redacted(entry=entry))["log"]["entries"][0]
        self.assertEqual(out["request"]["queryString"][0]["value"],
                         debugcap.REDACTED)
        self.assertEqual(out["response"]["content"]["text"],
                         '{"rows": 1, "pages": 1}')

    def test_a_short_cookie_value_is_not_chased_through_the_bodies(self):
        # The same floor from the other side: a one-character session
        # cookie must not blank every matching character in the bodies
        # beside it.
        entry = {
            "request": {"method": "GET", "url": "https://x.invalid/a",
                        "cookies": [{"name": "sid", "value": "1"}]},
            "response": {"status": 200,
                         "content": {"mimeType": "application/json",
                                     "text": '{"count": 1}'}},
        }
        out = json.loads(self._redacted(entry=entry))["log"]["entries"][0]
        self.assertEqual(out["request"]["cookies"][0]["value"],
                         debugcap.REDACTED)
        self.assertEqual(out["response"]["content"]["text"], '{"count": 1}')

    def test_a_form_body_the_recorder_did_not_parse_is_masked_by_name(self):
        # No `params` array, so the parsed pass has nothing to walk and
        # the echo pass has nothing to echo. The field name in the body
        # is the only thing left that identifies the value.
        entry = {
            "request": {
                "method": "POST", "url": "https://x.invalid/login",
                "postData": {
                    "mimeType": "application/x-www-form-urlencoded;"
                                " charset=UTF-8",
                    "text": "user=example-user&password=EXAMPLEPASSWORD"
                            "&locale=de",
                },
            },
            "response": {"status": 200},
        }
        blob = self._redacted(entry=entry)
        self.assertNotIn("EXAMPLEPASSWORD", blob)
        self.assertIn("password=", blob)     # the name is the diagnostic
        self.assertIn("locale=de", blob)     # an innocent field is untouched

    def test_a_base64_response_body_is_dropped_not_passed_through(self):
        # No value-based pass can see inside base64, so passing the body
        # through would leave it whole in a file that now reads as
        # redacted. That there was a body stays; the body goes.
        entry = {
            "request": {"method": "GET", "url": "https://x.invalid/a"},
            "response": {"status": 200,
                         "content": {"mimeType": "application/octet-stream",
                                     "encoding": "base64",
                                     "text": "RVhBTVBMRUJPRFk="}},
        }
        out = json.loads(self._redacted(entry=entry))["log"]["entries"][0]
        content = out["response"]["content"]
        self.assertNotIn("RVhBTVBMRUJPRFk=", content["text"])
        self.assertNotIn("encoding", content)
        self.assertEqual(content["mimeType"], "application/octet-stream")

    def test_a_credential_named_form_field_is_masked_anywhere_it_sits(self):
        # One name, three places. A CSRF token is a header on one call and
        # a form field on the next, so the name set that reaches it in a
        # header has to reach it in a body and a query string too.
        entry = {
            "request": {
                "method": "POST", "url": "https://x.invalid/login",
                "postData": {
                    "mimeType": "application/x-www-form-urlencoded",
                    "text": "x-csrf-token=EXAMPLECSRF&step=2",
                    "params": [{"name": "x-csrf-token",
                                "value": "EXAMPLECSRF"},
                               {"name": "step", "value": "2"}],
                },
            },
            "response": {"status": 200},
        }
        blob = self._redacted(entry=entry)
        self.assertNotIn("EXAMPLECSRF", blob)
        self.assertIn("step", blob)

    def test_an_absent_har_is_not_an_error(self):
        # The browser-closed path never flushes one.
        self.assertFalse(debugcap.redact_har(self.har, log=log))

    def test_an_unparseable_har_is_left_alone_with_a_warning(self):
        # And the warning says what the file now is. A line reading only
        # "redaction failed" invites the file being opened as if it were
        # clean; the credentials it recorded are still in it.
        self.har.write_text("{not json", encoding="utf-8")
        with self.assertLogs(log, level="WARNING") as caught:
            self.assertFalse(debugcap.redact_har(self.har, log=log))
        self.assertIn("still holds every credential it recorded",
                      "\n".join(caught.output))
        self.assertEqual(self.har.read_text(), "{not json")

    def test_a_har_without_entries_is_left_alone(self):
        self.har.write_text(json.dumps({"log": {"version": "1.2"}}),
                            encoding="utf-8")
        with self.assertLogs(log, level="WARNING"):
            self.assertFalse(debugcap.redact_har(self.har, log=log))

    def test_a_failed_rewrite_leaves_the_original_and_cleans_up(self):
        """The guarantee that makes an in-place rewrite safe: if the write
        or the replace fails part-way, the file that survives is the one
        that was already there — never a half-written mixture — and the
        .redacting temp does not outlive the attempt as a second copy of
        everything the HAR recorded."""
        original = json.dumps({"log": {"entries": [
            {"request": {"url": "https://example.test/in",
                         "headers": [{"name": "authorization",
                                      "value": "Bearer EXAMPLESECRET"}]},
             "response": {}}]}})
        self.har.write_text(original, encoding="utf-8")

        def _boom(src, dst):
            raise OSError("replace failed")

        with mock.patch.object(debugcap.os, "replace", _boom):
            with self.assertLogs(log, level="WARNING") as caught:
                self.assertFalse(debugcap.redact_har(self.har, log=log))

        self.assertIn("still holds every credential it recorded",
                      "\n".join(caught.output))
        self.assertEqual(self.har.read_text(encoding="utf-8"), original)
        self.assertFalse(self.har.with_name(self.har.name + ".redacting").exists())


class RedactBodyTest(unittest.TestCase):
    """The body-shaped counterpart to redact_headers: a body's own syntax
    names its values, which is the only thing that reaches a credential
    nobody passed in."""

    def test_masks_a_form_field_by_name(self):
        out = debugcap.redact_body(
            "user=example-user&password=EXAMPLEPASSWORD&locale=de",
            "application/x-www-form-urlencoded")
        self.assertNotIn("EXAMPLEPASSWORD", out)
        self.assertIn("locale=de", out)

    def test_masks_a_json_member_by_key_at_any_depth(self):
        out = debugcap.redact_body(
            json.dumps({"op": "SignIn",
                        "vars": {"user": "example-user",
                                 "password": "EXAMPLEPASSWORD"}}),
            "application/json")
        self.assertNotIn("EXAMPLEPASSWORD", out)
        self.assertIn("SignIn", out)

    def test_masks_the_echo_of_what_it_masked_by_name(self):
        """A body names its secret once and then spells it again somewhere the
        name pass cannot see — a JSON member beside a free-text field that
        quotes it back. The HAR path has always run that second pass; the
        body-only path computed the same set and threw it away."""
        out = debugcap.redact_body(
            json.dumps({"password": "EXAMPLESECRET",
                        "detail": "rejected value EXAMPLESECRET for user"}),
            "application/json")
        self.assertNotIn("EXAMPLESECRET", out)
        self.assertIn("rejected value", out)

    def test_the_echo_is_body_local(self):
        """Only values THIS body named are echoed, so one body's secret can
        never widen the mask applied to another's."""
        out = debugcap.redact_body(
            json.dumps({"note": "EXAMPLESECRET is not named here"}),
            "application/json")
        self.assertIn("EXAMPLESECRET", out)

    def test_applies_the_value_mask_as_well(self):
        out = debugcap.redact_body(
            "user=example-user&step=2", "application/x-www-form-urlencoded",
            debugcap.secret_redactor("example-user"))
        self.assertNotIn("example-user", out)
        self.assertIn("step=2", out)

    def test_a_body_in_neither_shape_gets_the_value_mask_alone(self):
        out = debugcap.redact_body("plain EXAMPLESECRET text", "text/plain",
                                   debugcap.secret_redactor("EXAMPLESECRET"))
        self.assertEqual(out, f"plain {debugcap.REDACTED} text")

    def test_a_json_body_that_does_not_parse_is_not_mangled(self):
        # Best effort: a body the mime type mislabels keeps its bytes.
        self.assertEqual(
            debugcap.redact_body("{not json", "application/json"),
            "{not json")

    def test_an_absent_body_passes_through(self):
        self.assertIsNone(debugcap.redact_body(None, "application/json"))
        self.assertEqual(debugcap.redact_body("", "application/json"), "")


class SessionRedactorTest(unittest.TestCase):
    """A download run is handed no password, only a lifted session — so
    the jar is the credential a capture could leak."""

    JAR = [{"name": "sid", "value": "EXAMPLESESSIONVALUE"},
           {"name": "lang", "value": "de"},
           {"name": "csrf", "value": "EXAMPLECSRFVALUE"}]

    def test_masks_every_session_length_cookie_whatever_it_is_called(self):
        redact = debugcap.session_redactor(self.JAR)
        out = redact('<meta name="csrf" content="EXAMPLECSRFVALUE">'
                     "EXAMPLESESSIONVALUE")
        self.assertNotIn("EXAMPLESESSIONVALUE", out)
        self.assertNotIn("EXAMPLECSRFVALUE", out)

    def test_a_short_cookie_value_is_left_alone(self):
        # `de` is a locale, not a session, and masking it would blank the
        # letters out of the markup the capture exists to show.
        redact = debugcap.session_redactor(self.JAR)
        self.assertEqual(redact("<html lang=de>order</html>"),
                         "<html lang=de>order</html>")

    def test_an_extra_credential_is_masked_however_short(self):
        # Not a guess: the caller named it.
        redact = debugcap.session_redactor([], "pw")
        self.assertEqual(redact("pw"), debugcap.REDACTED)

    def test_an_empty_jar_is_the_identity(self):
        self.assertEqual(debugcap.session_redactor([])("anything"), "anything")
        self.assertEqual(debugcap.session_redactor(None)("anything"),
                         "anything")

    def test_a_malformed_jar_entry_is_skipped(self):
        redact = debugcap.session_redactor(
            ["not-a-dict", {"name": "sid"}, {"value": None}])
        self.assertEqual(redact("anything"), "anything")


class SessionMaskTest(unittest.TestCase):
    """The persistent-profile case: the jar belongs to the browser, so it
    is read at the first capture rather than at wiring time."""

    class _Page:
        def __init__(self, cookies, fail=False):
            self.reads = 0
            outer = self

            class _Context:
                def cookies(self_ctx):
                    outer.reads += 1
                    if fail:
                        raise RuntimeError("context gone")
                    return cookies

            self.context = _Context()

    def test_reads_the_jar_once_and_reuses_the_mask(self):
        page = self._Page([{"name": "sid", "value": "EXAMPLESESSIONVALUE"}])
        mask = debugcap.SessionMask()
        first = mask.for_page(page)
        self.assertNotIn("EXAMPLESESSIONVALUE", first("EXAMPLESESSIONVALUE"))
        self.assertIs(mask.for_page(page), first)
        self.assertEqual(page.reads, 1)

    def test_an_unreadable_jar_still_yields_a_mask(self):
        # A capture that fails is worse than one masked only by what the
        # caller named, so the jar read never propagates.
        mask = debugcap.SessionMask("EXAMPLEEXTRA")
        redact = mask.for_page(self._Page([], fail=True))
        self.assertEqual(redact("EXAMPLEEXTRA"), debugcap.REDACTED)


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

    def test_masks_every_known_credential_header(self):
        for name in KNOWN_PARAMS + HEADER_ONLY:
            with self.subTest(name):
                self.assertEqual(debugcap.redact_headers({name: "LEAKME"}),
                                 {name: debugcap.REDACTED})

    def test_the_credential_header_set_is_the_one_stated_here(self):
        self.assertEqual(frozenset(KNOWN_PARAMS) | frozenset(HEADER_ONLY),
                         debugcap._SECRET_HEADERS)

    def test_empty_and_unmappable_inputs_are_safe(self):
        self.assertEqual(debugcap.redact_headers(None), {})
        self.assertEqual(debugcap.redact_headers({}), {})
        self.assertEqual(debugcap.redact_headers("not a mapping"), {})


class TeeDebugLogTest(unittest.TestCase):
    def test_the_run_log_lands_beside_the_captures_at_debug(self):
        import tempfile
        root = logging.getLogger()
        level, levels = root.level, [(h, h.level) for h in root.handlers]
        with tempfile.TemporaryDirectory() as tmp:
            try:
                handler = debugcap.tee_debug_log(Path(tmp) / "debug",
                                                 logging.INFO, log=log)
                logging.getLogger("test.tee").debug("a debug line")
                handler.flush()
                (run_log,) = (Path(tmp) / "debug").glob("*-run.log")
                self.assertIn("a debug line", run_log.read_text())
            finally:
                root.removeHandler(handler)
                handler.close()
                root.setLevel(level)
                for h, lvl in levels:
                    h.setLevel(lvl)

    def test_no_dir_means_no_log(self):
        self.assertIsNone(debugcap.tee_debug_log(None, logging.INFO, log=log))

    def test_an_unwritable_dir_warns_and_the_run_goes_on(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            blocked = Path(tmp) / "blocked"
            blocked.mkdir()
            blocked.chmod(0o500)
            try:
                with self.assertLogs(log, level="WARNING") as seen:
                    self.assertIsNone(debugcap.tee_debug_log(
                        blocked / "sub", logging.INFO, log=log))
                self.assertIn("debug log", seen.output[0])
            finally:
                blocked.chmod(0o700)



if __name__ == "__main__":
    unittest.main()
