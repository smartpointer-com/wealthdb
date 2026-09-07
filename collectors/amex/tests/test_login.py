"""Unit tests for login.py's browserless surface: argument parsing, the
logon-outcome plumbing, the challenge-screen reading, and the six-box
passcode entry — the last driven against stub Playwright objects, so no
browser is needed. The live flow is validated separately.

Synthetic values only.
"""
from __future__ import annotations

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


@pytest.mark.parametrize("flag", ["--fresh", "--cli-mfa", "--no-cli-mfa",
                                  "--state-path", "--mfa-timeout"])
def test_the_sign_in_flags_moved_to_download(flag):
    # Leaving them parsing here would let a caller think login still signs in.
    with pytest.raises(SystemExit):
        login.parse_args(["--check", flag, "x"])


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


def test_a_page_whose_url_raises_is_treated_as_unknown():
    class _Broken:
        @property
        def url(self):
            raise RuntimeError("navigating")
    assert login._url(_Broken()) == ""


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

    def click(self, timeout=None):
        self.clicks += 1

    def fill(self, value, timeout=None):
        self.fills.append(value)
        if self.sticky or value == "":
            self.value = value

    def input_value(self, timeout=None):
        return self.value


class _SelectorPage:
    """A page that resolves selectors from a dict."""

    def __init__(self, mapping):
        self.mapping = mapping
        self.url = "https://global.americanexpress.com/dashboard"

    def locator(self, selector):
        return _Locator(self.mapping.get(selector, []))

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

class _CaptchaElement(_Element):
    pass


def test_a_captcha_is_recognised():
    # It cannot be answered from a terminal, so the only useful response is
    # to stop and name the verb that can.
    page = _SelectorPage({amexclient.SEL_CAPTCHA: [_Element()]})
    assert login.looks_like_captcha(page) is True


def test_no_captcha_on_a_plain_challenge_screen():
    page = _SelectorPage({amexclient.SEL_CHALLENGE_OPTION: [_Element()]})
    assert login.looks_like_captcha(page) is False


def test_the_captcha_selector_covers_the_common_widget_shapes():
    sel = amexclient.SEL_CAPTCHA
    for shape in ("recaptcha", "hcaptcha", "data-testid", "iframe"):
        assert shape in sel


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
    def __init__(self, ident=None, visible=True, attrs=None):
        super().__init__(present=True)
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
    # string is written to be pasted into a chat.
    page = _DescribePage([_Control(attrs={"data-testid": "opt",
                                          "text": "Text Message ***1234"})])
    assert "1234" not in login.describe_screen(page)


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

class _StubContext:
    """Just enough of a Playwright context to answer `cookies()`."""

    def __init__(self, cookies):
        self._cookies = cookies

    def cookies(self):
        return self._cookies


def test_the_trust_cookie_is_found_by_name():
    cookie = {"name": amexclient.DEVICE_TRUST_COOKIE, "value": "SYNTHETIC",
              "expires": 2000000000}
    assert login.device_trust_cookie(_StubContext([
        {"name": "session", "value": "x"}, cookie])) == cookie


def test_a_valueless_trust_cookie_does_not_count_as_trust():
    # A cookie cleared to "" is the shape a revoked/expired one takes in the
    # jar; reading it as trust would report a device that will be challenged.
    assert login.device_trust_cookie(_StubContext(
        [{"name": amexclient.DEVICE_TRUST_COOKIE, "value": ""}])) is None


def test_no_trust_cookie_at_all_is_not_trust():
    assert login.device_trust_cookie(_StubContext([])) is None
    assert login.device_trust_cookie(_StubContext(
        [{"name": "session", "value": "x"}])) is None


def test_a_context_that_raises_reports_no_trust_rather_than_failing():
    class _Broken:
        def cookies(self):
            raise RuntimeError("context closed")
    assert login.device_trust_cookie(_Broken()) is None


def test_run_check_reports_registration_without_touching_the_network(
        monkeypatch, tmp_path):
    # The probe's whole point: it answers from the profile, so it costs no
    # sign-in (DESIGN.md §L). If it ever navigates, this test's stub has no
    # page to navigate with and the call fails.
    import argparse
    import contextlib as _ctx

    for cookies, expected in (
            ([{"name": amexclient.DEVICE_TRUST_COOKIE, "value": "SYNTHETIC",
               "expires": 2000000000}], 0),
            ([], 1)):
        @_ctx.contextmanager
        def _camoufox(profile_dir, fresh=False, _c=cookies):
            assert fresh is False, "the probe must never move the profile"
            yield _StubContext(_c), None
        monkeypatch.setattr(login, "camoufox", _camoufox)
        args = argparse.Namespace(profile_dir=tmp_path, check=True)
        assert login.run_check(args) == expected


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
