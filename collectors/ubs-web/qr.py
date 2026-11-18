"""The QR challenge's image rules: decode the data URL the login page
shows, upscale the PNG, and render it on the terminal in half-block
characters. The login flow in `login.py` reads the image off the page.
"""

from __future__ import annotations

import base64
import io
import logging
import sys

# The login's own logger: a record reads the same whichever module
# writes it.
log = logging.getLogger("ubs-web.login")


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

    Pillow is used for the decoding and pixel access; the QR's payload
    is deliberately NOT decoded — re-rendering the bitmap as-is
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
    5-module dark centre. Rows are scanned from the top until one is
    found whose first dark pixel begins a long horizontal dark run;
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


# UBS renders the QR at 150x150 px, which Preview/QuickLook shows ~2 cm
# wide on a retina display — too small for the Access App camera to
# resolve modules. The PNG is upscaled this many times with
# nearest-neighbour so modules stay crisp.
QR_PNG_SCALE = 6


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
