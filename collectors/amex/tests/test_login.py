"""Unit tests for login.py's browserless surface: argument parsing, the
logon-outcome plumbing, the challenge-screen reading, the six-box passcode
entry, the terminal challenge drive and the device-trust probe — all driven
against stub Playwright objects and a synthetic cookie jar, so no browser is
needed. The live flow is validated separately.

Synthetic values only.
"""
from __future__ import annotations

import argparse
import logging
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import amexclient  # noqa: E402
import login  # noqa: E402

CODE = "123456"


# ============================================================
# Argument parsing
# ============================================================

def test_parse_args_is_only_the_check_probe():
    # Everything that signs in lives on `download` now; this parser carries
    # only what the probe reads.
    args = login.parse_args(["--check"])
    assert args.profile_dir == Path("/secrets/amex-profile")
    assert args.check is True


def test_a_bare_login_is_refused_by_the_script_too():
    # The wrapper and the entrypoint both trap it; this closes the path where
    # something addresses login.py directly expecting a verb that is gone.
    assert login.main([]) == 2


@pytest.mark.parametrize("argv", [
    ["--check", "--fresh"],
    ["--check", "--cli-mfa"],
    ["--check", "--no-cli-mfa"],
    ["--check", "--vnc-mfa"],
    ["--check", "--state-path", "x"],
    ["--check", "--mfa-timeout", "10"],
])
def test_the_sign_in_flags_moved_to_download(argv):
    # Leaving them parsing here would let a caller think login still signs
    # in. Each argv would parse cleanly if its flag still existed here, so
    # the only thing that can raise is the flag being unknown — a trailing
    # junk positional would raise either way and prove nothing.
    with pytest.raises(SystemExit):
        login.parse_args(argv)


def test_no_password_flag_exists():
    # Credentials arrive via env only (root CLAUDE.md §3) — never argv.
    with pytest.raises(SystemExit):
        login.parse_args(["--password", "x"])


def test_no_otp_flag_exists():
    # The passcode is read from stdin, never accepted on argv.
    with pytest.raises(SystemExit):
        login.parse_args(["--otp", CODE])


def test_verbose_parses_via_the_standard_group():
    assert login.parse_args(["-v"]).verbose is True


# ============================================================
# The logon watcher
# ============================================================

class _Request:
    def __init__(self, method):
        self.method = method


class _Resp:
    def __init__(self, url, status=200, body=None, method="POST"):
        self.url, self.status, self._body = url, status, body
        self.request = _Request(method)

    def json(self):
        if self._body is None:
            raise ValueError("no body")
        return self._body


class _Context:
    def __init__(self):
        self.handlers = []

    def on(self, _event, handler):
        self.handlers.append(handler)

    def fire(self, resp):
        for h in self.handlers:
            h(resp)


def _watch_with(status, body, method="POST"):
    ctx = _Context()
    watch = login._LogonWatch().attach(ctx)
    ctx.fire(_Resp(amexclient.LOGON_URL, status, body, method))
    return watch


def test_watch_reads_a_trusted_login_as_authenticated():
    watch = _watch_with(200, {"statusCode": 0, "challenge": False,
                              "reauth": {"trust": True}})
    assert watch.outcome().authenticated


def test_watch_reads_a_challenge():
    watch = _watch_with(200, {"statusCode": 1, "errorCode": "LGON013",
                              "reauth": {"mfaId": "abc"}})
    assert watch.outcome().needs_challenge


def test_watch_ignores_the_cors_preflight():
    # The browser preflights the logon URL, and that OPTIONS answers 200 with
    # an EMPTY BODY about a second before the real response. Read as the
    # outcome it reports a refusal the provider never made — which is what
    # the first live run hit.
    assert _watch_with(200, None, method="OPTIONS").outcome() is None


def test_the_preflight_does_not_mask_the_real_response():
    # Both arrive, preflight first; the verdict must be the POST's.
    ctx = _Context()
    watch = login._LogonWatch().attach(ctx)
    ctx.fire(_Resp(amexclient.LOGON_URL, 200, None, "OPTIONS"))
    ctx.fire(_Resp(amexclient.LOGON_URL, 200,
                   {"statusCode": 0, "challenge": False,
                    "reauth": {"trust": True}}, "POST"))
    assert watch.outcome().authenticated


