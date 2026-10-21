"""Every series states a basis the engine reads.

The engine reads a series' basis and compounding from its payload with
fixedincome.ParseBasis (wealthdb/internal/fixedincome/curve.go). A word
it does not take refuses the whole currency's curve, with only a log
line. These tests read the words from the Go source rather than restate
them, so a rename on either side fails here.
"""

from __future__ import annotations

import re
from pathlib import Path

import download

CURVE_GO = (Path(__file__).resolve().parents[3] / "wealthdb" / "internal"
            / "fixedincome" / "curve.go")


def _go() -> str:
    return CURVE_GO.read_text(encoding="utf-8")


def compoundings() -> dict[str, int]:
    """The `compoundings` map: each compounding word with its frequency."""
    body = re.search(r"var compoundings = map\[string\]int\{(.*?)\}", _go()).group(1)
    return {name: int(f) for name, f in re.findall(r'"(\w+)":\s*(\d+)', body)}


def stated_kinds() -> set[str]:
    """The basis words ParseBasis reads with a compounding: its `kind…`
    constants."""
    return set(re.findall(r'^\s*kind\w+\s*=\s*"(\w+)"', _go(), re.MULTILINE))


def par_frequencies() -> set[int]:
    """The coupon frequencies Basis.check lets a par curve take."""
    rule = re.search(r"if !b\.Spot && (b\.Frequency != \d+(?: && b\.Frequency != \d+)*)", _go()).group(1)
    return {int(f) for f in re.findall(r"\d+", rule)}


def test_the_go_vocabulary_reads():
    assert compoundings()["continuous"] == 0
    assert stated_kinds() == {"spot", "par"}
    assert par_frequencies()


def test_every_series_states_a_basis_parse_basis_takes():
    words, kinds, par = compoundings(), stated_kinds(), par_frequencies()
    for sid, (_, _, _, basis, compounding) in download.RATE_SERIES.items():
        assert basis in kinds, sid
        assert compounding in words, sid
        if basis == "par":
            assert words[compounding] in par, sid
