"""Transparent compression for bronze data files.

Bronze trees hold highly compressible text artefacts (CSV exports,
JSON payloads) that are written once at download time and re-read on
every ``load --force`` rebuild. This module gives collectors one
shared, reviewed implementation of the whole lifecycle:

* :func:`compress_file` — compress an artefact in place at download
  time (``trades.csv`` → ``trades.csv.zst``), atomically and with a
  decompress-and-verify pass before the original is removed, so a
  crash at any point leaves either the intact original or a verified
  compressed copy — never neither.
* :func:`compress_best_effort` — the download-side call: compress a
  fresh artefact, and on any failure warn and keep the plain file.
* :func:`resolve_variant` — the loader-side lookup: given the logical
  (uncompressed) path, return whichever variant exists on disk. Plain
  wins over compressed so the original is authoritative whenever both
  coexist (e.g. mid-way through a backlog recompression).
* :func:`open_bytes` / :func:`open_text` — suffix-dispatched readers
  that decompress in memory, for loaders that parse files in Python
  (the SQLite-silver collectors). DuckDB-silver collectors don't need
  them: DuckDB's ``read_csv_auto`` streams ``.csv.zst`` / ``.csv.gz``
  natively, so handing it the resolved path is enough.

zstd is the preferred codec: it beats gzip on ratio at every speed
point and DuckDB reads it natively. It needs the ``zstandard``
package, which is deliberately NOT a collectorkit install dependency —
host-side verbs (``prune``) import collectorkit with no third-party
packages, via ``PYTHONPATH``. The import is lazy: collectors that
compress add ``zstandard`` to their own requirements; everything else
never notices. The gzip paths are stdlib and always available.

Compression level: the writer default is high (:data:`DEFAULT_LEVEL`)
because bronze artefacts are written once, read many times, and
retained forever — and the read side is level-independent. At bronze
file sizes (single-digit MB) even the highest normal zstd level costs
well under a second per nightly run.
"""
from __future__ import annotations

import gzip
import io
import logging
import os
from pathlib import Path
from typing import BinaryIO

from collectorkit import bronze

log = logging.getLogger("collectorkit.compress")

# Compressed-variant suffixes appended to the logical filename
# (`trades.csv` → `trades.csv.zst`). Order = resolution preference
# among compressed forms; the plain file always wins over both.
ZSTD_SUFFIX = ".zst"
GZIP_SUFFIX = ".gz"
VARIANT_SUFFIXES = (ZSTD_SUFFIX, GZIP_SUFFIX)

# zstd 19 is the strongest level that needs no `--ultra`-style opt-in
# on the decode side; on CSV-shaped bronze it compresses to a fraction
# of gzip's best output for an immaterial one-time write cost.
DEFAULT_LEVEL = 19

_CHUNK = 1 << 20


def _zstandard():
    """Import ``zstandard`` lazily with an actionable failure message.

    Lazy so that ``import collectorkit`` (which eagerly imports every
    submodule) keeps working in dependency-free host-side contexts;
    only code paths that actually compress/decompress zstd pay the
    import.
    """
    try:
        import zstandard
    except ImportError as exc:  # pragma: no cover - trivial re-raise
        raise ImportError(
            "collectorkit.compress needs the 'zstandard' package for "
            ".zst files — add `zstandard` to the collector's "
            "requirements.txt (gzip paths work without it)"
        ) from exc
    return zstandard


def resolve_variant(path: Path) -> Path | None:
    """Return the on-disk variant of a logical bronze path.

    ``path`` is the logical, uncompressed name (``…/trades.csv``).
    Tries the plain file first, then ``.zst``, then ``.gz``; returns
    the first regular file found, or ``None`` when no variant exists.
    Plain-first means the original stays authoritative if a compressed
    copy and the original ever coexist (a recompression interrupted
    between verify and unlink).
    """
    path = Path(path)
    for candidate in (path, *(path.with_name(path.name + s)
                              for s in VARIANT_SUFFIXES)):
        if candidate.is_file():
            return candidate
    return None


def open_bytes(path: Path) -> BinaryIO:
    """Open ``path`` for reading, transparently decompressing by
    suffix (``.zst`` / ``.gz``; anything else is read verbatim).
    Closing the returned stream closes the underlying file."""
    path = Path(path)
    if path.name.endswith(ZSTD_SUFFIX):
        zstandard = _zstandard()
        fh = path.open("rb")
        return zstandard.ZstdDecompressor().stream_reader(fh, closefd=True)
    if path.name.endswith(GZIP_SUFFIX):
        return gzip.open(path, "rb")
    return path.open("rb")


def open_text(path: Path, encoding: str = "utf-8",
              newline: str = "") -> io.TextIOWrapper:
    """Text-mode :func:`open_bytes` (``newline=""`` suits ``csv``)."""
    return io.TextIOWrapper(io.BufferedReader(open_bytes(path)),
                            encoding=encoding, newline=newline)