def test_watch_ignores_an_unrelated_response():
    ctx = _Context()
    watch = login._LogonWatch().attach(ctx)
    ctx.fire(_Resp("https://global.americanexpress.com/api/servicing/v1/x",
                   200, {"statusCode": 0}))
    assert watch.outcome() is None


def test_watch_survives_a_body_it_cannot_parse():
    watch = _watch_with(500, None)
    out = watch.outcome()
    assert out is not None and not out.authenticated


def test_watch_reset_forgets_the_first_verdict():
    # The challenge flow ends with a SECOND logon call; the first one's
    # "needs a passcode" verdict must not be mistaken for the second's.
    watch = _watch_with(200, {"statusCode": 1, "reauth": {"mfaId": "abc"}})
    assert watch.outcome().needs_challenge
    watch.reset()
    assert watch.outcome() is None


# ============================================================
# The mid-challenge probe gate
# ============================================================

class _Page:
    def __init__(self, url=""):
        self.url = url

    def wait_for_timeout(self, _ms):
        pass


@pytest.mark.parametrize("url", [
    "https://www.americanexpress.com/",
    "https://www.americanexpress.com/en-us/account/login?inav=x",
    "https://global.americanexpress.com/myca/logon/us/action/login",
])
def test_login_routes_block_the_rest_probe(url):
    # Firing an authenticated request mid-challenge poisons the challenge
    # server-side (the firstcitizens root cause), so the probe is gated.
    assert login._on_login_flow(_Page(url)) is True


def test_the_dashboard_route_allows_the_rest_probe():
    assert login._on_login_flow(
        _Page("https://global.americanexpress.com/dashboard")) is False


# ============================================================
# Challenge screen: reading the delivery options
# ============================================================

class _Locator:
    """A Playwright-ish locator over a fixed list of stub elements."""

    def __init__(self, elements):
        self._elements = list(elements)

    def count(self):
        return len(self._elements)

    def nth(self, i):
        return self._elements[i]

    @property
    def first(self):
        return self._elements[0] if self._elements else _Element(present=False)


class _Element:
    def __init__(self, text="", value="", present=True, sticky=True):
        self.text, self.value, self.present, self.sticky = (
            text, value, present, sticky)
        self.clicks = 0
        self.fills: list[str] = []

    def count(self):
        return 1 if self.present else 0

    def inner_text(self):
        return self.text

    def text_content(self):
        # Playwright exposes both accessors, so a leak through either one
        # has to be reachable from these stubs.
        return self.text

    def click(self, timeout=None):
        self.clicks += 1

    def fill(self, value, timeout=None):
        self.fills.append(value)
        if self.sticky or value == "":
            self.value = value

    def input_value(self, timeout=None):
        return self.value


class _SelectorPage:
    """A page that resolves selectors from a dict.

    A CSS selector LIST matches the union of its alternatives, so a page
    seeded with one shape answers the whole joined selector too — which is
    what lets a test pin an individual alternative of `SEL_CAPTCHA` or
    `SEL_REGISTER_DEVICE` through the real code path."""

    def __init__(self, mapping, title="Example Title"):
        self.mapping = mapping
        self.url = "https://global.americanexpress.com/dashboard"
        self._title = title

    def title(self):
        return self._title

    def locator(self, selector):
        if selector in self.mapping:
            return _Locator(self.mapping[selector])
        found = []
        for alternative in selector.split(", "):
            found.extend(self.mapping.get(alternative, []))
        return _Locator(found)

    def wait_for_timeout(self, _ms):
        pass


def test_read_challenge_targets_reads_labels_and_indexes():
    page = _SelectorPage({amexclient.SEL_CHALLENGE_OPTION: [
        _Element(text="Text Message  *******0000"),
        _Element(text="Email  e****@example.com"),
    ]})
    targets = login.read_challenge_targets(page)
    assert [t.value for t in targets] == ["0", "1"]
    assert targets[0].kind == "sms"
    assert targets[1].kind == "email"


