#!/usr/bin/env python3
"""
Schwab client-web login + scrape driver.

Implementation backing the wrapper's `download` (and `vnc-login`)
subcommands. Drives Firefox (headed, against an Xvfb virtual
display set up by entrypoint.sh) through the Schwab login, then
hands off the same `page` to `download.walk()` for the actual
statements + tx-history scrape.

Schwab invalidates the persistent profile's session within
seconds of Firefox closing, so login + scrape must happen in one
Firefox lifetime — each invocation pays one MFA challenge. The
trade-off is operational simplicity: no daemon to babysit, no
trigger files, no idle timeouts.

Modes:
  --check      validate the persisted profile against the
               Account Summary URL. Logs the cookie jar including
               _abck trust state. Mostly diagnostic — Schwab will
               report DEAD between scrape runs by design.
  default      pre-fill the form from SCHWAB_LOGIN_ID /
               SCHWAB_PASSWORD; with --cli-mfa (default) drive
               Log In + 2FA from stdin, with --no-cli-mfa wait
               for them to be done by hand over VNC. Then hand
               off to download.walk() against the same page.
               --bronze-dir is required (the bronze tree root).

Browser: Camoufox (Firefox). Schwab's Akamai rejects Chromium-family
automation; README.md "Sign-in and 2FA" has the details.
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
import time
from pathlib import Path

import download
import landmarks as schwab
from download import maybe_screenshot

from collectorkit import cli, debugcap, envfile, launch

log = logging.getLogger("schwab-web.login")


class TerminalPageError(RuntimeError):
    """The browser landed on a page the flow must never interact with —
    the gateway's terminal notice route (account lockout among them).
    Stops the run immediately: no retries, no long waits (see
    schwab-api's DESIGN.md §3.1 incident note for the defect class)."""

    exit_code = 9

    def __init__(self, message: str, *, page_text: str = "",
                 locked: bool = False):
        super().__init__(message)
        self.page_text = page_text
        self.locked = locked

# Default env-file locations. The wrapper mounts ~/.secrets to
# /secrets inside the container, so /secrets/schwab-web.env is the
# canonical place to drop the login-id/password env vars.
# $HOME/.secrets/schwab-web.env is the fallback, so the script works
# outside the container for local dev.
DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/schwab-web.env"),
    Path.home() / ".secrets" / "schwab-web.env",
)

# Env var names. The companion ~/.secrets/schwab-web.env file is a
# bash script that sets `SCHWAB_LOGIN_ID=<login-id>` and
# `SCHWAB_PASSWORD=<password>`, quoted by bash's rules (single quotes
# keep every character as written).
#
# Note the asymmetry vs. schwab-api (Trader API), which uses
# SCHWAB_CLIENT_ID / SCHWAB_CLIENT_SECRET for OAuth credentials.
# These are the web-login credentials and live in a separate
# namespace so the two sets never collide in a single shell env.
LOGIN_ID_ENV = "SCHWAB_LOGIN_ID"
PASSWORD_ENV = "SCHWAB_PASSWORD"

