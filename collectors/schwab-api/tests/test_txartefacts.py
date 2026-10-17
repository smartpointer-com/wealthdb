"""The shared transactions-artefact enumerator.

Both `download` (via the harvester) and `load` (via the wrapper) resolve
a run's `transactions_*.json` set through
`_txartefacts.transaction_files`. These tests pin its result across a
battery of fixture run-dirs (compression variants, coexisting twins,
decoys, non-file entries), and that the two entry points agree.

Synthetic filenames / values only.
"""
from __future__ import annotations

import sys
from pathlib import Path


sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import _txartefacts  # noqa: E402
import download  # noqa: E402
import load as loader  # noqa: E402
from collectorkit import compress  # noqa: E402


def _touch(p: Path, body: bytes = b'{"transactions": []}') -> Path:
    p.write_bytes(body)
    return p


def _build_variants(tmp_path):
    """Yield (label, run_dir, expected names) fixtures covering the
    interesting shapes."""
    cases = []

    # 1. Empty dir.
    d = tmp_path / "empty"
    d.mkdir()
    cases.append(("empty", d, []))

    # 2. Plain files only, out of natural order (proves sort).
    d = tmp_path / "plain"
    d.mkdir()
    _touch(d / "transactions_002.json")
    _touch(d / "transactions_000.json")
    _touch(d / "transactions_010.json")
    cases.append(("plain-unordered", d, [
        "transactions_000.json", "transactions_002.json",
        "transactions_010.json"]))

    # 3. All compressed (.zst).
    d = tmp_path / "zst"
    d.mkdir()
    for n in (0, 1, 2):
        compress.compress_file(_touch(d / f"transactions_{n:03d}.json"))
    cases.append(("all-zst", d, [
        "transactions_000.json.zst", "transactions_001.json.zst",
        "transactions_002.json.zst"]))

    # 4. Coexisting plain + .zst twin (plain must win).
    d = tmp_path / "twin"
    d.mkdir()
    _touch(d / "transactions_000.json")
    compress.compress_file(_touch(d / "transactions_000.json"),
                           remove_original=False)
    _touch(d / "transactions_001.json")
    cases.append(("plain-zst-twin", d, [
        "transactions_000.json", "transactions_001.json"]))

    # 5. Decoys: other artefacts and lookalikes that must NOT match.
    d = tmp_path / "decoys"
    d.mkdir()
    _touch(d / "transactions_000.json")
    _touch(d / "accounts_positions.json")
    _touch(d / "run.json")
    _touch(d / "transactions_003.json.notjson")   # wrong logical suffix
    _touch(d / "transactionsX_000.json")           # missing underscore
    cases.append(("decoys", d, ["transactions_000.json"]))

    # 6. Non-file entry (a subdir named like an artefact) must be skipped.
    d = tmp_path / "subdir"
    d.mkdir()
    (d / "transactions_000.json").mkdir()           # a DIRECTORY, not a file
    _touch(d / "transactions_001.json")
    cases.append(("dir-named-like-artefact", d, ["transactions_001.json"]))

    return cases


def test_transaction_files_resolves_each_shape(tmp_path):
    for label, run_dir, names in _build_variants(tmp_path):
        expected = [run_dir / n for n in names]
        assert _txartefacts.transaction_files(run_dir) == expected, label


def test_entry_points_agree(tmp_path):
    """`load.transaction_files` (wrapper) and the downloader's harvest path
    resolve to the identical ordered list."""
    for label, run_dir, _names in _build_variants(tmp_path):
        expected = _txartefacts.transaction_files(run_dir)
        assert loader.transaction_files(run_dir) == expected, label
        # download.py harvests symbols through _txartefacts directly;
        # assert that binding is the shared helper.
        assert (download._txartefacts.transaction_files(run_dir)
                == expected), label
