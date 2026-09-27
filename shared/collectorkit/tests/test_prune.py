"""Unit tests for collectorkit.prune — the shared bronze-prune engine.

Ports the fidelity-web prune coverage onto the parameterized helper, and
adds coverage for the variation the helper exists to absorb:

  * complete dump: debug subdirs deleted, load inputs untouched
  * complete dump without a debug subdir: no-op
  * non-complete dumps (absent manifest / status != complete) deleted
    whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes) is protected
  * unreadable / corrupt / non-object manifest is UNKNOWN → skipped,
    never deleted (a read failure is not evidence of incompleteness)
  * calendar-invalid slug is skipped, not a crash
  * symlinked debug subdir / run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root are never touched
  * validate_target refuses paths outside the configured shapes
  * empty debug_subdirs: only non-complete whole dirs are pruned
  * manifest_name=None collectors classify from a terminal artefact
  * legacy statusless-complete dumps keep their inputs
  * a file-shaped debug artefact (trace.zip) is pruned
  * multiple debug subdirs
  * status_classification unit behaviour
  * the host-side debug cache (--debug-dir, outside bronze): aged-out
    files AND dirs reclaimed, fresh ones protected, --dry-run inert,
    missing/absent dir a no-op, bronze untouched when the flag is absent,
    and a --debug-dir overlapping bronze refused before anything is deleted
"""

from __future__ import annotations

import json
import logging
import os
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

from collectorkit import prune

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default
FRESH_FMT = "%Y%m%dT%H%M%SZ"


# ============================================================
# Test configs
# ============================================================

def _legacy_complete(run_dir, meta):
    # Statusless dumps are complete iff the terminal artefact is present.
    return run_dir is not None and (run_dir / "positions" / "positions.csv").exists()


def _is_complete(run_dir, meta):
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=_legacy_complete)


# A fidelity-web-shaped collector: one debug subdir, run.json manifest.
CFG = prune.PruneConfig(
    debug_subdirs=("screenshots",),
    is_complete=_is_complete,
)

# A collector that writes no bronze-resident debug artefact.
CFG_NO_DEBUG = prune.PruneConfig(
    debug_subdirs=(),
    is_complete=_is_complete,
)

# A collector with no manifest file at all — completeness keys purely on
# the terminal artefact (mirrors schwab-api / ubs-psn, which write no
# run.json historically).
CFG_NO_MANIFEST = prune.PruneConfig(
    debug_subdirs=(),
    manifest_name=None,
    is_complete=lambda run_dir, meta: prune.status_classification(
        meta, run_dir=run_dir,
        legacy_complete=lambda rd, m: (rd / "accounts_positions.json").exists()),
)

# A collector with a debug subdir *and* a file-shaped debug artefact.
CFG_MULTI = prune.PruneConfig(
    debug_subdirs=("screenshots", "trace.zip"),
    is_complete=_is_complete,
)

# A collector that also reclaims a deep-nested debug FILE via a glob two
# levels down — mirrors schwab-web's legacy transactions/*/page-*.html
# orphans, which a top-level debug_subdirs name cannot reach.
CFG_GLOB = prune.PruneConfig(
    debug_subdirs=("screenshots",),
    debug_globs=("transactions/*/page-*.html",),
    is_complete=_is_complete,
)


# ============================================================
# Fixtures
# ============================================================

def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, status: str | None = "complete",
              screenshots: bool = True, run_json: bool = True,
              age_s: float = 0.0, terminal: bool = True) -> Path:
    """Build a bronze run dir. ``age_s`` backdates every mtime so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh. ``terminal`` writes the legacy terminal artefact (positions
    CSV) that the statusless-complete fallback keys on."""
    d = root / slug
    (d / "positions").mkdir(parents=True)
    if terminal:
        (d / "positions" / "positions.csv").write_text("a,b\n1,2\n")
    (d / "documents").mkdir()
    (d / "documents" / "Statement_2026.pdf").write_bytes(b"%PDF-1.4 fake")
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "20260101T010101Z-landed.html").write_text("<html>x</html>")
        (shots / "20260101T010101Z-landed.png").write_bytes(b"\x89PNG fake")
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


