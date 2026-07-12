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
               for the operator to do it over VNC. Then hand
               off to download.walk() against the same page.
               --dest is required (the bronze tree root).

Browser choice: Firefox rather than Chromium. Schwab's Akamai
rejects every Chromium-family automation surface we tried
(headless Chromium, patchright-patched Chromium, real Chrome
unavailable on Linux ARM64). See README.md "Browser choice" for
the full diagnostic chain.
"""

from __future__ import annotations

import argparse
import contextlib
import logging
import os
import sys
import time
from pathlib import Path

import landmarks as schwab

from collectorkit import bronze, cli

log = logging.getLogger("schwab-web.login")

# Playwright timeouts (milliseconds). Generous defaults — WAN
# latency from arbitrary cloud regions to Schwab can be high, and
# the gateway SPA bundle is heavy.
NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 60_000

# Persistent profile dir mode. Holds session cookies + Akamai
# bot-manager state + localStorage.
PROFILE_DIR_MODE = 0o700

# Default env-file locations. The wrapper mounts ~/.secrets to
# /secrets inside the container, so /secrets/schwab-web.env is the
# canonical place to drop the login-id/password env vars. We also
# look at $HOME/.secrets/schwab-web.env so the script works outside
# the container for local dev.
DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/schwab-web.env"),
    Path.home() / ".secrets" / "schwab-web.env",
)

# Env var names. The companion ~/.secrets/schwab-web.env file
# should contain lines of the form `SCHWAB_LOGIN_ID=<login-id>`
# and `SCHWAB_PASSWORD=<password>` (single-quoted if the values
# contain shell metacharacters — see load_env_file's docstring).
#
# Note the asymmetry vs. schwab-api (Trader API), which uses
# SCHWAB_CLIENT_ID / SCHWAB_CLIENT_SECRET for OAuth credentials.
# These are the web-login credentials and live in a separate
# namespace so the two sets never collide in a single shell env.
LOGIN_ID_ENV = "SCHWAB_LOGIN_ID"
PASSWORD_ENV = "SCHWAB_PASSWORD"

# Credentials whose file value overrides anything inherited from
# the host env. See load_env_file for the rationale.
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
        help=("Path to a KEY=VALUE env file to load before resolving "
              "credentials. Defaults to /secrets/schwab-web.env if "
              "present, else ~/.secrets/schwab-web.env."),
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
              "fall back to the VNC-driven flow (the operator "
              "drives Log In + 2FA themselves) — useful if Schwab "
              "restyles the gateway DOM and the CLI selectors miss."),
    )
    p.add_argument(
        "--dest", default=None, type=Path,
        help=("Bronze tree root. Required for the scrape path; the "
              "script chains into download.walk(page, dest, ...) "
              "after login completes. A new <UTC-timestamp>/ run "
              "dir is created under it. Canonical container "
              "path: /data."),
    )
    p.add_argument(
        "--mode", choices=("statements", "transactions", "both"),
        default="both",
        help=("Scrape mode for the post-login walk. "
              "See download.py --mode help. Ignored when --dest "
              "is unset."),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Enumerate accounts and pages but do NOT click any "
              "PDF download button. Forwarded to download.walk()."),
    )
    p.add_argument(
        "--with-more-detail", action="store_true",
        help=("On the Transaction History pass, also click each "
              "row's 'More' link and capture the per-row detail "
              "modal contents. See download.py --with-more-detail "
              "help. Off by default."),
    )
    # Shared --since / --until / --lookback / --documents-since /
    # --documents-until contract. schwab-web's UI is preset-driven
    # (Last3Months / Last6Months / Last5Years / Last10Years), so the
    # CLI maps an explicit --since (or its --lookback shortcut) to
    # the closest preset that covers it. --range is kept as the
    # explicit-preset escape hatch and wins when set.
    cli.add_lookback_args(p)
    p.add_argument(
        "--range", dest="date_range",
        choices=tuple(v for v in schwab.DATE_RANGE_VALUES if v != "Custom"),
        default=None,
        help=("Explicit Schwab Statements preset (escape hatch). "
              "Overrides any --since / --lookback. Common values: "
              "Last3Months (default if no --since/--lookback set), "
              "Last6Months, Last5Years, Last10Years (full backfill)."),
    )
    p.add_argument(
        "--post-auth-timeout", type=int, default=7200,
        help=("Seconds to wait for login + 2FA to complete "
              "via VNC (default: 7200 = 2 hours). The default is "
              "deliberately generous — vnc-login is human-in-the-loop "
              "and the operator should not have to drop everything to "
              "stay under the deadline. Exits with rc=7 on timeout."),
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
              "bronze tree, e.g. the /debug mount). Useful for "
              "debugging the login / landmark flow. NEVER commit "
              "these — see CLAUDE.md §4."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help=("Capture a Playwright trace bundle. Requires "
              "--screenshot-dir; the bundle lands there alongside "
              "screenshots."),
    )
    p.add_argument(
        "-v", "--verbose", action="store_true", help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


# ============================================================
# Env-file loader
# ============================================================

def load_env_file(path: Path) -> None:
    """Source KEY=VALUE pairs from `path` into os.environ.

    For credentials (SCHWAB_LOGIN_ID / SCHWAB_PASSWORD) the file
    value wins over an already-set host env var. Reason: the host
    shell's `source ~/.secrets/schwab-web.env` does $-expansion on
    double-quoted values, so a password like "abc$def!" becomes
    "abc" before the wrapper forwards it via -e SCHWAB_PASSWORD.
    The file itself, read by us byte-for-byte, has the original
    intact. Use SINGLE quotes around values containing $/!/backtick
    to defeat the issue at the source.

    Other vars use setdefault (env-file is a fallback).

    Outer matching quotes (single OR double) are stripped. Lines
    beginning with `#` and blank lines are ignored. Malformed
    lines raise.
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
                    f"env file {path}:{lineno}: not a KEY=VALUE line: "
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
                        "(Use SINGLE quotes for values containing $/!/"
                        "backtick to avoid host `source` mangling.)",
                        key, len(prior), path, len(value),
                    )
                os.environ[key] = value
            else:
                os.environ.setdefault(key, value)


