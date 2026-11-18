"""The vocabularies the collector and the engine both write down, read
from the engine's Go source: a word one side renames fails here."""

import re

import wanted
import wants
from providers import REGISTRY, base, table
from tests.helpers import ENGINE, go_rows, go_source


def go_values(text, type_name) -> dict:
    """name -> value of each `<name> <type_name> = "…"` constant."""
    return dict(re.findall(rf'(\w+)\s+{type_name} = "([^"]*)"', text))


def test_dividend_event_kinds_and_statuses_are_the_engines():
    refdata = go_source("canonical/refdata.go")
    kinds = go_values(refdata, "DividendEventKind")
    listed = re.search(r"var DividendEventKinds = \[\]DividendEventKind\{(.*?)\}", refdata, re.S).group(1)
    assert set(base.EVENT_KINDS) == {kinds[name] for name in re.findall(r"\w+", listed)}
    assert set(base.EVENT_STATUSES) == set(go_values(refdata, "DividendEventStatus").values())


def test_the_us_exchange_patterns_are_golds():
    sql = re.search(r"const usLineSQL = `(.*?)`", go_source("gold/exposure_pass.go"), re.S).group(1)
    patterns = re.findall(r"regexp_matches\(lower\(COALESCE\(exchange, ''\)\),\s*'([^']*)'\)", sql)
    assert patterns == [table.US_EXCHANGE.pattern, table.NOT_US.pattern]


def test_a_security_claim_names_the_facts_a_security_wants():
    """The facts a security claim names (`facts`) are those a security's
    `wants` reads with the field gold dates them by."""
    assert wants.CLAIM_FACTS == {f.name for f in wants.FACTS if f.subject == "security" and f.asked_by == "facts"}
    assert wants.CLAIM_FACTS == set(wanted.SECURITY_WANTS)


def test_every_fact_of_the_engines_golden_list_has_a_provider():
    """A fund's `wants`, a security's `wants`, and the families of
    figures among its `missing` (periodFamilies) are facts the planner
    knows, and some registered provider answers each."""
    families = [family for family, _ in go_rows(go_source("gold/refdata_wanted.go"), "periodFamilies")]
    assert families == list(wants.FAMILIES)
    w = wanted.from_file(ENGINE / "gold" / "testdata" / "refdata_wanted.json")
    fund_facts = {f for fund in w.funds for f in fund.wants}
    security_facts = {f for s in w.securities for f in (*s.wants, *(f for f in s.missing if f in families))}
    assert fund_facts and security_facts
    assert [f for f in fund_facts if wants.fact_of("fund", f) not in wants.FACTS] == []
    assert [f for f in security_facts if wants.fact_of("security", f) not in wants.FACTS] == []
    for fact in sorted(fund_facts):
        assert any(p.serves_funds and fact in p.facts for p in REGISTRY.values()), fact
    for fact in sorted(security_facts):
        assert any((p.serves_securities or p.serves_bulk) and fact in p.answers for p in REGISTRY.values()), fact
