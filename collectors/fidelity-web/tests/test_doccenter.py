"""Unit tests for the document-center download predicate.

The doc center is API-driven: clicking a '(pdf)' row fires a POST whose
JSON response carries the PDF as base64, and the walk waits for that
response by predicate. Tax forms are served from a host other than the
one serving the page, so the browser preflights that URL — the
regression guarded here. Browserless: no live session, no Camoufox.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402

DOC_URL = ("https://dpservice.fidelity.com/retail-am-financialdoc/v1/"
           "financial-documents/download")


class FakeRequest:
    def __init__(self, method):
        self.method = method


class FakeResponse:
    def __init__(self, url, method="POST"):
        self.url = url
        self.request = FakeRequest(method)


def test_the_download_post_matches():
    assert download._is_docapi_download_response(FakeResponse(DOC_URL))


def test_the_cors_preflight_does_not_match():
    # The OPTIONS answers 200 with an empty body ahead of the real POST.
    # Taken for the response, it decodes to no PDF and the row fails as if
    # the endpoint had changed shape.
    assert not download._is_docapi_download_response(
        FakeResponse(DOC_URL, method="OPTIONS"))


def test_an_unrelated_response_does_not_match():
    assert not download._is_docapi_download_response(FakeResponse(
        "https://digitalservices.fidelity.com/navigate/ent-documentcenter/"
        "documents"))


def test_a_misshaped_response_is_not_a_match():
    # The predicate runs inside Playwright's matcher; raising there would
    # abort the wait rather than skip the response.
    class NoRequest:
        url = DOC_URL

    assert not download._is_docapi_download_response(NoRequest())