# Credentials whose env-file value wins over a value inherited from
# the calling environment: the file is authoritative for them.
_CRED_OVERRIDE_VARS = (LOGIN_ID_ENV, PASSWORD_ENV)

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", required=True, type=Path,
        help=("Persistent Firefox profile dir. Playwright stores "
              "cookies, Akamai bot-manager state, and localStorage "
              "here across runs. Treat it as sensitive — the script "
              "chmods the dir to 0700. Canonical container path: "
              "/secrets/schwab-web-profile/."),
    )
    p.add_argument(
        "--env-file", default=None, type=Path,
        help=("Bash-sourced env file with SCHWAB_LOGIN_ID / "
              "SCHWAB_PASSWORD, read before the credentials are resolved "
              "(default: /secrets/schwab-web.env if present, else "
              "~/.secrets/schwab-web.env)."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Validate the existing profile by navigating to the "
              "Account Summary URL and reading the resulting URL. "
              "Logs the cookie jar including the _abck bot-manager "
              "trust state. No credential submit, no 2FA. Mostly "
              "diagnostic — Schwab invalidates the session on "
              "Firefox close, so a fresh --check between scrape "
              "runs will report DEAD by design."),
    )
    p.add_argument(
        "--cli-mfa", action=argparse.BooleanOptionalAction, default=True,
        help=("Auto-submit the login form and prompt for the 2FA "
              "code on stdin. Default on. Pass --no-cli-mfa to "
              "fall back to the VNC-driven flow (Log In + 2FA "
              "driven by hand) — useful if Schwab restyles the "
              "gateway DOM and the CLI selectors miss."),
    )
    p.add_argument(
        "--bronze-dir", default=None, type=Path,
        help=("Bronze tree root. Required for the scrape path; the "
              "script chains into download.walk(page, bronze_dir, ...) "
              "after login completes. A new <UTC-timestamp>/ run "
              "dir is created under it. Canonical container "
              "path: /data."),
    )
    p.add_argument(
        "--mode", choices=("statements", "transactions", "all"),
        default="all",
        help=("Scrape mode for the post-login walk: 'statements' (the "
              "Statements & Tax Forms documents), 'transactions' (the "
              "Transaction History exports), or 'all' (both)."),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Walk the accounts and result pages without downloading "
              "any statement document. Tx-history exports still run, "
              "into a temporary directory: nothing is written under "
              "--bronze-dir."),
    )
    p.add_argument(
        "--no-more-detail", dest="with_more_detail", action="store_false",
        help=("Skip the per-row transaction detail pass. By DEFAULT the "
              "Transaction History pass also clicks each row's 'More' link "
              "and captures the per-row detail modal contents; pass this to "
              "skip that heavier pass. See download.py walk()."),
    )
    # The shared --lookback contract is the only window knob. A named
    # preset maps to the narrowest Schwab preset (Last3Months /
    # Last6Months / Last5Years / Last10Years) that still covers it; an
    # ISO date drives the custom date-range mode, with that preset as the
    # fallback. There is deliberately no raw-preset flag: a second
    # window flag could silently override the resolved window.
    cli.add_standard_args(p, verb="download")
    p.add_argument(
        "--mfa-timeout", type=int, default=300,
        help=("Seconds to wait for the sign-in to reach the post-auth "
              "page, whether the 2FA code comes from stdin or Log In + "
              "2FA are done by hand over VNC (default: 300 = 5 "
              "minutes). Exits with rc=7 on timeout."),
    )
    p.add_argument(
        "--debug", action="store_true",
        help=("Save opt-in debug captures INSIDE the bronze run dir "
              "under <run>/screenshots/ (the tx-history landing-page "
              "HTML baseline). Off by default — the capture is never "
              "read by load, and `prune` reclaims <run>/screenshots/ "
              "from complete dumps. Distinct from --screenshot-dir, "
              "which writes login/landmark captures OUTSIDE bronze."),
    )
    p.add_argument(
        "--screenshot-dir", default=None, type=Path,
        help=("If set, write a screenshot + HTML capture at each "
              "navigation landmark to this host path (OUTSIDE the "
              "bronze tree, e.g. the /debug mount), plus a full "
              "DEBUG-level run.log mirror of the run's output. Useful "
              "for debugging the login / landmark flow. NEVER commit "
              "these — see AGENTS.md §4."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help=("Capture a Playwright trace bundle. Requires "
              "--screenshot-dir; the bundle lands there alongside "
              "screenshots. The pinned Playwright tracer crashes the "
              "pinned Camoufox build, so a trace ends the login."),
    )
    p.add_argument(
        "--capture-bodies", action="store_true",
        help=("Debug aid: also save response bodies from the Schwab "
              "gateway host to --screenshot-dir (login-flow diagnosis). "
              "Requires --screenshot-dir. NEVER commit these — see "
              "AGENTS.md §4."),
    )
    return p.parse_args(argv)


# ============================================================
# Env-file loader
# ============================================================

def _load_env_file(path: Path) -> None:
    envfile.load_env_file(path, _CRED_OVERRIDE_VARS, logger=log,
                          warn_on_override=True)


def maybe_source_env_files(args: argparse.Namespace) -> None:
    if args.env_file is not None and not args.env_file.exists():
        raise SystemExit(f"--env-file does not exist: {args.env_file}")
    path = envfile.resolve_env_file(args.env_file, DEFAULT_ENV_FILE_CANDIDATES)
    if path is not None:
        _load_env_file(path)


# ============================================================
# Browser launch helpers
# ============================================================

def wait_for_dom_in_frame(frame_locator, selector: str,
                          timeout_s: float, poll_s: float = 0.5) -> bool:
    """Poll `frame_locator.locator(selector).count()` until the
    element is present or `timeout_s` expires.

    Used instead of `locator.wait_for(state="visible")` against the
    Schwab SPA: Schwab's Angular code emits perpetual change-
    detection cycles that confuse Playwright's "is page navigating?"
    guard, so wait_for runs to its full timeout even when the
    element is plainly there. A count() poll has no such guard.
    """
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        try:
            if frame_locator.locator(selector).count() > 0:
                return True
        except Exception as e:
            log.debug("wait_for_dom_in_frame eval err for %r: %s", selector, e)
        time.sleep(poll_s)
    return False


def open_page(context):
    """Return a usable page on the persistent context. Uses
    new_page() rather than context.pages[0] — the auto-created
    initial page can carry a partially-bound internal state that
    breaks page.evaluate(). new_page() avoids the trap.
    """
    return context.new_page()


def open_camoufox_context(profile_dir: Path, trace: bool):
    """Headed Camoufox on the persistent profile, yielding the
    BrowserContext (:func:`collectorkit.launch.camoufox_context`).

    Camoufox is a stealth-patched Firefox fork that masks the
    fingerprint surfaces (canvas, WebGL, audio, fonts, navigator.*,
    TLS) Schwab's anti-bot scoring uses to detect Playwright-driven
    Firefox. The window is 1280x800 on the entrypoint's Xvfb display,
    which avoids Schwab's mobile layout. The humanized cursor and the
    geoip fingerprint are off, as Schwab's sign-in has always run.

    A trace records no DOM snapshots: a sign-in is on screen, and a
    snapshot records every input's value, a hand-typed password
    included. The close is guarded, so a browser that died never hides
    the body's own error.
    """
    return launch.camoufox_context(profile_dir, trace=trace,
                                   humanize=False, geoip=False, logger=log)


# ============================================================
# Cookie-jar diagnostic
# ============================================================

def log_cookie_jar(context) -> None:
    """Log the schwab.com cookies in the current context, including
    a specific check on `_abck` (Akamai's bot-manager cookie). An
    `_abck` value starting with `~0~` means Akamai has invalidated
    its trust for this session's fingerprint; a longer value
    without the prefix means the cookie is still trusted.

    Used by --check and after manual login to confirm what made it
    onto the wire."""
    try:
        jar = context.cookies("https://client.schwab.com")
        schwab_cookies = [c for c in jar if "schwab.com" in c.get("domain", "")]
        log.info("cookie jar: %d schwab.com cookies", len(schwab_cookies))
        for c in schwab_cookies:
            n = c.get("name", "?")
            v = c.get("value", "") or ""
            short = v[:24] + ("…" if len(v) > 24 else "")
            log.info(
                "  %s @ %s  len=%d  starts=%s",
                n, c.get("domain", "?"), len(v), short,
            )
        abck = next((c for c in schwab_cookies if c.get("name") == "_abck"), None)
        if abck is None:
            log.warning("no _abck cookie — bot-manager state missing")
        else:
            av = abck.get("value", "") or ""
            if av.startswith("~0~"):
                log.warning(
                    "_abck starts with '~0~' — Akamai has INVALIDATED "
                    "bot-manager trust for this session's fingerprint"
                )
            else:
                log.info(
                    "_abck does not start with '~0~' — Akamai trust "
                    "intact (length=%d)", len(av),
                )
    except Exception as e:
        log.debug("cookie jar inspection failed: %s", e)


# ============================================================
# Flows
# ============================================================

def run_check(profile_dir: Path, screenshot_dir: Path | None,
              trace: bool) -> int:
    """Validate the persisted profile by navigating to the Account
    Summary URL. If Schwab serves the SPA at client.schwab.com/app/...
    the session is alive; if it redirects to Areas/Access/Login the
    session is dead (Schwab tends to kill sessions when the
    originating Firefox process exits, so --check after a manual
    login that closed Firefox typically reports DEAD even though
    the cookies are on disk)."""
    from playwright.sync_api import TimeoutError as PWTimeout

    if not profile_dir.is_dir():
        log.error("profile dir does not exist: %s", profile_dir)
        return 1

    log.info("validating session at %s", schwab.ACCOUNT_SUMMARY_URL)
    with open_camoufox_context(profile_dir, trace) as context:
        page = open_page(context)
        page.set_default_navigation_timeout(download.NAV_TIMEOUT_MS)
        try:
            page.goto(schwab.ACCOUNT_SUMMARY_URL, wait_until="domcontentloaded")
            url = _live_url(page)
            log.info("landed at %s", url)
            maybe_screenshot(page, screenshot_dir, "check-final")
            log_cookie_jar(context)
            if schwab.is_post_auth_url(url):
                log.info("session OK: %s", url)
                rc = 0
            else:
                log.error(
                    "session DEAD: %s — run `./schwab-web download` "
                    "(or `vnc-login`) to mint a new session",
                    url,
                )
                rc = 2
        except PWTimeout as e:
            maybe_screenshot(page, screenshot_dir, "check-timeout")
            log.error("timeout during --check: %s", e)
            rc = 3
        finally:
            debugcap.stop_trace_if_active(context, trace, screenshot_dir,
                                          "check", log=log)
        return rc


def run_manual(profile_dir: Path,
               screenshot_dir: Path | None,
               *,
               bronze_dir: Path,
               mode: str = "all",
               dry_run: bool = False,
               date_range: str = schwab.DATE_RANGE_DEFAULT,
               with_more_detail: bool = False,
               cli_mfa: bool = True,
               mfa_timeout_s: int,
               debug: bool = False,
               exact_window: tuple | None = None,
               capture_bodies: bool = False,
               trace: bool = False) -> int:
    """One-shot: CLI-MFA login → scrape → exit, in one Firefox
    lifetime (the session-invalidation constraint in the module
    docstring).

    Pre-fill is best-effort and non-fatal; creds missing from the
    env log a warning and (with --no-cli-mfa) leave Log In + 2FA
    to be driven by hand over VNC.
    """
    login_id_value = (os.environ.get(LOGIN_ID_ENV) or "").strip()
    password_value = os.environ.get(PASSWORD_ENV) or ""
    will_prefill = bool(login_id_value and password_value)
    if will_prefill:
        log.info(
            "will pre-fill form: login_id len=%d, password len=%d",
            len(login_id_value), len(password_value),
        )
    else:
        log.info(
            "creds not in env (login_id=%s, password=%s); skipping pre-fill",
            "set" if login_id_value else "MISSING",
            "set" if password_value else "MISSING",
        )
    log.info("scrape config: bronze_dir=%s mode=%s dry_run=%s range=%s more=%s",
             bronze_dir, mode, dry_run, date_range, with_more_detail)

    log.info("opening Firefox")
    with open_camoufox_context(profile_dir, trace) as context:
        bodies = debugcap.BodyCapture(
            screenshot_dir if capture_bodies else None,
            host_markers=("sws-gateway",), log=log,
            # An auth-host body echoes the login id back, and the file is
            # named after the URL path. Unset credentials are skipped by
            # the redactor, so the no-env case costs nothing.
            redact=debugcap.secret_redactor(login_id_value, password_value))
        bodies.attach(context)
        page = open_page(context)
        page.on("pageerror", lambda exc: log.warning("browser pageerror: %s", exc))
        try:
            page.goto(schwab.MARKETING_HOMEPAGE, wait_until="domcontentloaded")
            if will_prefill:
                _prefill_login_iframe(page, login_id_value, password_value)

            if cli_mfa:
                if not will_prefill:
                    log.warning(
                        "--cli-mfa requested but credentials are not set "
                        "in the environment; skipping auto-submit (Log In "
                        "+ 2FA must be driven by hand over VNC)"
                    )
                else:
                    verdict = _run_cli_mfa(page, screenshot_dir,
                                           mfa_wait_s=mfa_timeout_s)
                    if verdict == "abort":
                        log.error(
                            "CLI-MFA could not complete; re-run download "
                            "for a fresh attempt, or drive the login "
                            "via ./schwab-web vnc-login"
                        )
                        return 8
                    if verdict == "manual":
                        log.warning(
                            "CLI-MFA path failed; falling back to manual "
                            "drive — open a VNC session (./schwab-web "
                            "vnc-login) and complete the login by hand"
                        )

            if cli_mfa:
                log.info(
                    "waiting for the post-auth landing page (/app/...). "
                    "If anything stalls, open a VNC session "
                    "(./schwab-web vnc-login) to recover."
                )
            else:
                log.info(
                    "Firefox ready. Via VNC: click Log In, enter the "
                    "2FA code, land on Account Summary. Then the script "
                    "takes over and scrapes."
                )
            auth_page = _wait_for_post_auth(context, mfa_timeout_s)
            if auth_page is None:
                maybe_screenshot(page, screenshot_dir, "post-auth-timeout")
                log.error(
                    "post-auth URL not detected within %ds; either the "
                    "login didn't finish, or Firefox was closed.",
                    mfa_timeout_s,
                )
                return 7
            if auth_page is not page:
                log.info(
                    "post-auth landed on a different page (Schwab opened "
                    "a new tab/window for the logged-in session); "
                    "switching to it for the scrape"
                )
                page = auth_page
            log.info(
                "post-auth detected at %s; taking over in 3s — "
                "move the mouse out of the browser window",
                _live_url(page),
            )
            # Tiny grace period leaving a moment for the pointer to
            # be retracted before Playwright starts dispatching
            # synthetic events. Without it, a stray
            # hover-tooltip on the account selector can intercept
            # the next click.
            time.sleep(3)
            maybe_screenshot(page, screenshot_dir, "post-auth-handoff")
            page.set_default_navigation_timeout(download.NAV_TIMEOUT_MS)
            summary = download.walk(
                page, bronze_dir,
                mode=mode, dry_run=dry_run,
                screenshot_dir=screenshot_dir,
                date_range=date_range,
                with_more_detail=with_more_detail,
                debug=debug,
                exact_window=exact_window,
            )
            if summary.get("status") not in ("complete", "dry-run"):
                # The walk stopped short (no accounts enumerated) and left
                # its dump in-progress: a failure, not an empty success.
                log.error("scrape did not finish (status=%s)",
                          summary.get("status"))
                return 1
            log.info(
                "scrape complete: %d statement entries, %d tx entries",
                len(summary.get("statements", [])),
                len(summary.get("transactions", [])),
            )
            return 0
        except KeyboardInterrupt:
            log.info("interrupted; closing browser")
            return 130
        except TerminalPageError as e:
            maybe_screenshot(page, screenshot_dir, "terminal-page")
            _report_terminal_page(e)
            return e.exit_code
        except Exception as e:
            log.exception("login + scrape failed: %s", e)
            try:
                maybe_screenshot(page, screenshot_dir, "manual-error")
            except Exception:
                pass
            return 1
        finally:
            bodies.flush()
            debugcap.stop_trace_if_active(context, trace, screenshot_dir,
                                          "manual", log=log)


def _live_url(page) -> str:
    """Read the page's current URL by asking Firefox directly.

    Equivalent to `page.url` EXCEPT under camoufox 135 + playwright
    1.49: juggler doesn't reliably deliver `frameNavigated` events
    to the driver, so `page.url` stays stuck on the pre-redirect
    value after a top-level navigation completes (the Firefox UI
    URL bar updates, but Playwright's cached accessor doesn't).
    `location.href` via evaluate forces Firefox to answer from its
    actual document state, which is what landmark detection needs.

    Returns the cached page.url on evaluation failure — page mid-
    navigation, page closing, or other races — so a transient
    error doesn't kill the polling loop.
    """
    try:
        result = page.evaluate("() => location.href")
        if isinstance(result, str) and result:
            return result
    except Exception:
        pass
    return page.url


def _visible_page_text(scope, limit: int = 600) -> str:
    """Visible body text of a page or frame, whitespace-collapsed and
    capped — the provider's own words for terminal error reports."""
    try:
        text = scope.evaluate(
            "() => (document.body && document.body.innerText) || ''")
        return " ".join(str(text).split())[:limit]
    except Exception:
        return ""


def _notice_scope(page):
    """The page or frame currently on the gateway's terminal notice
    route (landmarks.is_gateway_notice_url), or None. Checks frames too
    — on the homepage the gateway runs inside the login iframe."""
    if schwab.is_gateway_notice_url(_live_url(page)):
        return page
    try:
        frames = [f for f in page.frames if f.parent_frame is not None]
    except Exception:
        frames = []
    for f in frames:
        if schwab.is_gateway_notice_url(f.url or ""):
            return f
    return None


def _notice_message(scope, settle_s: float = 8.0, poll_s: float = 0.5) -> str:
    """The notice page's own message, read from its message container
    (landmarks.NOTICE_MESSAGE_SELECTORS), waiting for the SPA to render
    it — the content arrives via an async fetch seconds after the route
    change, and reading too early yields footer boilerplate. Falls back
    to whole-body text when the container never shows."""
    deadline = time.monotonic() + settle_s
    while time.monotonic() < deadline:
        for sel in schwab.NOTICE_MESSAGE_SELECTORS:
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
    locked = schwab.looks_locked(text)
    what = ("an account-lockout notice" if locked
            else "a terminal notice page")
    return TerminalPageError(
        f"Schwab ended the login with {what} (gateway #/information "
        f"route); stopping all interaction",
        page_text=text, locked=locked)


def _report_terminal_page(exc: TerminalPageError) -> None:
    """Log a terminal notice page: the diagnosis, the provider's own
    page text verbatim, and — for a lockout — where recovery lives."""
    log.error("%s", exc)
    if exc.page_text:
        log.error("Schwab's page says (verbatim): %s", exc.page_text)
    if exc.locked:
        log.error("The Schwab account is locked; it must be unlocked "
                  "with Schwab directly before another login attempt.")


def _wait_for_post_auth(context, timeout_s: float, poll_s: float = 1.0):
    """Wait for any page in `context` to land on a post-auth URL.

    Schwab's login flow can either redirect the same tab to
    client.schwab.com or open a NEW tab/window for the
    authenticated session, so each poll enumerates context.pages and
    returns whichever page matches.

    Reads each page's URL via `_live_url` (location.href in-page)
    rather than `p.url` because the camoufox/playwright pin doesn't
    propagate frame-navigated events — `p.url` stays stuck on the
    pre-login value indefinitely.

    Returns the matching Page object, or None on timeout or if all
    pages were closed before login completed. Raises TerminalPageError
    when any page lands on the gateway's terminal notice route — waiting
    longer can never succeed there.
    """
    deadline = time.monotonic() + timeout_s
    last_state = None
    while time.monotonic() < deadline:
        pages = [p for p in context.pages if not p.is_closed()]
        if not pages:
            log.warning("no open pages remain — Firefox was closed")
            return None
        urls = [_live_url(p) for p in pages]
        if urls != last_state:
            log.info("waiting for /app/... — pages=%s", urls)
            last_state = urls
        for p, u in zip(pages, urls, strict=True):
            if schwab.is_post_auth_url(u):
                return p
        for p in pages:
            scope = _notice_scope(p)
            if scope is not None:
                raise _notice_error(scope)
        time.sleep(poll_s)
    return None


def _password_field_present(gateway) -> bool:
    """Whether the login iframe still shows its password field."""
    try:
        loc = gateway.locator(f"#{schwab.PASSWORD_INPUT_ID}")
        return loc.count() > 0 and loc.first.is_visible(timeout=500)
    except Exception:
        return False


def _submit_login_form(page) -> str:
    """Submit the login form inside the homepage's `#schwablmslogin`
    iframe, with a hard budget of submit gestures: Enter on the password
    field first (native submit, most human-like — Schwab's anti-bot
    heuristics flag tight pre-fill→submit timing and synthetic clicks),
    then, ONLY when the form is demonstrably still sitting there
    un-submitted, a single Log In button click. Never more — a gesture
    that may have registered is never repeated blind (the defect class
    behind schwab-api's lockout incident, DESIGN.md §3.1 there).

    Returns "progress" (the gateway moved), "error" (Schwab shows an
    on-page error, logged verbatim), or "failed" (nothing registered).
    """
    gateway = page.frame_locator(f"#{schwab.LOGIN_IFRAME_ID}")
    pre_iframe_url = _iframe_url(page)
    log.debug("pre-submit iframe url: %s", pre_iframe_url)

    # Pre-submit pause — humans don't click 50ms after the last keypress.
    time.sleep(1.5)

    entered = False
    try:
        pwd = gateway.locator(f"#{schwab.PASSWORD_INPUT_ID}")
        if pwd.count() > 0:
            pwd.press("Enter")
            entered = True
            log.info("submitted login form via Enter on password field")
            if _wait_iframe_progress(page, pre_iframe_url, timeout_s=8):
                return "progress"
    except Exception as e:
        log.debug("Enter on password failed: %s", e)

    err = _visible_error_text_anywhere(page)
    if err:
        log.error("Schwab reports:\n  - %s", err)
        return "error"

    if entered and not _password_field_present(gateway):
        # Enter was dispatched and the form is gone, yet nothing moved —
        # ambiguous mid-transition state; a second submit could double up.
        log.error("login form no longer visible but the gateway did not "
                  "move; not submitting again — re-run to retry")
        return "failed"

    # One fallback click: the form is demonstrably still sitting there
    # (or Enter never dispatched), so either the SPA ignored the Enter
    # or it never happened. The first matching candidate is clicked and
    # that is the last gesture, progress or not.
    candidates = [
        ("id",   f"#{schwab.LOGIN_BUTTON_ID}"),
        ("text", f"button:has-text('{schwab.LOGIN_BUTTON_TEXT}')"),
        ("type", "button[type='submit']"),
    ]
    for kind, sel in candidates:
        try:
            btn = gateway.locator(sel).first
            if btn.count() == 0:
                log.debug("login button candidate %s (%s): no match", sel, kind)
                continue
            btn.click(timeout=10_000)
            log.info("submitted login form via %s selector %s "
                     "(single fallback)", kind, sel)
            if _wait_iframe_progress(page, pre_iframe_url, timeout_s=20):
                return "progress"
            break  # one submit click only — never hammer further candidates
        except Exception as e:
            log.debug("login button candidate %s (%s) failed: %s", sel, kind, e)

    err = _visible_error_text_anywhere(page)
    if err:
        log.error("Schwab reports:\n  - %s", err)
        return "error"
    log.error(
        "form did not visibly submit; not retrying — see debug "
        "screenshots (the iframe may show an error or Schwab may have "
        "silently rejected the submit)"
    )
    return "failed"


def _iframe_url(page) -> str | None:
    """Return the current URL of the login iframe (the
    sws-gateway-nr.schwab.com frame), or None if not yet
    attached."""
    for f in page.frames:
        if "sws-gateway" in (f.url or ""):
            return f.url
    return None


def _wait_iframe_progress(page, baseline_url: str | None,
                          timeout_s: float, poll_s: float = 0.5) -> bool:
    """Poll until either (a) the top-level URL leaves www.schwab.com
    (meaning the form submit triggered a navigation out of the
    homepage), or (b) the iframe's URL changes from `baseline_url`
    (meaning the gateway SPA stepped to the next page — typically
    the 2FA challenge — inside the iframe).

    Returns True on progress, False on timeout. Used after a
    submit attempt to decide whether to try the next candidate.
    """
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        top = _live_url(page)
        if not top.startswith(schwab.MARKETING_HOMEPAGE):
            log.info("top-level URL advanced to %s", top)
            return True
        cur_iframe = _iframe_url(page)
        if cur_iframe != baseline_url:
            log.info(
                "iframe URL advanced: %s -> %s", baseline_url, cur_iframe,
            )
            return True
        time.sleep(poll_s)
    return False


# Visible <input> and <button>/[role=button] elements of a document, by
# their identifying attributes only — never an input's value (AGENTS.md
# §4). Logged when the CLI-MFA selectors miss, so the next iteration can
# identify the new ids without burning a fresh MFA round on the DOM.
_VISIBLE_FORM_ELEMENTS_JS = """
() => {
    const visible = e => {
        const r = e.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
    };
    const inputs = [...document.querySelectorAll('input')]
        .filter(visible)
        .map(e => ({
            id: e.id || null,
            name: e.name || null,
            type: e.type || null,
            placeholder: e.placeholder || null,
            autocomplete: e.autocomplete || null,
            maxlength: e.maxLength > 0 ? e.maxLength : null,
        }));
    const buttons = [...document.querySelectorAll('button, [role="button"]')]
        .filter(visible)
        .map(e => ({
            id: e.id || null,
            type: e.type || null,
            text: (e.textContent || '').trim().slice(0, 60) || null,
        }));
    return {inputs, buttons};
}
"""


def _dump_visible_form_elements(page, label: str) -> None:
    """Log the top-level page's visible form elements
    (_VISIBLE_FORM_ELEMENTS_JS), one line each."""
    try:
        info = page.evaluate(_VISIBLE_FORM_ELEMENTS_JS)
        log.info("DOM snapshot at %s: url=%s", label, _live_url(page))
        for x in info.get("inputs") or []:
            log.info("  input %s", x)
        for x in info.get("buttons") or []:
            log.info("  button %s", x)
    except Exception as e:
        log.debug("DOM snapshot at %s failed: %s", label, e)


def _dump_iframe_state(page) -> None:
    """Log every attached frame's URL plus its visible form elements
    (_VISIBLE_FORM_ELEMENTS_JS). Used when MFA selectors miss — tells
    where in the frame tree the challenge actually landed.
    """
    try:
        for f in page.frames:
            try:
                summary = f.evaluate(_VISIBLE_FORM_ELEMENTS_JS)
            except Exception as e:
                summary = {"error": str(e)}
            log.info(
                "frame %r url=%s parent=%s: %s",
                f.name, f.url,
                "yes" if f.parent_frame is not None else "no (main)",
                summary,
            )
    except Exception as e:
        log.debug("frame state dump failed: %s", e)


def _wait_for_mfa_input(page, timeout_s: float, poll_s: float = 0.5):
    """Poll for the first visible MFA code input matching any of
    `schwab.MFA_CODE_INPUT_CANDIDATES`. Searches the top-level
    page AND every attached frame, since Schwab's 2FA challenge
    may render either at top-level (after a redirect out of the
    iframe) or inside the gateway iframe itself.

    Returns `(scope_label, Locator)` on hit, or `None` on timeout
    / if the page already redirected to a post-auth URL. Raises
    TerminalPageError on the gateway's terminal notice route: the
    challenge never comes there."""
    deadline = time.monotonic() + timeout_s
    last_log = 0.0
    while time.monotonic() < deadline:
        if schwab.is_post_auth_url(_live_url(page)):
            log.info(
                "post-auth URL detected without MFA challenge — "
                "device already trusted or no 2FA required"
            )
            return None
        scope = _notice_scope(page)
        if scope is not None:
            raise _notice_error(scope)
        scopes = [("page", page)]
        for f in page.frames:
            if f.parent_frame is None:
                continue  # main frame == page; already covered
            scopes.append((f"frame[{f.url}]", f))
        for scope_label, scope in scopes:
            for sel in schwab.MFA_CODE_INPUT_CANDIDATES:
                try:
                    loc = scope.locator(sel).first
                    if loc.count() == 0:
                        continue
                    if loc.is_visible(timeout=500):
                        return f"{scope_label} :: {sel}", loc
                except Exception:
                    continue
        now = time.monotonic()
        if now - last_log > 5:
            log.debug(
                "waiting for MFA input (top=%s, iframe=%s)",
                _live_url(page), _iframe_url(page),
            )
            last_log = now
        time.sleep(poll_s)
    return None


def _prompt_for_mfa_code() -> str:
    """Print a prompt to stderr and read a code from stdin. stderr
    is used so the prompt is visible even when stdout is
    redirected to a log file. Returns the stripped code string;
    empty input returns ''."""
    return cli.prompt_on_stderr(
        "Schwab 2FA: enter your 2FA code, then press Enter.")


def _submit_mfa_code(page, code_locator, code: str) -> str | None:
    """Fill the MFA input with `code` and click the Continue button
    (trying each candidate selector). Returns "clicked" on a button
    click, "enter" when only the Enter-key fallback landed, None
    when nothing could submit."""
    code_locator.fill(code)
    for sel in schwab.MFA_CONTINUE_BUTTON_CANDIDATES:
        try:
            btn = page.locator(sel).first
            if btn.count() == 0:
                continue
            if not btn.is_visible(timeout=500):
                continue
            btn.click(timeout=10_000)
            log.info("submitted 2FA via Continue selector %s", sel)
            return "clicked"
        except Exception as e:
            log.debug("continue button candidate %s failed: %s", sel, e)
    log.warning("no Continue button matched; pressing Enter in the input")
    try:
        code_locator.press("Enter")
        return "enter"
    except Exception as e:
        log.error("could not press Enter to submit 2FA: %s", e)
        return None


def _verify_mfa_outcome(page, code_input, *, budget_s: float = 60,
                        grace_s: float = 5, poll_s: float = 1.0) -> str:
    """Classify what Schwab did with a submitted 2FA code: 'auth'
    (post-auth URL reached), 'rejected' (code input still visible
    after the grace period), 'login' (the login iframe is back — the
    challenge session ended), or 'pending' (nothing conclusive
    within the budget; the caller's long post-auth wait takes over).
    Raises TerminalPageError on the gateway's terminal notice route.
    The grace period covers the navigation lag after Continue."""
    start = time.monotonic()
    while time.monotonic() - start < budget_s:
        if schwab.is_post_auth_url(_live_url(page)):
            return "auth"
        scope = _notice_scope(page)
        if scope is not None:
            raise _notice_error(scope)
        if time.monotonic() - start >= grace_s:
            try:
                if code_input.is_visible():
                    return "rejected"
            except Exception:
                pass
            try:
                if page.locator(f"#{schwab.LOGIN_IFRAME_ID}").is_visible():
                    return "login"
            except Exception:
                pass
        time.sleep(poll_s)
    return "pending"


def _visible_error_text(page) -> str:
    """Visible error text, one message per line. role=alert regions
    are preferred over the generic aria-live/error-classed fallback.
    Only innermost matches are read — a matched container's
    textContent swallows its children's (tooltip widgets, the error
    span) into one blob, so containers of other matches are dropped."""
    try:
        found = page.evaluate(
            """() => {
                const grab = (sel) => {
                    const els = Array.from(
                        document.querySelectorAll(sel))
                        .filter(el => el.offsetWidth || el.offsetHeight);
                    return els
                        .filter(el => !els.some(
                            o => o !== el && el.contains(o)))
                        .map(el => el.textContent
                            .replace(/\\s+/g, ' ').trim())
                        .filter(t => t && t.length <= 300);
                };
                return {
                    alerts: grab('[role="alert"]'),
                    other: grab('[aria-live], [class*="error" i], '
                                + '[class*="alert" i]'),
                };
            }"""
        )
        texts = found.get("alerts") or found.get("other") or []
        return "\n  - ".join(list(dict.fromkeys(texts))[:5])
    except Exception:
        return ""


def _visible_error_text_anywhere(page) -> str:
    """_visible_error_text across the page and every attached frame —
    login errors render inside the gateway iframe, which a top-level
    DOM query cannot see."""
    texts = []
    try:
        scopes = [page, *[f for f in page.frames
                          if f.parent_frame is not None]]
    except Exception:
        scopes = [page]
    for scope in scopes:
        t = _visible_error_text(scope)
        if t:
            texts.append(t)
    return "\n  - ".join(dict.fromkeys(texts))


def _run_cli_mfa(page, screenshot_dir: Path | None, *,
                 mfa_wait_s: float) -> str:
    """Auto-submit the login form (single submit gesture with one
    DOM-verified fallback), wait up to `mfa_wait_s` (--mfa-timeout) for
    the 2FA page, prompt for the code, submit it ONCE, and verify the
    outcome. No cleverer than the web
    page: any failure after the submissions is terminal — Schwab's own
    on-page error is logged verbatim and re-running the command is the
    fresh start. Repeated challenge submissions are the defect class
    that locked an account via schwab-api (see that collector's
    DESIGN.md §3.1).

    Verdicts: "ok" (landed, pending, or trusted-device skip),
    "manual" (selectors missed; VNC can still finish, so the caller
    keeps its long wait), "abort" (exit instead of waiting). Terminal
    notice pages raise TerminalPageError out of the wait helpers.
    """
    maybe_screenshot(page, screenshot_dir, "pre-login-submit")
    submit = _submit_login_form(page)
    if submit == "error":
        return "abort"
    if submit == "failed":
        maybe_screenshot(page, screenshot_dir, "submit-failed")
        _dump_visible_form_elements(page, "login-submit-failed")
        return "manual"
    # Two captures: immediate (during transition) and ~3s later
    # (after navigation settles) record what Schwab served without
    # burning a fresh MFA round to inspect it by hand.
    maybe_screenshot(page, screenshot_dir, "post-login-submit-immediate")
    time.sleep(3)
    maybe_screenshot(page, screenshot_dir, "post-login-submit-settled")

    hit = _wait_for_mfa_input(page, timeout_s=mfa_wait_s)
    if hit is None:
        # Either the device was trusted (caller will see post-auth
        # URL and proceed) or the input never appeared. Distinguish
        # by re-checking the URL.
        if schwab.is_post_auth_url(_live_url(page)):
            return "ok"
        log.error(
            "no MFA input field appeared within %ds; "
            "Schwab may have served a different challenge (security "
            "question, push-to-device prompt, etc.) — fall back to "
            "--no-cli-mfa and drive via VNC", mfa_wait_s,
        )
        _dump_iframe_state(page)
        return "manual"

    sel, loc = hit
    log.info("MFA code input found (%s); prompting for code on stdin", sel)
    maybe_screenshot(page, screenshot_dir, "mfa-prompt")
    code = _prompt_for_mfa_code()
    if not code:
        log.error("no 2FA code entered; aborting login")
        return "abort"
    submitted = _submit_mfa_code(page, loc, code)
    if submitted is None:
        return "manual"
    maybe_screenshot(page, screenshot_dir, "post-mfa-submit")
    outcome = _verify_mfa_outcome(page, loc)
    if outcome in ("auth", "pending"):
        return "ok"
    err = _visible_error_text_anywhere(page)
    if err:
        log.warning("Schwab reports:\n  - %s", err)
    if outcome == "login":
        log.warning("the login form is back — the challenge session "
                    "ended; re-run download to start fresh")
    else:
        log.error("2FA code rejected; one submission per run — re-run "
                  "download to start fresh with a new code")
    return "abort"


def _prefill_login_iframe(page, login_id_value: str, password_value: str) -> None:
    """Pre-fill #loginIdInput + #passwordInput inside the homepage's
    `#schwablmslogin` iframe. Non-fatal on failure — if anything
    goes wrong, the form can still be filled in by hand."""
    try:
        iframe = page.locator(f"#{schwab.LOGIN_IFRAME_ID}")
        iframe.wait_for(state="visible", timeout=20_000)
        gateway = page.frame_locator(f"#{schwab.LOGIN_IFRAME_ID}")
        if not wait_for_dom_in_frame(
            gateway, f"#{schwab.LOGIN_ID_INPUT_ID}", 15,
        ):
            log.warning("login form did not appear in iframe; skipping pre-fill")
            return
        gateway.locator(f"#{schwab.LOGIN_ID_INPUT_ID}").fill(login_id_value)
        gateway.locator(f"#{schwab.PASSWORD_INPUT_ID}").fill(password_value)
        log.info("login form pre-filled")
    except Exception as e:
        log.warning("pre-fill failed (%s); the credentials must be typed "
                    "by hand over VNC", e)


# ============================================================
# Entry point
# ============================================================

# Schwab's Statements filter tops out at Last10Years. A window reaching
# further back cannot be honoured, so the walk covers less than was asked
# for — worth one warning rather than a silent shortfall.
LAST_10_YEARS_DAYS = 3650


def _since_to_schwab_preset(since, until, log=None) -> str:
    """Map a (since, until) window to the narrowest Statements preset that
    still covers it.

    A named --lookback window runs on the preset; an exact window
    (_exact_window) uses the custom date-range mode and falls back to this
    preset. A window wider than the longest preset is capped, with a
    warning."""
    days = (until - since).days
    if days <= 90:
        return "Last3Months"
    if days <= 180:
        return "Last6Months"
    # <= keeps the shared 5y preset (exactly 1825 days) on Last5Years.
    # Safe: Schwab resolves Last5Years to the 5-calendar-year
    # anniversary date, and any 5-year calendar span contains at least
    # one leap day, so the preset always covers >= 1826 days — never a
    # day short of 1825.
    if days <= 1825:
        return "Last5Years"
    if days > LAST_10_YEARS_DAYS and log is not None:
        log.warning(
            "requested window reaches back to %s (%d days), but Schwab's "
            "filter stops at Last10Years — coverage is capped at ~10 years.",
            since, days)
    return "Last10Years"


def _exact_window(lookback, since, until):
    """(since, until) when ``--lookback`` names an exact ISO date, else
    None. resolve_lookback already validated the value, so anything
    that is not a named preset is an ISO date. Named presets keep the
    preset ladder on both pages; an exact date drives the shared
    SpecifyDateRange custom mode on Statements and Tx-history alike,
    each falling back to the covering preset if the fill fails."""
    if lookback and lookback not in cli.LOOKBACK_PRESETS:
        return (since, until)
    return None


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    console_level = logging.DEBUG if args.verbose else logging.INFO
    logging.basicConfig(level=console_level, format=cli.LOG_FORMAT)
    debugcap.tee_debug_log(args.screenshot_dir, console_level, log=log)
    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir (see AGENTS.md §3).")
    if args.capture_bodies and args.screenshot_dir is None:
        raise SystemExit("--capture-bodies requires --screenshot-dir "
                         "(see AGENTS.md §4).")
    # Translate the one window flag into the Schwab preset that covers it.
    # An ISO-date --lookback expresses an exact window; both pages honour
    # it verbatim via their custom date-range mode, so the resolved window
    # rides along and the preset is the fallback when that mode fails.
    since, until = cli.resolve_lookback(args)
    args.date_range = _since_to_schwab_preset(since, until, log=log)
    args.exact_window = _exact_window(args.lookback, since, until)
    maybe_source_env_files(args)
    if args.check:
        return run_check(args.profile_dir, args.screenshot_dir, args.trace)
    if args.bronze_dir is None:
        raise SystemExit(
            "--bronze-dir is required for the scrape path (or pass --check "
            "to validate the existing profile)."
        )
    if args.cli_mfa and not sys.stdin.isatty():
        raise SystemExit(
            "--cli-mfa reads the 2FA code from stdin, which is not a TTY "
            "here — nothing could answer the prompt. Run from a terminal, "
            "or pass --no-cli-mfa and drive the login over VNC.")
    return run_manual(
        args.profile_dir, args.screenshot_dir,
        bronze_dir=args.bronze_dir,
        mode=args.mode,
        dry_run=args.dry_run,
        date_range=args.date_range,
        with_more_detail=args.with_more_detail,
        cli_mfa=args.cli_mfa,
        mfa_timeout_s=args.mfa_timeout,
        debug=args.debug,
        exact_window=args.exact_window,
        capture_bodies=args.capture_bodies,
        trace=args.trace,
    )


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
