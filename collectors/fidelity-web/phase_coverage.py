"""Phase coverage: what a run got, beside what it attempted.

The walk in `download.py` records each phase's result in run.json;
these rules read those results into the run's `coverage` block and its
exit code.
"""

from __future__ import annotations

import logging

from collectorkit import cli

# The walk's own logger: a record reads the same whichever module
# writes it.
log = logging.getLogger("fidelity-web.download")


# The run.json key each phase writes its result under. A key is
# present exactly when the phase was requested AND attempted, so the
# presence of a key — not the value of --mode — is what says a phase
# ran.
PHASE_RESULT_KEYS = (
    ("positions", "positions_results"),
    ("activity", "activity_results"),
    ("documents", "documents_results"),
    ("balances", "balances_results"),
    ("performance", "performance_results"),
    ("daf", "daf_results"),
    ("lots", "lots_results"),
)

# For the phases whose result is a status dict rather than a list of
# attempts, the statuses that mean the phase did its job. `no-daf` is
# one of them: a login with no donor-advised fund has nothing to
# fetch, and walk() has already upgraded a no-daf that contradicts the
# enumerated accounts into an error.
PHASE_OK_STATUS = {
    "documents": ("walked",),
    "balances": ("explored-no-export",),
    "performance": ("explored-no-export",),
    "daf": ("complete", "no-daf"),
    "lots": ("complete",),
}

# The fleet's "came back short" status (collectorkit.cli.EXIT_SHORT):
# the dump is loadable and its coverage names the gaps.
EXIT_PHASE_INCOMPLETE = cli.EXIT_SHORT


def _gap_label(entry):
    """A failed attempt, named the way ubs-web names one: a window as
    ``YYYY-MM-DD..YYYY-MM-DD``, anything else by whatever identifies
    it."""
    win = entry.get("window")
    if isinstance(win, (list, tuple)) and len(win) == 2 and all(win):
        return f"{win[0]}..{win[1]}"
    for key in ("view", "row_label", "account"):
        if entry.get(key):
            return str(entry[key])
    return str(entry.get("error") or "unknown")


# A nested attempt reports failure two ways: with an `ok` flag (the
# list phases) or with a status (the DAF's per-account map). The
# statuses that mean failure are enumerated here rather than the ones
# that mean success — at this depth an unfamiliar status is far more
# likely to be a shape this table has not learned than a failure, and
# the top-level allow-list (PHASE_OK_STATUS) already catches a phase
# that went wrong as a whole.
ENTRY_FAIL_STATUS = ("error", "session-timeout")


def _attempt_entries(result):
    """The per-attempt dicts inside a phase result, whatever shape
    that phase uses. A list phase yields itself; a status-dict phase
    yields the dicts nested one level under it — both its sub-lists
    (documents keeps statements / tax_forms under one envelope) and
    its sub-maps (the DAF keys its per-account results by account)."""
    if isinstance(result, list):
        return [e for e in result if isinstance(e, dict)]
    if isinstance(result, dict):
        out = []
        for value in result.values():
            if isinstance(value, list):
                out += [e for e in value if isinstance(e, dict)]
            elif isinstance(value, dict):
                out += [e for e in value.values() if isinstance(e, dict)]
        return out
    return []


def _entry_failed(entry):
    return (not entry.get("ok", True)
            or entry.get("status") in ENTRY_FAIL_STATUS)


def phase_coverage(run_json, requested_window=None):
    """What each attempted phase actually covered.

    One entry per phase the run attempted, ALWAYS carrying a `gaps`
    list — empty included, so a reader can tell a run that covered
    everything from one taken before this field existed. That is
    ubs-web's convention (`csv_gaps` / `mt940_gaps`), applied per
    phase.

    `complete` is the field a caller acts on: True when every attempt
    the phase made succeeded, False when any did not. A phase the run
    never requested has no entry at all, so "not asked for" is never
    confused with "asked for and came back empty".
    """
    coverage = {}
    for name, key in PHASE_RESULT_KEYS:
        if key not in run_json:
            continue                      # phase not requested
        result = run_json[key]
        entries = _attempt_entries(result)
        gaps = [_gap_label(e) for e in entries if _entry_failed(e)]
        complete = not gaps

        # A status-dict phase also has to like its own status.
        if isinstance(result, dict) and name in PHASE_OK_STATUS:
            status = result.get("status")
            if status not in PHASE_OK_STATUS[name]:
                complete = False
                gaps.append(f"status={status}")

        # A sub-walk that raised outright leaves no attempt list to
        # judge — only a `<name>_error` beside an envelope status that
        # is still the successful one, because the sibling sub-walk
        # did finish. Without this the documents phase can lose half
        # of what it went for and report complete.
        if isinstance(result, dict):
            for field, value in result.items():
                if field.endswith("_error") and value:
                    complete = False
                    gaps.append(f"{field[:-len('_error')]}: {value}")

        # An empty ATTEMPT list is the inverse of an empty gap list:
        # the phase ran and covered nothing at all. It is only a gap
        # when something was actually asked for — a window that
        # resolves to no days legitimately yields no attempts.
        if isinstance(result, list) and not entries:
            if name == "activity" and requested_window and all(requested_window):
                complete = False
                gaps.append("{}..{}".format(*requested_window))
            elif name != "activity":
                complete = False
                gaps.append("no attempt recorded")

        coverage[name] = {"complete": complete, "gaps": gaps}
    return coverage


def exit_code_for_coverage(coverage):
    """0 when every attempted phase covered what it set out to, else
    EXIT_PHASE_INCOMPLETE.

    This is the signal the nightly relies on. run.json's own status
    deliberately stays `complete` whatever a phase did (see walk), so
    the dump keeps its artefacts and its loadability; the exit code is
    the separate axis that says a phase came back short, and it is
    emitted only after the manifest has been finalised."""
    short = sorted(name for name, c in (coverage or {}).items()
                   if not c.get("complete"))
    if not short:
        return 0
    log.error(
        "phase(s) came back short: %s — the dump is still loadable and "
        "run.json records the gaps under `coverage`",
        ", ".join(short),
    )
    return EXIT_PHASE_INCOMPLETE
