#!/usr/bin/env python3
"""
Fidelity client-web session minter.

Drives Camoufox (a stealth-patched Firefox fork) through fidelity.com
login. Camoufox runs headed against an Xvfb virtual display managed
by entrypoint.sh.

Default mode pre-fills the username and password from
FIDELITY_USERNAME / FIDELITY_PASSWORD env vars (or
/secrets/fidelity-web.env), clicks Log In, waits for the TOTP-style
2FA challenge, prompts the operator on stdin for the security code,
fills, submits, then exits — leaving the persistent Firefox profile
dir at --profile-dir holding the cookies + Akamai trust state for
subsequent runs.

Modes:
  --check      open the profile dir, navigate to the post-auth
               landing URL to verify the session is alive. No
               credential submit, no 2FA, no MFA push.
  --vnc        pre-fill the credentials, then HAND OFF to the
               operator via VNC (x11vnc on 127.0.0.1:5900,
               started by entrypoint.sh). The operator clicks Log
               In and completes 2FA manually in their VNC client.
               The script polls for the post-auth URL and exits
               once it lands. Used to put a real human click +
               keyboard event into Akamai's behavioural-detection
               stream — useful when the rung-3 CLI-MFA flow gets
               flagged at credential-submit despite an otherwise
               clean fingerprint stack.
  (default)    full credential + CLI-MFA flow, script-driven end
               to end.

Anti-bot posture: rung 3 of DESIGN.md §6 — Camoufox with
`os="macos"` mode, the stack that schwab-web-dump confirmed works
against Akamai-protected signin flows. Rungs 1 (vanilla Playwright
Chromium) and 2 (stealth-patched Chromium) are skipped because the
Akamai parallel with schwab-web-dump is strong enough that the
expected per-rung outcome is "still blocked, IP-counter incremented
for nothing".

Single-attempt by design. Repeated failed credential submits risk
both Akamai blacklisting AND Fidelity-side account lockout. Run
this exactly once per credential-config change.

Usage:
    login.py --profile-dir /secrets/fidelity-web-profile
             [--env-file /secrets/fidelity-web.env]
             [--check]
             [--screenshot-dir /debug/login]
             [--mfa-page-timeout 15]
             [--vnc]
             [-v]
"""

from __future__ import annotations

import argparse
import contextlib
import logging
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


log = logging.getLogger("fidelity-web-dump.login")

LANDMARK_TIMEOUT_MS = 60_000

PROFILE_DIR_MODE = 0o700

# Default env-file locations. The wrapper mounts ~/.secrets to
# /secrets inside the container, so /secrets/fidelity-web.env is
# canonical. We also look at $HOME/.secrets/fidelity-web.env for
# local-dev runs outside the container.
DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/fidelity-web.env"),
    Path.home() / ".secrets" / "fidelity-web.env",
)

USERNAME_ENV = "FIDELITY_USERNAME"
PASSWORD_ENV = "FIDELITY_PASSWORD"
# Credentials whose file value overrides anything inherited from the
# host env, to defeat the source-mangling-on-$ pitfall — see
# load_env_file docstring.
_CRED_OVERRIDE_VARS = (USERNAME_ENV, PASSWORD_ENV)

# Fidelity signin / landing URLs. Anchored to the DOM captures from
# 2026-05-24; will drift on a future Fidelity UI redesign.
SIGNIN_URL = "https://digital.fidelity.com/prgw/digital/signin/"
POST_AUTH_URL_PREFIX = "https://digital.fidelity.com/ftgw/digital/portfolio/"
POST_AUTH_LANDING = (
    "https://digital.fidelity.com/ftgw/digital/portfolio/summary"
)

# Login form selectors. Mirrors the captured DOM; see DESIGN.md §8.2.
# Username field: a <select> on returning devices ("remember my
# username" cookie present), a text <input> on first visit.
SEL_USERNAME_SELECT = "#dom-select-username"
SEL_USERNAME_OTHER_OPTION_VALUE = "default"
SEL_USERNAME_TEXT_INPUT_CANDIDATES = (
    "input#userId-input",
    "input[autocomplete=username]",
    "input[name=userId]",
    "input[aria-labelledby=dom-username-label]",
)
SEL_PASSWORD_INPUT = "#dom-pswd-input"
SEL_LOGIN_BUTTON = "#dom-login-button"

# International Usage Agreement interstitial — Fidelity interposes
# this page for non-US-locale clients (Camoufox geoip=True makes us
# present as Swiss-locale, which trips it). User must accept before
# the signin form is served. The page is a static HTML at a
# different URL than the SPA; the accept link runs
# javascript:acceptAgreement() which then routes onward.
SEL_IUA_ACCEPT_CANDIDATES = (
    "a.accept-link",
    "a[title='I Accept']",
    "a:has-text('I Accept')",
)

