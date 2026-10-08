"""Every Docker collector's image carries each module its verbs run.

A collector's Dockerfile copies its scripts by name, so a module a script
starts to import is missing from the image until it is listed too. The
wrappers that mount the source tree over `/app` hide the gap; the image on
its own then fails at import. The check reads, per collector:

* the scripts its `entrypoint.sh` runs (`python3 /app/<script>.py`);
* their first-party import closure: the collector's own modules they
  import, at any depth and also inside functions;
* what its Dockerfile `COPY`s into the image.

and asserts the closure is copied. Static only: nothing is built or run.
"""
from __future__ import annotations

import ast
import re
import shlex
from pathlib import Path

import pytest

COLLECTORS = Path(__file__).resolve().parents[3] / "collectors"
if not COLLECTORS.is_dir():
    pytest.skip("not run from a repo checkout", allow_module_level=True)

DOCKER_COLLECTORS = sorted(
    d.name for d in COLLECTORS.iterdir()
    if (d / "Dockerfile").is_file() and (d / "entrypoint.sh").is_file())

_RUN_RE = re.compile(r"\bpython3? /app/([\w./-]+\.py)\b")
_COPY_RE = re.compile(r"^\s*COPY\s", re.IGNORECASE)


def _copied(root: Path) -> set[str]:
    """The collector-relative paths the Dockerfile copies: each COPY
    source, a directory standing for every file under it."""
    text = (root / "Dockerfile").read_text(encoding="utf-8")
    text = text.replace("\\\n", " ")
    paths: set[str] = set()
    for line in text.splitlines():
        if not _COPY_RE.match(line):
            continue
        words = shlex.split(line, comments=True)
        sources = [w for w in words[1:-1] if not w.startswith("--")]
        for src in sources:
            hits = [root] if src.rstrip("/") in ("", ".") else root.glob(src)
            for hit in hits:
                files = hit.rglob("*") if hit.is_dir() else [hit]
                paths |= {str(f.relative_to(root)) for f in files}
    return paths


def _module_file(root: Path, name: str) -> Path | None:
    """The collector's own source file for top-level module `name`."""
    for f in (root / f"{name}.py", root / name / "__init__.py"):
        if f.is_file():
            return f
    return None


def _closure(root: Path, scripts: set[str]) -> set[Path]:
    """`scripts` and every collector module they import, transitively."""
    seen: set[Path] = set()
    todo = [root / s for s in scripts]
    while todo:
        f = todo.pop()
        if f in seen or not f.is_file():
            continue
        seen.add(f)
        for node in ast.walk(ast.parse(f.read_text(encoding="utf-8"))):
            if isinstance(node, ast.Import):
                names = [a.name for a in node.names]
            elif isinstance(node, ast.ImportFrom) and not node.level:
                names = [node.module] if node.module else []
            else:
                continue
            for name in names:
                dep = _module_file(root, name.split(".")[0])
                if dep is not None:
                    todo.append(dep)
    return seen


@pytest.mark.parametrize("name", DOCKER_COLLECTORS)
def test_the_image_ships_every_module_its_verbs_run(name):
    root = COLLECTORS / name
    scripts = set(_RUN_RE.findall(
        (root / "entrypoint.sh").read_text(encoding="utf-8")))
    assert scripts, f"{name}: entrypoint.sh runs no /app script"
    absent = sorted(s for s in scripts if not (root / s).is_file())
    assert not absent, f"{name}: entrypoint.sh runs {absent}, not in the tree"
    copied = _copied(root)
    missing = sorted(str(f.relative_to(root)) for f in _closure(root, scripts)
                     if str(f.relative_to(root)) not in copied)
    assert not missing, f"{name}: Dockerfile does not COPY {missing}"


def test_the_docker_collectors_are_found():
    assert {"fidelity-web", "schwab-web"} <= set(DOCKER_COLLECTORS)
