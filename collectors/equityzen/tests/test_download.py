"""Unit tests for equityzen download.py's docdedup wiring (no browser).

Exercises the pieces the live document walk builds on — the per-document class
mapping, the disk-driven extract hook, and the (deal-slug, doc-slug) keying —
plus the end-to-end mode dispatch with an injected stub fetch (no network, no
Camoufox). Asserts each document class behaves per the corrected Move 1 mapping:

  * executed-once legal / offering docs (SUB_AGT, TERMSHEET, …) → LINKED from a
    prior run, fetch avoided (safe: not parsed by load.py);
  * parsed / restatement-prone docs — capital-account statements, K-1s, reports
    — → ALWAYS fetched and content-compared (unchanged one hardlinked, a
    restated/corrected one KEPT — the re-issue case that link-mode would miss);
  * an unrecognised type → always fetched (fail-safe);
  * a document node with no downloadUrl → NO_BLOB (null-blob-by-design, not an
    error); a real fetch failure → an error;
  * _tally routes an unmapped outcome to 'other', never inflating 'fetched';
  * --documents-force bypasses the index.

Synthetic slugs / ids / bytes only — no real deal ids, company names, document
ids, or figures.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
from collectorkit import bronze, docdedup  # noqa: E402

# Synthetic identifiers — hashed by download._slug, so any stable string works.
DID = "deal-node-synthetic-0001"
STMT_ID = "document-capital-account-0001"
K1_ID = "document-k1-0001"
LEGAL_ID = "document-sub-agt-0001"          # an executed-once legal doc (link)
OTHER_ID = "document-side-letter-0001"

DEAL_SLUG = download._slug(DID)
STMT_SLUG = download._slug(STMT_ID)
K1_SLUG = download._slug(K1_ID)
LEGAL_SLUG = download._slug(LEGAL_ID)
OTHER_SLUG = download._slug(OTHER_ID)

BODY = b"%PDF-1.4 synthetic statement body " + b"x" * 200
BODY2 = b"%PDF-1.4 corrected synthetic body " + b"y" * 200

OLD_A = "20260101T010000Z"
CUR = "20260201T010000Z"


def _prior_run(root: Path, doc_slug: str, body: bytes,
               status: str = "complete") -> Path:
    """A synthetic COMPLETE prior equityzen run holding one document blob."""
    d = root / OLD_A
    blob = d / "documents" / DEAL_SLUG / f"{doc_slug}.pdf"
    blob.parent.mkdir(parents=True)
    blob.write_bytes(body)
    import json
    (d / "run.json").write_text(json.dumps({"status": status}))
    return d


def _stub_fetch(target_dir: Path, doc_slug: str, body: bytes, calls: list):
    """Mimics download.fetch_blob: writes the blob under its content-typed name
    (.pdf here) and returns the path; records the call so we can assert whether
    the fetch actually ran."""
    def _fetch():
        calls.append(doc_slug)
        path = target_dir / f"{doc_slug}.pdf"
        bronze.atomic_write_bytes(path, body)
        return path
    return _fetch


def _dispatch(run: Path, skip, doc_id: str, doc_class_type: str | None,
              body: bytes, calls: list, *, force: bool = False) -> str:
    """Replays download.py's per-document dispatch for one doc."""
    doc = {"id": doc_id, "documentType": doc_class_type}
    doc_slug = download._slug(doc_id)
    target_dir = run / "documents" / DEAL_SLUG
    return docdedup.process(
        skip, key=(DEAL_SLUG, doc_slug), doc_class=download._document_class(doc),
        target_dir=target_dir, stem=doc_slug,
        fetch=_stub_fetch(target_dir, doc_slug, body, calls), force=force)


def _ino(p: Path) -> int:
    return p.stat().st_ino


# ============================================================
# Document-class mapping
# ============================================================

