"""
Parse the text of a tx-history row's "More" dialog into a flat dict.

The download's detail pass stores each dialog's whole text as
`raw_text` in `more-details.json`. The dialog prints each label on a
line of its own and its value on the next line. Two trade layouts
exist: "Trade Transaction Details" (labels in title case) and
"More Details" (labels in sentence case, a second block that repeats
Quantity and Price). Wire and deposit dialogs carry no labels.

`parse` matches labels without regard to case against one vocabulary,
so both trade layouts give the same keys. Values stay as printed. A
line that is not a known label is never read as one.

Stored rows keep the parse they were loaded with: a later load fills
only a row that holds no detail. A change here that alters stored
values adds an entry to `load.REBUILD_ADVICE`.
"""

from __future__ import annotations

from collectorkit.parse import is_cusip

# The labels read, in lower case. A key is the label with underscores.
LABELS = (
    "trade date", "settle date", "action", "quantity", "price",
    "principal", "commission", "industry fee", "accrued interest",
    "total", "order id", "description",
)
_LABELS = frozenset(LABELS)

# The labels whose value the unlabeled security block follows: the
# settle date in the older layout, the order id in the newer one.
_BEFORE_SECURITY = frozenset(("settle date", "order id"))

_NEWER_TITLE = "more details"


def _label(line: str) -> str | None:
    """The vocabulary label `line` prints, or None."""
    norm = " ".join(line.split()).lower()
    return norm if norm in _LABELS else None


def parse(raw_text: str) -> dict:
    """The fields of one dialog's text. Empty when the text holds no
    known label.

    - Each known label takes the next line as its value, unless that
      line is a known label too.
    - A label printed twice with one value is stored once; with
      different values, every value is kept in order as a list.
    - The unlabeled lines after the settle date or order id value form
      the security block, stored verbatim as `security_lines`. When its
      last line is a valid CUSIP it is also stored as `cusip`.
    - In the newer layout the line after `Print` is the action; it is
      stored as `action` when no `Action` label is printed.
    """
    lines = [s for s in (ln.strip() for ln in (raw_text or "").splitlines())
             if s]
    values: dict[str, list[str]] = {}
    security: list[str] | None = None
    i = 0
    while i < len(lines):
        label = _label(lines[i])
        if label is None or i + 1 >= len(lines) or _label(lines[i + 1]):
            i += 1
            continue
        values.setdefault(label.replace(" ", "_"), []).append(lines[i + 1])
        i += 2
        if label in _BEFORE_SECURITY and security is None:
            j = i
            while j < len(lines) and _label(lines[j]) is None:
                j += 1
            if j > i:
                security = lines[i:j]
            i = j
    if not values:
        return {}

    out: dict = {}
    for key, vals in values.items():
        out[key] = vals[0] if len(set(vals)) == 1 else vals
    if security:
        out["security_lines"] = security
        if is_cusip(security[-1]):
            out["cusip"] = security[-1]
    if ("action" not in out and len(lines) > 2
            and lines[0].lower() == _NEWER_TITLE
            and lines[1].lower() == "print" and _label(lines[2]) is None):
        out["action"] = lines[2]
    return out
