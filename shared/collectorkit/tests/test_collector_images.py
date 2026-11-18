"""Every Docker collector runs the checkout's code, never its image's.

Each Docker collector's wrapper mounts the checkout over `/app`
(`HAS_APP_MOUNT=1`), so its Dockerfile copies `requirements.txt` alone:
the wrapper hashes only the Dockerfile and that file to decide on a
rebuild, and anything else copied would go stale. Static only: nothing
is built or run.
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

MOUNTED = sorted(
    n for n in DOCKER_COLLECTORS
    if re.search(r"^HAS_APP_MOUNT=1\b",
                 (COLLECTORS / n / n).read_text(encoding="utf-8"), re.M))

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


@pytest.mark.parametrize("name", MOUNTED)
def test_a_mounted_image_copies_requirements_alone(name):
    assert _copied(COLLECTORS / name) == {"requirements.txt"}, (
        f"{name}: a HAS_APP_MOUNT image copies requirements.txt alone")


def test_every_docker_collector_mounts_its_source():
    assert {"fidelity-web", "schwab-web"} <= set(DOCKER_COLLECTORS)
    assert MOUNTED == DOCKER_COLLECTORS
