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

from collectorkit import cli, envfile, session

log = logging.getLogger("schwab-login")

# Schwab refresh tokens are valid for 7 days from issue. Surfaced as a
# constant so --check output matches the actual cap.
REFRESH_TOKEN_TTL_SECONDS = 7 * 24 * 3600

PROFILE_DIR_MODE = 0o700

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
        help="Write HTML/screenshots (and the trace, with --trace) here. "
             "NEVER commit these — see CLAUDE.md §4.",
    )
    p.add_argument(
        # BooleanOptionalAction yields --trace / --no-trace, so the trace the
        # entrypoint injects on login can be turned off through the wrapper.
        "--trace", action=argparse.BooleanOptionalAction, default=False,
        help="Capture a Playwright trace bundle (requires --screenshot-dir); "
             "--no-trace disables it (the entrypoint injects --trace on login).",
    )
    p.add_argument(
        "--explore", action="store_true",
        help="Debug aid: dump HTML+PNG of each distinct page during the "
             "wait (to --screenshot-dir) for mapping the OAuth flow / "
             "pinning the account-checkbox selectors.",
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


def prepare_profile_dir(profile_dir: Path) -> None:
    profile_dir.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(profile_dir, PROFILE_DIR_MODE)
    except OSError as exc:
        log.warning("could not chmod %s to 0%o: %s",
                    profile_dir, PROFILE_DIR_MODE, exc)


@contextlib.contextmanager
def open_camoufox_context(profile_dir: Path, trace: bool):
    """Open Camoufox (stealth-patched Firefox) with a persistent profile,
    headed (Schwab flags headless), on the Xvfb display the entrypoint
    provides. Mirrors schwab-web's launch."""
    from camoufox.sync_api import Camoufox
    with Camoufox(
        persistent_context=True,
        user_data_dir=str(profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
    ) as context:
        if trace:
            context.tracing.start(screenshots=True, snapshots=True,
                                  sources=True)
        yield context


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
        (screenshot_dir / f"{ts}-{label}.html").write_text(
            page.content(), encoding="utf-8")
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
    sys.stderr.write("\n" + "=" * 60 + "\n")
    sys.stderr.write("Schwab 2FA: enter your code, then press Enter.\n> ")
    sys.stderr.flush()
    try:
        code = sys.stdin.readline()
    except KeyboardInterrupt:
        sys.stderr.write("\n")
        raise
    sys.stderr.write("=" * 60 + "\n")
    sys.stderr.flush()
    return code.strip()


def _wait_for_mfa_input(page, timeout_s: float, callback_url: str,
                        poll_s: float = 0.5):
    """Poll for the first visible 2FA input across the page + frames.
    Returns a Locator, or None on timeout / if we already reached the
    callback (no 2FA needed)."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if lm.is_callback_url(_live_url(page), callback_url):
            return None
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
        time.sleep(poll_s)
    return None


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
            page.content(), encoding="utf-8")
        try:
            page.screenshot(path=str(screenshot_dir / f"{label}.png"),
                            full_page=True, timeout=4_000)
        except Exception:
            pass
        log.info("explore: captured distinct page %s", label)
    except Exception as e:
        log.debug("explore capture failed: %s", e)


def _drive_consent(page) -> None:
    """For --cli-mfa: tick the Terms agreement box (on the T&C page) and
    the account boxes (on the link page), then click the advance button
    (Continue / Done / Allow). Never clicks Cancel; .check() never
    unchecks. Best-effort — the step can be driven by hand over VNC."""
    try:
        if page.get_by_text(lm.TERMS_HEADING, exact=False).count() > 0:
            _check_visible_checkboxes(page)
    except Exception:
        pass
    select_all_link_accounts(page)
    _click_first(page, lm.ADVANCE_BUTTON_CANDIDATES)


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
    with `drive` (--cli-mfa), also click through the consent pages; with
    `explore`, dump each distinct page's DOM."""
    deadline = time.monotonic() + timeout_s
    last_log = 0.0
    seen_link = [False]
    explore_state = [set(), 0]
    while time.monotonic() < deadline:
        if captured:
            return captured[0]
        url = _live_url(page)
        if lm.is_callback_url(url, callback_url):
            return url
        if explore:
            _explore_dump(page, screenshot_dir, explore_state)
        select_all_link_accounts(page, screenshot_dir, seen_link)
        if drive:
            _drive_consent(page)
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
        raise SystemExit("schwab-py is not installed.")

    client_id = envfile.resolve_credential(args.client_id, "SCHWAB_CLIENT_ID",
                                   "--client-id")
    client_secret = envfile.resolve_credential(args.client_secret,
                                       "SCHWAB_CLIENT_SECRET",
                                       "--client-secret")
    login_id = os.environ.get("SCHWAB_LOGIN_ID")
    password = os.environ.get("SCHWAB_PASSWORD")

    args.token_path.parent.mkdir(parents=True, exist_ok=True)
    prepare_profile_dir(args.profile_dir)

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
        page = context.new_page()
        try:
            page.goto(ctx.authorization_url, wait_until="domcontentloaded")
        except Exception as e:
            log.warning("initial navigation reported: %s", e)
        maybe_capture_html(page, args.screenshot_dir, "01-authorize")
        prefill_login(page, login_id, password)
        maybe_capture_html(page, args.screenshot_dir, "02-prefilled")

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

        received_url = _wait_for_callback(page, captured, args.callback_url,
                                          args.mfa_timeout,
                                          args.screenshot_dir, args.cli_mfa,
                                          args.explore)
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


def _attempt_cli_mfa(page, args: argparse.Namespace) -> None:
    """Experimental: auto-submit login, prompt for 2FA on stdin, click
    through consent. Best-effort — on selector drift the flow can be
    driven by hand over VNC and the redirect capture still completes."""
    if not _click_first(page, lm.LOGIN_SUBMIT_CANDIDATES):
        log.warning("login submit button not found; complete login over VNC")
        return
    code_loc = _wait_for_mfa_input(page, args.mfa_page_timeout, args.callback_url)
    if code_loc is None:
        log.info("no 2FA prompt detected (trusted device or already past it)")
    else:
        code = _prompt_for_mfa_code()
        if code:
            try:
                code_loc.fill(code)
            except Exception as e:
                log.warning("could not fill 2FA code: %s", e)
            if not _click_first(page, lm.MFA_CONTINUE_BUTTON_CANDIDATES):
                try:
                    code_loc.press("Enter")
                except Exception as e:
                    log.warning("could not submit 2FA: %s", e)
    # The Terms / account-selection / review pages are then driven from
    # the wait loop (_drive_consent), which ticks the boxes and advances.


def cmd_login_manual(args: argparse.Namespace) -> int:
    """No-browser fallback: schwab-py prints the auth URL and reads the
    pasted redirect URL from stdin."""
    try:
        from schwab import auth as schwab_auth
    except ImportError:
        raise SystemExit("schwab-py is not installed.")
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
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir (see CLAUDE.md §4).")
    if args.check:
        return cmd_check(args)
    source_env_files(args.env_file)
    if args.manual:
        return cmd_login_manual(args)
    return cmd_login_browser(args)


if __name__ == "__main__":
    sys.exit(main())
