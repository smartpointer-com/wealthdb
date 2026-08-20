"""
Debug log tee: with --screenshot-dir set, the full DEBUG-level log is
mirrored to a run.log in the debug dir — a run whose only record was
terminal scrollback cannot be diagnosed later.
"""

from __future__ import annotations

import logging
import sys
from pathlib import Path

import pytest  # noqa: F401

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402


def test_tee_captures_debug_even_when_console_is_info(tmp_path):
    root = logging.getLogger()
    prev_level = root.level
    handler = login.tee_debug_log(tmp_path, logging.INFO)
    try:
        assert handler is not None
        logging.getLogger("schwab-web.download").debug("dbg-marker")
        logging.getLogger("schwab-web.login").info("info-marker")
        handler.flush()
        content = Path(handler.baseFilename).read_text(encoding="utf-8")
        assert "dbg-marker" in content
        assert "info-marker" in content
    finally:
        root.removeHandler(handler)
        handler.close()
        root.setLevel(prev_level)


def test_tee_is_a_no_op_without_a_dir():
    assert login.tee_debug_log(None, logging.INFO) is None


def test_tee_survives_an_unwritable_dir(tmp_path, caplog):
    blocked = tmp_path / "blocked"
    blocked.mkdir()
    blocked.chmod(0o500)
    try:
        with caplog.at_level(logging.WARNING, logger="schwab-web.login"):
            assert login.tee_debug_log(blocked / "sub", logging.INFO) is None
        assert any("debug log" in r.message for r in caplog.records)
    finally:
        blocked.chmod(0o700)
