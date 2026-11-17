"""The vocabularies the collector and the engine both write down, read
from the engine's Go source: a word one side renames fails here."""

import json
import re

import wants
from providers import REGISTRY, base, table
from tests.helpers import ENGINE, go_source


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


def test_every_fact_of_the_engines_golden_list_has_a_provider():
    """A fund's `wants`, a security's `wants`, and the families of
    figures among its `missing` (periodFamilies) are facts the planner
    knows, and some registered provider answers each."""
    block = re.search(r"var periodFamilies = .*?\n\}\n", go_source("gold/refdata_wanted.go"), re.S).group(0)
    families = re.findall(r'\{"(\w+)",\s*\[\]string', block)
    assert families == list(wants.FAMILIES)
    doc = json.loads((ENGINE / "gold" / "testdata" / "refdata_wanted.json").read_text())
    fund_facts = {f for fund in doc["funds"] for f in fund.get("wants") or ()}
    security_facts = {f for s in doc["securities"]
                      for f in (*(s.get("wants") or ()), *(f for f in s.get("missing") or () if f in families))}
    assert fund_facts and security_facts
    assert [f for f in fund_facts if wants.fact_of("fund", f) not in wants.FACTS] == []
    assert [f for f in security_facts if wants.fact_of("security", f) not in wants.FACTS] == []
    for fact in sorted(fund_facts):
        assert any(p.serves_funds and fact in p.facts | p.composition_facts for p in REGISTRY.values()), fact
    for fact in sorted(security_facts):
        assert any((p.serves_securities or p.serves_bulk) and fact in p.answers for p in REGISTRY.values()), fact
