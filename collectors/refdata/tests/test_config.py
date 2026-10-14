"""refdata.cfg: a JSON object, refused when it names what does not exist."""

import json

import pytest

import config

KNOWN = dict.fromkeys((*config.DEFAULT_PROVIDERS, "ishares"))


def write(tmp_path, doc):
    p = tmp_path / "refdata.cfg"
    p.write_text(doc if isinstance(doc, str) else json.dumps(doc))
    return p


def test_missing_file_holds_the_defaults(tmp_path):
    c = config.load(KNOWN, tmp_path / "none.cfg")
    assert c.max_age_days == 30 and "sec-nport" in c.providers and "ishares" not in c.providers


def test_settings_read(tmp_path):
    c = config.load(KNOWN, write(tmp_path, {"providers": ["ishares", "inbox"], "contact": " a@example.org ",
                                            "max_age_days": 7, "constituents": True,
                                            "fidelity_529_plans": {"YY": "https://example.org/y.pdf",
                                                                   " XX ": "https://example.org/x.pdf"}}))
    assert c.providers == ("ishares", "inbox") and c.contact == "a@example.org"
    assert c.max_age_days == 7 and c.constituents
    assert c.fidelity_529_plans == (("XX", "https://example.org/x.pdf"), ("YY", "https://example.org/y.pdf"))


@pytest.mark.parametrize("doc,msg", [
    ("{", "not valid JSON"),
    ([], "JSON object"),
    ({"provider": []}, "does not know: provider"),
    ({"providers": ["nope"]}, "names nope"),
    ({"providers": ["inbox", "inbox"]}, "twice"),
    ({"max_age_days": 0}, "max_age_days"),
    ({"constituents": "yes"}, "constituents"),
    ({"fidelity_529_plans": {" ": "https://example.org/x.pdf"}}, "fidelity_529_plans"),
    ({"fidelity_529_plans": {"xx": "https://example.org/x.pdf", "XX ": "https://example.org/y.pdf"}}, "twice"),
    ({"fidelity_529_plans": ["https://example.org/x.pdf"]}, "fidelity_529_plans"),
    ({"fidelity_529_plans": {"X Y": "https://example.org/x.pdf"}}, "fidelity_529_plans"),
    ({"fidelity_529_plans": {"XX": "http://example.org/x.pdf"}}, "fidelity_529_plans"),
])
def test_refuses(tmp_path, doc, msg):
    path = write(tmp_path, doc)
    with pytest.raises(config.ConfigError) as e:
        config.load(KNOWN, path)
    # The message names the file; the reason follows it. A setting the
    # collector does not know is its own error, which lists them all.
    rest = str(e.value).replace(str(path), "")
    assert msg in rest and ("does not know" in rest) == ("does not know" in msg)


def test_engine_precedence(monkeypatch, tmp_path):
    monkeypatch.delenv("REFDATA_WEALTHDB", raising=False)
    # The checkout's own wrapper, else wealthdb on PATH.
    assert config.engine_command(config.Config()) == str(config.CHECKOUT_ENGINE)
    assert config.engine_command(config.Config(), tmp_path / "none") == "wealthdb"
    assert config.engine_command(config.Config(engine="/x/wealthdb")) == "/x/wealthdb"
    monkeypatch.setenv("REFDATA_WEALTHDB", "/y/wealthdb")
    assert config.engine_command(config.Config(engine="/x/wealthdb")) == "/y/wealthdb"


def test_every_provider_setting_is_a_config_field():
    from providers import REGISTRY
    fields = set(config.Config.__dataclass_fields__)
    assert {p.needs_setting for p in REGISTRY.values() if p.needs_setting} <= fields
