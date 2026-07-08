"""Unit tests for collectorkit.docdedup — the download-avoidance engine.

No network: the fetch is a stub closure that writes a known byte-string, so we
can assert exactly when it runs. Mirrors the dedup suite's fixture style
(synthetic run dirs + a run.json status) and covers the plan's §7.1 recipe:

  * SkipSet.derive indexes COMPLETE prior runs, ignores non-complete ones,
    self-heals a deleted file, is multiset-aware, honours the freshness window
    (default 35), and excludes the current run.
  * link-mode hardlinks a prior identical file (fetch NOT called), misses fetch,
    --force always fetches, falls through to fetch on an os.link error, honours
    the optional size-guard (mismatch → fetch), and leaves a real (non-symlink)
    self-contained file.
  * fetch-verify-dedup ALWAYS fetches, hardlinks an unchanged (byte-identical)
    doc, keeps a changed doc's fresh bytes (the re-issued-K-1 case), keeps a new
    doc, and keeps the fetched bytes when the relink hiccups.
  * process() dispatches by document class: immutable -> link, tax / mutable /
    unknown -> fetch-verify (changed kept / unchanged hardlinked).

Synthetic slugs / ids / bytes only — no real deal ids, company names, or
figures.
"""
from __future__ import annotations

import json
import os
from datetime import date
from pathlib import Path

from collectorkit import bronze, docdedup

OLD_A = "20260101T010000Z"
OLD_B = "20260102T010000Z"
OLD_C = "20260103T010000Z"

# Synthetic deal-slug / doc-slug: sha256(id)[:16]-shaped hex, no real id.
DEAL = "0123456789abcdef"
DOC1 = "fedcba9876543210"
DOC2 = "aaaabbbbccccdddd"

BODY = b"%PDF-1.4 synthetic document body " + b"x" * 200
BODY2 = b"%PDF-1.4 a different synthetic body " + b"y" * 200


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

def _run(root: Path, slug: str, files: dict[str, bytes],
         status: str | None = "complete", docs: list | None = None) -> Path:
    """Build a synthetic bronze run dir. ``files`` maps run-dir-relative paths
    to bytes; ``docs`` (optional) seeds the manifest-driven extract's date."""
    d = root / slug
    d.mkdir(parents=True, exist_ok=True)
    for rel, body in files.items():
        p = d / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(body)
    meta: dict = {"source": "test"}
    if status is not None:
        meta["status"] = status
    if docs is not None:
        meta["docs"] = docs
    (d / "run.json").write_text(json.dumps(meta))
    return d


def extract_disk(run_dir, manifest):
    """equityzen-style disk-driven extract: key = (deal-slug, doc-slug)."""
    docs_root = run_dir / "documents"
    if not docs_root.is_dir():
        return
    for deal_dir in docs_root.iterdir():
        if deal_dir.is_symlink() or not deal_dir.is_dir():
            continue
        for f in deal_dir.iterdir():
            if f.is_file() and not f.is_symlink():
                yield docdedup.DocRef(key=(deal_dir.name, f.stem),
                                      doc_date=None,
                                      relpath=str(f.relative_to(run_dir)))


def extract_dated(run_dir, manifest):
    """Manifest-driven extract carrying a pre-fetch doc_date (for the freshness
    window). Reads ``run.json``'s ``docs`` list of {key, relpath, doc_date}."""
    for d in (manifest or {}).get("docs", []):
        yield docdedup.DocRef(
            key=tuple(d["key"]), relpath=d["relpath"],
            doc_date=date.fromisoformat(d["doc_date"]) if d.get("doc_date") else None)


def _stub_fetch(target: Path, body: bytes, calls: list):
    """A fetch closure that records its call and writes ``body`` to ``target``."""
    def _fetch():
        calls.append(target)
        bronze.atomic_write_bytes(target, body)
        return target
    return _fetch


def _ino(p: Path) -> int:
    return p.stat().st_ino


