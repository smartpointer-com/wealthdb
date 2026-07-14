"""
CLI path defaults.

The wrapper resolves the host-side dirs (env vars → XDG defaults) and
mounts them at fixed container paths, so the scripts must be runnable
with no path flags at all: `login` / `download` default --state-path
to the canonical /secrets location instead of requiring it.

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

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


if __name__ == "__main__":
    unittest.main()
