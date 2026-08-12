"""Frame-aware helpers for driving Chase's MDS (Manhattan Design System) UI.

Two facts about the Chase surfaces shape all of this (from the explore DOM
snapshots, DESIGN.md §A):

- The login form, the 2FA challenge, and the account app render inside
  chase.com **iframes**, so a main-frame-only lookup misses them.
- The interactive MDS controls (`mds-list-item`, `mds-button`, `mds-select`
  options) are zero-box custom elements whose visible row/button renders
  inside an **open** shadow root — as an `<a href>` (role=link), a role=button,
  or a role=option, deeper in a list/select's shadow tree, not in the host's
  own (empty) shadow. Playwright pierces an open shadow, so the reliable
  activation is a **real Playwright click** on that rendered element
  (`click_role` / a boxed descendant) — verified live. A native `.click()`, a
  synthesised dispatch, keyboard, and coordinate clicks were all tried and did
  not fire the handler.

Shared by `login.py` (2FA) and `download.py` (the export/statement scrape).
"""
from __future__ import annotations

import contextlib
from urllib.parse import urlparse

# The chase.com origin gate lives with the prefill it was first written for.
from explore import HOST_RE

__all__ = ["HOST_RE", "chase_frames", "visible_in_frames", "locate",
           "first_in_frames", "click", "click_text", "click_role",
           "deep_click_text", "activate"]


def chase_frames(page):
    """Yield the frames served from a chase.com origin — the login iframe and
    the app frames — skipping the main document and the decoy password-manager
    frame. The single origin gate the rest of the module iterates over."""
    for frame in getattr(page, "frames", []) or []:
        try:
            host = urlparse(frame.url).hostname or ""
        except Exception:
            continue
        if HOST_RE.search(host):
            yield frame


def visible_in_frames(page, make_locator):
    """First VISIBLE element `make_locator(frame)` resolves to, across the
    chase frames. Iterates the matches, not just `.first`: MDS ids are
    duplicated (a hidden template beside the live control), so `.first` can be
    the hidden one. Returns a Locator, or None."""
    for frame in chase_frames(page):
        with contextlib.suppress(Exception):
            loc = make_locator(frame)
            for i in range(min(loc.count(), 8)):
                item = loc.nth(i)
                with contextlib.suppress(Exception):
                    if item.is_visible():
                        return item
    return None


def locate(page, selector: str):
    """First visible match for a CSS `selector` across the chase frames."""
    return visible_in_frames(page, lambda f: f.locator(selector))


def first_in_frames(page, selector: str):
    """First EXISTING (not necessarily visible) match — the read/activation
    target for an element that may have no box of its own."""
    for frame in chase_frames(page):
        with contextlib.suppress(Exception):
            loc = frame.locator(selector).first
            if loc.count():
                return loc
    return None


def click(page, selector: str, timeout: int = 5000) -> bool:
    """Frame-aware click of the first visible match for `selector`."""
    loc = locate(page, selector)
    if loc is None:
        return False
    with contextlib.suppress(Exception):
        loc.click(timeout=timeout)
        return True
    return False


def click_role(page, role: str, name: str, *, exact: bool = False) -> bool:
    """Real Playwright click of `get_by_role(role, name=…)` across the chase
    frames. The PROVEN path for MDS: its navigational list rows render as
    `<a href>` (role=link) and its buttons as role=button inside an OPEN shadow
    root, which Playwright pierces; a real click on that element fires the
    handler where a native `.click()` did not. Verified live."""
    for frame in chase_frames(page):
        with contextlib.suppress(Exception):
            loc = frame.get_by_role(role, name=name, exact=exact).first
            if loc.count() == 0:
                continue
            loc.scroll_into_view_if_needed(timeout=3000)
            loc.click(timeout=5000)
            return True
    return False


def click_text(page, text: str, *, exact: bool = False) -> bool:
    """Click the first visible element rendering `text`; `get_by_text` pierces
    the open shadow to reach the label the zero-box host does not expose."""
    loc = visible_in_frames(page, lambda f: f.get_by_text(text, exact=exact))
    if loc is None:
        return False
    with contextlib.suppress(Exception):
        loc.click(timeout=5000)
        return True
    return False


