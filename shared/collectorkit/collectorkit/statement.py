"""A small vocabulary for reading a card statement and checking it.

A card statement prints a summary block (labels with amounts), a period,
and activity rows under section headings. A parse is trusted only when the
document's own arithmetic holds: the summary adds up to its new balance,
and each section's rows add up to the figure the summary prints for it.

The pieces:

* :func:`labelled_amounts` finds the amount printed after each label;
* :func:`row_date` gives a printed MM/DD its year;
* :func:`identity_problems` checks one sum against its stated total;
* :func:`section_problems` checks each section's rows against the summary.

A problem list is empty when the check holds. Its messages name the check
and the figures by field, never an amount.

Each parser keeps its own tables, row patterns and number readers; these
functions take them as arguments.
"""
from __future__ import annotations

import re
from collections.abc import Iterable, Mapping
from datetime import date
from decimal import Decimal


def labelled_amounts(blob: str, labels: Mapping[str, str],
                     amount_pattern: str) -> dict[str, str | None]:
    """The amount printed after each label in ``blob``, as matched text.

    ``labels`` maps a field name to its printed label. Each label is
    searched on its own, over the whole blob, for the label, whitespace,
    then ``amount_pattern``; the first such place wins. A label followed
    by anything else gives None. The caller turns the text into a number
    with its own reader."""
    out: dict[str, str | None] = {}
    for name, label in labels.items():
        m = re.search(re.escape(label) + r"\s+(" + amount_pattern + r")", blob)
        out[name] = m.group(1) if m else None
    return out


def row_date(mm: int, dd: int, start: date, end: date) -> date | None:
    """The date of a row printed as MM/DD on a statement for the period
    ``start`` to ``end``, or None when MM/DD is no date in any candidate
    year.

    The candidate years are the year before the start, the start's and the
    end's. A date inside the period wins outright; otherwise the one
    closest to either end of it, and an equal distance goes to the earlier
    year. A printed transaction date can fall a few days before its
    period, and a December row sits on a January statement; one rule
    reads both."""
    best: tuple[int, date] | None = None
    for y in sorted({start.year - 1, start.year, end.year}):
        try:
            d = date(y, mm, dd)
        except ValueError:
            continue                            # 02/29 in a non-leap year
        dist = 0 if start <= d <= end else \
            min(abs((d - start).days), abs((d - end).days))
        if best is None or dist < best[0]:
            best = (dist, d)
    return best[1] if best else None


def identity_problems(name: str, terms: Iterable[Decimal | None],
                      total: Decimal | None) -> list[str]:
    """Whether ``terms`` add up to ``total``, as a problem list.

    The terms are summed in order, with their printed signs. A missing
    term or total is a problem: a figure that did not parse cannot agree
    with anything."""
    terms = list(terms)
    if total is None or any(t is None for t in terms):
        return [f"{name}: a figure is missing"]
    if sum(terms, Decimal(0)) != total:
        return [f"{name}: does not add up"]
    return []


def section_problems(rows, kind_to_field: Mapping[str, str],
                     stated: Mapping[str, Decimal | None],
                     zero_fields: Iterable[str] = ()) -> list[str]:
    """Whether each section's rows add up to its stated figure.

    ``rows`` carry ``kind`` and ``amount``. ``kind_to_field`` maps a row's
    kind to the summary field its section sums to; kinds that share a field
    are summed together. ``stated`` holds the summary's figures by field.
    ``zero_fields`` are figures that must be zero: summary addends no
    section maps to, so a section this parser does not read cannot pass.

    Problems: a row whose kind has no field; a stated figure that is
    missing; a field whose rows do not sum to it; a zero field that is not
    zero. Checked by section rather than by one total, since rows under an
    unknown heading inherit the section above and leave the total
    unchanged."""
    problems: list[str] = []
    sums: dict[str, Decimal] = {}
    for row in rows:
        field = kind_to_field.get(row.kind)
        if field is None:
            problems.append(f"a row of kind {row.kind!r} has no summary figure")
            continue
        sums[field] = sums.get(field, Decimal(0)) + row.amount
    for field in dict.fromkeys(kind_to_field.values()):
        figure = stated.get(field)
        if figure is None:
            problems.append(f"{field}: the figure is missing")
        elif sums.get(field, Decimal(0)) != figure:
            problems.append(f"{field}: its rows do not add up to it")
    for field in zero_fields:
        figure = stated.get(field)
        if figure is None:
            problems.append(f"{field}: the figure is missing")
        elif figure != 0:
            problems.append(f"{field}: not zero, and no section reads it")
    return problems
