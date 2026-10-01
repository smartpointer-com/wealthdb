"""Tests for the login verb: making an Item through Hosted Link, renewing
one in update mode, settling a sign-in an earlier run left behind, and the
read-only `--check`. Plaid is the scripted FakePlaid; time moves only when
the code sleeps."""
from __future__ import annotations

import json
import os

import pytest
from conftest import (
    HOSTED_URL,
    LINK_TOKEN,
    access_token,
    link_session,
    plaid_error,
    public_token,
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


def will_exchange(fake, n: int = 1, environment: str = "sandbox"):
    fake.exchanges[public_token(n=n)] = {
        "access_token": access_token(environment, n),
        "item_id": f"item-synthetic-{n}"}


def store(secrets, name="bank", environment="sandbox", n=1) -> items.Item:
    item = items.Item(
        name=name, environment=environment,
        access_token=access_token(environment, n),
        item_id=f"item-synthetic-{n}", institution_id="ins_000",
        institution_name="Synthetic Bank")
    items.save_item(secrets, item)
    return item


def leave_pending(secrets, clock, name="bank", *, age=60, lifetime=3600):
    """A sign-in an earlier run started `age` seconds ago."""
    pending = items.PendingLink(
        name=name, environment="sandbox", link_token=LINK_TOKEN,
        hosted_link_url=HOSTED_URL, required="transactions",
        created_at="2026-01-02T03:04:05+00:00",
        expires_at=int(clock.time()) - age + lifetime)
    items.save_pending(secrets, pending)
    return pending


# ---- a new Item ----------------------------------------------------------------

def test_a_new_link_stores_the_item(plaid, secrets, capsys, caplog):
    fake = plaid()
    fake.link_docs = [{"link_sessions": []},
                      {"link_sessions": [link_session(finished=False)]},
                      linked()]
    will_exchange(fake)

    assert run(secrets, "--item", "bank", "--sandbox") == 0

    path = secrets / "plaid-token-bank.json"
    assert path.stat().st_mode & 0o777 == 0o600
    stored = json.loads(path.read_text())
    assert stored["access_token"] == access_token()
    assert stored["item_id"] == "item-synthetic-1"
    assert stored["environment"] == "sandbox"
    assert stored["institution_id"] == "ins_000"
    assert stored["institution_name"] == "Synthetic Bank"
    assert stored["linked_at"]
    # The sign-in is settled, so its record is gone.
    assert not (secrets / "plaid-link-bank.json").exists()
    # The stored token is proven with one read.
    assert fake.called("item_get") == [("item_get", access_token())]

    out = capsys.readouterr().out + caplog.text
    assert HOSTED_URL in out
    assert "Linked Synthetic Bank as 'bank'" in out
    assert "The rest of the page can be finished or closed" in out
    for secret in (access_token(), public_token(), LINK_TOKEN):
        assert secret not in out


def test_the_session_is_written_down_before_its_url_is_shown(
        plaid, secrets, monkeypatch):
    # From the moment the URL is out, the page can make an Item whether or
    # not this process lives to hear of it.
    fake = plaid()
    fake.link_docs = [linked()]
    will_exchange(fake)
    seen = {}
    real_say = login.say

    def say(text=""):
        if HOSTED_URL in text:
            seen["pending"] = json.loads(
                (secrets / "plaid-link-bank.json").read_text())
        real_say(text)

    monkeypatch.setattr(login, "say", say)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert seen["pending"]["link_token"] == LINK_TOKEN
    assert seen["pending"]["hosted_link_url"] == HOSTED_URL
    assert seen["pending"]["environment"] == "sandbox"


@pytest.mark.parametrize("argv,required", [
    ([], "transactions"),
    (["--require", "investments"], "investments"),
    (["--require", "liabilities"], "liabilities"),
])
def test_the_link_names_the_required_product(plaid, secrets, argv, required):
    fake = plaid()
    fake.link_docs = [linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox", *argv) == 0
    assert fake.called("new_link") == [("new_link", required, ("US",), 3600)]


def test_the_page_lives_as_long_as_the_wait(plaid, secrets):
    fake = plaid()
    fake.link_docs = [linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox", "--mfa-timeout",
               "7200", "--country-codes", "us, ca") == 0
    assert fake.called("new_link") == [
        ("new_link", "transactions", ("US", "CA"), 7200)]


def test_production_is_the_default_environment(plaid, secrets, capsys):
    fake = plaid("production")
    fake.link_docs = [linked()]
    will_exchange(fake, environment="production")
    assert run(secrets, "--item", "bank") == 0
    assert items.load_item(secrets, "bank").environment == "production"
    out = capsys.readouterr().out
    # A production Item is one of ten for good: the count is stated.
    assert "1 production Item(s)" in out and "does not return" in out
    assert "user_good" not in out


def test_the_sandbox_page_names_the_test_login(plaid, secrets, capsys):
    fake = plaid()
    fake.link_docs = [linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    out = capsys.readouterr().out
    assert "user_good / pass_good" in out
    assert "production Item(s)" not in out


def test_a_session_marked_finished_before_its_result_is_waited_on(
        plaid, secrets):
    # Plaid stamps a session finished before its result can be read; with
    # no exit beside it, that is not yet an answer.
    fake = plaid()
    fake.link_docs = [{"link_sessions": [link_session()]}, linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert len(fake.called("link_get")) == 2


def test_a_later_visit_succeeds_after_an_earlier_one_was_closed(
        plaid, secrets):
    # Plaid lists a token's sessions in no fixed order: here the newer
    # visit comes first, and it is the start time that says so.
    fake = plaid()
    closed = link_session("s0", exited=True, minute=4)
    fake.link_docs = [
        {"link_sessions": [link_session("s1", finished=False, minute=9),
                           closed]},
        {"link_sessions": [link_session("s1", minute=9,
                                        public_tokens=[public_token()]),
                           closed]},
    ]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert len(fake.called("link_get")) == 2


def test_an_exit_ends_the_wait_even_beside_a_tab_that_was_just_closed(
        plaid, secrets, capsys):
    # A closed tab never ends its session, so it cannot hold the wait
    # open once a later visit was exited on purpose.
    fake = plaid()
    fake.link_docs = [{"link_sessions": [
        link_session("s1", exited=True, minute=9),
        link_session("s0", finished=False, minute=4)]}]
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    assert "closed before the sign-in finished" in capsys.readouterr().out
    assert len(fake.called("link_get")) == 1


def test_an_item_made_before_the_page_was_cancelled_is_still_claimed(
        plaid, secrets):
    # Plaid makes the Item when the accounts are confirmed. Cancelling
    # the screen after that ends the session in an exit and leaves the
    # Item made: unclaimed, it would hold a slot nothing can reach.
    fake = plaid()
    fake.link_docs = [{"link_sessions": [link_session(
        exited=True, public_tokens=[public_token()])]}]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert items.load_item(secrets, "bank").item_id == "item-synthetic-1"


def test_an_item_is_claimed_while_its_session_is_still_open(plaid, secrets):
    fake = plaid()
    fake.link_docs = [{"link_sessions": [link_session(
        finished=False, public_tokens=[public_token()])]}]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0


def test_the_wait_logs_the_pages_progress_once_per_event(
        plaid, secrets, caplog):
    fake = plaid()
    opened = ("e1", "OPEN", {"view_name": "CONSENT"})
    chosen = ("e2", "SELECT_INSTITUTION", {
        "institution_id": "ins_000", "institution_name": "Synthetic Bank"})
    failed = ("e3", "ERROR", {"error_code": "INVALID_CREDENTIALS"})
    fake.link_docs = [
        {"link_sessions": [link_session(finished=False, events=[opened])]},
        {"link_sessions": [link_session(finished=False,
                                        events=[opened, chosen, failed])]},
        {"link_sessions": [link_session(events=[opened, chosen, failed],
                                        public_tokens=[public_token()])]},
    ]
    will_exchange(fake)
    with caplog.at_level("INFO"):
        assert run(secrets, "--item", "bank", "--sandbox") == 0
    lines = [r.getMessage() for r in caplog.records
             if r.getMessage().startswith("sign-in page:")]
    assert lines == ["sign-in page: OPEN (CONSENT)",
                     "sign-in page: SELECT_INSTITUTION",
                     "sign-in page: ERROR (INVALID_CREDENTIALS)"]


def test_passing_faults_do_not_end_the_wait(plaid, secrets, caplog):
    fake = plaid()
    fake.link_docs = [plaidapi.TransportError("URLError: reset"),
                      plaid_error("INTERNAL_SERVER_ERROR", status=500),
                      plaid_error("RATE_LIMIT_EXCEEDED", status=429),
                      linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert caplog.text.count("still waiting") == 3


# ---- a sign-in that does not make an Item ----------------------------------------

def test_an_exit_ends_the_wait_with_plaids_own_words(plaid, secrets, capsys):
    fake = plaid()
    fake.link_docs = [{"link_sessions": [link_session(exit_error={
        "error_code": "INVALID_CREDENTIALS",
        "error_message": "the provided credentials were not correct",
        "display_message": "The credentials you provided were incorrect."})]}]

    assert run(secrets, "--item", "bank", "--sandbox") == 1

    out = capsys.readouterr().out
    assert "The credentials you provided were incorrect." in out
    assert "INVALID_CREDENTIALS" in out
    assert not (secrets / "plaid-token-bank.json").exists()
    assert fake.called("exchange") == []
    # The page is still good, so the next run reuses it.
    assert (secrets / "plaid-link-bank.json").exists()


def test_a_page_closed_without_an_error_says_so(plaid, secrets, capsys):
    fake = plaid()
    fake.link_docs = [{"link_sessions": [link_session(exited=True)]}]
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    assert "Plaid reports: the page was closed before the sign-in " \
           "finished" in capsys.readouterr().out


def test_a_timeout_keeps_the_session_for_the_next_run(plaid, secrets, capsys):
    plaid()                             # no session ever starts
    start = plaid.clock.now
    assert run(secrets, "--item", "bank", "--sandbox", "--mfa-timeout",
               "600") == 1
    assert plaid.clock.now - start >= 600
    assert "did not finish within 10 minutes" in capsys.readouterr().out
    assert (secrets / "plaid-link-bank.json").exists()
    assert not (secrets / "plaid-token-bank.json").exists()


def test_a_request_plaid_refuses_is_reported_in_its_words(
        plaid, secrets, capsys):
    fake = plaid()
    fake.link_docs = [plaid_error(
        "INVALID_FIELD", message="a use case must be selected")]
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    assert "INVALID_FIELD: a use case must be selected" in (
        capsys.readouterr().out)


def test_ctrl_c_stops_the_wait_and_keeps_the_session(plaid, secrets, capsys):
    fake = plaid()
    fake.link_docs = [KeyboardInterrupt()]
    assert run(secrets, "--item", "bank", "--sandbox") == 130
    assert "picks up where this left off" in capsys.readouterr().out
    assert (secrets / "plaid-link-bank.json").exists()


# ---- settling a sign-in an earlier run left behind -------------------------------

def test_a_sign_in_that_finished_late_is_claimed_without_a_new_link(
        plaid, secrets):
    fake = plaid()
    leave_pending(secrets, plaid.clock)
    fake.link_docs = [linked()]
    will_exchange(fake)

    assert run(secrets, "--item", "bank", "--sandbox") == 0

    assert fake.called("new_link") == []
    assert items.load_item(secrets, "bank").item_id == "item-synthetic-1"
    assert not (secrets / "plaid-link-bank.json").exists()


def test_an_open_page_is_reused_and_its_old_exit_is_not_held_against_it(
        plaid, secrets, capsys):
    fake = plaid()
    leave_pending(secrets, plaid.clock, age=600)
    closed = link_session("s0", exited=True)
    fake.link_docs = [
        {"link_sessions": [closed]},            # read when login starts
        {"link_sessions": [closed]},            # first poll: nothing new
        {"link_sessions": [closed, link_session(
            "s1", public_tokens=[public_token()])]},
    ]
    will_exchange(fake)

    assert run(secrets, "--item", "bank", "--sandbox") == 0

    assert fake.called("new_link") == []
    out = capsys.readouterr().out
    assert HOSTED_URL in out
    # 3600 s of lifetime less the 600 s already gone.
    assert "valid for 50 minutes" in out


def test_a_resumed_wait_does_not_replay_the_earlier_visit(
        plaid, secrets, caplog):
    fake = plaid()
    leave_pending(secrets, plaid.clock, age=600)
    closed = link_session("s0", exited=True, minute=4, events=[
        ("e1", "OPEN", {"view_name": "CONSENT"}), ("e2", "EXIT", {})])
    fake.link_docs = [
        {"link_sessions": [closed]},
        {"link_sessions": [closed, link_session(
            "s1", minute=9, public_tokens=[public_token()],
            events=[("e3", "OPEN", {"view_name": "CONSENT"})])]},
    ]
    will_exchange(fake)
    with caplog.at_level("INFO"):
        assert run(secrets, "--item", "bank", "--sandbox") == 0
    lines = [r.getMessage() for r in caplog.records
             if r.getMessage().startswith("sign-in page:")]
    assert lines == ["sign-in page: OPEN (CONSENT)"]


def test_an_expired_session_with_nothing_to_claim_is_replaced(plaid, secrets):
    fake = plaid()
    leave_pending(secrets, plaid.clock, age=4000, lifetime=3600)
    fake.link_docs = [{"link_sessions": []}, linked()]
    will_exchange(fake)

    assert run(secrets, "--item", "bank", "--sandbox") == 0

    assert len(fake.called("new_link")) == 1
    assert items.load_item(secrets, "bank") is not None


def test_an_expired_session_that_made_an_item_is_still_claimed(plaid, secrets):
    # Plaid serves a finished session's result for hours after its page
    # has expired; the expiry must not get in the way of claiming it.
    fake = plaid()
    leave_pending(secrets, plaid.clock, age=9000, lifetime=3600)
    fake.link_docs = [linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert fake.called("new_link") == []


def test_a_session_plaid_no_longer_knows_is_replaced(plaid, secrets):
    fake = plaid()
    leave_pending(secrets, plaid.clock)
    fake.link_docs = [plaid_error("INVALID_LINK_TOKEN"), linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert len(fake.called("new_link")) == 1


def test_a_passing_fault_does_not_discard_the_saved_session(
        plaid, secrets, capsys):
    fake = plaid()
    leave_pending(secrets, plaid.clock)
    fake.link_docs = [plaid_error("INTERNAL_SERVER_ERROR", status=500)]
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    assert (secrets / "plaid-link-bank.json").exists()
    assert fake.called("new_link") == []


def test_an_item_claimed_by_an_earlier_run_is_not_stored_twice(
        plaid, secrets, capsys):
    # The earlier run died after writing the token and before removing
    # the session's record.
    fake = plaid()
    store(secrets)
    before = (secrets / "plaid-token-bank.json").read_bytes()
    leave_pending(secrets, plaid.clock)
    fake.link_docs = [linked()]
    will_exchange(fake)

    assert run(secrets, "--item", "bank", "--sandbox") == 0

    assert (secrets / "plaid-token-bank.json").read_bytes() == before
    assert [i.name for i in items.list_items(secrets)] == ["bank"]
    assert not (secrets / "plaid-link-bank.json").exists()
    # Settled, not renewed: no update-mode page was opened.
    assert fake.called("update_link") == []


def test_a_public_token_plaid_refuses_ends_that_sign_in(
        plaid, secrets, capsys):
    fake = plaid()
    leave_pending(secrets, plaid.clock)
    fake.link_docs = [linked()]
    fake.exchanges[public_token()] = plaid_error(
        "INVALID_PUBLIC_TOKEN", message="the public token has expired")

    assert run(secrets, "--item", "bank", "--sandbox") == 1

    out = capsys.readouterr().out
    assert "can no longer be claimed" in out
    assert "the public token has expired" in out
    assert "Synthetic Bank" in out
    # Kept, the record would fail the same way on every later run.
    assert not (secrets / "plaid-link-bank.json").exists()
    assert not (secrets / "plaid-token-bank.json").exists()


def test_a_fault_at_the_exchange_keeps_the_session_for_a_retry(
        plaid, secrets):
    fake = plaid()
    fake.link_docs = [linked()]
    fake.exchanges[public_token()] = plaid_error(
        "INTERNAL_SERVER_ERROR", status=500)
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    assert (secrets / "plaid-link-bank.json").exists()


# ---- more than one Item, and tokens that cannot be kept ---------------------------

def test_a_second_item_of_one_sign_in_gets_a_name_of_its_own(
        plaid, secrets, capsys):
    # The hosted page lets a person link again before leaving. Each Item
    # is a slot spent and a token that exists once, so both are kept.
    fake = plaid()
    fake.link_docs = [{"link_sessions": [link_session(
        public_tokens=[public_token(n=1), public_token(n=2)])]}]
    will_exchange(fake, 1)
    will_exchange(fake, 2)

    assert run(secrets, "--item", "bank", "--sandbox") == 0

    assert {i.name: i.item_id for i in items.list_items(secrets)} == {
        "bank": "item-synthetic-1", "bank-2": "item-synthetic-2"}
    assert "Two stored Items are at the same institution" in (
        capsys.readouterr().out)


def test_one_item_reported_twice_is_stored_once(plaid, secrets):
    # The same link shows up under `results` and under the older
    # `on_success`.
    fake = plaid()
    session_doc = link_session(public_tokens=[public_token()])
    session_doc["on_success"] = {"public_token": public_token(),
                                 "metadata": {"institution": None}}
    fake.link_docs = [{"link_sessions": [session_doc]}]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert len(fake.called("exchange")) == 1
    assert [i.name for i in items.list_items(secrets)] == ["bank"]


def test_a_token_that_cannot_be_written_is_revoked(
        plaid, secrets, monkeypatch, capsys):
    fake = plaid()
    fake.link_docs = [linked()]
    will_exchange(fake)

    def no_space(secrets_dir, item):
        raise OSError(28, "No space left on device")

    monkeypatch.setattr(items, "save_item", no_space)

    assert run(secrets, "--item", "bank", "--sandbox") == 1

    assert fake.called("item_remove") == [("item_remove", access_token())]
    out = capsys.readouterr().out
    assert "could not be written" in out and "No space left" in out
    assert "Plaid has revoked it" in out
    assert access_token() not in out


def test_a_failed_revoke_is_reported_too(plaid, secrets, monkeypatch, capsys):
    fake = plaid()
    fake.link_docs = [linked()]
    will_exchange(fake)
    fake.remove_answer = plaid_error("INTERNAL_SERVER_ERROR", status=500)
    monkeypatch.setattr(
        items, "save_item",
        lambda d, i: (_ for _ in ()).throw(OSError(13, "Permission denied")))
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    assert "did not revoke it either" in capsys.readouterr().out


# ---- an Item that exists: update mode ---------------------------------------------

def test_an_existing_item_is_renewed_and_not_linked_again(
        plaid, secrets, capsys):
    fake = plaid()
    store(secrets)
    before = (secrets / "plaid-token-bank.json").read_bytes()
    # A completed renewal reports a public token, as a new link does.
    fake.link_docs = [{"link_sessions": [link_session(finished=False)]},
                      {"link_sessions": [link_session(
                          public_tokens=[public_token()])]}]

    assert run(secrets, "--item", "bank", "--sandbox") == 0

    assert fake.called("update_link") == [
        ("update_link", access_token(), ("US",), 3600)]
    assert fake.called("new_link") == []
    assert fake.called("exchange") == []
    # Update mode keeps the Item and its token.
    assert (secrets / "plaid-token-bank.json").read_bytes() == before
    assert not (secrets / "plaid-link-bank.json").exists()
    out = capsys.readouterr().out
    assert "update mode" in out and "makes no new one" in out
    assert "bank: ok" in out


def test_a_renewal_that_was_closed_reports_failure(plaid, secrets, capsys):
    fake = plaid()
    store(secrets)
    fake.link_docs = [{"link_sessions": [link_session(exited=True)]}]
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    assert "The renewal did not finish" in capsys.readouterr().out
    assert fake.called("item_get") == []


def test_a_renewal_completed_on_a_second_visit_succeeds(plaid, secrets):
    fake = plaid()
    store(secrets)
    fake.link_docs = [{"link_sessions": [
        link_session("s1", minute=9),
        link_session("s0", exited=True, minute=4)]}]
    assert run(secrets, "--item", "bank", "--sandbox") == 0


def test_a_renewed_item_plaid_still_faults_reports_failure(
        plaid, secrets, capsys):
    fake = plaid()
    store(secrets)
    fake.link_docs = [{"link_sessions": [link_session()]}]
    fake.item_docs[access_token()] = {"item": {"error": {
        "error_code": "ITEM_LOGIN_REQUIRED",
        "error_message": "the login details of this item have changed"}}}
    assert run(secrets, "--item", "bank", "--sandbox") == 1
    out = capsys.readouterr().out
    assert "bank: needs a new sign-in" in out
    assert "ITEM_LOGIN_REQUIRED: the login details of this item" in out


@pytest.mark.parametrize("argv", [
    ["--require", "investments"],
    ["--sandbox-institution", "ins_000"],
])
def test_a_new_item_flag_is_refused_for_an_existing_item(
        plaid, secrets, argv):
    fake = plaid()
    store(secrets)
    with pytest.raises(SystemExit, match="exists and keeps the products"):
        run(secrets, "--item", "bank", "--sandbox", *argv)
    assert fake.calls == []


# ---- the two environments never mix ------------------------------------------------

def test_a_sandbox_item_is_refused_without_the_sandbox_flag(plaid, secrets):
    fake = plaid("production")
    store(secrets, environment="sandbox")
    with pytest.raises(SystemExit, match="sandbox environment; pass"):
        run(secrets, "--item", "bank")
    assert fake.calls == []


def test_a_production_item_is_refused_with_the_sandbox_flag(plaid, secrets):
    fake = plaid("sandbox")
    store(secrets, environment="production")
    with pytest.raises(SystemExit, match="production environment; drop"):
        run(secrets, "--item", "bank", "--sandbox")
    assert fake.calls == []


def test_a_sandbox_run_never_reaches_for_production(plaid, secrets):
    # The fixture scripts a Sandbox Plaid only; a production client being
    # asked for would fail the test.
    fake = plaid("sandbox")
    fake.link_docs = [linked()]
    will_exchange(fake)
    assert run(secrets, "--item", "bank", "--sandbox") == 0
    assert run(secrets, "--check", "--sandbox") == 0


# ---- the Sandbox Item made without a browser ---------------------------------------

def test_a_sandbox_item_follows_what_the_institution_offers(plaid, secrets):
    fake = plaid()
    fake.institution = {"institution_id": "ins_000", "name": "Synthetic Bank",
                        "products": ["auth", "transactions", "investments"]}
    will_exchange(fake)

    assert run(secrets, "--item", "bank", "--sandbox-institution",
               "ins_000") == 0

    assert fake.called("sandbox_item") == [
        ("sandbox_item", "ins_000", ("transactions", "investments"))]
    assert fake.called("new_link") == [] and fake.called("link_get") == []
    assert items.load_item(secrets, "bank").institution_name == (
        "Synthetic Bank")


def test_a_sandbox_item_needs_its_required_product(plaid, secrets, capsys):
    fake = plaid()
    fake.institution = {"institution_id": "ins_000", "name": "Synthetic Bank",
                        "products": ["transactions"]}
    assert run(secrets, "--item", "bank", "--sandbox-institution", "ins_000",
               "--require", "investments") == 1
    assert "does not offer investments" in capsys.readouterr().out
    assert fake.called("sandbox_item") == []


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


def test_check_of_one_named_item(plaid, secrets, capsys):
    fake = plaid()
    store(secrets, "bank", n=1)
    store(secrets, "broker", n=2)
    assert run(secrets, "--check", "--sandbox", "--item", "broker") == 0
    assert fake.called("item_get") == [("item_get", access_token(n=2))]
    assert run(secrets, "--check", "--sandbox", "--item", "absent") == 1
    assert "no sandbox Item is named 'absent'" in capsys.readouterr().out


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


def test_check_without_an_answer_fails(plaid, secrets, capsys):
    fake = plaid()
    fake.keys_answer = plaidapi.TransportError("URLError: no route")
    assert run(secrets, "--check", "--sandbox") == 1
    assert "no answer from Plaid" in capsys.readouterr().out


# ---- the command line --------------------------------------------------------------

@pytest.mark.parametrize("argv", [
    [],                                         # neither --item nor --check
    ["--check", "--require", "investments"],
    ["--check", "--mfa-timeout", "600"],
    ["--check", "--sandbox-institution", "ins_000"],
    ["--item", "Bad Name"],
    ["--item", "bank", "--require", "transfer"],
    ["--item", "bank", "--mfa-timeout", "5"],
    ["--item", "bank", "--mfa-timeout", "99999999"],
    ["--item", "bank", "--country-codes", "USA"],
    ["--item", "bank", "--password", "x"],
])
def test_arguments_that_do_not_apply_are_refused(argv, capsys):
    with pytest.raises(SystemExit) as caught:
        login.parse_args(argv)
    assert caught.value.code == 2


def test_the_sandbox_institution_implies_the_sandbox():
    args = login.parse_args(["--item", "bank", "--sandbox-institution",
                             "ins_000"])
    assert args.sandbox is True


def test_country_codes_fall_back_to_the_environment(monkeypatch):
    monkeypatch.setenv("PLAID_COUNTRY_CODES", "US,CA")
    assert login.parse_args(["--check"]).country_codes == ["US", "CA"]
    assert login.parse_args(
        ["--check", "--country-codes", "ca"]).country_codes == ["CA"]
    monkeypatch.delenv("PLAID_COUNTRY_CODES")
    assert login.parse_args(["--check"]).country_codes == ["US"]


# ---- the keys ------------------------------------------------------------------------

@pytest.mark.parametrize("environment,variable", [
    ("production", "PLAID_SECRET"), ("sandbox", "PLAID_SANDBOX_SECRET")])
def test_each_environment_reads_its_own_secret(
        monkeypatch, environment, variable):
    monkeypatch.setenv("PLAID_CLIENT_ID", "synthetic-id")
    monkeypatch.setenv("PLAID_SECRET", "synthetic-production-secret")
    monkeypatch.setenv("PLAID_SANDBOX_SECRET", "synthetic-sandbox-secret")
    client = login.make_client(environment, None)
    assert client.environment == environment
    assert client._secret == f"synthetic-{environment}-secret"

    monkeypatch.delenv(variable)
    with pytest.raises(SystemExit, match=variable):
        login.make_client(environment, None)


def test_a_missing_client_id_is_named(monkeypatch):
    monkeypatch.delenv("PLAID_CLIENT_ID", raising=False)
    monkeypatch.setenv("PLAID_SECRET", "synthetic-secret")
    with pytest.raises(SystemExit, match="PLAID_CLIENT_ID"):
        login.make_client("production", None)
    assert login.make_client("production", "from-flag")._client_id == (
        "from-flag")


def test_the_env_file_is_sourced_as_a_shell_script_and_wins(
        tmp_path, monkeypatch):
    # Quoting and `export` are the shell's to read, so the file goes
    # through bash; its values win over what the process inherited.
    # Set first, so the fixture restores each one after the file has
    # written over it.
    for name in ("PLAID_CLIENT_ID", "PLAID_SANDBOX_SECRET", "PLAID_PART"):
        monkeypatch.setenv(name, "inherited")
    monkeypatch.delenv("PLAID_ENV_FILE", raising=False)
    env = tmp_path / "plaid.env"
    env.write_text("export PLAID_CLIENT_ID='from $file'\n"
                   "PLAID_PART=synthetic\n"
                   "export PLAID_SANDBOX_SECRET=\"${PLAID_PART}-value\"\n")
    login._source_env_file(env)
    assert os.environ["PLAID_CLIENT_ID"] == "from $file"
    assert os.environ["PLAID_SANDBOX_SECRET"] == "synthetic-value"


def test_the_env_file_variable_is_honoured(tmp_path, monkeypatch):
    env = tmp_path / "plaid.env"
    env.write_text("PLAID_CLIENT_ID=from-variable\n")
    monkeypatch.setenv("PLAID_ENV_FILE", str(env))
    monkeypatch.setenv("PLAID_CLIENT_ID", "inherited")
    login._source_env_file(None)
    assert os.environ["PLAID_CLIENT_ID"] == "from-variable"


def test_a_missing_env_file_is_an_error(tmp_path, monkeypatch):
    monkeypatch.delenv("PLAID_ENV_FILE", raising=False)
    with pytest.raises(SystemExit, match="does not exist"):
        login._source_env_file(tmp_path / "nope.env")