def test_document_class_mapping():
    # Parsed / restatement-prone → fetch-verify (K-1 is tax; statements + reports
    # are mutable — both resolve to MODE_FETCH_VERIFY).
    assert download._document_class({"documentType": "K1"}) == docdedup.CLASS_TAX
    for t in ("CAPITAL_ACCOUNT_STATEMENT", "QUARTERLY_REPORT",
              "ANNUAL_FINANCIAL_STATEMENTS"):
        assert download._document_class({"documentType": t}) == docdedup.CLASS_MUTABLE
    for t in ("K1", "CAPITAL_ACCOUNT_STATEMENT", "QUARTERLY_REPORT",
              "ANNUAL_FINANCIAL_STATEMENTS"):
        assert docdedup.mode_for_class(
            download._document_class({"documentType": t})) == docdedup.MODE_FETCH_VERIFY
    # Executed-once legal / offering docs → link (immutable, not parsed).
    for t in ("SUB_AGT", "COUNTERSIGN_SUB_AGT", "SERIES_SCHEDULE", "SUITABILITY",
              "SUMMARY_SHEET", "TERMSHEET", "OFFERING_DOC", "FUND_W_9", "W_8"):
        assert download._document_class({"documentType": t}) == docdedup.CLASS_IMMUTABLE
        assert docdedup.mode_for_class(
            download._document_class({"documentType": t})) == docdedup.MODE_LINK
    # Unknown / absent → unclassified → fetch-verify (the safe default).
    assert download._document_class({"documentType": "SUBSCRIPTION"}) is None
    assert download._document_class({"documentType": "SOMETHING_NEW"}) is None
    assert download._document_class({}) is None
    assert docdedup.mode_for_class(None) == docdedup.MODE_FETCH_VERIFY


# ============================================================
# extract_equityzen (disk-driven)
# ============================================================

def test_extract_equityzen_yields_docrefs(tmp_path):
    prior = _prior_run(tmp_path, STMT_SLUG, BODY)
    refs = list(download.extract_equityzen(prior, None))
    assert len(refs) == 1
    ref = refs[0]
    assert ref.key == (DEAL_SLUG, STMT_SLUG)
    assert ref.doc_date is None
    assert ref.relpath == f"documents/{DEAL_SLUG}/{STMT_SLUG}.pdf"


def test_extract_equityzen_skips_symlinks_and_stray_files(tmp_path):
    prior = _prior_run(tmp_path, STMT_SLUG, BODY)
    # A stray file directly under documents/ (not a deal dir) is ignored.
    (prior / "documents" / "loose.txt").write_text("x")
    # A symlinked deal dir is never descended into.
    external = tmp_path / "external"
    (external / "sub").mkdir(parents=True)
    (external / "sub" / f"{OTHER_SLUG}.pdf").write_bytes(BODY)
    (prior / "documents" / "linked-deal").symlink_to(
        external / "sub", target_is_directory=True)
    keys = {r.key for r in download.extract_equityzen(prior, None)}
    assert keys == {(DEAL_SLUG, STMT_SLUG)}


def test_extract_equityzen_no_documents_dir(tmp_path):
    d = tmp_path / OLD_A
    d.mkdir()
    assert list(download.extract_equityzen(d, None)) == []


# ============================================================
# End-to-end dispatch by class
# ============================================================

def test_executed_legal_doc_is_linked(tmp_path):
    # An executed-once legal / offering doc (SUB_AGT) identical to a prior run is
    # hardlinked in — the fetch-avoidance win, safe because it is not parsed.
    prior = _prior_run(tmp_path, LEGAL_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, LEGAL_ID, "SUB_AGT", BODY, calls)
    assert status == docdedup.LINKED
    assert calls == []                              # fetch avoided
    linked = run / "documents" / DEAL_SLUG / f"{LEGAL_SLUG}.pdf"
    assert _ino(linked) == _ino(prior / "documents" / DEAL_SLUG / f"{LEGAL_SLUG}.pdf")
    assert linked.read_bytes() == BODY