def run_main(cfg, root: Path, *extra: str) -> int:
    return prune.main(cfg, ["--bronze-dir", str(root), *extra])


# Synthetic account suffixes only — never a real one (repo AGENTS.md §4).
SUFFIX_A = "111"
SUFFIX_B = "222"


def _sha(path: Path) -> str:
    import hashlib
    return hashlib.sha256(path.read_bytes()).hexdigest()


def add_tx_account(dump: Path, suffix: str, *, page_html: bool = True,
                   siblings: bool = True) -> Path:
    """Add a ``transactions/<suffix>/`` dir carrying a legacy
    ``page-*.html`` orphan alongside the load-input siblings the
    tx-history loader actually reads (``more-details.json`` + the
    ``.csv``/``.json``/``.xml`` exports). Returns the account dir."""
    acct = dump / "transactions" / suffix
    acct.mkdir(parents=True)
    if page_html:
        (acct / "page-001.html").write_text("<html>legacy landing</html>")
    if siblings:
        (acct / "more-details.json").write_text('[{"row_key": "k"}]')
        (acct / f"Acct_XXX{suffix}_Transactions_20260101.json").write_text(
            '{"BrokerageTransactions": []}')
        (acct / f"Acct_XXX{suffix}_Transactions_20260101.csv").write_text(
            "Date,Amount\n")
        (acct / f"Acct_XXX{suffix}_Transactions_20260101.xml").write_text(
            "<txns/>")
    return acct


# ============================================================
# Complete dumps: debug pruned, inputs kept
# ============================================================

