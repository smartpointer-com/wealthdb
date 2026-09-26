"""Tests for relevate's download.py: the dry-run contract + the docdedup
download-avoidance wiring.

Part 1 — dry-run contract. Root CLAUDE.md §2: `download --dry-run` walks the
read-only export surfaces (verify the session, enumerate the overview +
document index) but must persist NOTHING under the bronze root
(`--bronze-dir`). A dry-run that left a run dir — even a run.json-only shell —
could be picked up by `load` and land as a dry-run snapshot in silver. These
tests mock the session (no real Relevate calls, no state file) and assert the
invariant.

Part 2 — the document download-avoidance walk (`collectorkit.docdedup`), with
no network. Exercises the per-document class mapping, the disk-driven extract
hook, the (doc_id,) keying, and the end-to-end mode dispatch with an injected
stub fetch. Asserts each document kind behaves per the mode mapping:

  * executed-once immutable kinds not parsed by load (fee statements, pension
    agreements/plans, investor profiles, account-opening docs) → LINKED from a
    prior run, fetch avoided;
  * parsed / tax-adjacent kinds (quarterly reports, credit notes, leaving
    statements) → ALWAYS fetched and content-compared (unchanged one
    hardlinked, a re-issue KEPT — the case link-mode would miss);
  * an unrecognised kind → always fetched (fail-safe);
  * a fetch that yields no PDF → an error; --documents-force bypasses the index;
  * a link-kind doc issued inside the freshness window is re-fetched, not
    linked;
  * _tally routes an unmapped outcome to 'other', never inflating 'fetched'.

Part 3 — the `--debug` HTTP trace. Asserts the flag defaults off, that the
request choke point records successes, unexpected statuses and transport
failures alike, that a credential in a URL never reaches the file, and that
a dry-run (which walks into a throwaway dir) captures nothing.

Synthetic ids / fileNames / bytes only — no real document ids, names, or
figures.
"""
from __future__ import annotations

import json
import sys
from datetime import date, timedelta
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402
from collectorkit import bronze, debugcap, docdedup  # noqa: E402


class _FakeResp:
    """Minimal stand-in for a requests.Response, enough for
    get_and_save_json's 200 path (status_code / content / json())."""

    def __init__(self, status_code: int, body: dict) -> None:
        self.status_code = status_code
        self._body = body
        self.content = json.dumps(body).encode("utf-8")
        self.headers: dict[str, str] = {"content-type": "application/json"}

    def json(self) -> dict:
        return self._body


class _FakeSession:
    """Routes the two master-listing GETs the dry-run walk makes, and
    records every URL so the test can prove the read-only walk ran."""

    def __init__(self) -> None:
        self.urls: list[str] = []

    def get(self, url, timeout=None, allow_redirects=None):  # noqa: ANN001
        self.urls.append(url)
        if url.endswith(download.EP_INVESTMENT_OVERVIEW):
            return _FakeResp(200, {"portfolios": [
                {"externalId": "ACC1", "id": 100}]})
        if url.endswith(download.EP_DOCUMENTS_INDEX):
            return _FakeResp(200, {"documents": [
                {"id": 1, "createDate": "2026-01-01T00:00:00"}]})
        raise AssertionError(f"unexpected dry-run GET: {url}")


def _patch_session(monkeypatch) -> _FakeSession:
    sess = _FakeSession()
    monkeypatch.setattr(
        download, "new_session_from_state", lambda state_path: (sess, None))
    monkeypatch.setattr(download, "probe_session_alive", lambda session: True)
    return sess


