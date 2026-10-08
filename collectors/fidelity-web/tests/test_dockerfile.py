"""The image carries every module its scripts import.

The Dockerfile copies the scripts by name, so a module a script starts
to import is missing from the image until it is listed too. The wrapper
mounts the source tree over `/app`, which hides the gap there; the
image on its own then fails at import.
"""

from __future__ import annotations

import ast
import re
from pathlib import Path

HERE = Path(__file__).resolve().parent.parent


def _copied_modules():
    """The `.py` files the Dockerfile copies into the image."""
    text = (HERE / "Dockerfile").read_text(encoding="utf-8")
    return {name for line in re.findall(r"^COPY (.+)$", text, re.MULTILINE)
            for name in line.split() if name.endswith(".py")}


def _first_party_imports(path):
    """The collector's own modules ``path`` imports, anywhere in it."""
    names = set()
    for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
        if isinstance(node, ast.Import):
            names |= {a.name.split(".")[0] for a in node.names}
        elif isinstance(node, ast.ImportFrom) and node.module and not node.level:
            names.add(node.module.split(".")[0])
    return {f"{n}.py" for n in names if (HERE / f"{n}.py").is_file()}


def test_every_imported_module_is_copied_into_the_image():
    copied = _copied_modules()
    assert "download.py" in copied and "load.py" in copied
    missing = {f"{name} imports {dep}"
               for name in sorted(copied)
               for dep in _first_party_imports(HERE / name)
               if dep not in copied}
    assert not missing
