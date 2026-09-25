#!/usr/bin/env python3
"""Interactive 2FA dialog for the First Citizens (Q2) access-code
challenge — the CLI core `login.py` wraps.

When a device is not yet trusted, the SPA presents a Secure Access Code
(2FA) challenge. This module is the **browserless, side-effect-free** part
of driving it: present the delivery methods (text / call), let a human pick
one, and read the one-time code from stdin. It touches no network and no
browser, so it is unit-tested on its own and driven from a `--demo` that
replays the dialog against synthetic data.

login.py drives the challenge itself over the DOM (DESIGN.md §4.2) — it
reads the delivery methods off the on-screen `btnTacTarget` buttons, clicks
the one this dialog picks, types the code this dialog reads into `#tacEntry`,
and submits — so the accessCode POSTs are made by the SPA's own JS, not
here. This module stays purely the human dialog; a delivery target is the
`q2client.AccessCodeTarget` shape (`value` = the button index login.py
clicks, `display` = the button label, `kind` = sms/voice).
"""
from __future__ import annotations

import argparse
import sys
from typing import Callable

from collectorkit.cli import ChallengeError, choose_one, read_code
from q2client import AccessCodeTarget

# Human labels per target kind, for the picker.
KIND_LABELS = {
    "sms": "Text me a code",
    "voice": "Call me with a code",
    "other": "Send me a code",
}

# Observed one-time-code length is not pinned (the code never reached this
# dialog in the read-only captures), so no length is asserted — the server's
# validate call is the final ruling, and asserting a wrong length would wedge
# a real login. Only empty / non-numeric input is rejected.


def choose_target(targets: tuple[AccessCodeTarget, ...], *,
                  input_fn: Callable[[str], str] = input,
                  output_fn: Callable[[str], None] = print) -> AccessCodeTarget:
    """Present the offered access-code targets and return the chosen one.

    A single target is chosen automatically (announced, not prompted). Each
    line shows the target's own masked `display` (e.g. "Text: (XXX)
    XXX-XXXX"), which already tells text from call. Raises ChallengeError on
    an empty list."""
    usable = tuple(t for t in targets if t.value)
    if not usable:
        raise ChallengeError("no usable 2FA target offered")
    return choose_one(
        usable, lambda t: t.display or KIND_LABELS.get(t.kind, KIND_LABELS["other"]),
        heading="First Citizens needs a second factor. "
                "Choose how to receive it:",
        single="2FA:", input_fn=input_fn, output_fn=output_fn)


def read_otp(*,
             input_fn: Callable[[str], str] = input,
             output_fn: Callable[[str], None] = print,
             max_attempts: int = 3) -> str:
    """Read the one-time code from stdin. No length is asserted: the code
    never reached the read-only captures, and the validate call is the
    final ruling."""
    return read_code(input_fn=input_fn, output_fn=output_fn,
                     max_attempts=max_attempts)


# --- demo -----------------------------------------------------------------

# Synthetic two-target menu mirroring the payload shape — placeholder ids and
# masks only (never a real delivery id or phone fragment).
_DEMO_TARGETS = (
    AccessCodeTarget(value="1001", display="Text: (XXX) XXX-XXXX", kind="sms"),
    AccessCodeTarget(value="1002", display="Call: (XXX) XXX-XXXX", kind="voice"),
)


def _demo() -> int:
    """Replay the dialog against synthetic data — no network, no browser.
    Shows the delivery picker and the OTP prompt exactly as a real login
    would; login.py then clicks the matching button and types the code."""
    print("── First Citizens 2FA dialog (demo; synthetic data, nothing is sent) ──")
    try:
        target = choose_target(_DEMO_TARGETS)
        print(f"→ login.py would click the {target.display!r} delivery button")
        read_otp()
        print("→ login.py would type the code into #tacEntry and submit")
        return 0
    except ChallengeError as exc:
        print(f"dialog ended: {exc}", file=sys.stderr)
        return 1


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--demo", action="store_true",
                   help="Replay the 2FA dialog against synthetic data.")
    args = p.parse_args(argv)
    if args.demo:
        return _demo()
    p.print_help()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
