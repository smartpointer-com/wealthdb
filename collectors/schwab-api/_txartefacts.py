"""Shared resolution of per-run `transactions_*.json` bronze artefacts.

Both the downloader (symbol harvesting for the default instrument lookup) and the
silver loader need the same enumeration of numbered transactions
artefacts in a run dir, resolving compressed variants. Extracting it here
keeps the two entry points byte-identical and avoids a download<->load
import cycle (this module depends only on `collectorkit.compress`).
"""

from __future__ import annotations

import re
from pathlib import Path

from collectorkit import compress

# Logical name of a numbered transactions artefact — matched after any
# compression suffix (.zst/.gz) is stripped, so the .json.zst form
# download writes resolves alongside the plain .json.
TRANSACTIONS_FILE_RE = re.compile(r"^transactions_.*\.json$")


def logical_bronze_path(path: Path) -> Path:
    """Strip a compression suffix (`.zst` / `.gz`) to recover the LOGICAL
    (uncompressed) bronze name. Plain paths pass through unchanged."""
    for suffix in compress.VARIANT_SUFFIXES:
        if path.name.endswith(suffix):
            return path.with_name(path.name[:-len(suffix)])
    return path


def transaction_files(run_dir: Path) -> list[Path]:
    """On-disk `transactions_*.json` artefacts in a run dir, resolving
    compressed variants.

    download / recompress may write each numbered transactions file as
    `.json.zst`, which a plain `run_dir.glob("transactions_*.json")`
    would miss. Enumerate the LOGICAL names (strip any `.zst`/`.gz`)
    matching `transactions_*.json`, dedup, then resolve each variant —
    plain wins over a coexisting `.zst` twin (a recompress interrupted
    between verify and unlink), so each window ingests exactly once.
    Sorted by logical name for deterministic ordering (matching the
    prior `sorted(glob(...))`)."""
    logical_names: set[str] = set()
    for entry in run_dir.iterdir():
        if not entry.is_file():
            continue
        logical = logical_bronze_path(entry)
        if TRANSACTIONS_FILE_RE.match(logical.name):
            logical_names.add(logical.name)
    resolved: list[Path] = []
    for name in sorted(logical_names):
        variant = compress.resolve_variant(run_dir / name)
        if variant is not None:
            resolved.append(variant)
    return resolved
