"""Unit tests for carta download.py's docdedup wiring (no browser).

Exercises the pieces the live document walk builds on — the per-document class
mapping over the free-text `document_type`, the disk-driven extract hook, the
(doc_id,) keying, and the real _process_document / _fetch_document path driven by
a stub Api (no network, no Camoufox). Asserts each document class behaves per the
carta mode mapping:

  * executed-once archival notices/reports (quarterly & annual financials,
    capital-call & distribution notices) → LINKED from a prior run, fetch avoided
    (safe: immutable under their id, never parsed by load.py);
  * parsed / restatement-prone docs — capital-account statements (parsed for NAV
    + fund cash flows) and tax docs (K-1, 1042-S) — → ALWAYS fetched and
    content-compared (unchanged one hardlinked, a restated/corrected one KEPT —
    the re-issue case link-mode would miss);
  * an unrecognised type → always fetched (fail-safe);
  * a row with no document_url / id → NO_BLOB (null-blob-by-design, not an
    error); a real fetch failure → an error;
  * the within-run guard collapses a paginated duplicate to a single fetch;
  * _tally routes an unmapped outcome to 'other', never inflating 'fetched';
  * --documents-force bypasses the cross-run index.

Also covers the cap-table access gate: a holder the dashboard reports as
having no cap-table access skips the cap-table-only endpoints entirely
(no request, no recorded error).

Synthetic ids / labels / bytes only — no real document ids, fund/company names,
or figures.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
from collectorkit import docdedup  # noqa: E402

# Synthetic surrogate document ids (Carta's numeric/uuid `id`, stringified).
STMT_ID = "1000001"
K1_ID = "1000002"
LINK_ID = "1000003"          # an archival notice (link-mode)
OTHER_ID = "1000004"

BODY = b"%PDF-1.4 synthetic statement body " + b"x" * 200
BODY2 = b"%PDF-1.4 corrected synthetic body " + b"y" * 200

OLD = "20260101T010000Z"
CUR = "20260201T010000Z"

# A document_url shaped like Carta's (relative → normalised to APP by
# _process_document) and its signed-CDN envelope target.
DOC_URL = "/api/investors/funds/1/lp_documents/synthetic/download/"
SIGNED_URL = "https://documents.carta.com/synthetic/blob.pdf"


class _StubApi:
    """Minimal stand-in for download.Api. The document_url GET returns
    ``envelope`` (a {"url": signed} JSON envelope), the signed GET returns
    ``binary``. Records every call so a test can assert whether a fetch actually
    ran (link-mode must avoid it)."""

    def __init__(self, *, envelope=None, binary=None):
        self.envelope = envelope if envelope is not None else {"url": SIGNED_URL}
        self.binary = binary
        self.calls: list[tuple[str, str]] = []

    def get(self, url, *, expect="json"):
        self.calls.append((url, expect))
        if expect == "bytes":
            return 200, self.binary
        return 200, self.envelope


def _prior_run(root: Path, doc_id: str, body: bytes,
               status: str = "complete") -> Path:
    """A synthetic COMPLETE prior carta run holding one document blob."""
    d = root / OLD
    blob = d / "documents" / f"doc_{doc_id}.pdf"
    blob.parent.mkdir(parents=True)
    blob.write_bytes(body)
    (d / "run.json").write_text(json.dumps({"status": status}))
    return d


def _new_run(root: Path) -> Path:
    run = root / CUR
    run.mkdir()
    (run / "run.json").write_text(json.dumps({"status": "in-progress"}))
    return run


def _skip(root: Path, run: Path) -> docdedup.SkipSet:
    return docdedup.SkipSet.derive(root, download.extract_carta,
                                   freshness_days=None, exclude_run=run)


def _process(run: Path, skip, doc_id: str, dtype: str | None, body: bytes,
             *, force: bool = False):
    """Drive the real _process_document with a stub Api returning ``body``."""
    api = _StubApi(binary=body)
    row = {"id": doc_id, "document_type": dtype, "document_url": DOC_URL}
    status = download._process_document(
        skip, api, row, run / "documents", force=force)
    return status, api


def _ino(p: Path) -> int:
    return p.stat().st_ino


def _blob(run: Path, doc_id: str) -> Path:
    return run / "documents" / f"doc_{doc_id}.pdf"


# ============================================================
# document_type → class mapping
# ============================================================

def test_document_class_mapping():
    # Tax → CLASS_TAX; every listed tax label resolves to fetch-verify.
    for t in ("Tax - Schedule K-1", "1042-S", "1099-B", "Tax"):
        assert download._document_class({"document_type": t}) == docdedup.CLASS_TAX
        assert docdedup.mode_for_class(
            download._document_class({"document_type": t})) == docdedup.MODE_FETCH_VERIFY
    # Capital-account statements (parsed by load.py) → CLASS_MUTABLE → fetch-verify.
    for t in ("Capital account statement", "Capital account statements"):
        assert download._document_class({"document_type": t}) == docdedup.CLASS_MUTABLE
        assert docdedup.mode_for_class(
            download._document_class({"document_type": t})) == docdedup.MODE_FETCH_VERIFY
    # Executed-once archival notices/reports → CLASS_IMMUTABLE → link.
    for t in ("Annual and quarterly report", "Quarterly report",
              "Distributions", "Distribution notice", "Capital calls",
              "Capital call notice", "Financial statements"):
        assert download._document_class({"document_type": t}) == docdedup.CLASS_IMMUTABLE
        assert docdedup.mode_for_class(
            download._document_class({"document_type": t})) == docdedup.MODE_LINK
    # Unknown / absent → unclassified → fetch-verify (the safe default).
    assert download._document_class({"document_type": "Some brand-new type"}) is None
    assert download._document_class({"document_type": ""}) is None
    assert download._document_class({}) is None
    assert docdedup.mode_for_class(None) == docdedup.MODE_FETCH_VERIFY


def test_parsed_statement_never_links():
    # Correctness invariant: any label load.py would parse (contains "apital
    # account") is CLASS_MUTABLE (fetch-verify), never immutable/link — even when
    # it also carries a link-ish word.
    for t in ("Quarterly capital account statement",
              "Annual capital account statement"):
        cls = download._document_class({"document_type": t})
        assert cls == docdedup.CLASS_MUTABLE
        assert docdedup.mode_for_class(cls) == docdedup.MODE_FETCH_VERIFY


# ============================================================
# extract_carta (disk-driven)
# ============================================================

def test_extract_carta_yields_docrefs(tmp_path):
    prior = _prior_run(tmp_path, STMT_ID, BODY)
    refs = list(download.extract_carta(prior, None))
    assert len(refs) == 1
    ref = refs[0]
    assert ref.key == (STMT_ID,)
    assert ref.doc_date is None
    assert ref.relpath == f"documents/doc_{STMT_ID}.pdf"


def test_extract_carta_skips_symlinks_and_non_pdf(tmp_path):
    prior = _prior_run(tmp_path, STMT_ID, BODY)
    # index.json and a stray non-doc file must not be enumerated.
    (prior / "documents" / "index.json").write_text("{}")
    (prior / "documents" / "notes.txt").write_text("x")
    # A symlinked doc_*.pdf is never followed.
    external = tmp_path / "external"
    external.mkdir()
    (external / "real.pdf").write_bytes(BODY)
    (prior / "documents" / f"doc_{OTHER_ID}.pdf").symlink_to(external / "real.pdf")
    keys = {r.key for r in download.extract_carta(prior, None)}
    assert keys == {(STMT_ID,)}


def test_extract_carta_no_documents_dir(tmp_path):
    d = tmp_path / OLD
    d.mkdir()
    assert list(download.extract_carta(d, None)) == []


# ============================================================
# End-to-end dispatch by class (real _process_document + _fetch_document)
# ============================================================

def test_archival_notice_is_linked(tmp_path):
    # An archival distribution notice identical to a prior run is hardlinked in —
    # the fetch-avoidance win, safe because it is immutable and not parsed.
    prior = _prior_run(tmp_path, LINK_ID, BODY)
    run = _new_run(tmp_path)
    status, api = _process(run, _skip(tmp_path, run), LINK_ID, "Distributions", BODY)
    assert status == docdedup.LINKED
    assert api.calls == []                                   # fetch avoided
    assert _ino(_blob(run, LINK_ID)) == _ino(_blob(prior, LINK_ID))
    assert _blob(run, LINK_ID).read_bytes() == BODY


def test_statement_unchanged_is_verified(tmp_path):
    # A capital-account statement is fetch-verify (parsed → restatement-prone),
    # NOT link: ALWAYS fetched, a byte-identical prior hardlinked only to reclaim
    # disk (never a fetch-skipping stale link).
    prior = _prior_run(tmp_path, STMT_ID, BODY)
    run = _new_run(tmp_path)
    status, api = _process(run, _skip(tmp_path, run), STMT_ID,
                           "Capital account statement", BODY)
    assert status == docdedup.VERIFIED
    assert api.calls                                         # statement ALWAYS fetched
    assert _ino(_blob(run, STMT_ID)) == _ino(_blob(prior, STMT_ID))


def test_statement_restated_is_kept(tmp_path):
    # The correctness case link-mode would get wrong: a statement whose bytes
    # change between runs (a restatement under a stable doc id) is fetched and its
    # NEW bytes kept — never linked to a stale prior that would feed a superseded
    # NAV into the silver replay.
    prior = _prior_run(tmp_path, STMT_ID, BODY)
    run = _new_run(tmp_path)
    status, _ = _process(run, _skip(tmp_path, run), STMT_ID,
                         "Capital account statement", BODY2)
    assert status == docdedup.CHANGED
    assert _blob(run, STMT_ID).read_bytes() == BODY2         # restated content kept
    assert _blob(prior, STMT_ID).read_bytes() == BODY


def test_tax_k1_unchanged_is_verified(tmp_path):
    prior = _prior_run(tmp_path, K1_ID, BODY)
    run = _new_run(tmp_path)
    status, api = _process(run, _skip(tmp_path, run), K1_ID,
                           "Tax - Schedule K-1", BODY)
    assert status == docdedup.VERIFIED
    assert api.calls                                         # tax docs ALWAYS fetched
    assert _ino(_blob(run, K1_ID)) == _ino(_blob(prior, K1_ID))


def test_tax_k1_corrected_is_kept(tmp_path):
    # A K-1 re-issued under a stable id is fetched and its NEW bytes kept.
    prior = _prior_run(tmp_path, K1_ID, BODY)
    run = _new_run(tmp_path)
    status, _ = _process(run, _skip(tmp_path, run), K1_ID,
                         "Tax - Schedule K-1", BODY2)
    assert status == docdedup.CHANGED
    assert _blob(run, K1_ID).read_bytes() == BODY2           # corrected content kept
    assert _blob(prior, K1_ID).read_bytes() == BODY


def test_unknown_type_is_fetch_verified(tmp_path):
    # An unclassified type is fetch-verified: ALWAYS fetched (never fetch-avoided
    # by a link), but a byte-identical prior still deduped to a hardlink.
    prior = _prior_run(tmp_path, OTHER_ID, BODY)
    run = _new_run(tmp_path)
    status, api = _process(run, _skip(tmp_path, run), OTHER_ID,
                           "Some brand-new type", BODY)
    assert status == docdedup.VERIFIED                       # fetched, then deduped
    assert api.calls                                         # the fetch DID run
    assert _ino(_blob(run, OTHER_ID)) == _ino(_blob(prior, OTHER_ID))


def test_unknown_type_changed_keeps_fresh(tmp_path):
    prior = _prior_run(tmp_path, OTHER_ID, BODY)
    run = _new_run(tmp_path)
    status, _ = _process(run, _skip(tmp_path, run), OTHER_ID,
                         "Some brand-new type", BODY2)
    assert status == docdedup.CHANGED
    assert _blob(run, OTHER_ID).read_bytes() == BODY2


def test_documents_force_bypasses_index(tmp_path):
    prior = _prior_run(tmp_path, STMT_ID, BODY)
    run = _new_run(tmp_path)
    status, api = _process(run, _skip(tmp_path, run), STMT_ID,
                           "Capital account statement", BODY, force=True)
    assert status == docdedup.FETCHED                        # forced fetch, no hardlink
    assert api.calls
    assert _ino(_blob(run, STMT_ID)) != _ino(_blob(prior, STMT_ID))


def test_force_bypasses_even_for_immutable(tmp_path):
    # --documents-force also disables link-mode: an archival notice is re-fetched.
    prior = _prior_run(tmp_path, LINK_ID, BODY)
    run = _new_run(tmp_path)
    status, api = _process(run, _skip(tmp_path, run), LINK_ID,
                           "Distributions", BODY, force=True)
    assert status == docdedup.FETCHED
    assert api.calls                                         # fetch NOT avoided
    assert _ino(_blob(run, LINK_ID)) != _ino(_blob(prior, LINK_ID))


# ============================================================
# NO_BLOB / error outcomes (via _process_document)
# ============================================================

def test_no_url_row_is_no_blob_not_error(tmp_path):
    run = _new_run(tmp_path)
    api = _StubApi()
    row = {"id": STMT_ID, "document_type": "Capital account statement"}  # no URL
    status = download._process_document(
        api=api, skip=_skip(tmp_path, run), row=row,
        docs_dir=run / "documents", force=False)
    assert status == download.NO_BLOB
    assert api.calls == []                                   # fetch path never entered
    counts = download._empty_doc_counts()
    download._tally(counts, status)
    assert counts["no_blob"] == 1 and counts["errors"] == 0


def test_no_id_row_is_no_blob(tmp_path):
    run = _new_run(tmp_path)
    api = _StubApi()
    row = {"document_type": "Distributions", "document_url": DOC_URL}    # no id/uuid
    status = download._process_document(
        api=api, skip=_skip(tmp_path, run), row=row,
        docs_dir=run / "documents", force=False)
    assert status == download.NO_BLOB
    assert api.calls == []


def test_missing_signed_url_is_fetch_failed(tmp_path):
    # URL IS present but the envelope carries no signed url → the fetch produced
    # no file → FETCH_FAILED, tallied as an error.
    run = _new_run(tmp_path)
    api = _StubApi(envelope={}, binary=None)
    row = {"id": STMT_ID, "document_type": "Capital account statement",
           "document_url": DOC_URL}
    status = download._process_document(
        api=api, skip=_skip(tmp_path, run), row=row,
        docs_dir=run / "documents", force=False)
    assert status == docdedup.FETCH_FAILED
    counts = download._empty_doc_counts()
    download._tally(counts, status)
    assert counts["errors"] == 1 and counts["no_blob"] == 0


def test_non_binary_body_is_fetch_failed(tmp_path):
    # Signed url resolves but returns a JSON/HTML error page, not a binary →
    # FETCH_FAILED (the data[:1] guard), no blob written.
    run = _new_run(tmp_path)
    api = _StubApi(binary=b'{"error": "expired"}')
    row = {"id": K1_ID, "document_type": "Tax - Schedule K-1",
           "document_url": DOC_URL}
    status = download._process_document(
        api=api, skip=_skip(tmp_path, run), row=row,
        docs_dir=run / "documents", force=False)
    assert status == docdedup.FETCH_FAILED
    assert not _blob(run, K1_ID).exists()


# ============================================================
# Within-run guard + full capture_documents walk
# ============================================================

class _WalkApi(_StubApi):
    """_StubApi that also answers the paginated received-documents index."""

    def __init__(self, pages, **kw):
        super().__init__(**kw)
        self.pages = pages

    def get(self, url, *, expect="json"):
        if "get-all-received-documents" in url:
            import re
            m = re.search(r"page=(\d+)", url)
            n = int(m.group(1)) if m else 1
            page = self.pages[n - 1] if n - 1 < len(self.pages) else \
                {"results": [], "has_next": False}
            return 200, page
        return super().get(url, expect=expect)


def test_within_run_guard_collapses_paginated_duplicate(tmp_path):
    # The same document appearing on two index pages is fetched ONCE — the
    # within-run guard (KEEP-it) short-circuits the second occurrence before the
    # cross-run docdedup layer runs again.
    run = _new_run(tmp_path)
    row = {"id": STMT_ID, "document_type": "Capital account statement",
           "document_url": DOC_URL}
    api = _WalkApi(pages=[{"results": [row], "has_next": True},
                          {"results": [row], "has_next": False}], binary=BODY)
    n_index, n_pdf, counts = download.capture_documents(
        api, "42", run / "documents", skip=_skip(tmp_path, run), force=False)
    assert n_index == 2                          # two index rows (a duplicate)
    assert counts["total"] == 1                  # one distinct document processed
    assert counts["fetched"] == 1                # fetched exactly once
    assert n_pdf == 1                            # one PDF on disk (both rows → one file)


def test_within_run_guard_no_url_row_not_re_tallied(tmp_path):
    # A no-url row recurring across index pages resolves to no_blob ONCE — the
    # seen-by-id guard processes each logical doc a single time, so the audit is
    # not inflated per page (no url → no file on disk either).
    run = _new_run(tmp_path)
    row = {"id": STMT_ID, "document_type": "Distributions"}   # no document_url
    api = _WalkApi(pages=[{"results": [row], "has_next": True},
                          {"results": [row], "has_next": False}], binary=BODY)
    n_index, n_pdf, counts = download.capture_documents(
        api, "42", run / "documents", skip=_skip(tmp_path, run), force=False)
    assert n_index == 2
    assert counts["total"] == 1                  # counted once despite two rows
    assert counts["no_blob"] == 1                # not inflated to 2
    assert n_pdf == 0


# ============================================================
# Audit tally
# ============================================================

def test_tally_and_counts():
    counts = download._empty_doc_counts()
    for s in (docdedup.LINKED, docdedup.LINKED, docdedup.FETCHED,
              docdedup.VERIFIED, docdedup.CHANGED, docdedup.FETCH_FAILED,
              download.NO_BLOB):
        download._tally(counts, s)
    assert counts == {"total": 0, "fetched": 1, "linked": 2, "verified": 1,
                      "changed": 1, "errors": 1, "no_blob": 1, "other": 0}


def test_tally_unmapped_code_goes_to_other():
    counts = download._empty_doc_counts()
    download._tally(counts, "totally-unexpected-code")
    assert counts["other"] == 1
    assert counts["fetched"] == 0


def test_lookback_flag():
    # Accepted for fleet uniformity: carta's download captures a full
    # holdings snapshot, so --lookback only drives a warning; the
    # value is still validated against the shared presets so a typo fails loudly.
    assert download.parse_args(["--lookback", "6m"]).lookback == "6m"
    assert download.parse_args([]).lookback is None
    with pytest.raises(SystemExit):
        download.parse_args(["--lookback", "1m"])  # not a preset


def test_no_documents_flag():
    # The fleet-wide document opt-out: the walk runs capture_documents only
    # when the flag is absent, and run.json's documents block records
    # skipped=true so a partial run is not read as one that found no
    # documents. Default off.
    assert download.parse_args(["--bronze-dir", "/tmp", "--no-documents"]).no_documents is True
    assert download.parse_args(["--bronze-dir", "/tmp"]).no_documents is False


def test_no_documents_help_promises_a_skip(capsys):
    # Guards against the flag regressing to a warn-only stub.
    with pytest.raises(SystemExit):
        download.parse_args(["--help"])
    out = capsys.readouterr().out
    assert "--no-documents" in out
    assert "NOT YET IMPLEMENTED" not in out


# ============================================================
# --debug: the bronze-resident landing capture
# ============================================================

# Shaped like the settled app URL land_and_get_individual_id reads the
# individual id out of. Synthetic id.
LANDING_URL = "https://app.carta.com/investors/individual/1/portfolio/"


class _FakePage:
    """Camoufox page stand-in for the landing capture. Reports a fixed
    ``url`` (the routing land_and_get_individual_id parses) and answers
    content() / screenshot() the way debugcap drives them."""

    def __init__(self, url: str):
        self.url = url

    def goto(self, *a, **k):
        pass

    def wait_for_timeout(self, ms):
        pass

    def content(self):
        return "<html>synthetic landing</html>"

    def screenshot(self, *, path, full_page=False):
        Path(path).write_bytes(b"\x89PNG synthetic")


class _FakeResponse:
    def __init__(self, body):
        self.status = 200
        self._body = body

    def json(self):
        return self._body

    def text(self):
        return ""


class _FakeRequest:
    """Answers download.Api's GETs from a url-fragment → body map; any
    unrouted URL returns an empty object, as a 200-with-nothing would."""

    def __init__(self, routes: dict):
        self.routes = routes

    def get(self, url, **kw):
        for frag, body in self.routes.items():
            if frag in url:
                return _FakeResponse(body)
        return _FakeResponse({})


class _FakeContext:
    def __init__(self, page, routes):
        self._page = page
        self.request = _FakeRequest(routes)

    def new_page(self):
        return self._page


# firm_id resolves from navigation-config; the portfolio holds no entities,
# so the walk finalises immediately after the landing.
_ROUTES = {"navigation-config": {"organizationPk": 9},
           "list_individual_portfolio_investments": []}


def _run(tmp_path, page, *flags, debug_dir=None):
    """Drive the real run() over a stub context — no Camoufox, no network.
    --no-documents keeps the walk to bootstrap + the (empty) entity list."""
    args = download.parse_args(
        ["--bronze-dir", str(tmp_path), "--no-documents", *flags])
    run_dir = tmp_path / "20260101T010000Z"
    run_dir.mkdir()
    ctx = _FakeContext(page, _ROUTES)
    return download.run(ctx, args, run_dir, 0, debug_dir), run_dir


def _captures(run_dir: Path) -> set[str]:
    d = run_dir / "screenshots"
    return {p.name for p in d.iterdir()} if d.exists() else set()


def test_debug_flag():
    # Off by default; parses cleanly when passed.
    assert download.parse_args(["--bronze-dir", "/tmp", "--debug"]).debug is True
    assert download.parse_args(["--bronze-dir", "/tmp"]).debug is False


def test_debug_help_promises_bronze_captures(capsys):
    # Guards against the flag regressing to a warn-only stub.
    with pytest.raises(SystemExit):
        download.parse_args(["--help"])
    out = capsys.readouterr().out
    assert "--debug" in out
    assert "gates nothing" not in out
    assert "NOT YET IMPLEMENTED" not in out


def test_debug_off_writes_no_captures(tmp_path):
    # The default: run() is handed no debug_dir and the run dir stays
    # capture-free.
    rc, run_dir = _run(tmp_path, _FakePage(LANDING_URL))
    assert rc == 0
    assert not (run_dir / "screenshots").exists()


def test_debug_on_captures_landing(tmp_path):
    # The landing is carta's only rendered surface, so it is the whole
    # browser-side diagnostic: DOM + screenshot, inside the run dir.
    page = _FakePage(LANDING_URL)
    rc, run_dir = _run(tmp_path, page, "--debug", debug_dir=None)
    assert rc == 0
    assert not (run_dir / "screenshots").exists(), \
        "run() captures only when handed a debug_dir, not off args.debug"


def test_debug_dir_captures_landing(tmp_path):
    page = _FakePage(LANDING_URL)
    run_dir = tmp_path / "20260101T010000Z"
    run_dir.mkdir()
    args = download.parse_args(
        ["--bronze-dir", str(tmp_path), "--no-documents", "--debug"])
    rc = download.run(_FakeContext(page, _ROUTES), args, run_dir, 0, run_dir)
    assert rc == 0
    assert _captures(run_dir) == {"10-landing.html", "10-landing.png"}
    assert (run_dir / "screenshots" / "10-landing.html").read_text() == \
        "<html>synthetic landing</html>"


def test_debug_captures_landing_even_when_id_unresolved(tmp_path):
    # The raise path matters most: a landing that never resolved an
    # individual id is precisely the failure the capture explains, so it
    # must survive land_and_get_individual_id giving up.
    page = _FakePage("https://app.carta.com/somewhere-unexpected/")
    run_dir = tmp_path / "20260101T010000Z"
    run_dir.mkdir()
    args = download.parse_args(
        ["--bronze-dir", str(tmp_path), "--no-documents", "--debug"])
    with pytest.raises(RuntimeError):
        download.run(_FakeContext(page, _ROUTES), args, run_dir, 0, run_dir)
    assert _captures(run_dir) == {"10-landing.html", "10-landing.png"}


# ---- cap-table access gate --------------------------------------------------
# A holder without cap-table access (captable_access_level "no access", e.g.
# a SAFE-only stake) gets a permanent 403 from the cap-table-only endpoints;
# the walk must skip them (INFO, no recorded error) rather than request them.

def test_captable_accessible_no_access_skips():
    assert download._captable_accessible(
        {"captable_access_level": "no access"}) is False


def test_captable_accessible_fetches_otherwise():
    # Any other level, a missing key, an unreadable dashboard, or schema
    # drift keeps the fetch-and-see behaviour.
    assert download._captable_accessible(
        {"captable_access_level": "summary cap table"}) is True
    assert download._captable_accessible({}) is True
    assert download._captable_accessible(None) is True
    assert download._captable_accessible(["not", "a", "dict"]) is True


class _RecordingApi:
    """Stub Api: canned 200 bodies per URL substring, every request logged."""

    def __init__(self, dashboard: dict):
        self._dashboard = dashboard
        self.urls: list[str] = []
        self.errors: list[dict] = []

    def get(self, url, *, expect="json"):
        self.urls.append(url)
        if "holdings-dashboard" in url:
            return 200, self._dashboard
        return 200, {"rows": []}


def _walk_captable(tmp_path, dashboard):
    api = _RecordingApi(dashboard)
    n_vest, captable_ok = download.capture_captable(
        api, "111", "222", tmp_path / "corp_222")
    return api, n_vest, captable_ok


def test_captable_no_access_never_requests_gated_endpoints(tmp_path):
    api, n_vest, captable_ok = _walk_captable(
        tmp_path, {"captable_access_level": "no access"})
    assert captable_ok is False and n_vest == 0
    assert not [u for u in api.urls if "post-money-list" in u]
    assert api.errors == []


def test_captable_with_access_requests_post_money(tmp_path):
    api, n_vest, captable_ok = _walk_captable(
        tmp_path, {"captable_access_level": "summary cap table"})
    assert captable_ok is True and n_vest == 0
    assert [u for u in api.urls if "post-money-list" in u]
