"""load.write for the facts of schema v2 and v3: a fund's rate
sensitivities go with its fund-day, its characteristics are replaced per
provider, a company's figure for a period replaces the same provider's,
and a dividend event the same provider's same event; a complete answer
also drops the events it no longer states."""

from datetime import date

import load
from providers.base import Parsed, row
from tests.helpers import silver_db

DAY = date(2026, 6, 30)
REF = "isin:XX0000000001"


def fetch(conn, q):
    return [tuple(r) for r in conn.execute(q)]


def composition(**payload):
    return row("compositions", fund_ref=REF, as_of=DAY, basis="holdings", provider="sec-nport",
               source_kind="filing", line_count=1, payload=payload)


def risk(tenor, dv01):
    return row("fund_risk", fund_ref=REF, as_of=DAY, currency="USD", tenor=tenor, dv01=dv01)


def characteristic(provider, field, value, label=""):
    return row("characteristics", fund_ref=REF, as_of=DAY, provider=provider, field=field, label=label,
               value=value)


def fact(value, filed):
    """A company's diluted earnings per share for 2026's second quarter,
    as one filing states it; a frame's states no filing day."""
    return row("period_facts", security_key="symbol:EXMP", field="eps_diluted", as_of=DAY,
               period_start=date(2026, 4, 1), provider="sec-edgar", value=value,
               scheme="us-gaap:EarningsPerShareDiluted", unit="USD/shares", fiscal_year=2026,
               fiscal_period="Q2", form="10-Q", filed=filed)


def test_a_line_keeps_its_debt_flags_and_figures(tmp_path):
    conn = silver_db(tmp_path)
    line = row("lines", fund_ref=REF, as_of=DAY, basis="holdings", line_no=1, name="EXAMPLE TREASURY",
               coupon_kind="Floating", is_default=False, in_arrears=True, paid_in_kind=True, exchange_rate="1.0825",
               duration="5.25", modified_duration="5.1", yield_to_maturity="4.2", accrual_date=date(2026, 1, 15))
    load.write(conn, Parsed(compositions=[composition()], lines=[line]), run_at=1)
    got = fetch(conn, "SELECT coupon_kind, is_default, in_arrears, paid_in_kind, exchange_rate, duration, "
                       "modified_duration, yield_to_maturity, yield_to_worst, accrual_date FROM lines")
    assert got == [("Floating", 0, 1, 1, "1.0825", "5.25", "5.1", "4.2", None, "2026-01-15")]


def test_a_re_served_fund_day_replaces_its_risk_rows(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(compositions=[composition()], fund_risk=[risk("1Y", "10"), risk("5Y", "40")]), run_at=1)
    load.write(conn, Parsed(compositions=[composition()], fund_risk=[risk("5Y", "42")]), run_at=2)
    assert fetch(conn, "SELECT tenor, dv01, run_at FROM fund_risk") == [("5Y", "42", 2)]


def test_a_composition_alone_leaves_the_fund_days_risk_rows(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(compositions=[composition()], fund_risk=[risk("5Y", "40")]), run_at=1)
    issuer = row("compositions", fund_ref=REF, as_of=DAY, basis="holdings", provider="ishares",
                 source_kind="daily_file", line_count=0)
    load.write(conn, Parsed(compositions=[issuer]), run_at=2)
    assert fetch(conn, "SELECT tenor, dv01, run_at FROM fund_risk") == [("5Y", "40", 1)]
    assert fetch(conn, "SELECT provider FROM compositions") == [("ishares",)]


def test_characteristics_are_replaced_per_provider(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(characteristics=[
        characteristic("ishares", "effective_duration", "6.1"),
        characteristic("ishares", "credit_quality", "40", "AAA"),
        characteristic("ishares", "credit_quality", "60", "BBB"),
    ]), run_at=1)
    load.write(conn, Parsed(characteristics=[characteristic("sec-nmfp", "seven_day_yield", "4.9")]), run_at=2)
    load.write(conn, Parsed(characteristics=[characteristic("ishares", "credit_quality", "100", "AA")]), run_at=3)
    got = fetch(conn, "SELECT provider, field, label, value FROM characteristics ORDER BY 1, 2, 3")
    assert got == [("ishares", "credit_quality", "AA", "100"), ("ishares", "effective_duration", "", "6.1"),
                   ("sec-nmfp", "seven_day_yield", "", "4.9")]


