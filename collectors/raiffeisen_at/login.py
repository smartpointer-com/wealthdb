#!/usr/bin/env python3
"""Mein ELBA login + fetch driver — the one-shot the wrapper's `download`
runs, and the shared authenticate() it uses.

Mein ELBA fires a pushTAN challenge at every sign-in and does not persist an
authenticated session across browser restarts (DESIGN.md §F), so — like the
chase twin — login and fetch happen in **one browser lifetime**: each run
pays one pushTAN. There is nothing durable to persist that skips 2FA, so the
`login` verb folds into `download`; this script backs both.

The login is an Angular SPA on `sso.raiffeisen.at` (OIDC/PKCE) that lands on
the banking app at `mein.elba.raiffeisen.at`. Two entry shapes (DESIGN.md
§A/§F):

* **Cold / --fresh profile** — a blank form: a region (Mandant) dropdown, a
  Verfüger field, and a PIN. login.py resolves `RAIFFEISEN_AT_REGION` to the
  dropdown option + Verfüger prefix via the public `config/mandanten`
  (elba_client), fills the form (prefix verified by read-back), and submits.
* **Warm profile** — a saved-user card instead of the form; clicking it
  skips straight to pushTAN.

Either way the SPA then shows a **pushTAN** wait screen with a 4-char
Vergleichswert (announced to the terminal) and polls for approval on its
own. There is nothing to type — the sign-in is approved in the Raiffeisen app
and the SPA completes the OIDC hand-off. Auth is detected event-free by
polling the SPA route (the app origin's dashboard) plus a REST probe
(`GET produkte` 200) — never a lone Playwright response event, which the
pinned Camoufox can drop across the OIDC navigation.

Modes:
  --check   probe the persisted profile against the app and exit 0 (alive) /
            1 (dead). Dead between runs is expected (§F); fires no pushTAN.
  default   fill/confirm the login, wait for the pushTAN approval, then fetch
            via download.walk() into --bronze-dir.

Read-only (CLAUDE.md): the browser only completes the logon; the deposit
data is fetched over REST. Never a money-movement, card, or settings surface.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import os
import shutil
import sys
import time
from pathlib import Path

from collectorkit import cli, debugcap, envfile, launch

import elba_client as elba
# Reuse explore's origin-gated frame-aware prefill is not applicable here (the
# Verfüger value is region-prefixed and the form is single-frame Angular), so
# login drives the RDS controls directly. Credentials come from explore's env
# names to stay in one place.
from explore import USER_ENV, PASS_ENV

log = logging.getLogger("raiffeisen_at.login")

DEFAULT_PROFILE_DIR = Path("/secrets/raiffeisen_at-profile")
DEFAULT_ENV_FILE = Path("/secrets/raiffeisen_at.env")
REGION_ENV = "RAIFFEISEN_AT_REGION"

# How long to wait for a login outcome after the form is submitted (the
# pushTAN wait screen or an authenticated session), and how long to then wait
# for the human to approve the pushTAN in the app.
OUTCOME_TIMEOUT_S = 120
PUSHTAN_TIMEOUT_S = 300          # the server-declared pushTAN `timeout`


# --- browser plumbing -----------------------------------------------------

@contextlib.contextmanager
def camoufox(profile_dir: Path, fresh: bool = False):
    """Open a persistent Camoufox context on the profile dir (headed under
    the entrypoint's Xvfb; VNC is exposed only for vnc-login). The persistent
    profile carries the saved-user identity (which skips form entry on a warm
    run, but never the pushTAN — §F). `fresh` wipes it so the next login is
    the cold blank-form path. Yields (context, page)."""
    from camoufox.sync_api import Camoufox
    if fresh and profile_dir.exists():
        log.warning("--fresh: wiping profile dir %s — the next login is the "
                    "cold blank-form path (region + Verfüger + PIN)",
                    profile_dir)
        shutil.rmtree(profile_dir)
    launch.prepare_profile_dir(profile_dir)
    cam = Camoufox(
        persistent_context=True,
        user_data_dir=str(profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
        humanize=True,
        geoip=True,
        firefox_user_prefs=launch.firefox_prefs(),
    )
    context = cam.__enter__()
    try:
        page = context.pages[0] if context.pages else context.new_page()
        yield context, page
    finally:
        with contextlib.suppress(Exception):
            cam.__exit__(None, None, None)


class _Watch:
    """Capture the two login-flow signals the SPA emits: the region list
    (`config/mandanten`) and the pushTAN send (`login/pushtan` → the
    Vergleichswert). Both are conveniences — the region list is also fetched
    directly, and the Vergleichswert is shown on screen — because the pinned
    Camoufox can drop `.on()` events. Also harvests the OIDC Bearer token
    from any authenticated `/api/` request the SPA makes, so download can
    replay the data calls over the context's `request` API."""

    def __init__(self):
        self.mandanten: list | None = None
        self.pushtan_display: str | None = None
        self.bearer: str | None = None

    def attach(self, context):
        context.on("response", self._on_response)
        context.on("request", self._on_request)
        return self

    def _on_response(self, resp):
        try:
            url = resp.url
            if url.endswith("/config/mandanten"):
                body = resp.json()
                if isinstance(body, list):
                    self.mandanten = body
            elif "/login/pushtan" in url and resp.request.method == "POST":
                txt = elba.pushtan_display_text(resp.json())
                if txt:
                    self.pushtan_display = txt
            elif url.endswith(elba.TOKEN_ENDPOINT_SUFFIX):
                # The authoritative Bearer source — the token exchange's own
                # response — in case the per-request header harvest misses it.
                tok = elba.bearer_from_token_response(resp.json())
                if tok:
                    self.bearer = tok
        except Exception:            # pragma: no cover — defensive
            pass

    def _on_request(self, req):
        try:
            if "/api/" not in req.url:
                return
            auth = req.headers.get("authorization", "")
            if auth.lower().startswith("bearer "):
                self.bearer = auth.split(" ", 1)[1]
        except Exception:            # pragma: no cover — defensive
            pass


def _pump(page, ms: int = 500) -> None:
    """Advance the Playwright sync event loop so `.on()` handlers fire and the
    SPA makes progress — a bare time.sleep() delivers no events in the sync
    API. Falls back to another Playwright call if a navigation destroys the
    timer context (the pinned-camoufox lesson)."""
    try:
        page.wait_for_timeout(ms)
    except Exception:
        with contextlib.suppress(Exception):
            page.wait_for_load_state(timeout=ms)


def _wait_for(predicate, page, timeout_s: float) -> bool:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if predicate():
            return True
        _pump(page)
    return False


def _url(page) -> str:
    """`page.url`, guarded — the property can raise mid-navigation on the
    pinned Camoufox."""
    try:
        return page.url or ""
    except Exception:
        return ""


# --- REST over the browser context ---------------------------------------

def api_headers(watch: "_Watch") -> dict:
    """Headers for an authenticated data call: the harvested OIDC Bearer
    (elba data API) plus a JSON Accept. Bearer-less if not yet seen (the
    caller then relies on the session cookies the context shares)."""
    h = {"Accept": "application/json"}
    if watch.bearer:
        h["Authorization"] = f"Bearer {watch.bearer}"
    return h


def api_get_json(context, watch: "_Watch", url: str) -> tuple[int, object]:
    """GET `url` over the browser context (shares cookies) with the Bearer;
    return (status, parsed-json-or-None)."""
    resp = context.request.get(url, headers=api_headers(watch))
    return _resp_json(resp)


def api_post_json(context, watch: "_Watch", url: str,
                  data: dict) -> tuple[int, object]:
    """POST JSON `data` over the browser context with the Bearer; return
    (status, parsed-json-or-None)."""
    resp = context.request.post(
        url, headers={**api_headers(watch), "Content-Type": "application/json"},
        data=data)
    return _resp_json(resp)


def _resp_json(resp) -> tuple[int, object]:
    body = None
    with contextlib.suppress(Exception):
        body = resp.json()
    return resp.status, body


def _probe_authenticated(context, watch: "_Watch") -> bool:
    """The definitive signed-in signal: a `GET produkte` with the harvested
    Bearer returns 200 with a JSON array. It requires the Bearer (produkte
    401s on cookies alone — the live 2026-08-15 failure), so this is only
    ever true once a working token has been harvested. A polled call, not an
    event, so it survives the pinned Camoufox dropping `.on()` events."""
    if not watch.bearer:
        return False
    with contextlib.suppress(Exception):
        status, body = api_get_json(context, watch, elba.produkte_url())
        return status == 200 and isinstance(body, list)
    return False


# --- login-form driving ---------------------------------------------------

def _load_credentials(env_file: Path) -> tuple[str, str, str]:
    if envfile.source_env_file(env_file):
        log.info("env file: %s (sourced)", env_file)
    username = os.environ.get(USER_ENV, "")
    password = os.environ.get(PASS_ENV, "")
    region = os.environ.get(REGION_ENV, "")
    if not (username and password and region):
        log.warning("%s / %s / %s not all set — the login form cannot be "
                    "pre-filled (complete it by hand over vnc-login).",
                    USER_ENV, PASS_ENV, REGION_ENV)
    return username, password, region


def _profile_card_present(page) -> bool:
    with contextlib.suppress(Exception):
        return page.locator(elba.SEL_PROFILE_CARD).count() > 0
    return False


def _form_present(page) -> bool:
    with contextlib.suppress(Exception):
        return page.locator(elba.SEL_REGION_SELECT).count() > 0
    return False


def _read_vergleichswert(page) -> str | None:
    """Read the Vergleichswert off the pushTAN screen — the large display
    paragraph holds it (DESIGN.md §A). Polls briefly, since the code may
    render a moment after the screen mounts. Returns None if not found (the
    code shown on screen is then compared directly)."""
    for _ in range(10):
        with contextlib.suppress(Exception):
            loc = page.locator(elba.SEL_VERGLEICHSWERT)
            for i in range(min(loc.count(), 5)):
                txt = (loc.nth(i).inner_text() or "").strip()
                if elba.looks_like_vergleichswert(txt):
                    return txt
        _pump(page, 500)
    return None


def _click_profile_card(page) -> bool:
    """Warm path: click the saved-user card to proceed to pushTAN (never the
    delete button). The clickable row is `app-user-entry .clickable`; the
    host component is the fallback."""
    for sel in (elba.SEL_PROFILE_CARD_CLICK, elba.SEL_PROFILE_CARD):
        with contextlib.suppress(Exception):
            loc = page.locator(sel).first
            if loc.count():
                loc.click(timeout=5000)
                return True
    return False


def _select_region(page, option_index: int) -> bool:
    """Open the Mandant dropdown and click the option at `option_index`
    (resolved from the region code via elba.resolve_mandant)."""
    with contextlib.suppress(Exception):
        page.locator(elba.SEL_REGION_SELECT).first.click(timeout=5000)
    if not _wait_for(lambda: page.locator(elba.SEL_REGION_OPTION).count() > 0,
                     page, 8):
        return False
    with contextlib.suppress(Exception):
        opts = page.locator(elba.SEL_REGION_OPTION)
        if option_index < opts.count():
            opts.nth(option_index).click(timeout=5000)
            return True
    return False


def _fill_verfueger(page, full_id: str, kennung: str) -> bool:
    """Fill the Verfüger field with the full (prefixed) id, then verify by
    read-back that it carries the region prefix — so a wrong region can never
    submit under the wrong Mandant (DESIGN.md §B). The field debounces a
    canonicalising lookup, so the read-back tolerates a reformatted value as
    long as the prefix holds."""
    field = page.locator(elba.SEL_VERFUEGER).first
    with contextlib.suppress(Exception):
        field.click(timeout=4000)
        field.fill("")
        field.fill(full_id)
    _pump(page, 800)                      # let the canonicalising lookup settle
    with contextlib.suppress(Exception):
        got = field.input_value()
        if got and got.startswith(kennung):
            return True
        log.warning("Verfüger read-back %r does not carry the expected region "
                    "prefix %r", got, kennung)
    return False


def _fill_pin(page, pin: str) -> bool:
    with contextlib.suppress(Exception):
        field = page.locator(elba.SEL_PIN).first
        field.click(timeout=4000)
        field.fill("")
        field.fill(pin)
        return field.input_value() == pin
    return False


def _submit(page) -> bool:
    with contextlib.suppress(Exception):
        btn = page.locator(elba.SEL_SUBMIT).first
        if btn.count():
            btn.click(timeout=5000)
            return True
    with contextlib.suppress(Exception):
        page.locator(elba.SEL_PIN).first.press("Enter")
        return True
    return False


def _drive_login_form(context, page, watch: "_Watch", args) -> bool:
    """Fill and submit the cold blank-form login: resolve the region to its
    dropdown option + Verfüger prefix (via config/mandanten), select it, fill
    the prefixed Verfüger and PIN, and submit. Returns True once submitted."""
    username, password, region = _load_credentials(args.env_file)
    if not (username and password and region):
        return False

    # Resolve the region → (option index, Verfüger prefix). Prefer the
    # SPA-captured mandanten; fall back to a direct fetch.
    mandanten = watch.mandanten
    if mandanten is None:
        status, body = api_get_json(context, watch, elba.mandanten_url())
        if status == 200 and isinstance(body, list):
            mandanten = body
    if not mandanten:
        _capture(page, args, "no-mandanten")
        log.error("could not fetch the region list (config/mandanten). Retry "
                  "with vnc-login. (DOM captured with --debug.)")
        return False
    try:
        option_index, kennung = elba.resolve_mandant(mandanten, region)
    except KeyError as exc:
        log.error("%s: %s", REGION_ENV, exc)
        return False

    if not _select_region(page, option_index):
        _capture(page, args, "region-select-failed")
        log.error("could not select the region in the Mandant dropdown. Retry "
                  "with vnc-login. (DOM captured with --debug.)")
        return False
    full_id = elba.full_verfueger(kennung, username)
    if not _fill_verfueger(page, full_id, kennung):
        _capture(page, args, "verfueger-fill-failed")
        log.error("could not fill/verify the Verfüger field. Retry with "
                  "vnc-login. (DOM captured with --debug.)")
        return False
    if not _fill_pin(page, password):
        _capture(page, args, "pin-fill-failed")
        log.error("could not fill the PIN field. Retry with vnc-login. "
                  "(DOM captured with --debug.)")
        return False
    _capture(page, args, "signin-filled")
    if not _submit(page):
        _capture(page, args, "submit-failed")
        log.error("could not submit the login form. Retry with vnc-login. "
                  "(DOM captured with --debug.)")
        return False
    return True


def _drive_to_pushtan(context, page, watch: "_Watch", args) -> bool:
    """Front half of a cli login: reach the login screen, then either drive
    the blank form (cold) or click the saved-user card (warm). Returns True
    once the login is submitted / the card is clicked (pushTAN follows)."""
    page.goto(elba.START_URL, wait_until="domcontentloaded", timeout=60_000)
    # Wait for the login screen to render — the form (cold) or the saved-user
    # card (warm). The initial URL is the bare app shell that bounces to the
    # sso login app, so it is NEVER treated as authed here (the shared-URL
    # trap that broke the first live run — DESIGN.md §A); a genuinely-live
    # session is caught by the Bearer probe instead.
    if not _wait_for(lambda: _form_present(page) or _profile_card_present(page)
                     or _probe_authenticated(context, watch),
                     page, OUTCOME_TIMEOUT_S):
        _capture(page, args, "no-login-screen")
        log.error("neither the login form nor a saved-user card appeared. "
                  "Retry with vnc-login. (DOM captured with --debug.)")
        return False
    if _probe_authenticated(context, watch):
        return True                       # already signed in (rare live session)
    if _form_present(page):
        log.info("cold profile — driving the region + Verfüger + PIN form")
        return _drive_login_form(context, page, watch, args)
    log.info("warm profile — clicking the saved-user card (skips form entry)")
    if not _click_profile_card(page):
        _capture(page, args, "card-click-failed")
        log.error("could not click the saved-user card. Retry with "
                  "--fresh (cold form) or vnc-login. (DOM captured with "
                  "--debug.)")
        return False
    return True


# --- the shared authenticate() -------------------------------------------

def authenticate(context, page, watch: "_Watch", *, args, cli_mfa: bool,
                 pushtan_timeout: int = PUSHTAN_TIMEOUT_S) -> bool:
    """Take a login to an authenticated session (DESIGN.md §A/§F).

    * `cli_mfa=True` (default `download`) — fill/click the login screen from
      the stored credentials (no VNC), announce the pushTAN Vergleichswert,
      then wait for the sign-in to be approved in the app; the SPA completes
      the OIDC hand-off on its own.
    * `cli_mfa=False` (`vnc-login`) — the human completes the whole login
      (form/card + pushTAN) by hand over VNC.

    The definitive completion signal is a working Bearer (the `produkte`
    probe) — never the SPA URL, which the pre-login bounce shares with the
    dashboard (§A). There is no code to type on either path — the second
    factor is a phone approval."""
    if cli_mfa:
        if not _drive_to_pushtan(context, page, watch, args):
            return False
        # Announce the Vergleichswert once the pushTAN screen is up — it is
        # compared against the code in the app. Prefer the captured
        # displayText; fall back to reading it off the screen (the pinned
        # Camoufox can drop the pushtan response event — it did on the first
        # live login).
        _wait_for(lambda: watch.pushtan_display is not None
                  or elba.is_pushtan_url(_url(page))
                  or _probe_authenticated(context, watch), page, 30)
        code = watch.pushtan_display or _read_vergleichswert(page)
        if code:
            log.info("pushTAN sent — approve the sign-in in the Raiffeisen "
                     "app. Vergleichswert (compare on your phone): %s", code)
        else:
            log.info("pushTAN sent — approve the sign-in in the Raiffeisen "
                     "app (compare the Vergleichswert shown on screen).")
    else:
        log.info("Complete the login (form/card + pushTAN approval) in the "
                 "browser over VNC — waiting up to %ds.", pushtan_timeout)

    if not _wait_for_auth(context, page, watch, pushtan_timeout):
        _capture(page, args, "pushtan-not-approved")
        log.error("no authenticated session appeared — the pushTAN was not "
                  "approved in time, or the login stalled (no Bearer "
                  "harvested). Retry with vnc-login. (DOM captured with "
                  "--debug.)")
        return False
    log.info("authenticated")
    return True


def _wait_for_auth(context, page, watch: "_Watch", timeout_s: int) -> bool:
    """Poll for a working Bearer (the `produkte` probe) until success or
    timeout, pumping the event loop each tick. If the SPA reaches a real
    in-app route (login completed) but no Bearer has been harvested, reload
    once to re-trigger the SPA's authenticated `/api/` calls so their
    Authorization headers can be captured — the belt-and-braces against the
    pinned Camoufox dropping the harvest events."""
    deadline = time.monotonic() + timeout_s
    reloaded = False
    on_route_since = None
    while time.monotonic() < deadline:
        if _probe_authenticated(context, watch):
            return True
        if elba.is_authed_route(_url(page)):
            now = time.monotonic()
            on_route_since = on_route_since or now
            if not reloaded and watch.bearer is None and now - on_route_since > 12:
                log.info("on the app route but no Bearer yet — reloading to "
                         "re-trigger the authenticated API calls")
                with contextlib.suppress(Exception):
                    page.reload(wait_until="domcontentloaded", timeout=30_000)
                reloaded = True
        _pump(page)
    return False


# --- diagnostics ----------------------------------------------------------

def _capture(page, args, name: str) -> None:
    """DOM + screenshot to --screenshot-dir, only under --debug (for pinning
    drifted selectors)."""
    if not getattr(args, "debug", False):
        return
    with contextlib.suppress(Exception):
        debugcap.capture_page(page, args.screenshot_dir, name, log=log)


# --- verbs ----------------------------------------------------------------

def run_check(args: argparse.Namespace) -> int:
    """Load the app on the persisted profile and probe for an authenticated
    session. Exit 0 if alive, 1 otherwise. Dead between runs is expected —
    the session does not survive a browser restart (§F). Sends no pushTAN."""
    with camoufox(args.profile_dir) as (context, page):
        watch = _Watch().attach(context)
        with contextlib.suppress(Exception):
            page.goto(elba.START_URL, wait_until="domcontentloaded",
                      timeout=45_000)
        if _wait_for(lambda: _probe_authenticated(context, watch), page, 20):
            log.info("session ALIVE")
            return 0
    log.info("session DEAD — a fresh login is required (expected between runs)")
    return 1


def run_login_and_fetch(args: argparse.Namespace) -> int:
    """Log in (pushTAN), then — if --bronze-dir is set — fetch via
    download.walk(). A bare login (no --bronze-dir) authenticates and exits,
    but that is not a persisted session, so the wrapper's `login` verb is a
    no-op instead (this path is reached only via `download` / `vnc-login`)."""
    since, until = cli.resolve_standard(args, verb="download", log=log)
    with camoufox(args.profile_dir, fresh=args.fresh) as (context, page):
        watch = _Watch().attach(context)
        if not authenticate(context, page, watch, args=args,
                            cli_mfa=args.cli_mfa,
                            pushtan_timeout=args.mfa_timeout):
            return 1
        if args.bronze_dir is None:
            log.info("no --bronze-dir: login only, nothing to fetch")
            return 0
        # authenticate() only returns True once the produkte probe succeeded
        # with a harvested Bearer, so the token is in hand here — no separate
        # wait is needed before the fetch replays the same calls.
        import download
        summary = download.walk(
            context, watch, args.bronze_dir, since=since, until=until,
            documents=not args.no_documents, dry_run=args.dry_run)
        log.info("fetch done: %s", summary)
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip().splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
                   help="Persistent Camoufox profile dir (holds the saved-user "
                        "identity). Default: %(default)s.")
    p.add_argument("--bronze-dir", type=Path, default=None,
                   help="Bronze tree root; required for the fetch path (the "
                        "download verb passes /data).")
    p.add_argument("--env-file", type=Path, default=DEFAULT_ENV_FILE,
                   help="Bash-sourced env file with RAIFFEISEN_AT_USERNAME / "
                        "RAIFFEISEN_AT_PASSWORD / RAIFFEISEN_AT_REGION. "
                        "Default: %(default)s.")
    p.add_argument("--check", action="store_true",
                   help="Probe the persisted session and exit 0/1; no "
                        "pushTAN. Reports DEAD between runs by design (§F).")
    p.add_argument("--cli-mfa", dest="cli_mfa", action="store_true",
                   default=True,
                   help="Fill/confirm the login from the stored credentials, "
                        "no VNC — approve the pushTAN on your phone (default).")
    p.add_argument("--no-cli-mfa", dest="cli_mfa", action="store_false",
                   help="Complete the whole login by hand over VNC "
                        "(the vnc-login fallback).")
    p.add_argument("--fresh", action="store_true",
                   help="Wipe the persistent Camoufox profile first, forcing "
                        "the cold blank-form login (region + Verfüger + PIN).")
    p.add_argument("--mfa-timeout", type=int, default=PUSHTAN_TIMEOUT_S,
                   help="Seconds to wait for the pushTAN approval. Default: "
                        "%(default)s.")
    p.add_argument("--no-documents", action="store_true",
                   help="Skip the statement-PDF pass (the run's heavy part).")
    p.add_argument("--dry-run", action="store_true",
                   help="Fetch nothing after login; stamp the manifest "
                        "'dry-run'.")
    p.add_argument("--debug", action="store_true",
                   help="Capture the login DOM/screenshots into "
                        "--screenshot-dir (for pinning selectors).")
    p.add_argument("--screenshot-dir", type=Path, default=Path("/debug"),
                   help="Where login diagnostics land (outside bronze). "
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
    return run_login_and_fetch(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
