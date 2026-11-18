"""A collector's declaration: the facts about it that every verb shares.

Each collector states them once, as ``SPEC`` in its ``collector.py``. That
file imports the standard library and collectorkit only, so a tool reads
it without the collector's own dependencies::

    python -m collectorkit.spec describe collectors/fred collectors/amex

prints one JSON object per collector: what its download signs in with,
whether it can run with no person present, and whether it can bound its
fetch. :func:`paths` derives the default file locations from the name.

Imported as ``from collectorkit import spec``; it is not imported by the
package itself.
"""
from __future__ import annotations

import argparse
import dataclasses
import importlib.util
import json
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import NamedTuple

from collectorkit.debugcap import SCREENSHOTS_DIR

# What a download signs in with:
#   "none"     nothing: a public source, or no download at all;
#   "key"      a key or client credential from the env file;
#   "token"    OAuth tokens a login stores and a download refreshes;
#   "state"    a browser session state file a login stores;
#   "profile"  a persistent browser profile that holds the device trust.
SESSIONS = ("none", "key", "token", "state", "profile")

# How far a download fetches: "bounded" honours --lookback, "full" always
# fetches the whole history. None: the collector has no download.
WINDOWS = ("bounded", "full")

# The mount points the wrappers give a container.
SECRETS_DIR = Path("/secrets")
DATA_DIR = Path("/data")


@dataclass(frozen=True)
class Collector:
    """The declaration of one collector.

    ``unattended`` is true only for a download that signs in nowhere and
    fires no challenge, so it may run with no person present (AGENTS.md
    §2). ``debug_subdirs`` and ``debug_globs`` name the debug artefacts
    ``prune`` reclaims from a complete run (see
    :class:`collectorkit.prune.PruneConfig`).
    """
    name: str
    env_prefix: str
    session: str
    unattended: bool
    window: str | None
    debug_subdirs: tuple[str, ...] = (SCREENSHOTS_DIR,)
    debug_globs: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        if self.session not in SESSIONS:
            raise ValueError(f"{self.name}: session must be one of "
                             f"{SESSIONS}, not {self.session!r}")
        if self.window is not None and self.window not in WINDOWS:
            raise ValueError(f"{self.name}: window must be one of "
                             f"{WINDOWS} or None, not {self.window!r}")
        if self.unattended and self.window is None:
            raise ValueError(f"{self.name}: a collector with no download "
                             "cannot run one unattended")
        if not re.fullmatch(r"[A-Z][A-Z0-9_]*", self.env_prefix):
            raise ValueError(f"{self.name}: env_prefix must be an upper-case "
                             f"variable name, not {self.env_prefix!r}")


class Paths(NamedTuple):
    """A collector's default file locations."""
    env_file: Path
    profile_dir: Path
    state_path: Path
    silver_db: Path
    migrations_dir: Path


def paths(spec: Collector, root: Path, *, secrets_dir: Path = SECRETS_DIR,
          data_dir: Path = DATA_DIR) -> Paths:
    """The default paths of the collector `spec` whose code is in `root`,
    under the container's mount points by default."""
    secrets_dir, data_dir = Path(secrets_dir), Path(data_dir)
    return Paths(
        env_file=secrets_dir / f"{spec.name}.env",
        profile_dir=secrets_dir / f"{spec.name}-profile",
        state_path=secrets_dir / f"{spec.name}-state.json",
        silver_db=data_dir / f"{spec.name}.db",
        migrations_dir=Path(root) / "migrations",
    )


def load(collector_dir: Path) -> Collector:
    """The ``SPEC`` of the ``collector.py`` in `collector_dir`."""
    path = Path(collector_dir) / "collector.py"
    module_spec = importlib.util.spec_from_file_location(
        f"_collector_spec_{Path(collector_dir).resolve().name}", path)
    if module_spec is None or not path.is_file():
        raise FileNotFoundError(f"no collector.py in {collector_dir}")
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    found = getattr(module, "SPEC", None)
    if not isinstance(found, Collector):
        raise TypeError(f"{path}: SPEC is not a collectorkit.spec.Collector")
    return found


def describe(spec: Collector) -> dict:
    """`spec` as a JSON-ready dict."""
    out = dataclasses.asdict(spec)
    for key in ("debug_subdirs", "debug_globs"):
        out[key] = list(out[key])
    return out


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="python -m collectorkit.spec",
        description="Print collector declarations as JSON.")
    sub = p.add_subparsers(dest="command", required=True)
    d = sub.add_parser("describe", help="Print the SPEC of each collector dir, "
                                        "one JSON object per line.")
    d.add_argument("dirs", nargs="+", type=Path, metavar="DIR",
                   help="A collector dir holding a collector.py.")
    args = p.parse_args(argv)
    for collector_dir in args.dirs:
        print(json.dumps(describe(load(collector_dir)), sort_keys=True))
    return 0


if __name__ == "__main__":
    # Run as a script, this file is `__main__`; a collector.py builds its
    # SPEC from the imported module, so that module's main does the work.
    from collectorkit.spec import main as _main
    sys.exit(_main())
