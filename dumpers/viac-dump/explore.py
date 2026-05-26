#!/usr/bin/env python3
"""
viac-dump Phase 1: VNC-driven discovery.

Launches Chromium headed (against the Xvfb display started by
entrypoint.sh `vnc-explore`), navigates to the VIAC SPA login
hash-route, pre-fills VIAC_LOGIN / VIAC_PASSWORD into the rendered
form so the operator doesn't have to paste through VNC, then sits
in a long-timeout loop recording every network response, DOM
snapshot, download, and storage waypoint while the operator
completes 2FA and walks the SPA via the VNC viewer.

Credentials are sourced from /secrets/viac.env (or
~/.secrets/viac.env outside the container); never from a CLI flag
— see CLAUDE.md §3.

Output lands under --discovery-dir (no default — must be user-
provided, conventionally /debug/discovery-<UTC-ts>/; see
CLAUDE.md §3 on the secrets-vs-debug separation).

This script DOES start a real browser session against app.viac.ch;
running it counts as a Phase 1 live session — see CLAUDE.md §2.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import re
import shlex
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

from playwright.sync_api import sync_playwright

log = logging.getLogger("viac-dump.explore")

# Hash-route login URL. Initial document is small; the form is
# JS-rendered, so Playwright must wait for it rather than relying
# on page-load events (DESIGN.md §2).
LOGIN_URL = "https://app.viac.ch/#/ext(modal:core/session/login)"

# Env-file candidates. The wrapper mounts ~/.secrets to /secrets
# inside the container so /secrets/viac.env is canonical; the
# ~/.secrets/viac.env fallback lets the script work outside the
# container for local dev.
DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/viac.env"),
    Path.home() / ".secrets" / "viac.env",
)

LOGIN_ENV = "VIAC_LOGIN"
PASSWORD_ENV = "VIAC_PASSWORD"

# Generous defaults — WAN latency to a Swiss cloud-host from
# anywhere outside CH can be high, and VIAC's SPA bundle is large.
NAV_TIMEOUT_MS = 60_000
LOGIN_FORM_TIMEOUT_MS = 60_000

# Default wall-clock cap on the discovery session. Generous so the
# operator has room to authenticate on their phone, walk every
# view, and re-do anything they missed. See the
# [No immediate-response interactive flows] memory.
DEFAULT_MAX_RUNTIME_SEC = 4 * 3600


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--discovery-dir", required=True, type=Path,
        help=("Where to write recorded artefacts (requests.jsonl, "
              "response bodies, DOM snapshots, screenshots, "
              "downloads, storage snapshots). Must be user-provided "
              "per CLAUDE.md §3; never falls back to a secrets-dir "
              "path. Conventionally /debug/discovery-<UTC-ts>/."),
    )
    p.add_argument(
        "--profile-dir", default=Path("/secrets/viac-profile"), type=Path,
        help=("Persistent Chromium user-data-dir. The first run "
              "seeds it; subsequent runs reuse it and may skip MFA "
              "if VIAC's device-trust cookie is still alive."),
    )
    p.add_argument(
        "--env-file", default=None, type=Path,
        help=(f"Path to a KEY=VALUE env file to load before "
              f"resolving credentials. Defaults to /secrets/viac.env "
              f"if present, else ~/.secrets/viac.env."),
    )
    p.add_argument(
        "--login-url", default=LOGIN_URL,
        help=f"Login URL to navigate to. Default: {LOGIN_URL}",
    )
    p.add_argument(
        "--no-fill-credentials", action="store_true",
        help=("Skip the credential pre-fill step. Useful when "
              "iterating on the recorder without burning an MFA push "
              "(operator can dismiss the login form via VNC)."),
    )
    p.add_argument(
        "--max-runtime-sec", type=int, default=DEFAULT_MAX_RUNTIME_SEC,
        help=(f"Wall-clock cap on the session. Default: "
              f"{DEFAULT_MAX_RUNTIME_SEC}s. Ctrl-C exits early."),
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


# Shell-internal variables we don't want to leak into os.environ
# when sourcing the env file. PATH is passed INTO the bash
# subprocess so it can find `env`; the rest are bash bookkeeping.
_BASH_VAR_BLOCKLIST = frozenset({"_", "PWD", "OLDPWD", "SHLVL", "PATH"})


def source_env_file(path: Path) -> bool:
    """Source `path` as a shell env file via bash and merge the
    resulting KEY=VALUE bindings into os.environ.

    The viac.env contract is "this file is bash" (see README.md):
    `VIAC_LOGIN='value'` etc. with single quotes around values
    containing shell metacharacters. Honour that contract by
    asking bash itself to read it (`set -a; source FILE; set +a;
    env -0`), rather than handrolling a parser that gets
    quoting / escapes / expansions subtly wrong.

    setdefault semantics: an already-set env var wins, matching
    how a parent shell's `export VAR=...` survives a re-source.

    Returns False if `path` doesn't exist (non-fatal). Raises
    ValueError on a shell syntax error inside the file — fail
    loud; the user fixes the file and re-runs.
    """
    if not path.is_file():
        return False
    # bash's `source` builtin prints parse errors to stderr but
    # returns 0 even so (and even `set -e` doesn't make `source`
    # failures fatal). Validate with `bash -n` first; that does
    # honour parse errors via the exit code.
    syntax_check = subprocess.run(
        ["bash", "--noprofile", "--norc", "-n", str(path)],
        capture_output=True,
    )
    if syntax_check.returncode != 0:
        stderr = (syntax_check.stderr or b"").decode("utf-8", errors="replace").rstrip()
        raise ValueError(
            f"env file {path} has bash syntax errors:\n{stderr}"
        )
    quoted = shlex.quote(str(path))
    result = subprocess.run(
        ["bash", "--noprofile", "--norc", "-c",
         f"set -a; source {quoted}; set +a; env -0"],
        env={"PATH": os.environ.get("PATH", "/usr/bin:/bin")},
        capture_output=True, check=True,
    )
    for entry in result.stdout.split(b"\x00"):
        if not entry:
            continue
        k, _sep, v = entry.partition(b"=")
        try:
            key = k.decode("utf-8")
            val = v.decode("utf-8")
        except UnicodeDecodeError:
            continue
        if key in _BASH_VAR_BLOCKLIST:
            continue
        os.environ.setdefault(key, val)
    return True


def resolve_env_file(arg_path: Path | None) -> Path | None:
    if arg_path is not None:
        return arg_path
    for candidate in DEFAULT_ENV_FILE_CANDIDATES:
        if candidate.is_file():
            return candidate
    return None


def safe_filename_part(s: str, max_len: int = 80) -> str:
    """Squash a URL fragment into a path-safe slug."""
    s = re.sub(r"[^A-Za-z0-9._-]+", "_", s)
    return s[:max_len].strip("_") or "x"


def utc_ts() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def _should_save_body(ct: str) -> bool:
    """Decide whether to save a response body to disk.

    Covers what session-1 discovery showed VIAC actually serves:
    `application/vnd.api+json` (the JSON:API auth + customer
    endpoints) and `application/pdf` (every document, including
    the per-transaction "transaction documents" the operator
    flagged). Plain JSON, HTML, and XML are kept for the SPA
    shell and any non-API endpoints.
    """
    if not ct:
        return False
    # JSON family: vanilla, JSON:API, schema-extended, problem+json…
    if ct.startswith("application/json"):
        return True
    if ct.startswith("application/") and "+json" in ct:
        return True
    # XML family.
    if ct.startswith("application/xml") or ct.startswith("text/xml"):
        return True
    if ct.startswith("application/") and "+xml" in ct:
        return True
    # HTML — the SPA shell and any server-rendered fallback view.
    if ct.startswith("text/html"):
        return True
    # PDFs — VIAC ships every transaction event as its own PDF
    # under /files/document/<id>. We need those for Phase 1.
    if ct.startswith("application/pdf"):
        return True
    return False


def _body_extension(ct: str) -> str:
    if "pdf" in ct:
        return "pdf"
    if "json" in ct:
        return "json"
    if "xml" in ct:
        return "xml"
    if "html" in ct:
        return "html"
    return "bin"


# URL substrings whose request headers + POST bodies we capture in
# request_details.jsonl. The point is to discover (a) the exact
# CSRF request-header name (the SPA reads cookie CSRFT<N>-S and
# sends it back as some header — we need to learn which), and
# (b) the auth POST body shape so login.py can replay it with
# httpx instead of driving a browser. We skip static assets / JS
# chunks / images where headers are uninteresting noise.
_API_URL_SUBSTRINGS = (
    "/rest/web",
    "/external-login",
    "/files/document",
    "/config/",
)


def _is_api_url(url: str) -> bool:
    """True for the endpoints we want full request detail for."""
    return any(seg in url for seg in _API_URL_SUBSTRINGS)


class Recorder:
    """Captures network responses, downloads, DOM, screenshots,
    storage snapshots.

    Event handlers run inside Playwright's sync-API dispatch
    context, where reentrant calls into Playwright (page.content(),
    page.screenshot(), context.storage_state()) are unreliable —
    session-1 discovery had 184 framenavigated events silently
    drop their snapshots from inside on_navigation. Handlers now
    do the minimum: log metadata + enqueue. The main loop calls
    `process_pending()` outside the dispatch context to do the
    real Playwright work.

    `response.body()` is the one Playwright call we *do* keep
    inside an event handler — the response object's buffer can
    be freed once the response is "consumed", so we can't defer
    it. Failures are logged at WARNING so we can audit.
    """

    def __init__(self, discovery_dir: Path):
        self.dir = discovery_dir
        self.responses_dir = discovery_dir / "responses"
        self.downloads_dir = discovery_dir / "downloads"
        self.dom_dir = discovery_dir / "dom"
        self.screenshots_dir = discovery_dir / "screenshots"
        self.storage_dir = discovery_dir / "storage"
        for d in (self.responses_dir, self.downloads_dir, self.dom_dir,
                  self.screenshots_dir, self.storage_dir):
            d.mkdir(parents=True, exist_ok=True)
        self.requests_log = (discovery_dir / "requests.jsonl").open("a", buffering=1)
        # request_details.jsonl carries the full headers + post_data
        # for API URLs only (see _is_api_url). It joins back to
        # requests.jsonl on `seq`. CONTAINS CREDENTIALS, SESSION
        # COOKIES, AND THE mTAN OTP — discovery-dir is gitignored
        # and host-local, but treat the file as sensitive and
        # delete after analysis.
        self.request_details_log = (
            discovery_dir / "request_details.jsonl").open("a", buffering=1)
        self._seq = 0
        self._snapshotted_urls: set[str] = set()
        self._pages_attached: set[int] = set()
        # (nav_seq, url, page) entries enqueued by on_navigation,
        # drained by process_pending() from the main loop.
        self._pending_navs: list[tuple[str, str, object]] = []

    def _next_seq(self) -> str:
        self._seq += 1
        return f"{self._seq:06d}"

    def attach_page(self, page) -> None:
        """Wire per-page listeners. Idempotent — `context.on('page')`
        fires for popup tabs (Chromium opens PDFs in a new tab by
        default), and we want to record those too without
        double-attaching to the original page."""
        pid = id(page)
        if pid in self._pages_attached:
            return
        self._pages_attached.add(pid)
        page.on("download", self.on_download)
        page.on("framenavigated", lambda frame, _p=page: self.on_navigation(frame, _p))
        try:
            url = page.url
        except Exception:
            url = "<unknown>"
        log.info("attached recorder to page (url=%s, pages_attached=%d)",
                 url, len(self._pages_attached))

    def on_response(self, response) -> None:
        seq = None
        url = "<unknown>"
        try:
            url = response.url
            method = response.request.method
            status = response.status
            ct = (response.headers.get("content-type") or "").lower()
            seq = self._next_seq()

            body_path = None
            if _should_save_body(ct):
                try:
                    body = response.body()
                    ext = _body_extension(ct)
                    fname = f"{seq}-{safe_filename_part(url)}.{ext}"
                    fpath = self.responses_dir / fname
                    fpath.write_bytes(body)
                    body_path = str(fpath.relative_to(self.dir))
                except Exception as e:
                    # Bumped to WARNING from DEBUG so we can audit
                    # the next session for silent body-capture
                    # failures.
                    log.warning("body capture failed (seq=%s ct=%s url=%s): %s",
                                seq, ct, url, e)

            rec = {
                "seq": seq,
                "ts": time.time(),
                "method": method,
                "url": url,
                "status": status,
                "content_type": ct,
                "body_path": body_path,
            }
            self.requests_log.write(json.dumps(rec, ensure_ascii=False) + "\n")

            # Capture request headers + post_data for the API
            # endpoints. After the requests.jsonl write so a
            # failure here doesn't lose the basic metadata.
            if _is_api_url(url):
                self._capture_request_details(seq, response, url, method)
        except Exception as e:
            log.warning("on_response error (seq=%s url=%s): %s", seq, url, e)

    def _capture_request_details(self, seq: str, response, url: str, method: str) -> None:
        """Capture full request headers + post_data into
        request_details.jsonl. Joined to requests.jsonl by `seq`."""
        try:
            req = response.request
            try:
                headers = req.all_headers()
            except Exception as e:
                log.debug("all_headers failed, falling back to headers: %s", e)
                headers = dict(req.headers) if hasattr(req, "headers") else {}
            post_data = req.post_data  # property, Optional[str]
            rec = {
                "seq": seq,
                "url": url,
                "method": method,
                "headers": headers,
                "post_data": post_data,
            }
            self.request_details_log.write(
                json.dumps(rec, ensure_ascii=False, default=str) + "\n")
        except Exception as e:
            log.warning("request-details capture failed (seq=%s url=%s): %s",
                        seq, url, e)

    def on_download(self, download) -> None:
        try:
            seq = self._next_seq()
            suggested = download.suggested_filename or "download.bin"
            target = self.downloads_dir / f"{seq}-{safe_filename_part(suggested)}"
            download.save_as(str(target))
            log.info("download saved: %s", target)
        except Exception as e:
            log.warning("on_download error: %s", e)

    def on_navigation(self, frame, page) -> None:
        """Top-frame navigation events — enqueue only; the real
        Playwright work happens in `process_pending()` from the
        main loop."""
        try:
            if frame != page.main_frame:
                return
            url = frame.url
            seq = self._next_seq()
            self._pending_navs.append((seq, url, page))
        except Exception as e:
            log.warning("on_navigation enqueue error: %s", e)

    def process_pending(self) -> int:
        """Drain pending navigation events. Called from the main
        loop, OUTSIDE the Playwright event-dispatch context.

        Dedup by URL so a noisy SPA hash-router doesn't fill the
        disk with identical DOM snapshots, but the requests.jsonl
        metadata records every transition regardless."""
        pending = self._pending_navs
        self._pending_navs = []
        for seq, url, page in pending:
            if url in self._snapshotted_urls:
                continue
            self._snapshotted_urls.add(url)
            slug = safe_filename_part(url)
            tag = f"{seq}-{slug}"
            try:
                html = page.content()
                (self.dom_dir / f"{tag}.html").write_text(html, encoding="utf-8")
            except Exception as e:
                log.warning("DOM snapshot failed (%s): %s", url, e)
            try:
                page.screenshot(
                    path=str(self.screenshots_dir / f"{tag}.png"),
                    full_page=False)
            except Exception as e:
                log.warning("screenshot failed (%s): %s", url, e)
            self.snapshot_storage(page.context, label=tag)
        return len(pending)

    def snapshot_storage(self, context, label: str = "manual") -> None:
        try:
            state = context.storage_state()
            (self.storage_dir / f"{label}.json").write_text(
                json.dumps(state, indent=2, ensure_ascii=False))
        except Exception as e:
            log.warning("storage snapshot failed (label=%s): %s", label, e)

    def close(self) -> None:
        for handle in (self.requests_log, self.request_details_log):
            try:
                handle.close()
            except Exception:
                pass


def find_username_field(page) -> object | None:
    """Locate the username/login input on a rendered VIAC login page.

    VIAC's exact selectors aren't documented yet (Phase 1 is what
    discovers them), so this uses a structural heuristic: find the
    visible password input, then return the closest preceding
    visible text-like input in DOM order. Mark it with a unique
    attribute so Playwright can address it from Python.
    """
    js = """
    () => {
      const pwd = document.querySelector('input[type="password"]');
      if (!pwd) return null;
      const all = Array.from(document.querySelectorAll('input'));
      const NON_TEXT = new Set([
        'password', 'hidden', 'submit', 'button',
        'checkbox', 'radio', 'file', 'image', 'reset',
      ]);
      const candidates = all.filter(el => {
        if (el === pwd) return false;
        const t = (el.type || 'text').toLowerCase();
        if (NON_TEXT.has(t)) return false;
        if (el.offsetParent === null) return false;
        const cmp = pwd.compareDocumentPosition(el);
        return (cmp & Node.DOCUMENT_POSITION_PRECEDING) !== 0;
      });
      if (!candidates.length) return false;
      const target = candidates[candidates.length - 1];
      target.setAttribute('data-viac-dump-login', '1');
      return true;
    }
    """
    try:
        found = page.evaluate(js)
    except Exception as e:
        log.debug("find_username_field evaluate: %s", e)
        return None
    if found:
        return page.locator('input[data-viac-dump-login="1"]').first
    return None


def prefill(page, login: str, password: str) -> None:
    log.info("waiting for SPA login form to render (timeout %ds)…",
             LOGIN_FORM_TIMEOUT_MS // 1000)
    page.wait_for_selector("input[type='password']",
                           timeout=LOGIN_FORM_TIMEOUT_MS, state="visible")

    user_locator = find_username_field(page)
    if user_locator is None:
        log.warning("could not locate username field heuristically. "
                    "Operator must fill it manually via VNC.")
    else:
        try:
            user_locator.click()
            user_locator.fill(login)
            log.info("login field filled.")
        except Exception as e:
            log.warning("login field fill failed: %s. Operator must fill via VNC.", e)

    try:
        pwd = page.locator("input[type='password']").first
        pwd.click()
        pwd.fill(password)
        log.info("password field filled.")
    except Exception as e:
        log.warning("password field fill failed: %s. Operator must fill via VNC.", e)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    # Load env-file (best-effort) and pull credentials.
    env_path = resolve_env_file(args.env_file)
    if env_path is None:
        log.info("no env file path resolved; relying on process env vars.")
    else:
        try:
            if source_env_file(env_path):
                log.info("env sourced from %s", env_path)
            else:
                log.info("env file %s not present; relying on process env vars.",
                         env_path)
        except (subprocess.CalledProcessError, ValueError) as e:
            if isinstance(e, subprocess.CalledProcessError):
                detail = (e.stderr or b"").decode("utf-8", errors="replace").rstrip()
                log.error("env file %s failed to source (bash exited %d):\n%s",
                          env_path, e.returncode, detail)
            else:
                log.error("%s", e)
            return 2

    login = os.environ.get(LOGIN_ENV)
    password = os.environ.get(PASSWORD_ENV)
    fill = not args.no_fill_credentials
    if fill and (not login or not password):
        log.error(
            "credential pre-fill requested but %s / %s not set. "
            "Either populate ~/.secrets/viac.env, set the env vars "
            "on the host before running the wrapper, or pass "
            "--no-fill-credentials.", LOGIN_ENV, PASSWORD_ENV)
        return 2

    # Directories. Discovery dir must be user-provided
    # (no fallback to ~/.secrets/); profile dir is auth-state-
    # bearing and lives under /secrets by default.
    args.discovery_dir.mkdir(parents=True, exist_ok=True)
    args.profile_dir.mkdir(parents=True, exist_ok=True)
    try:
        # Tighten the profile dir if we created it. CLAUDE.md §3:
        # never relax below 0700 on auth-state dirs.
        os.chmod(args.profile_dir, 0o700)
    except OSError as e:
        log.debug("could not chmod profile dir %s: %s", args.profile_dir, e)

    rec = Recorder(args.discovery_dir)

    log.info("discovery dir: %s", args.discovery_dir)
    log.info("profile dir:   %s", args.profile_dir)
    log.info("login URL:     %s", args.login_url)
    log.info("credential pre-fill: %s", "ON" if fill else "OFF")
    log.warning(
        "request_details.jsonl in the discovery dir will contain "
        "the session cookie, mTAN OTP, and the auth POST body "
        "(login + password). Delete the dir after analysis.")

    with sync_playwright() as p:
        context = p.chromium.launch_persistent_context(
            user_data_dir=str(args.profile_dir),
            headless=False,
            args=["--no-sandbox", "--disable-dev-shm-usage"],
            viewport={"width": 1280, "height": 800},
            accept_downloads=True,
        )
        context.set_default_navigation_timeout(NAV_TIMEOUT_MS)
        page = context.pages[0] if context.pages else context.new_page()

        # Wire recorders. context-level for responses (catches
        # every page, including popup tabs); attach_page() wires
        # per-page handlers (download + framenavigated) and is
        # idempotent. context.on("page", ...) fires for popups
        # so Chromium's new-tab-for-PDF behaviour gets recorded
        # too — the missing piece in session 1.
        context.on("response", rec.on_response)
        context.on("page", rec.attach_page)
        rec.attach_page(page)

        rec.snapshot_storage(context, label=f"start-{utc_ts()}")

        try:
            page.goto(args.login_url)
        except Exception as e:
            log.warning("initial navigation: %s", e)

        if fill:
            try:
                prefill(page, login, password)
            except Exception as e:
                log.error("pre-fill failed: %s — operator must fill via VNC.", e)

        rec.snapshot_storage(context, label=f"post-prefill-{utc_ts()}")

        # Banner to stderr (visible alongside the VNC connect info).
        print("=" * 72, file=sys.stderr, flush=True)
        print("viac-dump explore.py: ready.", file=sys.stderr, flush=True)
        print(f"  Connect a VNC client to 127.0.0.1:5900.",
              file=sys.stderr, flush=True)
        if fill:
            print("  Login + password are pre-filled — click 'Log in' "
                  "and complete 2FA on your phone.",
                  file=sys.stderr, flush=True)
        else:
            print("  Pre-fill OFF: fill the login form manually.",
                  file=sys.stderr, flush=True)
        print("  Walk every relevant view (positions / transactions / "
              "documents).", file=sys.stderr, flush=True)
        print(f"  Recordings: {args.discovery_dir}",
              file=sys.stderr, flush=True)
        print(f"  Ctrl-C in this terminal to stop (auto-stop after "
              f"{args.max_runtime_sec}s).", file=sys.stderr, flush=True)
        print("=" * 72, file=sys.stderr, flush=True)

        stop = {"flag": False}

        def _sigint(_sig, _frame):
            log.info("signal received; shutting down.")
            stop["flag"] = True

        signal.signal(signal.SIGINT, _sigint)
        signal.signal(signal.SIGTERM, _sigint)

        deadline = time.time() + args.max_runtime_sec
        last_periodic = time.time()
        try:
            while not stop["flag"]:
                if time.time() > deadline:
                    log.info("max-runtime reached; shutting down.")
                    break
                # wait_for_timeout yields to Playwright's event
                # dispatcher so queued events fire while we wait;
                # time.sleep() does not, and would let events back
                # up behind the Python interpreter.
                try:
                    page.wait_for_timeout(2000)
                except Exception:
                    # Page may have been closed (e.g. operator
                    # closed the tab). Fall back to plain sleep
                    # so the loop still wakes to check the stop
                    # flag and the deadline.
                    time.sleep(2)
                # Drain the navigation queue from outside the
                # event-dispatch context where page.content() etc.
                # are reliable.
                processed = rec.process_pending()
                if processed:
                    log.debug("processed %d navigation event(s)", processed)
                # Periodic storage snapshot so we capture cookie /
                # localStorage / sessionStorage state across the
                # whole walk, not just at start and end.
                if time.time() - last_periodic > 60:
                    rec.snapshot_storage(context, label=f"periodic-{utc_ts()}")
                    last_periodic = time.time()
        finally:
            rec.snapshot_storage(context, label=f"final-{utc_ts()}")
            rec.close()
            try:
                context.close()
            except Exception as e:
                log.debug("context.close: %s", e)

    log.info("done.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