def _strip_outer_quotes(s: str) -> str:
    """Strip a matched pair of leading+trailing single or double
    quotes from `s`. Single-side strips (e.g. `"foo`) are left
    alone — they're more likely a real value than a syntax slip."""
    if len(s) >= 2 and s[0] == s[-1] and s[0] in ("'", '"'):
        return s[1:-1]
    return s


def maybe_source_env_files(args: argparse.Namespace) -> None:
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

def prepare_profile_dir(profile_dir: Path) -> None:
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

def maybe_screenshot(page, screenshot_dir: Path | None, label: str) -> None:
    """Capture page state at a navigation landmark: always save the
    rendered HTML (fast — no font/animation wait), and best-effort
    a viewport screenshot (Firefox `page.screenshot()` against
    schwab.com tends to block on "waiting for fonts to load" which
    never resolves, so we cap it tightly and accept misses).
    Never raises."""
    if screenshot_dir is None:
        return
    try:
        screenshot_dir.mkdir(parents=True, exist_ok=True)
    except Exception as e:
        log.warning("could not create screenshot dir %s: %s", screenshot_dir, e)
        return
    ts = bronze.ts_slug()
    # HTML first. page.content() can fail with "page is navigating
    # and changing" on Angular SPAs; fall back to evaluate which
    # reads the DOM with no such guard.
    try:
        html_path = screenshot_dir / f"{ts}-{label}.html"
        try:
            html = page.content()
        except Exception:
            html = page.evaluate(
                "() => document.documentElement.outerHTML",
            )
        html_path.write_text(html, encoding="utf-8")
        log.debug("wrote HTML %s", html_path)
    except Exception as e:
        log.warning("html capture %s failed: %s", label, e)
    try:
        png_path = screenshot_dir / f"{ts}-{label}.png"
        page.screenshot(
            path=str(png_path), full_page=False,
            timeout=3_000, animations="disabled",
        )
        log.debug("wrote screenshot %s", png_path)
    except Exception as e:
        log.debug("screenshot %s failed (HTML saved): %s", label, e)


def stop_trace_if_active(context, trace: bool, screenshot_dir: Path | None,
                        label: str) -> None:
    if not trace:
        return
    if screenshot_dir is None:
        log.warning("--trace without --screenshot-dir; trace discarded")
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    trace_path = screenshot_dir / f"{bronze.ts_slug()}-{label}-trace.zip"
    context.tracing.stop(path=str(trace_path))
    log.info("trace saved to %s", trace_path)


