"""
The collector's own settings: $XDG_CONFIG_HOME/plaid.cfg, by default
~/.config/plaid.cfg. The file holds settings, never a credential: the
app keys stay in plaid.env. It is a JSON object:

    {"billed_reads": ["/investments/refresh"]}

`billed_reads` opts in to the reads Plaid bills per successful call on a
paid plan. A Production run makes such a read only when this list names
it. The Sandbox never bills, so it needs no opt-in.

The file is JSON, read as data, so nothing in it runs. The process
environment cannot stand in for it, so neither plaid.env nor a shell can
opt in by accident. A missing file holds no settings. A file that does not
parse, or names a setting or a read this collector does not know, is an
error: a typo must not pass for a setting that is off.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

import plaidapi

FILE_NAME = "plaid.cfg"
BILLED_READS = "billed_reads"


class ConfigError(Exception):
    """plaid.cfg exists and cannot be used. `str()` names the file."""


@dataclass(frozen=True)
class Config:
    billed_reads: frozenset = frozenset()


def path() -> Path:
    """Where plaid.cfg lives: in $XDG_CONFIG_HOME, else in ~/.config."""
    xdg = os.environ.get("XDG_CONFIG_HOME", "").strip()
    base = Path(xdg) if xdg else Path.home() / ".config"
    return base / FILE_NAME


def load(where: Path | None = None) -> Config:
    where = where or path()
    try:
        raw = where.read_bytes()
    except FileNotFoundError:
        return Config()
    except OSError as e:
        raise ConfigError(f"{where} cannot be read: {e}") from e
    try:
        # From bytes, so a file in no Unicode encoding is a JSON error too.
        doc = json.loads(raw)
    except ValueError as e:
        raise ConfigError(f"{where} is not valid JSON: {e}") from e
    if not isinstance(doc, dict):
        raise ConfigError(f"{where} must hold a JSON object.")
    unknown = sorted(set(doc) - {BILLED_READS})
    if unknown:
        raise ConfigError(f"{where} names settings this collector does not "
                          f"know: {', '.join(unknown)}. It knows: "
                          f"{BILLED_READS}.")
    reads = doc.get(BILLED_READS, [])
    if not isinstance(reads, list) or not all(isinstance(r, str)
                                              for r in reads):
        raise ConfigError(f"{where}: {BILLED_READS} is a list of routes, "
                          f"such as [\"/investments/refresh\"].")
    stray = sorted(set(reads) - plaidapi.BILLED_ENDPOINTS)
    if stray:
        raise ConfigError(
            f"{where}: {BILLED_READS} names {', '.join(stray)}, which this "
            f"collector does not call. The billed reads it calls are: "
            f"{', '.join(sorted(plaidapi.BILLED_ENDPOINTS))}.")
    return Config(billed_reads=frozenset(reads))
