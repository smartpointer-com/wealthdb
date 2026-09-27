"""Unit tests for the browserless half of explore.py: argument parsing,
the americanexpress.com origin gate, the click-recorder-JS ↔ Python
contract, the post-close HAR scrub, what a whole session leaves on disk,
and the login pre-fill logic — all driven against stub Playwright
objects, so no browser is needed.

Synthetic values only (no real credentials or account data).
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import explore  # noqa: E402

USERNAME = "example-user"
PASSWORD = "example-password"


# ============================================================
# Argument parsing
# ============================================================

def test_parse_args_defaults():
    args = explore.parse_args([])
    assert args.url == "https://www.americanexpress.com/"
    assert args.profile_dir == Path("/secrets/amex-profile")
    assert args.env_file == Path("/secrets/amex.env")
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
        "--url", "https://global.americanexpress.com/login",
        "--profile-dir", "/tmp/p",
        "--debug-dir", "/tmp/d",
        "--env-file", "/tmp/e.env",
        "--max-duration", "60",
        "--chunk-interval", "5",
        "--trace", "--no-prefill", "--fresh", "-v",
    ])
    assert args.url == "https://global.americanexpress.com/login"
    assert args.profile_dir == Path("/tmp/p")
    assert args.debug_dir == Path("/tmp/d")
    assert args.env_file == Path("/tmp/e.env")
    assert args.max_duration == 60
    assert args.chunk_interval == 5
    assert args.trace and args.no_prefill and args.fresh and args.verbose


def test_no_password_flag_exists():
    # Credentials reach the harness via env only (root AGENTS.md §3);
    # a --password flag must never parse.
    with pytest.raises(SystemExit):
        explore.parse_args(["--password", "x"])


# ============================================================
# The americanexpress.com origin gate
# ============================================================

def test_host_gate_accepts_amex_and_subdomains():
    for host in ("americanexpress.com", "www.americanexpress.com",
                 "global.americanexpress.com",
                 "online.americanexpress.com", "AMERICANEXPRESS.COM"):
        assert explore.HOST_RE.search(host), host


def test_host_gate_rejects_lookalikes():
    for host in ("americanexpress.com.evil.example",
                 "notamericanexpress.com", "evil-americanexpress.com",
                 "americanexpress.org", "american-express.com",
                 "aexp.com", ""):
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


def test_the_click_recorder_never_logs_a_form_value():
    # innerText is always empty on INPUT/TEXTAREA, so a button or link keeps
    # its label while no field value can contribute anything — which is what
    # keeps a filled username, and a password field a show-password toggle
    # flipped to type=text, out of the click log.
    assert ".value" not in explore.CLICK_RECORDER_JS


# ============================================================
# What reaches the network log, and what the HAR keeps
# ============================================================


def _har(**request_extra) -> dict:
    request = {
        "method": "POST",
        "url": "https://www.example.com/logon?api_key=SYNTHETICKEY",
        "queryString": [{"name": "api_key", "value": "SYNTHETICKEY"}],
        "headers": [
            {"name": "Cookie", "value": "session=SYNTHETICJAR"},
            {"name": "Authorization", "value": "Bearer SYNTHETICBEARER"},
            {"name": "Content-Type",
             "value": "application/x-www-form-urlencoded"},
        ],
        "cookies": [{"name": "session", "value": "SYNTHETICJAR"}],
        "postData": {
            "mimeType": "application/x-www-form-urlencoded",
            "text": "UserID=example-user&Password=p%40ss%2Bword",
            "params": [{"name": "UserID", "value": "example-user"},
                       {"name": "Password", "value": "p@ss+word"}],
        },
    }
    request.update(request_extra)
    return {"log": {"entries": [{
        "request": request,
        "response": {
            "status": 200,
            "url": "https://www.example.com/logon?api_key=SYNTHETICKEY",
            "redirectURL":
                "https://www.example.com/next?token=SYNTHETICREDIRECT",
            "headers": [{"name": "Set-Cookie",
                         "value": "session=SYNTHETICJAR"}],
            "cookies": [{"name": "session", "value": "SYNTHETICJAR"}],
            "content": {"text": '{"user":"example-user"}'},
        },
    }]}}


def test_the_har_is_redacted_of_every_credential_it_recorded(tmp_path):
    # Playwright records the HAR raw — the sign-in POST body, the cookie jar,
    # the response bodies — and nothing else in the harness touches it. The
    # rewrite is the SHARED one; what this pins is that amex's own recorded
    # shape comes out of it clean. The generic properties (an absent file, an
    # unparseable one, a base64 body) are pinned once, in the collectorkit
    # suite, rather than per collector.
    har = tmp_path / "network.har"
    har.write_text(json.dumps(_har()), encoding="utf-8")
    redact = explore.debugcap.secret_redactor("example-user", "p@ss+word")

    assert explore.debugcap.redact_har(har, redact, log=explore.log)

    blob = har.read_text(encoding="utf-8")
    for secret in ("SYNTHETICKEY", "SYNTHETICJAR", "SYNTHETICBEARER",
                   "SYNTHETICREDIRECT",
                   "example-user", "p@ss+word", "p%40ss%2Bword"):
        assert secret not in blob, secret
    entry = json.loads(blob)["log"]["entries"][0]
    # The jar's VALUES go and its names stay: a cookie name is not a
    # credential, and which cookies an endpoint set is a diagnostic.
    for half in ("request", "response"):
        assert [c["name"] for c in entry[half]["cookies"]] == ["session"]
        assert entry[half]["cookies"][0]["value"] == explore.debugcap.REDACTED
    # Header and field NAMES survive — that a request carried a bearer, and
    # that the form posts a Password field, is exactly the diagnostic.
    assert [h["name"] for h in entry["request"]["headers"]] == [
        "Cookie", "Authorization", "Content-Type"]
    assert "Password" in entry["request"]["postData"]["text"]
    # A header carrying nothing secret is left legible.
    assert entry["request"]["headers"][2]["value"] == (
        "application/x-www-form-urlencoded")


def test_a_form_field_named_password_goes_even_when_its_value_is_unknown(
        tmp_path):
    # The --no-prefill case on amex's own sign-in body: the credential was
    # typed by hand, so no value-based masker can reach it. The field NAME
    # still can.
    har = tmp_path / "network.har"
    har.write_text(json.dumps(_har()), encoding="utf-8")
    assert explore.debugcap.redact_har(har, log=explore.log)
    blob = har.read_text(encoding="utf-8")
    assert "p@ss+word" not in blob and "p%40ss%2Bword" not in blob


# ============================================================
# What a whole session leaves behind — driven with no browser
# ============================================================

class _StubPage:
    """The one page main() opens, which it only navigates."""
    url = "https://www.example.com/"

    def goto(self, *args, **kwargs) -> None:
        pass


class _StubRequest:
    """The sign-in POST: form-urlencoded, carrying a `Password` field."""
    method = "POST"
    url = "https://www.example.com/logon"
    resource_type = "xhr"
    headers = {"content-type": "application/x-www-form-urlencoded"}
    post_data = "UserID=example-user&Password=p%40ss%2Bword"


class _StubContext:
    """Enough BrowserContext for one lap of main() with no browser.

    `on` fires the request handler as it is registered: that handler is a
    closure over the open network.jsonl, so firing it from inside is the
    only way to put a real body through the path that writes the log.
    """

    def __init__(self, *, fire_request=None, fail_on_init_script=False):
        self._fire_request = fire_request
        self._fail = fail_on_init_script

    def add_init_script(self, *args, **kwargs) -> None:
        if self._fail:
            raise RuntimeError("session died mid-flight")

    def on(self, event, handler) -> None:
        if event == "request" and self._fire_request is not None:
            handler(self._fire_request)

    def new_page(self):
        return _StubPage()


def _stub_camoufox(monkeypatch, context, *, har_body=None):
    """Replace Camoufox with a stub whose close flushes a raw HAR.

    Playwright writes the recorded HAR only on the context close and writes
    it unmasked, which is exactly the sequencing the scrub has to survive.
    """
    sync_api = pytest.importorskip("camoufox.sync_api")

    class _Cam:
        def __init__(self, **kwargs):
            self._har = Path(kwargs["record_har_path"])

        def __enter__(self):
            return context

        def __exit__(self, *exc_info):
            if har_body is not None:
                self._har.write_text(json.dumps(har_body), encoding="utf-8")
            return False

    monkeypatch.setattr(sync_api, "Camoufox", _Cam)


def _session_argv(tmp_path) -> list[str]:
    # --max-duration 0 makes the poll loop fall through at its first check,
    # so a session runs start to finish without waiting on anything.
    return ["--debug-dir", str(tmp_path / "debug"),
            "--profile-dir", str(tmp_path / "profile"),
            "--env-file", str(tmp_path / "absent.env"),
            "--dom-interval", "0",
            "--max-duration", "0"]


def _no_credentials(monkeypatch, tmp_path) -> None:
    # The run this harness is most often given: no env file, so the redactor
    # is the identity function and the credential is typed by hand.
    monkeypatch.delenv(explore.USER_ENV, raising=False)
    monkeypatch.delenv(explore.PASS_ENV, raising=False)
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path / "cache"))


def test_the_network_log_masks_a_password_field_it_never_saw(tmp_path,
                                                             monkeypatch):
    # network.jsonl is the crash-safe record, so it cannot be the weaker of
    # the two: the sign-in body reaches it as well, and on a run with no
    # credentials to mask by value only the field NAME can catch it.
    _no_credentials(monkeypatch, tmp_path)
    _stub_camoufox(monkeypatch, _StubContext(fire_request=_StubRequest()))

    assert explore.main(_session_argv(tmp_path)) == 0

    blob = (tmp_path / "debug" / "network.jsonl").read_text(encoding="utf-8")
    assert "p@ss+word" not in blob and "p%40ss%2Bword" not in blob
    # That the form posts a Password field is the diagnostic; its value is
    # the secret.
    assert "Password" in blob


def test_an_error_mid_session_still_leaves_the_har_scrubbed(tmp_path,
                                                            monkeypatch):
    # The close that flushes the raw HAR runs on every unwind, so a scrub
    # sequenced after the with-block would be skipped on exactly the path
    # that leaves a cleartext credential on disk.
    _no_credentials(monkeypatch, tmp_path)
    _stub_camoufox(monkeypatch,
                   _StubContext(fail_on_init_script=True),
                   har_body=_har())

    with pytest.raises(RuntimeError):
        explore.main(_session_argv(tmp_path))

    blob = (tmp_path / "debug" / "network.har").read_text(encoding="utf-8")
    for secret in ("SYNTHETICKEY", "SYNTHETICJAR", "SYNTHETICBEARER",
                   "SYNTHETICREDIRECT", "p@ss+word", "p%40ss%2Bword"):
        assert secret not in blob, secret


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


def amex_frame(**kw):
    return StubFrame("https://www.americanexpress.com/", **kw)


def test_prefill_fills_both_fields_and_only_once():
    user, pwd = StubField(), StubField()
    page = StubPage(amex_frame(user=user, pwd=pwd))
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
    page = StubPage(StubFrame("https://americanexpress.com.evil.example/login",
                              user=user, pwd=pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_requires_both_fields_in_same_frame():
    # A lone matching input (e.g. a 2FA code entry, or a search box the
    # user selector accidentally matches) must never receive a credential.
    lone_user = StubField()
    page = StubPage(amex_frame(user=lone_user))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert lone_user.fill_calls == []

    lone_pwd = StubField()
    page = StubPage(amex_frame(pwd=lone_pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert lone_pwd.fill_calls == []


def test_prefill_ignores_invisible_form():
    user, pwd = StubField(visible=False), StubField()
    page = StubPage(amex_frame(user=user, pwd=pwd))
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_reaches_form_in_same_brand_iframe():
    # The embedded-sign-in shape: the top document has no form; the
    # sign-in module is an iframe on an americanexpress.com subdomain.
    user, pwd = StubField(), StubField()
    page = StubPage(
        StubFrame("https://www.americanexpress.com/"),
        StubFrame("https://global.americanexpress.com/frame",
                  user=user, pwd=pwd),
    )
    assert explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.value == USERNAME and pwd.value == PASSWORD


def test_prefill_never_reaches_foreign_iframe():
    # A third-party iframe with a login-shaped form (an embedded widget)
    # must not receive the credentials even when the top document is
    # americanexpress.com.
    user, pwd = StubField(), StubField()
    page = StubPage(
        StubFrame("https://www.americanexpress.com/"),
        StubFrame("https://widgets.example.net/login", user=user, pwd=pwd),
    )
    assert not explore._maybe_prefill_login(page, USERNAME, PASSWORD, set())
    assert user.fill_calls == [] and pwd.fill_calls == []


def test_prefill_leaves_hand_typed_values_alone():
    user = StubField(value="typed-by-hand")
    pwd = StubField()
    page = StubPage(amex_frame(user=user, pwd=pwd))
    filled: set = set()
    assert explore._maybe_prefill_login(page, USERNAME, PASSWORD, filled)
    assert user.value == "typed-by-hand"    # untouched, marked done
    assert user.fill_calls == []
    assert pwd.value == PASSWORD


def test_prefill_overwrites_an_existing_value_when_told_to():
    """The trusted-device shape (DESIGN.md §G): the form arrives already
    carrying a MASKED user id, which submits successfully only while the
    device trust holds. login.py passes overwrite=True so the real
    username replaces it — the default would leave the mask in place and
    the sign-in would work until it silently did not."""
    user = StubField(value="***masked***")
    pwd = StubField()
    page = StubPage(amex_frame(user=user, pwd=pwd))
    assert explore._maybe_prefill_login(page, USERNAME, PASSWORD, set(),
                                        overwrite=True)
    assert user.value == USERNAME


def test_prefill_retries_once_then_gives_up_without_refighting():
    # A field that rejects programmatic fills: fill → verify-fail →
    # clear + refill → verify-fail → marked done (so later polls never
    # fight whatever gets hand-typed), reported as not-filled.
    user = StubField(sticky=False)
    pwd = StubField(sticky=False)
    page = StubPage(amex_frame(user=user, pwd=pwd))
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
    page1 = StubPage(amex_frame(user=StubField(), pwd=StubField()))
    page2 = StubPage(amex_frame(user=StubField(), pwd=StubField()))
    assert explore._maybe_prefill_login(page1, USERNAME, PASSWORD, filled)
    assert explore._maybe_prefill_login(page2, USERNAME, PASSWORD, filled)
