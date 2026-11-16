"""Import one collector module for a kit test that compares against it.

The kit's suite runs on the host with the whole tree in view, so a test
can call a collector's own function and check that a kit function gives
the same answer. The module is loaded from its file under a private
name, with its collector's directory on the path for its sibling imports.
A third-party package it imports at the top and the kit's venv lacks
(pdfplumber) is stood in for by an empty module: the functions compared
never touch it. What the import added to ``sys.modules``, the kit's own
modules aside, is removed again, so no collector's sibling module leaks
into another test.
"""
from __future__ import annotations

import importlib.util
import sys
import types
from pathlib import Path

COLLECTORS = Path(__file__).resolve().parents[3] / "collectors"


def load(collector: str, module: str, stand_ins: tuple[str, ...] = ()):
    path = COLLECTORS / collector / f"{module}.py"
    name = f"_kit_test_{collector.replace('-', '_')}_{module}"
    before = set(sys.modules)
    sys.path.insert(0, str(path.parent))
    try:
        for missing in stand_ins:
            if (missing not in sys.modules
                    and importlib.util.find_spec(missing) is None):
                sys.modules[missing] = types.ModuleType(missing)
        spec = importlib.util.spec_from_file_location(name, path)
        mod = importlib.util.module_from_spec(spec)
        sys.modules[name] = mod
        spec.loader.exec_module(mod)
        return mod
    finally:
        sys.path.remove(str(path.parent))
        for added in set(sys.modules) - before:
            if added != name and added.partition(".")[0] != "collectorkit":
                del sys.modules[added]