def test_statement_unchanged_is_verified(tmp_path):
    # A capital-account statement is now fetch-verify (parsed → restatement-
    # prone), NOT link: it is ALWAYS fetched, and a byte-identical prior is
    # hardlinked only to reclaim disk (never a fetch-skipping stale link).
    prior = _prior_run(tmp_path, STMT_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, STMT_ID, "CAPITAL_ACCOUNT_STATEMENT", BODY, calls)
    assert status == docdedup.VERIFIED
    assert calls == [STMT_SLUG]                      # statement ALWAYS fetched
    fetched = run / "documents" / DEAL_SLUG / f"{STMT_SLUG}.pdf"
    assert _ino(fetched) == _ino(prior / "documents" / DEAL_SLUG / f"{STMT_SLUG}.pdf")


def test_statement_restated_is_kept(tmp_path):
    # The correctness case the OLD link-mode mapping got wrong: a capital-account
    # statement whose bytes change between runs (a restatement under a stable
    # Relay doc.id) is fetched and its NEW bytes kept — never linked to a stale
    # prior that would feed a superseded NAV into the positions replay.
    prior = _prior_run(tmp_path, STMT_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, STMT_ID, "CAPITAL_ACCOUNT_STATEMENT", BODY2, calls)
    assert status == docdedup.CHANGED
    fetched = run / "documents" / DEAL_SLUG / f"{STMT_SLUG}.pdf"
    assert fetched.read_bytes() == BODY2            # restated content kept
    assert (prior / "documents" / DEAL_SLUG / f"{STMT_SLUG}.pdf").read_bytes() == BODY


def test_tax_k1_unchanged_is_verified(tmp_path):
    prior = _prior_run(tmp_path, K1_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, K1_ID, "K1", BODY, calls)
    assert status == docdedup.VERIFIED
    assert calls == [K1_SLUG]                        # tax docs ALWAYS fetched
    fetched = run / "documents" / DEAL_SLUG / f"{K1_SLUG}.pdf"
    assert _ino(fetched) == _ino(prior / "documents" / DEAL_SLUG / f"{K1_SLUG}.pdf")


def test_tax_k1_corrected_is_kept(tmp_path):
    # The load-bearing correctness case: a K-1 re-issued under a stable id
    # (same doc.id) is fetched and its NEW bytes kept — never link'd to stale.
    prior = _prior_run(tmp_path, K1_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, K1_ID, "K1", BODY2, calls)
    assert status == docdedup.CHANGED
    fetched = run / "documents" / DEAL_SLUG / f"{K1_SLUG}.pdf"
    assert fetched.read_bytes() == BODY2            # corrected content kept
    assert (prior / "documents" / DEAL_SLUG / f"{K1_SLUG}.pdf").read_bytes() == BODY


def test_unknown_type_is_fetch_verified(tmp_path):
    # An unclassified type is fetch-verified (the safe default): ALWAYS fetched
    # (never fetch-avoided by a link), but a byte-identical prior is still
    # deduped to a hardlink — same behaviour as a statement / K-1.
    prior = _prior_run(tmp_path, OTHER_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, OTHER_ID, "SUBSCRIPTION", BODY, calls)
    assert status == docdedup.VERIFIED               # fetched, then deduped
    assert calls == [OTHER_SLUG]                      # the fetch DID run
    fetched = run / "documents" / DEAL_SLUG / f"{OTHER_SLUG}.pdf"
    assert _ino(fetched) == _ino(prior / "documents" / DEAL_SLUG / f"{OTHER_SLUG}.pdf")


def test_unknown_type_changed_keeps_fresh(tmp_path):
    # An unclassified type whose bytes differ from the prior keeps the fresh
    # bytes — never links to a stale copy.
    prior = _prior_run(tmp_path, OTHER_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, OTHER_ID, "SUBSCRIPTION", BODY2, calls)
    assert status == docdedup.CHANGED
    fetched = run / "documents" / DEAL_SLUG / f"{OTHER_SLUG}.pdf"
    assert fetched.read_bytes() == BODY2