# 2FA selectors.
SEL_2FA_CODE_INPUT = "#dom-totp-security-code-input"
SEL_2FA_SUBMIT_CANDIDATES = (
    "button[type=submit]:has-text('Submit')",
    "button[type=submit]:has-text('Continue')",
    "button[type=submit]:has-text('Verify')",
    "button[type=submit]",
)

# "Trust this browser" checkbox on the 2FA page. The input itself is
# a real <input type=checkbox> but PVD wraps it in a styled <label>
# whose ::before pseudo-element overlays the input and intercepts
# pointer events — clicking the input directly fails (Playwright's
# `.check()` retries for the full default 60s timeout). The fix is
# to click the LABEL element, which forwards focus to its `for=`
# input naturally. The id is confirmed `dom-trust-device-checkbox`
# from a captured 2FA page.
SEL_TRUST_BROWSER_CHECKBOX = "#dom-trust-device-checkbox"
SEL_TRUST_BROWSER_LABEL = "label[for='dom-trust-device-checkbox']"


def parse_args(argv):
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir",
        type=Path,
        default=Path("/secrets/fidelity-web-profile"),
        help=("Camoufox / Firefox persistent profile directory. "
              "Holds cookies, localStorage, and Akamai bot-manager "
              "trust state across runs. Default: "
              "/secrets/fidelity-web-profile (inside the container; "
              "the wrapper mounts ~/.secrets to /secrets)."),
    )
    p.add_argument(
        "--env-file",
        type=Path,
        default=None,
        help=("Path to a KEY=VALUE env file. When omitted, falls back "
              "to /secrets/fidelity-web.env then "
              "$HOME/.secrets/fidelity-web.env. Either resolution "
              "supplies FIDELITY_USERNAME / FIDELITY_PASSWORD."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Load the existing --profile-dir and navigate to the "
              "post-auth landing URL to verify the session is alive. "
              "No credential submit, no 2FA, no MFA push. Exits 0 "
              "if alive, non-zero otherwise."),
    )
    p.add_argument(
        "--trust-this-browser",
        action=argparse.BooleanOptionalAction, default=True,
        help=("Tick the 'Trust this browser' checkbox on the 2FA "
              "page so Fidelity suppresses MFA on subsequent runs "
              "from this profile dir. Default: True. Pass "
              "--no-trust-this-browser to leave it unchecked — "
              "useful when you want to exercise the CLI-MFA flow "
              "on every run (e.g. during selector debugging)."),
    )
    p.add_argument(
        "--vnc", action="store_true",
        help=("VNC-driven login: pre-fill the credentials, then "
              "WAIT for the operator to click Log In and complete "
              "2FA via a VNC client (x11vnc is started by "
              "entrypoint.sh on 127.0.0.1:5900 with a fresh "
              "single-use password printed at container startup). "
              "Script polls for the post-auth URL and exits once "
              "it lands."),
    )
    p.add_argument(
        "--vnc-wait-timeout", type=float, default=600.0,
        help=("In --vnc mode, seconds to wait for the operator to "
              "click Log In, complete 2FA, and land on the post-"
              "auth URL. Default: 600 (10 min). Generous because "
              "the human is in the loop end-to-end."),
    )
    p.add_argument(
        "--download-trigger", type=Path,
        default=Path("/data/.download-trigger"),
        help=("In keep-alive mode (default after a successful "
              "login), poll this file every 2s. When the file "
              "appears, parse KEY=VALUE config out of it, call "
              "download.walk() to scrape against the live Firefox "
              "session, then delete the file and keep polling. "
              "Used during development so the operator can iterate "
              "on scraping logic via repeated `./fidelity-web-dump "
              "download` calls without re-MFAing on every change. "
              "Pass --no-keep-alive to disable the loop and exit "
              "right after login."),
    )
    p.add_argument(
        "--keep-alive",
        action=argparse.BooleanOptionalAction, default=True,
        help=("Hold Firefox open after a successful login and poll "
              "--download-trigger for scrape requests. Default: True "
              "(matches the schwab-web-dump dev model — Fidelity's "
              "session is bound to the Firefox-process lifetime, "
              "so login + download must share one process). Pass "
              "--no-keep-alive to exit right after login (useful "
              "for `login --check`-style probes, or once we've "
              "collapsed to a single-shot download flow)."),
    )
    p.add_argument(
        "--mfa-page-timeout", type=float, default=15.0,
        help=("Seconds to wait for the 2FA challenge page to render "
              "after we click Log In. Default: 15. Either the 2FA "
              "input appears within this window or it doesn't — "
              "Fidelity does not lazy-load it further. This is NOT "
              "a wait on the operator typing the code; the stdin "
              "read after the prompt is blocking (Ctrl-C to abort)."),
    )
    p.add_argument(
        "--nav-timeout", type=float, default=60.0,
        help="Per-navigation timeout in seconds. Default 60.",
    )
    p.add_argument(
        "--screenshot-dir", type=Path, default=None,
        help=("If set, save an HTML dump + PNG screenshot at each "
              "navigation landmark. Diagnostic only. Never defaults "
              "to a path under /secrets/."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help=("Capture a Playwright trace bundle. Requires "
              "--screenshot-dir; trace lands there."),
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


# ============================================================
# Env-file loader (mirrors schwab-web-dump's pattern)
# ============================================================

def load_env_file(path):
    """Source KEY=VALUE pairs from `path` into os.environ.

    For credentials (FIDELITY_USERNAME / FIDELITY_PASSWORD) the file
    value wins over an already-set host env var, because the host
    shell's `source` does $-expansion on double-quoted values, which
    would silently mangle passwords containing $, !, backtick. The
    file itself, read byte-for-byte by us, has the original intact.
    Single-quoted values defeat the issue at the source.

    Other vars use setdefault (env-file is a fallback for those).

    Outer matching quotes (single OR double) are stripped. Lines
    starting with `#` and blank lines are ignored. Malformed lines
    raise.
    """
    log.debug("loading env file: %s", path)
    with path.open("r", encoding="utf-8") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):]
            if "=" not in line:
                raise SystemExit(
                    f"env file {path}:{lineno}: not KEY=VALUE: "
                    f"{raw.rstrip()!r}"
                )
            key, _, value = line.partition("=")
            key = key.strip()
            value = _strip_outer_quotes(value.strip())
            if not key:
                raise SystemExit(f"env file {path}:{lineno}: empty key")
            if key in _CRED_OVERRIDE_VARS:
                prior = os.environ.get(key)
                if prior is not None and prior != value:
                    log.warning(
                        "%s inherited from host env (len=%d) differs "
                        "from %s file value (len=%d); using file value. "
                        "(Use SINGLE quotes around values containing "
                        "$/!/backtick to avoid host `source` mangling.)",
                        key, len(prior), path, len(value),
                    )
                os.environ[key] = value
            else:
                os.environ.setdefault(key, value)


