"""Behaviour tests for the `svb` host wrapper's command dispatch.

svb is a load-only collector: `login`, `download`, and `prune` are documented
no-ops (see DESIGN.md "Bronze layout and `prune`"). These tests lock in that the
wrapper never crashes on those verbs — the fleet orchestrator forwards `prune`
uniformly to every collector, so a `prune` that fell through to the unknown-command
arm (exit 2) would break the run. They also guard that `prune` deletes nothing.
"""
from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

WRAPPER = Path(__file__).parent / "svb"
BASH = shutil.which("bash")


def _run(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [BASH, str(WRAPPER), *args],
        capture_output=True, text=True, cwd=str(WRAPPER.parent),
    )


@pytest.mark.skipif(BASH is None, reason="bash not available")
def test_prune_is_a_clean_noop() -> None:
    r = _run("prune")
    assert r.returncode == 0, r.stderr
    out = r.stdout + r.stderr
    assert "no-op" in out.lower()
    assert "never deleted" in out.lower()


@pytest.mark.skipif(BASH is None, reason="bash not available")
def test_prune_tolerates_forwarded_args() -> None:
    # The orchestrator may append flags (e.g. --dry-run); the no-op arm must
    # not choke on them.
    r = _run("prune", "--dry-run", "--min-age-hours", "1")
    assert r.returncode == 0, r.stderr


@pytest.mark.skipif(BASH is None, reason="bash not available")
def test_prune_deletes_nothing_in_bronze(tmp_path: Path) -> None:
    bronze = tmp_path / "bronze"
    bronze.mkdir()
    pdf = bronze / "statement.pdf"
    pdf.write_bytes(b"%PDF-1.4 synthetic")
    sig = bronze / "signature.txt"
    sig.write_text("SYNTHETIC SIGNATURE\n")
    before = {p.name for p in bronze.iterdir()}

    r = _run("prune", "--data-dir", str(tmp_path))
    assert r.returncode == 0, r.stderr
    # The load inputs are untouched (prune is a no-op; it never reads or writes
    # bronze at all).
    assert {p.name for p in bronze.iterdir()} == before
    assert pdf.read_bytes() == b"%PDF-1.4 synthetic"


@pytest.mark.skipif(BASH is None, reason="bash not available")
def test_help_documents_prune() -> None:
    r = _run("help")
    assert r.returncode == 0, r.stderr
    assert "prune" in r.stdout
    assert "load" in r.stdout


@pytest.mark.skipif(BASH is None, reason="bash not available")
def test_unknown_command_still_errors() -> None:
    r = _run("bogus")
    assert r.returncode == 2
    assert "unknown command" in r.stderr
