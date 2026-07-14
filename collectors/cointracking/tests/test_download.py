"""Unit tests for cointracking download.py's argument parsing.

The browser/export paths are exercised live, not here. This covers only the
shared-flag surface: cointracking always exports the complete trade history
(its holdings replay needs every row), so it accepts --lookback purely for
wealthdb-refresh uniformity and treats it as a no-op (logs a warning).
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402


def test_lookback_flag():
    # Accepted for wealthdb-refresh uniformity; the value is still validated
    # against the shared presets so a typo fails loudly.
    assert download.parse_args(["--lookback", "2y"]).lookback == "2y"
    assert download.parse_args([]).lookback is None
    with pytest.raises(SystemExit):
        download.parse_args(["--lookback", "1m"])  # not a preset


def test_debug_flag():
    # Uniform --debug gate (default off); cointracking's download writes no
    # bronze-resident debug artefact, so it currently gates nothing.
    assert download.parse_args(["--debug"]).debug is True
    assert download.parse_args([]).debug is False