def _strip_outer_quotes(s):
    if len(s) >= 2 and s[0] == s[-1] and s[0] in ("'", '"'):
        return s[1:-1]
    return s


def maybe_source_env_files(args):
    if args.env_file is not None:
        if not args.env_file.exists():
            raise SystemExit(f"--env-file does not exist: {args.env_file}")
        load_env_file(args.env_file)
        return
    for path in DEFAULT_ENV_FILE_CANDIDATES:
        if path.exists():
            load_env_file(path)
            return


# ============================================================
# Profile-dir setup
# ============================================================

def prepare_profile_dir(profile_dir):
    """Create the user-data-dir if missing and chmod it 0700."""
    profile_dir.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(profile_dir, PROFILE_DIR_MODE)
    except OSError as exc:
        log.warning(
            "could not chmod %s to 0%o: %s",
            profile_dir, PROFILE_DIR_MODE, exc,
        )


# ============================================================
# Screenshot / trace helpers
# ============================================================

def ts_slug():
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def maybe_capture(page, screenshot_dir, label):
    """Save HTML + PNG at a navigation landmark. Never raises."""
    if screenshot_dir is None:
        return
    try:
        screenshot_dir.mkdir(parents=True, exist_ok=True)
    except Exception as e:
        log.warning("create screenshot dir %s: %s", screenshot_dir, e)
        return
    ts = ts_slug()
    try:
        html_path = screenshot_dir / f"{ts}-{label}.html"
        try:
            html = page.content()
        except Exception:
            html = page.evaluate(
                "() => document.documentElement.outerHTML"
            )
        html_path.write_text(html, encoding="utf-8")
        log.debug("wrote HTML %s", html_path)
    except Exception as e:
        log.warning("html capture %s failed: %s", label, e)
    try:
        png_path = screenshot_dir / f"{ts}-{label}.png"
        page.screenshot(
            path=str(png_path),
            full_page=False,
            timeout=5_000,
            animations="disabled",
        )
        log.debug("wrote screenshot %s", png_path)
    except Exception as e:
        log.debug("screenshot %s failed (HTML saved): %s", label, e)