def test_documents_force_bypasses_index(tmp_path):
    prior = _prior_run(tmp_path, STMT_SLUG, BODY)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, STMT_ID, "CAPITAL_ACCOUNT_STATEMENT", BODY,
                       calls, force=True)
    assert status == docdedup.FETCHED               # forced fetch, no hardlink
    assert calls == [STMT_SLUG]
    fetched = run / "documents" / DEAL_SLUG / f"{STMT_SLUG}.pdf"
    assert _ino(fetched) != _ino(prior / "documents" / DEAL_SLUG / f"{STMT_SLUG}.pdf")


def test_tally_and_counts():
    counts = download._empty_doc_counts()
    for s in (docdedup.LINKED, docdedup.LINKED, docdedup.FETCHED,
              docdedup.VERIFIED, docdedup.CHANGED, docdedup.FETCH_FAILED,
              download.NO_BLOB):
        download._tally(counts, s)
    assert counts == {"total": 0, "fetched": 1, "linked": 2, "verified": 1,
                      "changed": 1, "errors": 1, "no_blob": 1, "other": 0}


def test_tally_unmapped_code_goes_to_other():
    # An outcome code the map does not know must NOT inflate 'fetched'; it lands
    # in its own 'other' bucket (audit stays honest).
    counts = download._empty_doc_counts()
    download._tally(counts, "totally-unexpected-code")
    assert counts["other"] == 1
    assert counts["fetched"] == 0


def test_no_url_document_is_no_blob_not_error(tmp_path):
    # A document node with no downloadUrl yields NO_BLOB (null-blob-by-design)
    # without ever attempting a fetch, and is NOT counted as an error.
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)
    attempts: list = []

    def _fetch_blob(url, dest):                      # must never be called
        attempts.append(url)
        return None

    doc = {"id": STMT_ID, "documentType": "CAPITAL_ACCOUNT_STATEMENT"}  # no URL
    status = download._process_document(
        skip, deal_slug=DEAL_SLUG, doc=doc,
        target_dir=run / "documents" / DEAL_SLUG, fetch_blob=_fetch_blob)
    assert status == download.NO_BLOB
    assert attempts == []                            # fetch path never entered
    counts = download._empty_doc_counts()
    download._tally(counts, status)
    assert counts["no_blob"] == 1 and counts["errors"] == 0


def test_real_fetch_failure_is_error(tmp_path):
    # A URL IS present but the GET fails (fetch_blob returns None) → a genuine
    # FETCH_FAILED, tallied as an error (reserved for real failures).
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_equityzen,
                                   exclude_run=run)

    def _failing_fetch(url, dest):
        return None                                  # URL present, GET failed

    doc = {"id": STMT_ID, "documentType": "CAPITAL_ACCOUNT_STATEMENT",
           "downloadUrl": "/documents/synthetic.pdf"}
    status = download._process_document(
        skip, deal_slug=DEAL_SLUG, doc=doc,
        target_dir=run / "documents" / DEAL_SLUG, fetch_blob=_failing_fetch)
    assert status == docdedup.FETCH_FAILED
    counts = download._empty_doc_counts()
    download._tally(counts, status)
    assert counts["errors"] == 1 and counts["no_blob"] == 0


def test_lookback_flag():
    # Accepted for wealthdb-refresh uniformity: equityzen always captures every
    # offering/position/cash-flow (no date surface), so --lookback only drives
    # a warning; the value is still validated against the shared
    # presets so a typo fails loudly.
    assert download.parse_args(["--lookback", "1y"]).lookback == "1y"
    assert download.parse_args([]).lookback is None
    with pytest.raises(SystemExit):
        download.parse_args(["--lookback", "1m"])  # not a preset


def test_debug_flag():
    # Gates the DOM + screenshot captures under <run>/screenshots/. Off by
    # default: a normal run writes none, and `prune` reclaims them from the
    # dumps that do.
    assert download.parse_args(["--debug"]).debug is True
    assert download.parse_args([]).debug is False


def test_debug_help_promises_bronze_captures(capsys):
    # Guards against the flag regressing to a warn-only stub.
    with pytest.raises(SystemExit):
        download.parse_args(["--help"])
    out = capsys.readouterr().out
    assert "--debug" in out
    assert "gates nothing" not in out
    assert "NOT YET IMPLEMENTED" not in out
