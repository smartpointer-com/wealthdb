"""The walk's pure name rules: the account suffix a directory is named
by, the result count a search reports, and the file-safe form of a name
part. The walk in `download.py` drives the page.
"""

from __future__ import annotations

import hashlib
import re


# Account-suffix-only matcher for sanitising filenames / dir names.
# Schwab renders the dropdown right-column as `<U+2026>NNN`
# (Unicode ellipsis followed by the last-3-to-5 digits of the
# account). Match both that form and the ASCII `...NNN` form
# defensively — copy-paste of the live label can normalise either
# way depending on the rendering path.
_SUFFIX_RE = re.compile(r"(?:…|\.{3})(\d{3,5})")

def _suffix_of(label: str) -> str:
    m = _SUFFIX_RE.search(label)
    if not m:
        # Fall back to a stable hash of the label so there is still
        # a per-account directory; not pretty, but never collides.
        return "x" + hashlib.sha256(label.encode("utf-8")).hexdigest()[:8]
    return m.group(1)


# Result-count banner format:
#   "123 document(s) found from 05/19/2016 to 05/19/2026"
# Only the leading integer is read.
_COUNT_RE = re.compile(r"^\s*(\d+)\s*document")


# Filename-safe replacement for the parts embedded in a file name.
_FN_SAFE = re.compile(r"[^A-Za-z0-9._-]+")

def _safe(s: str, fallback: str = "x") -> str:
    out = _FN_SAFE.sub("-", s).strip("-")
    return out or fallback
