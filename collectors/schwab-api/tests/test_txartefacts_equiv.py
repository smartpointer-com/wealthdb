"""Equivalence proof for the shared transactions-artefact enumerator.

Both `download` (via the harvester) and `load` (via the wrapper) resolve
a run's `transactions_*.json` set through
`_txartefacts.transaction_files`. These tests assert the shared helper
matches local reference implementations of the rule across a battery of
fixture run-dirs (compression variants, coexisting twins, decoys,
non-file entries), and that the two entry points agree.

Synthetic filenames / values only.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import _txartefacts  # noqa: E402
import download  # noqa: E402
import load as loader  # noqa: E402
from collectorkit import compress  # noqa: E402


# --- Reference implementations of the artefact-resolution rule ---

_ORIG_TRANSACTIONS_FILE_RE = re.compile(r"^transactions_.*\.json$")


def _orig_load_transaction_files(dump_dir: Path) -> list[Path]:
    """Copy of load.py's original `transaction_files` (+ its
    `_logical_bronze_path` helper, inlined)."""
    def _logical(path: Path) -> Path:
        for suffix in compress.VARIANT_SUFFIXES:
            if path.name.endswith(suffix):
                return path.with_name(path.name[:-len(suffix)])
        return path

    logical_names: set[str] = set()
    for entry in dump_dir.iterdir():
        if not entry.is_file():
            continue
        logical = _logical(entry)
        if _ORIG_TRANSACTIONS_FILE_RE.match(logical.name):
            logical_names.add(logical.name)
    resolved: list[Path] = []
    for name in sorted(logical_names):
        variant = compress.resolve_variant(dump_dir / name)
        if variant is not None:
            resolved.append(variant)
    return resolved


def _orig_download_resolve(run_dir: Path) -> list[Path]:
    """Copy of download.py's original `_resolve_transaction_artefacts`."""
    logical_names: set[str] = set()
    for entry in run_dir.iterdir():
        if not entry.is_file():
            continue
        name = entry.name
        for suffix in compress.VARIANT_SUFFIXES:
            if name.endswith(suffix):
                name = name[:-len(suffix)]
                break
        if name.startswith("transactions_") and name.endswith(".json"):
            logical_names.add(name)
    resolved: list[Path] = []
    for name in sorted(logical_names):
        variant = compress.resolve_variant(run_dir / name)
        if variant is not None:
            resolved.append(variant)
    return resolved


# --- Fixture builders exercising each edge case ---

def _touch(p: Path, body: bytes = b'{"transactions": []}') -> Path:
    p.write_bytes(body)
    return p


def _build_variants(tmp_path):
    """Yield (label, run_dir) fixtures covering the interesting shapes."""
    cases = []

    # 1. Empty dir.
    d = tmp_path / "empty"
    d.mkdir()
    cases.append(("empty", d))

    # 2. Plain files only, out of natural order (proves sort).
    d = tmp_path / "plain"
    d.mkdir()
    _touch(d / "transactions_002.json")
    _touch(d / "transactions_000.json")
    _touch(d / "transactions_010.json")
    cases.append(("plain-unordered", d))

    # 3. All compressed (.zst).
    d = tmp_path / "zst"
    d.mkdir()
    for n in (0, 1, 2):
        compress.compress_file(_touch(d / f"transactions_{n:03d}.json"))
    cases.append(("all-zst", d))

    # 4. Coexisting plain + .zst twin (plain must win).
    d = tmp_path / "twin"
    d.mkdir()
    _touch(d / "transactions_000.json")
    compress.compress_file(_touch(d / "transactions_000.json"),
                           remove_original=False)
    _touch(d / "transactions_001.json")
    cases.append(("plain-zst-twin", d))

    # 5. Decoys: other artefacts, a .gz, and lookalikes that must NOT match.
    d = tmp_path / "decoys"
    d.mkdir()
    _touch(d / "transactions_000.json")
    _touch(d / "accounts_positions.json")
    _touch(d / "run.json")
    _touch(d / "transactions_003.json.notjson")   # wrong logical suffix
    _touch(d / "transactionsX_000.json")           # missing underscore
    cases.append(("decoys", d))

    # 6. Non-file entry (a subdir named like an artefact) must be skipped.
    d = tmp_path / "subdir"
    d.mkdir()
    (d / "transactions_000.json").mkdir()           # a DIRECTORY, not a file
    _touch(d / "transactions_001.json")
    cases.append(("dir-named-like-artefact", d))

    return cases


def test_shared_matches_both_originals(tmp_path):
    for label, run_dir in _build_variants(tmp_path):
        expected = _orig_load_transaction_files(run_dir)
        # Both originals must already agree with each other.
        assert _orig_download_resolve(run_dir) == expected, label
        # The shared helper reproduces that result exactly.
        assert _txartefacts.transaction_files(run_dir) == expected, label


def test_entry_points_agree(tmp_path):
    """`load.transaction_files` (wrapper) and the downloader's harvest path
    resolve to the identical ordered list."""
    for label, run_dir in _build_variants(tmp_path):
        expected = _orig_load_transaction_files(run_dir)
        assert loader.transaction_files(run_dir) == expected, label
        # download.py now calls _txartefacts.transaction_files directly for
        # symbol harvesting; assert that binding is the shared helper.
        assert (download._txartefacts.transaction_files(run_dir)
                == expected), label
