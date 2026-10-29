"""Unit tests for viac download.py's docdedup wiring (no httpx, no session).

Exercises the pieces the live document walk builds on — the per-(type,
subType) class mapping, the disk-driven extract hook, and the (docid,)
keying — plus the end-to-end mode dispatch through the real
``download.fetch_pdf`` (with a fake streaming client). The engine's own
per-class behaviour is tested in collectorkit (test_docdedup.py). The class
mapping asserts each document class gets the correctness-safe mode:

  * executed-once, immutable, unparsed docs — contracts, investment
    profiles, credit notes, communications, and the per-event TRANSACTION
    receipts (TRADE_REPORT, DIVIDEND, …) → LINKED from a prior run, fetch
    avoided (safe: never parsed for a silver figure);
  * PARSED / restatement-prone docs — the INVESTMENT_REPORTING period-end
    statements load.py parses, every TAX Bescheinigung, and the data-bearing
    SECURITY_FUSION PDF → ALWAYS fetched and content-compared (unchanged one
    hardlinked, a restated/corrected one KEPT — the re-issue case a
    link-everything scheme would serve stale);
  * an unrecognised type → always fetched (fail-safe);
  * a real fetch failure → an error; the audit block carries viac's
    `skipped` bucket.

viac has no equityzen-style "no-blob" outcome: every in-gate index entry is
fetchable by its documentNumber (there is no separate URL that could be
absent), and a documentNumber-less index entry is dropped by the walk before
docdedup, so it is not a docdedup outcome.

Synthetic docids / bytes only — no real document numbers or figures.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402
from collectorkit import docdedup  # noqa: E402

# Synthetic document numbers — used verbatim as the docdedup key and the
# on-disk stem, so any stable string works.
REPORT_ID = "SYN-REPORT-0001"       # INVESTMENT_REPORTING (parsed → fetch-verify)
CONTRACT_ID = "SYN-CONTRACT-0001"   # PROVISION_CONTRACT (immutable → link)
OTHER_ID = "SYN-OTHER-0001"         # unrecognised subtype (fail-safe → fetch-verify)

BODY = b"%PDF-1.4 synthetic statement body " + b"x" * 200
BODY2 = b"%PDF-1.4 corrected synthetic body " + b"y" * 200

OLD = "20260101T010000Z"
CUR = "20260201T010000Z"


def _prior_run(root: Path, docid: str, body: bytes,
               status: str = "complete") -> Path:
    """A synthetic COMPLETE prior viac run holding one document blob at the
    same flat ``documents/<docid>.pdf`` path a live run would use."""
    d = root / OLD
    blob = d / "documents" / f"{docid}.pdf"
    blob.parent.mkdir(parents=True)
    blob.write_bytes(body)
    (d / "run.json").write_text(json.dumps({"status": status}))
    return d


def _prior_blob(prior: Path, docid: str) -> Path:
    return prior / "documents" / f"{docid}.pdf"


def _skip(root: Path, run: Path):
    return docdedup.SkipSet.derive(root, download.extract_viac,
                                   freshness_days=None, exclude_run=run)


def _ino(p: Path) -> int:
    return p.stat().st_ino


# ============================================================
# (type, subType) → docdedup class mapping
# ============================================================

def test_document_class_mapping():
    # PARSED report statements → mutable → fetch-verify.
    for st in ("INVESTMENT_REPORTING", "MANUAL_INVESTMENT_REPORTING"):
        assert download._document_class(
            {"type": "REPORT", "subType": st}) == docdedup.CLASS_MUTABLE
    # TAX (Bescheinigung) → tax → fetch-verify (keyed on the type, so an
    # uncatalogued tax subtype still fetch-verifies).
    assert download._document_class(
        {"type": "TAX", "subType": "TAX_REPORT"}) == docdedup.CLASS_TAX
    assert download._document_class(
        {"type": "TAX", "subType": "SOMETHING_NEW"}) == docdedup.CLASS_TAX
    # A not-yet-catalogued REPORT subtype still fetch-verifies (type fallback).
    assert download._document_class(
        {"type": "REPORT", "subType": "SOMETHING_NEW"}) == docdedup.CLASS_MUTABLE
    # SECURITY_FUSION (data-bearing) → mutable → fetch-verify.
    assert download._document_class(
        {"type": "TRANSACTION", "subType": "SECURITY_FUSION"}) == docdedup.CLASS_MUTABLE
    # All of the above resolve to the fetch-verify mode.
    for doc in ({"type": "REPORT", "subType": "INVESTMENT_REPORTING"},
                {"type": "TAX", "subType": "TAX_REPORT"},
                {"type": "TRANSACTION", "subType": "SECURITY_FUSION"}):
        assert docdedup.mode_for_class(
            download._document_class(doc)) == docdedup.MODE_FETCH_VERIFY
    # Executed-once, immutable, unparsed docs → immutable → link.
    for ty, st in (("CONTRACT", "PROVISION_CONTRACT"),
                   ("CONTRACT", "INVESTMENT_PROFILE"),
                   ("ACCOUNT_MOVEMENT", "CONTRIBUTION_CREDIT_NOTE"),
                   ("COMMUNICATION", "GENERIC_COMMUNICATION"),
                   ("TRANSACTION", "TRADE_REPORT"),
                   ("TRANSACTION", "FEE_CHARGE"),
                   ("TRANSACTION", "INTEREST"),
                   ("TRANSACTION", "DIVIDEND"),
                   ("TRANSACTION", "DIVIDEND_CANCELLATION")):
        assert download._document_class(
            {"type": ty, "subType": st}) == docdedup.CLASS_IMMUTABLE
        assert docdedup.mode_for_class(
            download._document_class({"type": ty, "subType": st})) == docdedup.MODE_LINK
    # Unknown / absent → unclassified → fetch-verify (the safe default).
    assert download._document_class(
        {"type": "TRANSACTION", "subType": "SOMETHING_NEW"}) is None
    assert download._document_class({"type": "MYSTERY", "subType": None}) is None
    assert download._document_class({}) is None
    assert docdedup.mode_for_class(None) == docdedup.MODE_FETCH_VERIFY


# ============================================================
# extract_viac (disk-driven)
# ============================================================

def test_extract_viac_yields_docrefs(tmp_path):
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    refs = list(download.extract_viac(prior, None))
    assert len(refs) == 1
    ref = refs[0]
    assert ref.key == (REPORT_ID,)
    assert ref.doc_date is None
    assert ref.relpath == f"documents/{REPORT_ID}.pdf"


def test_extract_viac_skips_index_json_and_non_pdf(tmp_path):
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    # index.json shares the documents/ dir but is not a document blob.
    (prior / "documents" / "index.json").write_text("[]")
    # A stray non-PDF is ignored too.
    (prior / "documents" / "loose.txt").write_text("x")
    keys = {r.key for r in download.extract_viac(prior, None)}
    assert keys == {(REPORT_ID,)}


def test_extract_viac_skips_symlinked_pdf(tmp_path):
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    external = tmp_path / "external"
    external.mkdir()
    (external / "sneaky.pdf").write_bytes(BODY)
    (prior / "documents" / f"{OTHER_ID}.pdf").symlink_to(external / "sneaky.pdf")
    keys = {r.key for r in download.extract_viac(prior, None)}
    assert keys == {(REPORT_ID,)}


def test_extract_viac_no_documents_dir(tmp_path):
    d = tmp_path / OLD
    d.mkdir()
    assert list(download.extract_viac(d, None)) == []


# ============================================================
# fetch_pdf wiring (real function, fake streaming client)
# ============================================================

class _FakeStreamResp:
    def __init__(self, status: int, body: bytes):
        self.status_code = status
        self._body = body
        self.headers = {"content-type": "application/pdf"}

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self) -> bytes:
        return self._body

    def iter_bytes(self, chunk_size: int = 65536):
        yield self._body


class FakeStreamClient:
    """Minimal stand-in for ViacClient.stream — records the GET paths so a
    test can assert whether fetch_pdf actually hit the network."""

    def __init__(self, body: bytes, status: int = 200):
        self.body = body
        self.status = status
        self.gets: list[str] = []

    def stream(self, method: str, path: str) -> _FakeStreamResp:
        self.gets.append(path)
        return _FakeStreamResp(self.status, self.body)


def test_fetch_pdf_links_immutable(tmp_path):
    # fetch_pdf drives the engine end-to-end: a prior identical contract is
    # LINKED and the stream client is never touched.
    prior = _prior_run(tmp_path, CONTRACT_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    skip = _skip(tmp_path, run)
    client = FakeStreamClient(BODY)
    doc = {"documentNumber": CONTRACT_ID, "type": "CONTRACT",
           "subType": "PROVISION_CONTRACT"}
    target = run / "documents" / f"{CONTRACT_ID}.pdf"
    status = download.fetch_pdf(client, doc, CONTRACT_ID, target, skip)
    assert status == docdedup.LINKED
    assert client.gets == []                             # fetch avoided
    assert _ino(target) == _ino(_prior_blob(prior, CONTRACT_ID))


def test_fetch_pdf_will_not_link_a_prior_copy_that_is_not_a_pdf(tmp_path):
    """Drives the REAL fetch_pdf, so it proves the guard is wired and not
    merely available. link-mode never re-fetches, so a prior copy that is an
    error page or a url envelope would be hardlinked forward for ever."""
    _prior_run(tmp_path, CONTRACT_ID, b'{"url": "https://cdn.example/x.pdf"}')
    run = tmp_path / CUR
    run.mkdir()
    skip = _skip(tmp_path, run)
    client = FakeStreamClient(BODY)
    doc = {"documentNumber": CONTRACT_ID, "type": "CONTRACT",
           "subType": "PROVISION_CONTRACT"}
    target = run / "documents" / f"{CONTRACT_ID}.pdf"
    status = download.fetch_pdf(client, doc, CONTRACT_ID, target, skip)
    assert status == docdedup.FETCHED                    # not LINKED
    assert client.gets                                   # it really fetched
    assert target.read_bytes() == BODY


def test_fetch_pdf_verifies_report(tmp_path):
    # A REPORT statement is always fetched (stream client hit once); a
    # byte-identical prior is hardlinked to reclaim disk → VERIFIED.
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    skip = _skip(tmp_path, run)
    client = FakeStreamClient(BODY)
    doc = {"documentNumber": REPORT_ID, "type": "REPORT",
           "subType": "INVESTMENT_REPORTING"}
    target = run / "documents" / f"{REPORT_ID}.pdf"
    status = download.fetch_pdf(client, doc, REPORT_ID, target, skip)
    assert status == docdedup.VERIFIED
    assert client.gets == [f"/files/document/{REPORT_ID}"]
    assert _ino(target) == _ino(_prior_blob(prior, REPORT_ID))


def test_fetch_pdf_report_changed_keeps_fresh(tmp_path):
    # The server returns different bytes for the same documentNumber (a
    # restatement): fetch_pdf keeps the fresh bytes → CHANGED.
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    skip = _skip(tmp_path, run)
    client = FakeStreamClient(BODY2)
    doc = {"documentNumber": REPORT_ID, "type": "REPORT",
           "subType": "INVESTMENT_REPORTING"}
    target = run / "documents" / f"{REPORT_ID}.pdf"
    status = download.fetch_pdf(client, doc, REPORT_ID, target, skip)
    assert status == docdedup.CHANGED
    assert target.read_bytes() == BODY2
    assert _prior_blob(prior, REPORT_ID).read_bytes() == BODY


def test_fetch_pdf_non_200_raises(tmp_path):
    # A non-200 propagates out of fetch_pdf (walk() catches it and tallies a
    # document error).
    run = tmp_path / CUR
    run.mkdir()
    skip = _skip(tmp_path, run)
    client = FakeStreamClient(b"", status=404)
    doc = {"documentNumber": REPORT_ID, "type": "REPORT",
           "subType": "INVESTMENT_REPORTING"}
    target = run / "documents" / f"{REPORT_ID}.pdf"
    try:
        download.fetch_pdf(client, doc, REPORT_ID, target, skip)
    except RuntimeError:
        pass
    else:
        raise AssertionError("expected a RuntimeError on HTTP 404")


# ============================================================
# The audit block
# ============================================================

def test_doc_counts_carry_a_skipped_bucket():
    # viac's audit block adds `skipped` (a document the date or type gate
    # leaves out, or any document under --dry-run) to the docdedup buckets.
    assert download._empty_doc_counts()["skipped"] == 0


def test_fetch_failed_is_error(tmp_path):
    # A fetch closure that returns None (no file) → FETCH_FAILED, tallied as
    # an error (reserved for real failures).
    run = tmp_path / CUR
    run.mkdir()
    skip = _skip(tmp_path, run)
    status = docdedup.process(
        skip, key=(REPORT_ID,),
        doc_class=download._document_class(
            {"type": "REPORT", "subType": "INVESTMENT_REPORTING"}),
        target_dir=run / "documents", stem=REPORT_ID,
        fetch=lambda: None)
    assert status == docdedup.FETCH_FAILED
    counts = download._empty_doc_counts()
    docdedup.tally(counts, status)
    assert counts["errors"] == 1 and counts["fetched"] == 0
