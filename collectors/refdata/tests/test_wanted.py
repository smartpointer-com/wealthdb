"""The wanted list: the contract this collector reads."""

import json
import stat

import pytest

import wanted
from tests.helpers import fund, wanted_doc


def test_parse_reads_funds_and_securities():
    doc = wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-03-31"])],
                     [{"key": "isin:XX0000000301", "isin": "XX0000000301", "scope": "direct",
                       "missing": ["sector"], "lookups": {"gleif": "2026-07-01"}}])
    w = wanted.parse(doc)
    (f,) = w.funds
    assert f.held_to is None and str(f.compositions[0]) == "2026-03-31"
    (s,) = w.securities
    assert s.missing == ("sector",) and str(s.lookups["gleif"]) == "2026-07-01"


@pytest.mark.parametrize("doc,msg", [
    ({"contract": "other", "version": 1}, "not a refdata-wanted"),
    ({"contract": "refdata-wanted", "version": 9}, "version 9"),
    ({"contract": "refdata-wanted", "version": 1, "funds": [{"held_from": "x"}]}, "malformed"),
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
    engine = _engine(tmp_path, 'echo "wealthdb: unknown subcommand \\"refdata-wanted\\"" >&2; exit 2\n')
    with pytest.raises(wanted.WantedError, match="older than this collector"):
        wanted.from_engine(engine)


def test_a_missing_engine_says_how_to_name_one():
    with pytest.raises(wanted.WantedError, match="REFDATA_WEALTHDB"):
        wanted.from_engine("/nonexistent/wealthdb")
