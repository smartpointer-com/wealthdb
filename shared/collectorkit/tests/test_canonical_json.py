"""collectorkit.silver.canonical_json: the compact, key-sorted JSON that
silver `payload` columns and content-based dedup compare byte-for-byte.

The schwab-api, ubs-psn and equityzen loaders store the `ascii=True` form,
so both variants are pinned here as literal expected strings.
"""
from __future__ import annotations

from datetime import date, datetime
from decimal import Decimal

import pytest

from collectorkit import bronze, silver

# (input, expected output) where both ascii variants agree: the input
# carries no non-ASCII text.
ASCII_ONLY = [
    (None, "null"),
    ({}, "{}"),
    ([], "[]"),
    ({"b": 1, "a": 2, "c": 3}, '{"a":2,"b":1,"c":3}'),
    ({"z": {"y": {"x": [3, 2, 1]}}, "a": 0}, '{"a":0,"z":{"y":{"x":[3,2,1]}}}'),
    ({"sep": [1, 2, {"a": "b"}], "n": None}, '{"n":null,"sep":[1,2,{"a":"b"}]}'),
    ({"f": 1.5, "g": -0.0, "h": 1e-9, "i": 100.0},
     '{"f":1.5,"g":-0.0,"h":1e-09,"i":100.0}'),
    ({"true": True, "false": False, "int": 10 ** 30},
     '{"false":false,"int":1000000000000000000000000000000,"true":true}'),
    ([{"a": 1}, {"a": 2}, {"a": 1}], '[{"a":1},{"a":2},{"a":1}]'),
]

# (input, ascii=True output, default output)
NON_ASCII = [
    ({"k": "ä"}, '{"k":"\\u00e4"}', '{"k":"ä"}'),
    ({"k": "grüße 日本語 🚀"},
     '{"k":"gr\\u00fc\\u00dfe \\u65e5\\u672c\\u8a9e \\ud83d\\ude80"}',
     '{"k":"grüße 日本語 🚀"}'),
    ("bare ü", '"bare \\u00fc"', '"bare ü"'),
]


@pytest.mark.parametrize("obj,expected", ASCII_ONLY)
def test_sorted_keys_and_compact_separators(obj, expected):
    assert silver.canonical_json(obj) == expected
    assert silver.canonical_json(obj, ascii=True) == expected


@pytest.mark.parametrize("obj,escaped,literal", NON_ASCII)
def test_ascii_flag_escapes_non_ascii(obj, escaped, literal):
    """ascii=True escapes to \\uXXXX (astral characters as a surrogate
    pair); the default emits UTF-8 literally."""
    assert silver.canonical_json(obj, ascii=True) == escaped
    assert silver.canonical_json(obj) == literal


@pytest.mark.parametrize("ascii", [False, True])
def test_non_json_scalars_serialise_as_their_string(ascii):
    obj = {"mix": [Decimal("0"), Decimal("12345.6789"), date(2026, 7, 12),
                   datetime(2026, 7, 12, 13, 45, 30), None]}
    assert silver.canonical_json(obj, ascii=ascii) == (
        '{"mix":["0","12345.6789","2026-07-12","2026-07-12 13:45:30",null]}')


def test_bronze_serialises_with_the_same_function():
    assert bronze.canonical_json is silver.canonical_json
