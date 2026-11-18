"""Every Docker collector runs the checkout's code, never its image's.

Every Docker wrapper mounts the checkout over `/app`, so a Dockerfile
copies `requirements.txt` alone: the wrapper hashes only the Dockerfile
and that file to decide on a rebuild, and anything else copied would go
stale. Static only: nothing is built or run.
"""
from __future__ import annotations

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


def test_the_docker_collectors_are_found():
    assert {"fidelity-web", "schwab-web"} <= set(DOCKER_COLLECTORS)


@pytest.mark.parametrize("name", DOCKER_COLLECTORS)
def test_an_image_copies_requirements_alone(name):
    assert _copied(COLLECTORS / name) == {"requirements.txt"}, (
        f"{name}: the image copies requirements.txt alone")