def test_read_challenge_targets_on_an_empty_screen():
    assert login.read_challenge_targets(_SelectorPage({})) == ()


def test_target_kind_classifies_from_the_label():
    assert amexclient.target_kind("Text Message ***0000") == "sms"
    assert amexclient.target_kind("Email a@b.example") == "email"
    assert amexclient.target_kind("Call us") == "voice"
    assert amexclient.target_kind("Something else") == "other"


def test_click_challenge_target_clicks_by_index():
    first, second = _Element(text="a"), _Element(text="b")
    page = _SelectorPage({amexclient.SEL_CHALLENGE_OPTION: [first, second]})
    target = amexclient.ChallengeTarget(value="1", display="b", kind="other")
    assert login._click_challenge_target(page, target) is True
    assert second.clicks == 1 and first.clicks == 0


def test_click_challenge_target_out_of_range_is_a_clean_false():
    page = _SelectorPage({amexclient.SEL_CHALLENGE_OPTION: [_Element()]})
    target = amexclient.ChallengeTarget(value="5", display="", kind="other")
    assert login._click_challenge_target(page, target) is False


# ============================================================
# Bot-defense challenge, and the self-diagnosing failure
# ============================================================

def test_a_captcha_is_recognised():
    # It cannot be answered from a terminal, so the only useful response is
    # to stop and name the verb that can.
    page = _SelectorPage({amexclient.SEL_CAPTCHA: [_Element()]})
    assert login.looks_like_captcha(page) is True


def test_no_captcha_on_a_plain_challenge_screen():
    page = _SelectorPage({amexclient.SEL_CHALLENGE_OPTION: [_Element()]})
    assert login.looks_like_captcha(page) is False


@pytest.mark.parametrize("shape", [
    "iframe[src*='recaptcha']",
    "iframe[src*='hcaptcha']",
    "[data-testid*='captcha' i]",
    "[class*='captcha' i]",
])
def test_the_captcha_selector_covers_the_common_widget_shapes(shape):
    # SEL_CAPTCHA is not pinned from a capture, so this is its only guard —
    # driven through looks_like_captcha rather than asserted against the
    # constant, which any string carrying the right words would satisfy.
    page = _SelectorPage({shape: [_Element()]})
    assert login.looks_like_captcha(page) is True


class _DescribePage:
    """A page whose locator answers the describe_screen probe."""

    def __init__(self, controls, url="https://global.americanexpress.com/x",
                 title="Example Title"):
        self._controls = controls
        self.url = url
        self._title = title

    def title(self):
        return self._title

    def locator(self, selector):
        return _Locator(self._controls)

    def wait_for_timeout(self, _ms):
        pass


class _Control(_Element):
    def __init__(self, ident=None, visible=True, attrs=None, text=""):
        super().__init__(text=text, present=True)
        self.visible = visible
        self.attrs = attrs or ({"data-testid": ident} if ident else {})

    def is_visible(self):
        return self.visible

    def get_attribute(self, name):
        return self.attrs.get(name)


def test_describe_screen_names_the_visible_controls():
    page = _DescribePage([_Control("otp-input-0"), _Control("continue-button")])
    out = login.describe_screen(page)
    assert "otp-input-0" in out and "continue-button" in out
    assert "path=/x" in out


def test_describe_screen_skips_invisible_and_anonymous_controls():
    page = _DescribePage([_Control("hidden-one", visible=False),
                          _Control(None), _Control("real-one")])
    out = login.describe_screen(page)
    assert "real-one" in out and "hidden-one" not in out


def test_describe_screen_carries_no_element_text():
    # A challenge screen's labels carry the masked destination, and this
    # string is written to be pasted into a chat. The text is threaded
    # through the stub's real accessors, so appending inner_text() to
    # describe_screen would fail this.
    page = _DescribePage([_Control(attrs={"data-testid": "opt"},
                                   text="Text Message ***0000")])
    assert "0000" not in login.describe_screen(page)


