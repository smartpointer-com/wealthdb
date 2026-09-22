"""Unit tests for stitch — one account's listings against its live history,
all cut from one synthetic ledger (listing_fixtures.Ledger) so they agree
unless a test makes them disagree. Covers ownership by the download windows,
precedence between extracts, disagreement, continuity at every join, islands
and bridges, holes between downloads, and days handed over to live."""
from __future__ import annotations

import itertools
import sys
from collections import Counter
from datetime import date, datetime
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import listing_parser as lp  # noqa: E402
import stitch  # noqa: E402
from listing_fixtures import DAY, Ledger, ledger, render  # noqa: E402

D = date
L = ledger()                                  # 2024-01-01 .. 2024-06-28


def _owned(outcome):
    return [(a, b) for a, b in outcome.owned]


def test_a_listing_owns_what_no_download_certified():
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    out = stitch.stitch(live, [L.candidate("A", D(2024, 1, 1), D(2024, 6, 30))])["A"]
    # up to the live window, plus the download's own (partial, empty) day
    assert out.status == "accepted"
    assert _owned(out) == [(D(2024, 1, 1), D(2024, 3, 31)), (D(2024, 6, 29), D(2024, 6, 29))]


def test_the_newer_extract_owns_and_the_older_only_confirms():
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    older = L.candidate("older", D(2024, 1, 1), D(2024, 5, 1))
    newer = L.candidate("newer", D(2024, 1, 1), D(2024, 6, 30))
    out = stitch.stitch(live, [older, newer])
    assert out["newer"].status == out["older"].status == "accepted"
    assert _owned(out["newer"])[0] == (D(2024, 1, 1), D(2024, 3, 31))
    assert out["older"].owned == []


def test_a_listing_without_booking_text_ranks_below_one_with_it():
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    bare = L.candidate("bare", D(2024, 1, 1), D(2024, 6, 30), hour=18, show_text=False)
    full = L.candidate("full", D(2024, 1, 1), D(2024, 6, 30), hour=9)
    out = stitch.stitch(live, [bare, full])
    assert out["full"].owned and out["bare"].owned == []


def test_overlapping_listings_that_agree_share_the_span():
    early = L.candidate("early", D(2024, 1, 1), D(2024, 4, 1))
    late = L.candidate("late", D(2024, 3, 1), D(2024, 6, 29))
    out = stitch.stitch(stitch.LiveView(), [early, late])
    assert _owned(out["late"]) == [(D(2024, 3, 1), D(2024, 6, 28))]      # seeds
    assert _owned(out["early"]) == [(D(2024, 1, 1), D(2024, 2, 29))]


def test_a_listing_that_disagrees_on_one_day_is_rejected_whole():
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    changed = dict(L.postings)
    changed[D(2024, 2, 3)] = [-1_001] + changed[D(2024, 2, 3)][1:]
    other = Ledger(L.start, L.opening, changed)
    good = L.candidate("good", D(2024, 1, 1), D(2024, 6, 30))
    bad = other.candidate("bad", D(2024, 1, 1), D(2024, 5, 1))
    out = stitch.stitch(live, [good, bad])
    assert out["good"].status == "accepted"
    assert out["bad"].status == "rejected"
    assert "differ from good" in out["bad"].reason


def test_adjacent_listings_join_on_a_continuous_balance():
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    a = L.candidate("a", D(2024, 1, 1), D(2024, 2, 1))
    b = L.candidate("b", D(2024, 2, 1), D(2024, 4, 1))
    out = stitch.stitch(live, [a, b])
    assert _owned(out["b"]) == [(D(2024, 2, 1), D(2024, 3, 31))]
    assert _owned(out["a"]) == [(D(2024, 1, 1), D(2024, 1, 31))]


