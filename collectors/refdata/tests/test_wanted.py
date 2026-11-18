"""The wanted list: the contract this collector reads."""

import json
import stat
from dataclasses import replace
from datetime import date
from pathlib import Path

import pytest

import wanted
from tests.helpers import fund, wanted_doc

# The wanted list the engine's own tests pin.
GOLDEN = Path(__file__).resolve().parents[3] / "wealthdb" / "internal" / "gold" / "testdata" / "refdata_wanted.json"
# The same list as gold builds it, in version 4's shape.
GOLDEN_V4 = GOLDEN.with_name("refdata_wanted_v4.json")


def test_parse_reads_funds_and_securities():
    doc = wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-03-31"])],
                     [{"key": "isin:XX0000000301", "isin": "XX0000000301", "scope": "direct",
                       "missing": ["sector"], "lookups": {"gleif": "2026-07-01"}}])
    w = wanted.parse(doc)
    (f,) = w.funds
    assert f.held_to is None and str(f.compositions[0]) == "2026-03-31"
    # A version 1 list names no facts.
    assert f.wants == () and f.facts == {}
    (s,) = w.securities
    assert s.missing == ("sector",) and str(s.lookups["gleif"]) == "2026-07-01"
    assert s.periods == {}


def test_parse_reads_the_facts_of_version_2():
    doc = wanted_doc([{**fund("symbol:EXBF", "EXBF", compositions=["2026-03-31", "2026-06-30"]),
                       "wants": ["risk_metrics", "characteristics", "money_market"],
                       "facts": {"risk_metrics": ["2026-03-31", "2026-06-30"], "money_market": ["2026-03-31"]}}],
                     [{"key": "isin:XX0000000302", "scope": "constituent", "share_pct": 1.5,
                       "missing": ["earnings"], "lookups": {},
                       "periods": {"dividends": "2026-06-30", "financials": "2026-03-31"}}], version=2)
    w = wanted.parse(doc)
    (f,) = w.funds
    assert f.wants == ("risk_metrics", "characteristics", "money_market")
    assert f.facts == {"risk_metrics": (date(2026, 3, 31), date(2026, 6, 30)), "money_market": (date(2026, 3, 31),)}
    (s,) = w.securities
    assert s.missing == ("earnings",)
    assert s.periods == {"dividends": date(2026, 6, 30), "financials": date(2026, 3, 31)}


def test_parse_reads_the_calendar_of_version_3():
    doc = wanted_doc([{**fund("symbol:SPY", "SPY", compositions=["2026-06-30"]),
                       "wants": ["characteristics", "dividend_calendar"],
                       "facts": {"dividend_calendar": ["2026-06-15", "2026-07-01"]}}],
                     [{"key": "symbol:QRCP", "scope": "direct", "missing": [], "lookups": {}, "periods": {},
                       "receipt": True, "wants": ["dividend_calendar"], "calendar": "2026-07-01"},
                      {"key": "symbol:QSHR", "scope": "direct", "missing": [], "lookups": {}, "periods": {},
                       "receipt": False, "wants": ["dividend_calendar"], "calendar": None}], version=3)
    w = wanted.parse(doc)
    (f,) = w.funds
    assert f.wants == ("characteristics", "dividend_calendar")
    assert f.facts == {"dividend_calendar": (date(2026, 6, 15), date(2026, 7, 1))}
    receipt, share = w.securities
    assert (receipt.receipt, receipt.wants, receipt.calendar) == (True, ("dividend_calendar",), date(2026, 7, 1))
    assert (share.receipt, share.calendar) == (False, None)
    # A version 2 list states none of them.
    (s,) = wanted.parse(wanted_doc(securities=[{"key": "symbol:QSHR", "scope": "direct"}], version=2)).securities
    assert (s.receipt, s.wants, s.calendar) == (False, (), None)


def test_parse_reads_the_history_of_version_4():
    (f,) = wanted.parse(wanted_doc(funds=[{"ref": "symbol:QIDX", "roles": ["benchmark"],
                                           "wants": ["monthly_returns", "characteristics"],
                                           "history_from": "2019-01-01"}], version=4)).funds
    assert f.history_from == date(2019, 1, 1) and f.roles == ("benchmark",)
    # A version 3 list states none.
    (f,) = wanted.parse(wanted_doc(funds=[{"ref": "symbol:QIDX"}], version=3)).funds
    assert f.history_from is None


