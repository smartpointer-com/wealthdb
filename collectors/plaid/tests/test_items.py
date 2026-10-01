"""Tests for items, the store of linked Items: one credential file per
Item, owner-only, and never mistaken for absent when it is merely
unreadable."""
from __future__ import annotations

import json

import pytest
from conftest import LINK_TOKEN, access_token

import items


def an_item(name="bank", environment="sandbox", n=1) -> items.Item:
    return items.Item(
        name=name, environment=environment,
        access_token=access_token(environment, n),
        item_id=f"item-synthetic-{n}", institution_id="ins_000",
        institution_name="Synthetic Bank",
        linked_at="2026-01-02T03:04:05+00:00")


# ---- names -------------------------------------------------------------------

@pytest.mark.parametrize("name", ["bank", "bank-2", "b", "0bank", "my_bank",
                                  "a" * 40])
def test_valid_names(name):
    assert items.check_name(name) == name


@pytest.mark.parametrize("name", ["", "Bank", "my bank", "-bank", "_bank",
                                  "bank.json", "../bank", "bank/x",
                                  "a" * 41, None])
def test_invalid_names(name):
    with pytest.raises(ValueError, match="not a valid Item name"):
        items.check_name(name)


def test_a_path_cannot_be_built_from_an_invalid_name(secrets):
    with pytest.raises(ValueError):
        items.token_path(secrets, "../escape")
    with pytest.raises(ValueError):
        items.pending_path(secrets, "a/b")


# ---- storing and reading back --------------------------------------------------

def test_an_item_round_trips_through_its_file(secrets):
    item = an_item()
    items.save_item(secrets, item)
    path = secrets / "plaid-token-bank.json"
    assert path.stat().st_mode & 0o777 == 0o600
    assert items.load_item(secrets, "bank") == item
    assert json.loads(path.read_text())["access_token"] == item.access_token


def test_no_file_means_no_item(secrets):
    assert items.load_item(secrets, "bank") is None
    assert items.load_pending(secrets, "bank") is None
    assert items.list_items(secrets) == []


def test_saving_one_item_leaves_the_others_files_untouched(secrets):
    items.save_item(secrets, an_item("one", n=1))
    before = (secrets / "plaid-token-one.json").stat().st_mtime_ns
    items.save_item(secrets, an_item("two", n=2))
    assert (secrets / "plaid-token-one.json").stat().st_mtime_ns == before
    assert [i.name for i in items.list_items(secrets)] == ["one", "two"]


def test_an_item_does_not_print_its_token(secrets):
    item = an_item()
    assert item.access_token not in repr(item)
    assert item.access_token not in str(item)


def test_a_file_readable_by_others_is_narrowed_on_load(secrets):
    items.save_item(secrets, an_item())
    path = secrets / "plaid-token-bank.json"
    path.chmod(0o644)
    items.load_item(secrets, "bank")
    assert path.stat().st_mode & 0o777 == 0o600


# ---- a file that exists is never "no Item" -----------------------------------

@pytest.mark.parametrize("content", [
    "", "not json", "[]", "{}", '{"name": "bank"}',
])
def test_an_unreadable_token_file_is_an_error_not_an_absence(secrets, content):
    # Read as absent, the next step would be a second link: one more of a
    # Trial plan's ten Items, and at some institutions the end of the first.
    (secrets / "plaid-token-bank.json").write_text(content)
    with pytest.raises(items.ItemStoreError):
        items.load_item(secrets, "bank")
    with pytest.raises(items.ItemStoreError):
        items.list_items(secrets)


def test_the_error_does_not_quote_the_file(secrets):
    token = access_token()
    (secrets / "plaid-token-bank.json").write_text(
        f'{{"name": "bank", "access_token": "{token}"')    # truncated JSON
    with pytest.raises(items.ItemStoreError) as caught:
        items.load_item(secrets, "bank")
    assert token not in str(caught.value)


def test_a_file_renamed_to_another_item_is_refused(secrets):
    items.save_item(secrets, an_item("bank"))
    (secrets / "plaid-token-bank.json").rename(
        secrets / "plaid-token-other.json")
    with pytest.raises(items.ItemStoreError, match="named after its Item"):
        items.load_item(secrets, "other")


def test_a_token_of_another_environment_is_refused(secrets):
    item = an_item(environment="production")
    item.access_token = access_token("sandbox")
    items.save_item(secrets, item)
    with pytest.raises(items.ItemStoreError, match="another one"):
        items.load_item(secrets, "bank")


@pytest.mark.parametrize("field", ["access_token", "item_id"])
def test_a_record_without_its_credential_is_refused(secrets, field):
    item = an_item()
    setattr(item, field, "")
    items.save_item(secrets, item)
    with pytest.raises(items.ItemStoreError):
        items.load_item(secrets, "bank")


def test_an_unknown_environment_is_refused(secrets):
    item = an_item()
    item.environment = "development"
    items.save_item(secrets, item)
    with pytest.raises(items.ItemStoreError, match="unknown environment"):
        items.load_item(secrets, "bank")


def test_a_stray_file_under_the_prefix_is_passed_over(secrets, caplog):
    items.save_item(secrets, an_item())
    (secrets / "plaid-token-bank copy.json").write_text("{}")
    (secrets / "plaid-token-bank.json.tmp").write_text("{}")
    assert [i.name for i in items.list_items(secrets)] == ["bank"]
    assert "bank copy.json" in caplog.text


# ---- a sign-in that is not settled yet -----------------------------------------

def a_pending(name="bank") -> items.PendingLink:
    return items.PendingLink(
        name=name, environment="sandbox", link_token=LINK_TOKEN,
        hosted_link_url="https://hosted.example.invalid/hl/synthetic",
        required="transactions", created_at="2026-01-02T03:04:05+00:00",
        expires_at=1_800_003_600)


def test_a_pending_link_round_trips_and_clears(secrets):
    pending = a_pending()
    items.save_pending(secrets, pending)
    path = secrets / "plaid-link-bank.json"
    assert path.stat().st_mode & 0o777 == 0o600
    assert items.load_pending(secrets, "bank") == pending
    items.clear_pending(secrets, "bank")
    assert not path.exists()
    items.clear_pending(secrets, "bank")        # clearing twice is fine


def test_a_pending_link_does_not_print_its_token_or_url():
    pending = a_pending()
    assert pending.link_token not in repr(pending)
    assert pending.hosted_link_url not in repr(pending)


def test_a_pending_link_is_not_an_item(secrets):
    items.save_pending(secrets, a_pending())
    assert items.list_items(secrets) == []
    assert items.load_item(secrets, "bank") is None


def test_an_unreadable_pending_link_is_an_error(secrets):
    (secrets / "plaid-link-bank.json").write_text("{")
    with pytest.raises(items.ItemStoreError):
        items.load_pending(secrets, "bank")
