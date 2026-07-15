"""
CLI path defaults and flag surface.

The wrapper resolves the host-side dirs (env vars → XDG defaults) and
mounts them at fixed container paths, so the scripts must be runnable
with no path flags at all: `login` / `download` default --state-path
to the canonical /secrets location instead of requiring it.

Also covers --debug, the gate on the bronze-resident landmark captures.
Its payload runs inside run()'s live Playwright session, so what is
asserted here is the surface: the flag parses, defaults off, and its
help still promises captures rather than the warn-only stub it replaced.

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

import contextlib
import io
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import download  # noqa: E402
import login  # noqa: E402


class TestCliDefaults(unittest.TestCase):
    def test_login_state_path_defaults_to_secrets_mount(self):
        args = login.parse_args([])
        self.assertEqual(args.state_path, login.DEFAULT_STATE_PATH)

    def test_download_state_path_defaults_to_secrets_mount(self):
        args = download.parse_args(["--dry-run"])
        self.assertEqual(args.state_path, download.DEFAULT_STATE_PATH)

    def test_login_and_download_agree_on_state_path(self):
        # download reads the file login mints; the defaults must never drift.
        self.assertEqual(login.DEFAULT_STATE_PATH, download.DEFAULT_STATE_PATH)


class TestDebugFlag(unittest.TestCase):
    def test_debug_defaults_off(self):
        # Opt-in: a routine dump holds only what `load` reads.
        self.assertFalse(download.parse_args([]).debug)

    def test_debug_parses(self):
        self.assertTrue(download.parse_args(["--debug"]).debug)

    def test_debug_is_independent_of_screenshot_dir(self):
        # The two are distinct concepts: --debug writes inside the run
        # dir, --screenshot-dir outside bronze. Neither implies the other.
        args = download.parse_args(["--debug"])
        self.assertTrue(args.debug)
        self.assertIsNone(args.screenshot_dir)

    def test_debug_help_promises_bronze_captures(self):
        # Guards against the flag regressing to a warn-only stub.
        out = io.StringIO()
        with contextlib.redirect_stdout(out), self.assertRaises(SystemExit):
            download.parse_args(["--help"])
        text = out.getvalue()
        self.assertIn("--debug", text)
        self.assertNotIn("gates nothing", text)


if __name__ == "__main__":
    unittest.main()
