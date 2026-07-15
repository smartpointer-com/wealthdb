"""
Unit tests for prune.py (schwab-web).

tmp_path bronze trees matching download.walk()'s layout. Covers:
  * complete dump: screenshots/ deleted, load inputs
    (statements/, transactions/ exports + run.json) untouched
  * complete dump without screenshots/: no-op
  * non-complete dumps (absent run.json / status != complete /
    status="dry-run") deleted whole once quiescent
  * statusless manifests: a real dump (dry_run=false) is kept as
    complete; a --dry-run shell (dry_run=true) is pruned
  * in-flight guard keyed on write activity, not slug age: a long
    walk (old slug, fresh writes) is protected
  * unreadable / corrupt run.json is UNKNOWN → skipped, never
    deleted (a read failure is not evidence of incompleteness)
  * calendar-invalid slug is skipped, not a crash
  * symlinked screenshots / run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root are never touched
  * validate_target refuses paths outside the expected shapes

The debug_subdirs = ("screenshots",) config mirrors the
tx-history landing HTML baseline download.walk() writes under
<run>/screenshots/ only with --debug. debug_globs =
("transactions/*/page-*.html",) additionally reclaims the orphans
an older ungated capture left inside the
transactions/<suffix>/ load-input dir; every other file in that
dir (the .csv/.json/.xml exports + more-details.json) is a load
input and must NEVER be a prune target.
"""

from __future__ import annotations

import json
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default
FRESH_FMT = "%Y%m%dT%H%M%SZ"
SUFFIX = "NNN"                        # synthetic account suffix


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, status: str | None = "complete",
              dry_run: bool = False, screenshots: bool = True,
              run_json: bool = True, age_s: float = 0.0) -> Path:
    """Build a bronze run dir shaped like download.walk() output.

    ``status`` is the run.json ``status`` field (``None`` ⇒ a
    statusless manifest). ``dry_run`` sets the manifest's ``dry_run``
    bool (the fallback completeness signal).
    ``age_s`` backdates every file/dir mtime so the write-activity
    guard sees an abandoned dump; the default (0) leaves it fresh.
    """
    d = root / slug
    # Load inputs: a statement PDF + the tx-history JSON/CSV export.
    (d / "statements" / SUFFIX).mkdir(parents=True)
    (d / "statements" / SUFFIX / "Brokerage-Statement_2026-04-30.PDF"
     ).write_bytes(b"%PDF-1.4 fake")
    (d / "transactions" / SUFFIX).mkdir(parents=True)
    (d / "transactions" / SUFFIX / "Acct_Transactions_20260101.json"
     ).write_text('{"BrokerageTransactions": []}')
    (d / "transactions" / SUFFIX / "Acct_Transactions_20260101.csv"
     ).write_text("Date,Action\n")
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / f"tx-{SUFFIX}-landing.html").write_text(
            "<html>landing baseline</html>")
    if run_json:
        manifest: dict = {"run_ts": slug, "dry_run": dry_run,
                          "statements": [], "transactions": []}
        if status is not None:
            manifest["status"] = status
        (d / "run.json").write_text(json.dumps(manifest))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


def _load_inputs_intact(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "statements" / SUFFIX
            / "Brokerage-Statement_2026-04-30.PDF").exists()
    assert (d / "transactions" / SUFFIX
            / "Acct_Transactions_20260101.json").exists()
    assert (d / "transactions" / SUFFIX
            / "Acct_Transactions_20260101.csv").exists()


# ============================================================
# Complete dumps: screenshots pruned, inputs kept
# ============================================================

def test_complete_dump_screenshots_pruned_inputs_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    _load_inputs_intact(d)


