"""The wanted list: the contract this collector reads."""

import json
import stat
from datetime import date
from pathlib import Path

import pytest

import wanted
from tests.helpers import fund, wanted_doc

# The wanted list the engine's own tests pin.
GOLDEN = Path(__file__).resolve().parents[3] / "wealthdb" / "internal" / "gold" / "testdata" / "refdata_wanted.json"


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
                       "wants": ["risk_metrics", "characteristics", "line_durations"],
                       "facts": {"risk_metrics": ["2026-03-31", "2026-06-30"], "line_durations": ["2026-03-31"]}}],
                     [{"key": "isin:XX0000000302", "scope": "constituent", "share_pct": 1.5,
                       "missing": ["earnings"], "lookups": {},
                       "periods": {"dividends": "2026-06-30", "financials": "2026-03-31"}}], version=2)
    w = wanted.parse(doc)
    (f,) = w.funds
    assert f.wants == ("risk_metrics", "characteristics", "line_durations")
    assert f.facts == {"risk_metrics": (date(2026, 3, 31), date(2026, 6, 30)), "line_durations": (date(2026, 3, 31),)}
    (s,) = w.securities
    assert s.missing == ("earnings",)
    assert s.periods == {"dividends": date(2026, 6, 30), "financials": date(2026, 3, 31)}


@pytest.mark.parametrize("doc,msg", [
    ({"contract": "other", "version": 1}, "not a refdata-wanted"),
    ({"contract": "refdata-wanted", "version": 9}, "version 9"),
    ({"contract": "refdata-wanted", "version": 1, "funds": [{"held_from": "x"}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 2, "as_of": "2026-07-15",
      "funds": [{"ref": "symbol:EXBF", "facts": {"risk_metrics": ["x"]}}]}, "malformed"),
    ({"contract": "refdata-wanted", "version": 2, "as_of": "2026-07-15",
      "securities": [{"key": "isin:XX0000000302", "periods": {"earnings": "x"}}]}, "malformed"),
])
def test_parse_refuses(doc, msg):
    with pytest.raises(wanted.WantedError, match=msg):
        wanted.parse(doc)


def _engine(tmp_path, body):
    script = tmp_path / "engine"
    script.write_text("#!/bin/sh\n" + body)
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    return str(script)


def test_from_engine_runs_the_read_only_subcommand(tmp_path):
    doc = wanted_doc([fund("symbol:FUNDA", "FUNDA")])
    (tmp_path / "doc.json").write_text(json.dumps(doc))
    engine = _engine(tmp_path, f'[ "$1 $2" = "-r refdata-wanted" ] || exit 9\ncat {tmp_path / "doc.json"}\n')
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
    w = wanted.from_file(GOLDEN)
    assert w.funds and w.securities
    assert [f.ref for f in w.funds] == [f["ref"] for f in raw["funds"]]
    assert [f.held_from for f in w.funds] == [date.fromisoformat(f["held_from"]) if f.get("held_from") else None
                                              for f in raw["funds"]]
    assert any(f.held_from for f in w.funds)
    assert [s.share_pct for s in w.securities] == [None if s.get("share_pct") is None else float(s["share_pct"])
                                                   for s in raw["securities"]]
    assert [{k: v.isoformat() for k, v in s.lookups.items()} for s in w.securities] == \
        [s.get("lookups") or {} for s in raw["securities"]]
    assert any(s.lookups for s in w.securities)
    assert raw["version"] in wanted.VERSIONS
    assert [list(f.wants) for f in w.funds] == [f["wants"] for f in raw["funds"]]
    assert {"risk_metrics", "characteristics", "money_market", "line_durations"} <= {x for f in w.funds for x in f.wants}
    assert [{k: [d.isoformat() for d in v] for k, v in f.facts.items()} for f in w.funds] == \
        [f["facts"] for f in raw["funds"]]
    assert any(len(v) for f in w.funds for v in f.facts.values())
    assert [s.missing for s in w.securities] == [tuple(s["missing"]) for s in raw["securities"]]
    assert {"earnings", "dividends", "financials"} <= {x for s in w.securities for x in s.missing}
    assert [{k: v.isoformat() for k, v in s.periods.items()} for s in w.securities] == \
        [s["periods"] for s in raw["securities"]]
    assert any(s.periods for s in w.securities)
