"""The Donor-Advised Fund phase's pure rules: whether the enumerated
accounts hold a DAF, the year span its history is read over, its
establish date, and the file stem of each of its documents. The phase
itself is `download.scrape_daf`.
"""

from __future__ import annotations

import re
from datetime import datetime, timezone


def _login_has_daf(dimensions):
    """True when the account selector enumerated a Fidelity Charitable
    Donor-Advised Fund. Primary signal is the ``Fidelity Charitable®
    Giving`` section label (matched loosely on 'charitable'); the
    fallback is the DAF's tell-tale non-9-digit account id (the retail
    accounts are all 9-digit — §3.1). Used to decide whether ``--mode
    all`` takes the DAF SSO hop, so a retail-only login skips it."""
    for d in dimensions:
        label = (d.get("portfolio") or "")
        if "charitable" in label.lower():
            return True
        aid = d.get("account_id") or ""
        if aid and len(aid) != 9:
            return True
    return False


def _daf_doc_stem(row, dtype):
    """Readable bronze filename stem for a DAF document row, prefixed by
    type so a directory listing sorts by kind. PII (account/document
    ids) stays out of the name — the date + type is enough to
    disambiguate, and the canonical mapping lives inside the PDF."""
    date = (row.get("correspondenceDate") or row.get("generatedDate")
            or "")[:10]
    base = re.sub(r"[^A-Za-z0-9]+", "_",
                  f"{dtype}_{date}").strip("_")[:60]
    return base or dtype


def _daf_year_span(since_date, establish_date):
    """Inclusive list of calendar years to sweep for the year-scoped
    endpoints (poolExchange, adjustments). Runs from the later of the
    requested window start and the account's establish date up to the
    current year — so a since-inception default covers the account's
    whole life without probing years before it existed."""
    today = datetime.now(timezone.utc).date()
    start_year = since_date.year if since_date else today.year
    if establish_date is not None:
        start_year = max(start_year, establish_date.year) \
            if since_date else establish_date.year
    return list(range(start_year, today.year + 1))


def _daf_parse_establish_date(account_master):
    """Pull the account establish date (YYYY-MM-DD or MM/DD/YYYY) from
    the givingAccounts master, returning a date or None. Bounds the
    year-scoped sweeps and the document-listing window."""
    raw = (account_master or {}).get("establishDate") \
        or (account_master or {}).get("establishmentDate")
    if not raw:
        return None
    for fmt in ("%Y-%m-%d", "%m/%d/%Y", "%Y-%m-%dT%H:%M:%S"):
        try:
            return datetime.strptime(raw[:19], fmt).date()
        except (ValueError, TypeError):
            continue
    return None
