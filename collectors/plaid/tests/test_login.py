"""Tests for the login verb: settling the sign-ins `link` left open, and
the read-only `--check`. Plaid is the scripted FakePlaid; time moves only
when the code sleeps."""
from __future__ import annotations

import pytest
from conftest import (
    HOSTED_URL,
    LINK_TOKEN,
    access_token,
    link_session,
    plaid_error,
    public_token,
    store,
)

import items
import login
import plaidapi


def run(secrets, *argv):
    return login.main(["--secrets-dir", str(secrets), *argv])


def linked(n: int = 1) -> dict:
    """A /link/token/get answer whose session made Item `n`."""
    return {"link_sessions": [
        link_session(f"s{n}", public_tokens=[public_token(n=n)])]}


def will_exchange(fake, n: int = 1):
    fake.exchanges[public_token(n=n)] = {
        "access_token": access_token("sandbox", n),
        "item_id": f"item-synthetic-{n}"}


def leave_open(secrets, clock, name="bank", *, environment="sandbox",
               age=60, lifetime=3600):
    """A sign-in a `link` run started `age` seconds ago and left open."""
    items.save_pending(secrets, items.PendingLink(
        name=name, environment=environment, link_token=LINK_TOKEN,
        hosted_link_url=HOSTED_URL, required="transactions",
        created_at="2026-01-02T03:04:05+00:00",
        expires_at=int(clock.time()) - age + lifetime))


# ---- settling the sign-ins link left open --------------------------------------------

def test_with_nothing_left_open_login_asks_plaid_nothing(
        plaid, secrets, capsys):
    fake = plaid()
    store(secrets)
    assert run(secrets, "--sandbox") == 0
    assert "No sandbox sign-in is left open." in capsys.readouterr().out
    assert fake.calls == []
    assert sorted(p.name for p in secrets.iterdir()) == ["plaid-token-bank.json"]


def test_login_claims_an_item_a_page_left_open_made(plaid, secrets, capsys):
    fake = plaid()
    leave_open(secrets, plaid.clock)
    fake.link_docs = [linked()]
    will_exchange(fake)

    assert run(secrets, "--sandbox") == 0

    assert items.load_item(secrets, "bank").item_id == "item-synthetic-1"
    assert sorted(p.name for p in secrets.iterdir()) == ["plaid-token-bank.json"]
    assert "Linked Synthetic Bank as 'bank'" in capsys.readouterr().out
    # Claimed, never linked anew.
    assert fake.called("new_link") == []


def test_login_removes_a_sign_in_whose_page_has_closed(plaid, secrets, capsys):
    plaid()
    leave_open(secrets, plaid.clock, age=4000)
    assert run(secrets, "--sandbox") == 0
    assert "bank: the sign-in made no Item. Its record is removed." in (
        capsys.readouterr().out)
    assert list(secrets.iterdir()) == []


def test_login_removes_a_sign_in_plaid_no_longer_knows(plaid, secrets):
    fake = plaid()
    leave_open(secrets, plaid.clock)
    fake.link_docs = [plaid_error("INVALID_LINK_TOKEN")]
    assert run(secrets, "--sandbox") == 0
    assert list(secrets.iterdir()) == []


def test_login_leaves_a_page_that_is_still_open(plaid, secrets, capsys):
    plaid()
    leave_open(secrets, plaid.clock)
    assert run(secrets, "--sandbox") == 0
    out = capsys.readouterr().out
    assert "bank: the sign-in page is open until " in out
    assert "claimed by the next `login --sandbox`" in out
    assert [p.name for p in secrets.iterdir()] == ["plaid-link-bank.json"]


def test_login_removes_a_sign_in_beside_an_item_of_its_name(plaid, secrets):
    # The name was renewed or linked since; the old page made nothing.
    plaid()
    store(secrets)
    leave_open(secrets, plaid.clock)
    assert run(secrets, "--sandbox") == 0
    assert sorted(p.name for p in secrets.iterdir()) == ["plaid-token-bank.json"]


def test_login_leaves_a_sign_in_a_link_run_is_waiting_on(
        plaid, secrets, capsys):
    fake = plaid()
    leave_open(secrets, plaid.clock)
    with items.held(secrets, "bank") as mine:
        assert mine
        assert run(secrets, "--sandbox") == 0
    assert "bank: a link run is waiting on this sign-in" in (
        capsys.readouterr().out)
    assert fake.calls == []
    assert (secrets / "plaid-link-bank.json").exists()


def test_a_sign_in_settled_just_before_the_hold_is_left_alone(
        plaid, secrets, monkeypatch):
    # A link run that ended between the listing and the hold settled it.
    fake = plaid()
    leave_open(secrets, plaid.clock)
    real_held = items.held

    def held(secrets_dir, name):
        items.clear_pending(secrets_dir, name)
        return real_held(secrets_dir, name)

    monkeypatch.setattr(items, "held", held)
    assert run(secrets, "--sandbox") == 0
    assert fake.calls == []


