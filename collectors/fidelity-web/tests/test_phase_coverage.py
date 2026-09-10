"""
Tests for the phase-coverage contract — what a run says it got.

The failure these exist to stop: the activity phase can fail nightly,
run after run, while every one of those runs writes
``"status": "complete"`` and exits 0. The other phases keep landing, so
the source reads current everywhere a person would check, and the hole
growing in the transaction history is invisible.

Three separable guarantees, one per group below:

  * `coverage` is ALWAYS written for a phase that ran, carrying a
    `gaps` list even when empty — the ubs-web convention, so an empty
    list is a positive statement of coverage and not the absence of a
    field;
  * the process EXIT CODE says a phase came back short, because that
    is the only signal an unattended nightly can act on;
  * the run's `status` stays `complete` regardless — the dump is still
    loadable, and downgrading it would hand the phases that DID work
    to prune.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


# ---------------------------------------------------------------- coverage

def test_a_phase_that_covered_everything_reports_an_empty_gap_list():
    cov = download.phase_coverage({
        "activity_results": [
            {"window": ["2026-09-01", "2026-09-30"], "file": "a.csv", "ok": True},
        ],
    })
    assert cov["activity"] == {"complete": True, "gaps": []}


def test_a_failed_window_becomes_a_gap_named_as_a_range():
    cov = download.phase_coverage({
        "activity_results": [
            {"window": ["2026-07-01", "2026-07-30"], "ok": True, "file": "a.csv"},
            {"window": ["2026-07-31", "2026-08-29"], "ok": False,
             "error": "custom-range not applied"},
        ],
    })
    assert cov["activity"]["complete"] is False
    assert cov["activity"]["gaps"] == ["2026-07-31..2026-08-29"]


def test_a_phase_that_was_not_requested_has_no_entry_at_all():
    """'not asked for' must never read as 'asked for and came back
    empty' — that conflation is what let the hole hide."""
    cov = download.phase_coverage({"positions_results": [
        {"view": "summary", "file": "p.csv", "ok": True},
    ]})
    assert "positions" in cov
    assert "activity" not in cov
    assert "documents" not in cov


def test_an_activity_phase_that_attempted_nothing_is_a_gap():
    """The inverse of an empty gap list: the phase ran and covered
    nothing, which the old shape recorded as an empty list that read
    like success."""
    cov = download.phase_coverage(
        {"activity_results": []},
        requested_window=("2026-07-01", "2026-09-10"),
    )
    assert cov["activity"]["complete"] is False
    assert cov["activity"]["gaps"] == ["2026-07-01..2026-09-10"]


def test_a_status_phase_is_judged_on_its_status():
    ok = download.phase_coverage({"balances_results": {
        "status": "explored-no-export", "file": "b.html"}})
    assert ok["balances"]["complete"] is True
    bad = download.phase_coverage({"balances_results": {
        "status": "session-timeout", "landed_url": "https://example.invalid/x"}})
    assert bad["balances"]["complete"] is False
    assert bad["balances"]["gaps"] == ["status=session-timeout"]


def test_a_login_with_no_daf_is_complete_not_short():
    """A phase with nothing to fetch did its job; only a phase that
    tried and failed is a gap."""
    cov = download.phase_coverage({"daf_results": {"status": "no-daf"}})
    assert cov["daf"]["complete"] is True


def test_documents_sub_walks_contribute_their_own_gaps():
    cov = download.phase_coverage({"documents_results": {
        "status": "walked",
        "statements": [{"row_label": "2026", "file": "s.pdf", "ok": True}],
        "tax_forms": [{"ok": False, "error": "tax-forms-nav-not-found"}],
    }})
    assert cov["documents"]["complete"] is False
    assert cov["documents"]["gaps"] == ["tax-forms-nav-not-found"]


# --------------------------------------------------------------- exit code

def test_a_complete_run_exits_zero():
    assert download.exit_code_for_coverage(
        {"activity": {"complete": True, "gaps": []},
         "positions": {"complete": True, "gaps": []}}) == 0


def test_a_short_phase_makes_the_run_exit_non_zero():
    """The whole point: the nightly must be able to see this."""
    code = download.exit_code_for_coverage(
        {"activity": {"complete": False, "gaps": ["2026-07-01..2026-07-30"]},
         "positions": {"complete": True, "gaps": []}})
    assert code == download.EXIT_PHASE_INCOMPLETE
    assert code != 0


def test_the_short_code_is_distinct_from_the_auth_codes():
    """A nightly summary has to tell 'the session died' from 'a phase
    quietly died'; 2 and 3 are already credentials and login."""
    assert download.EXIT_PHASE_INCOMPLETE not in (0, 1, 2, 3)


# ------------------------------------------------------- window arithmetic

def test_windows_are_never_wider_than_the_pages_own_default():
    """A window wider than the page's default filter can come back
    SILENTLY TRUNCATED to that default — the export is generated from
    whatever the table holds, and the filter's label updates before
    its rows do. At or below the default, the worst case is a superset,
    which reloads to a no-op."""
    assert download.MAX_ACTIVITY_WINDOW_DAYS <= 30
    windows = download.make_activity_windows(
        date(2026, 1, 1), date(2026, 12, 31))
    assert windows[0][0] == date(2026, 1, 1)
    assert windows[-1][1] == date(2026, 12, 31)
    for start, end in windows:
        assert (end - start).days + 1 <= download.MAX_ACTIVITY_WINDOW_DAYS
    # contiguous, no overlap and no hole
    for (_, prev_end), (next_start, _) in zip(windows, windows[1:]):
        assert (next_start - prev_end).days == 1


# ------------------------------------------------------- export coverage