def test_complete_dump_without_screenshots_untouched(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    assert run_main(tmp_path) == 0
    assert d.exists()
    _load_inputs_intact(d)


def test_fresh_complete_dump_screenshots_still_pruned(tmp_path):
    # The write-activity guard protects non-complete dumps only; a
    # finalised status="complete" means the walk is over and its
    # captures are fair game regardless of freshness.
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    run_main(tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()


def test_legacy_page_html_orphan_reclaimed_inputs_kept(tmp_path):
    # An older ungated capture wrote the landing HTML INSIDE the
    # tx-history load-input dir as page-001.html. It is a debug orphan
    # `load` never reads, reclaimed via debug_globs — while every
    # load-input sibling in that SAME dir (the .json/.csv exports)
    # stays byte-identical.
    d = make_dump(tmp_path, OLD_TS)
    orphan_html = d / "transactions" / SUFFIX / "page-001.html"
    orphan_html.write_text("<html>landing</html>")
    run_main(tmp_path)
    assert not (d / "screenshots").exists()
    assert not orphan_html.exists()          # the orphan is reclaimed
    _load_inputs_intact(d)                    # its load-input siblings stay


def test_non_page_html_sibling_in_transactions_kept(tmp_path):
    # The glob is scoped to page-*.html; any other file in the
    # tx-history dir (e.g. more-details.json) is a load input and must
    # never be touched.
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    sidecar = d / "transactions" / SUFFIX / "more-details.json"
    sidecar.write_text('[{"row_key": "k"}]')
    run_main(tmp_path)
    assert sidecar.exists()
    _load_inputs_intact(d)


def test_legacy_page_html_kept_in_non_complete_dump_dir(tmp_path):
    # In a non-complete dump the whole dir is the deletion unit; the
    # glob only fires from complete dumps. A fresh (age-guarded)
    # non-complete dump keeps everything, page-html included.
    d = make_dump(tmp_path, fresh_slug(age_s=60), run_json=False)
    orphan_html = d / "transactions" / SUFFIX / "page-001.html"
    orphan_html.write_text("<html>landing</html>")
    run_main(tmp_path)
    assert d.exists()
    assert orphan_html.exists()


def test_page_html_symlink_in_transactions_not_deleted(tmp_path):
    # A symlink whose name matches the glob is never a target.
    external = tmp_path / "ext.html"
    external.write_text("precious")
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    link = d / "transactions" / SUFFIX / "page-001.html"
    link.symlink_to(external)
    run_main(tmp_path)
    assert link.is_symlink()
    assert external.read_text() == "precious"
    _load_inputs_intact(d)


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="dry-run", screenshots=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker
    # download.walk() drops at run-dir creation); once quiescent it
    # is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress",
                  screenshots=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# Statusless manifests: dry_run splits keep vs prune
# ============================================================

def test_statusless_real_dump_kept_as_complete(tmp_path):
    # schwab-web writes run.json incrementally, so presence alone
    # proves nothing; for a statusless manifest the signal is
    # dry_run=false → a real dump: classify COMPLETE, keep its load
    # inputs, prune only its debug captures.
    d = make_dump(tmp_path, OLD_TS, status=None, dry_run=False,
                  screenshots=True, age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()
    _load_inputs_intact(d)


def test_statusless_dry_run_shell_pruned(tmp_path):
    # A statusless run.json with dry_run=true is a --dry-run shell:
    # non-complete → the whole dir is prunable once quiescent.
    d = make_dump(tmp_path, OLD_TS, status=None, dry_run=True,
                  screenshots=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # A walk whose slug is hours old but which is still writing
    # artefacts (fresh mtimes, no terminal status) must NOT be
    # classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "transactions" / SUFFIX
            / "Acct_Transactions_20260101.json").exists()


def test_fresh_incomplete_dump_kept_by_age_guard(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), run_json=False)
    run_main(tmp_path)
    assert d.exists()


def test_stale_incomplete_dump_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path, "--min-age-hours", "0")
    assert not d.exists()


# ============================================================
# Unreadable / corrupt / invalid manifests: never deleted
# ============================================================

def test_corrupt_json_run_json_kept(tmp_path):
    # Corrupt bytes are UNKNOWN, not evidence of incompleteness —
    # skip, never delete (could be a complete dump with a mangled
    # manifest).
    d = make_dump(tmp_path, OLD_TS, screenshots=False, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert (d / "statements" / SUFFIX
            / "Brokerage-Statement_2026-04-30.PDF").exists()


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory,
    # a deterministic OSError) must not classify the dump as
    # non-complete and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                  age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "transactions" / SUFFIX
            / "Acct_Transactions_20260101.json").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date
    # (Feb 30) must be skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    screenshots=False, age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                     age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert bad.exists()          # unparseable age → left alone
    assert not good.exists()     # the valid stale dump still pruned


# ============================================================
# Symlinks: left alone
# ============================================================

def test_symlinked_screenshots_not_followed(tmp_path):
    external = tmp_path / "external"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    (d / "screenshots").symlink_to(external, target_is_directory=True)
    run_main(tmp_path)
    assert (d / "screenshots").is_symlink()
    assert (external / "keep.txt").exists()


def test_symlinked_run_dir_skipped(tmp_path):
    external = tmp_path / "external_run"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    run_main(tmp_path)
    assert link.is_symlink()
    assert (external / "keep.txt").exists()


# ============================================================
# --dry-run and root-level safety
# ============================================================

def test_dry_run_deletes_nothing(tmp_path):
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert (complete / "screenshots").exists()
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS)
    (tmp_path / "schwab-web.db").write_bytes(b"sqlite fake")
    stray = tmp_path / "notes"
    stray.mkdir()
    (stray / "keep.txt").write_text("keep")
    run_main(tmp_path)
    assert (tmp_path / "schwab-web.db").exists()
    assert (stray / "keep.txt").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_expected_shapes(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "screenshots").mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "schwab-web.db", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(
            tmp_path / OLD_TS / "transactions", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(
            tmp_path / OLD_TS / "statements", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(
            tmp_path.parent / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
