"""Unit tests for viac download.py's docdedup wiring (no httpx, no session).

Exercises the pieces the live document walk builds on — the per-(type,
subType) class mapping, the disk-driven extract hook, and the (docid,)
keying — plus the end-to-end mode dispatch, both through
``docdedup.process`` directly (with an injected stub fetch) and through the
real ``download.fetch_pdf`` (with a fake streaming client). Asserts each
document class behaves per the correctness-safe mapping that replaced viac's
old link-everything ``find_existing_pdf`` / ``os.link``:

  * executed-once, immutable, unparsed docs — contracts, investment
    profiles, credit notes, communications, and the per-event TRANSACTION
    receipts (TRADE_REPORT, DIVIDEND, …) → LINKED from a prior run, fetch
    avoided (safe: never parsed for a silver figure);
  * PARSED / restatement-prone docs — the INVESTMENT_REPORTING period-end
    statements load.py parses, every TAX Bescheinigung, and the data-bearing
    SECURITY_FUSION PDF → ALWAYS fetched and content-compared (unchanged one
    hardlinked, a restated/corrected one KEPT — the re-issue case the old
    link-everything code would have served stale);
  * an unrecognised type → always fetched (fail-safe);
  * a real fetch failure → an error; _tally routes an unmapped outcome to
    'other', never inflating 'fetched'; --documents-force bypasses the index.

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
from collectorkit import bronze, docdedup  # noqa: E402

# Synthetic document numbers — used verbatim as the docdedup key and the
# on-disk stem, so any stable string works.
REPORT_ID = "SYN-REPORT-0001"       # INVESTMENT_REPORTING (parsed → fetch-verify)
TAX_ID = "SYN-TAX-0001"             # TAX_REPORT / Bescheinigung (fetch-verify)
FUSION_ID = "SYN-FUSION-0001"       # SECURITY_FUSION (data-bearing → fetch-verify)
CONTRACT_ID = "SYN-CONTRACT-0001"   # PROVISION_CONTRACT (immutable → link)
TRADE_ID = "SYN-TRADE-0001"         # per-event TRADE_REPORT (immutable → link)
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


def _dispatch(run: Path, skip, docid: str, doc_type: str | None,
              subtype: str | None, body: bytes, calls: list,
              *, force: bool = False) -> str:
    """Replay download.py's per-document dispatch for one doc through the
    engine, with a stub fetch that records whether it actually ran."""
    doc = {"documentNumber": docid, "type": doc_type, "subType": subtype}
    target_dir = run / "documents"

    def _fetch():
        calls.append(docid)
        path = target_dir / f"{docid}.pdf"
        bronze.atomic_write_bytes(path, body)
        return path

    return docdedup.process(
        skip, key=(docid,), doc_class=download._document_class(doc),
        target_dir=target_dir, stem=docid, fetch=_fetch, force=force)


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
# End-to-end dispatch by class (engine-level)
# ============================================================

def test_contract_is_linked(tmp_path):
    # An executed-once immutable contract identical to a prior run is
    # hardlinked in — the fetch-avoidance win, safe because it is not parsed.
    prior = _prior_run(tmp_path, CONTRACT_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), CONTRACT_ID,
                       "CONTRACT", "PROVISION_CONTRACT", BODY, calls)
    assert status == docdedup.LINKED
    assert calls == []                                   # fetch avoided
    linked = run / "documents" / f"{CONTRACT_ID}.pdf"
    assert _ino(linked) == _ino(_prior_blob(prior, CONTRACT_ID))
    assert linked.read_bytes() == BODY


def test_transaction_receipt_is_linked(tmp_path):
    # The bulk-volume case: a per-event TRADE_REPORT receipt (immutable once
    # settled, never parsed) is hardlinked from a prior identical copy.
    prior = _prior_run(tmp_path, TRADE_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), TRADE_ID,
                       "TRANSACTION", "TRADE_REPORT", BODY, calls)
    assert status == docdedup.LINKED
    assert calls == []
    assert _ino(run / "documents" / f"{TRADE_ID}.pdf") == \
        _ino(_prior_blob(prior, TRADE_ID))


def test_report_unchanged_is_verified(tmp_path):
    # A REPORT statement is fetch-verify (parsed → restatement-prone), NOT
    # link: it is ALWAYS fetched, and a byte-identical prior is hardlinked
    # only to reclaim disk (never a fetch-skipping stale link).
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), REPORT_ID,
                       "REPORT", "INVESTMENT_REPORTING", BODY, calls)
    assert status == docdedup.VERIFIED
    assert calls == [REPORT_ID]                          # statement ALWAYS fetched
    assert _ino(run / "documents" / f"{REPORT_ID}.pdf") == \
        _ino(_prior_blob(prior, REPORT_ID))


def test_report_restated_is_kept(tmp_path):
    # The correctness case viac's old link-everything code got wrong: a
    # REPORT statement whose bytes change between runs (a restatement under a
    # stable documentNumber) is fetched and its NEW bytes kept — never linked
    # to a stale prior that would feed a superseded holding into the replay.
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), REPORT_ID,
                       "REPORT", "INVESTMENT_REPORTING", BODY2, calls)
    assert status == docdedup.CHANGED
    fetched = run / "documents" / f"{REPORT_ID}.pdf"
    assert fetched.read_bytes() == BODY2                 # restated content kept
    assert _prior_blob(prior, REPORT_ID).read_bytes() == BODY


def test_tax_unchanged_is_verified(tmp_path):
    prior = _prior_run(tmp_path, TAX_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), TAX_ID,
                       "TAX", "TAX_REPORT", BODY, calls)
    assert status == docdedup.VERIFIED
    assert calls == [TAX_ID]                             # tax docs ALWAYS fetched
    assert _ino(run / "documents" / f"{TAX_ID}.pdf") == \
        _ino(_prior_blob(prior, TAX_ID))


def test_tax_corrected_is_kept(tmp_path):
    # A Bescheinigung re-issued under a stable documentNumber is fetched and
    # its NEW bytes kept — never linked to stale.
    prior = _prior_run(tmp_path, TAX_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), TAX_ID,
                       "TAX", "TAX_REPORT", BODY2, calls)
    assert status == docdedup.CHANGED
    fetched = run / "documents" / f"{TAX_ID}.pdf"
    assert fetched.read_bytes() == BODY2
    assert _prior_blob(prior, TAX_ID).read_bytes() == BODY


def test_security_fusion_is_fetch_verified(tmp_path):
    # SECURITY_FUSION carries the ONLY copy of the old→new ISIN map and a
    # parser pass is planned, so it is fetch-verified, never linked.
    prior = _prior_run(tmp_path, FUSION_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), FUSION_ID,
                       "TRANSACTION", "SECURITY_FUSION", BODY, calls)
    assert status == docdedup.VERIFIED
    assert calls == [FUSION_ID]                          # the fetch DID run
    assert _ino(run / "documents" / f"{FUSION_ID}.pdf") == \
        _ino(_prior_blob(prior, FUSION_ID))


def test_security_fusion_changed_keeps_fresh(tmp_path):
    prior = _prior_run(tmp_path, FUSION_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), FUSION_ID,
                       "TRANSACTION", "SECURITY_FUSION", BODY2, calls)
    assert status == docdedup.CHANGED
    assert (run / "documents" / f"{FUSION_ID}.pdf").read_bytes() == BODY2


def test_unknown_type_is_fetch_verified(tmp_path):
    # An unclassified type is fetch-verified (the safe default): ALWAYS
    # fetched, but a byte-identical prior is still deduped to a hardlink.
    prior = _prior_run(tmp_path, OTHER_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), OTHER_ID,
                       "TRANSACTION", "SOMETHING_NEW", BODY, calls)
    assert status == docdedup.VERIFIED                   # fetched, then deduped
    assert calls == [OTHER_ID]                           # the fetch DID run
    assert _ino(run / "documents" / f"{OTHER_ID}.pdf") == \
        _ino(_prior_blob(prior, OTHER_ID))


def test_documents_force_bypasses_index(tmp_path):
    prior = _prior_run(tmp_path, REPORT_ID, BODY)
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), REPORT_ID,
                       "REPORT", "INVESTMENT_REPORTING", BODY, calls, force=True)
    assert status == docdedup.FETCHED                    # forced fetch, no hardlink
    assert calls == [REPORT_ID]
    assert _ino(run / "documents" / f"{REPORT_ID}.pdf") != \
        _ino(_prior_blob(prior, REPORT_ID))


def test_new_document_is_fetched(tmp_path):
    # No prior copy → a genuine first fetch (kept), whatever the class.
    run = tmp_path / CUR
    run.mkdir()
    calls: list = []
    status = _dispatch(run, _skip(tmp_path, run), REPORT_ID,
                       "REPORT", "INVESTMENT_REPORTING", BODY, calls)
    assert status == docdedup.FETCHED
    assert calls == [REPORT_ID]
    assert (run / "documents" / f"{REPORT_ID}.pdf").read_bytes() == BODY


# ============================================================
# fetch_pdf wiring (real function, fake streaming client)
# ============================================================

class _FakeStreamResp:
    def __init__(self, status: int, body: bytes):
        self.status_code = status
        self._body = body

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
    # document error) — the same hard-failure semantics as before docdedup.
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
# _tally / counters
# ============================================================

def test_tally_and_counts():
    counts = download._empty_doc_counts()
    for s in (docdedup.LINKED, docdedup.LINKED, docdedup.FETCHED,
              docdedup.VERIFIED, docdedup.CHANGED, docdedup.FETCH_FAILED):
        download._tally(counts, s)
    assert counts == {"total": 0, "fetched": 1, "linked": 2, "verified": 1,
                      "changed": 1, "skipped": 0, "errors": 1, "other": 0}


def test_tally_unmapped_code_goes_to_other():
    # An outcome code the map does not know must NOT inflate 'fetched'; it
    # lands in its own 'other' bucket (audit stays honest).
    counts = download._empty_doc_counts()
    download._tally(counts, "totally-unexpected-code")
    assert counts["other"] == 1
    assert counts["fetched"] == 0


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
    download._tally(counts, status)
    assert counts["errors"] == 1 and counts["fetched"] == 0