def test_dry_run_persists_nothing_to_bronze(tmp_path, monkeypatch):
    sess = _patch_session(monkeypatch)
    bronze = tmp_path / "bronze"
    bronze.mkdir()

    rc = download.main([
        "--dry-run",
        "--bronze-dir", str(bronze),
        "--state-path", str(tmp_path / "state.json"),
    ])

    # The walk succeeded (session verified, both listings enumerated)...
    assert rc == 0
    assert any(u.endswith(download.EP_INVESTMENT_OVERVIEW) for u in sess.urls)
    assert any(u.endswith(download.EP_DOCUMENTS_INDEX) for u in sess.urls)

    # ...but NOTHING was written under the bronze root: no run dir, no
    # run.json shell, no endpoint dumps. This is the invariant.
    assert list(bronze.rglob("*")) == []


def test_dry_run_does_not_create_bronze_dir(tmp_path, monkeypatch):
    # Even the bronze root itself must not be materialised by a dry-run:
    # a real run's run_dir.mkdir(parents=True) would create it, a dry-run
    # must not touch --bronze-dir at all.
    _patch_session(monkeypatch)
    bronze = tmp_path / "bronze_absent"

    rc = download.main([
        "--dry-run",
        "--bronze-dir", str(bronze),
        "--state-path", str(tmp_path / "state.json"),
    ])

    assert rc == 0
    assert not bronze.exists()


# ============================================================
# Part 2 — docdedup download-avoidance wiring (no network)
# ============================================================

# Synthetic ids / fileNames / bytes only.
DOC_ID_FEE = 1001        # link kind      (quarterly_fee — unparsed, immutable)
DOC_ID_REPORT = 2002     # fetch-verify   (quarterly_report — parsed)
DOC_ID_CREDIT = 3003     # fetch-verify   (credit_note — parsed)
DOC_ID_OTHER = 4004      # unknown kind   → fetch-verify (fail-safe)

FEE_NAME = "Gebührenabrechnung"        # → quarterly_fee
REPORT_NAME = "Quartalsbericht"        # → quarterly_report
CREDIT_NAME = "Gutschriftsanzeige"     # → credit_note
OTHER_NAME = "Some Unlabelled Notice"  # → other

BODY = b"%PDF-1.4 synthetic body " + b"x" * 200
BODY2 = b"%PDF-1.4 corrected synthetic body " + b"y" * 200

OLD_TS = "20200101T010000Z"
CUR = "20260201T010000Z"
# A createDate well outside the default 35-day freshness window, so a prior doc
# is indexed (and thus link-eligible) regardless of when the suite runs.
OLD_CREATE = "2020-01-01T00:00:00"


def _prior_run(root: Path, doc_id: int, body: bytes, file_name: str, *,
               create_date: str = OLD_CREATE, status: str = "complete") -> Path:
    """A synthetic COMPLETE prior relevate run holding one document PDF plus the
    documents/index.json entry the extract hook reads its createDate from."""
    d = root / OLD_TS
    docs = d / "documents"
    docs.mkdir(parents=True)
    (docs / f"{doc_id}.pdf").write_bytes(body)
    (docs / "index.json").write_text(json.dumps(
        {"documents": [{"id": doc_id, "fileName": file_name,
                        "createDate": create_date}]}))
    (d / "run.json").write_text(json.dumps({"status": status}))
    return d


def _dispatch(run: Path, skip, doc_id: int, file_name: str, body: bytes,
              calls: list, *, force: bool = False) -> str:
    """Replays download.py's per-document dispatch for one doc: the class is
    chosen from the entry's fileName, and the stub fetch writes <id>.pdf and
    records the call so a test can assert whether the fetch actually ran."""
    entry = {"id": doc_id, "fileName": file_name}
    docs_dir = run / "documents"

    def _fetch():
        calls.append(doc_id)
        path = docs_dir / f"{doc_id}.pdf"
        bronze.atomic_write_bytes(path, body)
        return path

    return docdedup.process(
        skip, key=(doc_id,), doc_class=download._document_class(entry),
        target_dir=docs_dir, stem=str(doc_id), fetch=_fetch, force=force,
        usable=docdedup.is_pdf)


def _ino(p: Path) -> int:
    return p.stat().st_ino


