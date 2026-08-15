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


class ChallengeError(RuntimeError):
    """The challenge cannot be driven from here (no usable target, or the
    human gave up / provided no usable input)."""


def _read_choice(prompt: str, n: int, *, input_fn: Callable[[str], str],
                 output_fn: Callable[[str], None], max_attempts: int = 3) -> int:
    """Read a 1..n menu choice, re-prompting on junk. Returns the 0-based
    index. Raises ChallengeError after `max_attempts` bad entries or on EOF
    (a piped, non-interactive stdin). No implicit default — a login
    destination is always chosen explicitly."""
    for _ in range(max_attempts):
        try:
            raw = input_fn(prompt).strip()
        except EOFError:
            raise ChallengeError("no input on stdin for the 2FA prompt")
        if raw.isdigit() and 1 <= int(raw) <= n:
            return int(raw) - 1
        output_fn(f"  Please enter a number between 1 and {n}.")
    raise ChallengeError(f"no valid selection after {max_attempts} attempts")


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
    if len(usable) == 1:
        output_fn(f"2FA: {usable[0].display or KIND_LABELS.get(usable[0].kind)}")
        return usable[0]
    output_fn("First Citizens needs a second factor. Choose how to receive it:")
    for i, t in enumerate(usable, 1):
        label = t.display or KIND_LABELS.get(t.kind, KIND_LABELS["other"])
        output_fn(f"  {i}. {label}")
    idx = _read_choice(f"Enter 1-{len(usable)}: ", len(usable),
                       input_fn=input_fn, output_fn=output_fn)
    return usable[idx]


def read_otp(*,
             input_fn: Callable[[str], str] = input,
             output_fn: Callable[[str], None] = print,
             max_attempts: int = 3) -> str:
    """Read the one-time code from stdin.

    First Citizens texts / reads out an all-digit code. Spaces and dashes in
    the entry are stripped; only empty / non-numeric input is rejected and
    re-prompted. No length is asserted (the validate call is the final
    ruling). Never logs the code. Raises ChallengeError on repeated
    empty/invalid input or EOF."""
    for _ in range(max_attempts):
        try:
            raw = input_fn("Code: ")
        except EOFError:
            raise ChallengeError("no input on stdin for the OTP code")
        code = raw.replace(" ", "").replace("-", "").strip()
        if not code.isdigit():
            output_fn("  The code is all digits — try again.")
            continue
        return code
    raise ChallengeError(f"no valid code after {max_attempts} attempts")


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
