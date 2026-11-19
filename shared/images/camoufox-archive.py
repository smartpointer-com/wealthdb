#!/usr/bin/env python3
"""Check and unpack the local archive of the pinned Camoufox browser build.

The archive is a gzipped tar of base-camoufox's /opt/camoufox-cache: the
browser build and its add-on, under the one top directory camoufox-cache/.
Its checksum is a SHA-256 over what it holds, not over its bytes: each
entry's path, type and mode, each file's content and each link's target.
Two archives of one tree have one checksum, whatever order tar wrote
them in and however gzip packed them.

  camoufox-archive.py sum ARCHIVE
      print the checksum of ARCHIVE.
  camoufox-archive.py unpack ARCHIVE SUM DEST
      check ARCHIVE against SUM, then unpack its tree to DEST, which must
      not exist. A mismatch unpacks nothing and exits with status 1.

Standard library only, so the image build and the host both run it.
"""
from __future__ import annotations

import hashlib
import os
import posixpath
import sys
import tarfile
import tempfile

TOP = "camoufox-cache"


class ArchiveError(Exception):
    pass


def _entries(archive: str) -> list[tuple[str, str]]:
    """(path, what) for each entry: its type and mode, and a file's
    SHA-256 or a link's target."""
    out: dict[str, str] = {}
    with tarfile.open(archive, "r:gz") as tar:
        for m in tar:
            name = posixpath.normpath(m.name)
            if name != TOP and not name.startswith(TOP + "/"):
                raise ArchiveError(f"{m.name!r} is outside {TOP}/")
            if name in out:
                raise ArchiveError(f"{m.name!r} is in the archive twice")
            if m.isdir():
                what = f"d {m.mode:o}"
            elif m.isfile():
                h = hashlib.sha256()
                with tar.extractfile(m) as fh:
                    for chunk in iter(lambda: fh.read(1 << 20), b""):
                        h.update(chunk)
                what = f"f {m.mode:o} {h.hexdigest()}"
            elif m.issym():
                what = f"l {m.linkname}"
            else:
                raise ArchiveError(f"{m.name!r} is not a file, a directory "
                                   "or a symbolic link")
            out[name] = what
    if TOP not in out:
        raise ArchiveError(f"no {TOP}/ directory")
    return sorted(out.items())


def checksum(archive: str) -> str:
    h = hashlib.sha256()
    for name, what in _entries(archive):
        h.update(f"{name}\0{what}\0".encode())
    return h.hexdigest()


def unpack(archive: str, want: str, dest: str) -> None:
    if not want:
        raise ArchiveError("base-camoufox.Dockerfile pins no checksum for "
                           "this CPU architecture")
    got = checksum(archive)
    if got != want:
        raise ArchiveError(f"its checksum is {got}, and base-camoufox."
                           f"Dockerfile pins {want}")
    if os.path.lexists(dest):
        raise ArchiveError(f"{dest} exists")
    tmp = tempfile.mkdtemp(dir=os.path.dirname(os.path.abspath(dest)))
    with tarfile.open(archive, "r:gz") as tar:
        tar.extractall(tmp, filter="data")
    os.rename(os.path.join(tmp, TOP), dest)
    os.rmdir(tmp)


def main(argv: list[str]) -> int:
    try:
        if len(argv) == 2 and argv[0] == "sum":
            print(checksum(argv[1]))
            return 0
        if len(argv) == 4 and argv[0] == "unpack":
            unpack(argv[1], argv[2], argv[3])
            return 0
    except (ArchiveError, OSError, tarfile.TarError) as exc:
        print(f"camoufox-archive: {argv[1]}: {exc}. Recreate the archive "
              "(make camoufox-archive), or remove it and the build "
              "downloads the browser", file=sys.stderr)
        return 1
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