# ------------------------------------------------------------
# Document-class mapping
# ------------------------------------------------------------

def test_document_class_mapping():
    # Parsed / tax-adjacent kinds → fetch-verify (reports + credit notes are
    # mutable; a leaving statement is tax-adjacent — all resolve to fetch-verify).
    assert download._document_class(
        {"fileName": REPORT_NAME}) == docdedup.CLASS_MUTABLE
    assert download._document_class(
        {"fileName": CREDIT_NAME}) == docdedup.CLASS_MUTABLE
    assert download._document_class(
        {"fileName": "Leaving statement"}) == docdedup.CLASS_TAX
    for name in (REPORT_NAME, CREDIT_NAME, "Leaving statement"):
        assert docdedup.mode_for_class(
            download._document_class({"fileName": name})) \
            == docdedup.MODE_FETCH_VERIFY
    # Executed-once immutable, unparsed kinds → link.
    for name in ("Gebührenabrechnung", "Fee statement", "Vorsorgevereinbarung",
                 "Pension Agreement", "Pension Plan", "Anlegerprofil",
                 "Investor profile", "Eröffnung / Eintritt"):
        assert download._document_class(
            {"fileName": name}) == docdedup.CLASS_IMMUTABLE
        assert docdedup.mode_for_class(
            download._document_class({"fileName": name})) == docdedup.MODE_LINK
    # Unknown / absent fileName → unclassified → fetch-verify (the safe default).
    assert download._document_class({"fileName": "Random unlabelled file"}) is None
    assert download._document_class({"fileName": None}) is None
    assert download._document_class({}) is None
    assert docdedup.mode_for_class(None) == docdedup.MODE_FETCH_VERIFY


# ------------------------------------------------------------
# extract_relevate (disk-driven)
# ------------------------------------------------------------

def test_extract_relevate_yields_docrefs(tmp_path):
    prior = _prior_run(tmp_path, DOC_ID_REPORT, BODY, REPORT_NAME)
    refs = list(download.extract_relevate(prior, None))
    assert len(refs) == 1
    ref = refs[0]
    assert ref.key == (DOC_ID_REPORT,)
    assert ref.doc_date == date(2020, 1, 1)
    assert ref.relpath == f"documents/{DOC_ID_REPORT}.pdf"


def test_extract_relevate_skips_noise(tmp_path):
    prior = _prior_run(tmp_path, DOC_ID_REPORT, BODY, REPORT_NAME)
    docs = prior / "documents"
    # A non-PDF shell left for an unexpected content type is ignored.
    (docs / f"{DOC_ID_OTHER}.unexpected.json").write_text("{}")
    # A non-numeric stem is ignored.
    (docs / "notanumber.pdf").write_bytes(BODY)
    # A symlinked PDF is never followed.
    external = tmp_path / "external.pdf"
    external.write_bytes(BODY)
    (docs / "9999.pdf").symlink_to(external)
    keys = {r.key for r in download.extract_relevate(prior, None)}
    assert keys == {(DOC_ID_REPORT,)}


def test_extract_relevate_no_documents_dir(tmp_path):
    d = tmp_path / OLD_TS
    d.mkdir()
    assert list(download.extract_relevate(d, None)) == []


def test_extract_relevate_missing_index_leaves_date_none(tmp_path):
    # No index.json → the PDF is still indexed by (doc_id,), but with no
    # pre-fetch date, so the freshness window never applies to it.
    d = tmp_path / OLD_TS
    docs = d / "documents"
    docs.mkdir(parents=True)
    (docs / f"{DOC_ID_FEE}.pdf").write_bytes(BODY)
    refs = list(download.extract_relevate(d, None))
    assert len(refs) == 1
    assert refs[0].key == (DOC_ID_FEE,)
    assert refs[0].doc_date is None


# ------------------------------------------------------------
# End-to-end dispatch by class
# ------------------------------------------------------------

