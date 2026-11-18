"""Document-center files: the year of a statement row, the PDF inside a
download response, and the name and write of each saved document.
The walk over the document center is in `download.py`.
"""

from __future__ import annotations

import base64
import json
import re
import zlib


# Statement row labels are heterogeneous (monthly: "January 2026 — ...",
# quarterly: "Jan-March 2026 — ...", annual: "Annual 2026 — ..."), so the
# filter anchors on the 4-digit '20YY' year token, which every label form
# contains. Year-precision filtering keeps the per-row download path
# index-stable; finer granularity would need per-label format-specific
# parsing.
_STATEMENT_LABEL_YEAR_RE = re.compile(r"\b(20\d{2})\b")


def _statement_label_year(label: str) -> int | None:
    """Extract the year from a statement-row aria-label, or None if
    no recognisable year token is present (the row is then kept; the
    safer default is to download than to silently skip)."""
    m = _STATEMENT_LABEL_YEAR_RE.search(label)
    return int(m.group(1)) if m else None


def _write_doc_bytes(docs_dir, name, body):
    """Non-clobbering write of ``body`` under ``docs_dir``/``name``."""
    out_path = docs_dir / name
    base, _, ext = name.rpartition(".")
    k = 1
    while out_path.exists():
        out_path = docs_dir / (f"{base}__{k}.{ext}" if ext else f"{name}__{k}")
        k += 1
    out_path.write_bytes(body)
    return out_path


def _pdf_from_docapi_body(body):
    """Extract the PDF bytes from a ``financial-documents/download``
    JSON response. The endpoint returns the PDF as base64 in
    ``document.docDetail.content`` (``contentType: application/pdf``).
    The ``deflated`` flag is unreliable — some payloads are plain
    base64(PDF), so the payload is decoded, used directly if it is
    already a PDF, and only zlib-inflated as a fallback. Returns bytes
    or None."""
    try:
        d = json.loads(body)
    except Exception:
        return None
    content = (d.get("document", {}) or {}).get("docDetail", {}) \
                .get("content")
    if not content:
        return None
    try:
        raw = base64.b64decode(content)
    except Exception:
        return None
    if raw[:4] == b"%PDF":
        return raw
    try:
        inflated = zlib.decompress(raw)
        if inflated[:4] == b"%PDF":
            return inflated
    except Exception:
        pass
    return None


def _is_docapi_download_response(resp):
    """True for the doc center's own ``financial-documents/download``
    POST response.

    **Only the POST counts.** Documents are fetched from a host other
    than the one serving the page they are clicked on, so the browser
    preflights that URL, and the OPTIONS answers 200 with an empty body
    before the real response arrives. Matched on the URL alone the wait
    below returns the preflight, whose empty body decodes to no PDF, and
    every row this serves — tax forms and statements alike — fails as if
    the endpoint had changed shape.
    Browserless, so it is unit-tested."""
    try:
        if "financial-documents/download" not in (resp.url or ""):
            return False
        return (resp.request.method or "").upper() == "POST"
    except Exception:
        return False


def _doc_stem(label, tag):
    """Build a bronze filename stem from a row label. For the
    statements type the stem is forced to begin with ``Statement`` so
    the silver loader's 529 historical path (which globs
    ``Statement*.pdf``) parses the householded Investment Report —
    a combined statement can carry EDUCATION (529) account sections,
    which ``pdf_parsers.parse`` reads. Redundant
    ``Statement`` / ``(pdf)`` tokens in the label are dropped first so
    the name stays readable."""
    base = re.sub(r"\(pdf\)", "", label, flags=re.I)
    if tag == "statements":
        base = re.sub(r"statement", "", base, flags=re.I)
    base = re.sub(r"[^A-Za-z0-9]+", "_", base).strip("_")[:60]
    if tag == "statements":
        return ("Statement_" + base).strip("_") if base else "Statement"
    return base or "doc"