def test_parse_reads_a_version_5_list_as_the_fields_before():
    """A benchmark fund's history_from is the `from` of its
    monthly_returns; its other wants and their days, a security's lookups,
    missing families and calendar read as the versions before state
    them."""
    doc = wanted_doc(funds=[{"ref": "symbol:QIDX", "roles": ["benchmark"], "wants": [
        {"fact": "composition", "from": "2026-07-15", "to": None, "have": ["2026-06-30"]},
        {"fact": "monthly_returns", "from": "2019-01-01", "to": None, "have": ["2026-05-31", "2026-06-30"]},
        {"fact": "characteristics", "from": "2026-07-15", "to": None, "have": []}]}],
        securities=[{"key": "isin:XX0000000303", "scope": "direct", "wants": [
            {"fact": "identity", "by": {"gleif": "2026-07-01"}},
            {"fact": "earnings", "have": ["2026-03-31"]},
            {"fact": "dividend_calendar", "have": ["2026-07-01"]}]}], version=5)
    w = wanted.parse(doc)
    (f,) = w.funds
    assert (f.held_from, f.held_to, f.compositions) == (date(2026, 7, 15), None, (date(2026, 6, 30),))
    assert f.history_from == date(2019, 1, 1) and f.wants == ("monthly_returns", "characteristics")
    assert f.facts == {"monthly_returns": (date(2026, 5, 31), date(2026, 6, 30))}
    (s,) = w.securities
    assert (s.lookups, s.missing, s.periods) == ({"gleif": date(2026, 7, 1)}, ("earnings",),
                                                 {"earnings": date(2026, 3, 31)})
    assert (s.wants, s.calendar) == (("dividend_calendar",), date(2026, 7, 1))
    # A fund that wants no monthly returns has no history_from.
    (f,) = wanted.parse(wanted_doc(funds=[{"ref": "symbol:QHLD", "wants": [
        {"fact": "composition", "from": "2026-01-02", "to": None, "have": []}]}], version=5)).funds
    assert f.history_from is None and f.wants == ()


