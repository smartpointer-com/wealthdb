"""Shared fixtures: the collector's modules on the path, and a scripted
stand-in for Plaid. No test reaches the network."""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import items  # noqa: E402
import plaidapi  # noqa: E402

# Synthetic tokens in Plaid's own shape, <type>-<environment>-<uuid>.
UUID = "00000000-0000-4000-8000-00000000000{n}"


def access_token(environment: str = "sandbox", n: int = 1) -> str:
    return f"access-{environment}-" + UUID.format(n=n)


def public_token(n: int = 1) -> str:
    return "public-sandbox-" + UUID.format(n=n)


LINK_TOKEN = "link-sandbox-" + UUID.format(n=9)
HOSTED_URL = "https://hosted.example.invalid/hl/synthetic"


def link_session(session_id: str = "s1", *, finished: bool = True,
                 public_tokens=(), exit_error: dict | None = None,
                 exited: bool = False, minute: int = 4,
                 events=()) -> dict:
    """One entry of a /link/token/get answer's `link_sessions`, in the
    shape Plaid's Sandbox returns. `minute` places its start, which is what
    orders sessions; `events` are (event_id, name, metadata) triples."""
    doc: dict = {
        "link_session_id": session_id,
        "started_at": f"2026-01-02T03:{minute:02d}:05.123456789Z",
        "results": {"item_add_results": [
            {"public_token": t, "accounts": [],
             "institution": {"institution_id": "ins_000",
                             "name": "Synthetic Bank"}}
            for t in public_tokens]},
        "events": [{"event_id": event_id, "event_name": name,
                    "timestamp": f"2026-01-02T03:{minute:02d}:{n:02d}Z",
                    "event_metadata": metadata}
                   for n, (event_id, name, metadata) in enumerate(events)],
    }
    if finished:
        doc["finished_at"] = f"2026-01-02T03:{minute:02d}:55.123456789Z"
    if exit_error is not None or exited:
        doc["exit"] = {"error": exit_error,
                       "metadata": {"institution": {},
                                    "link_session_id": session_id}}
    return doc


def plaid_error(code: str, status: int = 400,
                message: str = "synthetic error",
                error_type: str = "INVALID_INPUT") -> plaidapi.PlaidError:
    return plaidapi.PlaidError("/synthetic", status, {
        "error_type": error_type, "error_code": code,
        "error_message": message, "request_id": "req-synthetic"})


def store(secrets, name="bank", environment="sandbox", n=1) -> items.Item:
    item = items.Item(
        name=name, environment=environment,
        access_token=access_token(environment, n),
        item_id=f"item-synthetic-{n}", institution_id="ins_000",
        institution_name="Synthetic Bank")
    items.save_item(secrets, item)
    return item


