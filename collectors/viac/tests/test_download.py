"""Tests for download.py's dry-run contract and --debug trace (viac).

Root CLAUDE.md §2: `download --dry-run` walks the export surfaces with
the existing session but must **export nothing** — it may not create a
bronze run dir or write any artefact under `--bronze-dir`. A dry-run that
leaves even a `run.json`-only shell is a violation: `load`/`prune` would
then have to reason about it, and for a real download the shell already
carries endpoint JSON.

These tests drive the real ``download.main`` with a fake ViacClient
(no network, no browser) whose GETs return synthetic JSON, and assert:

  * dry-run: the walk still reaches every endpoint (session verified,
    surfaces enumerated) but NOTHING lands under the bronze root — with
    or without ``--debug``, which has no run dir to capture into.
  * real run (contrast): the same walk DOES write the run dir + JSON +
    a terminal ``status="complete"`` manifest — i.e. the dry-run gate
    did not change what a real download persists.
  * ``--debug``: every walked request lands one trace line, a retried
    transient failure included; without the flag, no capture dir at all.

Synthetic fixtures only — empty portfolio inventory + empty document
index, so no real portfolio numbers / holdings / doc ids appear.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402
from collectorkit import bronze  # noqa: E402


# ============================================================
# Fake VIAC client: no network, routes GETs to synthetic JSON
# ============================================================

class FakeResponse:
    def __init__(self, status_code: int, payload, headers=None):
        self.status_code = status_code
        self._payload = payload
        self.headers = ({"content-type": "application/json"}
                        if headers is None else headers)

    @property
    def content(self) -> bytes:
        return json.dumps(self._payload).encode("utf-8")

    def json(self):
        return self._payload


class FakeClient:
    """Stand-in for ViacClient. Records every GET path so a test can
    assert the read-only walk really happened, and answers with the
    minimal synthetic shapes ``walk`` needs: a live heartbeat, an empty
    portfolio inventory (no per-portfolio loop) and an empty document
    index (a list, no PDF loop)."""

    def __init__(self):
        self.calls: list[str] = []

    def __enter__(self) -> FakeClient:
        return self

    def __exit__(self, *exc) -> bool:
        return False

    def close(self) -> None:
        pass

    def get(self, path: str, **kwargs) -> FakeResponse:
        self.calls.append(path)
        if "heartbeat" in path:
            return FakeResponse(204, None)
        if "portfolio-inventory" in path:
            return FakeResponse(200, {"p3a": [], "pvb": [], "inv": []})
        if "/document/" in path:                 # document index endpoint
            return FakeResponse(200, [])          # must be a list; empty = no PDFs
        return FakeResponse(200, {})              # customer, summary, allocation, transactions


@pytest.fixture()
def patched_client(monkeypatch):
    """Make ``ViacClient.from_state`` yield a fresh FakeClient, ignoring
    the (dummy) state file on disk."""
    fake = FakeClient()
    monkeypatch.setattr(
        download.ViacClient, "from_state",
        staticmethod(lambda path: fake))
    return fake


def _state_file(tmp_path: Path) -> Path:
    """main() checks state_path.is_file() before from_state; give it a
    real (but ignored — from_state is patched) file."""
    p = tmp_path / "viac-state.json"
    p.write_text("{}")
    return p


def _under_dest(dest: Path) -> list[Path]:
    return list(dest.rglob("*"))


# ============================================================
# dry-run: nothing persisted under --bronze-dir
# ============================================================

def test_dry_run_persists_nothing_to_bronze(tmp_path, patched_client):
    dest = tmp_path / "bronze"
    dest.mkdir()
    state = _state_file(tmp_path)

    rc = download.main(
        ["--dry-run", "--bronze-dir", str(dest), "--state-path", str(state)])

    assert rc == 0
    # The invariant: no run dir, no manifest, no JSON — nothing at all
    # under the bronze root after a dry-run.
    assert _under_dest(dest) == []
    assert list(bronze.iter_run_dirs(dest)) == []
    assert not (dest / "run.json").exists()

    # ...yet the read-only walk still ran: session probed + surfaces
    # enumerated (this is the point of a dry-run).
    assert any("heartbeat" in c for c in patched_client.calls)
    assert any("customer/current" in c for c in patched_client.calls)
    assert any("portfolio-inventory" in c for c in patched_client.calls)
    assert any("/rest/web/document/" in c for c in patched_client.calls)


def test_dry_run_creates_no_bronze_dir_when_absent(tmp_path, patched_client):
    # Even the bronze root itself is not conjured: a dry-run touches
    # nothing under --bronze-dir, whether or not the dir pre-exists.
    dest = tmp_path / "bronze-absent"          # deliberately not created
    state = _state_file(tmp_path)

    rc = download.main(
        ["--dry-run", "--bronze-dir", str(dest), "--state-path", str(state)])

    assert rc == 0
    assert not dest.exists()


# ============================================================
# real run (contrast): the same walk DOES persist bronze
# ============================================================

def test_real_run_writes_complete_bronze_dump(tmp_path, patched_client):
    # Guardrail against over-gating: a non-dry-run still writes the run
    # dir, the endpoint JSON, and a terminal status="complete" manifest.
    dest = tmp_path / "bronze"
    dest.mkdir()
    state = _state_file(tmp_path)

    rc = download.main(
        ["--bronze-dir", str(dest), "--state-path", str(state)])

    assert rc == 0
    run_dirs = list(bronze.iter_run_dirs(dest))
    assert len(run_dirs) == 1
    d = run_dirs[0]
    # Endpoint JSON landed in bronze.
    for rel in ("customer.json",
                "wealth/portfolio-inventory.json",
                "wealth/summary.json",
                "wealth/allocation.json",
                "transactions/all.json",
                "documents/index.json"):
        assert (d / rel).is_file(), rel
    # Terminal manifest reads complete (the forward signal load/prune key on).
    manifest = json.loads((d / "run.json").read_text())
    assert manifest["status"] == "complete"
    assert manifest["dry_run"] is False
    # No --debug ⇒ no capture dir: debug artefacts are opt-in.
    assert not (d / "screenshots").exists()


# ============================================================
# --debug: the bronze-resident HTTP trace
# ============================================================

def _trace_lines(dest: Path) -> list[dict]:
    hits = list(dest.glob("*/screenshots/http-trace.jsonl"))
    return ([json.loads(x) for x in hits[0].read_text().splitlines()]
            if hits else [])


def test_debug_parses_and_defaults_off():
    assert download.parse_args([]).debug is False
    assert download.parse_args(["--debug"]).debug is True


def test_debug_traces_every_walked_endpoint(tmp_path, patched_client):
    dest = tmp_path / "bronze"
    dest.mkdir()
    state = _state_file(tmp_path)

    rc = download.main(
        ["--debug", "--bronze-dir", str(dest), "--state-path", str(state)])

    assert rc == 0
    lines = _trace_lines(dest)
    # The heartbeat probe runs before any run dir exists, so the trace
    # covers the walk's GETs — one line each, in flight order.
    traced = [x["url"] for x in lines]
    assert len(traced) == len(patched_client.calls) - 1     # minus heartbeat
    assert any(u.endswith("/rest/web/wealth/portfolio-inventory")
               for u in traced)
    assert all(u.startswith(download.BASE_URL) for u in traced)
    assert {x["status"] for x in lines} == {200}
    assert lines[0]["headers"]["content-type"] == "application/json"


def test_debug_trace_records_a_transient_failure(tmp_path, monkeypatch):
    # A dropped HTTP/2 stream is retried into a success; both attempts
    # must be on record, or the trace hides the very thing it exists for.
    import httpx

    class _FlakyClient(FakeClient):
        def __init__(self):
            super().__init__()
            self.failed = False

        def get(self, path, **kwargs):
            if "wealth/summary" in path and not self.failed:
                self.failed = True
                raise httpx.RemoteProtocolError("stream reset")
            return super().get(path, **kwargs)

    fake = _FlakyClient()
    monkeypatch.setattr(download.ViacClient, "from_state",
                        staticmethod(lambda path: fake))
    monkeypatch.setattr(download.time, "sleep", lambda *_a, **_k: None)
    dest = tmp_path / "bronze"
    dest.mkdir()

    rc = download.main(["--debug", "--bronze-dir", str(dest),
                        "--state-path", str(_state_file(tmp_path))])

    assert rc == 0
    lines = _trace_lines(dest)
    failures = [x for x in lines if "error" in x]
    assert len(failures) == 1
    assert "RemoteProtocolError" in failures[0]["error"]
    assert "status" not in failures[0]      # nothing came back to carry one
    # ...and the retry that succeeded is recorded beside it.
    assert sum(1 for x in lines if x["url"].endswith("/rest/web/wealth/summary")) == 2


def test_dry_run_with_debug_persists_nothing_to_bronze(tmp_path, patched_client):
    # A dry-run mints no run dir, so the trace has nowhere to land:
    # HttpTrace(None) swallows the records rather than resurrecting a
    # bronze shell that load/prune would then have to reason about.
    dest = tmp_path / "bronze"
    dest.mkdir()

    rc = download.main(["--dry-run", "--debug", "--bronze-dir", str(dest),
                        "--state-path", str(_state_file(tmp_path))])

    assert rc == 0
    assert _under_dest(dest) == []
    # The walk still ran — it just captured nothing.
    assert any("portfolio-inventory" in c for c in patched_client.calls)