def test_describe_screen_survives_a_page_that_raises():
    class _Broken:
        @property
        def url(self):
            raise RuntimeError("navigating")

        def title(self):
            raise RuntimeError("navigating")

        def locator(self, _s):
            raise RuntimeError("navigating")
    # A diagnostic that throws while diagnosing is worse than useless.
    assert "controls=none" in login.describe_screen(_Broken())


# ============================================================
# Challenge screen: the six-box passcode entry
# ============================================================

def _otp_page(boxes, continue_btn=None):
    mapping = {amexclient.SEL_OTP_INPUT.format(i=i): [b]
               for i, b in enumerate(boxes)}
    mapping[amexclient.SEL_CONTINUE] = [continue_btn or _Element()]
    return _SelectorPage(mapping)


class _DistributingBox(_Element):
    """A box in a group that spreads a whole-code paste across its siblings,
    the way the real widget does. Filling box 0 with all six digits leaves
    one digit in each box; filling a single digit behaves normally."""

    def __init__(self, group, index):
        super().__init__()
        self.group, self.index = group, index

    def fill(self, value, timeout=None):
        self.fills.append(value)
        if len(value) == amexclient.OTP_DIGITS and self.index == 0:
            for box, digit in zip(self.group, value):
                box.value = digit
        else:
            self.value = value


def test_a_paste_into_the_first_box_that_distributes_is_accepted():
    # The widget accepts the whole code in box 0 and spreads it; the
    # read-back is what confirms it landed, and only then is Continue
    # clicked.
    group: list = []
    group.extend(_DistributingBox(group, i)
                 for i in range(amexclient.OTP_DIGITS))
    cont = _Element()
    assert login.enter_otp(_otp_page(group, cont), CODE) is True
    assert "".join(b.value for b in group) == CODE
    assert cont.clicks == 1
    # The paste is tried first, so no per-digit fill was needed.
    assert group[1].fills == [""]


def test_per_digit_entry_is_the_fallback():
    boxes = [_Element() for _ in range(amexclient.OTP_DIGITS)]
    cont = _Element()
    assert login.enter_otp(_otp_page(boxes, cont), CODE) is True
    assert "".join(b.value for b in boxes) == CODE
    assert cont.clicks == 1


def test_continue_is_not_clicked_when_the_code_did_not_land():
    # A partially filled control submits and is rejected — the failure the
    # first capture caught. Better to report it than to burn the passcode.
    boxes = [_Element(sticky=False) for _ in range(amexclient.OTP_DIGITS)]
    cont = _Element()
    assert login.enter_otp(_otp_page(boxes, cont), CODE) is False
    assert cont.clicks == 0


def test_the_boxes_are_cleared_before_every_attempt():
    boxes = [_Element(sticky=False) for _ in range(amexclient.OTP_DIGITS)]
    login.enter_otp(_otp_page(boxes), CODE)
    # Each attempt clears first, and a final clear runs on give-up, so a
    # retry never types onto a rejected code.
    assert boxes[0].fills.count("") >= 2
    assert boxes[0].fills[-1] == ""


def test_a_drifted_box_count_fails_cleanly():
    boxes = [_Element() for _ in range(3)]
    assert login.enter_otp(_otp_page(boxes), CODE) is False


# ============================================================
# authenticate(): the branches a live run cannot be relied on to reach
# ============================================================
# The watch is pre-loaded, so the outcome loop breaks on its first tick and
# the 120s ceiling is never approached.

def test_an_unattended_run_surfaces_a_challenge_as_needs_login():
    # `--no-cli-mfa` / no TTY: nobody is there to answer a passcode, so the
    # run must fail loudly rather than block on a prompt (DESIGN.md §L).
    watch = _watch_with(200, {"statusCode": 1, "errorCode": "LGON013",
                              "reauth": {"mfaId": "abc"}})
    with pytest.raises(login.NeedsLogin):
        login.authenticate(None, _SelectorPage({}), watch,
                           two_factor=login.TWOFACTOR_NONE)


