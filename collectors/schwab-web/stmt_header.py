"""The header of a Schwab brokerage statement: the period, the
registration label and the full account number from page 1, and the
account value at the period's end.
"""

from __future__ import annotations

import re
from datetime import date

from collectorkit import statement_period
from stmt_tokens import _parse_number


# ============================================================
# Statement-period header
# ============================================================

# 2020+: "February 1-28, 2026" or "January 30-February 5, 2026"
# (Schwab seems to drop spaces around the hyphen sometimes.)
_MONTH = "|".join(statement_period.MONTHS)
_PERIOD_RE = re.compile(
    rf"(?P<m1>{_MONTH})\s*"
    r"(?P<d1>\d{1,2})\s*[\-–]\s*"
    rf"(?:(?P<m2>{_MONTH})\s*)?"
    r"(?P<d2>\d{1,2}),\s*"
    r"(?P<year>\d{4})"
)
# 2017-2019: "Statement Period: December 1, 2019 to December 31, 2019"
# (two full dates separated by " to ").
_PERIOD_RE_LONG = statement_period.month_range_pattern(r"\s+to\s+")

_MONTH_NUMS = statement_period.MONTH_NUMS


# ============================================================
# Account-registration header
# ============================================================
#
# Every Schwab statement names the account registration ("Schwab
# One® Account", "Contributory IRA", "Schwab One® Custodial
# Account", "Education Savings", etc.) at the very top of page
# 1, adjacent to the account number. This is the only Schwab-
# side signal for the per-account tax wrapper — the Trader API
# doesn't surface it (see schwab-api DESIGN.md §4.10).
# wealthdb's gold adapter maps the verbatim string returned
# here to its canonical `tax_wrapper` enum.
#
# Three layout eras to handle, each with a distinct anchor:
#
#   2025+         "<LABEL> of Account Nickname"      (single line)
#   2020-2024     "<LABEL> of"                       (label only;
#                                                    holder name on
#                                                    the next line)
#   2017-2019     bare "<LABEL>" immediately above
#                 "Account Number: <NNNN-NNNN>"      (the colon-and-
#                                                    value form is
#                                                    unique to this
#                                                    era — newer
#                                                    statements put
#                                                    "Account Number"
#                                                    and the value on
#                                                    separate lines)

_REG_RE_2025 = re.compile(
    r"^(?P<reg>[A-Za-z][^\n]*?)\s+of\s+Account\s+Nickname\s*$",
)
_REG_RE_2020 = re.compile(
    r"^(?P<reg>[A-Za-z][^\n]*?)\s+of\s*$",
)
_REG_ACCOUNT_NUMBER_INLINE_RE = re.compile(
    r"^Account\s+Number:\s*\S+",
)
# Substrings that mark a plausible registration label. Used to
# filter out lines that happen to end in " of" (e.g. disclosure
# prose) but aren't the registration header. The known Schwab
# labels, plus the catalogue of registrations the wealthdb adapter
# expects to see — see DESIGN.md §8.
_REG_KEYWORDS = (
    "Account",
    "IRA",
    "ESA",
    "Education Savings",
    "Coverdell",
    "Custodial",
    "Trust",
    "401",
    "529",
    "Annuity",
)
# Schwab decorates some labels with a registered-mark glyph or
# stray whitespace that doesn't carry information. Stripping
# keeps the column values comparable across statements (the
# 2024 statement uses "Schwab One® International Account" while
# the 2026 one uses "Schwab One International® Account" — same
# wrapper, but the ® migrates by one word).
_REG_TRIM_RE = re.compile(r"\s+")