# ============================================================
# Browser launch helpers
# ============================================================

def wait_for_dom(page, selector: str, timeout_s: float,
                 poll_s: float = 0.5) -> bool:
    """Poll `page.evaluate(document.querySelector(selector))` until
    the element is present (and either visible or the body), or
    `timeout_s` expires.

    Used instead of `locator.wait_for(state="visible")` against the
    Schwab SPA: Schwab's Angular code emits perpetual change-
    detection cycles that confuse Playwright's "is page navigating?"
    guard, so wait_for runs to its full timeout even when the
    element is plainly there. evaluate() has no such guard.
    """
    js = (
        "(sel) => { const e = document.querySelector(sel); "
        "return e && (e.offsetParent !== null || e.tagName === 'BODY') ? 1 : 0; }"
    )
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        try:
            if page.evaluate(js, selector):
                return True
        except Exception as e:
            log.debug("wait_for_dom eval err for %r: %s", selector, e)
        time.sleep(poll_s)
    return False


def wait_for_dom_in_frame(frame_locator, selector: str,
                          timeout_s: float, poll_s: float = 0.5) -> bool:
    """wait_for_dom() variant for a FrameLocator."""
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
    initial page has historically had a partially-bound internal
    state that broke page.evaluate(). new_page() avoids the trap.
    """
    return context.new_page()


@contextlib.contextmanager
def open_camoufox_context(profile_dir: Path, trace: bool):
    """Open Camoufox with a persistent profile dir, yielding the
    BrowserContext. The context auto-closes on exit.

    Camoufox is a stealth-patched Firefox fork that masks the
    fingerprint surfaces (canvas, WebGL, audio, fonts, navigator.*,
    TLS) Schwab's anti-bot scoring uses to detect Playwright-driven
    Firefox. `os="macos"` runs the full macOS-pretend mode so the
    fingerprint is internally consistent — much stronger than the
    piecemeal UA + navigator.platform overrides we used to set on
    upstream Playwright Firefox.

    Headed + window=(1280, 800) avoids Schwab's mobile responsive
    layout; the Xvfb display from entrypoint.sh provides the X11
    surface (camoufox respects DISPLAY when set).
    """
    from camoufox.sync_api import Camoufox
    with Camoufox(
        persistent_context=True,
        user_data_dir=str(profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
    ) as context:
        if trace:
            context.tracing.start(screenshots=True, snapshots=True, sources=True)
        yield context


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
        page.set_default_navigation_timeout(NAV_TIMEOUT_MS)
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
                    "session DEAD: %s — run `./schwab-web login` to mint a new session",
                    url,
                )
                rc = 2
        except PWTimeout as e:
            maybe_screenshot(page, screenshot_dir, "check-timeout")
            log.error("timeout during --check: %s", e)
            rc = 3
        finally:
            stop_trace_if_active(context, trace, screenshot_dir, "check")
        return rc


def run_manual(profile_dir: Path,
               screenshot_dir: Path | None,
               *,
               dest: Path,
               mode: str = "both",
               dry_run: bool = False,
               date_range: str = schwab.DATE_RANGE_DEFAULT,
               with_more_detail: bool = False,
               cli_mfa: bool = True,
               post_auth_timeout_s: int = 600,
               debug: bool = False) -> int:
    """One-shot: CLI-MFA login → scrape → exit.

    Schwab invalidates the persistent profile's session within
    seconds of Firefox closing, so login + scrape must happen
    in one Firefox lifetime — which means every `download`
    invocation pays one MFA challenge. The trade-off is
    operational simplicity: no daemon to babysit, no trigger
    files, no idle timeouts to tune.

    Pre-fill is best-effort and non-fatal; if creds aren't in
    the env we log a warning and (with --no-cli-mfa) let the
    operator drive Log In + 2FA via VNC.
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
    log.info("scrape config: dest=%s mode=%s dry_run=%s range=%s more=%s",
             dest, mode, dry_run, date_range, with_more_detail)

    log.info("opening Firefox")
    with open_camoufox_context(profile_dir, trace=False) as context:
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
                        "in the environment; skipping auto-submit (you'll "
                        "have to drive Log In + 2FA manually)"
                    )
                else:
                    ok = _run_cli_mfa(page, screenshot_dir)
                    if not ok:
                        log.warning(
                            "CLI-MFA path failed; falling back to manual "
                            "drive — open a VNC session (./schwab-web "
                            "vnc-login) and complete the login yourself"
                        )

            if cli_mfa:
                log.info(
                    "2FA submitted; waiting for post-auth landing page "
                    "(/app/...). If anything stalls, open a VNC "
                    "session (./schwab-web vnc-login) to recover."
                )
            else:
                log.info(
                    "Firefox ready. Via VNC: click Log In, enter your "
                    "VIP code, land on Account Summary. Then the script "
                    "takes over and scrapes."
                )
            auth_page = _wait_for_post_auth(
                page, context, post_auth_timeout_s,
            )
            if auth_page is None:
                maybe_screenshot(page, screenshot_dir, "post-auth-timeout")
                log.error(
                    "post-auth URL not detected within %ds; either you "
                    "didn't finish logging in, or Firefox was closed.",
                    post_auth_timeout_s,
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
                "move your mouse out of the browser window",
                _live_url(page),
            )
            # Tiny grace period so the operator has a moment to
            # retract their pointer before Playwright starts
            # dispatching synthetic events. Without it, a stray
            # hover-tooltip on the account selector can intercept
            # the next click.
            time.sleep(3)
            maybe_screenshot(page, screenshot_dir, "post-auth-handoff")
            page.set_default_navigation_timeout(NAV_TIMEOUT_MS)
            import download
            try:
                summary = download.walk(
                    page, dest,
                    mode=mode, dry_run=dry_run,
                    screenshot_dir=screenshot_dir,
                    date_range=date_range,
                    with_more_detail=with_more_detail,
                    debug=debug,
                )
                log.info(
                    "scrape complete: %d statement entries, %d tx entries",
                    len(summary.get("statements", [])),
                    len(summary.get("transactions", [])),
                )
                return 0
            except KeyboardInterrupt:
                log.info("interrupted; closing browser")
                return 0
        except KeyboardInterrupt:
            log.info("interrupted; closing browser")
            return 0
        except Exception as e:
            log.exception("login + scrape failed: %s", e)
            try:
                maybe_screenshot(page, screenshot_dir, "manual-error")
            except Exception:
                pass
            return 1


