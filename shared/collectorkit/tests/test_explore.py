"""Tests for collectorkit.explore — the recording side of every collector's
discovery harness, driven against stub browser objects.

What is pinned is what reaches disk: that the two logs, the HAR and the DOM
snapshots carry no credential, that the HAR is scrubbed on every unwind, that
downloads cannot collide or escape the downloads dir, and that a session
starts and stops with its lifecycle recorded. Synthetic values only.
"""
from __future__ import annotations

import argparse
import contextlib
import io
import json
import logging
import re
import signal
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from collectorkit import debugcap, explore  # noqa: E402

log = logging.getLogger("test.explore")

USERNAME = "example-user"
PASSWORD = "p@ss+word"


class _Request:
    def __init__(self, *, method="POST", url="https://www.example.com/logon",
                 headers=None, post_data=None, resource_type="xhr"):
        self.method = method
        self.url = url
        self.headers = headers or {}
        self.post_data = post_data
        self.resource_type = resource_type


class _Response:
    def __init__(self, body: bytes, *, ctype="application/json",
                 url="https://www.example.com/api"):
        self.url = url
        self.status = 200
        self.headers = {"content-type": ctype}
        self.request = _Request(method="GET", url=url)
        self._body = body

    def body(self):
        return self._body


class _Frame:
    def __init__(self, url, html):
        self.url = url
        self._html = html

    def content(self):
        return self._html


class _Page:
    def __init__(self, url="https://www.example.com/", frames=()):
        self.url = url
        self.frames = list(frames)
        self.main_frame = self.frames[0] if self.frames else None
        self.handlers = {}

    def on(self, event, handler):
        self.handlers.setdefault(event, []).append(handler)

    def goto(self, *args, **kwargs):
        pass

    def screenshot(self, **kwargs):
        pass


class _Context:
    def __init__(self, pages=()):
        self.pages = list(pages)
        self.handlers = {}
        self.init_scripts = []

    def on(self, event, handler):
        self.handlers[event] = handler

    def add_init_script(self, script):
        self.init_scripts.append(script)

    def new_page(self):
        page = _Page()
        self.pages.append(page)
        self.handlers["page"](page)
        return page


class _Download:
    url = "https://www.example.com/file"

    def __init__(self, suggested):
        self.suggested_filename = suggested
        self.saved = None

    def save_as(self, path):
        self.saved = path
        Path(path).write_bytes(b"x")


def _playwright_stub():
    """A `playwright.sync_api` holding just the timeout type the wait loop
    catches; collectorkit's own venv does not install Playwright."""
    sync_api = types.ModuleType("playwright.sync_api")

    class TimeoutError(Exception):  # noqa: A001 — mirrors Playwright's name
        pass
    sync_api.TimeoutError = TimeoutError
    root = types.ModuleType("playwright")
    root.sync_api = sync_api
    return {"playwright": root, "playwright.sync_api": sync_api}


class _Tmp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        # attach() installs a SIGTERM handler; put the runner's back.
        self._sigterm = signal.getsignal(signal.SIGTERM)

    def tearDown(self):
        signal.signal(signal.SIGTERM, self._sigterm)
        self._tmp.cleanup()