def test_login_settles_only_its_own_environment(plaid, secrets, capsys):
    fake = plaid()
    leave_open(secrets, plaid.clock, environment="production")
    assert run(secrets, "--sandbox") == 0
    assert "No sandbox sign-in is left open." in capsys.readouterr().out
    assert fake.calls == []
    assert (secrets / "plaid-link-bank.json").exists()


def test_login_of_one_name_settles_that_one_only(plaid, secrets, capsys):
    plaid()
    leave_open(secrets, plaid.clock, "bank", age=4000)
    leave_open(secrets, plaid.clock, "broker", age=4000)
    assert run(secrets, "--sandbox", "--item", "broker") == 0
    assert [p.name for p in secrets.iterdir()] == ["plaid-link-bank.json"]
    assert run(secrets, "--sandbox", "--item", "absent") == 0
    assert "`link --item absent --sandbox` links or renews it" in (
        capsys.readouterr().out)


def test_a_fault_on_one_sign_in_does_not_stop_the_others(
        plaid, secrets, capsys):
    fake = plaid()
    leave_open(secrets, plaid.clock, "a-bank")
    leave_open(secrets, plaid.clock, "b-bank", age=4000)
    fake.link_docs = [plaidapi.TransportError("URLError: reset"),
                      {"link_sessions": []}]
    assert run(secrets, "--sandbox") == 1
    out = capsys.readouterr().out
    assert "a-bank: Plaid could not say how the sign-in ended" in out
    assert "b-bank: the sign-in made no Item" in out
    assert [p.name for p in secrets.iterdir()] == ["plaid-link-a-bank.json"]


def test_a_sign_in_record_that_cannot_be_read_fails_login(
        plaid, secrets, capsys):
    fake = plaid()
    (secrets / "plaid-link-bank.json").write_text("{")
    assert run(secrets, "--sandbox") == 1
    assert "plaid-link-bank.json exists but cannot be read" in (
        capsys.readouterr().out)
    assert fake.calls == []


# ---- --check ---------------------------------------------------------------------

def test_check_with_nothing_linked_reports_the_keys_and_fails(
        plaid, secrets, capsys):
    fake = plaid()
    assert run(secrets, "--check", "--sandbox") == 1
    out = capsys.readouterr().out
    assert "sandbox: Plaid accepts the app keys" in out
    assert "no sandbox Item is linked" in out
    assert fake.called("keys") == [("keys", ("US",))]


def test_check_reports_rejected_keys_and_asks_nothing_else(
        plaid, secrets, capsys):
    fake = plaid()
    store(secrets)
    fake.keys_answer = plaid_error(
        "INVALID_API_KEYS", message="invalid client_id or secret provided")
    assert run(secrets, "--check", "--sandbox") == 1
    out = capsys.readouterr().out
    assert "sandbox: Plaid rejects the app keys" in out
    assert "INVALID_API_KEYS: invalid client_id or secret provided" in out
    assert fake.called("item_get") == []


def test_check_passes_when_every_item_answers(plaid, secrets, capsys):
    fake = plaid()
    store(secrets, "bank", n=1)
    store(secrets, "broker", n=2)
    fake.item_docs[access_token(n=2)] = {
        "item": {"institution_id": "ins_111", "institution_name": "Synthetic "
                 "Broker", "products": ["investments", "transactions"],
                 "error": None,
                 "consent_expiration_time": "2027-01-02T00:00:00Z"},
        "status": {"investments": {
            "last_successful_update": "2026-01-02T05:00:00Z"}}}

    assert run(secrets, "--check", "--sandbox") == 0

    out = capsys.readouterr().out
    assert "bank: ok" in out and "broker: ok" in out
    assert "Synthetic Broker ins_111" in out
    assert "investments, transactions" in out
    assert "2027-01-02T00:00:00Z" in out
    assert "no date reported" in out            # the Item with no expiry
    assert "updated 2026-01-02T05:00:00Z" in out
    for n in (1, 2):
        assert access_token(n=n) not in out


def test_check_reports_every_item_even_after_a_failing_one(
        plaid, secrets, capsys):
    fake = plaid()
    store(secrets, "a-bank", n=1)
    store(secrets, "b-bank", n=2)
    fake.item_docs[access_token(n=1)] = plaid_error(
        "ITEM_NOT_FOUND", message="the item was removed")
    assert run(secrets, "--check", "--sandbox") == 1
    out = capsys.readouterr().out
    assert "a-bank: Plaid refuses the Item" in out
    assert "ITEM_NOT_FOUND: the item was removed" in out
    assert "b-bank: ok" in out


def test_check_sees_only_its_own_environment(plaid, secrets, capsys):
    fake = plaid("production")
    store(secrets, "test-bank", environment="sandbox", n=1)
    store(secrets, "bank", environment="production", n=2)
    assert run(secrets, "--check") == 0
    out = capsys.readouterr().out
    assert "bank: ok" in out and "test-bank" not in out
    assert fake.called("item_get") == [
        ("item_get", access_token("production", 2))]


