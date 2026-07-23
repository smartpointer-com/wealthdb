"""Equivalence tests for collectorkit.silver.canonical_json.

The shared helper unified the compact-JSON serializers that several loaders
each defined locally. The migrated loaders (schwab-api, ubs-psn, equityzen) used the
`ensure_ascii`-defaulting-True variant with `default=str`; adopting the shared
helper is only sound if `silver.canonical_json(obj, ascii=True)` reproduces
their output byte-for-byte. These tests pin that against inlined copies of the
ORIGINAL serializers across a battery of fixtures (unicode, nested dicts, key
ordering, separators, floats, Decimal / date, None), and confirm the
`ascii=False` default matches bronze.canonical_json.
"""
from __future__ import annotations

import json
from datetime import date, datetime
from decimal import Decimal

from collectorkit import bronze, silver


# --- Inlined copies of the ORIGINAL local serializers (pre-migration) -------

def _orig_schwab_api(obj) -> str:
    # schwab-api / ubs-psn / equityzen were byte-identical: ensure_ascii
    # defaults to True, plus default=str.
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), default=str)


def _orig_bronze_ascii_false(obj) -> str:
    # bronze.canonical_json: ensure_ascii=False, default=str.
    return json.dumps(obj, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False, default=str)


# --- Fixture battery --------------------------------------------------------

FIXTURES = [
    None,
    {},
    [],
    {"b": 1, "a": 2, "c": 3},                      # sort_keys ordering
    {"z": {"y": {"x": [3, 2, 1]}}, "a": 0},        # nesting
    {"k": "grüße äöü ß"},                          # non-ASCII latin
    {"k": "日本語 テスト"},                          # non-ASCII CJK
    {"k": "emoji 🚀🔥"},                            # astral plane
    {"sep": [1, 2, {"a": "b"}], "n": None},        # separators + None
    {"f": 1.5, "g": -0.0, "h": 1e-9, "i": 100.0},  # floats
    {"d": Decimal("12345.6789")},                  # Decimal via default=str
    {"day": date(2026, 7, 12)},                    # date via default=str
    {"ts": datetime(2026, 7, 12, 13, 45, 30)},     # datetime via default=str
    {"mix": [Decimal("0"), date(2020, 1, 1), None, "grüße", 3.14]},
    {"true": True, "false": False, "int": 10 ** 30},
    "bare string with ünïcöde",
    [{"a": 1}, {"a": 2}, {"a": 1}],
]


def test_ascii_true_matches_original_local_serializers():
    """silver.canonical_json(obj, ascii=True) is byte-identical to the
    schwab-api / ubs-psn / equityzen serializer it replaced."""
    for obj in FIXTURES:
        assert silver.canonical_json(obj, ascii=True) == _orig_schwab_api(obj)


def test_ascii_false_matches_bronze():
    """The default (ascii=False) matches bronze.canonical_json byte-for-byte."""
    for obj in FIXTURES:
        got = silver.canonical_json(obj)
        assert got == bronze.canonical_json(obj)
        assert got == _orig_bronze_ascii_false(obj)


def test_ascii_flag_escapes_non_ascii():
    """ascii=True escapes to \\uXXXX; the default emits UTF-8 literally."""
    obj = {"k": "ä"}
    assert silver.canonical_json(obj, ascii=True) == '{"k":"\\u00e4"}'
    assert silver.canonical_json(obj) == '{"k":"ä"}'


def test_default_str_is_shared_by_both_variants():
    """Both ascii variants carry default=str, so non-JSON-native scalars
    serialise instead of raising."""
    for obj in ({"d": Decimal("1.5")}, {"day": date(2026, 1, 1)}):
        # Neither call raises.
        silver.canonical_json(obj, ascii=True)
        silver.canonical_json(obj)
