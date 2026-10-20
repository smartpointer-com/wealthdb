"""load.write for the facts of schema v2: a fund's rate sensitivities go
with its fund-day, its characteristics are replaced per provider, and a
company's figure for a period replaces the same provider's."""

from datetime import date

import load
from collectorkit import silver
from providers.base import Parsed, row

DAY = date(2026, 6, 30)
REF = "isin:XX0000000001"


def open_silver(tmp_path):
    conn = silver.open_db(tmp_path / "refdata.db")
    silver.apply_migrations(conn, load.MIGRATIONS)
    return conn


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


def test_a_line_keeps_its_debt_flags_and_figures(tmp_path):
    conn = open_silver(tmp_path)
    line = row("lines", fund_ref=REF, as_of=DAY, basis="holdings", line_no=1, name="EXAMPLE TREASURY",
               coupon_kind="Floating", is_default=False, in_arrears=True, paid_in_kind=True, exchange_rate="1.0825",
               duration="5.25", modified_duration="5.1", yield_to_maturity="4.2", accrual_date=date(2026, 1, 15))
    load.write(conn, Parsed(compositions=[composition()], lines=[line]), run_at=1)
    got = fetch(conn, "SELECT coupon_kind, is_default, in_arrears, paid_in_kind, exchange_rate, duration, "
                       "modified_duration, yield_to_maturity, yield_to_worst, accrual_date FROM lines")
    assert got == [("Floating", 0, 1, 1, "1.0825", "5.25", "5.1", "4.2", None, "2026-01-15")]


def test_a_re_served_fund_day_replaces_its_risk_rows(tmp_path):
    conn = open_silver(tmp_path)
    load.write(conn, Parsed(compositions=[composition()], fund_risk=[risk("1Y", "10"), risk("5Y", "40")]), run_at=1)
    load.write(conn, Parsed(compositions=[composition()], fund_risk=[risk("5Y", "42")]), run_at=2)
    assert fetch(conn, "SELECT tenor, dv01, run_at FROM fund_risk") == [("5Y", "42", 2)]


def test_a_composition_alone_leaves_the_fund_days_risk_rows(tmp_path):
    conn = open_silver(tmp_path)
    load.write(conn, Parsed(compositions=[composition()], fund_risk=[risk("5Y", "40")]), run_at=1)
    issuer = row("compositions", fund_ref=REF, as_of=DAY, basis="holdings", provider="ishares",
                 source_kind="daily_file", line_count=0)
    load.write(conn, Parsed(compositions=[issuer]), run_at=2)
    assert fetch(conn, "SELECT tenor, dv01, run_at FROM fund_risk") == [("5Y", "40", 1)]
    assert fetch(conn, "SELECT provider FROM compositions") == [("ishares",)]


def test_characteristics_are_replaced_per_provider(tmp_path):
    conn = open_silver(tmp_path)
    load.write(conn, Parsed(characteristics=[
        characteristic("ishares", "effective_duration", "6.1"),
        characteristic("ishares", "credit_quality", "40", "AAA"),
        characteristic("ishares", "credit_quality", "60", "BBB"),
    ]), run_at=1)
    load.write(conn, Parsed(characteristics=[characteristic("sec-nmfp", "seven_day_yield", "4.9")]), run_at=2)
    load.write(conn, Parsed(characteristics=[characteristic("ishares", "credit_quality", "100", "AA")]), run_at=3)
    got = fetch(conn, "SELECT provider, field, label, value FROM characteristics ORDER BY 1, 2, 3")
    assert got == [("ishares", "credit_quality", "AA", "100"), ("sec-nmfp", "seven_day_yield", "", "4.9")]


def test_a_composition_served_again_keeps_the_fund_days_characteristics(tmp_path):
    conn = open_silver(tmp_path)
    load.write(conn, Parsed(characteristics=[characteristic("ishares", "sec_yield", "4.1")]), run_at=1)
    load.write(conn, Parsed(compositions=[composition()]), run_at=2)
    assert fetch(conn, "SELECT field, value FROM characteristics") == [("sec_yield", "4.1")]


def test_a_period_figure_replaces_the_same_providers(tmp_path):
    conn = open_silver(tmp_path)

    def fact(value, filed):
        return row("period_facts", security_key="symbol:EXMP", field="eps_diluted", as_of=DAY,
                   period_start=date(2026, 4, 1), provider="sec-edgar", value=value,
                   scheme="us-gaap:EarningsPerShareDiluted", unit="USD/shares", fiscal_year=2026,
                   fiscal_period="Q2", form="10-Q", filed=filed)

    load.write(conn, Parsed(period_facts=[fact("1.10", date(2026, 8, 5))]), run_at=1)
    load.write(conn, Parsed(period_facts=[fact("1.12", date(2026, 11, 3))]), run_at=2)
    got = fetch(conn, "SELECT as_of, period_start, value, filed, run_at FROM period_facts")
    assert got == [(load.epoch(DAY), "2026-04-01", "1.12", "2026-11-03", 2)]


def test_a_figure_filed_earlier_does_not_replace_a_later_one(tmp_path):
    """A run fetched earlier but loaded later states the period as first
    filed; the restatement silver already holds stays."""
    conn = open_silver(tmp_path)

    def fact(value, filed):
        return row("period_facts", security_key="symbol:EXMP", field="revenue", as_of=DAY,
                   period_start=date(2026, 4, 1), provider="sec-edgar", value=value, filed=filed)

    load.write(conn, Parsed(period_facts=[fact("120", date(2026, 11, 3))]), run_at=2)
    load.write(conn, Parsed(period_facts=[fact("100", date(2026, 8, 5))]), run_at=1)
    assert fetch(conn, "SELECT value, filed FROM period_facts") == [("120", "2026-11-03")]
    # A figure that states no filing day (a frame's) replaces as written.
    load.write(conn, Parsed(period_facts=[fact("121", None)]), run_at=3)
    assert fetch(conn, "SELECT value, filed FROM period_facts") == [("121", None)]
