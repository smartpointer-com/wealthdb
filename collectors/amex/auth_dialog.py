#!/usr/bin/env python3
"""Interactive dialog for the American Express one-time-passcode challenge
— the CLI core `login.py` wraps.

When a device is not trusted, the sign-in presents a passcode challenge: a
list of delivery options, then a six-box code entry (DESIGN.md §B). This
module is the **browserless, side-effect-free** part of driving it: present
the delivery options, let a human pick one, and read the code from stdin. It
touches no network and no browser, so it is unit tested on its own and
driven from a `--demo` that replays the dialog against synthetic data.

login.py drives the challenge itself over the DOM — it reads the options off
the on-screen `challenge-options-list` buttons, clicks the one this dialog
picks, types the code this dialog reads into the six `otp-input-*` boxes,
and submits — so the passcode calls are made by the page's own JS, not here.
A delivery option is the `amexclient.ChallengeTarget` shape (`value` = the
button index login.py clicks, `display` = its label, `kind` = a coarse
sms/email/voice read from that label).
"""
from __future__ import annotations

import argparse
import sys
from typing import Callable

from amexclient import OTP_DIGITS, ChallengeTarget
from collectorkit.cli import ChallengeError, choose_one, read_code

# Fallback wording when an option's own label is empty. The label normally
# carries the masked destination and is preferred.
KIND_LABELS = {
    "sms": "Text me a code",
    "email": "Email me a code",
    "voice": "Call me with a code",
    "other": "Send me a code",
}


def choose_target(targets: tuple[ChallengeTarget, ...], *,
                  input_fn: Callable[[str], str] = input,
                  output_fn: Callable[[str], None] = print) -> ChallengeTarget:
    """Present the offered delivery options and return the chosen one.

    A single option is chosen automatically (announced, not prompted). Each
    line shows the option's own label, which already carries the masked
    destination. Raises ChallengeError on an empty list."""
    usable = tuple(t for t in targets if t.value)
    if not usable:
        raise ChallengeError("no usable passcode delivery option offered")
    return choose_one(
        usable, lambda t: t.display or KIND_LABELS.get(t.kind, KIND_LABELS["other"]),
        heading="American Express needs a one-time passcode. "
                "Choose how to receive it:",
        single="2FA:", input_fn=input_fn, output_fn=output_fn)


def read_otp(*,
             input_fn: Callable[[str], str] = input,
             output_fn: Callable[[str], None] = print,
             max_attempts: int = 3) -> str:
    """Read the one-time passcode from stdin.

    The length IS asserted here, unusually for the fleet, because the entry
    control settles it rather than a guess at provider policy: there are
    exactly six single-digit boxes on the page, so a code of another length
    cannot be typed in at all, and catching that here gives a clear message
    instead of a half-filled form and a server-side rejection."""
    return read_code(prompt=f"Code ({OTP_DIGITS} digits): ", digits=OTP_DIGITS,
                     exact=True, input_fn=input_fn, output_fn=output_fn,
                     max_attempts=max_attempts)


# --- demo -----------------------------------------------------------------

# A synthetic two-option menu mirroring the payload shape — placeholder
# masks only, never a real destination.
_DEMO_TARGETS = (
    ChallengeTarget(value="0", display="Text Message  *******0000",
                    kind="sms"),
    ChallengeTarget(value="1", display="Email  e****@example.com",
                    kind="email"),
)


def _demo() -> int:
    """Replay the dialog against synthetic data — no network, no browser.
    Shows the delivery picker and the passcode prompt exactly as a real
    login would; login.py then clicks the matching button and types the
    code."""
    print("── Amex 2FA dialog (demo; synthetic data, "
          "nothing is sent) ──")
    try:
        target = choose_target(_DEMO_TARGETS)
        print(f"→ login.py would click the {target.display!r} option")
        read_otp()
        print(f"→ login.py would type the code into the {OTP_DIGITS} "
              f"otp-input boxes and click Continue")
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