def test_immutable_doc_is_linked(tmp_path):
    # An executed-once immutable, unparsed kind (fee statement) identical to a
    # prior run is hardlinked in — the fetch-avoidance win, safe because load
    # never parses it.
    prior = _prior_run(tmp_path, DOC_ID_FEE, BODY, FEE_NAME)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_FEE, FEE_NAME, BODY, calls)
    assert status == docdedup.LINKED
    assert calls == []                               # fetch avoided
    linked = run / "documents" / f"{DOC_ID_FEE}.pdf"
    assert _ino(linked) == _ino(prior / "documents" / f"{DOC_ID_FEE}.pdf")
    assert linked.read_bytes() == BODY


def test_parsed_report_unchanged_is_verified(tmp_path):
    # A quarterly report is fetch-verify (parsed → restatement-prone), NOT link:
    # it is ALWAYS fetched, and a byte-identical prior is hardlinked only to
    # reclaim disk (never a fetch-skipping stale link).
    prior = _prior_run(tmp_path, DOC_ID_REPORT, BODY, REPORT_NAME)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_REPORT, REPORT_NAME, BODY, calls)
    assert status == docdedup.VERIFIED
    assert calls == [DOC_ID_REPORT]                  # report ALWAYS fetched
    fetched = run / "documents" / f"{DOC_ID_REPORT}.pdf"
    assert _ino(fetched) == _ino(prior / "documents" / f"{DOC_ID_REPORT}.pdf")


def test_parsed_report_restated_is_kept(tmp_path):
    # The correctness case link-mode would get wrong: a quarterly report whose
    # bytes change between runs (a restatement under a stable Relevate id) is
    # fetched and its NEW bytes kept — never linked to a stale prior that would
    # feed a superseded position/cash figure into historical_position_snapshots.
    prior = _prior_run(tmp_path, DOC_ID_REPORT, BODY, REPORT_NAME)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_REPORT, REPORT_NAME, BODY2, calls)
    assert status == docdedup.CHANGED
    fetched = run / "documents" / f"{DOC_ID_REPORT}.pdf"
    assert fetched.read_bytes() == BODY2             # restated content kept
    assert (prior / "documents" / f"{DOC_ID_REPORT}.pdf").read_bytes() == BODY


def test_credit_note_corrected_is_kept(tmp_path):
    # A credit note is parsed into transactions; a re-issue under a stable id is
    # fetched and its NEW bytes kept, never linked to stale.
    _prior_run(tmp_path, DOC_ID_CREDIT, BODY, CREDIT_NAME)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_CREDIT, CREDIT_NAME, BODY2, calls)
    assert status == docdedup.CHANGED
    fetched = run / "documents" / f"{DOC_ID_CREDIT}.pdf"
    assert fetched.read_bytes() == BODY2


def test_unknown_kind_is_fetch_verified(tmp_path):
    # An unclassified kind is fetch-verified (the safe default): ALWAYS fetched,
    # but a byte-identical prior is still deduped to a hardlink.
    prior = _prior_run(tmp_path, DOC_ID_OTHER, BODY, OTHER_NAME)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_OTHER, OTHER_NAME, BODY, calls)
    assert status == docdedup.VERIFIED               # fetched, then deduped
    assert calls == [DOC_ID_OTHER]                   # the fetch DID run
    fetched = run / "documents" / f"{DOC_ID_OTHER}.pdf"
    assert _ino(fetched) == _ino(prior / "documents" / f"{DOC_ID_OTHER}.pdf")


def test_a_linked_document_must_still_be_a_pdf(tmp_path):
    """link-mode never re-fetches, so a prior copy that is an error page or a
    url envelope would be hardlinked forward for ever. Relevate's own fetch
    already refuses a non-PDF content type, so this is belt-and-braces here —
    but the guard costs five bytes and the failure it prevents is permanent."""
    _prior_run(tmp_path, DOC_ID_FEE, b'{"url": "https://cdn.example/x.pdf"}', FEE_NAME)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_FEE, FEE_NAME, BODY, calls)
    assert status == docdedup.FETCHED       # not LINKED
    assert len(calls) == 1
    assert (run / "documents" / f"{DOC_ID_FEE}.pdf").read_bytes() == BODY


