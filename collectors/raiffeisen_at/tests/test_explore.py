"""Unit tests for the browserless half of explore.py: argument parsing,
the raiffeisen.at origin gate, the click-recorder-JS ↔ Python contract,
the env-file sourcing contract, and the login pre-fill logic — the
latter driven against stub Playwright objects, so no browser is needed.

Synthetic values only (no real credentials or account data).
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import explore  # noqa: E402

USERNAME = "example-user"
PASSWORD = "example-password"


# ============================================================
# Argument parsing
# ============================================================

def test_parse_args_defaults():
    args = explore.parse_args([])
    assert args.url == "https://mein.elba.raiffeisen.at/"
    assert args.profile_dir == Path("/secrets/raiffeisen_at-profile")
    assert args.env_file == Path("/secrets/raiffeisen_at.env")
    assert args.debug_dir is None          # → /debug/<UTC-ts>/ at runtime
    assert args.max_duration == 3600       # human-in-the-loop: 1 hour
    assert args.chunk_interval == 30
    assert args.dom_interval == 3          # DOM snapshots on by default
    assert args.trace is False             # opt-in (tracer crash, see help)
    assert args.no_prefill is False
    assert args.fresh is False
    assert args.verbose is False


def test_parse_args_overrides():
    args = explore.parse_args([
        "--url", "https://sso.raiffeisen.at/login",
        "--profile-dir", "/tmp/p",
        "--debug-dir", "/tmp/d",
        "--env-file", "/tmp/e.env",
        "--max-duration", "60",
        "--chunk-interval", "5",
        "--trace", "--no-prefill", "--fresh", "-v",
    ])
    assert args.url == "https://sso.raiffeisen.at/login"
    assert args.profile_dir == Path("/tmp/p")
    assert args.debug_dir == Path("/tmp/d")
    assert args.env_file == Path("/tmp/e.env")
    assert args.max_duration == 60
    assert args.chunk_interval == 5
    assert args.trace and args.no_prefill and args.fresh and args.verbose


def test_no_password_flag_exists():
    # Credentials reach the harness via env only (root CLAUDE.md §3);
    # a --password flag must never parse.
    import pytest
    with pytest.raises(SystemExit):
        explore.parse_args(["--password", "x"])


# ============================================================
# DOM-snapshot skeleton (structure-only dedup)
# ============================================================

def test_dom_skeleton_ignores_text_and_values():
    # Same structure, different text / dynamic attribute values → same
    # skeleton, so a screen is snapshotted once, not every tick.
    a = explore._dom_skeleton('<div id="opt"><span>pushTAN senden</span></div>')
    b = explore._dom_skeleton('<div id="opt"><span>Warten 12:03</span></div>')
    assert a == b


def test_dom_skeleton_differs_on_structure():
    # A new screen (different tags / ids) → different skeleton → new snapshot.
    a = explore._dom_skeleton('<li id="pushtan">')
    b = explore._dom_skeleton('<input id="tan" inputmode="numeric">')
    assert a != b


# ============================================================
# The raiffeisen.at origin gate
# ============================================================

def test_host_gate_accepts_raiffeisen_at_and_subdomains():
    for host in ("raiffeisen.at", "www.raiffeisen.at",
                 "mein.elba.raiffeisen.at", "sso.raiffeisen.at",
                 "RAIFFEISEN.AT"):
        assert explore.HOST_RE.search(host), host


def test_host_gate_rejects_lookalikes_and_other_countries():
    # The collector is namespaced to Austria: Raiffeisen's banking systems
    # in other countries are distinct, so their hosts are rejected along
    # with the usual suffix-spoof lookalikes.
    for host in ("raiffeisen.at.evil.example", "notraiffeisen.at",
                 "evil-raiffeisen.at", "elba-raiffeisen.at",
                 "raiffeisenbank.at", "raiffeisen.ch", "raiffeisen.de",
                 "raiffeisen.rs", ""):
        assert not explore.HOST_RE.search(host), host


# ============================================================
# Click-recorder JS ↔ Python contract
# ============================================================

def test_js_uses_the_python_event_prefix():
    # The console listener matches on EVENT_PREFIX (trailing space
    # included); the injected JS must emit exactly that sentinel or every
    # event is silently dropped.
    assert f"'{explore.EVENT_PREFIX}'" in explore.CLICK_RECORDER_JS


def test_js_mirrors_the_python_host_gate():
    # The JS detector carries its own copy of the origin gate; the two
    # must not drift apart.
    assert explore.HOST_RE.pattern in explore.CLICK_RECORDER_JS


def test_js_redacts_password_values():
    assert "<redacted>" in explore.CLICK_RECORDER_JS
    assert "type === 'password'" in explore.CLICK_RECORDER_JS


def test_user_selector_carries_the_german_hooks():
    # The Mein ELBA login is keyed on a Verfüger number or user name; the
    # German attribute hooks must be present in the Python selector AND
    # its JS mirror (the two are maintained by hand).
    for hook in ("verfueger", "benutzer"):
        assert hook in explore.USER_SELECTOR
        assert hook in explore.CLICK_RECORDER_JS


# ============================================================
# Env-file sourcing contract
# ============================================================

def test_absent_env_file_is_skipped_silently(tmp_path):
    # explore sources /secrets/raiffeisen_at.env when present and proceeds
    # without it otherwise (pre-fill then simply disables itself).
    assert explore.envfile.source_env_file(
        tmp_path / "raiffeisen_at.env") is False


# ============================================================
# Login pre-fill — stub Playwright objects, no browser
# ============================================================

class StubField:
    """Stands in for a Playwright Locator resolving to one input field.

    ``sticky=False`` models a field whose page JS rejects programmatic
    fills (the read-back never matches), driving the retry-then-warn
    path."""

    def __init__(self, value="", visible=True, sticky=True):
        self.value = value
        self.visible = visible
        self.sticky = sticky
        self.fill_calls = []

    @property
    def first(self):
        return self

    def count(self):
        return 1

    def is_visible(self):
        return self.visible

    def fill(self, v, timeout=None):
        self.fill_calls.append(v)
        if self.sticky or v == "":
            self.value = v

    def input_value(self, timeout=None):
        return self.value


class AbsentField:
    """A Locator that matches nothing."""

    @property
    def first(self):
        return self

    def count(self):
        return 0

    def is_visible(self):
        return False

    def fill(self, v, timeout=None):
        raise AssertionError("fill() on an absent field")

    def input_value(self, timeout=None):
        raise AssertionError("input_value() on an absent field")


class StubFrame:
    def __init__(self, url, user=None, pwd=None):
        self.url = url
        self._user = user if user is not None else AbsentField()
        self._pwd = pwd if pwd is not None else AbsentField()

    def locator(self, selector):
        return self._pwd if selector == explore.PWD_SELECTOR else self._user


class StubPage:
    def __init__(self, *frames):
        self.frames = list(frames)
        self.url = frames[0].url


def elba_frame(**kw):
    return StubFrame("https://mein.elba.raiffeisen.at/", **kw)


def test_prefill_fills_both_fields_and_only_once():
    user, pwd = StubField(), StubField()
    page = StubPage(elba_frame(user=user, pwd=pwd))
    filled: set = set()
    assert explore._maybe_prefill_login(page, USERNAME, PASSWORD, filled)
    assert user.value == USERNAME
    assert pwd.value == PASSWORD
    # Second poll is a no-op: the fields are marked done, nothing re-fills.
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, filled)
    assert user.fill_calls == [USERNAME]
    assert pwd.fill_calls == [PASSWORD]


def test_prefill_rejects_foreign_host():
    user, pwd = StubField(), StubField()
    page = StubPage(StubFrame("https://raiffeisen.at.evil.example/login",
                              user=user, pwd=pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_requires_both_fields_in_same_frame():
    # A lone matching input (a TAN code entry, a two-step login's first
    # screen, or a search box the user selector accidentally matches) must
    # never receive a credential.
    lone_user = StubField()
    page = StubPage(elba_frame(user=lone_user))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert lone_user.fill_calls == []

    lone_pwd = StubField()
    page = StubPage(elba_frame(pwd=lone_pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert lone_pwd.fill_calls == []


def test_prefill_ignores_invisible_form():
    user, pwd = StubField(visible=False), StubField()
    page = StubPage(elba_frame(user=user, pwd=pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_reaches_form_in_same_brand_iframe():
    # The embedded-sign-in shape: the top document has no form; the
    # sign-in module is an iframe on a raiffeisen.at subdomain.
    user, pwd = StubField(), StubField()
    page = StubPage(
        StubFrame("https://mein.elba.raiffeisen.at/"),
        StubFrame("https://sso.raiffeisen.at/frame", user=user, pwd=pwd),
    )
    assert explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.value == USERNAME and pwd.value == PASSWORD


def test_prefill_never_reaches_foreign_iframe():
    # A third-party iframe with a login-shaped form (an embedded widget)
    # must not receive the credentials even when the top document is
    # raiffeisen.at.
    user, pwd = StubField(), StubField()
    page = StubPage(
        StubFrame("https://mein.elba.raiffeisen.at/"),
        StubFrame("https://widgets.example.net/login", user=user, pwd=pwd),
    )
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_leaves_hand_typed_values_alone():
    user = StubField(value="typed-by-hand")
    pwd = StubField()
    page = StubPage(elba_frame(user=user, pwd=pwd))
    filled: set = set()
    assert explore._maybe_prefill_login(page, USERNAME, PASSWORD, filled)
    assert user.value == "typed-by-hand"    # untouched, marked done
    assert user.fill_calls == []
    assert pwd.value == PASSWORD


def test_prefill_retries_once_then_gives_up_without_refighting():
    # A field that rejects programmatic fills: fill → verify-fail →
    # clear + refill → verify-fail → marked done (so later polls never
    # fight whatever gets hand-typed), reported as not-filled.
    user = StubField(sticky=False)
    pwd = StubField(sticky=False)
    page = StubPage(elba_frame(user=user, pwd=pwd))
    filled: set = set()
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, filled)
    assert user.fill_calls == [USERNAME, "", USERNAME]
    # Marked done: a second poll touches nothing.
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, filled)
    assert user.fill_calls == [USERNAME, "", USERNAME]


def test_prefill_tracks_fills_per_page():
    # The fill-once bookkeeping is keyed per page: a second tab with its
    # own login form still gets filled.
    filled: set = set()
    page1 = StubPage(elba_frame(user=StubField(), pwd=StubField()))
    page2 = StubPage(elba_frame(user=StubField(), pwd=StubField()))
    assert explore._maybe_prefill_login(page1, USERNAME, PASSWORD, filled)
    assert explore._maybe_prefill_login(page2, USERNAME, PASSWORD, filled)
