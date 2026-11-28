#!/usr/bin/env python3
"""George sign-in + fetch: the one-shot behind `download` and `vnc-login`.

It also holds the shared authenticate() and the REST helper download.py
reads through.

George keeps no session and no device trust across a browser restart
(DESIGN.md §F): every sign-in fires a push to the George app. So the
sign-in and the fetch run in **one browser lifetime**, and the `login` verb
folds into `download`; this script backs both.

The sign-in (DESIGN.md §A-§B) is an OAuth implicit grant on
`login.sparkasse.at`:

1. George redirects to the sign-in page, which asks for the user name only.
   login.py fills it from the env file, reads it back, and submits.
2. The push fires at once. The wait screen shows a 4-character comparison
   code, which is read from the DOM and printed to the terminal. The page
   polls for the approval on its own.
3. On approval the page redirects to George with the bearer in the URL
   fragment (`#access_token=…`).

The bearer is taken from two independent places: the page URL, polled, and
the `Authorization` header of the SPA's first data calls, harvested from
request events. Either one is only a candidate. **The sign-in counts as done
only when the candidate makes a read succeed** (`GET …/accounts`), never on
a URL or route. No data endpoint is called before a candidate exists, and
one exists only after the approval, so nothing touches the data API while
the push is pending.

Modes:
  --check   open the profile and probe for a live session; exit 0 (alive) /
            1 (dead). Dead between runs is expected (§F); fires no push.
  default   sign in from the terminal (push approved on the phone), then
            fetch via download.walk() into --bronze-dir.
  --no-cli-mfa  the vnc-login fallback: the whole sign-in is done by hand
            over VNC, then the same fetch runs.

Read-only (AGENTS.md): the browser only completes the sign-in. The data is
read over REST. Never a money-movement, card, or settings surface.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import os
import sys
import time
import uuid
from pathlib import Path
from urllib.parse import urlsplit

from collectorkit import cli, debugcap, envfile, launch
from collectorkit import explore as kit_explore
from collectorkit.launch import page_url, pump, wait_for

import george_client as george
from explore import DEFAULT_ENV_FILE, DEFAULT_PROFILE_DIR, USER_ENV

log = logging.getLogger("erstebank_at.login")

# How long to wait for the sign-in form to render, and for the wait screen
# once the form is submitted. The push wait is the fleet's five minutes
# (root AGENTS.md §9); the wait screen shows no shorter deadline (§B).
FORM_TIMEOUT_S = 90
CHALLENGE_SCREEN_TIMEOUT_S = 30
MFA_TIMEOUT_S = 300
# Back on George without a bearer for this long, the page is reloaded once
# so the SPA's data calls (and their headers) run again.
NO_TOKEN_RELOAD_S = 15


# --- browser plumbing -----------------------------------------------------

@contextlib.contextmanager
def camoufox(profile_dir: Path, fresh: bool = False):
    """A persistent Camoufox context on the profile dir, headed under the
    entrypoint's Xvfb (VNC only for vnc-login). Nothing in the profile
    skips the push (§F); `fresh` wipes it anyway, as on every verb that
    offers it. Yields (context, page)."""
    kit_explore.prepare_profile(
        profile_dir, fresh=fresh, log=log,
        note="George keeps no device trust, so the sign-in is unchanged")
    with launch.persistent_camoufox(profile_dir) as (context, page):
        yield context, page


class Watch:
    """Event-side signals of the sign-in. Each has a polled twin in
    authenticate(), because the pinned Camoufox can drop events across the
    sign-in's navigations.

    * `token` — a bearer candidate, from the `Authorization` header of a
      data-API request, a frame navigating to the George redirect URL, or
      the redirect's `Location` header.
    * `push_status` — the latest `secondFactorStatus` of the page's own
      push poll. Diagnostic only; nothing waits on it.
    """

    def __init__(self):
        self.token: str | None = None
        self.push_status: str | None = None

    def attach(self, context):
        context.on("request", self._on_request)
        context.on("response", self._on_response)
        context.on("framenavigated", self._on_frame)
        return self

    def offer(self, token: str | None) -> None:
        if token:
            self.token = token

    def _on_request(self, request):
        with contextlib.suppress(Exception):
            if urlsplit(request.url).netloc == urlsplit(george.API_ORIGIN).netloc:
                self.offer(george.bearer_from_header(
                    request.headers.get("authorization")))

    def _on_response(self, response):
        with contextlib.suppress(Exception):
            url = response.url
            if (george.SECONDFACTOR_PATH in url
                    and response.request.method == "POST"):
                status = george.secondfactor_status(response.json())
                if status:
                    self.push_status = status
            elif 300 <= response.status < 400:
                self.offer(george.token_from_url(
                    response.headers.get("location", "")))

    def _on_frame(self, frame):
        with contextlib.suppress(Exception):
            self.offer(george.token_from_url(frame.url))


# --- REST over the browser context ----------------------------------------

class Api:
    """The data API, called over the browser context's `request` API (it
    shares the cookie jar the API also wants, §C) with the bearer added.
    The headers mirror the SPA's own calls."""

    def __init__(self, context, token: str):
        self.context = context
        self.token = token

    def _headers(self, **extra: str) -> dict:
        return {"Authorization": f"Bearer {self.token}",
                "Accept": "*/*",
                "Origin": f"https://{george.GEORGE_HOST}",
                "Referer": f"https://{george.GEORGE_HOST}/",
                "X-Request-Id": str(uuid.uuid4()),
                **extra}

    def get_json(self, url: str) -> tuple[int, object]:
        """GET `url`; return (status, parsed JSON or None)."""
        resp = self.context.request.get(url, headers=self._headers())
        return resp.status, _json_or_none(resp)

    def post_token_form(self, url: str) -> tuple[int, bytes, object]:
        """POST the form the SPA sends to a document download: the bearer
        as `access_token`, nothing else (§E, §G). Returns (status, body
        bytes, parsed JSON or None — set on an error answer)."""
        resp = self.context.request.post(
            url, headers=self._headers(), form={"access_token": self.token})
        body = resp.body()
        parsed = _json_or_none(resp) if resp.status >= 400 else None
        return resp.status, body, parsed