def test_link_kind_within_freshness_window_is_refetched(tmp_path):
    # A link-kind doc issued inside the default 35-day freshness window is left
    # out of the index and re-fetched rather than linked — the safety margin
    # against a just-issued doc still being corrected under its id.
    recent = (date.today() - timedelta(days=5)).isoformat() + "T00:00:00"
    prior = _prior_run(tmp_path, DOC_ID_FEE, BODY, FEE_NAME, create_date=recent)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_FEE, FEE_NAME, BODY, calls)
    assert status == docdedup.FETCHED                # re-fetched, not LINKED
    assert calls == [DOC_ID_FEE]
    fetched = run / "documents" / f"{DOC_ID_FEE}.pdf"
    assert _ino(fetched) != _ino(prior / "documents" / f"{DOC_ID_FEE}.pdf")


def test_documents_force_bypasses_index(tmp_path):
    prior = _prior_run(tmp_path, DOC_ID_REPORT, BODY, REPORT_NAME)
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    calls: list = []
    status = _dispatch(run, skip, DOC_ID_REPORT, REPORT_NAME, BODY, calls,
                       force=True)
    assert status == docdedup.FETCHED                # forced fetch, no hardlink
    assert calls == [DOC_ID_REPORT]
    fetched = run / "documents" / f"{DOC_ID_REPORT}.pdf"
    assert _ino(fetched) != _ino(prior / "documents" / f"{DOC_ID_REPORT}.pdf")


# ------------------------------------------------------------
# Audit tally
# ------------------------------------------------------------

def test_tally_and_counts():
    counts = download._empty_doc_counts()
    for s in (docdedup.LINKED, docdedup.LINKED, docdedup.FETCHED,
              docdedup.VERIFIED, docdedup.CHANGED, docdedup.FETCH_FAILED):
        download._tally(counts, s)
    assert counts["linked"] == 2
    assert counts["fetched"] == 1
    assert counts["verified"] == 1
    assert counts["changed"] == 1
    assert counts["errors"] == 1
    assert counts["other"] == 0


def test_tally_unmapped_code_goes_to_other():
    # An outcome the map does not know must NOT inflate 'fetched'; it lands in
    # its own 'other' bucket so the audit stays honest.
    counts = download._empty_doc_counts()
    download._tally(counts, "totally-unexpected-code")
    assert counts["other"] == 1
    assert counts["fetched"] == 0


def test_fetch_failure_is_error(tmp_path):
    # A fetch that yields no PDF (the closure returns None: non-200, network
    # error, or a non-PDF body) surfaces as FETCH_FAILED, tallied under errors.
    run = bronze.run_dir(tmp_path, CUR)
    run.mkdir()
    skip = docdedup.SkipSet.derive(tmp_path, download.extract_relevate,
                                   exclude_run=run)
    entry = {"id": DOC_ID_REPORT, "fileName": REPORT_NAME}
    status = docdedup.process(
        skip, key=(DOC_ID_REPORT,), doc_class=download._document_class(entry),
        target_dir=run / "documents", stem=str(DOC_ID_REPORT),
        fetch=lambda: None)
    assert status == docdedup.FETCH_FAILED
    counts = download._empty_doc_counts()
    download._tally(counts, status)
    assert counts["errors"] == 1


# ============================================================
# Part 3 — the --debug HTTP trace (no network)
# ============================================================

class _FakeManifest:
    """The slice of Manifest get_and_save_json touches."""

    def __init__(self) -> None:
        self.errors: list[dict] = []
        self.files: list[str] = []

    def add_error(self, **kw) -> None:
        self.errors.append(kw)

    def add_file(self, rel: str) -> None:
        self.files.append(rel)