# ============================================================
# SkipSet.derive
# ============================================================

def test_derive_indexes_complete_prior(tmp_path):
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk)
    assert (DEAL, DOC1) in skip
    got = skip.take((DEAL, DOC1))
    assert got == a / f"documents/{DEAL}/{DOC1}.pdf"
    # exhausted after one take
    assert skip.take((DEAL, DOC1)) is None


def test_derive_ignores_non_complete(tmp_path):
    _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY},
         status="in-progress")
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk)
    assert (DEAL, DOC1) not in skip


def test_derive_self_heals_deleted_file(tmp_path):
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    (a / f"documents/{DEAL}/{DOC1}.pdf").unlink()   # bronze pruned the blob
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk)
    assert skip.take((DEAL, DOC1)) is None          # dropped out → will re-fetch


def test_derive_multiset_hands_out_n_priors(tmp_path):
    # Same key present in two complete runs → two distinct priors, once each.
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    b = _run(tmp_path, OLD_B, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk)
    first = skip.take((DEAL, DOC1))
    second = skip.take((DEAL, DOC1))
    third = skip.take((DEAL, DOC1))
    # Oldest run handed out first (canonical = archival copy).
    assert first == a / f"documents/{DEAL}/{DOC1}.pdf"
    assert second == b / f"documents/{DEAL}/{DOC1}.pdf"
    assert third is None


def test_derive_freshness_window_excludes_recent(tmp_path):
    now = date(2026, 3, 1)
    _run(tmp_path, OLD_A,
         {f"documents/{DEAL}/{DOC1}.pdf": BODY,
          f"documents/{DEAL}/{DOC2}.pdf": BODY2},
         docs=[{"key": [DEAL, DOC1], "relpath": f"documents/{DEAL}/{DOC1}.pdf",
                "doc_date": "2026-02-20"},        # 9 days before now — recent
               {"key": [DEAL, DOC2], "relpath": f"documents/{DEAL}/{DOC2}.pdf",
                "doc_date": "2025-12-01"}])       # months old — stable
    skip = docdedup.SkipSet.derive(tmp_path, extract_dated,
                                   freshness_days=35, now=now)
    assert skip.take((DEAL, DOC1)) is None         # inside window → re-fetch
    assert skip.take((DEAL, DOC2)) is not None      # outside window → linkable


def test_derive_freshness_default_is_35(tmp_path):
    # freshness_days now DEFAULTS to 35 (the plan's safe default), so a doc dated
    # within 35 days of now is excluded even when the caller omits the argument.
    now = date(2026, 3, 1)
    _run(tmp_path, OLD_A,
         {f"documents/{DEAL}/{DOC1}.pdf": BODY,
          f"documents/{DEAL}/{DOC2}.pdf": BODY2},
         docs=[{"key": [DEAL, DOC1], "relpath": f"documents/{DEAL}/{DOC1}.pdf",
                "doc_date": "2026-02-20"},        # 9 days before now — recent
               {"key": [DEAL, DOC2], "relpath": f"documents/{DEAL}/{DOC2}.pdf",
                "doc_date": "2025-12-01"}])       # months old — stable
    skip = docdedup.SkipSet.derive(tmp_path, extract_dated, now=now)  # default 35
    assert skip.take((DEAL, DOC1)) is None          # inside default window
    assert skip.take((DEAL, DOC2)) is not None      # outside window
    # Explicit None disables the window (equityzen's no-pre-fetch-date case).
    skip_off = docdedup.SkipSet.derive(tmp_path, extract_dated,
                                       freshness_days=None, now=now)
    assert skip_off.take((DEAL, DOC1)) is not None   # window off → linkable


def test_derive_excludes_current_run(tmp_path):
    cur = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    assert (DEAL, DOC1) not in skip


# ============================================================
# link-mode
# ============================================================