def test_an_outright_refusal_carries_the_providers_own_words():
    # A refusal is surfaced verbatim, never reinterpreted.
    watch = _watch_with(200, {"statusCode": 1, "errorCode": "LGON999",
                              "errorMessage": "example refusal"})
    with pytest.raises(login.LogonFailed) as raised:
        login.authenticate(None, _SelectorPage({}), watch,
                           two_factor=login.TWOFACTOR_NONE)
    assert "LGON999" in str(raised.value)
    assert "example refusal" in str(raised.value)


def test_a_verdictless_logon_response_says_so_rather_than_blaming_the_login():
    watch = _watch_with(500, None)
    with pytest.raises(login.LogonFailed) as raised:
        login.authenticate(None, _SelectorPage({}), watch,
                           two_factor=login.TWOFACTOR_NONE)
    assert "no statusCode" in str(raised.value)


def test_a_trusted_device_authenticates_with_no_challenge_in_any_mode():
    watch = _watch_with(200, {"statusCode": 0, "challenge": False,
                              "reauth": {"trust": True}})
    assert login.authenticate(None, _SelectorPage({}), watch,
                              two_factor=login.TWOFACTOR_NONE) is True


# ============================================================
# The terminal challenge drive, and the device registration it ends with
# ============================================================

def _challenge_page(*, option=None, register=None, boxes=None,
                    continue_btn=None):
    """A passcode challenge screen: the six code boxes and Continue, plus
    optionally a delivery option and the device-registration control."""
    boxes = boxes if boxes is not None else [
        _Element() for _ in range(amexclient.OTP_DIGITS)]
    mapping = {amexclient.SEL_OTP_INPUT.format(i=i): [b]
               for i, b in enumerate(boxes)}
    mapping[amexclient.SEL_CONTINUE] = [continue_btn or _Element()]
    if option is not None:
        mapping[amexclient.SEL_CHALLENGE_OPTION] = [option]
    if register is not None:
        mapping[amexclient.SEL_REGISTER_DEVICE] = [register]
    return _SelectorPage(mapping), boxes


def test_the_terminal_drive_picks_sends_reads_and_registers(monkeypatch):
    option = _Element(text="Text Message  *******0000")
    register = _Element()
    page, boxes = _challenge_page(option=option, register=register)
    watch = _watch_with(200, {"statusCode": 1, "reauth": {"mfaId": "abc"}})
    order: list[str] = []
    real_reset = watch.reset
    monkeypatch.setattr(watch, "reset",
                        lambda: (order.append("reset"), real_reset())[1])
    monkeypatch.setattr(login.auth_dialog, "read_otp",
                        lambda: (order.append("read_otp"), CODE)[1])

    assert login._drive_challenge_cli(page, watch, None) is True
    # A single destination auto-picks, and it is clicked once.
    assert option.clicks == 1
    # The first logon's verdict is dropped BEFORE the code is read: the
    # challenge ends with a second logon call, and that is the one that
    # decides.
    assert order == ["reset", "read_otp"]
    assert "".join(b.value for b in boxes) == CODE
    # And the device is registered while the run is there — without it every
    # later run pays a passcode on a source whose budget is the scarce thing.
    assert register.clicks == 1


def test_the_terminal_drive_aborts_on_a_captcha(monkeypatch):
    # No terminal can answer one, so the drive must stop before it prompts.
    monkeypatch.setattr(login.auth_dialog, "read_otp",
                        lambda: pytest.fail("prompted on a captcha screen"))
    monkeypatch.setattr(
        login.auth_dialog, "choose_target",
        lambda *a, **k: pytest.fail("picked a target on a captcha screen"))
    page = _SelectorPage({amexclient.SEL_CAPTCHA: [_Element()]})
    watch = _watch_with(200, {"statusCode": 1, "reauth": {"mfaId": "abc"}})
    assert login._drive_challenge_cli(page, watch, None) is False


