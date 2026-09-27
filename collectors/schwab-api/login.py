#!/usr/bin/env python3
"""
Schwab OAuth login helper.

Schwab access tokens last 30 minutes and refresh transparently from a
refresh token. Schwab refresh tokens last 7 days and CANNOT be renewed
programmatically — they require a fresh authorization-code grant through
a browser. This script drives that grant and writes the token
bundle to a file that `download.py` consumes.

The default flow opens Schwab's OAuth authorize page in a headed
Camoufox browser inside the container (the same anti-bot-resistant
browser the `schwab-web` collector uses), pre-fills the Schwab login
credentials, auto-submits, prompts for the 2FA code on stdin, drives the
Terms / account-selection / review consent pages (ticking every account
checkbox), captures the `?code=…` redirect to the callback URL, and
exchanges it for the token bundle. All browser activity can be traced
(--trace).

Modes:
  - default (--cli-mfa) Camoufox flow that auto-submits the login form,
                       prompts for the 2FA code on stdin, and drives the
                       consent / account-link pages. Used by `login`.
  - --no-cli-mfa       Camoufox flow you drive yourself over VNC — the
                       `vnc-login` subcommand uses this (fallback on UI
                       drift). The account checkboxes are still auto-ticked.
  - --manual           No browser: print the authorize URL, paste the
                       redirected URL back (schwab-py's manual flow).
  - --check            Inspect the existing token file's age. No browser,
                       no network.

Failure discipline (see DESIGN.md §3.1's incident note): one credential
submission and at most one 2FA submission per run; every page-advancing
click is classification-gated and budgeted; Schwab's terminal notice
pages (account lockout among them) stop the run immediately with the
page's own message. Re-running is the only retry.

Run this whenever `download.py` reports a refresh failure (the 7-day
window expired), or to mint the first token.
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import logging
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import oauth_landmarks as lm

from collectorkit import cli, debugcap, envfile, launch, session

log = logging.getLogger("schwab-login")

# Schwab refresh tokens are valid for 7 days from issue. Surfaced as a
# constant so --check output matches the actual cap.
REFRESH_TOKEN_TTL_SECONDS = 7 * 24 * 3600

# Credential env vars whose env-file value wins over an inherited host
# value (the host shell's `source` mangles $-containing values; see
# collectorkit.envfile.load_env_file). The OAuth app id/secret live in
# schwab-api.env; the Schwab web login id/password (reused to pre-fill
# the consent login) live in schwab-web.env.
_CRED_OVERRIDE_VARS = frozenset({
    "SCHWAB_CLIENT_ID", "SCHWAB_CLIENT_SECRET",
    "SCHWAB_LOGIN_ID", "SCHWAB_PASSWORD",
})

# Env files sourced (first existing path of each set, container then host
# form). schwab-api.env carries the OAuth app credentials; schwab-web.env
# carries the Schwab login id/password we pre-fill into the consent login.
_ENV_FILE_SETS = (
    (Path("/secrets/schwab-api.env"), Path.home() / ".secrets" / "schwab-api.env"),
    (Path("/secrets/schwab-web.env"), Path.home() / ".secrets" / "schwab-web.env"),
)


# ============================================================
# Arg parsing
# ============================================================

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--token-path", type=Path,
        default=Path("/secrets/schwab-api-token.json"),
        help="Path to read/write the OAuth token JSON file. "
             "Default: /secrets/schwab-api-token.json.",
    )
    p.add_argument(
        "--env-file", type=Path, default=None,
        help="Extra KEY=VALUE credentials env file, sourced last so its "
             "values win over the default schwab-api.env / schwab-web.env "
             "(also honours the SCHWAB_API_ENV_FILE env var).",
    )
    p.add_argument(
        "--client-id", default=None,
        help="Schwab OAuth Client ID (falls back to SCHWAB_CLIENT_ID).",
    )
    p.add_argument(
        "--client-secret", default=None,
        help="Schwab OAuth Client Secret (falls back to SCHWAB_CLIENT_SECRET). "
             "Prefer the env var over the command line.",
    )
    p.add_argument(
        "--callback-url", default=lm.DEFAULT_CALLBACK_URL,
        help=f"OAuth callback URL registered with your Schwab app "
             f"(default: {lm.DEFAULT_CALLBACK_URL}). Must match the portal "
             f"value exactly.",
    )
    p.add_argument(
        "--profile-dir", type=Path,
        default=Path("/secrets/schwab-api-oauth-profile"),
        help="Persistent Camoufox profile dir for the OAuth browser flow.",
    )
    p.add_argument(
        "--cli-mfa", dest="cli_mfa", action="store_true",
        help="Auto-submit the login form, prompt for the 2FA code on "
             "stdin, and drive the consent pages (default).",
    )
    p.add_argument(
        "--no-cli-mfa", dest="cli_mfa", action="store_false",
        help="Don't automate — drive login / 2FA / consent yourself over "
             "VNC (the `vnc-login` subcommand uses this).",
    )
    p.set_defaults(cli_mfa=True)
    p.add_argument(
        "--mfa-timeout", type=float, default=600.0,
        help="Seconds to wait for the human MFA + consent redirect to the "
             "callback URL (default 600 — time to complete login over VNC).",
    )
    p.add_argument(
        "--mfa-page-timeout", type=float, default=300.0,
        help="With --cli-mfa: seconds to wait for the 2FA input to appear.",
    )
    p.add_argument(
        "--screenshot-dir", type=Path, default=None,
        help="Write HTML/screenshots (and the trace, with --trace) here, "
             "plus a full DEBUG-level run.log mirror of the run's "
             "output. NEVER commit these — see AGENTS.md §4.",
    )
    p.add_argument(
        "--trace", action="store_true",
        help="Capture a Playwright trace bundle (requires --screenshot-dir). "
             "Opt-in — off by default. Note: the base image's Firefox and "
             "the pinned Playwright are incompatible on tracing.start(), so "
             "a trace currently crashes the browser mid-login.",
    )
    p.add_argument(
        "--explore", action="store_true",
        help="Debug aid: dump HTML+PNG of each distinct page during the "
             "wait (to --screenshot-dir) for mapping the OAuth flow / "
             "pinning the account-checkbox selectors.",
    )
    p.add_argument(
        "--capture-bodies", action="store_true",
        help="Debug aid: also save response bodies from the Schwab "
             "gateway / authorize hosts to --screenshot-dir (flow "
             "diagnosis). NEVER commit these — see AGENTS.md §4.",
    )
    mode = p.add_mutually_exclusive_group()
    mode.add_argument(
        "--manual", action="store_true",
        help="No browser: print the auth URL, paste the redirected URL back.",
    )
    mode.add_argument(
        "--check", action="store_true",
        help="Inspect the existing token file's age. No browser, no network.",
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


# ============================================================
# Env-file loading (mirrors schwab-web)
# ============================================================

def source_env_files(explicit: Path | None = None) -> None:
    """Source the first existing path of each env-file set (schwab-api
    then schwab-web), so the OAuth app creds and the Schwab login creds
    are both available. Absent files are fine. An explicit --env-file (or
    the SCHWAB_API_ENV_FILE env var) is then sourced last and wins."""
    for candidates in _ENV_FILE_SETS:
        for path in candidates:
            if path.exists():
                envfile.load_env_file(path, _CRED_OVERRIDE_VARS, logger=log)
                break
    # An explicit --env-file / ${SCHWAB_API}_ENV_FILE is sourced LAST so its
    # credential values win over the defaults.
    lead = explicit or (Path(os.environ["SCHWAB_API_ENV_FILE"])
                        if os.environ.get("SCHWAB_API_ENV_FILE") else None)
    if lead is not None:
        if not lead.exists():
            raise SystemExit(f"--env-file does not exist: {lead}")
        envfile.load_env_file(lead, _CRED_OVERRIDE_VARS, logger=log)


# Credential resolution (value-with-env-fallback) is
# collectorkit.envfile.resolve_credential; call sites use it directly.


# ============================================================
# Token-file inspection (--check)
# ============================================================

def read_token_creation_time(path: Path) -> datetime | None:
    try:
        with path.open("r", encoding="utf-8") as fh:
            blob = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        log.warning("Could not parse token file %s: %s", path, exc)
        return None
    ts = blob.get("creation_timestamp")
    if isinstance(ts, (int, float)):
        return datetime.fromtimestamp(ts, tz=timezone.utc)
    try:
        return datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc)
    except OSError:
        return None


def cmd_check(args: argparse.Namespace) -> int:
    if not args.token_path.is_file():
        print(f"No token file at {args.token_path}.")
        print("Run login without --check to mint one.")
        return 1
    issued = read_token_creation_time(args.token_path)
    if issued is None:
        print(f"Token file exists at {args.token_path}, "
              f"but its issue timestamp could not be determined.")
        return 2
    now = datetime.now(timezone.utc)
    age = now - issued
    remaining_s = REFRESH_TOKEN_TTL_SECONDS - age.total_seconds()
    print(f"Token file:        {args.token_path}")
    print(f"Issued (UTC):      {issued.isoformat(timespec='seconds')}")
    print(f"Age:               {age}")
    if remaining_s > 0:
        renew_by = datetime.fromtimestamp(
            issued.timestamp() + REFRESH_TOKEN_TTL_SECONDS, tz=timezone.utc,
        )
        print(f"Estimated expiry:  {renew_by.isoformat(timespec='seconds')} "
              f"({remaining_s / 3600:.1f}h from now)")
        if remaining_s / 3600 < 24:
            print("WARNING: refresh window expires in under 24 hours. "
                  "Re-run login soon.")
        return 0
    print("Estimated expiry:  EXPIRED. Re-run login to mint a new token.")
    return 3


# ============================================================
# Browser helpers (adapted from schwab-web)
# ============================================================

def ts_slug() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


@contextlib.contextmanager
def open_camoufox_context(profile_dir: Path, trace: bool):
    """Open Camoufox (stealth-patched Firefox) with a persistent profile,
    headed (Schwab flags headless), on the Xvfb display the entrypoint
    provides. Mirrors schwab-web's launch.

    The close is guarded: when the body fails because the browser itself
    died (startup crash, closed window), Playwright's own cleanup raises
    TargetClosedError `from None`, which would replace the body's
    exception — the actual diagnosis — with a generic close error. A
    cleanup failure is logged instead, never raised."""
    from camoufox.sync_api import Camoufox
    cm = Camoufox(
        persistent_context=True,
        user_data_dir=str(profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
        firefox_user_prefs=launch.firefox_prefs(),
    )
    context = cm.__enter__()
    try:
        if trace:
            # snapshots=False while a sign-in is on screen: a trace's DOM
            # snapshots record every input's value, a hand-typed password
            # included, and nothing can redact a trace after the fact. The
            # per-action screenshots still show the flow (the browser draws
            # a password field as dots) and the network records are the
            # redacted ones.
            context.tracing.start(screenshots=True, snapshots=False,
                                  sources=True)
        yield context
    finally:
        try:
            cm.__exit__(None, None, None)
        except Exception as exc:
            log.warning("browser cleanup failed (%s: %s) — the browser "
                        "likely crashed or was closed; the propagating "
                        "error above is the real cause",
                        type(exc).__name__, exc)


def stop_trace_if_active(context, trace: bool, screenshot_dir: Path | None,
                         label: str) -> None:
    if not trace:
        return
    if screenshot_dir is None:
        log.warning("--trace without --screenshot-dir; trace discarded")
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    trace_path = screenshot_dir / f"{ts_slug()}-{label}-trace.zip"
    context.tracing.stop(path=str(trace_path))
    log.info("trace saved to %s", trace_path)


def _live_url(page) -> str:
    """page.url can lag behind a client-side redirect; read it live."""
    try:
        return page.evaluate("() => window.location.href")
    except Exception:
        return page.url


def maybe_capture_html(page, screenshot_dir: Path | None, label: str) -> None:
    if screenshot_dir is None:
        return
    try:
        screenshot_dir.mkdir(parents=True, exist_ok=True)
        ts = ts_slug()
        # Scrubbed: this is called on the sign-in page after prefill, so a
        # raw serialization would persist the typed password.
        (screenshot_dir / f"{ts}-{label}.html").write_text(
            debugcap.scrub_dom(page.content()), encoding="utf-8")
        try:
            page.screenshot(path=str(screenshot_dir / f"{ts}-{label}.png"),
                            full_page=False, timeout=3_000,
                            animations="disabled")
        except Exception as e:
            log.debug("screenshot %s failed (HTML saved): %s", label, e)
    except Exception as e:
        log.debug("capture %s failed: %s", label, e)


def _fill_first(scope, candidates, value: str) -> bool:
    """Fill the first matching/visible candidate selector in `scope`
    (a page or frame). Returns True on success."""
    for sel in candidates:
        try:
            loc = scope.locator(sel).first
            if loc.count() == 0 or not loc.is_visible(timeout=500):
                continue
            loc.fill(value)
            return True
        except Exception:
            continue
    return False


def prefill_login(page, login_id: str | None, password: str | None,
                  timeout_s: float = 25.0, poll_s: float = 0.5) -> None:
    """Best-effort pre-fill of the Schwab login form on the authorize
    page. The exact DOM is uncharted (the consent login may differ from
    schwab.com's) and the form may only appear after a redirect, so this
    polls for a login-id input across the top-level page and every frame,
    fills it plus the password, and is non-fatal — a miss leaves the creds
    to be typed by hand over VNC."""
    if not login_id or not password:
        log.info("login credentials not set (SCHWAB_LOGIN_ID / "
                 "SCHWAB_PASSWORD); skipping pre-fill")
        return
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        scopes = [page, *[f for f in page.frames
                          if f.parent_frame is not None]]
        for scope in scopes:
            if _fill_first(scope, lm.LOGIN_ID_INPUT_CANDIDATES, login_id):
                # Password may share the page or be a later step; fill it
                # if present, but a found login id alone counts as success.
                _fill_first(scope, lm.PASSWORD_INPUT_CANDIDATES, password)
                log.info("login form pre-filled")
                return
        time.sleep(poll_s)
    log.warning("could not locate the login form to pre-fill within %ss; "
                "type the credentials over VNC", timeout_s)


def _prompt_for_mfa_code() -> str:
    return cli.prompt_on_stderr(
        "Schwab 2FA: enter your code, then press Enter.")


class LoginFlowError(RuntimeError):
    """Terminal login-flow failure: the run stops, reports, and exits
    nonzero. Re-running login is the recovery path — there are no in-run
    retries (see DESIGN.md §3.1's incident note)."""

    exit_code = 8

    def __init__(self, message: str, *, page_text: str = "",
                 locked: bool = False):
        super().__init__(message)
        self.page_text = page_text
        self.locked = locked


class TerminalPageError(LoginFlowError):
    """The browser landed on a page the flow must never interact with —
    the gateway's terminal notice route (account lockout among them)."""

    exit_code = 7


def _visible_page_text(scope, limit: int = 600) -> str:
    """Visible body text of a page or frame, whitespace-collapsed and
    capped — the provider's own words for terminal error reports."""
    try:
        text = scope.evaluate(
            "() => (document.body && document.body.innerText) || ''")
        return " ".join(str(text).split())[:limit]
    except Exception:
        return ""


# Innermost visible error/alert text. Mirrors schwab-web's extractor:
# role=alert regions preferred over the generic aria-live/error-classed
# fallback; containers of other matches dropped so one blob doesn't
# swallow its children.
_ERROR_TEXT_JS = """() => {
    const grab = (sel) => {
        const els = Array.from(document.querySelectorAll(sel))
            .filter(el => el.offsetWidth || el.offsetHeight);
        return els
            .filter(el => !els.some(o => o !== el && el.contains(o)))
            .map(el => el.textContent.replace(/\\s+/g, ' ').trim())
            .filter(t => t && t.length <= 300);
    };
    return {
        alerts: grab('[role="alert"]'),
        other: grab('[aria-live], [class*="error" i], [class*="alert" i]'),
    };
}"""


def _visible_error_text(page) -> str:
    """Visible error text across the page and its frames, one message
    per line — Schwab's own words, logged verbatim on a rejection."""
    texts: list[str] = []
    scopes = [page, *[f for f in page.frames
                      if f.parent_frame is not None]]
    for scope in scopes:
        try:
            found = scope.evaluate(_ERROR_TEXT_JS)
        except Exception:
            continue
        texts.extend(found.get("alerts") or found.get("other") or [])
    return "\n  - ".join(list(dict.fromkeys(texts))[:5])


def _notice_scope(page):
    """The page or frame currently on the gateway's terminal notice
    route, or None."""
    if lm.is_gateway_notice_url(_live_url(page)):
        return page
    try:
        frames = [f for f in page.frames if f.parent_frame is not None]
    except Exception:
        frames = []
    for f in frames:
        if lm.is_gateway_notice_url(f.url or ""):
            return f
    return None


def _notice_message(scope, settle_s: float = 8.0, poll_s: float = 0.5) -> str:
    """The notice page's own message, read from its message container
    (lm.NOTICE_MESSAGE_SELECTORS), waiting for the SPA to render it —
    the content arrives via an async fetch seconds after the route
    change, and reading too early yields footer boilerplate. Falls back
    to whole-body text when the container never shows."""
    deadline = time.monotonic() + settle_s
    while time.monotonic() < deadline:
        for sel in lm.NOTICE_MESSAGE_SELECTORS:
            try:
                loc = scope.locator(sel).first
                if not loc.count():
                    continue
                text = " ".join((loc.inner_text(timeout=1_000) or "").split())
                if text:
                    return text[:600]
            except Exception:
                continue
        time.sleep(poll_s)
    return _visible_page_text(scope)


def _notice_error(scope) -> TerminalPageError:
    """Build the terminal error for a gateway notice page, carrying the
    page's own visible text verbatim."""
    text = _notice_message(scope)
    locked = lm.looks_locked(text)
    what = ("an account-lockout notice" if locked
            else "a terminal notice page")
    return TerminalPageError(
        f"Schwab ended the login with {what} (gateway #/information "
        f"route); stopping all interaction",
        page_text=text, locked=locked)


# ============================================================
# Page classification & click guard
# ============================================================

# classify_page states that belong to the OAuth consent flow — the ONLY
# pages an advance button may be clicked on.
CONSENT_PAGES = frozenset({"terms", "account-link", "review"})


def _heading_present(page, heading: str) -> bool:
    try:
        return page.get_by_text(heading, exact=False).count() > 0
    except Exception:
        return False


def _scan_for_mfa_input(page):
    """The first visible 2FA code input across the page + frames, or
    None. One non-blocking sweep — polling is the caller's job."""
    scopes = [page, *[f for f in page.frames
                      if f.parent_frame is not None]]
    for scope in scopes:
        for sel in lm.MFA_CODE_INPUT_CANDIDATES:
            try:
                loc = scope.locator(sel).first
                if loc.count() and loc.is_visible(timeout=500):
                    return loc
            except Exception:
                continue
    return None


def _login_form_present(page) -> bool:
    scopes = [page, *[f for f in page.frames
                      if f.parent_frame is not None]]
    for scope in scopes:
        for sel in lm.PASSWORD_INPUT_CANDIDATES:
            try:
                loc = scope.locator(sel).first
                if loc.count() and loc.is_visible(timeout=300):
                    return True
            except Exception:
                continue
    return False


def classify_page(page, callback_url: str) -> str:
    """Positively classify the current page: 'callback', 'notice' (the
    gateway's terminal notice route), 'mfa' (a 2FA input is visible),
    'terms' / 'account-link' / 'review' (the consent flow, by heading),
    'login' (the credential form), or 'unknown'. Ordered so terminal and
    challenge states win over anything else the DOM might still show."""
    url = _live_url(page)
    if lm.is_callback_url(url, callback_url):
        return "callback"
    if _notice_scope(page) is not None:
        return "notice"
    if _scan_for_mfa_input(page) is not None:
        return "mfa"
    if _heading_present(page, lm.TERMS_HEADING):
        return "terms"
    if _heading_present(page, lm.ACCOUNT_LINK_HEADING):
        return "account-link"
    if _heading_present(page, lm.REVIEW_HEADING):
        return "review"
    if _login_form_present(page):
        return "login"
    return "unknown"


class AdvanceGuard:
    """Budget + progress guard for the consent-drive loop.

    Two invariants (DESIGN.md §3.1 incident note): a page that did not
    change since the last advance click is never clicked again, and a
    page that stops changing at all — clicked or not — fails the run
    after STALL_LIMIT_S instead of spinning. CLICK_BUDGET caps total
    advance clicks well above the flow's three known pages."""

    CLICK_BUDGET = 8
    STALL_LIMIT_S = 60.0

    def __init__(self):
        self.clicks = 0
        self._sig = None
        self._sig_since = time.monotonic()
        self._clicked_sig = None

    def observe(self, kind: str, sig: str) -> None:
        """Track the page state; raise once it has stalled too long."""
        now = time.monotonic()
        if sig != self._sig:
            self._sig = sig
            self._sig_since = now
            return
        if now - self._sig_since > self.STALL_LIMIT_S:
            raise LoginFlowError(
                f"page stopped changing for {int(self.STALL_LIMIT_S)}s "
                f"while driving the consent flow (state: {kind}); not "
                f"clicking further — re-run login, or drive it over VNC "
                f"(vnc-login)")

    def may_click(self) -> bool:
        """Whether an advance click is allowed: the page must have
        changed since the last click. Budget exhaustion raises."""
        if self.clicks >= self.CLICK_BUDGET:
            raise LoginFlowError(
                f"advance-click budget ({self.CLICK_BUDGET}) exhausted "
                f"without reaching the callback; not clicking further")
        return self._sig != self._clicked_sig

    def record_click(self) -> None:
        self.clicks += 1
        self._clicked_sig = self._sig


def _page_signature(page) -> str:
    """Hash of the page URL + structural DOM signature, for deciding
    whether a click actually changed anything. Text-insensitive, so a
    ticking countdown doesn't read as progress."""
    try:
        struct = page.evaluate(_STRUCT_SIG_JS)
    except Exception:
        struct = ""
    raw = f"{_live_url(page)}|{struct}"
    return hashlib.sha256(raw.encode("utf-8", "replace")).hexdigest()[:16]


def _wait_for_mfa_input(page, timeout_s: float, callback_url: str,
                        poll_s: float = 0.5):
    """Poll until the login lands somewhere recognizable. Returns
    ("mfa", Locator) when a 2FA input is visible, or ("callback", None) /
    ("consent", None) when the flow skipped the challenge (trusted
    device). Raises TerminalPageError on the gateway notice route and
    LoginFlowError when nothing recognizable appears within timeout_s —
    a timeout is never read as "no 2FA needed": that assumption is what
    let a locked-out re-run spin (DESIGN.md §3.1)."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        kind = classify_page(page, callback_url)
        if kind == "callback":
            return "callback", None
        if kind == "notice":
            raise _notice_error(_notice_scope(page) or page)
        if kind == "mfa":
            loc = _scan_for_mfa_input(page)
            if loc is not None:
                return "mfa", loc
        if kind in CONSENT_PAGES:
            return "consent", None
        time.sleep(poll_s)
    raise LoginFlowError(
        f"neither a 2FA input nor a consent page appeared within "
        f"{timeout_s:.0f}s — unrecognized page; re-run login, or drive "
        f"it over VNC (vnc-login)",
        page_text=_visible_page_text(page))


def _click_first(page, candidates) -> bool:
    for sel in candidates:
        try:
            btn = page.locator(sel).first
            if btn.count() == 0 or not btn.is_visible(timeout=500):
                continue
            btn.click(timeout=10_000)
            log.info("clicked %s", sel)
            return True
        except Exception as e:
            log.debug("click candidate %s failed: %s", sel, e)
    return False


def _check_visible_checkboxes(scope) -> int:
    """CHECK every visible, currently-unchecked checkbox in `scope`.
    Idempotent: Playwright's .check() leaves an already-checked box alone
    and NEVER unchecks, so this can run repeatedly without toggling a box
    off. Returns how many it newly checked."""
    n = 0
    try:
        boxes = scope.locator(lm.ACCOUNT_CHECKBOX_SELECTOR)
        count = boxes.count()
    except Exception:
        return 0
    for i in range(count):
        try:
            cb = boxes.nth(i)
            if not cb.is_visible(timeout=300):
                continue
            if cb.is_checked():
                continue
            cb.check(timeout=3_000)
            n += 1
        except Exception:
            continue
    return n


def _safe_is_checked(loc) -> bool | None:
    try:
        return loc.is_checked()
    except Exception:
        return None


def select_all_link_accounts(page, screenshot_dir=None, seen=None) -> int:
    """Tick every account checkbox on Schwab's "Select your Schwab
    accounts to link" page, so a newly opened account is linked to the
    token without anyone remembering to tick it. Scoped to that page (by
    its heading) so the login "remember me" box is never touched, and
    idempotent (see _check_visible_checkboxes). Captures the page DOM once
    for selector refinement. Returns how many boxes it newly ticked."""
    try:
        if page.get_by_text(lm.ACCOUNT_LINK_HEADING, exact=False).count() == 0:
            return 0
    except Exception:
        return 0
    if seen is not None and not seen[0]:
        seen[0] = True
        # Report what we can see so the selector + state-read is verifiable
        # from the CLI log even on a run where everything is already ticked.
        try:
            boxes = page.locator(lm.ACCOUNT_CHECKBOX_SELECTOR)
            total = boxes.count()
            checked = sum(1 for i in range(total)
                          if _safe_is_checked(boxes.nth(i)) is True)
            log.info("on the account-link page: matched %d checkbox(es), "
                     "%d already ticked (selector %r)",
                     total, checked, lm.ACCOUNT_CHECKBOX_SELECTOR)
        except Exception as e:
            log.warning("on the account-link page but could not read the "
                        "checkboxes (%s) — capturing DOM for refinement", e)
        maybe_capture_html(page, screenshot_dir, "025-account-link")
    n = _check_visible_checkboxes(page)
    if n:
        log.info("auto-ticked %d previously-unselected account checkbox(es)", n)
    return n


# Structural signature of the live DOM (tag#id.class[type]), ignoring text
# so a 2FA countdown doesn't re-trigger a capture. Used by --explore.
_STRUCT_SIG_JS = (
    "() => Array.from(document.querySelectorAll('*')).map(e => "
    "e.tagName + '#' + (e.id || '') + '.' + (e.getAttribute('class') || '') "
    "+ '[' + (e.getAttribute('type') || '') + ']').join('|')"
)


def _explore_dump(page, screenshot_dir, state) -> None:
    """During --explore: capture HTML + full-page PNG once per distinct
    page LAYOUT (structural-signature deduped, capped). `state` is a
    mutable [seen_set, count] pair."""
    if screenshot_dir is None or state[1] >= 40:
        return
    try:
        sig = page.evaluate(_STRUCT_SIG_JS)
    except Exception:
        return
    h = hashlib.sha256(sig.encode("utf-8", "replace")).hexdigest()[:12]
    if h in state[0]:
        return
    state[0].add(h)
    state[1] += 1
    label = f"explore-{state[1]:02d}-{h}"
    try:
        screenshot_dir.mkdir(parents=True, exist_ok=True)
        (screenshot_dir / f"{label}.html").write_text(
            debugcap.scrub_dom(page.content()), encoding="utf-8")
        try:
            page.screenshot(path=str(screenshot_dir / f"{label}.png"),
                            full_page=True, timeout=4_000)
        except Exception:
            pass
        log.info("explore: captured distinct page %s", label)
    except Exception as e:
        log.debug("explore capture failed: %s", e)


def _drive_consent(page, guard: AdvanceGuard, callback_url: str) -> None:
    """One iteration of the --cli-mfa consent drive: classify the page,
    tick the Terms agreement box when on the T&C page, and click the
    advance button (Continue / Done / Allow) — but ONLY on a page
    positively classified as consent-flow, never on a challenge, login,
    or unknown page, and only when the page changed since the last click
    (guard). Never clicks Cancel; .check() never unchecks. A stalled or
    over-budget flow raises instead of clicking again."""
    kind = classify_page(page, callback_url)
    guard.observe(kind, _page_signature(page))
    if kind == "terms":
        _check_visible_checkboxes(page)
    if kind not in CONSENT_PAGES:
        return
    if guard.may_click() and _click_first(page, lm.ADVANCE_BUTTON_CANDIDATES):
        guard.record_click()


# ============================================================
# OAuth browser flow
# ============================================================

def _token_writer(token_path: Path):
    def _write(token, *args, **kwargs):
        with open(token_path, "w") as f:
            json.dump(token, f)
    return _write


def _wait_for_callback(page, captured: list, callback_url: str,
                       timeout_s: float, screenshot_dir=None,
                       drive: bool = False, explore: bool = False) -> str | None:
    """Wait until the browser navigates to the callback URL (consent done)
    and return the full received URL with the `?code=…`. While waiting,
    auto-tick every account checkbox on the "Select your accounts to link"
    page (both modes — so a new account is linked without manual clicking);
    with `drive` (--cli-mfa), also click through the consent pages —
    classification-gated and budgeted (AdvanceGuard); with `explore`, dump
    each distinct page's DOM. Raises TerminalPageError when the gateway
    serves its terminal notice route (both modes — no flow proceeds past
    it), LoginFlowError when the driven flow stalls or exhausts its click
    budget."""
    deadline = time.monotonic() + timeout_s
    last_log = 0.0
    seen_link = [False]
    explore_state = [set(), 0]
    guard = AdvanceGuard()
    while time.monotonic() < deadline:
        if captured:
            return captured[0]
        url = _live_url(page)
        if lm.is_callback_url(url, callback_url):
            return url
        scope = _notice_scope(page)
        if scope is not None:
            raise _notice_error(scope)
        if explore:
            _explore_dump(page, screenshot_dir, explore_state)
        select_all_link_accounts(page, screenshot_dir, seen_link)
        if drive:
            _drive_consent(page, guard, callback_url)
        now = time.monotonic()
        if now - last_log > 15:
            log.info("waiting for consent redirect to %s … (current: %s)",
                     callback_url, url)
            last_log = now
        time.sleep(0.5)
    return None


def cmd_login_browser(args: argparse.Namespace) -> int:
    try:
        from schwab import auth as schwab_auth
    except ImportError:
        raise SystemExit("schwab-py is not installed.") from None

    client_id = envfile.resolve_credential(args.client_id, "SCHWAB_CLIENT_ID",
                                   "--client-id")
    client_secret = envfile.resolve_credential(args.client_secret,
                                       "SCHWAB_CLIENT_SECRET",
                                       "--client-secret")
    login_id = os.environ.get("SCHWAB_LOGIN_ID")
    password = os.environ.get("SCHWAB_PASSWORD")

    args.token_path.parent.mkdir(parents=True, exist_ok=True)
    launch.prepare_profile_dir(args.profile_dir)

    ctx = schwab_auth.get_auth_context(client_id, args.callback_url)
    log.info("Authorize URL built; opening in Camoufox.")
    log.info("Callback URL: %s", args.callback_url)
    log.info("Token will be written to: %s", args.token_path)

    with open_camoufox_context(args.profile_dir, args.trace) as context:
        captured: list[str] = []

        def _on_request(req) -> None:
            try:
                if lm.is_callback_url(req.url, args.callback_url):
                    if not captured:
                        captured.append(req.url)
            except Exception:
                pass

        context.on("request", _on_request)
        bodies = debugcap.BodyCapture(
            args.screenshot_dir if args.capture_bodies else None,
            host_markers=("sws-gateway", "api.schwabapi.com"), log=log,
            # An auth-host body echoes the login id back, and the file is
            # named after the URL path. Unset credentials are skipped by
            # the redactor, so the no-env case costs nothing.
            redact=debugcap.secret_redactor(login_id, password))
        bodies.attach(context)
        page = context.new_page()
        try:
            page.goto(ctx.authorization_url, wait_until="domcontentloaded")
        except Exception as e:
            log.warning("initial navigation reported: %s", e)
        maybe_capture_html(page, args.screenshot_dir, "01-authorize")
        prefill_login(page, login_id, password)
        maybe_capture_html(page, args.screenshot_dir, "02-prefilled")

        try:
            if args.cli_mfa:
                _attempt_cli_mfa(page, args)
            else:
                sys.stderr.write(
                    "\n" + "=" * 60 + "\n"
                    "Drive the login over VNC: log in, satisfy 2FA, pick the\n"
                    "account(s), and click Allow. This script captures the\n"
                    "redirect and exchanges it automatically.\n"
                    + "=" * 60 + "\n")
                sys.stderr.flush()

            received_url = _wait_for_callback(page, captured,
                                              args.callback_url,
                                              args.mfa_timeout,
                                              args.screenshot_dir,
                                              args.cli_mfa, args.explore)
        except LoginFlowError as exc:
            maybe_capture_html(page, args.screenshot_dir, "99-terminal")
            stop_trace_if_active(context, args.trace, args.screenshot_dir,
                                 "login-terminal")
            _report_flow_error(exc, args.screenshot_dir)
            return exc.exit_code
        finally:
            bodies.flush()
        maybe_capture_html(page, args.screenshot_dir, "03-postconsent")
        if received_url is None:
            stop_trace_if_active(context, args.trace, args.screenshot_dir,
                                 "login-timeout")
            log.error("Did not reach the callback URL within %ss. The grant "
                      "was not completed.", args.mfa_timeout)
            return 6

        log.info("Captured callback redirect; exchanging for tokens.")
        try:
            schwab_auth.client_from_received_url(
                client_id, client_secret, ctx, received_url,
                _token_writer(args.token_path),
            )
        except Exception as exc:
            stop_trace_if_active(context, args.trace, args.screenshot_dir,
                                 "exchange-failed")
            log.error("token exchange failed: %s: %s",
                      type(exc).__name__, exc)
            return 5
        stop_trace_if_active(context, args.trace, args.screenshot_dir,
                             "login-ok")

    if not args.token_path.is_file():
        log.error("flow completed but no token file at %s", args.token_path)
        return 4
    session.secure_file(args.token_path)
    issued = read_token_creation_time(args.token_path) or datetime.now(
        timezone.utc)
    renew_by = datetime.fromtimestamp(
        issued.timestamp() + REFRESH_TOKEN_TTL_SECONDS, tz=timezone.utc)
    log.info("Token minted successfully.")
    log.info("Renew by (UTC): %s", renew_by.isoformat(timespec="seconds"))
    return 0


def _verify_mfa_outcome(page, callback_url: str, *, budget_s: float = 60.0,
                        grace_s: float = 5.0, poll_s: float = 1.0) -> str:
    """Classify what Schwab did with the submitted 2FA code: "advanced"
    (callback or a consent page reached), "rejected" (still on the
    challenge with an on-page error), "stuck" (still on the challenge,
    no error, budget spent), or "pending" (left the challenge for
    nowhere recognizable yet; the consent wait's own guards take over).
    Raises TerminalPageError on the gateway notice route. The grace
    period covers navigation lag after Continue."""
    start = time.monotonic()
    kind = "unknown"
    while time.monotonic() - start < budget_s:
        kind = classify_page(page, callback_url)
        if kind == "callback" or kind in CONSENT_PAGES:
            return "advanced"
        if kind == "notice":
            raise _notice_error(_notice_scope(page) or page)
        if kind == "mfa" and time.monotonic() - start >= grace_s:
            if _visible_error_text(page):
                return "rejected"
        time.sleep(poll_s)
    return "stuck" if kind == "mfa" else "pending"


def _attempt_cli_mfa(page, args: argparse.Namespace) -> None:
    """Submit the login form once, resolve the 2FA challenge with at
    most ONE code submission, and return with the flow ready for the
    consent wait. Any ambiguity is terminal (LoginFlowError /
    TerminalPageError) rather than a fallthrough: the incident in
    DESIGN.md §3.1 came from driving the consent pages while the
    challenge was still unresolved."""
    if not _click_first(page, lm.LOGIN_SUBMIT_CANDIDATES):
        log.warning("login submit button not found; complete login over VNC")
        return
    state, code_loc = _wait_for_mfa_input(page, args.mfa_page_timeout,
                                          args.callback_url)
    if state != "mfa":
        log.info("no 2FA challenge served (%s reached); continuing", state)
        return
    code = _prompt_for_mfa_code()
    if not code:
        raise LoginFlowError(
            "no 2FA code entered (empty line or closed stdin); aborting "
            "before touching the challenge page — re-run login to retry")
    try:
        code_loc.fill(code)
    except Exception as e:
        raise LoginFlowError(f"could not fill the 2FA code input: {e}") from e
    if not _click_first(page, lm.MFA_CONTINUE_BUTTON_CANDIDATES):
        try:
            code_loc.press("Enter")
        except Exception as e:
            raise LoginFlowError(f"could not submit the 2FA code: {e}") from e
    outcome = _verify_mfa_outcome(page, args.callback_url)
    if outcome == "rejected":
        raise LoginFlowError(
            "Schwab rejected the 2FA code; one submission per run — "
            "re-run login to try again with a fresh code",
            page_text=_visible_error_text(page))
    if outcome == "stuck":
        raise LoginFlowError(
            "the 2FA challenge page did not move after the code was "
            "submitted; not retrying — re-run login",
            page_text=_visible_page_text(page))
    log.info("2FA submission outcome: %s; the consent wait takes over",
             outcome)
    # The Terms / account-selection / review pages are then driven from
    # the wait loop (_drive_consent), which ticks the boxes and advances.


def _report_flow_error(exc: LoginFlowError, screenshot_dir: Path | None) -> None:
    """Log a terminal flow failure: the diagnosis, the provider's own
    page text verbatim, and — for a lockout — where recovery lives."""
    log.error("%s", exc)
    if exc.page_text:
        log.error("Schwab's page says (verbatim): %s", exc.page_text)
    if exc.locked:
        log.error("The Schwab account is locked; it must be unlocked "
                  "with Schwab directly before another login attempt.")
    if screenshot_dir is None:
        log.error("(re-run with --screenshot-dir /debug to capture the "
                  "page for diagnosis)")


def cmd_login_manual(args: argparse.Namespace) -> int:
    """No-browser fallback: schwab-py prints the auth URL and reads the
    pasted redirect URL from stdin."""
    try:
        from schwab import auth as schwab_auth
    except ImportError:
        raise SystemExit("schwab-py is not installed.") from None
    client_id = envfile.resolve_credential(args.client_id, "SCHWAB_CLIENT_ID",
                                   "--client-id")
    client_secret = envfile.resolve_credential(args.client_secret,
                                       "SCHWAB_CLIENT_SECRET",
                                       "--client-secret")
    args.token_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        schwab_auth.client_from_manual_flow(
            api_key=client_id, app_secret=client_secret,
            callback_url=args.callback_url, token_path=str(args.token_path),
        )
    except Exception as exc:
        log.error("manual OAuth flow failed: %s: %s",
                  type(exc).__name__, exc)
        return 5
    if not args.token_path.is_file():
        log.error("flow completed but no token file at %s", args.token_path)
        return 4
    session.secure_file(args.token_path)
    log.info("Token minted successfully.")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    console_level = logging.DEBUG if args.verbose else logging.INFO
    logging.basicConfig(
        level=console_level,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    debugcap.tee_debug_log(args.screenshot_dir, console_level, log=log)
    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir (see AGENTS.md §4).")
    if args.capture_bodies and args.screenshot_dir is None:
        raise SystemExit("--capture-bodies requires --screenshot-dir "
                         "(see AGENTS.md §4).")
    if args.check:
        return cmd_check(args)
    source_env_files(args.env_file)
    if args.manual:
        return cmd_login_manual(args)
    if args.cli_mfa and not sys.stdin.isatty():
        raise SystemExit(
            "--cli-mfa reads the 2FA code from stdin, which is not a TTY "
            "here — nothing could answer the prompt, and an unanswered "
            "prompt must never fall through to page driving (DESIGN.md "
            "§3.1). Run from a terminal, or use vnc-login.")
    return cmd_login_browser(args)


if __name__ == "__main__":
    sys.exit(main())
