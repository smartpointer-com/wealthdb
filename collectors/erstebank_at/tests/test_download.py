"""Unit tests for download.walk() against a fake data API: the run layout,
the roster and history paging, the lookback stop, the statement render
wait, and --dry-run. Synthetic values only."""
from __future__ import annotations

import json
import sys
from datetime import date, datetime, timezone
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
import george_client as george  # noqa: E402

ACCOUNT = "ACCT0000000000A1"
CARD = "CARD0000000000C1"
SHADOW = "SHDW0000000000S1"
PRODUCT = f"{SHADOW}-{CARD}"
PDF = b"%PDF-1.4 synthetic"


def _ms(day: date) -> int:
    return int(datetime(day.year, day.month, day.day, 12,
                        tzinfo=timezone.utc).timestamp() * 1000)


def _rows(prefix: str, days: list[date]) -> list[dict]:
    return [{"id": f"{prefix}{i}", "bookingDate": _ms(d),
             "amount": {"value": -1000, "precision": 2, "currency": "EUR"}}
            for i, d in enumerate(days)]


class FakeApi:
    """Routes the walk's calls to canned bodies and records them."""

    def __init__(self, *, history: dict[str, list[list[dict]]],
                 render_pending: int = 0):
        self.history = history
        self.render_pending = render_pending
        self.gets: list[str] = []
        self.posts: list[str] = []

    def get_json(self, url):
        self.gets.append(url)
        parts = urlsplit(url)
        q = {k: v[0] for k, v in parse_qs(parts.query).items()}
        path = parts.path
        if path.endswith("/my/accounts"):
            return 200, {"pageNumber": 0, "pageCount": 1, "pageSize": 1,
                         "accounts": [{"id": ACCOUNT, "type": "CURRENT"}]}
        if path.endswith("/my/cards"):
            return 200, {"cards": [
                {"id": "DEBIT00000000001", "type": "BANK_CARD"},
                {"id": CARD, "type": "CREDIT",
                 "creditCardShadowAccountId": SHADOW}]}
        if path.endswith("/transactions"):
            pages = self.history[q["id"]]
            page = int(q["page"])
            return 200, {"collection": pages[page], "totalPages": len(pages),
                         "totalElements": sum(map(len, pages))}
        if path.endswith("/statements"):
            return 200, {"pageNumber": 0, "pageCount": 1, "statements": [
                {"id": "100000000000000000001", "maxBookingDate": "2026-03-31",
                 "statementDate": "2026-04-01T00:00:00Z"},
                {"id": "100000000000000000002", "maxBookingDate": "2020-03-31",
                 "statementDate": "2020-04-01T00:00:00Z"}]}
        if path.endswith("/invoices"):
            return 200, [{"billId": "2000001", "to": _ms(date(2026, 3, 20)),
                          "billingDate": _ms(date(2026, 3, 21))}]
        raise AssertionError(f"unexpected GET {url}")

    def post_token_form(self, url):
        self.posts.append(url)
        if "/statements/download" in url and self.render_pending:
            self.render_pending -= 1
            return 422, b"{}", {"errors": [{"scope": "ASYNCHRON",
                                            "error": "FIELD_INVALID"}]}
        return 200, PDF, None


def _history():
    return {
        ACCOUNT: [_rows("a", [date(2026, 4, 2), date(2026, 3, 2)]),
                  _rows("b", [date(2026, 1, 2), date(2025, 6, 1)]),
                  _rows("c", [date(2024, 1, 1)])],
        PRODUCT: [_rows("k", [date(2026, 3, 10)])],
    }


def _run_dir(tmp_path: Path) -> Path:
    (run,) = [p for p in tmp_path.iterdir() if p.is_dir()]
    return run


def test_walk_writes_the_run_and_stops_history_at_the_lookback(tmp_path):
    api = FakeApi(history=_history())
    summary = download.walk(api, tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 4, 30))
    run = _run_dir(tmp_path)
    manifest = json.loads((run / "run.json").read_text())
    assert manifest["status"] == "complete"
    assert manifest["account_ids"] == [ACCOUNT]
    assert manifest["card_products"] == [PRODUCT]

    hist = json.loads((run / "history" / f"{ACCOUNT}.json").read_text())
    # Page 1 reaches before the window, so page 2 is never read.
    assert [r["id"] for r in hist["transactions"]] == ["a0", "a1", "b0", "b1"]
    assert hist["complete"] is True
    assert hist["kind"] == "account"
    assert not any("page=2" in u and ACCOUNT in u for u in api.gets)

    card = json.loads((run / "history" / f"{PRODUCT}.json").read_text())
    assert card["kind"] == "card" and len(card["transactions"]) == 1

    # Only the statement inside the window is downloaded.
    pdfs = sorted(p.name for p in (run / "statements" / ACCOUNT).glob("*.pdf"))
    assert pdfs == ["2026-04-01_100000000000000000001.pdf"]
    assert (run / "statements" / ACCOUNT / "listing.json").exists()
    inv = sorted(p.name for p in (run / "invoices" / PRODUCT).glob("*.pdf"))
    assert inv == ["2026-03-21_2000001.pdf"]
    assert summary["transactions"] == 5
    assert summary["statements"] == 1 and summary["card_statements"] == 1
    assert summary["incomplete_histories"] == 0
    assert json.loads((run / "cards.json").read_text())[0]["card_id"] == CARD


