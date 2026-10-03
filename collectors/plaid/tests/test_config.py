"""Tests for config: where plaid.cfg is, and that only a well-formed
opt-in passes. A typo in the file is an error, never a setting that is
off."""
from __future__ import annotations

import pytest

import config


def test_the_file_lives_in_the_xdg_config_dir(monkeypatch, tmp_path):
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "xdg"))
    assert config.path() == tmp_path / "xdg" / "plaid.cfg"
    default = tmp_path / "home" / ".config" / "plaid.cfg"
    monkeypatch.setenv("XDG_CONFIG_HOME", "  ")
    assert config.path() == default
    monkeypatch.delenv("XDG_CONFIG_HOME")
    assert config.path() == default


def test_a_missing_file_opts_in_to_nothing(tmp_path):
    assert config.load(tmp_path / "plaid.cfg") == config.Config()


def test_an_empty_object_opts_in_to_nothing(tmp_path):
    cfg = tmp_path / "plaid.cfg"
    cfg.write_text("{}")
    assert config.load(cfg).billed_reads == frozenset()


def test_the_refresh_opt_in(tmp_path):
    cfg = tmp_path / "plaid.cfg"
    cfg.write_text('{"billed_reads": ["/investments/refresh"]}')
    assert config.load(cfg).billed_reads == {"/investments/refresh"}


@pytest.mark.parametrize("text,message", [
    ("{", "is not valid JSON"),
    ("[]", "must hold a JSON object"),
    ('{"billed_read": []}', "does not know: billed_read"),
    ('{"billed_reads": "/investments/refresh"}', "is a list of routes"),
    ('{"billed_reads": [1]}', "is a list of routes"),
    ('{"billed_reads": ["/accounts/balance/get"]}', "does not call"),
])
def test_a_file_that_cannot_be_used_is_an_error(tmp_path, text, message):
    cfg = tmp_path / "plaid.cfg"
    cfg.write_text(text)
    with pytest.raises(config.ConfigError) as caught:
        config.load(cfg)
    assert str(cfg) in str(caught.value)
    assert message in str(caught.value)


def test_a_file_in_no_unicode_encoding_is_an_error(tmp_path):
    cfg = tmp_path / "plaid.cfg"
    cfg.write_bytes(b'{"billed_reads": ["\xff"]}')
    with pytest.raises(config.ConfigError, match="is not valid JSON"):
        config.load(cfg)


def test_a_file_that_cannot_be_read_is_an_error(tmp_path):
    cfg = tmp_path / "plaid.cfg"
    cfg.mkdir()
    with pytest.raises(config.ConfigError, match="cannot be read"):
        config.load(cfg)