def test_a_one_cent_break_at_the_join_is_rejected():
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    postings = L.between(D(2024, 1, 1), D(2024, 1, 31))
    text = render(postings, opening=L.balance_at(D(2023, 12, 31)) + 1,
                  coverage_start=D(2024, 1, 1),
                  printed=datetime(2024, 2, 1, 9))
    a = stitch.Candidate("a", "a", lp.parse_listing(text))
    b = L.candidate("b", D(2024, 2, 1), D(2024, 4, 1))
    out = stitch.stitch(live, [a, b])
    assert out["b"].status == "accepted"
    assert out["a"].status == "rejected"
    assert "2024-01-31" in out["a"].reason      # the join: end of a's last day


def test_an_island_names_its_gap_and_a_bridge_connects_it_in_any_order():
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    island = L.candidate("island", D(2024, 1, 1), D(2024, 2, 1))
    out = stitch.stitch(live, [island])["island"]
    assert out.status == "rejected"
    assert out.reason.endswith("nothing covers 2024-02-01 to 2024-03-31")
    bridge = L.candidate("bridge", D(2024, 2, 1), D(2024, 4, 1))
    for order in itertools.permutations([island, bridge]):
        out = stitch.stitch(live, list(order))
        assert out["island"].status == out["bridge"].status == "accepted"


def test_a_hole_between_downloads_is_filled_and_checked_at_both_edges():
    live = L.live([(D(2024, 1, 1), D(2024, 2, 1)), (D(2024, 4, 1), D(2024, 6, 29))])
    out = stitch.stitch(live, [L.candidate("A", D(2024, 1, 15), D(2024, 4, 15))])["A"]
    assert _owned(out) == [(D(2024, 2, 1), D(2024, 3, 31))]
    # a listing whose balance runs one cent off inside the hole disagrees
    postings = L.between(D(2024, 1, 15), D(2024, 4, 14))
    off = [p if p.day != D(2024, 3, 1) else type(p)(p.day, p.amount + 1) for p in postings]
    text = render(off, opening=L.balance_at(D(2024, 1, 14)), coverage_start=D(2024, 1, 15),
                  printed=datetime(2024, 4, 15, 9))
    out = stitch.stitch(live, [stitch.Candidate("B", "B", lp.parse_listing(text))])["B"]
    assert out.status == "rejected"


def test_a_deeper_download_takes_days_over():
    cand = L.candidate("A", D(2024, 1, 1), D(2024, 6, 30))
    shallow = stitch.stitch(L.live([(D(2024, 4, 1), D(2024, 6, 29))]), [cand])["A"]
    deep = stitch.stitch(L.live([(D(2024, 2, 1), D(2024, 6, 29))]), [cand])["A"]
    assert _owned(shallow)[0] == (D(2024, 1, 1), D(2024, 3, 31))
    assert _owned(deep)[0] == (D(2024, 1, 1), D(2024, 1, 31))


def test_a_listing_printed_after_the_last_download_owns_the_tail_until_the_next():
    cand = L.candidate("A", D(2024, 1, 1), D(2024, 6, 30))
    before = stitch.stitch(L.live([(D(2024, 4, 1), D(2024, 6, 1))]), [cand])["A"]
    assert _owned(before)[-1] == (D(2024, 6, 1), D(2024, 6, 29))
    after = stitch.stitch(L.live([(D(2024, 4, 1), D(2024, 6, 1)),
                                  (D(2024, 5, 1), D(2024, 6, 29))]), [cand])["A"]
    assert _owned(after)[-1] == (D(2024, 6, 29), D(2024, 6, 29))


def _with_extra_live_posting(live: stitch.LiveView, day: date, cents: int) -> stitch.LiveView:
    postings = dict(live.postings)
    postings[day] = postings.get(day, Counter()) + Counter({cents: 1})
    return stitch.LiveView(live.txn_windows, live.balance_windows, postings, live.balances)


