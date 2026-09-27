#!/usr/bin/env python3
"""
UBS Switzerland e-banking session minter.

Drives headless Chromium through the UBS login dialog and the
Access App QR challenge, then persists the resulting Playwright
storageState.json. Subsequent download.py runs reuse that file
until UBS invalidates the session.

The `--check` mode validates an existing state file against a live
landmark URL without re-logging in (no QR challenge). See AGENTS.md
§2: non-check invocations must be explicitly authorised.

Usage:
    login.py [--contract-number <num>]
             [--env-file <path>] [--state-path <file>]
             [--check] [--mfa-timeout <sec>]
             [--qr-png <path>] [--no-terminal-qr]
             [--screenshot-dir <dir>] [--trace]
"""

from __future__ import annotations

import argparse
import base64
import io
import logging
import os
import sys
import time
from pathlib import Path

import landmarks as ubs  # local module: URL + DOM landmarks

from collectorkit import bronze, cli, envfile, launch, session

log = logging.getLogger("ubs-web.login")

# Real desktop Chrome UA, not HeadlessChrome. Banks commonly sniff
# `HeadlessChrome` and either block or add anti-bot steps; this
# string matches the Chrome major version Playwright 1.62 ships, so
# it's plausible without being deceptive about capabilities. The
# only signal we strip is the "Headless" qualifier.
#
# The major version matters: UBS server-side redirects UAs below a
# minimum Chrome version to a "browser outdated" page (observed
# 2026-08 rejecting Chrome 147), so this string must move forward
# in lockstep with the Playwright base-image bump.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/151.0.0.0 Safari/537.36"
)

# Playwright timeouts (milliseconds). Generous defaults — WAN
# latency from arbitrary cloud regions to UBS can be high.
NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 30_000

# Default env-file locations (the wrapper mounts ~/.secrets at /secrets).
# ubs-web owns `<source>.env` (ubs-web.env) for the contract number; the
# bank-level `ubs.env` is a fallback, shared with any ubs-* sibling. First
# existing wins. Host paths let the script run outside the container for
# local dev.
DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/ubs-web.env"),
    Path.home() / ".secrets" / "ubs-web.env",
    Path("/secrets/ubs.env"),
    Path.home() / ".secrets" / "ubs.env",
)

# Env var the contract number is read from. The companion
# `~/.secrets/ubs.env` file should contain a line of the form
# `UBS_CONTRACT_NUMBER=<digits>`.
CONTRACT_NUMBER_ENV = "UBS_CONTRACT_NUMBER"