def test_complete_dump_debug_pruned_inputs_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(CFG, tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions.csv").exists()
    assert (d / "documents" / "Statement_2026.pdf").exists()
    assert (d / "run.json").exists()


def test_complete_dump_without_debug_untouched(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    assert run_main(CFG, tmp_path) == 0
    assert d.exists()
    assert (d / "positions" / "positions.csv").exists()


def test_fresh_complete_dump_debug_still_pruned(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    run_main(CFG, tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()


def test_empty_debug_subdirs_keeps_complete_dump_whole(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    run_main(CFG_NO_DEBUG, tmp_path)
    assert (d / "screenshots").exists()      # not in debug_subdirs → kept
    assert (d / "positions" / "positions.csv").exists()


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, terminal=False,
                  age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="dry-run", screenshots=False,
                  age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="in-progress",
                  screenshots=False, age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_status_missing_key_no_legacy_signal_deleted(tmp_path):
    # Statusless AND no terminal artefact → NON_COMPLETE → deleted.
    d = make_dump(tmp_path, OLD_TS, status=None, screenshots=False,
                  terminal=False, age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_statusless_legacy_complete_dump_inputs_kept(tmp_path):
    # Statusless but the terminal artefact is present → COMPLETE via the
    # legacy fallback: prune its debug, keep its inputs.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions.csv").exists()


# ============================================================
# No-manifest collector (schwab-api / ubs-psn shape)
# ============================================================

def test_no_manifest_terminal_artefact_present_kept(tmp_path):
    d = tmp_path / OLD_TS
    d.mkdir()
    (d / "accounts_positions.json").write_text("{}")
    backdate(d, STALE_S)
    run_main(CFG_NO_MANIFEST, tmp_path)
    assert d.exists()
    assert (d / "accounts_positions.json").exists()


def test_no_manifest_terminal_artefact_absent_deleted(tmp_path):
    d = tmp_path / OLD_TS
    d.mkdir()
    (d / "transactions_000.json").write_text("[]")   # partial: no terminal
    backdate(d, STALE_S)
    run_main(CFG_NO_MANIFEST, tmp_path)
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, terminal=False,
                  age_s=0.0)
    run_main(CFG, tmp_path)
    assert d.exists()


def test_fresh_incomplete_dump_kept_by_age_guard(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), run_json=False,
                  terminal=False)
    run_main(CFG, tmp_path)
    assert d.exists()


def test_stale_incomplete_dump_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, terminal=False,
                  age_s=STALE_S)
    run_main(CFG, tmp_path, "--min-age-hours", "0")
    assert not d.exists()


# ============================================================
# Unreadable / corrupt / non-object manifests: never deleted
# ============================================================

def test_corrupt_json_run_json_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=False, terminal=False,
                  age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(CFG, tmp_path)
    assert d.exists()
    assert (d / "positions").exists()


def test_non_object_json_run_json_kept(tmp_path):
    # A JSON array/scalar is not a manifest object → UNKNOWN, never deleted.
    d = make_dump(tmp_path, OLD_TS, screenshots=False, terminal=False,
                  age_s=STALE_S)
    (d / "run.json").write_text("[1, 2, 3]")
    run_main(CFG, tmp_path)
    assert d.exists()


def test_unreadable_run_json_kept(tmp_path):
    # run.json is a directory → deterministic OSError on read → UNKNOWN.
    d = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                  terminal=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(CFG, tmp_path)
    assert d.exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    screenshots=False, terminal=False, age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                     terminal=False, age_s=STALE_S)
    assert run_main(CFG, tmp_path) == 0
    assert bad.exists()
    assert not good.exists()


# ============================================================
# Symlinks: left alone
# ============================================================

def test_symlinked_debug_not_followed(tmp_path):
    external = tmp_path / "external"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    (d / "screenshots").symlink_to(external, target_is_directory=True)
    run_main(CFG, tmp_path)
    assert (d / "screenshots").is_symlink()
    assert (external / "keep.txt").exists()


def test_symlinked_run_dir_skipped(tmp_path):
    external = tmp_path / "external_run"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    run_main(CFG, tmp_path)
    assert link.is_symlink()
    assert (external / "keep.txt").exists()


# ============================================================
# File-shaped debug artefacts + multiple subdirs
# ============================================================

def test_file_shaped_debug_artefact_pruned(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=True)
    (d / "trace.zip").write_bytes(b"PK\x03\x04 fake-trace")
    run_main(CFG_MULTI, tmp_path)
    assert not (d / "screenshots").exists()
    assert not (d / "trace.zip").exists()
    assert (d / "positions" / "positions.csv").exists()


# ============================================================
# --dry-run and root-level safety
# ============================================================

def test_dry_run_deletes_nothing(tmp_path):
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        terminal=False, age_s=STALE_S)
    run_main(CFG, tmp_path, "--dry-run")
    assert (complete / "screenshots").exists()
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS)
    shared = tmp_path / "manual"
    shared.mkdir()
    (shared / "note.txt").write_text("sig")
    (tmp_path / "collector.db").write_bytes(b"sqlite fake")
    run_main(CFG, tmp_path)
    assert (shared / "note.txt").exists()
    assert (tmp_path / "collector.db").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(CFG, ["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_expected_shapes(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "screenshots").mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path, CFG)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path, CFG)


def test_validate_target_refuses_stray_paths(tmp_path):
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "manual", tmp_path, CFG)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / OLD_TS / "positions", tmp_path, CFG)
    with pytest.raises(SystemExit):
        prune.validate_target(
            tmp_path.parent / OLD_TS / "screenshots", tmp_path, CFG)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path, CFG)


# ============================================================
# status_classification unit behaviour
# ============================================================

def test_status_classification_states():
    assert prune.status_classification({"status": "complete"})[0] == prune.COMPLETE
    assert prune.status_classification({"status": "in-progress"})[0] == prune.NON_COMPLETE
    assert prune.status_classification({"status": "dry-run"})[0] == prune.NON_COMPLETE
    assert prune.status_classification(None)[0] == prune.NON_COMPLETE
    assert prune.status_classification({})[0] == prune.NON_COMPLETE