def _live_url(page) -> str:
    """Read the page's current URL by asking Firefox directly.

    Equivalent to `page.url` EXCEPT under camoufox 135 + playwright
    1.49: juggler doesn't reliably deliver `frameNavigated` events
    to the driver, so `page.url` stays stuck on the pre-redirect
    value after a top-level navigation completes (the Firefox UI
    URL bar updates, but Playwright's cached accessor doesn't).
    `location.href` via evaluate forces Firefox to answer from its
    actual document state, which is what we want for landmark
    detection.

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


def _wait_for_post_auth(page, context, timeout_s: float,
                        poll_s: float = 1.0):
    """Wait for any page in `context` to land on a post-auth URL.

    Schwab's login flow can either redirect the same tab to
    client.schwab.com or open a NEW tab/window for the
    authenticated session, so we enumerate context.pages on each
    poll and return whichever page matches.

    Reads each page's URL via `_live_url` (location.href in-page)
    rather than `p.url` because the camoufox/playwright pin doesn't
    propagate frame-navigated events — `p.url` stays stuck on the
    pre-login value indefinitely.

    Returns the matching Page object, or None on timeout or if the
    operator closed all pages before logging in.
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
        for p, u in zip(pages, urls):
            if schwab.is_post_auth_url(u):
                return p
        time.sleep(poll_s)
    return None