def decompressed_sha256(path: Path) -> tuple[str, int]:
    """``(hex sha256, byte size)`` of ``path``'s decompressed content
    (of its raw content for a plain file). The verification primitive:
    a compressed variant is equivalent to its original iff their
    digests match."""
    import hashlib

    h = hashlib.sha256()
    size = 0
    with open_bytes(path) as fh:
        while True:
            chunk = fh.read(_CHUNK)
            if not chunk:
                break
            h.update(chunk)
            size += len(chunk)
    return h.hexdigest(), size


def read_text_and_sha(path: Path, encoding: str = "utf-8",
                      newline: str = "") -> tuple[str, str, int]:
    """Decompress/read ``path`` once, returning
    ``(text, hex sha256, byte size)`` of its decompressed content.

    Collapses the common loader two-pass — :func:`decompressed_sha256` to
    stamp a source digest, then :func:`open_text` to parse — into a single
    decompression. The sha256 and size are computed over the *decompressed*
    bytes, so they equal :func:`decompressed_sha256` for the same artefact;
    ``text`` equals what :func:`open_text` would read, decoded with
    ``encoding`` (``newline=""`` suits ``csv``).

    Intended use in a loader, replacing a hash pass plus a read pass::

        text, src_sha, _ = compress.read_text_and_sha(
            csv_path, encoding="utf-8-sig")
        # store src_sha as the provenance column; parse text in-process
        rows = csv.reader(io.StringIO(text, newline=""))
    """
    import hashlib

    h = hashlib.sha256()
    size = 0
    raw = bytearray()
    with open_bytes(path) as fh:
        while True:
            chunk = fh.read(_CHUNK)
            if not chunk:
                break
            h.update(chunk)
            size += len(chunk)
            raw += chunk
    # Decode through the same TextIOWrapper machinery as open_text so the
    # text is byte-for-byte what a separate open_text().read() would yield
    # (identical encoding and newline handling), without a second read.
    text = io.TextIOWrapper(io.BytesIO(bytes(raw)),
                            encoding=encoding, newline=newline).read()
    return text, h.hexdigest(), size


def compress_file(path: Path, *, level: int = DEFAULT_LEVEL,
                  remove_original: bool = True,
                  preserve_mtime: bool = True) -> Path:
    """Compress ``path`` to ``<path>.zst`` and (by default) remove the
    original. Returns the compressed path.

    Failure-safety contract, in order:

    1. The frame is written to a sibling ``.tmp`` and renamed into
       place only when complete — no reader ever sees a partial file.
    2. Before the original is unlinked, the *renamed* compressed copy
       is decompressed and its sha256 compared against the original.
       Only a verified copy costs the original its place on disk; on
       any failure the original survives untouched and the temp/final
       artefacts of this attempt are cleaned up.
    3. With ``preserve_mtime`` the original's mtime is carried over,
       keeping "when was this downloaded" visible on the artefact.

    An existing ``<path>.zst`` is overwritten (the rename is atomic),
    which makes re-running after an interrupted attempt idempotent.
    """
    path = Path(path)
    zstandard = _zstandard()
    final = path.with_name(path.name + ZSTD_SUFFIX)
    tmp = final.with_name(final.name + ".tmp")

    orig_digest, _ = bronze.sha256_file(path)
    st = path.stat()
    cctx = zstandard.ZstdCompressor(level=level)
    try:
        with path.open("rb") as src, tmp.open("wb") as dst:
            cctx.copy_stream(src, dst, read_size=_CHUNK)
        if preserve_mtime:
            os.utime(tmp, (st.st_atime, st.st_mtime))
        tmp.replace(final)
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise

    comp_digest, _ = decompressed_sha256(final)
    if comp_digest != orig_digest:
        final.unlink(missing_ok=True)
        raise RuntimeError(
            f"round-trip verification failed for {final.name}: "
            "decompressed sha256 does not match the original "
            "(original kept)"
        )
    if remove_original:
        path.unlink()
    return final


def compress_best_effort(path: Path, log: logging.Logger) -> Path:
    """Compress a freshly written bronze artefact in place with
    :func:`compress_file` and return the path now on disk.

    A failure (disk full, the codec missing) is a warning on ``log``, and
    the plain file stays: a loader resolves either form
    (:func:`resolve_variant`), and :func:`compress_file` removes the
    original only once the compressed copy is verified, so no failure loses
    the artefact.
    """
    path = Path(path)
    try:
        final = compress_file(path)
    except Exception as exc:  # noqa: BLE001 — best-effort by design
        log.warning("could not compress %s (%s); keeping the plain file",
                    path.name, exc)
        return path
    log.info("compressed %s → %s (%d bytes)", path.name, final.name,
             final.stat().st_size)
    return final