def test_a_missing_registration_control_reports_what_was_on_screen(caplog):
    # §M: the two reasons a registration can be missing — a drifted selector
    # and a provider that did not offer it — want different responses, so the
    # miss names the controls that WERE on screen.
    control = _Control("continue-button")
    page = _SelectorPage({"button, input, [data-testid]": [control]})
    with caplog.at_level(logging.WARNING, logger="amex.login"):
        login.register_device(page, timeout_s=0)
    assert control.clicks == 0
    assert "no device-registration control" in caplog.text
    assert "continue-button" in caplog.text


# ============================================================
# The authenticated probe
# ============================================================

class _RequestContext:
    def __init__(self, status, body):
        self.status, self.body, self.calls = status, body, []

    def post(self, url, headers=None, data=None):
        self.calls.append((url, headers, data))
        return _Resp(url, self.status, self.body)


class _ProbeContext:
    def __init__(self, status, body):
        self.request = _RequestContext(status, body)


def test_probe_authenticated_is_true_on_a_200_payload():
    ctx = _ProbeContext(200, {"products": {"data": {"products": {}}}})
    assert login.probe_authenticated(ctx) is True
    url, headers, _ = ctx.request.calls[0]
    assert url.endswith(amexclient.FN_CUSTOMER_OVERVIEW[0])
    assert headers["ce-source"] == amexclient.FN_CUSTOMER_OVERVIEW[1]


def test_probe_authenticated_is_false_on_401():
    # The signed-out answer, measured before the sign-in (DESIGN.md §G).
    assert login.probe_authenticated(_ProbeContext(401, None)) is False


def test_probe_authenticated_is_false_on_an_empty_200():
    assert login.probe_authenticated(_ProbeContext(200, {})) is False


# ============================================================
# Device trust — what the whole verb split rests on
# ============================================================

TRUST_COOKIE_ROW = (".americanexpress.com", amexclient.DEVICE_TRUST_COOKIE,
                    "SYNTHETIC", "/", 2000000000)


def _cookie_jar(profile_dir: Path, rows) -> None:
    """A synthetic Firefox cookie jar carrying the columns the probe reads."""
    profile_dir.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(profile_dir / login.COOKIE_DB))
    conn.execute("CREATE TABLE moz_cookies (id INTEGER PRIMARY KEY, "
                 "host TEXT, name TEXT, value TEXT, path TEXT, "
                 "expiry INTEGER)")
    conn.executemany("INSERT INTO moz_cookies (host, name, value, path, "
                     "expiry) VALUES (?,?,?,?,?)", rows)
    conn.commit()
    conn.close()


def _check(profile_dir: Path) -> int:
    return login.run_check(argparse.Namespace(profile_dir=profile_dir,
                                              check=True))


def test_the_check_reads_the_trust_cookie_off_the_profile(tmp_path):
    profile = tmp_path / "amex-profile"
    _cookie_jar(profile, [("www.example.com", "session", "x", "/", 0),
                          TRUST_COOKIE_ROW])
    assert login.profile_trust_cookie(profile)["value"] == "SYNTHETIC"
    assert login.profile_trust_cookie(profile)["expires"] == 2000000000
    assert _check(profile) == 0


def test_a_millisecond_expiry_is_normalised_to_seconds(tmp_path):
    # Read as seconds a millisecond column lands tens of thousands of years
    # out, and the reported "until" date with it.
    profile = tmp_path / "amex-profile"
    _cookie_jar(profile, [TRUST_COOKIE_ROW[:4] + (2000000000000,)])
    assert login.profile_trust_cookie(profile)["expires"] == 2000000000


@pytest.mark.parametrize("rows", [
    [],
    [(".americanexpress.com", amexclient.DEVICE_TRUST_COOKIE, "", "/", 0)],
    [(".example.com", amexclient.DEVICE_TRUST_COOKIE, "SYNTHETIC", "/", 0)],
    [("evil-americanexpress.com", amexclient.DEVICE_TRUST_COOKIE,
      "SYNTHETIC", "/", 0)],
])
def test_an_absent_valueless_or_foreign_trust_cookie_is_not_trust(tmp_path,
                                                                  rows):
    # A cookie cleared to "" is the shape a revoked one takes in the jar, and
    # a same-named cookie on another host is not this device's trust — nor is
    # one on a lookalike host that merely ENDS in the brand's domain, which
    # an unanchored suffix match would read as registered.
    profile = tmp_path / "amex-profile"
    _cookie_jar(profile, rows)
    assert login.profile_trust_cookie(profile) is None
    assert _check(profile) == 1