def _submit_login_form(page) -> bool:
    """Submit the login form inside the homepage's `#schwablmslogin`
    iframe. Returns True if any submit path landed.

    Strategy (most-human-like first):
      1. Pause ~1.5s — humans don't click 50ms after the last
         keypress, and Schwab's anti-bot heuristics flag tight
         pre-fill→submit timing.
      2. Press Enter inside the password field. Native HTML form
         submit; no synthetic mouse click; Angular sees a real
         keyboard event that travels through the proper change-
         detection cycle.
      3. If Enter doesn't visibly progress (iframe URL unchanged
         after a poll), fall back to clicking the Log In button.
    """
    gateway = page.frame_locator(f"#{schwab.LOGIN_IFRAME_ID}")
    pre_iframe_url = _iframe_url(page)
    log.debug("pre-submit iframe url: %s", pre_iframe_url)

    # 1) Pre-submit pause.
    time.sleep(1.5)

    # 2) Enter on password field.
    try:
        pwd = gateway.locator(f"#{schwab.PASSWORD_INPUT_ID}")
        if pwd.count() > 0:
            pwd.press("Enter")
            log.info("submitted login form via Enter on password field")
            if _wait_iframe_progress(page, pre_iframe_url, timeout_s=8):
                return True
            log.info("iframe URL unchanged after Enter — trying button click")
    except Exception as e:
        log.debug("Enter on password failed: %s", e)

    # 3) Button click fallback.
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
            log.info("submitted login form via %s selector %s", kind, sel)
            if _wait_iframe_progress(page, pre_iframe_url, timeout_s=8):
                return True
            log.info("iframe URL unchanged after click — trying next candidate")
        except Exception as e:
            log.debug("login button candidate %s (%s) failed: %s", sel, kind, e)
    log.error(
        "form did not visibly submit after all attempts — see debug "
        "screenshots; the iframe may show an error or Schwab may have "
        "silently rejected the submit"
    )
    return False


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


def _dump_visible_form_elements(page, label: str) -> None:
    """Log visible <input> and <button>/[role=button] elements on
    the top-level page. Used when CLI-MFA selectors miss, so the
    next iteration can identify the new ids without burning a
    fresh MFA round to inspect the DOM manually.

    Tags identifiers as keys but NOT values — Schwab's MFA inputs
    are typically empty when this fires, but be safe (CLAUDE.md
    §4: don't leak identifiers anywhere).
    """
    try:
        info = page.evaluate(
            """
            () => {
                const visible = e => {
                    const r = e.getBoundingClientRect();
                    return r.width > 0 && r.height > 0;
                };
                const inputs = [];
                for (const e of document.querySelectorAll('input')) {
                    if (!visible(e)) continue;
                    inputs.push({
                        id: e.id || null,
                        name: e.name || null,
                        type: e.type || null,
                        placeholder: e.placeholder || null,
                        autocomplete: e.autocomplete || null,
                        maxlength: e.maxLength > 0 ? e.maxLength : null,
                    });
                }
                const buttons = [];
                for (const e of document.querySelectorAll('button, [role="button"]')) {
                    if (!visible(e)) continue;
                    buttons.push({
                        id: e.id || null,
                        type: e.type || null,
                        text: (e.textContent || '').trim().slice(0, 60) || null,
                    });
                }
                return {url: location.href, inputs, buttons};
            }
            """
        )
        log.info("DOM snapshot at %s: url=%s", label, info.get("url"))
        for x in info.get("inputs") or []:
            log.info("  input %s", x)
        for x in info.get("buttons") or []:
            log.info("  button %s", x)
    except Exception as e:
        log.debug("DOM snapshot at %s failed: %s", label, e)


