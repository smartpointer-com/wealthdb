"""The sign-in form fill shared by explore harnesses and logins, and the
one submit a login makes.

A harness pre-fills the source's sign-in form so the person at the VNC
session only drives the challenge. A collector's ``login.py`` often reuses
the same fill, with the same selectors, before it submits. The fill never
submits.

- :func:`load_credentials` sources the env file and reads the username and
  password from their env names.
- :func:`prefill_login` fills the form, origin-gated and frame-aware, each
  field at most once per page.
- :func:`submit_once` submits a sign-in or a code with one gesture.

:mod:`collectorkit.explore` re-exports the fill and the credentials.
"""
from __future__ import annotations

import logging
import os
import re
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import debugcap, envfile

# The fields a sign-in form is recognised by when a source names none of its
# own. A source with a known control passes its own selector first.
DEFAULT_PASSWORD_SELECTOR = "input[type='password']"
DEFAULT_OTP_SELECTOR = (
    "input[autocomplete='one-time-code'], input[inputmode='numeric'], "
    "input[name*='otp' i], input[name*='code' i], input[id*='otp' i], "
    "input[id*='code' i], input[maxlength='6'], input[maxlength='8']"
)


def load_credentials(env_file: Path | None, user_envs: tuple[str, ...],
                     pass_envs: tuple[str, ...], *, no_prefill: bool,
                     host: str, log: logging.Logger) -> tuple[str, str, bool]:
    """Source the env file and read the login credentials.

    `env_file` is sourced when it exists and skipped when it does not; its
    values win over the environment's. Each credential is the first
    non-empty of its env names. Returns (username, password, prefill),
    where `prefill` says whether both are there to fill and
    ``--no-prefill`` did not refuse them.
    """
    envfile.load(None, candidates=(env_file,), logger=log)
    username = _first_set(user_envs)
    password = _first_set(pass_envs)
    if no_prefill:
        log.info("--no-prefill: login form pre-fill disabled")
    elif not (username and password):
        log.warning("%s / %s not set; login-form pre-fill disabled. Drop "
                    "credentials into %s to enable.", "/".join(user_envs),
                    "/".join(pass_envs), env_file)
    else:
        log.info("pre-fill ready (origin-gated to %s)", host)
    return username, password, bool(username and password) and not no_prefill


def _first_set(names: tuple[str, ...]) -> str:
    return next((os.environ[k] for k in names if os.environ.get(k)), "")


def prefill_login(page, username: str, password: str, filled: set, *,
                  host_re: re.Pattern, user_selector: str,
                  password_selector: str = DEFAULT_PASSWORD_SELECTOR,
                  user_alt_selector: str | None = None,
                  overwrite: bool = False, log: logging.Logger) -> bool:
    """Fill the username and password fields of a sign-in form, each AT
    MOST ONCE per page (tracked in `filled`, keyed by (id(page), kind)).
    Never submits. Returns True iff a field was newly filled and verified.

    Frame-aware: the form is looked for in every frame whose host matches
    `host_re`, so the credentials never touch a third-party frame. The
    password is filled only beside a username control in the SAME frame,
    so it can never land in a lone code-entry field. That control is the
    `user_selector` input, or the `user_alt_selector` control (a
    remembered-username drop-down), which is left alone. The first frame
    holding the form is the only one filled.

    Each fill is verified by read-back, with one clear-and-retry, so a
    stray autofill can never concatenate onto a correct value. A field is
    marked done whatever the outcome, so a later DOM mutation never
    re-fills over a hand-typed value. A field that already has content is
    marked done untouched, unless `overwrite`: a source that pre-fills a
    masked username of its own needs the real one written over it.
    """
    did = False
    for frame in page.frames:
        try:
            host = urlparse(frame.url).hostname or ""
        except Exception:  # noqa: BLE001
            continue
        if not host_re.search(host):
            continue
        try:
            # count() / is_visible() are instant (no auto-wait), so a frame
            # without the form costs nothing per poll.
            pwd_field = frame.locator(password_selector).first
            if pwd_field.count() == 0 or not pwd_field.is_visible():
                continue
            user_field = frame.locator(user_selector).first
            has_user = user_field.count() > 0 and user_field.is_visible()
            has_alt = False
            if user_alt_selector:
                alt = frame.locator(user_alt_selector).first
                has_alt = alt.count() > 0 and alt.is_visible()
            if not (has_user or has_alt):
                continue
        except Exception as exc:  # noqa: BLE001
            log.debug("prefill frame probe failed: %s",
                      debugcap.safe_error(exc))
            continue
        targets = [("pwd", pwd_field, password)]
        if has_user:
            targets.insert(0, ("user", user_field, username))
        for kind, field, value in targets:
            key = (id(page), kind)
            if key in filled:
                continue
            try:
                if field.input_value(timeout=1000) and not overwrite:
                    filled.add(key)       # hand-typed: leave it alone
                    continue
                field.fill(value, timeout=2000)
                got = field.input_value(timeout=1500)
                if got != value:
                    field.fill("", timeout=1500)
                    field.fill(value, timeout=2000)
                    got = field.input_value(timeout=1500)
                filled.add(key)
                if got == value:
                    did = True
                else:
                    log.warning(
                        "pre-fill of %s field did not stick (got len %d, "
                        "want %d) — type it by hand",
                        kind, len(got or ""), len(value))
            except Exception as exc:  # noqa: BLE001
                # safe_error, never %r: a failed fill() renders the typed
                # value verbatim inside Playwright's call log.
                log.debug("prefill %s field failed: %s", kind,
                          debugcap.safe_error(exc))
        break
    return did


_log = logging.getLogger(__name__)

SUBMIT_GESTURES = ("click", "enter")


def submit_once(choices, *, what: str = "the form",
                timeout_ms: int = 5000) -> str | None:
    """Submit a sign-in form or a code with exactly one gesture.

    `choices` are (locator, gesture) pairs in the order a submit prefers
    them, the gesture "click" or "enter" (Enter pressed in that field). The
    first whose control is on the page, visible and (for a click) enabled
    gets the one gesture, and no other is tried after it. A gesture that
    raised may still have gone out, since a navigation can detach a control
    under the click, and a second gesture would submit again: a second push
    or code, and a step closer to a lockout. The caller then looks for the
    outcome (a challenge, an error), which also tells whether it went out.

    Returns the gesture made ("click" or "enter"), raised or not, or None
    when no control was there and nothing was sent."""
    for locator, gesture in choices:
        if gesture not in SUBMIT_GESTURES:
            raise ValueError(f"no such submit gesture: {gesture!r}")
        try:
            if not locator.count() or not locator.is_visible():
                continue
            if gesture == "click" and not locator.is_enabled():
                continue
        except Exception:
            continue
        try:
            if gesture == "click":
                locator.click(timeout=timeout_ms)
            else:
                locator.press("Enter")
        except Exception as exc:
            _log.warning("submitting %s: the %s may not have gone out (%s). "
                        "Not sending it again.", what, gesture,
                        debugcap.safe_error(exc))
        return gesture
    return None
