#!/usr/bin/env python3
"""
Shared HTTP client for VIAC's REST API.

VIAC's web portal is a JSON-over-cookies SPA backend. Phase 1
discovery (see DESIGN.md) established that after the mTAN-gated
login flow, the session is held entirely in a single httpOnly
cookie (`AL_SESS-S`) plus a CSRF token in `CSRFT<N>-S` that the
SPA echoes back as an `x-csrft<N>` request header on every
state-mutating verb (POST / PUT / PATCH / DELETE).

The `<N>` digit suffix is deploy-bound — bundle-build-version
shaped, rotates on the next VIAC release. We detect it at
runtime from the cookie jar (regex `^CSRFT\\d+-S$`) and derive
the header name by stripping the `-S` and lowercasing.

login.py and download.py both use this client; neither needs a
real browser. (Phase 1's `explore.py` keeps Playwright for the
discovery role; once the API surface drifts we re-run that.)
"""

from __future__ import annotations

import json
import re
from datetime import datetime, timezone
from pathlib import Path

import httpx

BASE_URL = "https://app.viac.ch"

# Realistic Chromium UA. VIAC accepts it across captured sessions.
# No fingerprinting evidence in the auth flow — the server doesn't
# appear to challenge non-browser clients with this UA.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
)

# Headers the SPA sends on every request. Included defensively —
# we don't know which are load-bearing, so we ship them all.
DEFAULT_HEADERS = {
    "accept": "application/json",
    "accept-language": "en-US,en;q=0.9",
    "cache-control": "no-cache, no-store",
    "expires": "0",
    "pragma": "no-cache",
    "referer": f"{BASE_URL}/",
    "user-agent": USER_AGENT,
    "x-same-domain": "1",
}

# CSRF double-submit cookie name. Digits rotate per deploy; detect
# at runtime rather than hardcoding.
CSRF_COOKIE_PATTERN = re.compile(r"^CSRFT\d+-S$")

# Verbs that mutate state and require the CSRF header.
_CSRF_VERBS = frozenset({"POST", "PUT", "PATCH", "DELETE"})


def csrf_header_for(cookie_name: str) -> str:
    """Map cookie name to request-header name: `CSRFT<N>-S` → `x-csrft<N>`."""
    if not CSRF_COOKIE_PATTERN.match(cookie_name):
        raise ValueError(f"unexpected CSRF cookie name: {cookie_name!r}")
    return cookie_name[:-2].lower()


def detect_csrf_cookie_name(cookies: httpx.Cookies) -> str | None:
    """Find the CSRF cookie in `cookies` by name pattern."""
    for c in cookies.jar:
        if CSRF_COOKIE_PATTERN.match(c.name):
            return c.name
    return None


class ViacClient:
    """httpx.Client wrapper that auto-adds the CSRF header on
    mutating requests. CSRF cookie/header names are detected from
    the cookie jar on first use; the caller can also supply them
    explicitly via `from_state()`.
    """

    def __init__(
        self,
        *,
        csrf_cookie_name: str | None = None,
        csrf_header_name: str | None = None,
        cookies: httpx.Cookies | None = None,
    ):
        self._client = httpx.Client(
            base_url=BASE_URL,
            headers=DEFAULT_HEADERS,
            cookies=cookies if cookies is not None else httpx.Cookies(),
            http2=True,
            follow_redirects=False,
            timeout=30.0,
        )
        self.csrf_cookie_name = csrf_cookie_name
        self.csrf_header_name = csrf_header_name

    def __enter__(self) -> "ViacClient":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self) -> None:
        self._client.close()

    @property
    def cookies(self) -> httpx.Cookies:
        return self._client.cookies

    def _ensure_csrf_known(self) -> None:
        if self.csrf_cookie_name is None:
            name = detect_csrf_cookie_name(self.cookies)
            if name is not None:
                self.csrf_cookie_name = name
                self.csrf_header_name = csrf_header_for(name)

    def request(self, method: str, path: str, **kwargs) -> httpx.Response:
        self._ensure_csrf_known()
        if method.upper() in _CSRF_VERBS and self.csrf_header_name:
            value = self.cookies.get(self.csrf_cookie_name)
            if value:
                headers = dict(kwargs.pop("headers", {}) or {})
                headers[self.csrf_header_name] = value
                # Real browsers add Origin on mutating requests.
                headers.setdefault("origin", BASE_URL)
                kwargs["headers"] = headers
        return self._client.request(method, path, **kwargs)

    # Convenience wrappers.
    def get(self, path: str, **kwargs) -> httpx.Response:
        return self.request("GET", path, **kwargs)

    def post(self, path: str, **kwargs) -> httpx.Response:
        return self.request("POST", path, **kwargs)

    def delete(self, path: str, **kwargs) -> httpx.Response:
        return self.request("DELETE", path, **kwargs)

    def stream(self, method: str, path: str, **kwargs):
        """Streaming variant for PDF downloads (avoid loading 1019
        full PDF bodies into RAM at once)."""
        self._ensure_csrf_known()
        return self._client.stream(method, path, **kwargs)

    # State serialization ---------------------------------------

    def save_state(self, path: Path) -> None:
        """Serialize cookies + CSRF metadata at chmod 0600.

        CLAUDE.md §3 — the file holds the live session cookie;
        never relax the mode."""
        cookies = []
        for c in self.cookies.jar:
            cookies.append({
                "name": c.name,
                "value": c.value,
                "domain": c.domain,
                "path": c.path,
                "secure": c.secure,
                "expires": c.expires,
            })
        state = {
            "saved_at": datetime.now(timezone.utc).isoformat(),
            "csrf_cookie_name": self.csrf_cookie_name,
            "csrf_header_name": self.csrf_header_name,
            "cookies": cookies,
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        # Write then chmod so the temporary mode never widens 0600.
        path.write_text(json.dumps(state, indent=2))
        path.chmod(0o600)

    @classmethod
    def from_state(cls, path: Path) -> "ViacClient":
        state = json.loads(path.read_text())
        jar = httpx.Cookies()
        for c in state["cookies"]:
            jar.set(
                c["name"], c["value"],
                domain=c.get("domain", ""),
                path=c.get("path", "/"),
            )
        return cls(
            csrf_cookie_name=state.get("csrf_cookie_name"),
            csrf_header_name=state.get("csrf_header_name"),
            cookies=jar,
        )