def test_link_hit_hardlinks_prior_stub_not_called(tmp_path):
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)

    calls: list = []
    target_dir = cur / "documents" / DEAL
    status = docdedup.link_or_fetch(
        skip, (DEAL, DOC1), target_dir=target_dir, stem=DOC1,
        fetch=_stub_fetch(target_dir / f"{DOC1}.pdf", BODY, calls))

    assert status == docdedup.LINKED
    assert calls == []                              # fetch never ran
    linked = target_dir / f"{DOC1}.pdf"
    prior = a / f"documents/{DEAL}/{DOC1}.pdf"
    assert _ino(linked) == _ino(prior)              # shared inode
    assert linked.read_bytes() == BODY


def test_link_miss_calls_fetch(tmp_path):
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    calls: list = []
    target_dir = cur / "documents" / DEAL
    status = docdedup.link_or_fetch(
        skip, (DEAL, DOC1), target_dir=target_dir, stem=DOC1,
        fetch=_stub_fetch(target_dir / f"{DOC1}.pdf", BODY, calls))
    assert status == docdedup.FETCHED
    assert len(calls) == 1
    assert (target_dir / f"{DOC1}.pdf").read_bytes() == BODY


def test_link_force_always_fetches(tmp_path):
    _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    calls: list = []
    target_dir = cur / "documents" / DEAL
    status = docdedup.link_or_fetch(
        skip, (DEAL, DOC1), target_dir=target_dir, stem=DOC1,
        fetch=_stub_fetch(target_dir / f"{DOC1}.pdf", BODY, calls), force=True)
    assert status == docdedup.FETCHED and len(calls) == 1


def test_link_fallthrough_on_oslink_error(tmp_path, monkeypatch):
    _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)

    def _boom(src, dst):
        raise OSError("simulated cross-device link")
    monkeypatch.setattr(docdedup.os, "link", _boom)

    calls: list = []
    target_dir = cur / "documents" / DEAL
    status = docdedup.link_or_fetch(
        skip, (DEAL, DOC1), target_dir=target_dir, stem=DOC1,
        fetch=_stub_fetch(target_dir / f"{DOC1}.pdf", BODY, calls))
    # Degraded to a real fetch — never to a missing document.
    assert status == docdedup.FETCHED and len(calls) == 1
    assert (target_dir / f"{DOC1}.pdf").read_bytes() == BODY


def test_link_result_is_real_self_contained_file(tmp_path):
    _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    target_dir = cur / "documents" / DEAL
    docdedup.link_or_fetch(skip, (DEAL, DOC1), target_dir=target_dir, stem=DOC1,
                           fetch=_stub_fetch(target_dir / f"{DOC1}.pdf", BODY, []))
    linked = target_dir / f"{DOC1}.pdf"
    assert linked.is_file() and not linked.is_symlink()   # real file, in-run
    assert linked.read_bytes() == BODY


def test_link_fetch_failed(tmp_path):
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    status = docdedup.link_or_fetch(
        skip, (DEAL, DOC1), target_dir=cur / "documents" / DEAL, stem=DOC1,
        fetch=lambda: None)
    assert status == docdedup.FETCH_FAILED


def test_docref_carries_optional_size():
    # The size-guard field for label-keyed callers; default None (guard off).
    assert docdedup.DocRef(key=(DEAL, DOC1), doc_date=None,
                           relpath="p", size=1234).size == 1234
    assert docdedup.DocRef(key=(DEAL, DOC1), doc_date=None, relpath="p").size is None


def test_link_size_guard_mismatch_falls_through_to_fetch(tmp_path):
    # A mis-keyed cross-account collision: the prior file under this key is a
    # DIFFERENT size than the current doc expects → do not link the wrong file;
    # fall through to a real fetch (belt-and-braces for label-keyed adopters).
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    calls: list = []
    target_dir = cur / "documents" / DEAL
    status = docdedup.link_or_fetch(
        skip, (DEAL, DOC1), target_dir=target_dir, stem=DOC1,
        fetch=_stub_fetch(target_dir / f"{DOC1}.pdf", BODY2, calls),
        expected_size=len(BODY) + 99)               # deliberately wrong size
    assert status == docdedup.FETCHED               # collision → fetched
    assert len(calls) == 1
    linked = target_dir / f"{DOC1}.pdf"
    assert _ino(linked) != _ino(a / f"documents/{DEAL}/{DOC1}.pdf")
    assert linked.read_bytes() == BODY2