def test_lookback_all_reads_every_page(tmp_path):
    api = FakeApi(history=_history())
    download.walk(api, tmp_path, since=date(1990, 1, 1), until=date(2026, 4, 30))
    hist = json.loads(
        (_run_dir(tmp_path) / "history" / f"{ACCOUNT}.json").read_text())
    assert len(hist["transactions"]) == 5 and hist["complete"]


def test_a_rendering_statement_is_asked_again(tmp_path):
    waits = []
    api = FakeApi(history=_history(), render_pending=2)
    download.walk(api, tmp_path, since=date(2026, 1, 1),
                  until=date(2026, 4, 30), pump=waits.append)
    assert waits == [download.RENDER_WAIT_MS] * 2
    run = _run_dir(tmp_path)
    assert len(list((run / "statements" / ACCOUNT).glob("*.pdf"))) == 1


def test_a_slow_render_is_fetched_on_the_late_retry(tmp_path):
    waits = []
    api = FakeApi(history=_history(),
                  render_pending=download.RENDER_ATTEMPTS + 1)
    summary = download.walk(api, tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 4, 30), pump=waits.append)
    assert download.LATE_RETRY_WAIT_MS in waits
    assert summary["statements"] == 1 and summary["documents_failed"] == 0
    # The late retry comes after the card statements.
    kinds = ["card" if "/cards/" in u else "statement" for u in api.posts]
    assert kinds.index("card") < len(kinds) - 1 and kinds[-1] == "statement"


def test_a_statement_that_never_renders_is_counted_as_failed(tmp_path):
    api = FakeApi(history=_history(), render_pending=100)
    summary = download.walk(api, tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 4, 30))
    assert summary["statements"] == 0 and summary["documents_failed"] == 1
    assert summary["card_statements"] == 1
    assert sum("/statements/download" in u for u in api.posts) == \
        2 * download.RENDER_ATTEMPTS


def test_a_refused_document_is_not_retried(tmp_path):
    class Refusing(FakeApi):
        def post_token_form(self, url):
            self.posts.append(url)
            return 404, b"", {"errors": [{"error": "NOT_FOUND"}]}

    api = Refusing(history=_history())
    summary = download.walk(api, tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 4, 30))
    assert len(api.posts) == 2 and summary["documents_failed"] == 2


def test_no_documents_keeps_the_listings(tmp_path):
    api = FakeApi(history=_history())
    download.walk(api, tmp_path, since=date(2026, 1, 1),
                  until=date(2026, 4, 30), documents=False)
    run = _run_dir(tmp_path)
    assert api.posts == []
    assert (run / "statements" / ACCOUNT / "listing.json").exists()
    assert (run / "invoices" / PRODUCT / "listing.json").exists()


def test_dry_run_reads_the_rosters_only(tmp_path):
    api = FakeApi(history=_history())
    download.walk(api, tmp_path, since=date(2026, 1, 1),
                  until=date(2026, 4, 30), dry_run=True)
    run = _run_dir(tmp_path)
    assert json.loads((run / "run.json").read_text())["status"] == "dry-run"
    assert not any("/transactions" in u for u in api.gets)
    assert api.posts == []
    assert not (run / "history").exists()


def test_a_failed_later_history_page_marks_it_incomplete(tmp_path):
    class Flaky(FakeApi):
        def get_json(self, url):
            if "/transactions" in url and "page=1" in url:
                return 500, None
            return super().get_json(url)

    api = Flaky(history=_history())
    summary = download.walk(api, tmp_path, since=date(1990, 1, 1),
                            until=date(2026, 4, 30), documents=False)
    hist = json.loads(
        (_run_dir(tmp_path) / "history" / f"{ACCOUNT}.json").read_text())
    assert hist["complete"] is False
    assert summary["incomplete_histories"] == 1


def test_a_roster_that_ignores_the_page_parameter_fails_the_run(tmp_path):
    class Repeating(FakeApi):
        def get_json(self, url):
            if urlsplit(url).path.endswith("/my/accounts"):
                self.gets.append(url)
                return 200, {"pageNumber": 0, "pageCount": 3,
                             "accounts": [{"id": ACCOUNT}]}
            return super().get_json(url)

    with pytest.raises(RuntimeError, match="ignored page=1"):
        download.walk(Repeating(history=_history()), tmp_path,
                      since=date(2026, 1, 1), until=date(2026, 4, 30))
    manifest = json.loads((_run_dir(tmp_path) / "run.json").read_text())
    assert manifest["status"] == "in-progress"


