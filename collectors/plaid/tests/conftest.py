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

# Plaid's error on an Item whose sign-in has to be renewed.
LOGIN_REQUIRED = {
    "error_type": "ITEM_ERROR", "error_code": "ITEM_LOGIN_REQUIRED",
    "error_message": "the login details of this item have changed"}


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


def linked(n: int = 1) -> dict:
    """A /link/token/get answer whose session made Item `n`."""
    return {"link_sessions": [
        link_session(f"s{n}", public_tokens=[public_token(n=n)])]}


def plaid_error(code: str, status: int = 400,
                message: str = "synthetic error",
                error_type: str = "INVALID_INPUT") -> plaidapi.PlaidError:
    return plaidapi.PlaidError("/synthetic", status, {
        "error_type": error_type, "error_code": code,
        "error_message": message, "request_id": "req-synthetic"})


def an_item(name="bank", environment="sandbox", n=1) -> items.Item:
    return items.Item(
        name=name, environment=environment,
        access_token=access_token(environment, n),
        item_id=f"item-synthetic-{n}", institution_id="ins_000",
        institution_name="Synthetic Bank",
        linked_at="2026-01-02T03:04:05+00:00")


def store(secrets, name="bank", environment="sandbox", n=1) -> None:
    items.save_item(secrets, an_item(name, environment, n))


def will_exchange(fake, n: int = 1) -> None:
    """Script the exchange of Item `n`'s public token."""
    fake.exchanges[public_token(n=n)] = {
        "access_token": access_token(fake.environment, n),
        "item_id": f"item-synthetic-{n}"}


def leave_pending(secrets, clock, name="bank", *, environment="sandbox",
                  age=60, lifetime=3600) -> None:
    """A sign-in a `link` run started `age` seconds ago and left open."""
    items.save_pending(secrets, items.PendingLink(
        name=name, environment=environment, link_token=LINK_TOKEN,
        hosted_link_url=HOSTED_URL, required="transactions",
        created_at="2026-01-02T03:04:05+00:00",
        expires_at=int(clock.time()) - age + lifetime))


class FakePlaid:
    """Answers what a test scripts and records what was asked.

    An answer may be an exception instance, which is raised. `link_docs`
    is consumed one per poll; the last one repeats, as a real session
    that has reached its end keeps answering the same. An entry of
    `item_docs`, and `refresh_answers`, may be a list consumed the same
    way.
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
        self.billed_reads: frozenset = frozenset()
        self.refresh_answers: list = [{"request_id": "req-synthetic"}]

    def _answer(self, value):
        if isinstance(value, BaseException):
            raise value
        return value

    @staticmethod
    def _next(script):
        """The next answer of a script: a list is consumed in order, its
        last entry repeating; anything else is the answer every time."""
        if isinstance(script, list):
            return script.pop(0) if len(script) > 1 else script[0]
        return script

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
        return self._answer(self._next(self.link_docs))

    def exchange_public_token(self, public_token):
        self.calls.append(("exchange", public_token))
        return self._answer(self.exchanges[public_token])

    def item_get(self, access_token):
        self.calls.append(("item_get", access_token))
        return self._answer(self._next(self.item_docs.get(access_token, {
            "item": {"institution_id": "ins_000",
                     "institution_name": "Synthetic Bank",
                     "products": ["transactions"], "error": None,
                     "consent_expiration_time": None},
            "status": {"transactions": {
                "last_successful_update": "2026-01-02T00:00:00Z"}}})))

    def investments_refresh(self, access_token):
        # The real client's gate: Production asks only with the opt-in.
        if (self.environment == "production"
                and "/investments/refresh" not in self.billed_reads):
            raise ValueError("/investments/refresh needs the opt-in in "
                             "plaid.cfg")
        self.calls.append(("refresh", access_token))
        return self._answer(self._next(self.refresh_answers))

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
        return self._answer(self._next(self._script(token, name)))

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
    """A clock that only moves when the code under test sleeps. A wait
    longer than a day fails the test, so a loop that never ends cannot
    hang the suite."""

    def __init__(self):
        self.now = 1_000.0

    def sleep(self, seconds):
        self.now += seconds
        if self.now > 1_000.0 + 24 * 3600:
            raise AssertionError("the code under test waited over a day")

    def monotonic(self):
        return self.now

    def time(self):
        return 1_800_000_000 + self.now


def fake_plaids(monkeypatch, *modules):
    """A factory of FakePlaids, one per environment, that `make_client` in
    each of `modules` hands out. Code that reaches for an environment the
    test scripted no Plaid for fails the test."""
    made = {}

    def make(environment: str = "sandbox") -> FakePlaid:
        made[environment] = FakePlaid(environment)
        return made[environment]

    def make_client(environment, client_id):
        if environment not in made:
            raise AssertionError(
                f"the test scripted no {environment} Plaid, and the code "
                f"reached for one")
        return made[environment]

    for module in modules:
        monkeypatch.setattr(module, "make_client", make_client)
    return make


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
    make = fake_plaids(monkeypatch, link, login)
    make.clock = clock
    return make