def parse_account_registration(text: str) -> str | None:
    """Extract the account-registration label from the top of
    page 1 of a Schwab statement. Returns the raw label string
    Schwab printed ("Schwab One® International Account",
    "Contributory IRA", "Schwab One® Custodial Account",
    "Education Savings", ...) verbatim — extended with a
    " (UTMA)" / " (UGMA)" suffix for custodial accounts, since
    Schwab's header line "Schwab One® Custodial Account" is
    the same string for both sub-types but the holder block
    immediately below carries a "<state>UTMA" / "<state>UGMA"
    marker that resolves the ambiguity. Silver does this
    refinement so the wealthdb gold adapter doesn't need to
    re-read bronze.

    Returns None when no recognisable header is found; the
    loader leaves the column NULL in that case.
    """
    # Constrain the scan to the top of the document — the
    # registration header always sits in the first ~80 lines on
    # page 1. This also avoids accidental matches against later
    # disclosure prose that happens to end in " of".
    lines = [ln.strip() for ln in text.split("\n")[:80]]

    label = _detect_registration_label(lines)
    if label is None:
        return None
    return _augment_custodial_subtype(label, lines)


def _detect_registration_label(lines: list[str]) -> str | None:
    """Scan the first ~80 lines for the registration header
    line, trying each layout era's anchor in turn. Returns the
    raw label or None."""
    # Era 1 (2025+): "<LABEL> of Account Nickname"
    for ln in lines:
        if not ln:
            continue
        m = _REG_RE_2025.match(ln)
        if m:
            return _clean_registration(m.group("reg"))

    # Era 2 (2020-2024): "<LABEL> of" (the holder name follows
    # on the next line). Many lines on page 1 end in " of" so
    # a registration keyword is additionally required to land
    # on the legitimate header.
    for ln in lines:
        if not ln:
            continue
        m = _REG_RE_2020.match(ln)
        if not m:
            continue
        cand = m.group("reg").strip()
        if any(kw in cand for kw in _REG_KEYWORDS):
            return _clean_registration(cand)

    # Era 3 (2017-2019): bare "<LABEL>" immediately above an
    # inline "Account Number: <value>" line.
    for i, ln in enumerate(lines):
        if not _REG_ACCOUNT_NUMBER_INLINE_RE.match(ln):
            continue
        # Scan backwards up to 5 lines for a plausible
        # registration label.
        for j in range(i - 1, max(-1, i - 5), -1):
            prev = lines[j]
            if prev and any(kw in prev for kw in _REG_KEYWORDS):
                return _clean_registration(prev)
    return None


def _augment_custodial_subtype(label: str, lines: list[str]) -> str:
    """If `label` is the generic Schwab custodial registration,
    promote it to "<label> (UTMA)" / "<label> (UGMA)" using the
    account-holder-block markers Schwab prints in the top-left
    corner of every statement ("<NAME> CUST FOR / <NAME>
    UCAUTMA / UNTIL AGE 18", where "UCAUTMA" = California UTMA,
    "NYUTMA" = New York UTMA, etc.; UGMA accounts use the
    matching "<state>UGMA" marker).

    The header line "Schwab One® Custodial Account of" is the
    same string for UTMA and UGMA accounts, so it's the only
    place in silver that looks BEYOND the header for a
    resolution. Doing it here keeps wealthdb's gold adapter
    out of bronze and gives it a single column to key off.

    If neither marker is present (defensive — no known Schwab
    statement lacks one for a custodial account) or the
    registration isn't custodial, `label` is returned unchanged.
    """
    if "Custodial" not in label:
        return label
    # Substring scan: matches CAUTMA, NYUTMA, UCAUTMA, plain UTMA, etc.
    # UTMA is checked first explicitly; there's no UCAUGMA-substring-
    # of-UCAUTMA collision but the ordering keeps intent obvious.
    for ln in lines:
        u = ln.upper()
        if "UTMA" in u:
            return f"{label} (UTMA)"
        if "UGMA" in u:
            return f"{label} (UGMA)"
    return label


def _clean_registration(s: str) -> str:
    """Collapse internal whitespace runs into single spaces but
    otherwise return the label verbatim — the ® / & / parenthesis
    characters are part of Schwab's official label and the
    wealthdb adapter keys off the exact text."""
    return _REG_TRIM_RE.sub(" ", s).strip()