def test_link_size_guard_match_links(tmp_path):
    # A correct expected_size passes the guard → the prior is hardlinked as usual.
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    calls: list = []
    target_dir = cur / "documents" / DEAL
    status = docdedup.link_or_fetch(
        skip, (DEAL, DOC1), target_dir=target_dir, stem=DOC1,
        fetch=_stub_fetch(target_dir / f"{DOC1}.pdf", BODY, calls),
        expected_size=len(BODY))                    # correct size → link
    assert status == docdedup.LINKED
    assert calls == []
    assert _ino(target_dir / f"{DOC1}.pdf") == _ino(a / f"documents/{DEAL}/{DOC1}.pdf")


# ============================================================
# fetch-verify-dedup
# ============================================================

def _fetch_verify(tmp_path, prior_body, fetched_body, *, force=False):
    """Helper: one prior run with ``prior_body``; fetch writes ``fetched_body``.
    Returns (status, fetched_path, prior_path, calls)."""
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": prior_body})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    calls: list = []
    fetched = cur / "documents" / DEAL / f"{DOC1}.pdf"
    status = docdedup.fetch_verify_dedup(
        skip, (DEAL, DOC1),
        fetch=_stub_fetch(fetched, fetched_body, calls), force=force)
    return status, fetched, a / f"documents/{DEAL}/{DOC1}.pdf", calls


def test_fetch_verify_unchanged_hardlinks_prior(tmp_path):
    status, fetched, prior, calls = _fetch_verify(tmp_path, BODY, BODY)
    assert status == docdedup.VERIFIED
    assert len(calls) == 1                          # ALWAYS fetches
    assert _ino(fetched) == _ino(prior)             # disk reclaimed via hardlink
    assert fetched.read_bytes() == BODY


def test_fetch_verify_changed_keeps_fresh(tmp_path):
    # The re-issued-K-1 case: bytes differ under a stable id → keep the new copy.
    status, fetched, prior, calls = _fetch_verify(tmp_path, BODY, BODY2)
    assert status == docdedup.CHANGED
    assert len(calls) == 1
    assert _ino(fetched) != _ino(prior)             # distinct copy retained
    assert fetched.read_bytes() == BODY2            # the corrected content
    assert prior.read_bytes() == BODY               # prior untouched


def test_fetch_verify_new_doc_keeps_fetched(tmp_path):
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    calls: list = []
    fetched = cur / "documents" / DEAL / f"{DOC1}.pdf"
    status = docdedup.fetch_verify_dedup(
        skip, (DEAL, DOC1), fetch=_stub_fetch(fetched, BODY, calls))
    assert status == docdedup.FETCHED
    assert fetched.read_bytes() == BODY


def test_fetch_verify_force_keeps_fetched(tmp_path):
    status, fetched, prior, calls = _fetch_verify(tmp_path, BODY, BODY, force=True)
    assert status == docdedup.FETCHED              # force skips the dedup
    assert _ino(fetched) != _ino(prior)            # independent copy
    assert fetched.read_bytes() == BODY


def test_fetch_verify_relink_error_keeps_fetched(tmp_path, monkeypatch):
    def _boom(src, dst):
        raise OSError("simulated link failure")
    monkeypatch.setattr(docdedup.os, "link", _boom)
    status, fetched, prior, calls = _fetch_verify(tmp_path, BODY, BODY)
    # Byte-identical but the relink failed → keep the fetched bytes, never lose.
    assert status == docdedup.FETCHED
    assert fetched.is_file() and fetched.read_bytes() == BODY


