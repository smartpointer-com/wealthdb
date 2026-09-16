"""The hand-copied canonical vocabularies must match the Go source.

A collector cannot import Go, so `collectors/manual/load.py` keeps literal
sets of the account-kind / tax-wrapper / management-style values and fails a
CSV row that names anything else — with the file and row number still in
hand, which is the whole point of that collector. The cost of the copy is
that it drifts silently in BOTH directions: a value gold accepts but the
mirror omits aborts an otherwise good load, and a value the mirror allows
but gold rejects reaches the adapter and is laundered or refused there.

It had already drifted by four values (`401k`, `403b`, `457b`, `529` — every
one of them a no-portal plan, which is exactly what the manual collector
exists for) before anything noticed.

This test lives in collectorkit rather than in either side because it is the
only suite that runs on the HOST: the collector's pytest sees only its own
directory inside its image, and `go test` is mounted at the inner
`wealthdb/` dir, so neither can read the other's source.
"""
from __future__ import annotations

import ast
import re
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[3]
ENUMS_GO = REPO / "wealthdb/internal/canonical/enums.go"
MANUAL_LOAD = REPO / "collectors/manual/load.py"

# (Go value-set var, Go const type, Python set name)
MIRRORS = [
    ("accountKindValues", "AccountKind", "ACCOUNT_KINDS"),
    ("taxWrapperValues", "TaxWrapper", "TAX_WRAPPERS"),
    ("managementStyleValues", "ManagementStyle", "MANAGEMENT_STYLES"),
]


def _go_source() -> str:
    assert ENUMS_GO.is_file(), f"canonical enums not found at {ENUMS_GO}"
    return ENUMS_GO.read_text()


def _const_strings(src: str, go_type: str) -> dict[str, str]:
    """Map each `Foo <Type> = "bar"` constant name to its string value."""
    return dict(re.findall(
        rf'^\s*(\w+)\s+{go_type}\s*=\s*"([^"]+)"', src, re.MULTILINE))


def _value_set(src: str, var: str, consts: dict[str, str]) -> set[str]:
    """The string values a `var <name> = map[...]struct{}{...}` admits."""
    m = re.search(rf"var {var} = map\[[^\]]+\]struct\{{\}}\{{(.*?)\n\}}",
                  src, re.DOTALL)
    assert m, f"{var} not found in {ENUMS_GO.name}"
    idents = re.findall(r"(\w+):\s*\{\}", m.group(1))
    missing = [i for i in idents if i not in consts]
    assert not missing, f"{var} names constants with no string literal: {missing}"
    return {consts[i] for i in idents}


def _python_set(name: str) -> set[str]:
    assert MANUAL_LOAD.is_file(), f"manual loader not found at {MANUAL_LOAD}"
    tree = ast.parse(MANUAL_LOAD.read_text())
    for node in tree.body:
        if not isinstance(node, ast.Assign):
            continue
        for target in node.targets:
            if isinstance(target, ast.Name) and target.id == name:
                return set(ast.literal_eval(node.value))
    raise AssertionError(f"{name} not found in {MANUAL_LOAD.name}")


@pytest.mark.parametrize("go_var,go_type,py_name", MIRRORS)
def test_the_mirror_matches_the_canonical_vocabulary(go_var, go_type, py_name):
    src = _go_source()
    canonical = _value_set(src, go_var, _const_strings(src, go_type))
    mirrored = _python_set(py_name)

    # Both directions matter. Missing values abort a load gold would have
    # accepted; extra ones pass validation here and are refused downstream,
    # where the CSV row number is long gone.
    assert mirrored == canonical, (
        f"{py_name} has drifted from {go_var}: "
        f"missing {sorted(canonical - mirrored)}, "
        f"extra {sorted(mirrored - canonical)}")