def _json_or_none(resp) -> object:
    with contextlib.suppress(Exception):
        return resp.json()
    return None


def probe(context, token: str | None) -> bool:
    """The signed-in proof: the bearer reads the checking roster (§C)."""
    if not token:
        return False
    with contextlib.suppress(Exception):
        status, body = Api(context, token).get_json(george.accounts_url())
        return status == 200 and isinstance(body, dict) and "accounts" in body
    return False


# --- the sign-in page -----------------------------------------------------

def load_username(env_file: Path) -> str:
    if envfile.source_env_file(env_file):
        log.info("env file: %s (sourced)", env_file)
    return os.environ.get(USER_ENV, "")


def _visible(page, selector: str) -> bool:
    with contextlib.suppress(Exception):
        loc = page.locator(selector)
        return loc.count() > 0 and loc.first.is_visible()
    return False


def _text(page, selector: str) -> str:
    with contextlib.suppress(Exception):
        loc = page.locator(selector)
        if loc.count() and loc.first.is_visible():
            return " ".join((loc.first.inner_text() or "").split())
    return ""


def read_error(page) -> str:
    """The sign-in page's own error text, verbatim, or ''."""
    for selector in george.SEL_ERRORS:
        text = _text(page, selector)
        if text:
            return text
    return ""


def read_code(page) -> str | None:
    """The comparison code on the push wait screen, read from the DOM."""
    text = _text(page, george.SEL_VERIFICATION_CODE)
    return text if george.looks_like_code(text) else None


def _token_now(page, watch: Watch) -> str | None:
    """The bearer candidate: the event-side one, or the page URL's."""
    watch.offer(george.token_from_url(page_url(page)))
    return watch.token


def _submit_username(page, username: str, args) -> bool:
    """Fill the user-name form, verify by read-back, and submit."""
    if _visible(page, george.SEL_COOKIE_CLOSE):
        with contextlib.suppress(Exception):
            page.locator(george.SEL_COOKIE_CLOSE).first.click(timeout=3000)
    field = page.locator(george.SEL_USER).first
    with contextlib.suppress(Exception):
        field.click(timeout=4000)
        field.fill("")
        field.fill(username)
    got = ""
    with contextlib.suppress(Exception):
        got = field.input_value()
    if got != username:
        _capture(page, args, "username-fill-failed")
        log.error("could not fill the user-name field. Retry with "
                  "vnc-login. (DOM captured with --debug.)")
        return False
    with contextlib.suppress(Exception):
        page.locator(george.SEL_SUBMIT).first.click(timeout=5000)
        return True
    with contextlib.suppress(Exception):
        field.press("Enter")
        return True
    _capture(page, args, "submit-failed")
    log.error("could not submit the sign-in form. Retry with vnc-login. "
              "(DOM captured with --debug.)")
    return False


