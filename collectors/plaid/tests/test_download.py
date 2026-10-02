"""Tests for the download verb: one run per Item, only the products the
Item was linked with, every page of the two ledgers, and a manifest that
says what each product held. Plaid is the scripted FakePlaid, or the real
client over a scripted transport; time moves only when the code sleeps."""
from __future__ import annotations

import json
from datetime import date

import pytest
from conftest import Clock, FakePlaid, access_token, plaid_error, store

import download
import plaidapi
from collectorkit import bronze, cli

TODAY = date(2026, 1, 31)


def tx(n: int, prefix: str = "tx") -> dict:
    return {"transaction_id": f"{prefix}-{n:03d}", "amount": 1.25 * n,
            "date": "2026-01-02"}


def itx(n: int) -> dict:
    return {"investment_transaction_id": f"itx-{n:03d}", "amount": 2.5 * n,
            "date": "2026-01-02"}


def script(fake: FakePlaid, n: int = 1, products=("investments",
                                                   "liabilities",
                                                   "transactions")):
    """A healthy Item with every product, in the shape Plaid answers."""
    fake.item_docs[access_token(fake.environment, n)] = {
        "item": {"item_id": f"item-synthetic-{n}", "error": None,
                 "products": list(products), "institution_id": "ins_000"},
        "status": {"investments": {
            "last_successful_update": "2026-01-31T05:00:00Z"}}}
    fake.data.update({
        "accounts": {"accounts": [{"account_id": "acc-1"},
                                  {"account_id": "acc-2"}]},
        "holdings": {"holdings": [{"security_id": "sec-1"}] * 3,
                     "securities": [{"security_id": "sec-1"}]},
        "liabilities": {"liabilities": {"credit": [{}], "mortgage": [{}],
                                        "student": None}},
        "transactions": [tx(n) for n in range(5)],
        "investment_transactions": [itx(n) for n in range(3)],
    })


@pytest.fixture
def dl(monkeypatch, tmp_path, caplog):
    """download wired to fakes: returns a factory for the scripted Plaid,
    with `.bronze`, `.secrets` and `.clock` on it."""
    caplog.set_level("INFO")
    clock = Clock()
    monkeypatch.setattr(download, "_sleep", clock.sleep)
    monkeypatch.setattr(download, "_monotonic", clock.monotonic)
    monkeypatch.setattr(cli, "_today_utc", lambda: TODAY)
    monkeypatch.delenv("PLAID_ENV_FILE", raising=False)
    made = {}

    def make(environment="sandbox") -> FakePlaid:
        made[environment] = FakePlaid(environment)
        return made[environment]

    def make_client(environment, client_id):
        if environment not in made:
            raise AssertionError(f"no {environment} Plaid was scripted")
        return made[environment]

    monkeypatch.setattr(download, "make_client", make_client)
    make.bronze = tmp_path / "bronze"
    make.secrets = tmp_path / "secrets"
    make.secrets.mkdir(mode=0o700)
    make.clock = clock
    return make


def run(dl, *argv):
    return download.main(["--bronze-dir", str(dl.bronze), "--secrets-dir",
                          str(dl.secrets), *argv])


def the_run(dl, name="bank"):
    (run_dir,) = sorted((dl.bronze / name).iterdir())
    return run_dir, json.loads((run_dir / "run.json").read_text())


def files_of(run_dir) -> list[str]:
    return sorted(p.name for p in run_dir.iterdir())


# ---- a run ------------------------------------------------------------------------

def test_a_run_writes_every_linked_product(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)

    assert run(dl, "--sandbox") == 0

    run_dir, manifest = the_run(dl)
    assert manifest["status"] == "complete"
    assert manifest["item"] == "bank" and manifest["environment"] == "sandbox"
    assert manifest["item_id"] == "item-synthetic-1"
    assert manifest["api_version"] == plaidapi.API_VERSION
    assert (manifest["since"], manifest["until"]) == ("2025-11-02",
                                                      "2026-01-31")
    products = manifest["products"]
    assert products["accounts"] == {"status": "fetched", "rows": 2,
                                    "files": ["accounts.json"]}
    assert products["holdings"] == {"status": "fetched", "rows": 3,
                                    "files": ["holdings.json"]}
    assert products["liabilities"] == {"status": "fetched", "rows": 2,
                                       "files": ["liabilities.json"]}
    assert products["transactions"] == {
        "status": "fetched", "rows": 5, "since": "2025-11-02",
        "until": "2026-01-31", "history": "HISTORICAL_UPDATE_COMPLETE",
        "files": ["transactions-0001.json", "transactions-0002.json",
                  "transactions-0003.json"]}
    assert products["investment_transactions"] == {
        "status": "fetched", "rows": 3, "since": "2025-11-02",
        "until": "2026-01-31", "files": [
            "investment_transactions-0001.json",
            "investment_transactions-0002.json"]}
    assert files_of(run_dir) == sorted(
        ["run.json", "item.json"] + [f for e in products.values()
                                     for f in e["files"]])


