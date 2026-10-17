"""Tests for relevate's `login --check`: it loads the stored state,
replays its cookies into a fresh session, and reports the probe's
verdict. The probe is stubbed; no request leaves the test.

Synthetic cookie values only.
"""
from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))

import login  # noqa: E402
from collectorkit import session as ck_session  # noqa: E402


def _state(tmp_path: Path) -> Path:
    path = tmp_path / "relevate-state.json"
    ck_session.save_state(path, {
        "minted_at": "2099-01-01T00:00:00+00:00",
        "cookies": [{"name": "AL_SESS-S", "value": "synthetic",
                     "domain": "portal.pens-expert.ch", "path": "/"}]})
    return path


def test_check_without_a_state_file_is_missing(tmp_path, capsys):
    assert login.do_check(tmp_path / "absent.json") == 1
    assert capsys.readouterr().out.strip() == "MISSING"


def test_check_reports_the_probe_verdict(tmp_path, monkeypatch, capsys):
    seen = []

    def probe(session):
        seen.append(session.cookies.get("AL_SESS-S"))
        return True

    monkeypatch.setattr(login, "step_probe", probe)
    assert login.do_check(_state(tmp_path)) == 0
    assert capsys.readouterr().out.strip() == "ALIVE"
    assert seen == ["synthetic"]

    monkeypatch.setattr(login, "step_probe", lambda session: False)
    assert login.do_check(_state(tmp_path)) == 2
    assert capsys.readouterr().out.strip() == "DEAD"