def stop_trace_if_active(context, trace, screenshot_dir, label):
    if not trace:
        return
    if screenshot_dir is None:
        log.warning("--trace without --screenshot-dir; trace discarded")
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    trace_path = screenshot_dir / f"{ts_slug()}-{label}-trace.zip"
    try:
        context.tracing.stop(path=str(trace_path))
        log.info("trace saved to %s", trace_path)
    except Exception as e:
        log.warning("stop trace failed: %s", e)


# ============================================================
# Camoufox launch
# ============================================================

@contextlib.contextmanager
def open_camoufox_context(profile_dir, trace):
    """Open Camoufox with a persistent profile dir, yielding the
    BrowserContext. The context auto-closes on exit.

    Camoufox is a stealth-patched Firefox fork that masks the
    fingerprint surfaces (canvas, WebGL, audio, fonts, navigator.*,
    TLS) Akamai's bot-scoring uses to detect Playwright-driven
    browsers. `os="macos"` runs the full macOS-pretend mode so the
    fingerprint is internally consistent — much stronger than the
    piecemeal UA + navigator.platform overrides you'd set on
    upstream Playwright Firefox.

    Headed + window=(1280, 800) avoids the mobile responsive layout
    Fidelity serves to narrow viewports; the Xvfb display from
    entrypoint.sh provides the X11 surface (camoufox respects
    DISPLAY when set).

    Same configuration as schwab-web-dump's open_camoufox_context.
    """
    from camoufox.sync_api import Camoufox
    with Camoufox(
        persistent_context=True,
        user_data_dir=str(profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
        # humanize: curved, variable-speed cursor movement on every
        # click. Camoufox routes Playwright's instant .click() calls
        # through a synthetic-cursor path that fires mousemove +
        # mousedown + mouseup events Akamai's bot-scoring can
        # observe. Not enabled in schwab-web-dump (it gets through
        # without). For Fidelity the static fingerprint stack alone
        # was not enough — the bot-block fired again with vanilla
        # rung-3 config — so we add the behavioural-mimicry layer.
        humanize=True,
        # geoip=True uses the camoufox[geoip] MaxMind DB bundled
        # at build time to derive the timezone, locale, lat/lon from
        # the egress IP autodetected at launch. Without this, Camoufox
        # defaults can mismatch the IP's geography (e.g. en-US locale
        # on a Swiss IP), which is a cheap signal for Akamai's
        # geo-anomaly heuristic. (Note: camoufox expects the literal
        # boolean True for autodetect, not the string "auto" — that
        # string gets handed straight to the IP-validator and raises
        # InvalidIP.)
        geoip=True,
    ) as context:
        context.set_default_navigation_timeout(LANDMARK_TIMEOUT_MS)
        context.set_default_timeout(LANDMARK_TIMEOUT_MS)
        if trace:
            context.tracing.start(
                screenshots=True, snapshots=True, sources=True,
            )
        yield context


def open_page(context):
    """Return a Page in the given context, reusing the existing one
    (Camoufox's persistent_context always opens one) or creating a
    fresh one if needed."""
    pages = context.pages
    if pages:
        return pages[0]
    return context.new_page()


# ============================================================
# Login-flow primitives
# ============================================================

def live_url(page):
    """Read the page's current URL by asking Firefox directly.

    Equivalent to `page.url` EXCEPT under camoufox 135 + playwright
    1.49: juggler doesn't reliably deliver `frameNavigated` events
    to the driver, so `page.url` stays stuck on the pre-redirect
    value after a top-level navigation completes (the Firefox UI
    URL bar updates, but Playwright's cached accessor doesn't).
    `location.href` via evaluate forces Firefox to answer from its
    actual document state, which is what we want for landmark
    detection (post-auth URL, post-IUA URL, etc.). Same workaround
    schwab-web-dump uses for the same camoufox + playwright pin.

    Returns the cached page.url on evaluation failure (page mid-
    navigation, page closing) so a transient error doesn't kill
    the polling loop.
    """
    try:
        result = page.evaluate("() => location.href")
        if isinstance(result, str) and result:
            return result
    except Exception:
        pass
    return page.url


def wait_for_signin_or_iua(page, timeout_s):
    """Poll for whichever lands first: the signin form (password
    input visible) or the International Usage Agreement
    interstitial (accept link visible). Returns ('signin', None),
    ('iua', locator), or (None, None) on timeout.

    Needed because `wait_until='domcontentloaded'` on page.goto
    returns before the SPA hydrates AND before Fidelity decides
    which page to render. With Camoufox `geoip=True` presenting as
    Swiss-locale, we get routed to the IUA before the signin form;
    with US-locale we'd get the form directly. Both cases need to
    be handled."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        for sel in SEL_IUA_ACCEPT_CANDIDATES:
            try:
                loc = page.locator(sel)
                if loc.count() > 0 and loc.first.is_visible(timeout=300):
                    return ("iua", loc.first)
            except Exception as e:
                log.debug("IUA probe (%s): %s", sel, e)
        try:
            pwd_loc = page.locator(SEL_PASSWORD_INPUT)
            if pwd_loc.count() > 0 and pwd_loc.first.is_visible(timeout=300):
                return ("signin", None)
        except Exception as e:
            log.debug("signin-form probe: %s", e)
        time.sleep(0.5)
    return (None, None)


def accept_iua(page, accept_loc):
    """Click the I Accept link on the International Usage Agreement
    interstitial. The link runs `javascript:acceptAgreement()` and
    routes onward to the signin form (possibly at a new URL)."""
    accept_loc.click()
    log.info("clicked I Accept on International Usage Agreement")


def fill_username(page, username):
    """Fill the username field, handling both the text-input form
    (fresh-device case) and the <select> dropdown form (returning-
    device, where Fidelity remembered the username). On dropdown:
    select 'Enter different username' and fill the text input that
    then appears."""
    sel_locator = page.locator(SEL_USERNAME_SELECT)
    if sel_locator.count() > 0:
        log.info(
            "username <select> present (returning-device path); "
            "selecting 'Enter different username'"
        )
        sel_locator.select_option(SEL_USERNAME_OTHER_OPTION_VALUE)
    for sel in SEL_USERNAME_TEXT_INPUT_CANDIDATES:
        loc = page.locator(sel)
        try:
            if loc.count() > 0 and loc.first.is_visible(timeout=2_000):
                loc.first.fill(username)
                log.info("filled username via %s", sel)
                return
        except Exception as e:
            log.debug("username candidate %s failed: %s", sel, e)
    fallback = page.locator(
        "input[type=text]:visible, input:not([type]):visible"
    )
    if fallback.count() > 0:
        fallback.first.fill(username)
        log.info("filled username via last-resort visible text input")
        return
    raise SystemExit(
        "could not locate the username input field. Check the "
        "captured HTML in --screenshot-dir for what Fidelity served."
    )


def fill_password(page, password):
    page.locator(SEL_PASSWORD_INPUT).fill(password)
    log.info("filled password")


def click_login(page):
    page.locator(SEL_LOGIN_BUTTON).click(timeout=LANDMARK_TIMEOUT_MS)
    log.info("clicked %s", SEL_LOGIN_BUTTON)


def wait_for_mfa_input_or_post_auth(page, timeout_s):
    """Poll for whichever appears first: the 2FA code input field
    (still needs the operator) or the post-auth URL (device was
    trusted, MFA skipped). Returns one of: ('mfa', locator),
    ('post_auth', None), or (None, None) on timeout."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        url = live_url(page)
        if url.startswith(POST_AUTH_URL_PREFIX):
            return ("post_auth", None)
        try:
            loc = page.locator(SEL_2FA_CODE_INPUT)
            if loc.count() > 0 and loc.first.is_visible(timeout=500):
                return ("mfa", loc.first)
        except Exception as e:
            log.debug("MFA probe error: %s", e)
        time.sleep(0.5)
    return (None, None)


def prompt_for_mfa_code():
    """Print a prompt to stderr and read a code from stdin. stderr
    is used so the prompt is visible even when stdout is redirected
    to a log file. Returns the stripped code; empty input returns
    ''. Blocking — Ctrl-C to abort."""
    sys.stderr.write("\n")
    sys.stderr.write("=" * 60 + "\n")
    sys.stderr.write(
        "Fidelity 2FA: enter your security code, then press Enter.\n"
    )
    sys.stderr.write("> ")
    sys.stderr.flush()
    try:
        code = sys.stdin.readline()
    except KeyboardInterrupt:
        sys.stderr.write("\n")
        raise
    sys.stderr.write("=" * 60 + "\n")
    sys.stderr.flush()
    return code.strip()


def ensure_trust_browser_checked(page):
    """Best-effort: tick the 'Trust this browser' checkbox so the
    device-trust cookie suppresses MFA on subsequent runs.

    PVD checkboxes wrap the <input type=checkbox> in a <label> whose
    styled ::before overlay intercepts pointer events on the input.
    Playwright's .check() retries the input click for the full
    default 60s timeout before giving up — burning a full minute of
    TOTP validity for nothing. The fix is to click the LABEL
    instead, which forwards the activation to its `for=` input
    natively. Bounded to 2s per attempt; non-fatal on failure.
    """
    try:
        cb = page.locator(SEL_TRUST_BROWSER_CHECKBOX)
        if cb.count() == 0:
            log.info("no Trust-Browser checkbox on this page")
            return
        if cb.is_checked():
            log.info("Trust-Browser checkbox already checked")
            return
        page.locator(SEL_TRUST_BROWSER_LABEL).click(timeout=2_000)
        log.info("ticked Trust-Browser checkbox via its label")
    except Exception as e:
        log.warning(
            "trust-browser tick failed (%s); proceeding anyway", e,
        )


def submit_mfa_code(page, code_locator, code):
    """Fill the MFA input and submit. Try button candidates; on
    no-match, press Enter inside the input."""
    code_locator.fill(code)
    for sel in SEL_2FA_SUBMIT_CANDIDATES:
        try:
            btn = page.locator(sel).first
            if btn.count() == 0:
                continue
            if not btn.is_visible(timeout=500):
                continue
            btn.click(timeout=10_000)
            log.info("submitted 2FA via %s", sel)
            return True
        except Exception as e:
            log.debug("2FA submit candidate %s failed: %s", sel, e)
    try:
        code_locator.press("Enter")
        log.info("submitted 2FA via Enter key in input")
        return True
    except Exception as e:
        log.error("could not press Enter to submit 2FA: %s", e)
        return False


def wait_for_post_auth_url(page, timeout_s):
    """Poll until the page URL falls under POST_AUTH_URL_PREFIX, or
    timeout. Returns True on success. Uses live_url() (location.href
    via evaluate) because page.url is stale under camoufox 135 +
    playwright 1.49 — see live_url docstring."""
    deadline = time.monotonic() + timeout_s
    last_logged = None
    while time.monotonic() < deadline:
        try:
            url = live_url(page)
            if url != last_logged:
                log.debug("waiting for post-auth; live URL: %s", url)
                last_logged = url
            if url.startswith(POST_AUTH_URL_PREFIX):
                log.info("post-auth landing reached: %s", url)
                return True
        except Exception as e:
            log.debug("url probe: %s", e)
        time.sleep(1.0)
    return False


# ============================================================
# Top-level flows
# ============================================================

def run_login(args):
    """Full credential + MFA flow against the Fidelity signin SPA.
    Returns 0 on success."""
    username = os.environ.get(USERNAME_ENV)
    password = os.environ.get(PASSWORD_ENV)
    if not username or not password:
        log.error(
            "%s and %s must be set (or sourced from --env-file / "
            "default env-file paths). See README.md.",
            USERNAME_ENV, PASSWORD_ENV,
        )
        return 2

    # NEVER log credentials. Log lengths for diagnostics.
    log.info(
        "creds loaded: %s (len=%d), %s (len=%d)",
        USERNAME_ENV, len(username),
        PASSWORD_ENV, len(password),
    )

    prepare_profile_dir(args.profile_dir)

    with open_camoufox_context(args.profile_dir, args.trace) as context:
        try:
            page = open_page(context)
            log.info("navigating to %s", SIGNIN_URL)
            page.goto(SIGNIN_URL, wait_until="domcontentloaded")
            maybe_capture(
                page, args.screenshot_dir, "01-initial-load",
            )

            # Two outcomes possible at this point:
            #   1. Fidelity served the signin SPA directly (US-locale
            #      headers) — password input renders after hydration.
            #   2. Fidelity routed us to the International Usage
            #      Agreement interstitial (non-US-locale, our case
            #      with Camoufox geoip=True). We must click "I Accept"
            #      to continue.
            kind, iua_loc = wait_for_signin_or_iua(page, timeout_s=30)
            if kind is None:
                maybe_capture(
                    page, args.screenshot_dir, "01b-load-timeout",
                )
                log.error(
                    "neither the signin form nor the IUA "
                    "interstitial rendered within 30s. The page "
                    "skeleton may be present but stuck in Akamai's "
                    "JS challenge, or Fidelity served a third page "
                    "type. Inspect the --screenshot-dir captures."
                )
                return 3
            if kind == "iua":
                log.info(
                    "International Usage Agreement interstitial "
                    "detected; clicking I Accept"
                )
                maybe_capture(
                    page, args.screenshot_dir, "01a-iua-page",
                )
                accept_iua(page, iua_loc)
                # The acceptAgreement() JS routes to the signin
                # form (may be a navigation, may be in-page DOM
                # swap). Wait for the password input.
                try:
                    page.locator(SEL_PASSWORD_INPUT).wait_for(
                        state="visible", timeout=30_000,
                    )
                except Exception as e:
                    maybe_capture(
                        page, args.screenshot_dir,
                        "01c-post-iua-no-form",
                    )
                    log.error(
                        "signin form did not appear within 30s "
                        "after IUA acceptance: %s", e,
                    )
                    return 3
                maybe_capture(
                    page, args.screenshot_dir,
                    "01d-signin-after-iua",
                )
            else:
                log.info("signin form rendered directly (no IUA)")
                maybe_capture(
                    page, args.screenshot_dir, "01e-signin-direct",
                )

            try:
                fill_username(page, username)
                fill_password(page, password)
            except Exception as e:
                maybe_capture(
                    page, args.screenshot_dir, "02-prefill-failed"
                )
                log.error("credential pre-fill failed: %s", e)
                return 3

            maybe_capture(page, args.screenshot_dir, "02-prefilled")

            if args.vnc:
                # VNC handoff: do NOT click Log In. The operator
                # connects via their VNC client and drives the
                # click + 2FA themselves, so Akamai's behavioural
                # check sees a real human-generated mousedown +
                # mouseup + keyboard event sequence.
                sys.stderr.write("\n")
                sys.stderr.write("=" * 60 + "\n")
                sys.stderr.write(
                    "READY: Camoufox is up and the login form is "
                    "pre-filled.\n"
                )
                sys.stderr.write(
                    "Connect with your VNC client now "
                    "(127.0.0.1:5900, password printed at the top "
                    "of this output by entrypoint.sh).\n"
                )
                sys.stderr.write(
                    "Then in the VNC window:\n"
                    "  1. Click 'Log in'.\n"
                    "  2. Complete the 2FA challenge.\n"
                    "  3. Wait for the post-auth landing page to "
                    "load.\n"
                )
                sys.stderr.write(
                    f"This script will detect the post-auth URL "
                    f"and exit automatically (up to "
                    f"{args.vnc_wait_timeout:.0f}s wait).\n"
                )
                sys.stderr.write("=" * 60 + "\n")
                sys.stderr.flush()

                if wait_for_post_auth_url(
                    page, timeout_s=args.vnc_wait_timeout,
                ):
                    maybe_capture(
                        page, args.screenshot_dir, "08-post-auth"
                    )
                    log.info(
                        "VNC-driven login complete; persistent "
                        "profile state is at %s",
                        args.profile_dir,
                    )
                    return 0
                maybe_capture(
                    page, args.screenshot_dir,
                    "vnc-post-auth-timeout",
                )
                log.error(
                    "did not see %s* within %.0fs after VNC handoff. "
                    "Operator may not have clicked Log In, the 2FA "
                    "may have failed, or Akamai blocked the submit "
                    "even with human-driven click. Inspect "
                    "--screenshot-dir captures.",
                    POST_AUTH_URL_PREFIX, args.vnc_wait_timeout,
                )
                return 5

            try:
                click_login(page)
            except Exception as e:
                maybe_capture(
                    page, args.screenshot_dir, "03-submit-failed"
                )
                log.error("login submit failed: %s", e)
                return 3

            maybe_capture(page, args.screenshot_dir, "03-submit-clicked")
            time.sleep(1.0)
            maybe_capture(
                page, args.screenshot_dir, "04-submit-settled"
            )

            log.info(
                "waiting up to %.0fs for the 2FA challenge page "
                "(or for direct post-auth landing if device is "
                "already trusted)",
                args.mfa_page_timeout,
            )
            kind, code_loc = wait_for_mfa_input_or_post_auth(
                page, timeout_s=args.mfa_page_timeout,
            )
            if kind == "post_auth":
                log.info(
                    "post-auth URL reached WITHOUT a 2FA challenge — "
                    "device-trust cookie must have been present"
                )
            elif kind == "mfa":
                maybe_capture(
                    page, args.screenshot_dir, "05-mfa-page"
                )
                if args.trust_this_browser:
                    ensure_trust_browser_checked(page)
                else:
                    log.info(
                        "--no-trust-this-browser: leaving the "
                        "checkbox unticked"
                    )
                code = prompt_for_mfa_code()
                if not code:
                    log.error("no 2FA code entered; aborting")
                    return 4
                if not submit_mfa_code(page, code_loc, code):
                    maybe_capture(
                        page, args.screenshot_dir,
                        "06-mfa-submit-failed",
                    )
                    return 4
                maybe_capture(
                    page, args.screenshot_dir, "06-mfa-submitted"
                )
                log.info(
                    "2FA submitted; waiting for post-auth landing URL"
                )
                if not wait_for_post_auth_url(
                    page, timeout_s=args.nav_timeout,
                ):
                    maybe_capture(
                        page, args.screenshot_dir,
                        "07-post-auth-timeout",
                    )
                    log.error(
                        "did not see %s* within %.0fs after 2FA submit; "
                        "Fidelity may have served another challenge "
                        "(security question, International Usage "
                        "Agreement gate, etc.) — see "
                        "--screenshot-dir captures",
                        POST_AUTH_URL_PREFIX, args.nav_timeout,
                    )
                    return 5
            else:
                maybe_capture(
                    page, args.screenshot_dir,
                    "05-mfa-timeout-or-block",
                )
                log.error(
                    "neither 2FA input nor post-auth URL within %.0fs. "
                    "Likely outcomes: anti-bot block (Akamai), "
                    "selectors drifted, or Fidelity served a "
                    "different challenge type. Inspect "
                    "--screenshot-dir captures.",
                    args.mfa_page_timeout,
                )
                return 6

            maybe_capture(page, args.screenshot_dir, "08-post-auth")

            log.info(
                "login complete; persistent profile state is at %s",
                args.profile_dir,
            )

            if args.keep_alive:
                return run_keep_alive_loop(context, page, args)
            return 0
        finally:
            stop_trace_if_active(
                context, args.trace, args.screenshot_dir, "login",
            )


def parse_trigger_file(path):
    """Parse a KEY=VALUE trigger file into a dict. Lines starting
    with `#` and blank lines are skipped. Malformed lines raise."""
    config = {}
    for lineno, raw in enumerate(path.read_text().splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            raise ValueError(
                f"trigger file {path}:{lineno}: "
                f"not KEY=VALUE: {raw!r}"
            )
        k, _, v = line.partition("=")
        config[k.strip()] = v.strip()
    return config


def run_keep_alive_loop(context, page, args):
    """After successful login, hold the Camoufox context open and
    poll the trigger file for download requests. When a trigger
    appears, parse it, call download.walk(), then delete it and
    keep polling. Ctrl-C exits cleanly (Camoufox's persistent
    context flushes the profile dir on the with-statement's exit).

    Required by Fidelity's session model — see
    [[fidelity-session-lifetime]] / DESIGN.md §5.4: the session is
    bound to the Firefox-process lifetime, so login + downloads
    must share one continuous Camoufox process. The trigger-file
    indirection lets the operator iterate on scraping logic via
    repeated `./fidelity-web-dump download` calls (which just
    write the trigger file from the host side) without paying
    for an MFA round on every change.
    """
    log.info(
        "keep-alive: holding Firefox open; polling %s every 2s. "
        "Ctrl-C to exit (Camoufox flushes profile dir cleanly).",
        args.download_trigger,
    )
    # Combined with -v $HERE:/app in the wrapper, this gives us a
    # hot-reload dev loop: edit download.py on the host, fire a
    # trigger, the next walk() call uses the fresh module — no
    # rebuild, no container restart, no MFA. Iteration on scraping
    # logic stays cheap.
    import importlib
    import download
    try:
        while True:
            time.sleep(2.0)
            if not args.download_trigger.exists():
                continue
            try:
                config = parse_trigger_file(args.download_trigger)
            except Exception as e:
                log.error(
                    "malformed trigger file (%s); deleting and "
                    "continuing", e,
                )
                args.download_trigger.unlink(missing_ok=True)
                continue
            log.info("trigger received: %s", config)
            args.download_trigger.unlink(missing_ok=True)
            try:
                importlib.reload(download)
                download.walk(context, page, config, args)
            except Exception:
                log.exception(
                    "download.walk failed; continuing keep-alive"
                )
    except KeyboardInterrupt:
        sys.stderr.write("\n")
        log.info("keep-alive: SIGINT, exiting")
        return 0


def run_check(args):
    """Validate the existing profile dir by navigating to the
    post-auth landing. Returns 0 if alive, non-zero otherwise."""
    if not args.profile_dir.exists():
        log.error("--profile-dir does not exist: %s", args.profile_dir)
        return 2

    prepare_profile_dir(args.profile_dir)
    with open_camoufox_context(args.profile_dir, args.trace) as context:
        try:
            page = open_page(context)
            log.info("navigating to %s", POST_AUTH_LANDING)
            try:
                page.goto(
                    POST_AUTH_LANDING,
                    wait_until="domcontentloaded",
                )
            except Exception as e:
                log.error("navigation failed: %s", e)
                return 3
            maybe_capture(page, args.screenshot_dir, "check-landed")
            time.sleep(2.0)
            url = live_url(page)
            if url.startswith(POST_AUTH_URL_PREFIX):
                log.info("session ALIVE — landed at %s", url)
                return 0
            log.warning(
                "session DEAD — landed at %s (expected prefix %s)",
                url, POST_AUTH_URL_PREFIX,
            )
            return 1
        finally:
            stop_trace_if_active(
                context, args.trace, args.screenshot_dir, "check",
            )


def main(argv):
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    if args.trace and args.screenshot_dir is None:
        log.error("--trace requires --screenshot-dir")
        return 2

    maybe_source_env_files(args)

    if args.check:
        return run_check(args)
    return run_login(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