def test_every_file_is_what_plaid_answered(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    assert run(dl, "--sandbox") == 0
    run_dir, _ = the_run(dl)

    def saved(name):
        return json.loads((run_dir / name).read_text())

    assert saved("item.json") == fake.item_docs[access_token()]
    for name in ("accounts", "holdings", "liabilities"):
        assert saved(f"{name}.json") == fake.data[name]
    ledgers = {"transactions": ("total_transactions", 3),
               "investment_transactions": ("total_investment_transactions",
                                           2)}
    for name, (total_key, pages) in ledgers.items():
        rows = fake.data[name]
        for n in range(pages):
            assert saved(f"{name}-{n + 1:04d}.json") == {
                name: rows[2 * n:2 * n + 2], total_key: len(rows),
                "accounts": [], "securities": []}


def test_bronze_is_owner_only_and_holds_no_token(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    assert run(dl, "--sandbox") == 0
    run_dir, _ = the_run(dl)
    assert run_dir.stat().st_mode & 0o777 == 0o700
    for path in run_dir.iterdir():
        assert path.stat().st_mode & 0o777 == 0o600, path.name
        assert access_token() not in path.read_text()


def test_the_ledgers_are_read_over_the_window_page_by_page(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    assert run(dl, "--sandbox", "--lookback", "2025-01-01") == 0
    since, until = date(2025, 1, 1), TODAY
    assert fake.called("transactions") == [
        ("transactions", since, until, offset) for offset in (0, 2, 4)]
    assert fake.called("investment_transactions") == [
        ("investment_transactions", since, until, offset)
        for offset in (0, 2)]


def test_a_window_of_one_day_is_asked_as_it_is(dl):
    # Plaid takes a window whose start is its end, and returns that day.
    fake = dl()
    store(dl.secrets)
    script(fake)
    assert run(dl, "--sandbox", "--lookback", TODAY.isoformat()) == 0
    (call, *_) = fake.called("transactions")
    assert call[1] == TODAY and call[2] == TODAY
    _, manifest = the_run(dl)
    assert manifest["since"] == manifest["until"] == TODAY.isoformat()


def test_only_the_linked_products_are_read(dl):
    # Asking for a product the Item lacks would add it to the Item.
    fake = dl()
    store(dl.secrets)
    script(fake, products=["transactions"])

    assert run(dl, "--sandbox") == 0

    _, manifest = the_run(dl)
    for product in ("holdings", "investment_transactions", "liabilities"):
        assert manifest["products"][product] == {"status": "not_linked"}
        assert fake.called(product) == []
    assert manifest["products"]["transactions"]["status"] == "fetched"


def test_an_investments_only_item_reads_no_bank_ledger(dl):
    fake = dl()
    store(dl.secrets)
    script(fake, products=["investments"])

    assert run(dl, "--sandbox") == 0

    run_dir, manifest = the_run(dl)
    products = manifest["products"]
    assert products["transactions"] == {"status": "not_linked"}
    assert products["liabilities"] == {"status": "not_linked"}
    assert fake.called("transactions") == fake.called("history") == []
    assert products["holdings"]["status"] == "fetched"
    entry = products["investment_transactions"]
    assert entry["status"] == "fetched" and "history" not in entry
    assert (entry["since"], entry["until"]) == ("2025-11-02", "2026-01-31")
    assert not list(run_dir.glob("transactions-*"))


@pytest.mark.parametrize("product,code", [
    ("holdings", "NO_INVESTMENT_ACCOUNTS"),
    ("liabilities", "NO_LIABILITY_ACCOUNTS"),
])
def test_a_product_the_item_has_no_account_for_is_absent(dl, caplog,
                                                         product, code):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data[product] = plaid_error(code, error_type="ITEM_ERROR")

    assert run(dl, "--sandbox") == 0

    run_dir, manifest = the_run(dl)
    assert manifest["products"][product]["status"] == "absent"
    assert manifest["products"][product]["error_code"] == code
    assert not (run_dir / f"{product}.json").exists()
    assert f"{product}: none ({code})" in caplog.text


def test_a_product_plaid_does_not_support_for_the_item_fails(dl):
    # Only products the Item carries are asked, so this is not "none".
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["liabilities"] = plaid_error("PRODUCTS_NOT_SUPPORTED",
                                           error_type="ITEM_ERROR")
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    assert manifest["products"]["liabilities"]["status"] == "failed"


# ---- the ledger's history ------------------------------------------------------------

def test_the_history_status_is_asked_before_the_ledger(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    assert run(dl, "--sandbox") == 0
    names = [c[0] for c in fake.calls]
    assert names.index("history") < names.index("transactions")
    assert fake.called("history") == [("history", access_token())]


@pytest.mark.parametrize("history", [
    "INITIAL_UPDATE_COMPLETE", "NOT_READY",
    "TRANSACTIONS_UPDATE_STATUS_UNKNOWN", None])
def test_a_ledger_with_part_of_its_history_is_partial(dl, caplog, history):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["history"] = history

    assert run(dl, "--sandbox", "--lookback", "all") == 1

    run_dir, manifest = the_run(dl)
    assert manifest["status"] == "complete"
    entry = manifest["products"]["transactions"]
    assert entry["status"] == "partial"
    assert entry["history"] == history and entry["rows"] == 5
    assert len(list(run_dir.glob("transactions-*.json"))) == 3
    assert ("Once it holds the rest, `download --item bank --lookback all "
            "--sandbox` reads it.") in caplog.text


def test_a_history_status_plaid_refuses_fails_the_ledger(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["history"] = plaid_error("INVALID_FIELD")
    assert run(dl, "--sandbox") == 1
    run_dir, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["status"] == "failed"
    assert fake.called("transactions") == []
    assert not list(run_dir.glob("transactions-*"))


# ---- answers that pass ---------------------------------------------------------------

def test_a_product_not_ready_is_waited_for(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    rows = fake.data["transactions"]
    pages = [plaid_error("PRODUCT_NOT_READY")] * 3

    def page(offset):
        if pages:
            raise pages.pop()
        return {"transactions": rows[offset:offset + 2],
                "total_transactions": len(rows)}

    fake.data["transactions"] = page
    start = dl.clock.now

    assert run(dl, "--sandbox") == 0

    _, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["status"] == "fetched"
    assert dl.clock.now - start == 3 * download.NOT_READY_POLL


def test_a_product_never_ready_is_recorded_and_fails_the_run(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["transactions"] = plaid_error("PRODUCT_NOT_READY")
    start = dl.clock.now

    assert run(dl, "--sandbox") == 1

    waited = dl.clock.now - start
    assert download.NOT_READY_WAIT <= waited
    assert waited <= download.NOT_READY_WAIT + download.NOT_READY_POLL
    run_dir, manifest = the_run(dl)
    assert manifest["status"] == "complete"
    assert manifest["products"]["transactions"]["status"] == "not_ready"
    assert manifest["products"]["liabilities"]["status"] == "fetched"
    assert not list(run_dir.glob("transactions-*"))
    assert ("Once it is ready, `download --item bank --sandbox` reads it."
            in caplog.text)


def test_a_passing_fault_is_asked_again(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["holdings"] = [plaid_error("INTERNAL_SERVER_ERROR", 500),
                             plaid_error("RATE_LIMIT_EXCEEDED", 429),
                             fake.data["holdings"]]
    assert run(dl, "--sandbox") == 0
    _, manifest = the_run(dl)
    assert manifest["products"]["holdings"]["status"] == "fetched"
    assert len(fake.called("holdings")) == 3


def test_a_fault_that_does_not_pass_fails_the_product_only(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["liabilities"] = plaid_error("INTERNAL_SERVER_ERROR", 500,
                                           "synthetic outage")

    assert run(dl, "--sandbox") == 1

    run_dir, manifest = the_run(dl)
    assert manifest["status"] == "complete"
    assert manifest["products"]["liabilities"] == {
        "status": "failed", "error_code": "INTERNAL_SERVER_ERROR",
        "error_message": "synthetic outage"}
    assert len(fake.called("liabilities")) == 1 + len(download.TRANSIENT_WAITS)
    assert manifest["products"]["holdings"]["status"] == "fetched"
    assert not (run_dir / "liabilities.json").exists()
    assert "liabilities: not read: synthetic outage" in caplog.text


def test_no_answer_fails_the_product_only(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["holdings"] = plaidapi.TransportError("TimeoutError: timed out")
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    assert manifest["products"]["holdings"] == {
        "status": "failed", "error": "TimeoutError: timed out"}


def test_a_request_plaid_refuses_is_not_asked_again(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["holdings"] = plaid_error("INVALID_FIELD")
    assert run(dl, "--sandbox") == 1
    assert len(fake.called("holdings")) == 1


def test_a_ledger_that_fails_part_way_leaves_no_page(dl):
    # Pages are written only once all of them are in, so a product that
    # failed never reads as a shorter ledger.
    fake = dl()
    store(dl.secrets)
    script(fake)
    rows = fake.data["transactions"]

    def page(offset):
        if offset:
            raise plaid_error("INVALID_FIELD")
        return {"transactions": rows[:2], "total_transactions": len(rows)}

    fake.data["transactions"] = page
    assert run(dl, "--sandbox") == 1
    run_dir, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["status"] == "failed"
    assert not list(run_dir.glob("transactions-*"))


# ---- a ledger that moves while it is read ----------------------------------------

def test_a_ledger_whose_total_changes_mid_read_is_read_again(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    totals = [5, 6, 6, 6, 6]

    def page(offset):
        total = totals.pop(0) if len(totals) > 1 else totals[0]
        rows = [tx(n) for n in range(total)]
        return {"transactions": rows[offset:offset + 2],
                "total_transactions": total}

    fake.data["transactions"] = page

    assert run(dl, "--sandbox") == 0

    _, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["rows"] == 6
    offsets = [c[3] for c in fake.called("transactions")]
    assert offsets == [0, 2, 0, 2, 4]
    assert "changed while it was read; reading it again" in caplog.text


def test_a_row_that_posts_mid_read_is_caught_by_its_id(dl):
    # A pending row leaves and its posted twin arrives on top: the total
    # stays the same and every row below shifts down by one.
    fake = dl()
    store(dl.secrets)
    script(fake)
    before = [tx(n) for n in range(6)]
    after = [tx(4, "posted")] + [r for r in before if r != before[4]]
    served = {"pages": 0}

    def page(offset):
        rows = before if served["pages"] == 0 else after
        served["pages"] += 1
        return {"transactions": rows[offset:offset + 2],
                "total_transactions": 6}

    fake.data["transactions"] = page

    assert run(dl, "--sandbox") == 0

    run_dir, manifest = the_run(dl)
    ids = [r["transaction_id"]
           for name in manifest["products"]["transactions"]["files"]
           for r in json.loads((run_dir / name).read_text())["transactions"]]
    assert ids == [r["transaction_id"] for r in after]


def test_pages_that_add_up_to_more_than_the_total_are_read_again(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    answers = [{"transactions": [tx(0), tx(1)], "total_transactions": 1},
               {"transactions": [tx(0)], "total_transactions": 1}]
    fake.data["transactions"] = lambda offset: answers.pop(0)
    assert run(dl, "--sandbox") == 0
    _, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["rows"] == 1


def test_a_ledger_that_never_settles_fails(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    counter = {"total": 4}

    def page(offset):
        counter["total"] += 1
        return {"transactions": [tx(0), tx(1)],
                "total_transactions": counter["total"]}

    fake.data["transactions"] = page
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    entry = manifest["products"]["transactions"]
    assert entry["status"] == "failed"
    assert "changed while it was read" in entry["error"]
    assert len(fake.called("transactions")) == 2 * download.PAGING_ATTEMPTS


def test_a_page_short_of_its_total_is_read_again(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    answers = [{"transactions": [tx(0), tx(1)], "total_transactions": 3},
               {"transactions": [], "total_transactions": 3},
               {"transactions": [tx(0), tx(1)], "total_transactions": 3},
               {"transactions": [tx(2)], "total_transactions": 3}]
    fake.data["transactions"] = lambda offset: answers.pop(0)
    assert run(dl, "--sandbox") == 0
    _, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["rows"] == 3


def test_an_empty_ledger_is_one_empty_page(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["transactions"] = []
    assert run(dl, "--sandbox") == 0
    _, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["rows"] == 0
    assert manifest["products"]["transactions"]["files"] == [
        "transactions-0001.json"]


# ---- an Item that cannot be read ------------------------------------------------------

def test_an_item_that_needs_a_new_sign_in_fails_its_run(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.item_docs[access_token()]["item"]["error"] = {
        "error_type": "ITEM_ERROR", "error_code": "ITEM_LOGIN_REQUIRED",
        "error_message": "the login details of this item have changed"}

    assert run(dl, "--sandbox") == 1

    run_dir, manifest = the_run(dl)
    assert manifest["status"] == "failed"
    assert "ITEM_LOGIN_REQUIRED" in manifest["reason"]
    assert ("`login --item bank --sandbox` renews the sign-in"
            in manifest["reason"])
    assert files_of(run_dir) == ["run.json"]
    assert fake.called("accounts") == []
    assert "`login --item bank --sandbox` renews the sign-in" in caplog.text


def test_another_error_on_the_item_fails_its_run_with_plaids_text(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.item_docs[access_token()]["item"]["error"] = {
        "error_type": "INSTITUTION_ERROR",
        "error_code": "INSTITUTION_NO_LONGER_SUPPORTED",
        "error_message": "synthetic institution text"}
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    assert manifest["status"] == "failed"
    assert "synthetic institution text" in manifest["reason"]
    assert "login --item" not in manifest["reason"]


def test_an_item_plaid_no_longer_has_says_so(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.item_docs[access_token()] = plaid_error(
        "ITEM_NOT_FOUND", message="the item was removed",
        error_type="ITEM_ERROR")
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    reason = manifest["reason"]
    assert "Plaid no longer has this Item" in reason
    assert "`login --item NEW-NAME --sandbox`" in reason
    assert "while plaid-token-bank.json is in the secrets dir" in reason
    assert "renews" not in reason
    assert "Plaid no longer has this Item" in caplog.text


def test_a_token_plaid_does_not_accept_points_at_the_keys(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.item_docs[access_token()] = plaid_error("INVALID_ACCESS_TOKEN")
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    reason = manifest["reason"]
    assert "PLAID_CLIENT_ID and PLAID_SANDBOX_SECRET" in reason
    assert "plaid-token-bank.json must match its backup" in reason
    assert "no longer has" not in reason and "renews" not in reason
    assert "A new link does not repair this." in caplog.text


def test_accounts_that_cannot_be_read_fail_the_item(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["accounts"] = plaidapi.TransportError("URLError: no route")
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    assert manifest["status"] == "failed"
    assert "no answer from Plaid" in manifest["reason"]
    assert fake.called("holdings") == []


def test_one_failing_item_does_not_stop_the_others(dl):
    fake = dl()
    store(dl.secrets, "a-bank", n=1)
    store(dl.secrets, "b-bank", n=2)
    script(fake, n=1)
    script(fake, n=2)
    fake.item_docs[access_token(n=1)] = plaid_error("ITEM_NOT_FOUND")

    assert run(dl, "--sandbox") == 1

    assert the_run(dl, "a-bank")[1]["status"] == "failed"
    assert the_run(dl, "b-bank")[1]["status"] == "complete"


def test_each_item_gets_its_own_answers(dl):
    fake = dl()
    store(dl.secrets, "a-bank", n=1)
    store(dl.secrets, "b-bank", n=2)
    script(fake, n=1)
    script(fake, n=2)
    fake.data_of[access_token(n=2)] = {
        "accounts": {"accounts": [{"account_id": "acc-b"}]},
        "transactions": [tx(9, "b")]}

    assert run(dl, "--sandbox") == 0

    a_dir, a = the_run(dl, "a-bank")
    b_dir, b = the_run(dl, "b-bank")
    assert (a["products"]["accounts"]["rows"],
            b["products"]["accounts"]["rows"]) == (2, 1)
    page = json.loads((b_dir / "transactions-0001.json").read_text())
    assert [r["transaction_id"] for r in page["transactions"]] == ["b-009"]
    assert a["products"]["transactions"]["rows"] == 5


# ---- what stops a run -------------------------------------------------------------------

def test_a_stopped_download_exits_130_and_leaves_its_run_in_progress(
        dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.data["liabilities"] = KeyboardInterrupt()

    assert run(dl, "--sandbox") == 130

    _, manifest = the_run(dl)
    assert manifest["status"] == "in-progress"
    assert "stopped" in caplog.text


def test_a_write_error_fails_that_item_only(dl, monkeypatch, caplog):
    fake = dl()
    store(dl.secrets, "a-bank", n=1)
    store(dl.secrets, "b-bank", n=2)
    script(fake, n=1)
    script(fake, n=2)
    write = bronze.atomic_write_json

    def full_disk(path, doc):
        if path.name == "accounts.json" and "a-bank" in str(path):
            raise OSError(28, "No space left on device")
        write(path, doc)

    monkeypatch.setattr(bronze, "atomic_write_json", full_disk)

    assert run(dl, "--sandbox") == 1

    assert the_run(dl, "a-bank")[1]["status"] == "in-progress"
    assert the_run(dl, "b-bank")[1]["status"] == "complete"
    assert "a-bank: cannot write the run" in caplog.text


def test_a_run_whose_files_vanish_mid_way_is_not_complete(dl, monkeypatch,
                                                         caplog):
    # A prune with no age guard removes a run that is still under way.
    fake = dl()
    store(dl.secrets)
    script(fake)
    read = fake.liabilities_get

    def pruned_mid_way(token):
        (run_dir,) = (dl.bronze / "bank").iterdir()
        (run_dir / "accounts.json").unlink()
        return read(token)

    monkeypatch.setattr(fake, "liabilities_get", pruned_mid_way)
    assert run(dl, "--sandbox") == 1
    _, manifest = the_run(dl)
    assert manifest["status"] == "in-progress"
    assert "lost accounts.json while it was written" in caplog.text


# ---- whose tree ----------------------------------------------------------------------

def foreign_run(tree, slug="20250101T000000Z", **manifest):
    run_dir = tree / slug
    run_dir.mkdir(parents=True)
    if manifest:
        (run_dir / "run.json").write_text(json.dumps(manifest))
    else:
        (run_dir / "export.csv").write_text("not plaid's\n")
    return run_dir


@pytest.mark.parametrize("manifest", [
    {"item": "bank", "item_id": "item-synthetic-7",
     "environment": "sandbox", "status": "complete"},
    {"item": "bank", "item_id": "item-synthetic-1",
     "environment": "production", "status": "complete"},
    {"status": "complete"},
    {},
], ids=["another-item", "another-environment", "another-collector",
        "no-manifest"])
def test_a_tree_holding_other_runs_is_refused(dl, caplog, manifest):
    fake = dl()
    store(dl.secrets)
    script(fake)
    other = foreign_run(dl.bronze / "bank", **manifest)

    assert run(dl, "--sandbox") == 1

    assert files_of(dl.bronze / "bank") == [other.name]
    assert fake.calls == []
    assert "holds runs that are not this Item's" in caplog.text


def test_a_run_that_stopped_before_its_first_write_is_no_stranger(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    stopped = dl.bronze / "bank" / "20250101T000000Z"
    stopped.mkdir(parents=True)
    (stopped / "run.json.tmp").write_text("{")
    assert run(dl, "--sandbox") == 0


def test_a_tree_of_earlier_runs_of_the_item_is_written_to(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    foreign_run(dl.bronze / "bank", item="bank", item_id="item-synthetic-1",
                environment="sandbox", status="failed")
    assert run(dl, "--sandbox") == 0
    assert len(list((dl.bronze / "bank").iterdir())) == 2


# ---- the dry run --------------------------------------------------------------------

def test_a_dry_run_reads_two_free_answers_and_writes_nothing(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)

    assert run(dl, "--sandbox", "--dry-run") == 0

    assert not dl.bronze.exists() or not any(dl.bronze.rglob("*"))
    assert {c[0] for c in fake.calls} == {"item_get", "accounts"}
    assert ("bank: 2 accounts; a run would read accounts, holdings, "
            "investment transactions, transactions, liabilities; ledgers "
            "from 2025-11-02 to 2026-01-31") in caplog.text


def test_a_dry_run_reports_an_item_that_cannot_be_read(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    fake.item_docs[access_token()] = plaid_error("ITEM_NOT_FOUND")
    assert run(dl, "--sandbox", "--dry-run") == 1
    assert not dl.bronze.exists() or not any(dl.bronze.rglob("*"))


def test_a_dry_run_reports_a_tree_a_run_would_refuse(dl, caplog):
    fake = dl()
    store(dl.secrets)
    script(fake)
    foreign_run(dl.bronze / "bank")
    assert run(dl, "--sandbox", "--dry-run") == 1
    assert "holds runs that are not this Item's" in caplog.text


# ---- which Items -------------------------------------------------------------------

def test_every_item_of_the_environment_is_read(dl):
    fake = dl()
    store(dl.secrets, "bank", n=1)
    store(dl.secrets, "broker", n=2)
    store(dl.secrets, "live-bank", environment="production", n=3)
    script(fake, n=1)
    script(fake, n=2)

    assert run(dl, "--sandbox") == 0

    assert sorted(p.name for p in dl.bronze.iterdir()) == ["bank", "broker"]


def test_named_items_only(dl):
    fake = dl()
    store(dl.secrets, "bank", n=1)
    store(dl.secrets, "broker", n=2)
    script(fake, n=2)
    assert run(dl, "--sandbox", "--item", "broker", "--item", "broker") == 0
    assert sorted(p.name for p in dl.bronze.iterdir()) == ["broker"]
    assert len(fake.called("item_get")) == 1


def test_an_unknown_item_is_refused(dl):
    dl()
    store(dl.secrets)
    with pytest.raises(SystemExit, match="no Item is named 'absent'"):
        run(dl, "--sandbox", "--item", "absent")


@pytest.mark.parametrize("stored,argv,word", [
    ("sandbox", [], "pass"), ("production", ["--sandbox"], "drop")])
def test_an_item_of_the_other_environment_is_refused(dl, stored, argv, word):
    dl("sandbox")
    dl("production")
    store(dl.secrets, environment=stored)
    with pytest.raises(SystemExit, match=f"{stored} environment; {word}"):
        run(dl, "--item", "bank", *argv)


@pytest.mark.parametrize("environment,argv,hint", [
    ("sandbox", ["--sandbox"], "`login --item NAME --sandbox` links one"),
    ("production", [], "`login --item NAME` links one"),
])
def test_nothing_linked_fails_and_names_the_environments_command(
        dl, caplog, environment, argv, hint):
    dl(environment)
    assert run(dl, *argv) == 1
    assert f"no {environment} Item is linked; {hint}" in caplog.text


def test_an_unknown_sandbox_item_suggests_a_sandbox_link(dl):
    dl()
    with pytest.raises(SystemExit, match="`login --item absent --sandbox`"):
        run(dl, "--sandbox", "--item", "absent")


def test_a_token_file_that_cannot_be_read_fails(dl, caplog):
    dl()
    (dl.secrets / "plaid-token-bank.json").write_text("{")
    assert run(dl, "--sandbox") == 1
    assert "exists but cannot be read" in caplog.text


def test_a_damaged_token_file_fails_the_run_but_not_the_other_items(
        dl, caplog):
    fake = dl()
    store(dl.secrets, "bank")
    script(fake)
    (dl.secrets / "plaid-token-broken.json").write_text("{")
    assert run(dl, "--sandbox") == 1
    assert the_run(dl)[1]["status"] == "complete"
    assert "plaid-token-broken.json exists but cannot be read" in caplog.text
    # Named, the healthy Item runs clean: the damaged file is not read.
    assert run(dl, "--sandbox", "--item", "bank") == 0


@pytest.mark.parametrize("argv", [
    ["--item", "Bad Name"],
    ["--lookback", "next-week"],
    ["--password", "x"],
    ["--dry-run", "--debug"],
])
def test_arguments_that_do_not_apply_are_refused(argv):
    with pytest.raises(SystemExit) as caught:
        download.parse_args(["--bronze-dir", "/nonexistent", *argv])
    assert caught.value.code == 2


# ---- the real client, over a scripted transport ---------------------------------------

class Transport:
    """Answers Plaid's routes from a table and keeps every request. A
    route's answer may be a list, served one entry per request (the last
    one repeating), or an exception, which is raised."""

    def __init__(self, answers):
        self.answers = answers
        self.requests = []

    def __call__(self, url, headers, body, timeout):
        path = "/" + url.split("/", 3)[3]
        self.requests.append((path, headers, json.loads(body), timeout))
        answer = self.answers[path]
        if isinstance(answer, list):
            answer = answer.pop(0) if len(answer) > 1 else answer[0]
        if isinstance(answer, BaseException):
            raise answer
        return 200, json.dumps(answer).encode()

    def bodies(self, path):
        return [(body, timeout) for p, _, body, timeout in self.requests
                if p == path]


def every_route(transport_answers=None):
    """The answers of an Item with every product, by route."""
    answers = {
        "/item/get": {"item": {"products": ["investments", "liabilities",
                                            "transactions"],
                               "error": None}, "status": {}},
        "/accounts/get": {"accounts": [{"account_id": "acc-1"}]},
        "/investments/holdings/get": {"holdings": [{"security_id": "s"}],
                                      "securities": []},
        "/investments/transactions/get": {
            "investment_transactions": [itx(1)],
            "total_investment_transactions": 1},
        "/transactions/sync": {
            "transactions_update_status": "HISTORICAL_UPDATE_COMPLETE",
            "added": [tx(1)], "modified": [], "removed": [],
            "has_more": True, "next_cursor": "synthetic-cursor"},
        "/transactions/get": [
            {"transactions": [tx(1), tx(2)], "total_transactions": 3},
            {"transactions": [tx(3)], "total_transactions": 3}],
        "/liabilities/get": {"liabilities": {"credit": [{}]}},
    }
    answers.update(transport_answers or {})
    return answers


def real_client(monkeypatch, answers):
    transport = Transport(answers)
    client = plaidapi.Client("synthetic-id", "synthetic-secret", "sandbox",
                             transport=transport, sleep=lambda s: None)
    monkeypatch.setattr(download, "make_client", lambda e, c: client)
    return client, transport


def test_every_data_route_is_asked_as_plaid_documents_it(dl, monkeypatch):
    store(dl.secrets)
    _, transport = real_client(monkeypatch, every_route())

    assert run(dl, "--sandbox", "--lookback", "2025-01-01") == 0

    token = {"access_token": access_token()}
    assert transport.bodies("/investments/holdings/get") == [
        (token, 60.0)]
    assert transport.bodies("/liabilities/get") == [(token, 60.0)]
    assert transport.bodies("/transactions/sync") == [(
        {**token, "count": 1,
         "options": {"personal_finance_category_version": "v2"}}, 60.0)]
    assert transport.bodies("/investments/transactions/get") == [(
        {**token, "start_date": "2025-01-01", "end_date": "2026-01-31",
         "options": {"count": plaidapi.PAGE_SIZE, "offset": 0}},
        plaidapi.SLOW_TIMEOUT)]
    # Two pages of the bank ledger, the second from where the first ended.
    assert [(b["options"]["offset"], b["start_date"], b["end_date"])
            for b, _ in transport.bodies("/transactions/get")] == [
        (0, "2025-01-01", "2026-01-31"), (2, "2025-01-01", "2026-01-31")]
    for body, _ in transport.bodies("/transactions/get"):
        assert body["options"]["include_original_description"] is True
        assert body["options"]["personal_finance_category_version"] == "v2"
    _, manifest = the_run(dl)
    assert manifest["products"]["transactions"]["files"] == [
        "transactions-0001.json", "transactions-0002.json"]
    # No route outside the reads a run makes.
    assert {p for p, *_ in transport.requests} == set(every_route())


def test_a_run_over_the_real_client_traces_with_debug(dl, monkeypatch):
    store(dl.secrets)
    client, _ = real_client(monkeypatch, every_route({
        "/item/get": {"item": {"products": ["transactions"], "error": None},
                      "status": {}},
        "/accounts/get": [plaidapi.TransportError("TimeoutError: synthetic"),
                          {"accounts": [{"account_id": "acc-1"}]}],
    }))

    assert run(dl, "--sandbox", "--debug") == 0

    run_dir, _ = the_run(dl)
    trace = (run_dir / "screenshots" / "http-trace.jsonl").read_text()
    lines = [json.loads(line) for line in trace.splitlines()]
    assert [line["url"].split("/", 3)[3] for line in lines] == [
        "item/get", "accounts/get", "accounts/get", "transactions/sync",
        "transactions/get", "transactions/get"]
    failed, *answered = lines[1:]
    assert failed["error"] == "TimeoutError: synthetic"
    assert "status" not in failed
    assert lines[0]["status"] == 200
    assert all(line["status"] == 200 and line["bytes"] > 0
               for line in answered)
    for secret in ("synthetic-secret", "synthetic-id", access_token()):
        assert secret not in trace
    # The hook belongs to the run; the client is left as it was found.
    assert client.on_exchange is None


def test_without_debug_no_trace_is_written(dl):
    fake = dl()
    store(dl.secrets)
    script(fake)
    assert run(dl, "--sandbox") == 0
    run_dir, _ = the_run(dl)
    assert not (run_dir / "screenshots").exists()