def test_a_figure_dated_later_leaves_the_others_days(tmp_path):
    """A page dates its daily figures by today and a monthly one by its
    month end: the next run's daily figures leave the month end's other
    figures standing."""
    conn = silver_db(tmp_path)
    sep, oct_ = date(2026, 9, 30), date(2026, 10, 9)

    def fig(field, value, day):
        return row("characteristics", fund_ref=REF, as_of=day, provider="ishares", field=field, value=value)

    load.write(conn, Parsed(characteristics=[fig("effective_duration", "6.1", sep), fig("sec_yield", "4.1", sep),
                                             fig("trailing_yield", "3.7", sep)]), run_at=1)
    load.write(conn, Parsed(characteristics=[fig("effective_duration", "6.0", oct_), fig("sec_yield", "4.0", oct_),
                                             fig("trailing_yield", "3.8", sep)]), run_at=2)
    got = fetch(conn, "SELECT field, date(as_of, 'unixepoch'), value FROM characteristics ORDER BY 1, 2")
    assert got == [("effective_duration", "2026-09-30", "6.1"), ("effective_duration", "2026-10-09", "6.0"),
                   ("sec_yield", "2026-09-30", "4.1"), ("sec_yield", "2026-10-09", "4.0"),
                   ("trailing_yield", "2026-09-30", "3.8")]


def test_a_composition_served_again_keeps_the_fund_days_characteristics(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(characteristics=[characteristic("ishares", "sec_yield", "4.1")]), run_at=1)
    load.write(conn, Parsed(compositions=[composition()]), run_at=2)
    assert fetch(conn, "SELECT field, value FROM characteristics") == [("sec_yield", "4.1")]


def test_a_period_figure_replaces_the_same_providers(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(period_facts=[fact("1.10", date(2026, 8, 5))]), run_at=1)
    load.write(conn, Parsed(period_facts=[fact("1.12", date(2026, 11, 3))]), run_at=2)
    got = fetch(conn, "SELECT as_of, period_start, value, filed, run_at FROM period_facts")
    assert got == [(load.epoch(DAY), "2026-04-01", "1.12", "2026-11-03", 2)]


def test_a_figure_filed_earlier_does_not_replace_a_later_one(tmp_path):
    """A run fetched earlier but loaded later states the period as first
    filed; the restatement silver already holds stays."""
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(period_facts=[fact("120", date(2026, 11, 3))]), run_at=2)
    load.write(conn, Parsed(period_facts=[fact("100", date(2026, 8, 5))]), run_at=1)
    assert fetch(conn, "SELECT value, filed FROM period_facts") == [("120", "2026-11-03")]
    # A figure that states no filing day (a frame's) leaves the filing's.
    load.write(conn, Parsed(period_facts=[fact("121", None)]), run_at=3)
    assert fetch(conn, "SELECT value, filed FROM period_facts") == [("120", "2026-11-03")]
    # Two frames' figures replace each other as written.
    conn.execute("DELETE FROM period_facts")
    load.write(conn, Parsed(period_facts=[fact("119", None)]), run_at=4)
    load.write(conn, Parsed(period_facts=[fact("118", None)]), run_at=5)
    assert fetch(conn, "SELECT value, filed FROM period_facts") == [("118", None)]


def event(amount, as_of=DAY, provider="ishares", kind="dividend"):
    ex, pay = date(2026, 6, 15), date(2026, 6, 19)
    return row("dividend_events", ref=REF, provider=provider, event_id=f"{kind}|{ex}|{ex}|{pay}", as_of=as_of,
               kind=kind, ex_date=ex, record_date=ex, pay_date=pay, amount=amount, currency="USD",
               payload={"total": amount})


def test_a_later_runs_event_replaces_the_same_providers_same_event(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(dividend_events=[event("0.25"), event("0.25", provider="spdr")]), run_at=1)
    load.write(conn, Parsed(dividend_events=[event("0.2512", as_of=date(2026, 7, 31)),
                                             event("0.05", kind="capital_gain")]), run_at=2)
    got = fetch(conn, "SELECT provider, event_id, date(as_of, 'unixepoch'), kind, ex_date, pay_date, amount, "
                      "payload, run_at FROM dividend_events ORDER BY 1, 2")
    assert got == [
        ("ishares", "capital_gain|2026-06-15|2026-06-15|2026-06-19", "2026-06-30", "capital_gain", "2026-06-15",
         "2026-06-19", "0.05", '{"total":"0.05"}', 2),
        ("ishares", "dividend|2026-06-15|2026-06-15|2026-06-19", "2026-07-31", "dividend", "2026-06-15",
         "2026-06-19", "0.2512", '{"total":"0.2512"}', 2),
        ("spdr", "dividend|2026-06-15|2026-06-15|2026-06-19", "2026-06-30", "dividend", "2026-06-15",
         "2026-06-19", "0.25", '{"total":"0.25"}', 1)]


