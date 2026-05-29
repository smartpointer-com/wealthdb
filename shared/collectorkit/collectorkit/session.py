"""Session-state persistence shared by collectors that keep a JSON state
file (cookies, tokens, CSRF metadata). Provides atomic write + chmod
0o600 + an ISO-UTC timestamp helper.

The state file is the keys-to-the-kingdom (it lets the next run skip
MFA); mode is never wider than 0o600.
"""
from __future__ import annotations

import json
import logging
import os
from datetime import datetime, timezone
from pathlib import Path

log = logging.getLogger(__name__)

# Owner-only read/write. Never widen for state files.
STATE_FILE_MODE = 0o600


def iso_now() -> str:
    """UTC ISO-8601 timestamp at second precision (the collectors'
    convention for `saved_at` / `minted_at` / `creation_timestamp`)."""
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def save_state(path: Path, payload: dict, mode: int = STATE_FILE_MODE,
               indent: int = 2) -> None:
    """Atomically write `payload` as JSON to `path`, then chmod.

    A sibling `.tmp` file is written and chmodded *before* rename so the
    target's mode never widens past `mode`. Parent dirs are created.
    """
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(payload, indent=indent), encoding="utf-8")
    tmp.chmod(mode)
    tmp.replace(path)


def load_state(path: Path) -> dict | None:
    """Read JSON from `path`. Returns None if missing or unparseable —
    callers treat both as "no usable state, do a fresh login"."""
    path = Path(path)
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        log.warning("could not read state file %s: %s", path, exc)
        return None


def secure_file(path: Path, mode: int = STATE_FILE_MODE) -> bool:
    """Best-effort chmod for files written by external libraries
    (Playwright `storage_state`, schwab-py's token file). Returns True
    on success, False on a platform that doesn't honour POSIX modes."""
    try:
        os.chmod(path, mode)
        return True
    except OSError as exc:
        log.warning("could not chmod 0%o on %s: %s", mode, path, exc)
        return False