@pytest.mark.parametrize("doc,msg", [
    ({"contract": "other", "version": 1}, "not a refdata-wanted"),
    ({"contract": "refdata-wanted", "version": 9}, "version 9"),
    ({"contract": "refdata-wanted", "version": 1, "funds": [{"held_from": "x"}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 2, "as_of": "2026-07-15",
      "funds": [{"ref": "symbol:EXBF", "facts": {"risk_metrics": ["x"]}}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 2, "as_of": "2026-07-15",
      "securities": [{"key": "isin:XX0000000302", "periods": {"earnings": "x"}}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 3, "as_of": "2026-07-15",
      "securities": [{"key": "isin:XX0000000302", "receipt": "false"}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 3, "as_of": "2026-07-15",
      "securities": [{"key": "isin:XX0000000302", "calendar": "x"}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 5, "as_of": "2026-07-15",
      "funds": [{"ref": "symbol:EXBF", "wants": [{"from": "2026-01-02"}]}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 5, "as_of": "2026-07-15",
      "funds": [{"ref": "symbol:EXBF", "wants": [{"fact": "risk_metrics", "have": 20260630}]}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 5, "as_of": "2026-07-15",
      "securities": [{"key": "isin:XX0000000302", "wants": [{"have": ["2026-03-31"]}]}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 5, "as_of": "2026-07-15",
      "securities": [{"key": "isin:XX0000000302", "wants": [{"fact": "earnings", "have": 20260331}]}]}, "malformed"),
])
def test_parse_refuses(doc, msg):
    with pytest.raises(wanted.WantedError, match=msg):
        wanted.parse(doc)


def _engine(tmp_path, body):
    script = tmp_path / "engine"
    script.write_text("#!/bin/sh\n" + body)
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    return str(script)


def test_from_engine_runs_the_read_only_subcommand(tmp_path, monkeypatch):
    doc = wanted_doc([fund("symbol:FUNDA", "FUNDA")])
    (tmp_path / "doc.json").write_text(json.dumps(doc))
    # The engine gets the environment without FINRA's credential.
    monkeypatch.setenv("FINRA_CLIENT_ID", "example-id")
    monkeypatch.setenv("FINRA_CLIENT_SECRET", "example-secret")
    monkeypatch.setenv("REFDATA_EXAMPLE", "kept")
    engine = _engine(tmp_path, f'[ "$1 $2" = "-r refdata-wanted" ] || exit 9\n'
                               f'[ -z "${{FINRA_CLIENT_ID+x}}${{FINRA_CLIENT_SECRET+x}}" ] || exit 8\n'
                               f'[ "$REFDATA_EXAMPLE" = kept ] || exit 7\ncat {tmp_path / "doc.json"}\n')
    assert wanted.from_engine(engine).funds[0].ref == "symbol:FUNDA"


def test_an_engine_older_than_the_contract_says_so(tmp_path):
    # As the engine answers: the line that says so, then its whole usage.
    usage = "".join(f'echo "  command{i}   what it does" >&2\n' for i in range(20))
    engine = _engine(tmp_path, 'echo "wealthdb: unknown subcommand \\"refdata-wanted\\"" >&2\necho >&2\n'
                     'echo "Usage: wealthdb [flags] <command>" >&2\n' + usage + "exit 2\n")
    with pytest.raises(wanted.WantedError, match="older than this collector"):
        wanted.from_engine(engine)


def test_a_missing_engine_says_how_to_name_one():
    with pytest.raises(wanted.WantedError, match="REFDATA_WEALTHDB"):
        wanted.from_engine("/nonexistent/wealthdb")


def test_an_engine_that_cannot_run_says_so(tmp_path):
    script = tmp_path / "engine"
    script.write_text("#!/bin/sh\nexit 0\n")
    script.chmod(0o644)
    with pytest.raises(wanted.WantedError, match="could not run"):
        wanted.from_engine(str(script))


def test_an_engine_that_runs_too_long_says_so(tmp_path):
    engine = _engine(tmp_path, "exec sleep 5\n")
    with pytest.raises(wanted.WantedError, match="could not run: .*timed out"):
        wanted.from_engine(engine, timeout=0.05)


def test_from_file_reads_a_saved_list_and_refuses_what_is_not_one(tmp_path):
    saved = tmp_path / "wanted.json"
    saved.write_text(json.dumps(wanted_doc([fund("symbol:FUNDA", "FUNDA")])))
    assert wanted.from_file(saved).funds[0].ref == "symbol:FUNDA"
    with pytest.raises(wanted.WantedError, match="none.json"):
        wanted.from_file(tmp_path / "none.json")
    saved.write_text("{")
    with pytest.raises(wanted.WantedError, match="is not JSON"):
        wanted.from_file(saved)
    saved.write_text(json.dumps({"contract": "refdata-wanted", "version": 1}))
    with pytest.raises(wanted.WantedError, match="malformed"):
        wanted.from_file(saved)


def test_an_engine_that_fails_says_how(tmp_path):
    engine = _engine(tmp_path, 'echo "wealthdb: no config" >&2; exit 4\n')
    with pytest.raises(wanted.WantedError, match=r"failed \(4\): wealthdb: no config"):
        wanted.from_engine(engine)


def test_an_engine_that_prints_no_json_says_so(tmp_path):
    engine = _engine(tmp_path, 'echo "Usage: wealthdb"\n')
    with pytest.raises(wanted.WantedError, match="printed no JSON"):
        wanted.from_engine(engine)


def test_parse_reads_the_engine_golden():
    # The engine's own pinned output reads as the contract this collector
    # knows, field by field.
    raw = json.loads(GOLDEN.read_text(encoding="utf-8"))
    assert raw["version"] == max(wanted.VERSIONS)
    w = wanted.from_file(GOLDEN)
    assert [f.ref for f in w.funds] == [f["ref"] for f in raw["funds"]]
    assert [s.key for s in w.securities] == [s["key"] for s in raw["securities"]]
    assert any(f.held_from for f in w.funds) and any(s.lookups for s in w.securities)
    assert {"risk_metrics", "characteristics", "money_market"} <= {x for f in w.funds for x in f.wants}
    assert any(len(v) for f in w.funds for v in f.facts.values())
    assert {"earnings", "dividends", "financials"} <= {x for s in w.securities for x in s.missing}
    assert any(s.periods for s in w.securities) and any(s.calendar for s in w.securities)
    assert any(f.history_from for f in w.funds)


def test_a_version_5_list_reads_as_the_version_4_list_of_the_same_gold():
    """Gold pins its build in version 4's shape beside the list it prints.
    Both read alike, but for what version 5 leaves out: the days of a fact
    a fund does not want, the periods of a family a security does not
    lack, and the calendar of a security that does not want it."""
    def fund(f):
        return replace(f, facts={k: v for k, v in f.facts.items() if k in f.wants})

    def security(s):
        return replace(s, periods={k: v for k, v in s.periods.items() if k in s.missing},
                       calendar=s.calendar if "dividend_calendar" in s.wants else None)

    v4, v5 = wanted.from_file(GOLDEN_V4), wanted.from_file(GOLDEN)
    assert json.loads(GOLDEN_V4.read_text(encoding="utf-8"))["version"] == 4
    assert [fund(f) for f in v4.funds] == list(v5.funds)
    assert [security(s) for s in v4.securities] == list(v5.securities)
    assert (v4.as_of, v4.max_gap_days) == (v5.as_of, v5.max_gap_days)