class HelpersTest(_Tmp):
    def test_skeleton_ignores_text_and_attribute_values(self):
        a = explore.dom_skeleton('<div id="opt"><span class="a">Get a text</span></div>')
        b = explore.dom_skeleton('<div id="opt"><span class="b">Confirm 12:03</span></div>')
        self.assertEqual(a, b)

    def test_skeleton_tracks_structure_and_ids(self):
        self.assertNotEqual(explore.dom_skeleton('<div id="cards"></div>'),
                            explore.dom_skeleton('<div id="accounts"></div>'))
        self.assertNotEqual(explore.dom_skeleton("<div><span></span></div>"),
                            explore.dom_skeleton("<div><span></span><span></span></div>"))

    def test_download_names_are_sequenced_so_repeats_do_not_collide(self):
        first = explore.safe_download_name("statement.pdf", 1)
        second = explore.safe_download_name("statement.pdf", 2)
        self.assertNotEqual(first, second)
        self.assertTrue(first.endswith("statement.pdf"))

    def test_download_name_cannot_escape_the_downloads_dir(self):
        name = explore.safe_download_name("../../etc/passwd", 3)
        self.assertNotIn("/", name)
        self.assertNotIn("..", name)

    def test_download_name_survives_a_missing_or_long_suggestion(self):
        self.assertTrue(explore.safe_download_name(None, 7).startswith("07-"))
        self.assertTrue(explore.safe_download_name("", 8).startswith("08-"))
        self.assertLess(len(explore.safe_download_name("x" * 500, 1)), 140)


class ArgsTest(unittest.TestCase):
    def _parse(self, argv, **kwargs):
        p = argparse.ArgumentParser()
        explore.add_args(p, url="https://www.example.com/",
                         env_file=Path("/secrets/example.env"), **kwargs)
        return p.parse_args(argv)

    def test_defaults(self):
        args = self._parse([], profile_dir=Path("/secrets/example-profile"),
                           dom_snapshots=True)
        self.assertEqual(args.url, "https://www.example.com/")
        self.assertEqual(args.profile_dir, Path("/secrets/example-profile"))
        self.assertEqual(args.env_file, Path("/secrets/example.env"))
        self.assertIsNone(args.debug_dir)       # a stamped /debug subdir
        self.assertEqual(args.max_duration, 3600)
        self.assertEqual(args.chunk_interval, 30)
        self.assertEqual(args.dom_interval, 3)
        self.assertFalse(args.trace or args.no_prefill or args.fresh
                         or args.verbose)

    def test_profile_and_dom_flags_are_opt_in(self):
        args = self._parse([])
        self.assertFalse(hasattr(args, "profile_dir"))
        self.assertFalse(hasattr(args, "fresh"))
        self.assertFalse(hasattr(args, "dom_interval"))

    def test_no_password_flag_exists(self):
        # Credentials reach a harness via env only (root AGENTS.md §3).
        with self.assertRaises(SystemExit), \
                contextlib.redirect_stderr(io.StringIO()):
            self._parse(["--password", "x"])


class CredentialsTest(_Tmp):
    def test_the_first_set_name_wins_and_enables_the_fill(self):
        env = {"EXAMPLE_EMAIL": USERNAME, "EXAMPLE_PASSWORD": PASSWORD}
        with mock.patch.dict("os.environ", env, clear=True):
            got = explore.load_credentials(
                self.root / "absent.env", ("EXAMPLE_USERNAME", "EXAMPLE_EMAIL"),
                ("EXAMPLE_PASSWORD",), no_prefill=False, host="example.com",
                log=log)
        self.assertEqual(got, (USERNAME, PASSWORD, True))

    def test_a_missing_credential_disables_the_fill(self):
        with mock.patch.dict("os.environ", {"EXAMPLE_USERNAME": USERNAME},
                             clear=True):
            got = explore.load_credentials(
                self.root / "absent.env", ("EXAMPLE_USERNAME",),
                ("EXAMPLE_PASSWORD",), no_prefill=False, host="example.com",
                log=log)
        self.assertFalse(got[2])

    def test_no_prefill_refuses_credentials_that_are_there(self):
        env = {"EXAMPLE_USERNAME": USERNAME, "EXAMPLE_PASSWORD": PASSWORD}
        with mock.patch.dict("os.environ", env, clear=True):
            got = explore.load_credentials(
                None, ("EXAMPLE_USERNAME",), ("EXAMPLE_PASSWORD",),
                no_prefill=True, host="example.com", log=log)
        self.assertFalse(got[2])

    def test_fresh_wipes_the_profile(self):
        profile = self.root / "profile"
        profile.mkdir()
        (profile / "cookies.sqlite").write_bytes(b"session")
        with mock.patch.object(explore.launch, "prepare_profile_dir") as prep:
            explore.prepare_profile(profile, fresh=True, note="n", log=log)
        self.assertFalse((profile / "cookies.sqlite").exists())
        prep.assert_called_once_with(profile)