def _trace(tmp_path):
    return debugcap.HttpTrace(tmp_path, log=download.logger, enabled=True)


def _lines(tmp_path):
    p = tmp_path / debugcap.SCREENSHOTS_DIR / debugcap.HttpTrace.FILENAME
    return [json.loads(x) for x in p.read_text().splitlines()] if p.exists() else []


def _get_json(tmp_path, session, *, trace):
    return download.get_and_save_json(
        session, download.EP_INVESTMENT_OVERVIEW,
        tmp_path / "accounts" / "investment-overview.json",
        _FakeManifest(), relative_to=tmp_path, trace=trace)


def test_debug_parses_and_defaults_off():
    base = ["--bronze-dir", "/x"]
    assert download.parse_args(base).debug is False
    assert download.parse_args([*base, "--debug"]).debug is True


def test_untraced_request_writes_nothing(tmp_path):
    # The default trace is disabled, so a request records nothing.
    download.get_and_save_json(
        _FakeSession(), download.EP_INVESTMENT_OVERVIEW,
        tmp_path / "accounts" / "investment-overview.json",
        _FakeManifest(), relative_to=tmp_path)
    assert not (tmp_path / debugcap.SCREENSHOTS_DIR).exists()


def test_trace_records_a_request(tmp_path):
    _get_json(tmp_path, _FakeSession(), trace=_trace(tmp_path))
    entry = _lines(tmp_path)[0]
    assert (entry["method"], entry["status"]) == ("GET", 200)
    assert entry["url"].endswith(download.EP_INVESTMENT_OVERVIEW)
    assert entry["headers"]["content-type"] == "application/json"


def test_trace_records_a_transport_failure(tmp_path):
    # The walk records the error in its manifest and carries on; the trace
    # is what says how long it hung and what it was reaching for.
    class _Dead:
        def get(self, url, timeout=None, allow_redirects=None):
            raise download.RequestException("connection reset")

    assert _get_json(tmp_path, _Dead(), trace=_trace(tmp_path)) is None
    entry = _lines(tmp_path)[0]
    assert "status" not in entry          # nothing came back to carry one
    assert "connection reset" in entry["error"]


def test_trace_records_an_unexpected_status(tmp_path):
    # A session that has quietly died answers 302 to the login page, not an
    # error — exactly the case bronze alone cannot explain.
    class _Redirect:
        def get(self, url, timeout=None, allow_redirects=None):
            return _FakeResp(302, {})

    assert _get_json(tmp_path, _Redirect(), trace=_trace(tmp_path)) is None
    assert _lines(tmp_path)[0]["status"] == 302


def test_trace_redacts_a_credential_query_param(tmp_path):
    class _WithToken:
        def get(self, url, timeout=None, allow_redirects=None):
            return _FakeResp(200, {"portfolios": []})

    download.get_and_save_json(
        _WithToken(), download.EP_INVESTMENT_OVERVIEW + "?token=SYNTHETIC_TOKEN",
        tmp_path / "x.json", _FakeManifest(), relative_to=tmp_path,
        trace=_trace(tmp_path))
    assert "SYNTHETIC_TOKEN" not in json.dumps(_lines(tmp_path))


def test_dry_run_with_debug_persists_nothing_to_bronze(tmp_path, monkeypatch):
    # A dry-run walks into a throwaway temp dir, so --debug captures
    # nothing and the bronze root stays untouched.
    _patch_session(monkeypatch)
    bronze_dir = tmp_path / "bronze"
    bronze_dir.mkdir()

    rc = download.main([
        "--dry-run", "--debug",
        "--bronze-dir", str(bronze_dir),
        "--state-path", str(tmp_path / "state.json"),
    ])

    assert rc == 0
    assert list(bronze_dir.rglob("*")) == []