def test_an_export_reports_the_rows_and_span_it_actually_holds(tmp_path):
    """A header-only CSV looks exactly like a full one from the
    outside — same path, same ok flag — so the row count and span are
    what make a short export visible."""
    csv = tmp_path / "activity.csv"
    csv.write_text(
        "﻿\n\n"
        "Run Date,Account,Action,Amount ($)\n"
        '09/03/2026,X,BUY,-10.00\n'
        '"09/10/2026",X,SELL,20.00\n'
        "\nSome disclaimer text that is not a row\n",
        encoding="utf-8",
    )
    rows, first, last = download.activity_csv_coverage(csv)
    assert rows == 2
    assert first == date(2026, 9, 3)
    assert last == date(2026, 9, 10)


def test_an_empty_export_reports_no_span_rather_than_raising(tmp_path):
    csv = tmp_path / "activity.csv"
    csv.write_text("Run Date,Account,Action\n", encoding="utf-8")
    assert download.activity_csv_coverage(csv) == (0, None, None)


def test_coverage_of_a_missing_file_never_raises(tmp_path):
    assert download.activity_csv_coverage(tmp_path / "nope.csv") == (0, None, None)


# ----------------------------------------------------- backfill is bounded

TODAY = date(2026, 9, 10)


def test_a_backfill_stays_bounded_when_the_page_publishes_no_bounds():
    """`--lookback all` asks for thirty years. The panel that used to
    publish min/max on its date inputs no longer does, so the probe
    that clamped the request comes back empty as a matter of course —
    and without a floor every year past Fidelity's retention is a
    month of identical empty exports."""
    since, until = download.clamp_activity_window(
        date(1996, 1, 1), TODAY, None, None, TODAY)
    assert until == TODAY
    assert since > date(2019, 1, 1), "no floor was applied"
    windows = download.make_activity_windows(since, until)
    assert len(windows) < 100, (
        f"{len(windows)} windows is a siege, not a backfill"
    )


def test_a_window_inside_the_floor_is_left_alone():
    """The floor is a backstop for `all`, not a rewrite of every
    request — an ordinary nightly window must survive it intact."""
    assert download.clamp_activity_window(
        date(2026, 8, 1), TODAY, None, None, TODAY) == (date(2026, 8, 1), TODAY)


def test_published_bounds_win_over_the_assumed_floor():
    """When the panel does publish min/max, that is the real answer
    and the floor must not widen or narrow it."""
    assert download.clamp_activity_window(
        date(1996, 1, 1), TODAY,
        date(2024, 6, 1), date(2026, 9, 9), TODAY,
    ) == (date(2024, 6, 1), date(2026, 9, 9))


def test_a_request_wholly_outside_the_available_window_inverts():
    """An inverted range is how the clamp says 'nothing to walk';
    scrape_activity reads it and skips the phase."""
    since, until = download.clamp_activity_window(
        date(1996, 1, 1), date(1997, 1, 1),
        date(2024, 6, 1), date(2026, 9, 9), TODAY)
    assert until < since


# ------------------------------------------- an export must be its own window

def test_an_export_holding_its_own_window_is_accepted():
    assert download.activity_export_matches(
        534, date(2026, 7, 1), date(2026, 7, 30),
        date(2026, 7, 1), date(2026, 7, 30)) is True


def test_an_export_holding_the_previous_windows_rows_is_rejected():
    """The table lags the filter by a whole apply: a freshly-applied
    range hands back the PREVIOUS one's rows, repeatably. Nothing on
    the page reports that, so the content is the only honest test."""
    assert download.activity_export_matches(
        336, date(2026, 8, 11), date(2026, 9, 10),
        date(2026, 7, 1), date(2026, 7, 30)) is False


def test_an_export_that_overruns_its_window_is_rejected():
    assert download.activity_export_matches(
        10, date(2026, 7, 1), date(2026, 8, 2),
        date(2026, 7, 1), date(2026, 7, 30)) is False


def test_a_window_with_no_rows_matches_by_default():
    """A genuinely quiet window is legitimate and has nothing to
    place — treating it as stale would retry for ever."""
    assert download.activity_export_matches(
        0, None, None, date(2026, 7, 1), date(2026, 7, 30)) is True


# ------------------------------------------- failure shapes that hid before

def test_a_sub_walk_that_raised_outright_is_a_gap():
    """The envelope's status stays `walked` because the SIBLING
    sub-walk finished, and a walk that raised leaves no attempt list
    to judge — only a `<name>_error`. The documents phase could lose
    half of what it went for and still report complete."""
    cov = download.phase_coverage({"documents_results": {
        "status": "walked",
        "statements": [{"row_label": "2026", "file": "s.pdf", "ok": True}],
        "statements_error": "Timeout 30000ms exceeded",
    }})
    assert cov["documents"]["complete"] is False
    assert cov["documents"]["gaps"] == ["statements: Timeout 30000ms exceeded"]


def test_a_failed_daf_account_is_a_gap():
    """The DAF keys its per-account results by account rather than
    listing them, and each says it failed with a status rather than an
    `ok` flag — two shapes past the list-and-flag reading, so a dead
    account was invisible under an envelope reading `complete`."""
    cov = download.phase_coverage({"daf_results": {
        "status": "complete",
        "accounts": 2,
        "per_account": {
            "acct-a": {"status": "complete", "grants": 3},
            "acct-b": {"status": "error", "error": "poolBalances 500"},
        },
    }})
    assert cov["daf"]["complete"] is False
    assert cov["daf"]["gaps"] == ["poolBalances 500"]


def test_a_daf_whose_accounts_all_landed_is_complete():
    cov = download.phase_coverage({"daf_results": {
        "status": "complete", "accounts": 1,
        "per_account": {"acct-a": {"status": "complete", "grants": 3}},
    }})
    assert cov["daf"] == {"complete": True, "gaps": []}
