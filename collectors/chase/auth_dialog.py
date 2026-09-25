#!/usr/bin/env python3
"""Interactive 2FA dialog for the Chase step-up challenge — the CLI core
`login.py` drives.

Chase's sign-in fires a step-up challenge (`challenge-options` → invoke →
verify/poll). This module is the **browserless, side-effect-free** part of
driving it: parse the `challenge-options` body, let a human pick a factor
and a destination (which phone for SMS, which device for push), read the
one-time code, and build the exact request payloads. It touches no network
and no browser, so it is unit-tested on its own and driven from a `--demo`
that replays the dialog against synthetic data.

The wire contract is what the 2026-08-11 explore captures showed
(DESIGN.md "Observed" §A):

  - `challenge-options` returns `challengeMethodsDisplay.showAll` (the
    offered factors), `phoneList` (SMS/voice destinations, each a
    `contactReferenceIdentifier` + a masked `last4DgtsPhoneNumber`),
    `inAppDeviceList` (push targets, each a `oneTimeDeviceIdentifier` +
    `digitalDeviceName`), and a `challengeTokenIdentifier` threading the
    flow.
  - `challenge-invocations` (POST) sends the challenge. Both factors share
    one body shape — `{challengeTokenIdentifier, communicationMethodTypeCode,
    contactReferenceIdentifier}` — differing only in the method code and
    which list the contact id comes from: **push** is `"I"` + a device id,
    **SMS** is `"S"` + a phone id.
  - **SMS** then returns a 3-char `oneTimePasswordPrefixText` — echoed
    back on the verification, but absent from the message the code
    arrives in, so it is never shown (`read_otp`) — and the typed 8-digit
    code goes to `challenge-verifications`.
  - **Push** instead completes by polling `challenge-statuses` — no code to
    type — so it needs only its "approve on your device, then continue"
    prompt here.

The method codes are all observed (2026-08-11 captures): push `"I"`, SMS
`"S"`, voice `"V"`, and the 8-digit code length. Voice behaves as SMS does
— same prefixed invocation, same `challenge-verifications` completion — so
the SMS interaction path drives it unchanged. `CALL_US` is a human phone
call and is never offered.
"""
from __future__ import annotations

import argparse
import sys
from dataclasses import dataclass
from typing import Callable

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

# communicationMethodTypeCode per factor, all observed (2026-08-11
# captures): push "I", SMS "S", voice "V". Voice behaves exactly like SMS
# — its invocation returns the same 3-char prefix and completes through
# `challenge-verifications` — differing only in this code and that the code
# is read out over a phone call rather than texted.
METHOD_CODE = {
    FACTOR_INAPP: "I",
    FACTOR_OTP_SMS: "S",
    FACTOR_OTP_VOICE: "V",
}

# Observed Chase SMS one-time code length. Kept as a soft check (warn, do
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

    def targets_for(self, factor: str) -> tuple[Target, ...]:
        """The destinations a factor picks among: devices for push, phones
        for SMS/voice."""
        return self.devices if factor == FACTOR_INAPP else self.phones


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


def build_invocation(menu: ChallengeMenu, factor: str, target: Target) -> dict:
    """The `challenge-invocations` POST body that sends the code / push.

    Both push and SMS share the shape `{challengeTokenIdentifier,
    communicationMethodTypeCode, contactReferenceIdentifier}`; the method
    code and which list the target came from are the only difference.
    Raises ChallengeError for a factor with no confirmed method code —
    `CALL_US` (a human phone call), or one Chase adds later."""
    method = METHOD_CODE.get(factor)
    if method is None:
        raise ChallengeError(
            f"no confirmed communicationMethodTypeCode for {factor} yet — "
            "capture a real trace before enabling it"
        )
    return {
        "challengeTokenIdentifier": menu.token,
        "communicationMethodTypeCode": method,
        "contactReferenceIdentifier": target.contact_id,
    }


def build_verification(menu: ChallengeMenu, factor: str, target: Target,
                       prefix: str, code: str) -> dict:
    """The `challenge-verifications` POST body that submits the typed code,
    mirroring the fields the capture showed (the prefix is echoed back)."""
    return {
        "challengeTokenIdentifier": menu.token,
        "communicationMethodTypeCode": METHOD_CODE.get(factor),
        "contactReferenceIdentifier": target.contact_id,
        "otp": {
            "oneTimeUserPasswordText": code,
            "oneTimePasswordPrefixText": prefix,
        },
    }


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

# Stands in for the `oneTimePasswordPrefixText` a real invocation response
# carries — the demo sends nothing, so there is no response to read it from.
DEMO_OTP_PREFIX = "ABC"


def _demo() -> int:
    """Replay the dialog against synthetic data — no network, no browser.
    Shows the factor menu, the phone picker (the multi-destination case),
    and the OTP prompt exactly as a real login would present them."""
    menu = parse_challenge_options(_DEMO_BODY)
    print("── Chase 2FA dialog (demo; synthetic data, nothing is sent) ──")
    try:
        factor = choose_factor(menu)
        if factor == FACTOR_INAPP:
            device = choose_target(menu.devices, "Approve the sign-in on:",
                                   input_fn=input, output_fn=print)
            print("→ would POST challenge-invocations:",
                  build_invocation(menu, factor, device))
            input("Approve it in the app, then press Enter… ")
            print("→ would poll challenge-statuses until approved.")
            return 0
        phone = choose_phone(menu)
        print("→ would POST challenge-invocations:",
              build_invocation(menu, factor, phone))
        # The prompt never shows the prefix (it is absent from the message
        # the code arrives in), so `read_otp` does not take one. A real run
        # reads it off the invocation response to echo back on the
        # verification; the demo uses a fixed placeholder.
        code = read_otp()
        payload = build_verification(menu, factor, phone, DEMO_OTP_PREFIX, code)
        # Redact the code in the echo — it is never printed back.
        shown = {**payload, "otp": {**payload["otp"],
                                    "oneTimeUserPasswordText": "<redacted>"}}
        print("→ would POST challenge-verifications:", shown)
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