def test_check_of_one_named_item(plaid, secrets):
    fake = plaid()
    store(secrets, "bank", n=1)
    store(secrets, "broker", n=2)
    assert run(secrets, "--check", "--sandbox", "--item", "broker") == 0
    assert fake.called("item_get") == [("item_get", access_token(n=2))]
    with pytest.raises(SystemExit, match="no Item is named 'absent'"):
        run(secrets, "--check", "--sandbox", "--item", "absent")


def test_check_of_an_item_of_the_other_environment(plaid, secrets):
    fake = plaid()
    store(secrets, "bank", environment="production")
    with pytest.raises(SystemExit, match="production environment; drop"):
        run(secrets, "--check", "--sandbox", "--item", "bank")
    assert fake.called("item_get") == []


def test_check_of_an_item_with_another_error_names_no_sign_in(
        plaid, secrets, capsys):
    fake = plaid()
    store(secrets)
    fake.item_docs[access_token()] = {"item": {"error": {
        "error_type": "INSTITUTION_ERROR",
        "error_code": "INSTITUTION_NO_LONGER_SUPPORTED",
        "error_message": "synthetic institution text"}}}
    assert run(secrets, "--check", "--sandbox") == 1
    out = capsys.readouterr().out
    assert "bank: Plaid reports an error on the Item" in out
    assert "synthetic institution text" in out
    assert "link --item" not in out


@pytest.mark.parametrize("code,error_type,hint", [
    ("ITEM_NOT_FOUND", "ITEM_ERROR", "update mode cannot renew it"),
    ("INVALID_ACCESS_TOKEN", "INVALID_INPUT",
     "PLAID_CLIENT_ID and PLAID_SANDBOX_SECRET must be the keys"),
])
def test_check_of_an_item_plaid_refuses_says_what_to_do(
        plaid, secrets, capsys, code, error_type, hint):
    fake = plaid()
    store(secrets)
    fake.item_docs[access_token()] = plaid_error(code, error_type=error_type)
    assert run(secrets, "--check", "--sandbox") == 1
    out = capsys.readouterr().out
    assert f"bank: Plaid refuses the Item: /synthetic: {code}" in out
    assert hint in out
    assert "renews" not in out


def test_check_changes_nothing(plaid, secrets):
    fake = plaid()
    store(secrets)
    before = sorted((p.name, p.read_bytes()) for p in secrets.iterdir())
    assert run(secrets, "--check", "--sandbox") == 0
    assert sorted((p.name, p.read_bytes()) for p in secrets.iterdir()) == before
    assert {c[0] for c in fake.calls} == {"keys", "item_get"}


def test_check_fails_on_a_token_file_that_cannot_be_read(
        plaid, secrets, capsys):
    plaid()
    (secrets / "plaid-token-bank.json").write_text("{")
    assert run(secrets, "--check", "--sandbox") == 1
    assert "exists but cannot be read" in capsys.readouterr().out


def test_check_reports_the_other_items_beside_a_damaged_file(
        plaid, secrets, capsys):
    plaid()
    store(secrets, "bank")
    (secrets / "plaid-token-broken.json").write_text("{")
    assert run(secrets, "--check", "--sandbox") == 1
    out = capsys.readouterr().out
    assert "plaid-token-broken.json exists but cannot be read" in out
    assert "bank: ok" in out


def test_check_without_an_answer_fails(plaid, secrets, capsys):
    fake = plaid()
    fake.keys_answer = plaidapi.TransportError("URLError: no route")
    assert run(secrets, "--check", "--sandbox") == 1
    assert "no answer from Plaid" in capsys.readouterr().out


def test_check_lists_the_sign_ins_left_open(plaid, secrets, capsys):
    fake = plaid()
    store(secrets)
    leave_open(secrets, plaid.clock, "broker")
    assert run(secrets, "--check", "--sandbox") == 0
    out = capsys.readouterr().out
    assert "bank: ok" in out
    assert ("broker: a sign-in started 2026-01-02T03:04:05+00:00 is left "
            "open; `login --sandbox` settles it") in out
    assert fake.called("link_get") == []


# ---- the command line --------------------------------------------------------------

@pytest.mark.parametrize("argv", [
    ["--require", "investments"],
    ["--mfa-timeout", "600"],
    ["--sandbox-institution", "ins_000"],
    ["--item", "Bad Name"],
    ["--country-codes", "US"],                  # for --check only
    ["--check", "--country-codes", "USA"],
    ["--password", "x"],
])
def test_arguments_that_do_not_apply_are_refused(argv):
    with pytest.raises(SystemExit) as caught:
        login.parse_args(argv)
    assert caught.value.code == 2


def test_country_codes_fall_back_to_the_environment(monkeypatch):
    monkeypatch.setenv("PLAID_COUNTRY_CODES", "US,CA")
    assert login.parse_args(["--check"]).country_codes == ["US", "CA"]
    assert login.parse_args(
        ["--check", "--country-codes", "ca"]).country_codes == ["CA"]
    monkeypatch.delenv("PLAID_COUNTRY_CODES")
    assert login.parse_args(["--check"]).country_codes == ["US"]
