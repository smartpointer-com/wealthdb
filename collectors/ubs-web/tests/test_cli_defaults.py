"""CLI path defaults.

The wrapper resolves the host-side dirs (env vars → XDG defaults) and
mounts them at fixed container paths, so the scripts must be runnable
with no path flags at all: `login` / `download` default --state-path
to the canonical /secrets location instead of requiring it.
"""
from __future__ import annotations

import download
import login


def test_login_state_path_defaults_to_secrets_mount():
    args = login.parse_args([])
    assert args.state_path == login.DEFAULT_STATE_PATH


def test_download_state_path_defaults_to_secrets_mount():
    args = download.parse_args(["--dry-run"])
    assert args.state_path == download.DEFAULT_STATE_PATH
