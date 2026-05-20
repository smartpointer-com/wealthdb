#!/usr/bin/env python3
"""
Schwab client-web session minter.

Drives Firefox (headed, against an Xvfb virtual display managed
by entrypoint.sh) through the Schwab login. The credential submit
and Symantec VIP 2FA are completed by the operator over VNC
because Schwab's anti-bot rules reject any non-trivially-automated
login. This script opens the browser, pre-fills the login form for
ergonomics, polls for the post-auth `/app/...` URL, and (when
`--dest` is set) takes over the same Firefox page to run
`download.walk()` in the same continuous session. Schwab kills
the session on Firefox close, so login + scrape must happen in
one Firefox lifetime — close-then-reopen does not work.

Modes:
  --check      validate the persisted profile against the
               Account Summary URL. Logs the cookie jar including
               _abck trust state. No credential submit.
  --manual     open Firefox, pre-fill from SCHWAB_LOGIN_ID /
               SCHWAB_PASSWORD, wait for the operator to drive
               Log In + 2FA. With --dest, hand off to
               download.walk() after post-auth detection;
               without --dest, just block on Firefox close.

Browser choice: Firefox rather than Chromium. Schwab's Akamai
rejects every Chromium-family automation surface we tried
(headless Chromium, patchright-patched Chromium, real Chrome
unavailable on Linux ARM64). See README.md "Browser choice" for
the full diagnostic chain.

Usage:
    login.py --profile-dir <dir>
             [--env-file <path>]
             [--check] [--manual]
             [--screenshot-dir <dir>] [--trace]
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

import landmarks as schwab

log = logging.getLogger("schwab-web-dump.login")

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
# Note the asymmetry vs. schwab-dump (Trader API), which uses
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
              "trust state. No credential submit, no 2FA."),
    )
    p.add_argument(
        "--manual", action="store_true",
        help=("Open Firefox at the Schwab homepage, pre-fill the "
              "login form from $SCHWAB_LOGIN_ID / $SCHWAB_PASSWORD, "
              "and wait for the operator to log in via VNC. After "
              "the URL hits /app/... the script takes over the same "
              "page and (if --dest is set) runs download.walk() in "
              "the same Firefox session. Without --dest it just "
              "blocks on Firefox close — Schwab kills the session "
              "on close, so for any actual scrape pass --dest."),
    )
    p.add_argument(
        "--dest", default=None, type=Path,
        help=("Bronze tree root. When set together with --manual, "
              "the script chains into download.walk(page, dest, ...) "
              "after the operator finishes login. A new "
              "<UTC-timestamp>/ run dir is created under it. "
              "Canonical container path: /data."),
    )
    p.add_argument(
        "--mode", choices=("statements", "transactions", "both"),
        default="both",
        help=("Scrape mode for the auto-scrape after manual login. "
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
    p.add_argument(
        "--range", dest="date_range",
        choices=tuple(v for v in schwab.DATE_RANGE_VALUES if v != "Custom"),
        default=schwab.DATE_RANGE_DEFAULT,
        help=("Date-range preset for the Statements filter. "
              "See download.py --range help. Default %(default)s "
              "= longest preset = all available."),
    )
    p.add_argument(
        "--rerun-trigger", default="/data/.rerun", type=Path,
        help=("File whose mtime change signals 'reload download / "
              "landmarks and re-run walk() against the live "
              "Firefox session' — lets iterations of the scrape "
              "code run without re-MFA. Default %(default)s. "
              "From the host, `./schwab-web-dump rerun` touches it. "
              "Pass '' to disable the loop (script exits after one "
              "scrape, browser closes, Schwab session dies)."),
    )
    p.add_argument(
        "--post-auth-timeout", type=int, default=7200,
        help=("Seconds to wait for the user to complete login + 2FA "
              "via VNC (default: 7200 = 2 hours). The default is "
              "deliberately generous — vnc-login is human-in-the-loop "
              "and the operator should not have to drop everything to "
              "stay under the deadline. Exits with rc=7 on timeout."),
    )
    p.add_argument(
        "--screenshot-dir", default=None, type=Path,
        help=("If set, write a screenshot + HTML capture at each "
              "navigation landmark. Useful for debugging. NEVER "
              "commit these — see CLAUDE.md §4."),
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

def ts_slug() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


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
    ts = ts_slug()
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
    trace_path = screenshot_dir / f"{ts_slug()}-{label}-trace.zip"
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
                    "session DEAD: %s — re-run vnc-login to mint a new session",
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
               dest: Path | None = None,
               mode: str = "both",
               dry_run: bool = False,
               date_range: str = schwab.DATE_RANGE_DEFAULT,
               with_more_detail: bool = False,
               rerun_trigger: Path | None = None,
               post_auth_timeout_s: int = 600) -> int:
    """Open Firefox at the homepage, pre-fill the login form, and
    wait for the operator (driving via VNC) to complete login.

    Two modes depending on `dest`:

    - dest is None: block on Firefox close. The operator does
      whatever they want with the browser; on close the profile
      is persisted to disk. (Note: Schwab kills the session on
      close, so the persisted profile won't authenticate the
      next run.)

    - dest is set: poll the URL for `/app/...` (post-auth
      landing). When detected, take over the same `page` and run
      `download.walk(page, dest, ...)` in the same Firefox
      session — the only path that produces bronze data, since
      Schwab's session can't be reopened from disk.

    Either way pre-fill is best-effort and non-fatal.
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
    if dest is not None:
        log.info(
            "auto-scrape after login: dest=%s mode=%s dry_run=%s",
            dest, mode, dry_run,
        )

    log.info("opening Firefox for manual driving")
    with open_camoufox_context(profile_dir, trace=False) as context:
        page = open_page(context)
        page.on("pageerror", lambda exc: log.warning("browser pageerror: %s", exc))
        try:
            page.goto(schwab.MARKETING_HOMEPAGE, wait_until="domcontentloaded")
            if will_prefill:
                _prefill_login_iframe(page, login_id_value, password_value)

            if dest is None:
                log.info(
                    "Firefox ready. Via VNC: drive the browser yourself. "
                    "Close the Firefox window to exit."
                )
                try:
                    page.wait_for_event("close", timeout=0)
                except KeyboardInterrupt:
                    log.info("interrupted; closing browser")
                maybe_screenshot(page, screenshot_dir, "manual-final")
                return 0

            # Auto-scrape path: wait for the operator to finish
            # logging in, then take over.
            log.info(
                "Firefox ready. Via VNC: click Log In, enter your VIP "
                "code, land on Account Summary. Then the script will "
                "take over and scrape. Do NOT close the window yourself; "
                "the script closes it when the scrape is done."
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
            # Tiny grace period so the operator (still on VNC) has
            # a moment to retract their pointer before Playwright
            # starts dispatching synthetic events. Without it, a
            # stray hover-tooltip on the account selector can
            # intercept the next click.
            time.sleep(3)
            maybe_screenshot(page, screenshot_dir, "post-auth-handoff")
            # Late import so download's top-level setup runs after
            # logging is configured; importlib.reload(download)
            # below picks up host-side edits between iterations.
            import importlib
            import download
            page.set_default_navigation_timeout(NAV_TIMEOUT_MS)

            iteration = 0
            last_mtime = (
                rerun_trigger.stat().st_mtime
                if rerun_trigger and rerun_trigger.exists()
                else None
            )
            while True:
                iteration += 1
                log.info("=== scrape iteration %d ===", iteration)
                try:
                    summary = download.walk(
                        page, dest,
                        mode=mode, dry_run=dry_run,
                        screenshot_dir=screenshot_dir,
                        date_range=date_range,
                        with_more_detail=with_more_detail,
                    )
                    log.info(
                        "iteration %d complete: %d statement entries, "
                        "%d tx entries",
                        iteration,
                        len(summary.get("statements", [])),
                        len(summary.get("transactions", [])),
                    )
                except KeyboardInterrupt:
                    raise
                except Exception as e:
                    log.exception(
                        "iteration %d failed: %s — keeping session "
                        "alive for re-run", iteration, e,
                    )

                if rerun_trigger is None:
                    log.info(
                        "no --rerun-trigger; exiting after one iteration"
                    )
                    return 0

                log.info(
                    "iteration %d done. To re-run with fresh code: "
                    "edit host files, then `./schwab-web-dump rerun` "
                    "(touches %s).",
                    iteration, rerun_trigger,
                )
                # Block until the trigger file's mtime advances. If
                # the file doesn't exist yet, treat its creation as
                # the trigger.
                while True:
                    if rerun_trigger.exists():
                        cur = rerun_trigger.stat().st_mtime
                        if last_mtime is None or cur > last_mtime:
                            last_mtime = cur
                            break
                    time.sleep(2)
                # Reload landmarks first (download imports it as
                # `schwab`), then download itself, so download
                # picks up the fresh landmarks values.
                importlib.reload(schwab)
                importlib.reload(download)
                # Apply any per-iteration overrides written into
                # the trigger file by `./schwab-web-dump rerun
                # --flag ...`. Empty file = inherit current
                # settings.
                dry_run, mode, date_range, with_more_detail = (
                    _apply_rerun_overrides(
                        rerun_trigger, dry_run, mode, date_range,
                        with_more_detail,
                    )
                )
                log.info(
                    "reloaded landmarks + download; running iteration "
                    "%d (dry_run=%s mode=%s range=%s more=%s)",
                    iteration + 1, dry_run, mode, date_range,
                    with_more_detail,
                )
        except KeyboardInterrupt:
            log.info("interrupted; closing browser")
            return 0
        except Exception as e:
            log.exception("manual+scrape flow failed: %s", e)
            try:
                maybe_screenshot(page, screenshot_dir, "manual-error")
            except Exception:
                pass
            return 1


def _apply_rerun_overrides(trigger: Path,
                           dry_run: bool, mode: str, date_range: str,
                           with_more_detail: bool,
                           ) -> tuple[bool, str, str, bool]:
    """Parse the rerun-trigger file's content (if non-empty) and
    return updated (dry_run, mode, date_range, with_more_detail).
    Empty file = inherit current values.

    Supported keys (one per line, `key=value`):
        dry_run            = true | false
        mode               = statements | transactions | both
        date_range         = any preset accepted by download.walk()
        with_more_detail   = true | false

    Unknown keys are logged and ignored. Parse failures fall back
    to the inherited value — the loop never raises here, since a
    bad config shouldn't terminate the live Firefox session.
    """
    try:
        content = trigger.read_text(encoding="utf-8").strip()
    except Exception as e:
        log.debug("could not read rerun trigger %s: %s", trigger, e)
        return dry_run, mode, date_range, with_more_detail
    if not content:
        return dry_run, mode, date_range, with_more_detail

    def _bool(v: str) -> bool:
        return v.lower() in ("1", "true", "yes")

    for raw in content.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            log.warning("rerun config: skipping malformed line %r", line)
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip()
        if key == "dry_run":
            new_val = _bool(value)
            if new_val != dry_run:
                log.info("rerun config: dry_run %s → %s", dry_run, new_val)
            dry_run = new_val
        elif key == "with_more_detail":
            new_val = _bool(value)
            if new_val != with_more_detail:
                log.info("rerun config: with_more_detail %s → %s",
                         with_more_detail, new_val)
            with_more_detail = new_val
        elif key == "mode":
            if value in ("statements", "transactions", "both"):
                if value != mode:
                    log.info("rerun config: mode %s → %s", mode, value)
                mode = value
            else:
                log.warning("rerun config: ignoring bad mode %r", value)
        elif key == "date_range":
            if value != date_range:
                log.info("rerun config: date_range %s → %s",
                         date_range, value)
            date_range = value
        else:
            log.warning("rerun config: ignoring unknown key %r", key)
    return dry_run, mode, date_range, with_more_detail


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


def _prefill_login_iframe(page, login_id_value: str, password_value: str) -> None:
    """Pre-fill #loginIdInput + #passwordInput inside the homepage's
    `#schwablmslogin` iframe. Non-fatal on failure — if anything
    goes wrong the user can still type into the form themselves."""
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

def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir (see CLAUDE.md §3).")
    if not (args.check or args.manual):
        raise SystemExit(
            "specify --check or --manual. Automated form-submit is not "
            "supported (Schwab anti-bot rejects it); manual VNC drive is "
            "the only path that works."
        )
    if args.check and args.manual:
        raise SystemExit("--check and --manual are mutually exclusive")
    maybe_source_env_files(args)
    prepare_profile_dir(args.profile_dir)
    if args.check:
        return run_check(args.profile_dir, args.screenshot_dir, args.trace)
    # Empty-string sentinel disables the keep-alive loop; treat
    # the resulting Path('.') as opt-out.
    rerun_trigger = args.rerun_trigger
    if str(rerun_trigger) in ("", "."):
        rerun_trigger = None
    return run_manual(
        args.profile_dir, args.screenshot_dir,
        dest=args.dest,
        mode=args.mode,
        dry_run=args.dry_run,
        date_range=args.date_range,
        with_more_detail=args.with_more_detail,
        rerun_trigger=rerun_trigger,
        post_auth_timeout_s=args.post_auth_timeout,
    )


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
