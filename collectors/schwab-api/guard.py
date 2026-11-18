"""The consent drive's guard, and the error that ends a login flow.

`AdvanceGuard` caps the advance clicks and fails a page that stops
changing; `LoginFlowError` is how it, and every other terminal step of
the flow in `login.py`, stops the run.
"""

from __future__ import annotations

import time


class LoginFlowError(RuntimeError):
    """Terminal login-flow failure: the run stops, reports, and exits
    nonzero. Re-running login is the recovery path — there are no in-run
    retries (see DESIGN.md §3.1, "Failure discipline")."""

    exit_code = 8

    def __init__(self, message: str, *, page_text: str = "",
                 locked: bool = False):
        super().__init__(message)
        self.page_text = page_text
        self.locked = locked


class AdvanceGuard:
    """Budget + progress guard for the consent-drive loop.

    Two invariants (DESIGN.md §3.1, "Failure discipline"): a page that did not
    change since the last advance click is never clicked again, and a
    page that stops changing at all — clicked or not — fails the run
    after STALL_LIMIT_S instead of spinning. CLICK_BUDGET caps total
    advance clicks well above the flow's three known pages."""

    CLICK_BUDGET = 8
    STALL_LIMIT_S = 60.0

    def __init__(self):
        self.clicks = 0
        self._sig = None
        self._sig_since = time.monotonic()
        self._clicked_sig = None

    def observe(self, kind: str, sig: str) -> None:
        """Track the page state; raise once it has stalled too long."""
        now = time.monotonic()
        if sig != self._sig:
            self._sig = sig
            self._sig_since = now
            return
        if now - self._sig_since > self.STALL_LIMIT_S:
            raise LoginFlowError(
                f"page stopped changing for {int(self.STALL_LIMIT_S)}s "
                f"while driving the consent flow (state: {kind}); not "
                f"clicking further — re-run login, or drive it over VNC "
                f"(vnc-login)")

    def may_click(self) -> bool:
        """Whether an advance click is allowed: the page must have
        changed since the last click. Budget exhaustion raises."""
        if self.clicks >= self.CLICK_BUDGET:
            raise LoginFlowError(
                f"advance-click budget ({self.CLICK_BUDGET}) exhausted "
                f"without reaching the callback; not clicking further")
        return self._sig != self._clicked_sig

    def record_click(self) -> None:
        self.clicks += 1
        self._clicked_sig = self._sig