def test_status_classification_legacy_fallback():
    ok = prune.status_classification(
        {}, run_dir=Path("/x"), legacy_complete=lambda rd, m: True)
    assert ok[0] == prune.COMPLETE
    none_ok = prune.status_classification(
        None, run_dir=Path("/x"), legacy_complete=lambda rd, m: True)
    assert none_ok[0] == prune.COMPLETE
    # A present non-complete status is never overridden by the fallback.
    still_nc = prune.status_classification(
        {"status": "dry-run"}, legacy_complete=lambda rd, m: True)
    assert still_nc[0] == prune.NON_COMPLETE


# ============================================================
# debug_globs: deep-nested debug FILES reclaimed from complete dumps
# ============================================================

def test_debug_glob_deletes_page_html_keeps_siblings(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    acct = add_tx_account(d, SUFFIX_A)
    # Shasums of every load-input sibling in the SAME deep dir.
    sib_shas = {p.name: _sha(p) for p in acct.iterdir()
                if p.name != "page-001.html"}
    run_json_sha = _sha(d / "run.json")
    assert run_main(CFG_GLOB, tmp_path) == 0
    # The orphan is gone …
    assert not (acct / "page-001.html").exists()
    # … while every load-input sibling next to it is byte-identical.
    for name, sha in sib_shas.items():
        assert (acct / name).exists(), name
        assert _sha(acct / name) == sha, name
    # run.json and the top-level debug-subdir handling are unaffected.
    assert _sha(d / "run.json") == run_json_sha
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions.csv").exists()


def test_debug_glob_prunes_every_account(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    a = add_tx_account(d, SUFFIX_A)
    b = add_tx_account(d, SUFFIX_B)
    run_main(CFG_GLOB, tmp_path)
    assert not (a / "page-001.html").exists()
    assert not (b / "page-001.html").exists()
    assert (a / "more-details.json").exists()
    assert (b / "more-details.json").exists()


def test_debug_glob_dry_run_deletes_nothing(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    acct = add_tx_account(d, SUFFIX_A)
    run_main(CFG_GLOB, tmp_path, "--dry-run")
    assert (acct / "page-001.html").exists()
    assert (d / "screenshots").exists()


def test_debug_glob_symlink_not_deleted(tmp_path):
    external = tmp_path / "external.html"
    external.write_text("precious")
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    acct = add_tx_account(d, SUFFIX_A, page_html=False)
    (acct / "page-001.html").symlink_to(external)
    run_main(CFG_GLOB, tmp_path)
    assert (acct / "page-001.html").is_symlink()
    assert external.exists() and external.read_text() == "precious"


def test_debug_glob_empty_default_leaves_page_html(tmp_path):
    # Regression guard: the default (empty debug_globs) config must not
    # touch a page-*.html file — every existing collector relies on this.
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    acct = add_tx_account(d, SUFFIX_A)
    run_main(CFG, tmp_path)
    assert (acct / "page-001.html").exists()
    assert (acct / "more-details.json").exists()


def test_debug_glob_symlinked_intermediate_dir_not_followed(tmp_path):
    # A `*` component of a debug glob must not follow a symlinked
    # INTERMEDIATE directory: Path.glob would traverse it, but the file
    # behind it lives outside the bronze tree, so deleting it would violate
    # the never-follow-symlinks envelope. Here transactions/<suffix> is a
    # symlink to an external dir that happens to contain a page-001.html.
    external = tmp_path / "external"
    external.mkdir()
    (external / "page-001.html").write_text("precious external content")
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    (d / "transactions").mkdir()
    (d / "transactions" / SUFFIX_A).symlink_to(
        external, target_is_directory=True)

    run_main(CFG_GLOB, tmp_path)

    # The external file (reachable only through the symlinked dir) survives,
    # and the symlink itself is untouched.
    assert (external / "page-001.html").exists()
    assert (external / "page-001.html").read_text() == "precious external content"
    assert (d / "transactions" / SUFFIX_A).is_symlink()
    # And validate_target refuses that path outright (belt-and-braces).
    with pytest.raises(SystemExit):
        prune.validate_target(
            d / "transactions" / SUFFIX_A / "page-001.html", tmp_path, CFG_GLOB)


def test_debug_glob_not_applied_to_unknown_dump(tmp_path):
    # A corrupt-manifest dump is UNKNOWN → skipped wholesale; debug_globs
    # only fire in the COMPLETE branch, so the orphan survives.
    d = make_dump(tmp_path, OLD_TS, screenshots=False, terminal=False,
                  age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    acct = add_tx_account(d, SUFFIX_A)
    run_main(CFG_GLOB, tmp_path)
    assert d.exists()
    assert (acct / "page-001.html").exists()


def test_debug_glob_non_complete_dump_removed_whole(tmp_path):
    # A stale non-complete dump is removed whole (page files included) —
    # debug_globs don't change that path. Backdate AFTER adding tx files
    # so the write-activity guard sees an abandoned dump.
    d = make_dump(tmp_path, OLD_TS, run_json=False, terminal=False,
                  screenshots=False)
    add_tx_account(d, SUFFIX_A)
    backdate(d, STALE_S)
    run_main(CFG_GLOB, tmp_path)
    assert not d.exists()


# ============================================================
# validate_target: debug_globs branch (the irreversible-unlink gate)
# ============================================================

def test_validate_target_accepts_glob_matched_file(tmp_path):
    acct = tmp_path / OLD_TS / "transactions" / SUFFIX_A
    acct.mkdir(parents=True)
    f = acct / "page-001.html"
    f.write_text("<html/>")
    prune.validate_target(f, tmp_path, CFG_GLOB)  # must not raise


def test_validate_target_rejects_deep_non_glob_files(tmp_path):
    acct = tmp_path / OLD_TS / "transactions" / SUFFIX_A
    acct.mkdir(parents=True)
    for name in ("more-details.json",
                 f"Acct_XXX{SUFFIX_A}_Transactions_20260101.json",
                 f"Acct_XXX{SUFFIX_A}_Transactions_20260101.csv",
                 f"Acct_XXX{SUFFIX_A}_Transactions_20260101.xml"):
        (acct / name).write_text("x")
        with pytest.raises(SystemExit):
            prune.validate_target(acct / name, tmp_path, CFG_GLOB)


def test_validate_target_rejects_glob_named_symlink(tmp_path):
    external = tmp_path / "ext.html"
    external.write_text("precious")
    acct = tmp_path / OLD_TS / "transactions" / SUFFIX_A
    acct.mkdir(parents=True)
    link = acct / "page-001.html"
    link.symlink_to(external)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path, CFG_GLOB)
    assert external.exists()


def test_validate_target_glob_cfg_still_accepts_subdir_and_run_dir(tmp_path):
    # The restructure must not regress the debug-subdir / whole-run-dir
    # shapes for a config that also carries debug_globs.
    d = tmp_path / OLD_TS
    (d / "screenshots").mkdir(parents=True)
    prune.validate_target(d, tmp_path, CFG_GLOB)
    prune.validate_target(d / "screenshots", tmp_path, CFG_GLOB)


def test_validate_target_rejects_glob_file_outside_run_dir(tmp_path):
    # A page-*.html whose ancestor is NOT a run-slug dir is refused even
    # though its name matches the glob (no ancestor run dir under bronze).
    stray = tmp_path / "notarun" / "transactions" / SUFFIX_A
    stray.mkdir(parents=True)
    f = stray / "page-001.html"
    f.write_text("x")
    with pytest.raises(SystemExit):
        prune.validate_target(f, tmp_path, CFG_GLOB)


# ============================================================
# Host-side debug cache (--debug-dir): outside the bronze tree
# ============================================================

TRACE_BUNDLE = "20260101T010101Z-trace"      # a trace bundle: a DIR
SCREENSHOT = "20260101T010101Z-consent.png"  # a loose capture: a FILE


def make_debug_cache(root: Path, age_s: float = 0.0) -> Path:
    """A host-side debug cache holding both shapes a run leaves there: a
    Playwright trace bundle (a dir) and a loose screenshot (a file).
    ``age_s`` backdates both past the guard; the default leaves them fresh,
    as an in-flight run's captures would be."""
    root.mkdir(parents=True)
    bundle = root / TRACE_BUNDLE
    bundle.mkdir()
    (bundle / "trace.network").write_text('{"trace": "synthetic"}')
    (bundle / "0-screenshot.png").write_bytes(b"\x89PNG fake")
    (root / SCREENSHOT).write_bytes(b"\x89PNG fake")
    if age_s:
        backdate(bundle, age_s)
        backdate(root / SCREENSHOT, age_s)
    return root


def make_bronze(tmp_path: Path) -> Path:
    """A bronze root beside (not containing) the debug cache — the real
    layout, where the two are unrelated trees."""
    root = tmp_path / "bronze"
    root.mkdir()
    return root


def test_debug_cache_stale_dir_and_file_both_reclaimed(tmp_path):
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    assert run_main(CFG, root, "--debug-dir", str(dbg)) == 0
    assert not (dbg / TRACE_BUNDLE).exists()   # a dir goes via rmtree …
    assert not (dbg / SCREENSHOT).exists()     # … a file via unlink
    assert dbg.is_dir()                        # the cache root itself stays


def test_debug_cache_fresh_entries_kept_by_age_guard(tmp_path):
    # The guard is the whole point: a run writing its captures right now
    # must not have them deleted out from under it.
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug")
    run_main(CFG, root, "--debug-dir", str(dbg))
    assert (dbg / TRACE_BUNDLE).exists()
    assert (dbg / SCREENSHOT).exists()


def test_debug_cache_in_flight_bundle_kept_despite_old_dir_mtime(tmp_path):
    # A trace bundle whose own mtime is old but whose contents are being
    # written right now is in flight. Quiescence keys on the newest mtime
    # anywhere under the entry, so it survives.
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    os.utime(dbg / TRACE_BUNDLE / "trace.network", None)
    run_main(CFG, root, "--debug-dir", str(dbg))
    assert (dbg / TRACE_BUNDLE).exists()
    assert not (dbg / SCREENSHOT).exists()   # the quiet sibling still goes


def test_debug_cache_zero_min_age_reclaims_fresh_entries(tmp_path):
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug")
    run_main(CFG, root, "--debug-dir", str(dbg), "--min-age-hours", "0")
    assert not (dbg / TRACE_BUNDLE).exists()
    assert not (dbg / SCREENSHOT).exists()


def test_debug_cache_dry_run_deletes_nothing(tmp_path):
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    run_main(CFG, root, "--debug-dir", str(dbg), "--dry-run")
    assert (dbg / TRACE_BUNDLE / "trace.network").exists()
    assert (dbg / SCREENSHOT).exists()


def test_debug_cache_missing_dir_is_clean_noop(tmp_path):
    # Debug output is opt-in, so most runs write no cache at all: a
    # --debug-dir that was never created is a no-op, not an error.
    root = make_bronze(tmp_path)
    d = make_dump(root, OLD_TS)
    assert run_main(CFG, root, "--debug-dir",
                    str(tmp_path / "never-written")) == 0
    assert not (d / "screenshots").exists()          # bronze still pruned
    assert (d / "positions" / "positions.csv").exists()


def test_debug_cache_empty_dir_is_clean_noop(tmp_path):
    root = make_bronze(tmp_path)
    dbg = tmp_path / "debug"
    dbg.mkdir()
    assert run_main(CFG, root, "--debug-dir", str(dbg)) == 0
    assert dbg.is_dir()


def test_debug_cache_symlinked_entry_not_followed(tmp_path):
    external = tmp_path / "external"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    root = make_bronze(tmp_path)
    dbg = tmp_path / "debug"
    dbg.mkdir()
    link = dbg / "20260101T010101Z-trace"
    link.symlink_to(external, target_is_directory=True)
    backdate(link, STALE_S)
    run_main(CFG, root, "--debug-dir", str(dbg))
    assert link.is_symlink()
    assert (external / "keep.txt").exists()


def test_debug_cache_reclaim_leaves_bronze_alone(tmp_path):
    # The two reclaims are independent: the debug cache going does not cost
    # a complete dump its load inputs.
    root = make_bronze(tmp_path)
    d = make_dump(root, OLD_TS)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    run_main(CFG, root, "--debug-dir", str(dbg))
    assert not (dbg / TRACE_BUNDLE).exists()
    assert (d / "positions" / "positions.csv").exists()
    assert (d / "documents" / "Statement_2026.pdf").exists()
    assert (d / "run.json").exists()


def test_no_debug_dir_flag_leaves_bronze_reclaim_unchanged(tmp_path):
    # The flag is optional. Absent it, the bronze prune behaves exactly as
    # before: debug subdirs of a complete dump go, load inputs stay, and a
    # stale non-complete dump goes whole.
    root = make_bronze(tmp_path)
    complete = make_dump(root, OLD_TS)
    crashed = make_dump(root, "20260102T010000Z", run_json=False,
                        terminal=False, age_s=STALE_S)
    assert run_main(CFG, root) == 0
    assert not (complete / "screenshots").exists()
    assert (complete / "positions" / "positions.csv").exists()
    assert not crashed.exists()


def test_no_debug_dir_flag_never_touches_a_cache(tmp_path):
    # Without --debug-dir the cache is not even looked at.
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    run_main(CFG, root)
    assert (dbg / TRACE_BUNDLE / "trace.network").exists()
    assert (dbg / SCREENSHOT).exists()


def test_debug_cache_only_still_reports_freed(tmp_path, capsys):
    # An empty bronze plus a reclaimable cache is NOT "nothing to prune" —
    # the summary must account for the debug bytes too.
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    run_main(CFG, root, "--debug-dir", str(dbg))
    out = capsys.readouterr().out
    assert "nothing to prune" not in out
    assert "freed:" in out
    assert "2 paths" in out          # the bundle dir + the loose file


def test_debug_cache_dry_run_uses_the_would_verbs(tmp_path, capsys):
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    run_main(CFG, root, "--debug-dir", str(dbg), "--dry-run")
    out = capsys.readouterr().out
    assert "would delete" in out and "would free" in out
    assert "deleting" not in out


def test_debug_cache_empty_bronze_and_no_cache_is_nothing_to_prune(tmp_path,
                                                                   capsys):
    root = make_bronze(tmp_path)
    run_main(CFG, root, "--debug-dir", str(tmp_path / "never-written"))
    assert "nothing to prune" in capsys.readouterr().out


# ============================================================
# validate_debug_target: the irreversible-delete gate for the cache
# ============================================================

def test_validate_debug_target_accepts_direct_entries(tmp_path):
    dbg = make_debug_cache(tmp_path / "debug")
    prune.validate_debug_target(dbg / TRACE_BUNDLE, dbg)   # must not raise
    prune.validate_debug_target(dbg / SCREENSHOT, dbg)


def test_validate_debug_target_refuses_nested_and_stray_paths(tmp_path):
    dbg = make_debug_cache(tmp_path / "debug")
    with pytest.raises(SystemExit):      # one level down, not a direct entry
        prune.validate_debug_target(dbg / TRACE_BUNDLE / "trace.network", dbg)
    with pytest.raises(SystemExit):      # outside the cache entirely
        prune.validate_debug_target(tmp_path / "bronze", dbg)


def test_debug_dir_overlapping_bronze_refused(tmp_path):
    # The cache is reclaimed with no completeness check, so aiming it at
    # bronze would delete complete dumps whole — load inputs and all.
    # Refused in every overlapping shape, before anything is deleted.
    root = make_bronze(tmp_path)
    d = make_dump(root, OLD_TS)
    for bad in (root,                       # exactly the bronze root
                root / OLD_TS,              # a run dir inside bronze
                tmp_path):                  # an ancestor of the bronze root
        with pytest.raises(SystemExit):
            run_main(CFG, root, "--debug-dir", str(bad))
    # Nothing was touched on the way out.
    assert (d / "positions" / "positions.csv").exists()
    assert (d / "screenshots").exists()


def test_debug_dir_sibling_of_bronze_accepted(tmp_path):
    # The real layout — two unrelated trees — is not caught by the guard.
    root = make_bronze(tmp_path)
    dbg = make_debug_cache(tmp_path / "debug", age_s=STALE_S)
    assert run_main(CFG, root, "--debug-dir", str(dbg)) == 0
    assert not (dbg / TRACE_BUNDLE).exists()


def test_validate_debug_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    dbg = tmp_path / "debug"
    dbg.mkdir()
    link = dbg / "trace"
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_debug_target(link, dbg)


# ---------------------------------------------------------------------------
# -v / --verbose
#
# Every login/download/load surface takes -v; prune/recompress/dedup did not,
# so `wealthdb-collect <source> prune -v` died at argparse. It is wired to a
# real decision trace rather than accepted as a no-op: the plan prints only
# what is deleted or explicitly skipped, so a COMPLETE dump is kept without a
# word — -v is the only way to see the walk considered it at all.
# ---------------------------------------------------------------------------

def test_verbose_flag_is_accepted(tmp_path):
    make_dump(tmp_path, OLD_TS)
    assert run_main(CFG, tmp_path, "-v") == 0
    assert run_main(CFG, tmp_path, "--verbose") == 0


def test_verbose_traces_a_kept_complete_dump(tmp_path, caplog):
    # The decision the plan makes in total silence: a complete dump is kept
    # and never printed, so this trace is the only evidence it was examined.
    make_dump(tmp_path, OLD_TS, status="complete")
    with caplog.at_level(logging.DEBUG, logger="collectorkit.prune"):
        run_main(CFG, tmp_path, "-v")
    traced = [r for r in caplog.records if r.name == "collectorkit.prune"]
    assert any(OLD_TS in r.getMessage() and "complete" in r.getMessage()
               for r in traced), [r.getMessage() for r in traced]


def test_the_trace_is_debug_level_so_it_is_off_by_default(tmp_path, caplog):
    # What keeps a bare `prune` quiet is the trace living at DEBUG, below
    # configure_logging's default INFO. (Asserting silence directly would
    # only prove caplog's own level handling: logging.basicConfig is a no-op
    # once the root logger has handlers, so a second call in-process cannot
    # lower the level back.)
    make_dump(tmp_path, OLD_TS, status="complete")
    with caplog.at_level(logging.DEBUG, logger="collectorkit.prune"):
        run_main(CFG, tmp_path, "-v")
    traced = [r for r in caplog.records if r.name == "collectorkit.prune"]
    assert traced, "expected a trace record"
    assert all(r.levelno == logging.DEBUG for r in traced)


def test_verbose_does_not_change_what_is_deleted(tmp_path):
    # The trace is diagnostics, never a behaviour switch: the same tree
    # prunes identically with and without -v.
    loud = tmp_path / "loud"
    quiet = tmp_path / "quiet"
    for root in (loud, quiet):
        root.mkdir()
        make_dump(root, OLD_TS, run_json=False, terminal=False, age_s=STALE_S)
    run_main(CFG, loud, "-v")
    run_main(CFG, quiet)
    assert sorted(p.name for p in loud.iterdir()) == \
           sorted(p.name for p in quiet.iterdir())
