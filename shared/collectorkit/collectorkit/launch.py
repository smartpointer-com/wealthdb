"""Launch hardening for the collectors that drive a browser.

Three launch paths are in play. The scrapers behind bot-detection (Akamai,
Cloudflare) run Camoufox — a patched Firefox — against a *persistent*
profile dir under `/secrets/<source>-profile`, because the session cookie
has to survive between the `login` and `download` verbs. Others drive
vanilla Playwright Chromium with an ephemeral profile and a JSON
`storage_state` file. One collector whose login is bot-walled past any
automation stack starts a *stock* Firefox binary from its entrypoint, for
a by-hand sign-in whose cookie jar is lifted afterwards — that one takes
its prefs as a rendered `user.js` (see `firefox_user_js`) because a plain
binary gets no `firefox_user_prefs`.

Both engines default to writing regenerable caches next to that session
state. On a persistent profile those caches accumulate without bound; on
an ephemeral one they still land on disk for the life of the run. Either
way they are a second copy of data the collector already has:

- Firefox `cache2/` (and Chromium's `Cache`/`Code Cache`/GPU + shader
  caches) hold HTTP response bodies fetched inside an authenticated
  banking session — balances and holdings at rest, in a hash-keyed blob
  store with no request ordering, timings, or replayable structure. The
  financial XHR endpoints send `no-store` and never cache anyway, so
  nothing diagnostic is lost; the collectors' `--trace` (a Playwright
  trace) is the strictly better debugging artefact.
- Firefox `places.sqlite` / `favicons.sqlite` are a browsing history of
  those same sessions.
- `datareporting/` is telemetry the collectors never read.

Suppressing all of it leaves a persistent profile holding only session
state — `cookies.sqlite`, `key4.db`, `cert9.db`, `prefs.js` — a few MB that
stays flat run over run instead of growing without limit.

The in-*memory* cache stays enabled: within-run caching is where the
benefit is, and it never reaches disk.

Deliberately untouched: `storage/` (IndexedDB). Financial SPAs keep real
session state there, so disabling or pruning it breaks logins. It is
application state, not an HTTP cache.

Both helpers return fresh copies, so a caller may mutate the result
without affecting the next call.
"""
from __future__ import annotations

import json

# Validated against Firefox 135, the build camoufox-py 0.4.11 downloads
# (see shared/images/base-camoufox.Dockerfile). Playwright forwards this
# dict verbatim to the launch; Camoufox passes **launch_options through to
# Playwright, so the same dict works for both.
FIREFOX_PREFS: dict[str, bool | int] = {
    # --- Password manager ------------------------------------------
    # A saved credential in the persistent profile autofills on top of a
    # collector's programmatic fill, concatenating the password field and
    # getting the login rejected. These stop Firefox saving, generating,
    # or autofilling logins, leaving the collector as the only writer.
    "signon.rememberSignons": False,
    "signon.autofillForms": False,
    "signon.generation.enabled": False,
    "signon.management.page.breach-alerts.enabled": False,

    # --- Disk cache ------------------------------------------------
    # `capacity` is in KB and pairs with `enable`: the smart-size logic
    # would otherwise recompute a capacity from free disk space and
    # override the 0.
    "browser.cache.disk.enable": False,
    "browser.cache.disk.capacity": 0,
    "browser.cache.disk.smart_size.enabled": False,
    # Memory cache stays ON — see the module docstring.
    "browser.cache.memory.enable": True,

    # --- History & favicons ----------------------------------------
    "places.history.enabled": False,
    "browser.chrome.site_icons": False,

    # --- Telemetry -------------------------------------------------
    # `datareporting.policy.dataSubmissionEnabled` is the master switch;
    # the rest keep the subsystems from staging archives on disk.
    "datareporting.policy.dataSubmissionEnabled": False,
    "datareporting.healthreport.uploadEnabled": False,
    "toolkit.telemetry.unified": False,
    "toolkit.telemetry.archive.enabled": False,
}

# The shared set disables Firefox's password manager: a saved credential
# autofills on top of a collector's programmatic fill and breaks the login
# (see FIREFOX_PREFS). A profile that is signed into by hand needs the
# opposite — Firefox autofilling the saved login on the next sign-in — and
# layers this back over the shared set. Such a profile is itself a
# credential store, kept under ~/.secrets at 0700.
PASSWORD_MANAGER_ON: dict[str, bool] = {
    "signon.rememberSignons": True,
    "signon.autofillForms": True,
}

# Chromium has no single "disk cache off" switch. A size of 0 means
# "pick a default", so the caches are pinned to 1 byte instead — the
# backend then evicts everything immediately and nothing accumulates.
CHROMIUM_CACHE_ARGS: tuple[str, ...] = (
    "--disk-cache-size=1",
    "--media-cache-size=1",
    "--disable-gpu-shader-disk-cache",
)


def firefox_prefs(**overrides: bool | int | str) -> dict[str, bool | int | str]:
    """The canonical `firefox_user_prefs` for a collector launch.

    Passed to `Camoufox(..., firefox_user_prefs=firefox_prefs())` or
    `playwright.firefox.launch_persistent_context(..., firefox_user_prefs=
    firefox_prefs())`. `overrides` win over the defaults, for the rare
    per-collector pref that has nothing to do with caching.
    """
    return {**FIREFOX_PREFS, **overrides}


def chromium_args(*extra: str) -> list[str]:
    """The canonical Chromium `args` for a collector launch.

    Returns the cache-suppression flags followed by `extra` — each
    collector's own sandbox / anti-detection flags, which differ by target
    and stay at the call site.
    """
    return [*CHROMIUM_CACHE_ARGS, *extra]


def firefox_user_js(**overrides: bool | int | str) -> str:
    """The same pref set rendered as a `user.js` for a profile dir.

    A Firefox started as a plain binary (rather than through Playwright)
    takes no `firefox_user_prefs`; it reads `<profile>/user.js` on startup.
    Rendering that file from `FIREFOX_PREFS` keeps a hand-launched profile
    on the same prefs as the driven ones instead of a bash heredoc drifting
    out of sync. `overrides` win over the defaults and are applied as a
    merge, so an override restates a shared pref rather than emitting a
    second, conflicting line.
    """
    prefs = firefox_prefs(**overrides)
    lines = ["// Generated by collectorkit.launch.firefox_user_js — do not",
             "// edit here; change the shared pref set or the caller's",
             "// overrides instead."]
    # json.dumps is exactly JS literal syntax for bool/int/str: True -> true,
    # 0 -> 0, and strings quoted with the escaping (and \uXXXX for non-ASCII)
    # a pref value needs.
    lines += [f"user_pref({json.dumps(k)}, {json.dumps(v)});"
              for k, v in prefs.items()]
    return "\n".join(lines) + "\n"