# Walk the frame's document AND every open shadow root (BFS), find the leaf
# element rendering `needle`, then climb — crossing shadow boundaries via
# `getRootNode().host` — to the nearest clickable ancestor and native-click it.
# Reaches a row rendered deep in a list's shadow tree, not in the
# mds-list-item's own (empty) shadow. Returns the clicked descriptor, or null.
_DEEP_CLICK_JS = r"""
([needle, exact]) => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
  const CLICKABLE = 'a[href],button,[role="button"],[role="link"],'
    + '[role="menuitem"],[role="option"],[role="radio"],[onclick],'
    + '[tabindex]:not([tabindex="-1"])';
  const roots = [document];
  for (let i = 0; i < roots.length; i++) {
    let els; try { els = roots[i].querySelectorAll('*'); } catch (e) { continue; }
    for (const el of els) if (el.shadowRoot) roots.push(el.shadowRoot);
  }
  const hit = t => exact ? t === needle : t.includes(needle);
  for (const root of roots) {
    let els; try { els = root.querySelectorAll('*'); } catch (e) { continue; }
    for (const el of els) {
      if (el.childElementCount === 0 && hit(norm(el.textContent))) {
        let n = el;
        while (n) {
          let c = null;
          try { c = n.closest ? n.closest(CLICKABLE) : null; } catch (e) {}
          if (c) {
            c.click();
            return c.tagName.toLowerCase()
                 + (c.getAttribute('role') ? '[role=' + c.getAttribute('role') + ']' : '');
          }
          const rt = n.getRootNode();
          n = (rt && rt.host) ? rt.host : n.parentElement;
        }
        try { el.click(); return el.tagName.toLowerCase(); } catch (e) {}
      }
    }
  }
  return null;
}
"""


def deep_click_text(page, text: str, *, exact: bool = True) -> bool:
    """Click the control rendering `text`, piercing every open shadow root to
    reach it (see `_DEEP_CLICK_JS`). Fallback for MDS rows whose handler sits on
    an inner element deep in a list's shadow tree."""
    for frame in chase_frames(page):
        with contextlib.suppress(Exception):
            if frame.evaluate(_DEEP_CLICK_JS, [text, exact]):
                return True
    return False


# Boxed, clickable descendants an MDS control renders inside its open shadow
# root — the click lands on one of these, and the row/button delegates the
# handler off the zero-box host.
_MDS_INNER = "button, a, [role='button'], [role='link'], [role='option'], label"

# Native `.click()` on the interactive element inside an element's open shadow
# root, where the component's own handler lives. Broad query so it works
# whatever tag the row/button is; falls back to the shadow's first child.
_SHADOW_CLICK_JS = r"""
(el) => {
  const sr = el.shadowRoot;
  if (!sr) return null;
  const c = sr.querySelector(
    'button, a[href], [role="button"], [role="link"], [role="menuitem"],'
    + ' [role="option"], [tabindex]:not([tabindex="-1"]), input')
    || sr.firstElementChild;
  if (!c) return null;
  try { c.scrollIntoView({block: 'center'}); } catch (e) {}
  c.click();
  return true;
}
"""


def _shadow_click(page, selector: str) -> bool:
    """Click the interactive element inside the open shadow root of the
    element(s) matching `selector`, via native `.click()`. Iterates the matches
    so a hidden duplicate template is tolerated."""
    for frame in chase_frames(page):
        with contextlib.suppress(Exception):
            loc = frame.locator(selector)
            for i in range(min(loc.count(), 8)):
                with contextlib.suppress(Exception):
                    if loc.nth(i).evaluate(_SHADOW_CLICK_JS):
                        return True
    return False


def activate(page, selector: str) -> bool:
    """Activate an MDS control addressed by its host `selector`: click the host
    (boxed builds), then the interactive element inside its open shadow (native
    click), then a boxed descendant Playwright reaches through the open shadow.
    A selector-based fallback to `click_role`, whose primary path handles the
    live controls."""
    if click(page, selector):
        return True
    if _shadow_click(page, selector):
        return True
    host = first_in_frames(page, selector)
    if host is not None:
        with contextlib.suppress(Exception):
            inner = host.locator(_MDS_INNER).first
            if inner.count() and inner.is_visible():
                inner.click(timeout=4000)
                return True
    return False