class SessionTest(_Tmp):
    def _session(self, stack, *, redact=None, **kwargs):
        return explore.Session(
            stack, self.root / "debug",
            redact=redact or debugcap.secret_redactor(USERNAME, PASSWORD),
            log=log, **kwargs)

    def _network(self):
        return (self.root / "debug" / "network.jsonl").read_text()

    def _events(self):
        text = (self.root / "debug" / "clicks.jsonl").read_text()
        return [json.loads(line) for line in text.splitlines()]

    def test_the_network_log_masks_a_password_field_it_never_saw(self):
        # The --no-prefill run: the credential was typed by hand, so no
        # value-based mask can reach it. The field NAME still can.
        with contextlib.ExitStack() as stack:
            s = self._session(stack, redact=debugcap.secret_redactor())
            s._on_request(_Request(
                headers={"content-type": "application/x-www-form-urlencoded"},
                post_data="UserID=example-user&Password=p%40ss%2Bword"))
        blob = self._network()
        self.assertNotIn("p%40ss%2Bword", blob)
        self.assertIn("Password", blob)

    def test_the_network_log_masks_by_name_as_well_as_by_value(self):
        with contextlib.ExitStack() as stack:
            s = self._session(stack)
            s._on_request(_Request(
                method="GET",
                url="https://www.example.com/x?api_key=SYNTHETICKEY",
                headers={"authorization": "Bearer SYNTHETICBEARER"}))
            s._on_response(_Response(
                json.dumps({"user": USERNAME}).encode()))
        blob = self._network()
        for secret in ("SYNTHETICKEY", "SYNTHETICBEARER", USERNAME):
            self.assertNotIn(secret, blob)

    def test_an_oversized_body_is_recorded_by_size_alone(self):
        with contextlib.ExitStack() as stack:
            s = self._session(stack)
            s._on_response(_Response(b"x" * (explore.MAX_BODY_BYTES + 1)))
        record = json.loads(self._network())
        self.assertTrue(record["body_truncated"])
        self.assertNotIn("body_text", record)

    def test_the_click_log_masks_the_credentials(self):
        with contextlib.ExitStack() as stack:
            s = self._session(stack)
            s.event({"kind": "click", "text": USERNAME,
                     "url": f"https://www.example.com/?u={USERNAME}"})
        self.assertNotIn(USERNAME, (self.root / "debug" / "clicks.jsonl")
                         .read_text())

    def test_an_error_mid_session_still_leaves_the_har_scrubbed(self):
        # The close that flushes the raw HAR runs on every unwind; the scrub
        # queued at construction has to run after it.
        har = {"log": {"entries": [{"request": {
            "method": "GET", "url": "https://www.example.com/?token=SYNTHETICTOKEN",
            "headers": [{"name": "Cookie", "value": "session=SYNTHETICJAR"}],
            "cookies": [], "queryString": []},
            "response": {"headers": [], "cookies": [], "content": {}}}]}}

        class Cam:
            def __init__(self, **kwargs):
                self.har = Path(kwargs["record_har_path"])

            def __enter__(self):
                return _Context()

            def __exit__(self, *exc):
                self.har.write_text(json.dumps(har), encoding="utf-8")
                return False

        camoufox = types.ModuleType("camoufox")
        camoufox.sync_api = types.SimpleNamespace(Camoufox=Cam)
        modules = {"camoufox": camoufox, "camoufox.sync_api": camoufox.sync_api}
        with mock.patch.dict(sys.modules, modules):
            with self.assertRaises(RuntimeError):
                with contextlib.ExitStack() as stack:
                    s = self._session(stack)
                    s.open_camoufox(stack, self.root / "profile")
                    raise RuntimeError("session died mid-flight")
        blob = (self.root / "debug" / "network.har").read_text()
        self.assertNotIn("SYNTHETICTOKEN", blob)
        self.assertNotIn("SYNTHETICJAR", blob)

    def test_dom_snapshots_keep_to_the_site_and_blank_passwords(self):
        site = _Frame("https://secure.example.com/login",
                      '<form><input type="password" value="typed-secret"></form>')
        foreign = _Frame("https://tracker.example.net/", "<div id='ad'></div>")
        page = _Page(frames=[site, foreign])
        with contextlib.ExitStack() as stack:
            s = self._session(stack, dom_interval=1,
                              observe=re.compile(r"(^|\.)example\.com$"))
            s.context = _Context(pages=[page])
            s.snapshot_dom()
            s.snapshot_dom()          # same structure: not written again
        self.assertEqual(s.screens, 1)
        snap = self.root / "debug" / "dom" / "001"
        self.assertEqual(sorted(p.name for p in snap.iterdir()),
                         ["frame0.html", "url.txt"])
        self.assertNotIn("typed-secret", (snap / "frame0.html").read_text())

    def test_snapshots_need_a_host_gate(self):
        # Without one there is no telling the site's frames from a
        # tracker's, so the session takes none.
        with contextlib.ExitStack() as stack:
            s = self._session(stack, dom_interval=3)
        self.assertEqual(s.dom_interval, 0)
        self.assertFalse((self.root / "debug" / "dom").exists())

    def test_a_download_is_saved_under_a_sequenced_name(self):
        with contextlib.ExitStack() as stack:
            s = self._session(stack)
            first, second = _Download("statement.pdf"), _Download("statement.pdf")
            s._save_download(first)
            s._save_download(second)
        self.assertNotEqual(first.saved, second.saved)
        self.assertEqual(Path(first.saved).parent,
                         self.root / "debug" / "downloads")
        self.assertEqual([e["kind"] for e in self._events()],
                         ["download", "download"])

    def test_a_detected_login_form_is_filled_through_the_hook(self):
        filled = []
        with contextlib.ExitStack() as stack:
            s = self._session(stack)
            context = _Context()
            s.attach(context, init_js="/* detector */", event_prefix="__EX__ ",
                     prefill=lambda page: filled.append(page) or True)
            page = context.new_page()
            (on_console,) = page.handlers["console"]
            on_console(types.SimpleNamespace(
                text='__EX__ {"kind": "login-form-detected"}'))
            on_console(types.SimpleNamespace(text="page noise"))
        self.assertEqual(context.init_scripts, ["/* detector */"])
        self.assertEqual(filled, [page])
        self.assertEqual([e["kind"] for e in self._events()],
                         ["login-form-detected", "credentials-prefilled"])

    def test_every_page_is_watched_once(self):
        # The context's page event fires for new_page() too; a harness that
        # also watched its first page by hand logged every event twice.
        with contextlib.ExitStack() as stack:
            s = self._session(stack)
            context = _Context()
            s.attach(context, event_prefix="__EX__ ")
            page = s.open("https://www.example.com/")
        self.assertEqual(len(page.handlers["console"]), 1)
        self.assertEqual(len(page.handlers["download"]), 1)

    def test_a_session_records_its_start_and_its_stop(self):
        with mock.patch.dict(sys.modules, _playwright_stub()):
            with contextlib.ExitStack() as stack:
                s = self._session(stack)
                context = _Context()
                s.attach(context)
                s.open("https://www.example.com/")
                reason = s.record("walk it.", max_duration=0)
        self.assertEqual(reason, "timeout")
        self.assertEqual([e["kind"] for e in self._events()],
                         ["started", "stopped"])


if __name__ == "__main__":
    unittest.main()
