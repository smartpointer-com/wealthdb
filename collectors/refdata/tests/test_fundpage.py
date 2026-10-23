"""fundpage: the rules every issuer's fund page is read by."""

import logging
from datetime import date

import pytest

from providers import fundpage, invesco, ishares, spdr, xtrackers
from tests.helpers import go_source, go_strings

DAY = date(2026, 10, 12)
log = logging.getLogger("refdata.test")


def test_no_figure_is_one_gold_reads_as_a_money_market_figure():
    """Gold counts a fund's characteristics row as a money-market figure
    by its field alone: a maturity in days, a money-market filing's
    field, is kept in years by every provider's map."""
    money_market = set(go_strings(go_source("gold/refdata_wanted.go"), "moneyMarketFields"))
    names = {*ishares.NAMES.values(), *ishares.LABELS.values(), *spdr.LABELS.values(), *invesco.NAMES.values(),
             *xtrackers.LABELS.values()}
    fields = {fundpage.figure(name, "35", unit)[0] for name in names for unit in ("", "days", "yrs", "%", "bps")}
    assert "weighted_average_maturity_years" in fields
    breakdowns = {*ishares.BREAKDOWNS.values(), *spdr.BREAKDOWNS, *invesco.BREAKDOWNS}
    assert breakdowns <= set(fundpage.BREAKDOWN_FIELDS)
    assert not (fields | breakdowns) & money_market


def test_a_breakdowns_parts():
    """A part of no label, no share or a zero share is none, and so is a
    label that came before; a negative share is one."""
    pairs = [("AAA", "60.5"), (" Not  Rated ", 1), ("AAA", "39.5"), ("", "5"), ("BB", None), ("B", "0.00"),
             ("Cash", "-0.5")]
    assert fundpage.parts("credit_quality", pairs, DAY) == [
        ("credit_quality", "AAA", "60.5", DAY), ("credit_quality", "Not Rated", "1", DAY),
        ("credit_quality", "Cash", "-0.5", DAY)]


@pytest.mark.parametrize("came,known,missing,warned", [
    (True, True, [], False),
    (True, False, ["characteristics"], True),
    (False, False, ["characteristics"], False),
])
def test_a_page_answers_the_characteristics_in_a_known_layout_only(caplog, came, known, missing, warned):
    with caplog.at_level(logging.WARNING, logger="refdata.test"):
        assert fundpage.due(came, known, "isin:XX0000000001", log=log) == missing
    assert ("matched no known layout" in caplog.text) is warned


@pytest.mark.parametrize("named,skip", [
    ("Equity", fundpage.BOND_FIGURES), ("Equities", fundpage.BOND_FIGURES),
    ("Fixed Income", fundpage.EQUITY_FIGURES), ("Money Market", fundpage.EQUITY_FIGURES),
    ("Commodities", fundpage.BOND_FIGURES | fundpage.EQUITY_FIGURES), ("Multi Asset", frozenset()),
    ("", frozenset()), (None, frozenset()),
])
def test_each_sites_word_for_an_asset_class(named, skip):
    assert fundpage.not_applicable(named) == skip