def test_the_check_creates_nothing_when_there_is_no_profile(tmp_path):
    # It must not chmod, relink or create anything it did not find.
    profile = tmp_path / "absent-profile"
    assert _check(profile) == 1
    assert not profile.exists()


def test_the_check_never_opens_a_browser(tmp_path, monkeypatch):
    # The probe's whole claim: no sign-in, and no network either. Launching
    # the shared context resolves the egress IP over the network, so opening
    # a browser at all would break it (DESIGN.md §L).
    fake = type(sys)("camoufox.sync_api")

    def _explode(**kwargs):
        raise AssertionError("the device-trust probe launched a browser")
    fake.Camoufox = _explode
    monkeypatch.setitem(sys.modules, "camoufox.sync_api", fake)
    profile = tmp_path / "amex-profile"
    _cookie_jar(profile, [TRUST_COOKIE_ROW])
    assert _check(profile) == 0


# ============================================================
# --fresh sets the profile aside; it never deletes it
# ============================================================

def test_fresh_moves_the_profile_aside_rather_than_deleting_it(monkeypatch,
                                                               tmp_path):
    # Losing a profile costs a real passcode, so --fresh must be undoable.
    profile = tmp_path / "amex-profile"
    profile.mkdir()
    (profile / "cookies.sqlite").write_text("synthetic")

    monkeypatch.setattr(login.launch, "prepare_profile_dir", lambda p: None)

    class _Boom(Exception):
        pass

    def _explode(**kwargs):
        raise _Boom
    monkeypatch.setitem(sys.modules, "camoufox.sync_api",
                        type(sys)("camoufox.sync_api"))
    sys.modules["camoufox.sync_api"].Camoufox = _explode

    with pytest.raises(_Boom):
        with login.camoufox(profile, fresh=True):
            pass

    aside = [p for p in tmp_path.iterdir() if p.name.startswith(
        "amex-profile.pre-fresh-")]
    assert len(aside) == 1, "the profile was not set aside"
    assert (aside[0] / "cookies.sqlite").read_text() == "synthetic"
    assert not (profile / "cookies.sqlite").exists()


# ============================================================
# A captured DOM never carries the passcode
# ============================================================

def test_the_passcode_boxes_are_blanked_out_of_a_capture():
    # The six boxes are plain text inputs holding one digit each, so neither
    # scrub_dom's password rule nor a value-based masker can reach them.
    markup = "".join(
        f"<input data-testid='otp-input-{i}' type='text' value='{d}'>"
        for i, d in enumerate(CODE))
    out = login.blank_otp(markup)
    assert "value=\"\"" in out
    for d in CODE:
        assert f"value='{d}'" not in out


def test_blanking_the_passcode_leaves_every_other_input_alone():
    markup = "<input data-testid='eliloUserID' type='text' value='keepme'>"
    assert login.blank_otp(markup) == markup
    assert login.blank_otp("") == ""


def test_prefill_asks_explore_to_overwrite(monkeypatch):
    """`_prefill` exists to pass overwrite=True: on a trusted device the
    form arrives carrying a MASKED user id, which submits successfully
    only while the trust holds (DESIGN.md §G). A dropped keyword here is
    silent — the sign-in keeps working until the device is forgotten."""
    seen = {}

    def _spy(page, username, password, filled, *, overwrite=False):
        seen["overwrite"] = overwrite
        return True

    monkeypatch.setattr(login, "_maybe_prefill_login", _spy)
    assert login._prefill(object(), "EXAMPLEUSER", "EXAMPLEPASS")
    assert seen["overwrite"] is True
