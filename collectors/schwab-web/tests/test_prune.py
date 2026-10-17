"""
Unit tests for what schwab-web's prune config decides; the shared engine
(age guard, symlinks, unreadable manifests, --dry-run, target checks) is
tested in collectorkit. The tests drive the collector's `prune.main`
and `CONFIG`.

tmp_path bronze trees matching download.walk()'s layout. Covers:
  * complete dump: screenshots/ deleted, load inputs
    (statements/, transactions/ exports + run.json) untouched
  * debug_globs: transactions/*/page-*.html reclaimed from a complete
    dump, every other file in that load-input dir kept
  * no run.json: a non-complete dump, deleted once quiescent
  * statusless manifests: a real dump (dry_run=false) is kept as
    complete; a --dry-run shell (dry_run=true) is pruned
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
SUFFIX = "NNN"                        # synthetic account suffix


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


def test_legacy_page_html_orphan_reclaimed_inputs_kept(tmp_path):
    # Landing HTML INSIDE the tx-history load-input dir
    # (page-001.html) is a debug artefact `load` never reads,
    # reclaimed via debug_globs — while every load-input sibling in
    # that SAME dir (the .json/.csv exports) stays byte-identical.
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


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
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
# Target validation
# ============================================================

def test_validate_target_accepts_the_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)
