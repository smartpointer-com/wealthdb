"""
Unit tests for what schwab-api's prune config decides; the shared engine
(age guard, symlinks, unreadable manifests, --dry-run, target checks) is
tested in collectorkit. The tests drive the collector's `prune.main`
and `CONFIG`.

tmp_path bronze trees. A schwab-api bronze run dir is a flat set of JSON
load inputs plus, only under `download --debug`, a screenshots/ HTTP trace
(debug_subdirs). Covers:
  * complete dump: the --debug trace reclaimed, every load input kept
  * no run.json, or a statusless one, with open_orders.json (plain or
    .zst) present: COMPLETE, kept; without it: NON_COMPLETE, deleted
  * forward status is authoritative: in-progress + open_orders present
    is still NON_COMPLETE
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default

ACCT_PLAIN = "00000001"
ACCT_HASH = "HASH00000000000000000001"


def make_dump(root: Path, slug: str, *, status: str | None = "complete",
              run_json: bool = True, open_orders: bool = True,
              screenshots: bool = False, age_s: float = 0.0) -> Path:
    """Build a schwab-api bronze run dir with synthetic load inputs.

    ``run_json`` toggles the manifest (``status`` its status field, or
    a statusless ``{}`` when ``status is None``); ``open_orders`` toggles
    the terminal ``open_orders.json`` artefact (the legacy completeness
    signal); ``screenshots`` adds the ``--debug`` HTTP trace, off by
    default to mirror a download without ``--debug``. ``age_s`` backdates
    every file/dir mtime (and the run dir's own) to now-age_s so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh."""
    d = root / slug
    d.mkdir(parents=True)
    if screenshots:
        (d / "screenshots").mkdir()
        (d / "screenshots" / "http-trace.jsonl").write_text(
            '{"method": "GET", "url": "https://x.invalid/a", "status": 200}\n')
    (d / "account_numbers.json").write_text(json.dumps(
        [{"accountNumber": ACCT_PLAIN, "hashValue": ACCT_HASH}]))
    (d / "user_preference.json").write_text(json.dumps({"accounts": []}))
    (d / "accounts_positions.json").write_text(json.dumps([
        {"securitiesAccount": {"accountNumber": ACCT_PLAIN,
                               "type": "CASH", "positions": []}}]))
    (d / "transactions_000.json").write_text(json.dumps({
        "account_hash": ACCT_HASH, "window_start": "2026-01-01",
        "window_end": "2026-01-07", "transactions": []}))
    if open_orders:
        (d / "open_orders.json").write_text(json.dumps({"orders": []}))
    if run_json:
        (d / "run.json").write_text(json.dumps(
            {"status": status} if status is not None else {}))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


def _inputs_intact(d: Path) -> bool:
    return all((d / name).exists() for name in (
        "account_numbers.json", "accounts_positions.json",
        "user_preference.json", "transactions_000.json", "open_orders.json"))


# ============================================================
# Complete dumps: load inputs untouched, the --debug trace reclaimed
# ============================================================

def test_complete_dump_debug_trace_pruned_inputs_kept(tmp_path):
    # The --debug HTTP trace is a diagnostic, not a load input, so prune
    # reclaims it while every artefact beside it survives.
    d = make_dump(tmp_path, OLD_TS, screenshots=True)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert _inputs_intact(d)


def test_legacy_complete_dump_no_manifest_kept(tmp_path):
    # No run.json at all (meta is None); the terminal signal is
    # open_orders.json, the last unconditional artefact a complete run
    # writes. Present ⇒ COMPLETE, kept.
    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert _inputs_intact(d)


def test_statusless_manifest_with_open_orders_kept(tmp_path):
    # A run.json with no `status` key + open_orders.json present: the
    # fallback (open_orders exists) classifies COMPLETE.
    d = make_dump(tmp_path, OLD_TS, status=None, open_orders=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert _inputs_intact(d)


def test_legacy_complete_dump_compressed_open_orders_kept(tmp_path):
    # Regression: a dump with no run.json whose terminal artefact
    # open_orders.json was recompressed to open_orders.json.zst must
    # still classify COMPLETE. _is_complete resolves the on-disk variant,
    # so the `recompress` sweep does not turn a complete dump into a
    # prune target (which would silently delete its bronze).
    from collectorkit import compress

    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=True,
                  age_s=STALE_S)
    compress.compress_file(d / "open_orders.json")   # → open_orders.json.zst
    assert not (d / "open_orders.json").exists()
    assert (d / "open_orders.json.zst").is_file()
    # compress_file unlinks the plain file inside the dir, re-bumping the
    # run-dir mtime to now — which trips prune's quiescence guard and skips
    # the dir BEFORE _is_complete is ever consulted, masking a reverted
    # fix. Re-backdate so the age guard passes and completeness is what
    # actually decides this dump's fate (without this the test is vacuous:
    # it stays green even with the plain-name .exists() bug reintroduced).
    backdate(d, STALE_S)

    run_main(tmp_path)
    assert d.exists()
    assert (d / "account_numbers.json").exists()
    assert (d / "open_orders.json.zst").is_file()


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_crashed_dump_no_manifest_no_open_orders_deleted(tmp_path):
    # Crashed before open_orders.json and before any manifest existed:
    # no status, fallback signal absent ⇒ NON_COMPLETE.
    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_statusless_manifest_without_open_orders_deleted(tmp_path):
    # Statusless manifest but no open_orders.json: fallback signal absent
    # ⇒ NON_COMPLETE.
    d = make_dump(tmp_path, OLD_TS, status=None, open_orders=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_wins_over_present_open_orders(tmp_path):
    # Forward status is authoritative: even with open_orders.json present
    # (crash after the terminal artefact but before the terminal
    # manifest), status="in-progress" ⇒ NON_COMPLETE. The terminal
    # manifest is the completeness signal — this mirrors the fleet
    # convention (a clean finish always overwrites the marker).
    d = make_dump(tmp_path, OLD_TS, status="in-progress", open_orders=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)