# Canonical storageState location: the wrapper mounts the secrets
# dir (default ~/.secrets, overridable via UBS_WEB_SECRETS_DIR /
# WEALTHDB_SECRETS_DIR) at /secrets, so the session state lives
# there by default and survives across container runs.
DEFAULT_STATE_PATH = Path("/secrets/ubs-web-state.json")
# Fallback state location (underscore) — read when the canonical file is absent,
# so a session stored under this name keeps working. New logins write
# DEFAULT_STATE_PATH, migrating the session to the hyphenated name.
LEGACY_STATE_PATH = Path("/secrets/ubs_web_state.json")


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--state-path", type=Path, default=DEFAULT_STATE_PATH,
        help=("Path to read/write the Playwright storageState.json file "
              "(default: %(default)s, in the wrapper's /secrets mount)."),
    )
    p.add_argument(
        "--contract-number", default=None,
        help=("UBS contract number (8-digit customer ID). Falls back to "
              f"${CONTRACT_NUMBER_ENV} env var, which is typically "
              "sourced from ~/.secrets/ubs.env — see --env-file."),
    )
    p.add_argument(
        "--env-file", default=None, type=Path,
        help=("Path to a KEY=VALUE env file to load before resolving "
              "the contract number. Defaults to /secrets/ubs.env if "
              "present, else ~/.secrets/ubs.env."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Validate the existing state file against a landmark URL. "
              "No new login, no QR challenge."),
    )
    p.add_argument(
        "--mfa-timeout", type=int, default=300,
        help="Seconds to wait for Access-App approval (default: 300).",
    )
    p.add_argument(
        "--qr-png", default=None, type=Path,
        help=("Write the QR PNG to this path (useful for sending the "
              "QR to your phone via scp/AirDrop if terminal-rendered "
              "QR is unreadable). If unset, no PNG is written."),
    )
    p.add_argument(
        "--no-terminal-qr", action="store_true",
        help="Skip printing the QR to the terminal. Requires --qr-png.",
    )
    p.add_argument(
        "--screenshot-dir", default=None, type=Path,
        help=("If set, write a screenshot at each navigation landmark. "
              "Useful for debugging on a headless remote host. NEVER "
              "commit these — see AGENTS.md §4."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help=("Capture a Playwright trace bundle. Requires "
              "--screenshot-dir; the bundle lands there alongside "
              "screenshots. Never auto-writes to the secrets dir."),
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


# ============================================================
# Env-file loader
# ============================================================

def resolve_contract_number(args: argparse.Namespace) -> str:
    """CLI flag → env var → error. Honours --env-file.

    Per AGENTS.md §3 (secrets / value-with-env-fallback memory):
    the contract number can be passed by value on the CLI or read
    from an env var, but never as a password-style flag.
    """
    env_files: list[Path]
    if args.env_file is not None:
        if not args.env_file.exists():
            raise SystemExit(f"--env-file does not exist: {args.env_file}")
        env_files = [args.env_file]
    else:
        # First existing candidate wins (ubs-web.env over the bank-level
        # ubs.env); don't source both, which would let ubs.env override.
        env_files = [p for p in DEFAULT_ENV_FILE_CANDIDATES if p.exists()][:1]

    for env_file in env_files:
        envfile.source_env_file(env_file, prefer_file=True)

    if args.contract_number:
        return args.contract_number.strip()
    env = os.environ.get(CONTRACT_NUMBER_ENV)
    if env:
        return env.strip()
    raise SystemExit(
        f"Missing contract number: pass --contract-number, set "
        f"${CONTRACT_NUMBER_ENV}, or put it in ~/.secrets/ubs.env."
    )


# ============================================================
# Screenshot / trace helpers
# ============================================================

def maybe_screenshot(page, screenshot_dir: Path | None, label: str) -> None:
    if screenshot_dir is None:
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    path = screenshot_dir / f"{bronze.ts_slug()}-{label}.png"
    page.screenshot(path=str(path), full_page=True)
    log.debug("wrote screenshot %s", path)


# ============================================================
# QR rendering
# ============================================================

def decode_data_url_png(data_url: str) -> bytes:
    """Decode a `data:image/png;base64,<...>` URL to raw PNG bytes.

    UBS prefixes the base64 string with a stray space (the literal
    that spa.js prepends: `"data:image/png;base64, " + e.qrCodeImage`).
    Tolerate both forms.
    """
    if not data_url.startswith("data:image/"):
        raise ValueError(f"unexpected QR image src (not a data URL): "
                         f"{data_url[:40]!r}...")
    _, _, b64 = data_url.partition(",")
    return base64.b64decode(b64.strip())


def render_qr_to_terminal(png_bytes: bytes, out=sys.stdout) -> None:
    """Print a QR PNG to the terminal as Unicode half-blocks.

    Strategy: load the PNG, threshold to bilevel, detect the QR
    module pitch from the first dark-pixel run in the top-left
    finder pattern, downsample to one terminal-pixel per module,
    and emit `▀` / `▄` / `█` / ` ` so each pair of vertical modules
    shows up as one terminal cell. Pairs make the QR look square
    instead of tall.

    Pillow is used for the decoding and pixel access; we deliberately
    do NOT decode the QR's payload — re-rendering the bitmap as-is
    is enough for the Access App to scan and avoids pulling in
    libzbar / opencv-headless.
    """
    try:
        from PIL import Image  # noqa: WPS433 — runtime dep, see requirements.txt
    except ImportError as e:
        raise SystemExit(
            "Pillow is required to render the QR to the terminal. "
            "Install it (`pip install Pillow`) or pass --no-terminal-qr "
            "and use --qr-png to fetch the QR via SFTP instead."
        ) from e

    img = Image.open(io.BytesIO(png_bytes)).convert("L")  # grayscale
    w, h = img.size
    px = img.load()

    # Threshold to 0/1 (dark = 1). UBS draws black QR modules on
    # white; mid-grey antialiased edges are rare on these renders.
    thresh = 128
    bits = [[1 if px[x, y] < thresh else 0 for x in range(w)] for y in range(h)]

    # Detect the module pitch: scan the top row of the finder
    # pattern (first dark-pixel row from the top), count the run
    # of consecutive dark pixels — that's the width of 7 modules.
    module_px = _detect_module_pitch(bits, w, h)
    if module_px is None:
        # Fallback: just dump the raw image at one terminal pixel
        # per source pixel. Will be huge but works for debugging.
        log.warning("could not detect QR module pitch; rendering raw")
        module_px = 1

    # Downsample to one cell per module. Sample the centre pixel
    # of each module to avoid edge-AA artefacts.
    modules_x = w // module_px
    modules_y = h // module_px
    half = module_px // 2

    def sample(mx: int, my: int) -> int:
        sx = mx * module_px + half
        sy = my * module_px + half
        if 0 <= sx < w and 0 <= sy < h:
            return bits[sy][sx]
        return 0

    # Quiet zone — Access App scans more reliably with a few cells
    # of whitespace padding around the QR.
    pad = 2

    # Render two rows of modules per terminal line using half-blocks.
    write = out.write
    write("\n")
    write(" " * (modules_x + 2 * pad) + "\n")  # top padding
    for my in range(-pad, modules_y + pad, 2):
        line_chars: list[str] = []
        for mx in range(-pad, modules_x + pad):
            top = sample(mx, my) if 0 <= mx < modules_x and 0 <= my < modules_y else 0
            bot = sample(mx, my + 1) if 0 <= mx < modules_x and 0 <= my + 1 < modules_y else 0
            line_chars.append(_HALFBLOCK[(top, bot)])
        write("".join(line_chars))
        write("\n")
    write(" " * (modules_x + 2 * pad) + "\n")  # bottom padding
    out.flush()


_HALFBLOCK = {
    (0, 0): " ",
    (1, 0): "▀",  # ▀ upper half block
    (0, 1): "▄",  # ▄ lower half block
    (1, 1): "█",  # █ full block
}


def _detect_module_pitch(bits, w: int, h: int) -> int | None:
    """Find the QR module size in source pixels.

    The top-left finder pattern is a 7-module dark square with a
    5-module dark centre. We scan rows from the top until we find
    one whose first dark pixel begins a long horizontal dark run;
    that run is exactly 7 modules wide (the outer black border of
    the finder pattern at the very top of the pattern).
    """
    for y in range(h):
        x = 0
        while x < w and bits[y][x] == 0:
            x += 1
        if x == w:
            continue
        start = x
        while x < w and bits[y][x] == 1:
            x += 1
        run = x - start
        if run >= 7:
            pitch = run // 7
            if pitch >= 1:
                return pitch
    return None


# ============================================================
# Playwright flows
# ============================================================

def run_check(state_path: Path, screenshot_dir: Path | None,
              trace: bool) -> int:
    """Validate that the persisted state still authenticates.

    Hits a single protected URL with the stored cookies; if UBS
    redirects us into the workbench (`?navitemid=...`), the
    session is alive. If it serves the contract-entry dialog or
    a Nevis logout interstitial, the session has expired.
    """
    from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

    if not state_path.exists():
        log.error("state file does not exist: %s", state_path)
        return 1

    log.info("validating session at %s", ubs.LOGIN_ENTRY_URL)
    with sync_playwright() as pw:
        browser = pw.chromium.launch(
            headless=True,
            args=launch.chromium_args("--no-sandbox", "--disable-dev-shm-usage"),
        )
        context = browser.new_context(
            storage_state=str(state_path), user_agent=USER_AGENT,
        )
        if trace:
            # snapshots=False on the sign-in paths: a trace's DOM
            # snapshots record every input's value — here the contract
            # number typed into the login form — and nothing can redact a
            # trace after the fact. The per-action screenshots still show
            # the flow, and the network records are the redacted ones.
            context.tracing.start(screenshots=True, snapshots=False,
                                  sources=True)
        page = context.new_page()
        page.set_default_navigation_timeout(NAV_TIMEOUT_MS)
        try:
            page.goto(ubs.LOGIN_ENTRY_URL, wait_until="domcontentloaded")
            # Give the SPA a moment to either route us into the
            # workbench (cookie valid) or render the login dialog
            # (cookie dead). Wait on either side's anchor selector
            # rather than networkidle.
            try:
                page.wait_for_function(
                    "() => /\\/app\\/.*\\/ebanking\\/spa\\.html/.test(location.href) || "
                    "      /[?&]navitemid=/.test(location.href) || "
                    "      !!document.querySelector('input[name=\"loginalias\"]')",
                    timeout=LANDMARK_TIMEOUT_MS,
                )
            except PWTimeout:
                pass
            url = page.url
            maybe_screenshot(page, screenshot_dir, "check-final")
            if ubs.is_post_auth_url(url):
                log.info("session OK: %s", url)
                rc = 0
            else:
                log.error("session DEAD: landed at %s — run login.py (no --check)", url)
                rc = 2
        except PWTimeout as e:
            maybe_screenshot(page, screenshot_dir, "check-timeout")
            log.error("timeout during --check: %s", e)
            rc = 3
        finally:
            if trace:
                if screenshot_dir is None:
                    log.warning("--trace without --screenshot-dir; trace discarded")
                else:
                    screenshot_dir.mkdir(parents=True, exist_ok=True)
                    trace_path = screenshot_dir / f"{bronze.ts_slug()}-check-trace.zip"
                    context.tracing.stop(path=str(trace_path))
                    log.info("trace saved to %s", trace_path)
            context.close()
            browser.close()
        return rc


def run_login(contract_number: str, state_path: Path, mfa_timeout: int,
              qr_png: Path | None, terminal_qr: bool,
              screenshot_dir: Path | None, trace: bool) -> int:
    """Mint a fresh session via contract + Access App QR challenge."""
    from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

    log.info("opening UBS login")
    with sync_playwright() as pw:
        browser = pw.chromium.launch(
            headless=True,
            args=launch.chromium_args("--no-sandbox", "--disable-dev-shm-usage"),
        )
        context = browser.new_context(user_agent=USER_AGENT)
        if trace:
            # snapshots=False on the sign-in paths: a trace's DOM
            # snapshots record every input's value — here the contract
            # number typed into the login form — and nothing can redact a
            # trace after the fact. The per-action screenshots still show
            # the flow, and the network records are the redacted ones.
            context.tracing.start(screenshots=True, snapshots=False,
                                  sources=True)
        page = context.new_page()
        page.set_default_navigation_timeout(NAV_TIMEOUT_MS)

        try:
            # ---------- Stage 1: contract-number entry ----------
            page.goto(ubs.LOGIN_ENTRY_URL, wait_until="domcontentloaded")

            # Wait on the contract input rather than networkidle —
            # UBS's SPA keeps long-lived analytics + fingerprinting
            # requests open and `networkidle` rarely fires within
            # 30s. The input being visible is a stronger signal
            # that Stage 1 finished rendering.
            contract_input = page.locator(f'input[name="{ubs.CONTRACT_INPUT_NAME}"]').first
            contract_input.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)

            template = _read_initial_props_template(page)
            log.debug("stage-1 template: %s", template)
            if template != ubs.TEMPLATE_CONTRACT_NR:
                maybe_screenshot(page, screenshot_dir, "stage1-unexpected")
                _fail_on_unexpected_template(template)
            contract_input.fill(contract_number)
            maybe_screenshot(page, screenshot_dir, "stage1-filled")

            # Submit the form. The contract-entry card has exactly
            # one primary button; click whichever is visible. Falling
            # back to Enter is unreliable: in some flows it submits
            # cleanly, in others it leaves us on a "Login starten"
            # interstitial (see Stage 1b below).
            _click_primary_button(page, contract_input)

            # ---------- Stage 1b: optional "Login starten" interstitial ----------
            # UBS may render a confirmation card between contract
            # entry and the QR challenge. We resolve by waiting for
            # whichever shows up first: the QR image (no interstitial)
            # or the QR card title (interstitial present). If we land
            # on the interstitial, click its primary button and loop
            # back to the same wait.
            qr_locator = page.locator(f'[data-testid="{ubs.QR_IMG_TESTID}"]')
            _advance_through_access_app_interstitial(page, qr_locator)

            # ---------- Stage 2: QR challenge ----------
            # Wait for the QR <img> to be rendered with a real
            # base64 data URL (not the empty placeholder).
            qr_locator.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
            template = _read_initial_props_template(page)
            log.debug("stage-2 template: %s", template)
            if template != ubs.TEMPLATE_QR:
                maybe_screenshot(page, screenshot_dir, "stage2-unexpected")
                _fail_on_unexpected_template(template)

            # Wait until the SPA has populated the data URL. The
            # initial render has src="" or a placeholder; the
            # first /check_status poll fills it.
            _wait_for_qr_data_url(page, qr_locator)
            maybe_screenshot(page, screenshot_dir, "stage2-qr-shown")

            print(file=sys.stdout)
            print(f"UBS Access App login challenge — contract {contract_number[:2]}…",
                  file=sys.stdout)
            print("Open UBS Access App on your phone and scan the QR below.",
                  file=sys.stdout)
            print(f"Waiting up to {mfa_timeout}s for approval.",
                  file=sys.stdout)
            sys.stdout.flush()

            last_data_url = _render_qr_from_locator(
                page, qr_locator, qr_png, terminal_qr,
            )

            # ---------- Stage 3: wait for approval ----------
            deadline = time.monotonic() + mfa_timeout
            last_seen_url = ""
            while time.monotonic() < deadline:
                url = page.url
                if url != last_seen_url:
                    log.debug("page url: %s", url)
                    last_seen_url = url
                if ubs.is_post_auth_url(url):
                    log.info("login completed: %s", url)
                    break
                # Watch for QR rotation. The SPA replaces the img
                # src in place on REFRESH_CONTINUE.
                try:
                    current = qr_locator.get_attribute("src", timeout=1000) or ""
                except PWTimeout:
                    current = ""
                if current and current != last_data_url and current.startswith("data:image/"):
                    log.info("QR rotated; re-rendering")
                    last_data_url = _render_qr_from_locator(
                        page, qr_locator, qr_png, terminal_qr,
                    )
                # Short sleep — match the SPA's 2s cadence.
                page.wait_for_timeout(ubs.QR_POLL_INTERVAL_SECONDS * 1000)
            else:
                maybe_screenshot(page, screenshot_dir, "stage3-timeout")
                log.error("MFA timeout after %ds without approval", mfa_timeout)
                return 4

            # ---------- Stage 4: confirm + persist ----------
            # Don't wait_for networkidle here either — the workbench
            # loads many analytics requests and they're noise.
            if not ubs.is_post_auth_url(page.url):
                maybe_screenshot(page, screenshot_dir, "stage4-bad-landing")
                log.error("expected post-auth URL, got %s", page.url)
                return 5

            maybe_screenshot(page, screenshot_dir, "stage4-workbench")
            state_path.parent.mkdir(parents=True, exist_ok=True)
            context.storage_state(path=str(state_path))
            session.secure_file(state_path)
            log.info("session state written to %s (chmod 0600)", state_path)
            return 0

        except PWTimeout as e:
            maybe_screenshot(page, screenshot_dir, "timeout")
            log.error("playwright timeout: %s", e)
            return 6
        finally:
            if trace:
                if screenshot_dir is None:
                    log.warning("--trace without --screenshot-dir; trace discarded")
                else:
                    screenshot_dir.mkdir(parents=True, exist_ok=True)
                    trace_path = screenshot_dir / f"{bronze.ts_slug()}-login-trace.zip"
                    context.tracing.stop(path=str(trace_path))
                    log.info("trace saved to %s", trace_path)
            context.close()
            browser.close()


def _click_primary_button(page, focus_fallback) -> None:
    """Click the visible primary submit button on a UBS auth card.

    UBS labels are locale-dependent ("Weiter" / "Continue" /
    "Login starten" / ...). We locate by being a `<button
    type="submit">` inside the auth form; if there are multiple, we
    pick the first visible one. If none is found, fall back to
    pressing Enter on `focus_fallback`.
    """
    candidates = page.locator('form button[type="submit"]:visible')
    n = candidates.count()
    if n >= 1:
        candidates.first.click()
        return
    # Plain submit-typed inputs are not used in this UI but handle
    # them defensively.
    inputs = page.locator('form input[type="submit"]:visible')
    if inputs.count() >= 1:
        inputs.first.click()
        return
    focus_fallback.press("Enter")


def _advance_through_access_app_interstitial(page, qr_locator,
                                             max_clicks: int = 2) -> None:
    """If a confirm-Access-App page sits between Stage 1 and the QR,
    click its primary button. Idempotent — if the QR is already
    visible, returns immediately.
    """
    for _ in range(max_clicks):
        # Wait for the page to settle on one of the two known
        # screens: the QR (success) or the confirm interstitial.
        try:
            page.wait_for_function(
                "() => !!document.querySelector("
                "       '[data-testid=\"qr-scanner-image\"]') || "
                "      !!Array.from(document.querySelectorAll("
                "         'form button[type=\"submit\"]'))"
                "         .find(b => b.offsetParent !== null)",
                timeout=LANDMARK_TIMEOUT_MS,
            )
        except Exception:  # noqa: BLE001 — re-raise via the wait below
            return
        if qr_locator.count() > 0:
            return
        # Still on a card with a submit button — assume confirm
        # interstitial and click through.
        log.debug("advancing through Access-App confirm interstitial")
        _click_primary_button(page, qr_locator)


def _read_initial_props_template(page) -> str | None:
    """Return `window.initialProps.template` or None if unavailable.

    The Nevis-rendered SPA bootstraps a `var initialProps = {...}`
    in inline <script>. We just read it back from the page context.
    """
    try:
        return page.evaluate(
            "() => (window.initialProps && window.initialProps.template) || null"
        )
    except Exception as e:  # noqa: BLE001 — log + return None
        log.debug("could not read initialProps.template: %s", e)
        return None


def _fail_on_unexpected_template(template: str | None) -> None:
    if template in ubs.UNEXPECTED_TEMPLATES:
        raise SystemExit(
            f"UBS routed us to {template!r}, which this script does "
            f"not handle. Complete the step once in a regular browser "
            f"and re-run login.py."
        )
    raise SystemExit(
        f"Unexpected UBS template {template!r} (expected one of "
        f"{ubs.TEMPLATE_CONTRACT_NR}, {ubs.TEMPLATE_QR}). UBS may "
        f"have changed the login flow; capture --screenshot-dir + "
        f"--trace and report."
    )


def _wait_for_qr_data_url(page, locator, timeout_s: int = 30) -> None:
    """Poll the img element until its `src` is a real data URL."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        src = locator.get_attribute("src") or ""
        if src.startswith("data:image/") and "base64," in src:
            return
        page.wait_for_timeout(500)
    raise SystemExit(
        "Timed out waiting for the QR image to render. The SPA "
        "may have failed to fetch /check_status; try --screenshot-dir."
    )


def _render_qr_from_locator(page, locator, qr_png: Path | None,
                            terminal_qr: bool) -> str:
    """Read the img data URL, decode, render to terminal + write PNG.

    Returns the data URL string so the caller can spot rotations.
    """
    data_url = locator.get_attribute("src") or ""
    png_bytes = decode_data_url_png(data_url)
    if qr_png is not None:
        qr_png.parent.mkdir(parents=True, exist_ok=True)
        # UBS renders the QR at 150x150 px, which Preview/QuickLook
        # shows ~2 cm wide on a retina display — too small for the
        # Access App camera to resolve modules. Upscale 6x with
        # nearest-neighbour so modules stay crisp.
        scaled = _upscale_qr_png(png_bytes, factor=6)
        qr_png.write_bytes(scaled)
        log.info("QR PNG written to %s (%d bytes, upscaled %dx)",
                 qr_png, len(scaled), 6)
    if terminal_qr:
        render_qr_to_terminal(png_bytes)
    return data_url


def _upscale_qr_png(png_bytes: bytes, factor: int) -> bytes:
    """Nearest-neighbour upscale of a QR PNG. Falls back to the raw
    bytes if Pillow can't be imported (shouldn't happen — render_qr_
    to_terminal already needs Pillow), so this is best-effort."""
    try:
        from PIL import Image
    except ImportError:
        return png_bytes
    img = Image.open(io.BytesIO(png_bytes))
    w, h = img.size
    big = img.resize((w * factor, h * factor), Image.NEAREST)
    buf = io.BytesIO()
    big.save(buf, format="PNG")
    return buf.getvalue()


# ============================================================
# Main
# ============================================================

def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    # `--no-terminal-qr` is only sensible if we still have a way to
    # see the QR, i.e. via `--qr-png`. Enforce that pairing.
    if args.no_terminal_qr and args.qr_png is None and not args.check:
        raise SystemExit(
            "--no-terminal-qr requires --qr-png (otherwise there's "
            "nothing for the Access App to scan)."
        )

    # `--trace` is a paired flag — refuse to silently drop the
    # trace if --screenshot-dir is missing (AGENTS.md §3).
    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir.")

    if args.check:
        return run_check(
            state_path=session.resolve_state_path(
                args.state_path, DEFAULT_STATE_PATH, LEGACY_STATE_PATH),
            screenshot_dir=args.screenshot_dir,
            trace=args.trace,
        )

    contract_number = resolve_contract_number(args)
    return run_login(
        contract_number=contract_number,
        state_path=args.state_path,
        mfa_timeout=args.mfa_timeout,
        qr_png=args.qr_png,
        terminal_qr=not args.no_terminal_qr,
        screenshot_dir=args.screenshot_dir,
        trace=args.trace,
    )


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
