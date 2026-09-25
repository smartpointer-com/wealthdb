"""Unit tests for the browserless half of explore.py: argument parsing,
the chase.com origin gate, the click-recorder-JS ↔ Python contract, and
the login pre-fill logic — the latter driven against stub Playwright
objects, so no browser is needed. The shared recording machinery is
tested in collectorkit.

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
    assert args.url == "https://secure.chase.com"
    assert args.profile_dir == Path("/secrets/chase-profile")
    assert args.env_file == Path("/secrets/chase.env")
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
        "--url", "https://www.chase.com",
        "--profile-dir", "/tmp/p",
        "--debug-dir", "/tmp/d",
        "--env-file", "/tmp/e.env",
        "--max-duration", "60",
        "--chunk-interval", "5",
        "--trace", "--no-prefill", "--fresh", "-v",
    ])
    assert args.url == "https://www.chase.com"
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
# The chase.com origin gate
# ============================================================

def test_host_gate_accepts_chase_and_subdomains():
    for host in ("chase.com", "www.chase.com", "secure.chase.com",
                 "secure05c.chase.com", "CHASE.COM"):
        assert explore.HOST_RE.search(host), host


def test_host_gate_rejects_lookalikes():
    for host in ("chase.com.evil.example", "notchase.com", "evil-chase.com",
                 "chase.org", "jpmorgan.com", ""):
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


def chase_frame(**kw):
    return StubFrame("https://secure.chase.com/", **kw)


def test_prefill_fills_both_fields_and_only_once():
    user, pwd = StubField(), StubField()
    page = StubPage(chase_frame(user=user, pwd=pwd))
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
    page = StubPage(StubFrame("https://chase.com.evil.example/login",
                              user=user, pwd=pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_requires_both_fields_in_same_frame():
    # A lone matching input (e.g. the 2FA code entry, or a search box the
    # user selector accidentally matches) must never receive a credential.
    lone_user = StubField()
    page = StubPage(chase_frame(user=lone_user))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert lone_user.fill_calls == []

    lone_pwd = StubField()
    page = StubPage(chase_frame(pwd=lone_pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert lone_pwd.fill_calls == []


def test_prefill_ignores_invisible_form():
    user, pwd = StubField(visible=False), StubField()
    page = StubPage(chase_frame(user=user, pwd=pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_reaches_form_in_chase_iframe():
    # The www.chase.com homepage shape: the top document has no form; the
    # sign-in module is an iframe on a secure*.chase.com subdomain.
    user, pwd = StubField(), StubField()
    page = StubPage(
        StubFrame("https://www.chase.com/"),
        StubFrame("https://secure05c.chase.com/frame", user=user, pwd=pwd),
    )
    assert explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.value == USERNAME and pwd.value == PASSWORD


def test_prefill_never_reaches_foreign_iframe():
    # A third-party iframe with a login-shaped form (an embedded widget)
    # must not receive the credentials even when the top document is
    # chase.com.
    user, pwd = StubField(), StubField()
    page = StubPage(
        StubFrame("https://www.chase.com/"),
        StubFrame("https://widgets.example.net/login", user=user, pwd=pwd),
    )
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_leaves_hand_typed_values_alone():
    user = StubField(value="typed-by-hand")
    pwd = StubField()
    page = StubPage(chase_frame(user=user, pwd=pwd))
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
    page = StubPage(chase_frame(user=user, pwd=pwd))
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
    page1 = StubPage(chase_frame(user=StubField(), pwd=StubField()))
    page2 = StubPage(chase_frame(user=StubField(), pwd=StubField()))
    assert explore._maybe_prefill_login(page1, USERNAME, PASSWORD, filled)
    assert explore._maybe_prefill_login(page2, USERNAME, PASSWORD, filled)