# ============================================================
# Full account number (page-1 header)
# ============================================================
#
# Every statement prints the full account number ("NNNN-NNNN")
# near the top of page 1, next to the registration header above.
# It is the only place the web feed carries more than the 3-to-5-
# digit UI suffix, which makes it the bridge key between this
# silver and schwab-api's (whose account_external_id is Schwab's
# opaque hashValue) — see INTEROP.md §1. The same two anchor
# shapes as the registration eras:
#
#   2020+         "Account Number" label line, the NNNN-NNNN
#                 value on the following line
#   2017-2019     inline "Account Number: NNNN-NNNN"
#
# The value is returned exactly as printed (dash kept). The gold
# bridge normalises both sides to digits-only when joining
# against schwab-api's undashed accountNumber, so silver stores
# the verbatim form.

_ACCT_NUM_INLINE_RE = re.compile(
    r"^\s*account\s+number\s*:?\s*(?P<num>\d{4}-\d{4})\b",
    re.IGNORECASE,
)
_ACCT_NUM_LABEL_RE = re.compile(
    r"^\s*account\s+number\s*:?\s*$",
    re.IGNORECASE,
)
_ACCT_NUM_VALUE_RE = re.compile(r"^\s*(?P<num>\d{4}-\d{4})\s*$")


def parse_account_number(text: str) -> str | None:
    """Extract the full account number from the top of page 1 of
    a Schwab statement, verbatim ("NNNN-NNNN", dash kept).

    Handles the inline colon form (2017-2019) and the label-and-
    value-on-consecutive-lines form (2020+; one intervening line
    tolerated for layout jitter). Returns None when neither shape
    appears in the first ~80 lines — a header layout the anchors
    don't recognise leaves the account without a bridge key, and
    the gold layer falls back to suffix matching.
    """
    lines = [ln.strip() for ln in text.split("\n")[:80]]
    for i, ln in enumerate(lines):
        m = _ACCT_NUM_INLINE_RE.match(ln)
        if m:
            return m.group("num")
        if _ACCT_NUM_LABEL_RE.match(ln):
            for nxt in lines[i + 1:i + 3]:
                v = _ACCT_NUM_VALUE_RE.match(nxt)
                if v:
                    return v.group("num")
    return None


def parse_statement_period(text: str) -> tuple[date, date] | None:
    """Return (start_date, end_date) for the first period header
    found in `text` (the entire PDF text or page 1 will do).

    Handles both header conventions Schwab has shipped:
    - 2020+ short form: "February 1-28, 2026"
    - 2017-2019 long form: "December 1, 2019 to December 31, 2019"
    """
    m = _PERIOD_RE.search(text)
    if m:
        year = int(m.group("year"))
        m1 = _MONTH_NUMS[m.group("m1")]
        m2 = _MONTH_NUMS[m.group("m2") or m.group("m1")]
        d1 = int(m.group("d1"))
        d2 = int(m.group("d2"))
        return date(year, m1, d1), date(year, m2, d2)
    return statement_period.read_month_range(text, _PERIOD_RE_LONG, strict=True)


# ============================================================
# Account value
# ============================================================

# The account's total value at period end, as the statement prints it:
# "Total Account Value $ 1,234.56" (2017-2024 layouts) or the first
# figure, this period's, of "Ending Account Value $1,234.56 $1,000.00"
# (2025+).
_ACCOUNT_VALUE_RES = (
    re.compile(r"^Total\s+Account\s+Value\s*\$?\s*(\(?[\d,]+\.\d{2}\)?)"),
    re.compile(r"^Ending\s+Account\s+Value\s*\$(\(?[\d,]+\.\d{2}\)?)"),
)


def parse_account_value(text: str) -> float | None:
    """The account's total value at period end, as printed; None when the
    statement prints none. Positions plus closing cash should equal it,
    which is what the loader checks every statement against."""
    for line in text.split("\n"):
        for rx in _ACCOUNT_VALUE_RES:
            m = rx.match(line.strip())
            if m:
                return _parse_number(m.group(1))
    return None