def test_a_roster_short_of_its_declared_count_fails_the_run(tmp_path):
    class Short(FakeApi):
        def get_json(self, url):
            if urlsplit(url).path.endswith("/my/accounts"):
                return 200, {"pageNumber": 0, "pageCount": 1,
                             "pageTotalSize": 2,
                             "accounts": [{"id": ACCOUNT}]}
            return super().get_json(url)

    with pytest.raises(RuntimeError, match="declares 2"):
        download.walk(Short(history=_history()), tmp_path,
                      since=date(2026, 1, 1), until=date(2026, 4, 30))


def test_a_roster_past_the_page_cap_fails_the_run(tmp_path):
    class Endless(FakeApi):
        def get_json(self, url):
            if urlsplit(url).path.endswith("/my/accounts"):
                page = int(parse_qs(urlsplit(url).query).get("page", [0])[0])
                return 200, {"pageNumber": page, "pageCount": 10 ** 6,
                             "accounts": [{"id": f"A{page}"}]}
            return super().get_json(url)

    with pytest.raises(RuntimeError, match="page"):
        download.walk(Endless(history=_history()), tmp_path,
                      since=date(2026, 1, 1), until=date(2026, 4, 30))


def test_a_refused_bearer_stops_the_run(tmp_path):
    class Lapsing(FakeApi):
        def get_json(self, url):
            if "/transactions" in url:
                raise george.BearerEnded("HTTP 401")
            return super().get_json(url)

    with pytest.raises(george.BearerEnded):
        download.walk(Lapsing(history=_history()), tmp_path,
                      since=date(2026, 1, 1), until=date(2026, 4, 30))
    manifest = json.loads((_run_dir(tmp_path) / "run.json").read_text())
    assert manifest["status"] == "in-progress"


def test_a_refused_bearer_on_a_document_is_not_swallowed(tmp_path):
    class Lapsing(FakeApi):
        def post_token_form(self, url):
            raise george.BearerEnded("HTTP 403")

    with pytest.raises(george.BearerEnded):
        download.walk(Lapsing(history=_history()), tmp_path,
                      since=date(2026, 1, 1), until=date(2026, 4, 30))


def test_a_failed_statement_listing_is_written_nowhere(tmp_path):
    class NoListing(FakeApi):
        def get_json(self, url):
            if urlsplit(url).path.endswith("/statements"):
                return 500, None
            return super().get_json(url)

    summary = download.walk(NoListing(history=_history()), tmp_path,
                            since=date(2026, 1, 1), until=date(2026, 4, 30))
    run = _run_dir(tmp_path)
    assert not (run / "statements" / ACCOUNT / "listing.json").exists()
    assert (run / "invoices" / PRODUCT / "listing.json").exists()
    assert summary["listings_failed"] == 1 and summary["short"] is True


def test_a_failed_card_listing_is_written_nowhere(tmp_path):
    class NoInvoices(FakeApi):
        def get_json(self, url):
            if urlsplit(url).path.endswith("/invoices"):
                return 500, None
            return super().get_json(url)

    summary = download.walk(NoInvoices(history=_history()), tmp_path,
                            since=date(2026, 1, 1), until=date(2026, 4, 30))
    assert not (_run_dir(tmp_path) / "invoices" / PRODUCT
                / "listing.json").exists()
    assert summary["listings_failed"] == 1 and summary["short"] is True


def test_a_whole_run_is_not_short(tmp_path):
    summary = download.walk(FakeApi(history=_history()), tmp_path,
                            since=date(2026, 1, 1), until=date(2026, 4, 30))
    assert summary["short"] is False


def test_a_failed_document_or_history_makes_the_run_short(tmp_path):
    api = FakeApi(history=_history(), render_pending=100)
    summary = download.walk(api, tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 4, 30))
    assert summary["short"] is True


def test_a_body_that_is_not_a_pdf_is_not_written(tmp_path):
    class Html(FakeApi):
        def post_token_form(self, url):
            self.posts.append(url)
            return 200, b"<html>session expired</html>", None

    summary = download.walk(Html(history=_history()), tmp_path,
                            since=date(2026, 1, 1), until=date(2026, 4, 30))
    run = _run_dir(tmp_path)
    assert list(run.rglob("*.pdf")) == []
    assert summary["documents_failed"] == 2 and summary["short"] is True


def test_a_failed_roster_fails_the_run(tmp_path):
    class Down(FakeApi):
        def get_json(self, url):
            return 401, None

    with pytest.raises(RuntimeError):
        download.walk(Down(history={}), tmp_path, since=date(2026, 1, 1),
                      until=date(2026, 4, 30))
    manifest = json.loads((_run_dir(tmp_path) / "run.json").read_text())
    assert manifest["status"] == "in-progress"
