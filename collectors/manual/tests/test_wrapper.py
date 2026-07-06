"""Wrapper-level guards for the `manual` bash wrapper.

`manual` is load-only: no download.py, no timestamped bronze run-dirs, no
debug artefacts. It still has to accept the fleet-wide `prune` verb — the
`wealthdb-collect` dispatcher forwards it — without crashing, as a documented
no-op, and its `help` must list every verb it accepts. These are pure
subprocess checks against the bash wrapper; no venv, network, or CSVs are
touched (`prune`/`help` exit before the wrapper ever reaches the loader).
"""
import subprocess
from pathlib import Path

_WRAPPER = Path(__file__).resolve().parent.parent / "manual"


def _run(*args):
    return subprocess.run([str(_WRAPPER), *args], capture_output=True,
                          text=True)


def test_prune_is_a_documented_noop():
    result = _run("prune")
    assert result.returncode == 0, (
        f"`manual prune` exited {result.returncode}; the dispatcher forwards "
        f"prune, so the wrapper must not crash on it. stderr:\n{result.stderr}")
    combined = (result.stdout + result.stderr).lower()
    assert "no-op" in combined, "prune should explain that it is a no-op"


def test_prune_tolerates_extra_args():
    # The dispatcher may forward flags (e.g. --dry-run); the no-op must still
    # exit 0 rather than choke on unknown arguments.
    assert _run("prune", "--dry-run").returncode == 0


def test_help_lists_prune():
    result = _run("help")
    assert result.returncode == 0
    assert "prune" in result.stdout, "help must document the prune verb"


def test_help_does_not_leak_shell_setup():
    # The help banner is the comment block only — it must never spill the
    # wrapper's own `set -euo pipefail` line into user-facing output.
    result = _run("help")
    assert "set -euo pipefail" not in result.stdout


def test_unknown_command_still_errors():
    result = _run("bogus")
    assert result.returncode == 2
    assert "unknown command" in result.stderr