def _dump_iframe_state(page) -> None:
    """Log every attached frame's URL plus a count of visible
    <input> and <button> elements inside each. Used when MFA
    selectors miss — tells us where in the frame tree the
    challenge actually landed.
    """
    try:
        for f in page.frames:
            try:
                summary = f.evaluate(
                    """
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
                        const buttons = [...document.querySelectorAll(
                            'button, [role="button"]')]
                            .filter(visible)
                            .map(e => ({
                                id: e.id || null,
                                type: e.type || null,
                                text: (e.textContent || '').trim().slice(0,60) || null,
                            }));
                        return {inputs, buttons};
                    }
                    """
                )
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
    / if the page already redirected to a post-auth URL."""
    deadline = time.monotonic() + timeout_s
    last_log = 0.0
    while time.monotonic() < deadline:
        if schwab.is_post_auth_url(_live_url(page)):
            log.info(
                "post-auth URL detected without MFA challenge — "
                "device already trusted or no 2FA required"
            )
            return None
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
    # Bookended by blanks so the prompt stands out in a busy log.
    sys.stderr.write("\n")
    sys.stderr.write("=" * 60 + "\n")
    sys.stderr.write("Schwab 2FA: enter your VIP / SMS code, then press Enter.\n")
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


def _submit_mfa_code(page, code_locator, code: str) -> bool:
    """Fill the MFA input with `code` and click the Continue
    button (trying each candidate selector). Returns True if a
    button click landed, False on no-match (caller may fall back
    to pressing Enter inside the input)."""
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
            return True
        except Exception as e:
            log.debug("continue button candidate %s failed: %s", sel, e)
    log.warning("no Continue button matched; pressing Enter in the input")
    try:
        code_locator.press("Enter")
        return True
    except Exception as e:
        log.error("could not press Enter to submit 2FA: %s", e)
        return False


def _run_cli_mfa(page, screenshot_dir: Path | None,
                  mfa_wait_s: float = 300) -> bool:
    """Auto-submit the login form, wait for the 2FA page, prompt
    for the code, fill, click Continue. Returns True on success
    (or if Schwab skipped 2FA because the device is already
    trusted), False on any step that failed in a way that warrants
    falling back to the VNC-driven flow.

    Does NOT wait for the post-auth landing page — the caller's
    existing `_wait_for_post_auth()` polls for that.
    """
    maybe_screenshot(page, screenshot_dir, "pre-login-submit")
    if not _submit_login_form(page):
        maybe_screenshot(page, screenshot_dir, "submit-failed")
        _dump_visible_form_elements(page, "login-submit-failed")
        return False
    # Two captures: immediate (during transition) and ~3s later
    # (after navigation settles) so we can see what Schwab served
    # without burning a fresh MFA round to inspect manually.
    maybe_screenshot(page, screenshot_dir, "post-login-submit-immediate")
    time.sleep(3)
    maybe_screenshot(page, screenshot_dir, "post-login-submit-settled")

    hit = _wait_for_mfa_input(page, timeout_s=mfa_wait_s)
    if hit is None:
        # Either the device was trusted (caller will see post-auth
        # URL and proceed) or the input never appeared. Distinguish
        # by re-checking the URL.
        if schwab.is_post_auth_url(_live_url(page)):
            return True
        log.error(
            "no MFA input field appeared within %ds; "
            "Schwab may have served a different challenge (security "
            "question, push-to-device prompt, etc.) — fall back to "
            "--no-cli-mfa and drive via VNC", mfa_wait_s,
        )
        _dump_visible_form_elements(page, "mfa-input-not-found")
        _dump_iframe_state(page)
        return False

    sel, loc = hit
    log.info("MFA code input found (%s); prompting for code on stdin", sel)
    maybe_screenshot(page, screenshot_dir, "mfa-prompt")
    code = _prompt_for_mfa_code()
    if not code:
        log.error("no 2FA code entered; aborting login")
        return False
    if not _submit_mfa_code(page, loc, code):
        return False
    maybe_screenshot(page, screenshot_dir, "post-mfa-submit")
    return True


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
        log.warning("pre-fill failed (%s); type the credentials yourself", e)


# ============================================================
# Entry point
# ============================================================

def _since_to_schwab_preset(since, until) -> str:
    """Map a (since, until) date pair to the closest Statements preset.

    Schwab's filter is preset-driven, not date-range, so the shared
    --since/--lookback contract translates to whichever preset
    fully covers the requested window. Same buckets the old
    SCHWAB_PRESET dict in wealthdb-refresh used."""
    days = (until - since).days
    if days <= 90:
        return "Last3Months"
    if days <= 180:
        return "Last6Months"
    if days < 1825:  # i.e. up to and incl. 4y; 5y → Last10Years per legacy mapping
        return "Last5Years"
    return "Last10Years"


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir (see CLAUDE.md §3).")
    # Translate the shared date-window contract into a Schwab preset.
    # --range wins (explicit-preset escape hatch); otherwise pick the
    # preset that covers the resolved (since, until) window.
    if args.date_range is None:
        since, until, _, _ = cli.resolve_lookback(args)
        args.date_range = _since_to_schwab_preset(since, until)
    maybe_source_env_files(args)
    prepare_profile_dir(args.profile_dir)
    if args.check:
        return run_check(args.profile_dir, args.screenshot_dir, args.trace)
    if args.dest is None:
        raise SystemExit(
            "--dest is required for the scrape path (or pass --check "
            "to validate the existing profile)."
        )
    return run_manual(
        args.profile_dir, args.screenshot_dir,
        dest=args.dest,
        mode=args.mode,
        dry_run=args.dry_run,
        date_range=args.date_range,
        with_more_detail=args.with_more_detail,
        cli_mfa=args.cli_mfa,
        post_auth_timeout_s=args.post_auth_timeout,
        debug=args.debug,
    )


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
