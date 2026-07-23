"""
URL and DOM landmarks for the Schwab OAuth token-refresh flow driven
through Camoufox.

The OAuth flow lands the browser on Schwab's authorization endpoint
(`api.schwabapi.com/v1/oauth/authorize`), which presents a standalone
login page (login id + password), a 2FA challenge, an account-selection
step, and a final "Allow"/"Accept" consent screen. On consent Schwab
redirects the browser to the registered callback URL
(`https://127.0.0.1:8182/?code=…&session=…`) — nothing listens there,
so the browser shows a connection error, but the URL is what we capture
and hand to schwab-py's token exchange.

The login + 2FA portion mirrors the schwab.com web login that the
`schwab-web` collector drives, so the selector candidates below are
seeded from `schwab-web/landmarks.py`. The consent-step page flow and
headings are live-verified (see the account-link / consent section);
the advance buttons remain a candidate list, and the step can always
be finished by hand over VNC. Centralising the selectors here means
drift is a one-file fix.
"""

from __future__ import annotations

# ============================================================
# OAuth endpoints / callback
# ============================================================

# The registered callback the consent step redirects to. Must match the
# value in the Schwab developer portal exactly; schwab-py builds the
# authorize URL from it. Nothing listens on this port — the browser just
# surfaces the `?code=…` in its address bar, which we scrape.
DEFAULT_CALLBACK_URL = "https://127.0.0.1:8182"

# Host the authorize page is served from (the page we navigate to first).
AUTHORIZE_URL_PREFIX = "https://api.schwabapi.com/v1/oauth/authorize"


def is_callback_url(url: str, callback_url: str) -> bool:
    """True once the browser has navigated to the OAuth callback — i.e.
    consent succeeded and Schwab handed back the `?code=…`. We compare on
    the scheme+host+port prefix because the path/query carry the code."""
    return url.startswith(callback_url)


# ============================================================
# Login form (Schwab authorize page)
# ============================================================

# Best-effort login-id / password input candidates. The first two mirror
# the schwab.com gateway SPA ids that schwab-web uses; the rest are
# generic fallbacks in case the authorize page differs. Pre-fill tries
# each in order, at top level and inside any frame.
LOGIN_ID_INPUT_CANDIDATES = (
    "#loginIdInput",
    "input#loginId",
    "input[name='loginId']",
    "input[name='LoginId']",
    "input[autocomplete='username']",
    "input[type='text'][name*='ogin' i]",
)
PASSWORD_INPUT_CANDIDATES = (
    "#passwordInput",
    "input#password",
    "input[name='password']",
    "input[type='password']",
    "input[autocomplete='current-password']",
)
LOGIN_SUBMIT_CANDIDATES = (
    "#btnLogin",
    "button#loginSubmit",
    "button[type='submit']",
    "button:has-text('Log In')",
    "button:has-text('Log in')",
    "button:has-text('Continue')",
)

# ============================================================
# 2FA / OTP (seeded from schwab-web)
# ============================================================

MFA_CODE_INPUT_CANDIDATES = (
    "#placeholderCode",
    "input[formcontrolname='placeholderCodeCtrl']",
    "#securityCode",
    "#txt-token",
    "input[name='securityCode']",
    "input[name='token']",
    "input[autocomplete='one-time-code']",
    "input[type='tel'][maxlength='6']",
    "input[type='number'][maxlength='6']",
    "input[type='text'][maxlength='6']",
    "input[type='text'][maxlength='8']",
)
MFA_CONTINUE_BUTTON_CANDIDATES = (
    "#continueButton",
    "#btnContinue",
    "button[type='submit']",
    "button:has-text('Continue')",
    "button:has-text('Verify')",
    "button:has-text('Submit')",
    "button:has-text('Next')",
)

# ============================================================
# Account-link / consent steps
# ============================================================
#
# After login + 2FA the OAuth grant walks several pages (observed live):
#   1. "Trader API End User Terms and Conditions" — an agreement checkbox
#      + Continue.
#   2. "Select your Schwab accounts to link" — one checkbox per account
#      (tick ALL so a newly opened account is linked
#      without anyone remembering to tick it) + Continue.
#   3. "Review your selected accounts" — Done.
#   4. redirect to the callback with ?code=…

# Heading that identifies the account-selection page (case-insensitive
# substring). We only auto-tick checkboxes when this is present, so the
# login page's "remember me" box is never touched.
ACCOUNT_LINK_HEADING = "Select your Schwab accounts to link"

# Heading that identifies the Terms & Conditions page (its agreement
# checkbox must be ticked before Continue enables).
TERMS_HEADING = "End User Terms and Conditions"

# Account-row checkboxes. Generic on purpose: the link page contains only
# the per-account boxes, and Playwright's .check() is idempotent (never
# unchecks), so checking every box here is safe.
ACCOUNT_CHECKBOX_SELECTOR = "input[type='checkbox'], [role='checkbox']"

# Buttons that advance the grant (used by --cli-mfa to drive the pages
# above). Positive-only — never "Cancel".
ADVANCE_BUTTON_CANDIDATES = (
    "button:has-text('Continue')",
    "button:has-text('Done')",
    "button:has-text('Allow')",
    "button:has-text('Authorize')",
    "button:has-text('Agree')",
    "button:has-text('Accept')",
    "input[type='submit'][value*='Continue' i]",
)
