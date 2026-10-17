#!/usr/bin/env python3
"""Interactive 2FA dialog for the Chase step-up challenge — the CLI core
`login.py` drives.

Chase's sign-in fires a step-up challenge (`challenge-options` → invoke →
verify/poll). This module is the **browserless, side-effect-free** part of
driving it: parse the `challenge-options` body, let a human pick a factor
and, for SMS or voice, a phone, and read the one-time code. It touches no
network and no browser, so it is unit-tested on its own and driven from a
`--demo` that replays the dialog against synthetic data. login.py clicks the
matching MDS controls, and the page's own JS makes the challenge calls.

The `challenge-options` body carries (DESIGN.md "Source behaviour" §A holds
the whole wire contract):

  - `challengeMethodsDisplay.showAll`, the offered factors;
  - `phoneList`, the SMS/voice destinations, each a
    `contactReferenceIdentifier` + a masked `last4DgtsPhoneNumber`;
  - `inAppDeviceList`, the push targets, each a `oneTimeDeviceIdentifier` +
    `digitalDeviceName`;
  - `challengeTokenIdentifier`, which threads the flow.

SMS and voice complete with a typed 8-digit code. Push completes on the
phone, with no code to type. The SMS invocation also returns a 3-char
`oneTimePasswordPrefixText`, which the page echoes back itself; it is absent
from the message the code arrives in, so it is never shown (`read_otp`).
`CALL_US` is a human phone call and is never offered.
"""
from __future__ import annotations

import argparse
import sys
from dataclasses import dataclass
from collections.abc import Callable

from collectorkit.cli import ChallengeError, choose_one, read_code

# Factor codes as they appear in challengeMethodsDisplay.showAll.
FACTOR_INAPP = "INAPP"          # push to a registered mobile app; poll to complete
FACTOR_OTP_SMS = "OTP_SMS"      # one-time code by text
FACTOR_OTP_VOICE = "OTP_VOICE"  # one-time code read out by phone call
FACTOR_CALL_US = "CALL_US"      # phone a representative — not automatable

# Human labels for the factor menu.
FACTOR_LABELS = {
    FACTOR_INAPP: "Approve a notification in the Chase app",
    FACTOR_OTP_SMS: "Text me a code",
    FACTOR_OTP_VOICE: "Call me with a code",
    FACTOR_CALL_US: "Call Chase (cannot be automated)",
}

# Chase's SMS one-time code length. Kept as a soft check (warn, do
# not reject) so a policy change in code length can't wedge a real login.
OTP_CODE_LEN = 8

# The factors this dialog can actually drive today. CALL_US is a dead end
# (human phone call); the rest map to a captured completion path.
SUPPORTED_FACTORS = (FACTOR_INAPP, FACTOR_OTP_SMS, FACTOR_OTP_VOICE)


@dataclass(frozen=True)
class Target:
    """One challenge destination — a phone (SMS/voice) or an app device
    (push). `contact_id` is the `contactReferenceIdentifier` the invocation
    carries; `label` is what the human sees when picking (a masked phone
    number, or a device name)."""
    contact_id: str
    label: str


@dataclass(frozen=True)
class ChallengeMenu:
    """The parsed `challenge-options` body, reduced to what the dialog needs."""
    token: str
    factors: tuple[str, ...]              # showAll, order preserved
    phones: tuple[Target, ...] = ()       # SMS / voice destinations
    devices: tuple[Target, ...] = ()      # in-app push targets

    def offerable(self) -> list[str]:
        """Factors we can both drive AND have the inputs for: SMS/voice need
        at least one phone; in-app needs a registered device. Order follows
        Chase's own `showAll`."""
        out = []
        for f in self.factors:
            if f not in SUPPORTED_FACTORS:
                continue
            if f in (FACTOR_OTP_SMS, FACTOR_OTP_VOICE) and not self.phones:
                continue
            if f == FACTOR_INAPP and not self.devices:
                continue
            out.append(f)
        return out


def parse_challenge_options(body: dict) -> ChallengeMenu:
    """Reduce a raw `challenge-options` JSON body to a `ChallengeMenu`.

    Tolerant of missing/renamed fields — a factor whose backing list is
    absent simply won't be offerable. Never raises on shape; a body with no
    token yields an empty token that the caller can treat as a hard error.
    (phoneList also carries `smsEnabledIndicator`; it is not read here — the
    picker shows every number and lets Chase reject a voice-only one, which
    surfaces as Chase's own error, per the fail-fast auth convention.)"""
    show_all = tuple(
        (body.get("challengeMethodsDisplay") or {}).get("showAll") or ()
    )
    phones = tuple(
        Target(contact_id=str(p.get("contactReferenceIdentifier", "")),
               label=str(p.get("last4DgtsPhoneNumber", "")))
        for p in (body.get("phoneList") or ())
    )
    devices = tuple(
        Target(contact_id=str(d.get("oneTimeDeviceIdentifier", "")),
               label=str(d.get("digitalDeviceName", "")).strip())
        for d in (body.get("inAppDeviceList") or ())
    )
    return ChallengeMenu(
        token=str(body.get("challengeTokenIdentifier", "")),
        factors=show_all,
        phones=phones,
        devices=devices,
    )