class FakePlaid:
    """Answers what a test scripts and records what was asked.

    An answer may be an exception instance, which is raised. `link_docs`
    is consumed one per poll; the last one repeats, as a real session
    that has reached its end keeps answering the same.
    """

    def __init__(self, environment: str = "sandbox"):
        self.environment = environment
        self.calls: list[tuple] = []
        self.link_docs: list = [{"link_sessions": []}]
        self.exchanges: dict = {}
        self.item_docs: dict = {}
        self.keys_answer = {"institutions": [{}], "total": 1}
        self.institution = {"institution_id": "ins_000",
                            "name": "Synthetic Bank",
                            "products": list(plaidapi.DATA_PRODUCTS)}
        self.remove_answer = {"request_id": "req-synthetic"}
        self.update_link_answer = {
            "link_token": LINK_TOKEN, "hosted_link_url": HOSTED_URL,
            "expiration": "2026-01-02T04:04:05Z"}
        self.data: dict = {}
        self.data_of: dict = {}
        self.on_exchange = None

    def _answer(self, value):
        if isinstance(value, BaseException):
            raise value
        return value

    def called(self, name: str) -> list[tuple]:
        return [c for c in self.calls if c[0] == name]

    def link_token_for_new_item(self, *, required, country_codes,
                                lifetime_seconds):
        self.calls.append(("new_link", required, tuple(country_codes),
                           lifetime_seconds))
        return {"link_token": LINK_TOKEN, "hosted_link_url": HOSTED_URL,
                "expiration": "2026-01-02T04:04:05Z"}

    def link_token_for_update(self, *, access_token, country_codes,
                              lifetime_seconds):
        self.calls.append(("update_link", access_token,
                           tuple(country_codes), lifetime_seconds))
        return self._answer(self.update_link_answer)

    def link_token_get(self, link_token):
        self.calls.append(("link_get", link_token))
        doc = (self.link_docs.pop(0) if len(self.link_docs) > 1
               else self.link_docs[0])
        return self._answer(doc)

    def exchange_public_token(self, public_token):
        self.calls.append(("exchange", public_token))
        return self._answer(self.exchanges[public_token])

    def item_get(self, access_token):
        self.calls.append(("item_get", access_token))
        return self._answer(self.item_docs.get(access_token, {
            "item": {"institution_id": "ins_000",
                     "institution_name": "Synthetic Bank",
                     "products": ["transactions"], "error": None,
                     "consent_expiration_time": None},
            "status": {"transactions": {
                "last_successful_update": "2026-01-02T00:00:00Z"}}}))

    def item_remove(self, access_token):
        self.calls.append(("item_remove", access_token))
        return self._answer(self.remove_answer)

    def institutions_first(self, country_codes):
        self.calls.append(("keys", tuple(country_codes)))
        return self._answer(self.keys_answer)

    def institution_get_by_id(self, institution_id, country_codes):
        self.calls.append(("institution", institution_id))
        return self._answer({"institution": self.institution})

    def sandbox_public_token_create(self, institution_id, products):
        self.calls.append(("sandbox_item", institution_id, tuple(products)))
        return {"public_token": public_token()}

    # ---- data reads --------------------------------------------------------
    #
    # `data[name]` scripts a read for every Item; `data_of[token][name]`
    # scripts it for the Item of one access token and wins over `data`. A
    # list is consumed in order, its last entry repeating; an exception
    # instance is raised. For the two paged reads, a list of rows is served
    # `page_size` at a time, with the total Plaid states; a callable takes
    # the offset and returns the page. `history` is the ledger's history
    # status, complete unless a test says otherwise.

    page_size = 2

    def _script(self, token, name):
        mine = self.data_of.get(token, {})
        if name in mine:
            return mine[name]
        if name == "history" and name not in self.data:
            return "HISTORICAL_UPDATE_COMPLETE"
        return self.data[name]

    def _scripted(self, token, name):
        script = self._script(token, name)
        if isinstance(script, list):
            value = script.pop(0) if len(script) > 1 else script[0]
        else:
            value = script
        return self._answer(value)

    def _page(self, token, name, rows_key, total_key, offset):
        script = self._script(token, name)
        if callable(script):
            return self._answer(script(offset))
        if isinstance(script, BaseException):
            raise script
        rows = script
        return {rows_key: rows[offset:offset + self.page_size],
                total_key: len(rows), "accounts": [], "securities": []}

    def accounts_get(self, access_token):
        self.calls.append(("accounts", access_token))
        return self._scripted(access_token, "accounts")

    def holdings_get(self, access_token):
        self.calls.append(("holdings", access_token))
        return self._scripted(access_token, "holdings")

    def liabilities_get(self, access_token):
        self.calls.append(("liabilities", access_token))
        return self._scripted(access_token, "liabilities")

    def ledger_history_status(self, access_token):
        self.calls.append(("history", access_token))
        return self._scripted(access_token, "history")

    def investment_transactions_get(self, access_token, *, start, end, offset):
        self.calls.append(("investment_transactions", start, end, offset))
        return self._page(access_token, "investment_transactions",
                          "investment_transactions",
                          "total_investment_transactions", offset)

    def transactions_get(self, access_token, *, start, end, offset):
        self.calls.append(("transactions", start, end, offset))
        return self._page(access_token, "transactions", "transactions",
                          "total_transactions", offset)


@pytest.fixture
def secrets(tmp_path):
    path = tmp_path / "secrets"
    path.mkdir(mode=0o700)
    return path


class Clock:
    """A clock that only moves when the code under test sleeps."""

    def __init__(self):
        self.now = 1_000.0

    def sleep(self, seconds):
        self.now += seconds

    def monotonic(self):
        return self.now

    def time(self):
        return 1_800_000_000 + self.now


@pytest.fixture
def plaid(monkeypatch):
    """`link` and `login` wired to a FakePlaid and a clock that needs no
    waiting. Returns a factory, so a test picks the environment."""
    import link
    import login

    clock = Clock()
    monkeypatch.setattr(link, "_sleep", clock.sleep)
    monkeypatch.setattr(link, "_monotonic", clock.monotonic)
    monkeypatch.setattr(link, "_time", clock.time)
    monkeypatch.delenv("PLAID_ENV_FILE", raising=False)
    monkeypatch.delenv("PLAID_COUNTRY_CODES", raising=False)

    made = {}

    def make(environment: str = "sandbox") -> FakePlaid:
        fake = FakePlaid(environment)
        made[environment] = fake
        return fake

    def make_client(environment, client_id):
        if environment not in made:
            raise AssertionError(
                f"the test scripted no {environment} Plaid, and the code "
                f"reached for one")
        return made[environment]

    for module in (link, login):
        monkeypatch.setattr(module, "make_client", make_client)
    make.clock = clock
    return make
