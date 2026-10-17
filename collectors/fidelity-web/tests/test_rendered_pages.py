"""The balances and performance phases: each saves its page's rendered
DOM (download._save_rendered_page), since neither surface offers a
structured export. A fake page stands in for Playwright; synthetic HTML
only."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
from collectorkit import compress  # noqa: E402


class FakeLocator:
    def __init__(self, n):
        self._n = n

    def count(self):
        return self._n


class FakePage:
    def __init__(self, html, ready=0):
        self.html = html
        self.ready = ready
        self.selectors = []

    def content(self):
        return self.html

    def locator(self, selector):
        self.selectors.append(selector)
        return FakeLocator(self.ready)


@pytest.fixture
def landed(monkeypatch):
    """Stub the navigation; returns a setter for the URL the page lands on."""
    where = {"url": download.URL_BALANCES}
    monkeypatch.setattr(download, "goto_and_wait", lambda *a, **k: None)
    monkeypatch.setattr(download, "capture", lambda *a, **k: None)
    monkeypatch.setattr(download, "live_url", lambda page: where["url"])
    monkeypatch.setattr(download.time, "sleep", lambda s: None)
    return lambda url: where.update(url=url)


def _saved(bronze, name):
    return compress.resolve_variant(bronze / name / f"{name}.html")


def test_balances_waits_for_a_total_and_saves_the_page(tmp_path, landed):
    page = FakePage("<html><span>synthetic</span></html>", ready=1)
    result = download.scrape_balances(page, tmp_path, None)
    assert result["status"] == "explored-no-export"
    assert page.selectors == ["[data-testid$='-totalaccountvalue-label']"]
    saved = _saved(tmp_path, "balances")
    assert result["file"] == str(saved.relative_to(tmp_path))
    with compress.open_text(saved) as f:
        assert "synthetic" in f.read()


def test_performance_saves_the_page_without_waiting(tmp_path, landed):
    landed(download.URL_PERFORMANCE)
    page = FakePage("<html><div>1yr +0.00%</div></html>")
    result = download.scrape_performance(page, tmp_path, None)
    assert result["status"] == "explored-no-export"
    assert page.selectors == []
    assert _saved(tmp_path, "performance") is not None


def test_a_session_timeout_redirect_saves_nothing(tmp_path, landed):
    landed("https://digital.fidelity.com/prgw/digital/signin/retail")
    result = download.scrape_balances(FakePage("<html/>"), tmp_path, None)
    assert result["status"] == "session-timeout"
    assert _saved(tmp_path, "balances") is None