def _drive_to_push(context, page, watch: Watch, args) -> bool:
    """Front half of a terminal sign-in: open George, wait for the
    user-name form, submit the user name, and announce the push with its
    comparison code. Returns True once the push is pending."""
    username = load_username(args.env_file)
    if not username:
        log.error("%s is not set in %s. Set it, or sign in by hand with "
                  "vnc-login.", USER_ENV, args.env_file)
        return False
    page.goto(george.START_URL, wait_until="domcontentloaded",
              timeout=60_000)
    if not wait_for(lambda: _visible(page, george.SEL_USER)
                    or _token_now(page, watch) is not None,
                    page, FORM_TIMEOUT_S):
        _capture(page, args, "no-sign-in-form")
        log.error("the sign-in form did not appear. Retry with vnc-login. "
                  "(DOM captured with --debug.)")
        return False
    if watch.token is not None:
        return True                       # signed in already; probed later
    if not _submit_username(page, username, args):
        return False

    code = None
    error = ""

    def challenge_up() -> bool:
        nonlocal code, error
        code = read_code(page)
        error = read_error(page)
        return bool(code or error or _token_now(page, watch))

    wait_for(challenge_up, page, CHALLENGE_SCREEN_TIMEOUT_S)
    if error:
        _capture(page, args, "sign-in-refused")
        log.error("George refused the sign-in: %s", error)
        return False
    if code:
        log.info("Push sent — approve the sign-in in the George app (or "
                 "George ID). Comparison code: %s", code)
    else:
        log.info("Push sent — approve the sign-in in the George app (or "
                 "George ID).")
    return True


def authenticate(context, page, watch: Watch, *, args, cli_mfa: bool,
                 mfa_timeout: int = MFA_TIMEOUT_S) -> str | None:
    """Take a sign-in to a working bearer and return it, or None.

    * `cli_mfa=True` (`download`): the user name is submitted from the env
      file and the push is announced in the terminal with its code.
    * `cli_mfa=False` (`vnc-login`): the whole sign-in is done by hand over
      VNC.

    Either way the wait ends on a working bearer (`probe`), on the page's
    own error text, or on `mfa_timeout`."""
    if cli_mfa:
        if not _drive_to_push(context, page, watch, args):
            return None
    else:
        with contextlib.suppress(Exception):
            page.goto(george.START_URL, wait_until="domcontentloaded",
                      timeout=60_000)
        log.info("Sign in over VNC (user name, then the push) — waiting up "
                 "to %ds.", mfa_timeout)
    token = _wait_for_token(context, page, watch, mfa_timeout,
                            watch_errors=cli_mfa)
    if token is None:
        _capture(page, args, "not-signed-in")
        error = read_error(page)
        if error:
            log.error("George ended the sign-in: %s", error)
        else:
            log.error("no working session within %ds (last push status: "
                      "%s). Re-run to send a new push, or use vnc-login. "
                      "(DOM captured with --debug.)", mfa_timeout,
                      watch.push_status or "unknown")
        return None
    log.info("signed in")
    return token


def _wait_for_token(context, page, watch: Watch, timeout_s: int, *,
                    watch_errors: bool) -> str | None:
    """Poll until a bearer candidate passes the probe, pumping the event
    loop each tick. A candidate that fails the probe is dropped, so a stale
    one never ends the wait. Back on George with no candidate for a while,
    the page is reloaded once so the SPA's data calls run again."""
    deadline = time.monotonic() + timeout_s
    on_george_since = None
    reloaded = False
    while time.monotonic() < deadline:
        token = _token_now(page, watch)
        if token is not None:
            if probe(context, token):
                return token
            watch.token = None
        if watch_errors and read_error(page):
            return None
        if urlsplit(page_url(page)).hostname == george.GEORGE_HOST:
            on_george_since = on_george_since or time.monotonic()
            if (not reloaded and watch.token is None
                    and time.monotonic() - on_george_since > NO_TOKEN_RELOAD_S):
                log.info("back on George without a bearer — reloading once")
                with contextlib.suppress(Exception):
                    page.reload(wait_until="domcontentloaded", timeout=30_000)
                reloaded = True
        else:
            on_george_since = None
        pump(page)
    return None


# --- diagnostics ----------------------------------------------------------