def choose_factor(menu: ChallengeMenu, *,
                  input_fn: Callable[[str], str] = input,
                  output_fn: Callable[[str], None] = print) -> str:
    """Present the offerable factors and return the chosen factor code.

    A single offerable factor is chosen automatically (announced, not
    prompted). Raises ChallengeError when none can be driven — e.g. Chase
    offered only CALL_US."""
    offerable = menu.offerable()
    if not offerable:
        raise ChallengeError(
            "no automatable 2FA factor offered "
            f"(Chase showed: {', '.join(menu.factors) or 'nothing'})"
        )
    return choose_one(
        offerable, FACTOR_LABELS.__getitem__,
        heading="Chase needs a second factor. Choose how to receive it:",
        single="2FA:", input_fn=input_fn, output_fn=output_fn)


def choose_target(targets: tuple[Target, ...], title: str, *,
                  input_fn: Callable[[str], str] = input,
                  output_fn: Callable[[str], None] = print) -> Target:
    """Present destinations by their labels and return the chosen one — the
    dialog Chase shows when several are on file (e.g. two phone numbers). A
    single destination is chosen automatically. Raises ChallengeError when
    the list is empty."""
    if not targets:
        raise ChallengeError("no destination available for the chosen factor")
    return choose_one(targets, lambda t: t.label, heading=title, single=title,
                      input_fn=input_fn, output_fn=output_fn)


def choose_phone(menu: ChallengeMenu, *,
                 input_fn: Callable[[str], str] = input,
                 output_fn: Callable[[str], None] = print) -> Target:
    """Pick the phone number Chase texts the code to, when more than one is on
    file."""
    return choose_target(menu.phones,
                         "Which number should Chase send the code to?",
                         input_fn=input_fn, output_fn=output_fn)


def read_otp(*,
             input_fn: Callable[[str], str] = input,
             output_fn: Callable[[str], None] = print,
             max_attempts: int = 3) -> str:
    """Read the one-time code from stdin.

    Chase texts/reads out an all-digit code, so the prompt asks for exactly
    that — no anti-phishing prefix is shown (the `oneTimePasswordPrefixText`
    the API returns does not appear in the message the code arrives in). A
    code of unexpected length is warned about (Chase uses 8 digits) but
    still returned — Chase's verify call makes the final ruling, so a
    length-policy change can't wedge login."""
    return read_code(digits=OTP_CODE_LEN, input_fn=input_fn,
                     output_fn=output_fn, max_attempts=max_attempts)


# --- demo -----------------------------------------------------------------

# Synthetic multi-destination menu mirroring the payload shape — placeholder
# ids and masks only (never a real contact id or phone fragment).
_DEMO_BODY = {
    "challengeTokenIdentifier": "DEMO-TOKEN",
    "challengeMethodsDisplay": {
        "showAll": [FACTOR_INAPP, FACTOR_OTP_SMS, FACTOR_OTP_VOICE, FACTOR_CALL_US],
    },
    "phoneList": [
        {"contactReferenceIdentifier": "1001", "last4DgtsPhoneNumber": "xxx-xxx-1111"},
        {"contactReferenceIdentifier": "1002", "last4DgtsPhoneNumber": "xxx-xxx-2222"},
    ],
    "inAppDeviceList": [
        {"oneTimeDeviceIdentifier": "2001", "digitalDeviceName": "Example Phone"},
    ],
}

def _demo() -> int:
    """Replay the dialog against synthetic data — no network, no browser.
    Shows the factor menu, the phone picker (the multi-destination case),
    and the OTP prompt exactly as a real login would present them, and what
    login.py then does on the page."""
    menu = parse_challenge_options(_DEMO_BODY)
    print("── Chase 2FA dialog (demo; synthetic data, nothing is sent) ──")
    try:
        factor = choose_factor(menu)
        print("→ login.py would select that method on the page, then click "
              "Next")
        if factor == FACTOR_INAPP:
            input("Approve it in the app, then press Enter… ")
            print("→ login.py would wait for the sign-in to complete.")
            return 0
        phone = choose_phone(menu)
        print(f"→ login.py would select {phone.label!r}, then click Next")
        read_otp()
        print("→ login.py would type the code into the secure field and "
              "submit it")
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
