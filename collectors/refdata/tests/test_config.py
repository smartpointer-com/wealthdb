"""refdata.cfg: a JSON object, refused when it names what does not exist."""

import json

import pytest

import config

KNOWN = {"inbox": None, "sec-nport": None, "ishares": None}


def write(tmp_path, doc):
    p = tmp_path / "refdata.cfg"
    p.write_text(doc if isinstance(doc, str) else json.dumps(doc))
    return p


def test_missing_file_holds_the_defaults(tmp_path):
    c = config.load(KNOWN, tmp_path / "none.cfg")
    assert c.max_age_days == 30 and "sec-nport" in c.providers and "ishares" not in c.providers


def test_settings_read(tmp_path):
    c = config.load(KNOWN, write(tmp_path, {"providers": ["ishares", "inbox"], "contact": " a@example.org ",
                                            "max_age_days": 7, "constituents": True}))
    assert c.providers == ("ishares", "inbox") and c.contact == "a@example.org"
    assert c.max_age_days == 7 and c.constituents


@pytest.mark.parametrize("doc,msg", [
    ("{", "not valid JSON"),
    ([], "JSON object"),
    ({"provider": []}, "does not know: provider"),
    ({"providers": ["nope"]}, "names nope"),
    ({"providers": ["inbox", "inbox"]}, "twice"),
    ({"max_age_days": 0}, "max_age_days"),
    ({"constituents": "yes"}, "constituents"),
])
def test_refuses(tmp_path, doc, msg):
    with pytest.raises(config.ConfigError, match=msg):
        config.load(KNOWN, write(tmp_path, doc))


def test_engine_precedence(monkeypatch):
    monkeypatch.delenv("REFDATA_WEALTHDB", raising=False)
    assert config.engine_command(config.Config()) == "wealthdb"
    assert config.engine_command(config.Config(engine="/x/wealthdb")) == "/x/wealthdb"
    monkeypatch.setenv("REFDATA_WEALTHDB", "/y/wealthdb")
    assert config.engine_command(config.Config(engine="/x/wealthdb")) == "/y/wealthdb"
