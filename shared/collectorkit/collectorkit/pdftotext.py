"""Poppler's `pdftotext -layout`: a PDF's text with its printed columns.

`-layout` keeps every character at the column it was printed in, which is
what the fixed-width and column-anchored statement parsers across the fleet
read. `pdftotext` is a binary (poppler-utils), not a pip dependency: a
collector that calls this installs poppler-utils in its image, and one that
fingerprints its parser declares the binary to `srcfp` with
``extra_tools=(("pdftotext", "-v"),)``.

A module of its own, apart from `collectorkit.pdf`'s pypdfium / pdfplumber /
OCR extractors, because `srcfp` fingerprints a parser over the collectorkit
modules it imports: kept apart, an edit to either side re-parses only the
documents of the collectors that actually use it.

Failure is split in two, so each caller can keep its own policy — skip the
file, fall back to metadata, or stop the load: `ToolMissing` means no file
can be read at all, `ExtractionError` that this one could not.
"""
from __future__ import annotations

import subprocess
from pathlib import Path


class ExtractionError(RuntimeError):
    """`pdftotext` could not turn one file into text."""


class ToolMissing(ExtractionError):
    """`pdftotext` (poppler-utils) cannot be run — no file can be read."""


def layout_text(path: Path | str, *, timeout: float | None = None) -> str:
    """One PDF as `pdftotext -layout` text, decoded as UTF-8 (pdftotext's
    output encoding; an undecodable byte is replaced, never fatal).

    Raises ToolMissing when the binary cannot be run, and ExtractionError
    when this file cannot be extracted: a non-zero exit (unreadable,
    encrypted or copy-protected) or `timeout` seconds elapsing."""
    try:
        proc = subprocess.run(["pdftotext", "-layout", str(path), "-"],
                              capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise ExtractionError(f"pdftotext timed out after {timeout:g}s") from exc
    except OSError as exc:
        raise ToolMissing(f"pdftotext (poppler-utils) cannot be run: {exc}") from exc
    if proc.returncode != 0:
        detail = proc.stderr.decode("utf-8", errors="replace").strip()[:200]
        raise ExtractionError(f"pdftotext exited {proc.returncode}: {detail}")
    return proc.stdout.decode("utf-8", errors="replace")