def test_fetch_verify_fetch_failed(tmp_path):
    _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": BODY})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    status = docdedup.fetch_verify_dedup(skip, (DEAL, DOC1), fetch=lambda: None)
    assert status == docdedup.FETCH_FAILED


# ============================================================
# process() — document-class-aware mode dispatch
# ============================================================

def _process(tmp_path, doc_class, prior_body, fetched_body, *, force=False):
    a = _run(tmp_path, OLD_A, {f"documents/{DEAL}/{DOC1}.pdf": prior_body})
    cur = bronze.run_dir(tmp_path, OLD_B)
    cur.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, extract_disk, exclude_run=cur)
    calls: list = []
    target_dir = cur / "documents" / DEAL
    fetched = target_dir / f"{DOC1}.pdf"
    status = docdedup.process(
        skip, (DEAL, DOC1), doc_class=doc_class, target_dir=target_dir,
        stem=DOC1, fetch=_stub_fetch(fetched, fetched_body, calls), force=force)
    return status, fetched, a / f"documents/{DEAL}/{DOC1}.pdf", calls


def test_process_immutable_links(tmp_path):
    status, fetched, prior, calls = _process(
        tmp_path, docdedup.CLASS_IMMUTABLE, BODY, BODY)
    assert status == docdedup.LINKED
    assert calls == []                              # fetch avoided
    assert _ino(fetched) == _ino(prior)


def test_process_tax_unchanged_verifies(tmp_path):
    status, fetched, prior, calls = _process(
        tmp_path, docdedup.CLASS_TAX, BODY, BODY)
    assert status == docdedup.VERIFIED
    assert len(calls) == 1                          # tax docs ALWAYS fetched
    assert _ino(fetched) == _ino(prior)


def test_process_tax_changed_kept(tmp_path):
    # The load-bearing correctness case: a corrected K-1 under a stable id is
    # fetched and KEPT (never link/skip'd to a stale prior).
    status, fetched, prior, calls = _process(
        tmp_path, docdedup.CLASS_TAX, BODY, BODY2)
    assert status == docdedup.CHANGED
    assert fetched.read_bytes() == BODY2
    assert _ino(fetched) != _ino(prior)


def test_process_unknown_class_fetch_verifies(tmp_path):
    # Safe default: an unclassified doc is ALWAYS fetched (never fetch-avoided
    # by a link), but a byte-identical prior is still deduped to a hardlink —
    # so an unknown type behaves exactly like tax / mutable, not like a link.
    status, fetched, prior, calls = _process(tmp_path, None, BODY, BODY)
    assert status == docdedup.VERIFIED             # fetched, then deduped
    assert len(calls) == 1                         # the fetch DID run
    assert _ino(fetched) == _ino(prior)            # byte-identical → hardlinked


def test_process_unknown_class_changed_keeps_fresh(tmp_path):
    # An unclassified doc whose bytes differ from the prior keeps the fresh
    # bytes (never links to a stale copy) — same as the tax/mutable path.
    status, fetched, prior, calls = _process(tmp_path, None, BODY, BODY2)
    assert status == docdedup.CHANGED
    assert len(calls) == 1
    assert fetched.read_bytes() == BODY2


def test_mode_for_class_mapping():
    assert docdedup.mode_for_class(docdedup.CLASS_IMMUTABLE) == docdedup.MODE_LINK
    assert docdedup.mode_for_class(docdedup.CLASS_TAX) == docdedup.MODE_FETCH_VERIFY
    assert docdedup.mode_for_class(docdedup.CLASS_MUTABLE) == docdedup.MODE_FETCH_VERIFY
    # Unknown / None → fetch-verify (the safe default), not a bare fetch.
    assert docdedup.mode_for_class(None) == docdedup.MODE_FETCH_VERIFY
    assert docdedup.mode_for_class("nonsense") == docdedup.MODE_FETCH_VERIFY