def test_an_event_from_an_earlier_run_loaded_later_leaves_the_later_one(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(dividend_events=[event("0.2512")]), run_at=2)
    load.write(conn, Parsed(dividend_events=[event("0.25")]), run_at=1)
    assert fetch(conn, "SELECT amount, run_at FROM dividend_events") == [("0.2512", 2)]
    # The same run's row loaded again replaces itself.
    load.write(conn, Parsed(dividend_events=[event("0.2513")]), run_at=2)
    assert fetch(conn, "SELECT amount, run_at FROM dividend_events") == [("0.2513", 2)]



def dated(ex, pay, *, provider="ishares", ref=REF, amount="0.25"):
    """A dividend of `ref` with its ex day as its record day, its id
    built from its days, as a source that gives none."""
    ex, pay = date.fromisoformat(ex), date.fromisoformat(pay)
    return row("dividend_events", ref=ref, provider=provider, event_id=f"dividend|{ex}|{ex}|{pay}", as_of=DAY,
               kind="dividend", ex_date=ex, record_date=ex, pay_date=pay, amount=amount, currency="USD")


def ids(conn):
    return fetch(conn, "SELECT ref, provider, ex_date, pay_date FROM dividend_events ORDER BY 1, 2, 3, 4")


def test_a_complete_answer_drops_the_providers_rows_it_no_longer_states(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(dividend_events=[
        dated("2025-06-13", "2025-06-18"), dated("2025-12-12", "2025-12-17"), dated("2026-06-15", "2026-06-19"),
        dated("2026-06-15", "2026-06-19", provider="spdr"),
        dated("2026-06-15", "2026-06-19", ref="isin:XX0000000002")]), run_at=1)
    # The source corrects the newest pay day and no longer lists the
    # oldest distribution.
    load.write(conn, Parsed(dividend_events=[dated("2025-12-12", "2025-12-17"), dated("2026-06-15", "2026-06-22")],
                            events_complete=True), run_at=2)
    assert ids(conn) == [
        # Older than what the source lists: kept.
        (REF, "ishares", "2025-06-13", "2025-06-18"),
        (REF, "ishares", "2025-12-12", "2025-12-17"),
        # The corrected day leaves no stale row beside it.
        (REF, "ishares", "2026-06-15", "2026-06-22"),
        # Another provider's row, and another ref's, stand.
        (REF, "spdr", "2026-06-15", "2026-06-19"),
        ("isin:XX0000000002", "ishares", "2026-06-15", "2026-06-19")]


def test_an_answer_of_some_events_or_of_none_drops_nothing(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(dividend_events=[dated("2026-06-15", "2026-06-19")]), run_at=1)
    # A source that states only some (the latest, a list of days) adds.
    load.write(conn, Parsed(dividend_events=[dated("2026-06-15", "2026-06-22")]), run_at=2)
    # A complete answer that states no event leaves the history standing,
    # and so does one whose events state no day.
    load.write(conn, Parsed(events_complete=True), run_at=3)
    undated = row("dividend_events", ref=REF, provider="ishares", event_id="other|||", as_of=DAY, kind="other")
    load.write(conn, Parsed(dividend_events=[undated], events_complete=True), run_at=4)
    assert ids(conn) == [(REF, "ishares", None, None), (REF, "ishares", "2026-06-15", "2026-06-19"),
                         (REF, "ishares", "2026-06-15", "2026-06-22")]


def test_a_complete_answer_of_an_earlier_run_loaded_later_drops_no_later_row(tmp_path):
    conn = silver_db(tmp_path)
    load.write(conn, Parsed(dividend_events=[dated("2026-06-15", "2026-06-22")], events_complete=True), run_at=2)
    load.write(conn, Parsed(dividend_events=[dated("2026-06-15", "2026-06-19")], events_complete=True), run_at=1)
    # The earlier answer's row comes back beside the later one: nothing
    # records what the later answer dropped (load.py). Runs load in order.
    assert ids(conn) == [(REF, "ishares", "2026-06-15", "2026-06-19"), (REF, "ishares", "2026-06-15", "2026-06-22")]