def test_partial_download_days_only_require_containment():
    run_day = D(2024, 4, 6)                    # a posting day in the ledger
    cand = L.candidate("A", D(2024, 1, 1), D(2024, 6, 30))
    live = L.live([(D(2024, 3, 1), run_day)])
    # live saw nothing on its own day; the listing's postings there stand
    assert stitch.stitch(live, [cand])["A"].status == "accepted"
    # live saw a posting on its own day the listing does not have
    live = _with_extra_live_posting(live, run_day, -999)
    out = stitch.stitch(live, [cand])["A"]
    assert out.status == "rejected" and "2024-04-06" in out.reason


def test_without_live_history_the_first_listing_seeds_the_ledger():
    out = stitch.stitch(stitch.LiveView(), [L.candidate("A", D(2024, 1, 1), D(2024, 3, 1))])["A"]
    assert _owned(out) == [(D(2024, 1, 1), D(2024, 2, 29))]


def test_live_balance_on_the_day_before_a_window_comes_from_its_first_day():
    # the balance series opens on the window's first day (no lead day), so
    # the seam balance is that day's balance less its postings
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))], lead=0)
    assert live.closing(D(2024, 3, 31)) == L.balance_at(D(2024, 3, 31))
    out = stitch.stitch(live, [L.candidate("A", D(2024, 1, 1), D(2024, 6, 30))])["A"]
    assert out.status == "accepted"


def test_days_and_ranges():
    days = [D(2024, 1, 1) + i * DAY for i in (0, 1, 2, 5, 6)]
    assert stitch.day_ranges(days) == [(D(2024, 1, 1), D(2024, 1, 3)), (D(2024, 1, 6), D(2024, 1, 7))]
    assert list(stitch.days_between(D(2024, 1, 30), D(2024, 2, 1))) == [
        D(2024, 1, 30), D(2024, 1, 31), D(2024, 2, 1)]


def test_precedence_decides_who_speaks_even_across_passes():
    # `bare` (no booking text) connects first and takes Feb–Mar; `full` only
    # connects through it, in the next pass — yet speaks for what it covers.
    live = L.live([(D(2024, 4, 1), D(2024, 6, 29))])
    full = L.candidate("full", D(2024, 1, 1), D(2024, 2, 16))
    bare = L.candidate("bare", D(2024, 2, 1), D(2024, 4, 5), show_text=False)
    out = stitch.stitch(live, [full, bare])
    assert out["full"].status == out["bare"].status == "accepted"
    assert _owned(out["full"]) == [(D(2024, 1, 1), D(2024, 2, 15))]
    assert _owned(out["bare"]) == [(D(2024, 2, 16), D(2024, 3, 31))]


def _partly_seen():
    postings = dict(L.postings)
    postings[D(2024, 1, 31)] = [-1_000, -500]                # morning, afternoon
    return Ledger(L.start, L.opening, postings)


def test_a_listing_speaks_for_a_day_a_download_saw_only_partly():
    split = _partly_seen()
    run1 = split.live([(D(2024, 1, 1), D(2024, 1, 31))])
    run1 = _with_extra_live_posting(run1, D(2024, 1, 31), -1_000)   # the morning
    for second_start in (D(2024, 2, 1), D(2024, 4, 1)):           # adjacent, or a gap
        run2 = split.live([(second_start, D(2024, 6, 29))])
        live = stitch.LiveView(run1.txn_windows + run2.txn_windows,
                               run1.balance_windows + run2.balance_windows,
                               {**run2.postings, **run1.postings},
                               {**run1.balances, **run2.balances})
        out = stitch.stitch(live, [split.candidate("A", D(2024, 1, 1), D(2024, 6, 30))])["A"]
        assert out.status == "accepted", out.reason
        assert _owned(out)[0] == (D(2024, 1, 31), second_start - DAY)


def test_a_seed_must_agree_with_what_live_saw():
    live = stitch.LiveView(postings={D(2024, 1, 3): Counter({-999: 1})})
    out = stitch.stitch(live, [L.candidate("A", D(2024, 1, 1), D(2024, 3, 1)),
                               L.candidate("B", D(2024, 1, 1), D(2024, 2, 1))])
    assert out["A"].status == out["B"].status == "rejected"
    assert "2024-01-03" in out["A"].reason