def _capture(page, args, name: str) -> None:
    """DOM + screenshot to --screenshot-dir, only under --debug (for pinning
    drifted selectors). The user name is masked out of the markup."""
    if not getattr(args, "debug", False):
        return
    with contextlib.suppress(Exception):
        debugcap.capture_page(
            page, args.screenshot_dir, name, log=log,
            redact=debugcap.secret_redactor(os.environ.get(USER_ENV, "")))


# --- verbs ----------------------------------------------------------------

def run_check(args: argparse.Namespace) -> int:
    """Open George on the stored profile and probe for a live session.
    Exit 0 if alive, 1 otherwise. Dead between runs is expected (§F). The
    sign-in form is never submitted, so no push fires."""
    with camoufox(args.profile_dir) as (context, page):
        watch = Watch().attach(context)
        with contextlib.suppress(Exception):
            page.goto(george.START_URL, wait_until="domcontentloaded",
                      timeout=45_000)
        if wait_for(lambda: probe(context, _token_now(page, watch))
                    or _visible(page, george.SEL_USER), page, 30):
            if probe(context, watch.token):
                log.info("session ALIVE")
                return 0
    log.info("session DEAD — a sign-in is required (expected between runs)")
    return 1


def run_login_and_fetch(args: argparse.Namespace) -> int:
    """Sign in (push), then fetch via download.walk() into --bronze-dir."""
    since, until = cli.resolve_standard(args, verb="download", log=log)
    with camoufox(args.profile_dir, fresh=args.fresh) as (context, page):
        watch = Watch().attach(context)
        token = authenticate(context, page, watch, args=args,
                             cli_mfa=args.cli_mfa,
                             mfa_timeout=args.mfa_timeout)
        if token is None:
            return 1
        import download
        summary = download.walk(
            Api(context, token), args.bronze_dir, since=since, until=until,
            documents=not args.no_documents, dry_run=args.dry_run,
            pump=lambda ms: pump(page, ms))
        log.info("fetch done: %s", summary)
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip().splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
                   help="Persistent Camoufox profile dir. Default: "
                        "%(default)s.")
    p.add_argument("--bronze-dir", type=Path, default=None,
                   help="Bronze tree root; required unless --check (the "
                        "download verb passes /data).")
    p.add_argument("--env-file", type=Path, default=DEFAULT_ENV_FILE,
                   help=f"Bash-sourced env file with {USER_ENV}. Default: "
                        "%(default)s.")
    p.add_argument("--check", action="store_true",
                   help="Probe the stored profile for a live session and "
                        "exit 0/1; no push. Reports DEAD between runs by "
                        "design (§F).")
    p.add_argument("--cli-mfa", dest="cli_mfa", action="store_true",
                   default=True,
                   help="Submit the user name from the env file and wait "
                        "for the push approval in the terminal (default).")
    p.add_argument("--no-cli-mfa", dest="cli_mfa", action="store_false",
                   help="Do the whole sign-in by hand over VNC (the "
                        "vnc-login fallback).")
    p.add_argument("--fresh", action="store_true",
                   help="Wipe the persistent Camoufox profile first.")
    p.add_argument("--mfa-timeout", type=int, default=MFA_TIMEOUT_S,
                   help="Seconds to wait for the push approval. Default: "
                        "%(default)s.")
    p.add_argument("--no-documents", action="store_true",
                   help="Skip the statement PDFs (checking and card).")
    p.add_argument("--dry-run", action="store_true",
                   help="Sign in and read the rosters only; fetch no history "
                        "or PDFs and stamp the manifest 'dry-run'. Still "
                        "fires a push.")
    p.add_argument("--debug", action="store_true",
                   help="Capture the sign-in DOM/screenshots into "
                        "--screenshot-dir (for pinning selectors).")
    p.add_argument("--screenshot-dir", type=Path, default=Path("/debug"),
                   help="Where sign-in diagnostics land (outside bronze). "
                        "Default: %(default)s.")
    cli.add_standard_args(p, verb="download")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose or args.debug)
    if args.check:
        if args.fresh:
            log.error("--fresh cannot be combined with --check "
                      "(a read-only probe never wipes the profile).")
            return 2
        return run_check(args)
    if args.bronze_dir is None:
        # No session survives the run (§F), so a sign-in without a fetch
        # would cost a push for nothing.
        log.error("--bronze-dir is required to sign in: the sign-in serves "
                  "the fetch (the download verb passes /data).")
        return 2
    return run_login_and_fetch(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
