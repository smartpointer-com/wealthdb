"""
URL and DOM landmarks for Schwab client-web Playwright flows.

Centralising these means selector / URL drift can be fixed in one
place. Each constant carries a comment describing the surface it
identifies; bump them in lockstep when Schwab restyles.

Nothing here performs I/O — pure configuration data + a couple of
URL-shape helpers.
"""

from __future__ import annotations

import urllib.parse

# ============================================================
# Entry points
# ============================================================

# Where login.py lands. The header of this page embeds a
# `#schwablmslogin` iframe carrying the actual login form (gateway
# SPA at sws-gateway-nr.schwab.com). The form is pre-filled via
# frame_locator; Log In and the 2FA challenge are then satisfied
# via the CLI-MFA stdin prompt by default (by hand over VNC as
# the fallback).
MARKETING_HOMEPAGE = "https://www.schwab.com/"

# `id` of the login iframe on the marketing homepage.
LOGIN_IFRAME_ID = "schwablmslogin"

# Stable IDs for the form inputs inside the iframe (confirmed in
# the gateway SPA's main.*.js bundle as Angular form-control names).
LOGIN_ID_INPUT_ID = "loginIdInput"
PASSWORD_INPUT_ID = "passwordInput"

# Log-In submit button inside the iframe. Best-guess id (the
# gateway SPA's bundle uses a few naming variants across releases);
# CLI-MFA login falls back to a role/text query if the id misses.
LOGIN_BUTTON_ID = "btnLogin"
LOGIN_BUTTON_TEXT = "Log In"

# 2FA code entry. After the Log In click, a top-level
# page on sws-gateway-nr.schwab.com is served with a code
# input and a Continue button. Schwab has shipped at least two ids
# for the input (`securityCode` and the older `txt-token`), and the
# Continue button has shifted between a submit and a role=button —
# the CLI-MFA flow tries multiple selectors and falls back to a
# heuristic visible-text-input + Enter-key submit when none match.
# A DOM snapshot is logged on miss so the selector list can be
# narrowed across iterations.
MFA_CODE_INPUT_CANDIDATES = (
    # Current observation ("Confirm Your Identity" 2FA
    # page): input id="placeholderCode" — named after the gateway
    # SPA's #/placeholder route. type="number" maxlength="6".
    "#placeholderCode",
    "input[formcontrolname='placeholderCodeCtrl']",
    # Older / alternative-factor selectors retained as fallbacks.
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

# Landmark URL hit by `--check` (and the natural post-auth landing
# page after login). Same URL doubles as the post-auth-detected
# signal in the combined login+scrape flow (login.py).
ACCOUNT_SUMMARY_URL = "https://client.schwab.com/app/accounts/summary"

# Statements & Tax Forms page — download.py drives this to
# enumerate per-account bank documents and fetch the PDFs.
STATEMENTS_URL = "https://client.schwab.com/app/accounts/statements/"

# Transaction History page. download.py's transactions mode selects
# each account, applies the date-range filter, and drives the Export
# modal to save CSV/JSON/XML of the full tx-history; by default (unless
# --no-more-detail) it also walks each row's "More" detail modal.
TRANSACTION_HISTORY_URL = "https://client.schwab.com/app/accounts/history/"


# ============================================================
# Statements & Tax Forms page selectors
# ============================================================

# Document-type filter chips, addressed by their `lookupid`
# attribute (text labels live inside the Stencil <sdps-chips>
# shadow DOM, so `:has-text(...)` doesn't reach them). Per the
# collection brief, all bank documents EXCEPT trade confirms are
# wanted: DOC_TYPES_WANTED is the set we ensure ON, the rest of
# DOC_TYPES we ensure OFF.
DOC_TYPES = (
    ("Statements",      "statements-chip"),
    ("Tax Forms",       "taxforms-chip"),
    ("Letters",         "letters-chip"),
    ("Reports & Plans", "reportsplans-chip"),
    ("Trade Confirms",  "confirms-chip"),
)
DOC_TYPES_WANTED = frozenset({
    "Statements", "Tax Forms", "Letters", "Reports & Plans",
})

# The "selected" state on <sdps-chips> is exposed via the HTML
# attribute `selected=""` (NOT a CSS class). Older snapshots that
# suggested a `sdps-chips--selected` class were misreading the
# rendered DOM.
CHIP_SELECTED_ATTR = "selected"

# Dismiss-only close controls inside an open sdps modal — the
# escalation _dismiss_open_modal reaches for when Escape doesn't clear
# the overlay (observed live: the wire-details modal a wire row's
# "More" opens ignores Escape). Deliberately limited to dismissive
# "X" / Close controls, never OK / Continue / action buttons: on an
# unknown dialog those could confirm an action (read-only contract,
# AGENTS.md §1).
MODAL_CLOSE_SELECTORS = (
    '[role="dialog"]:visible button[aria-label*="close" i]',
    '[role="dialog"]:visible .sdps-modal__close',
    '[role="dialog"]:visible button:has-text("Close")',
)

# Account selector — opens a list of accounts; each entry has an
# id of the form `account-selector-header-0-account-<N>`.
ACCOUNT_SELECTOR_BUTTON_CLASS = "account-selector-button"
ACCOUNT_SELECTOR_ENTRY_ID_PREFIX = "account-selector-header-0-account-"

# Search button on the Statements page. Visible text.
SEARCH_BUTTON_TEXT = "Search"

# Result-table row. One per document in the current page;
# cells inside are <sdps-table-cell>.
RESULT_ROW_SELECTOR = "sdps-table-row.sdps-tables__row--body"

# Per-row download buttons. Schwab renders one
# `<button aria-label="Click to Download <FORMAT>">` per format
# the document is available in (PDF for everything; 1099 Composite
# and similar tax forms additionally expose XML and CSV). Match
# all of them by prefix and capture <FORMAT> as the file extension.
DOWNLOAD_ARIA_PREFIX = "Click to Download "

# Pagination control around the result table.
#
# The host element is <sdps-pagination id="document-pagination">.
# Inside it, each page (and the Prev/Next steppers) renders as an
# <a id="pagination-{N|next|previous}-link"> inside an <li>. The
# "no more pages" signal is the parent <li> getting class
# `sdps-hide` — there is NO disabled attribute (these are anchors,
# not buttons).
PAGINATION_ELEMENT_ID = "document-pagination"
PAGINATION_NEXT_LINK_ID = "pagination-next-link"
PAGINATION_HIDDEN_LI_CLASS = "sdps-hide"

# Date-range filter. <select id="date-range-select-id"> with the
# preset <option value="..."> values below, as observed in the
# live SPA. The UI defaults to "Last3Months", but a session can
# land with a different value pre-selected — so select_option is
# driven explicitly rather than trusting the default. Both pages
# share the custom mode under the value "SpecifyDateRange"
# (rendered label "Custom date range");
# download.fill_custom_date_range drives its two datepickers.
DATE_RANGE_SELECT_ID = "date-range-select-id"
DATE_RANGE_VALUES = (
    "Today",
    "Last7Days",
    "CurrentMonth",
    "PreviousMonth",
    "Last3Months",
    "Last6Months",
    "YearToDate",
    "PreviousYear",
    "Last5Years",
    "Last10Years",   # longest preset; "all available"
    "SpecifyDateRange",
)
# 3-month default matches the convention of the sibling
# collectors (schwab-api, ubs-psn, ubs-web).
# Widen with `--lookback` (e.g. `--lookback all`) for a full backfill.
DATE_RANGE_DEFAULT = "Last3Months"


# ============================================================
# Transaction History page selectors
# ============================================================
#
# Same SPA chrome as Statements (account-selector, date-range
# select), with a different pagination element id; filters apply
# via the page's own Search button (TX_SEARCH_BUTTON_ID). Reuses
# ACCOUNT_SELECTOR_* and DATE_RANGE_SELECT_ID above.

TX_PAGINATION_ELEMENT_ID = "pagination"
TX_ROW_SELECTOR = "sdps-table-row.sdps-tables__row--body"

# Tx-history's date-range <select> reuses Statements' `id`
# (`date-range-select-id`) but exposes a DIFFERENT set of option
# values (TX_DATE_RANGE_VALUES here vs DATE_RANGE_VALUES above):
# `Last10Years` is meaningless here; "all available" is `All`.
TX_DATE_RANGE_VALUES = (
    "Today", "Last7Days", "CurrentMonth", "PreviousMonth",
    "Last6Months", "CurrentYear", "PreviousYear",
    "All", "SpecifyDateRange",
)
# Tx-history applies date-range / symbol filters via a Search
# button (NOT an Apply button — that's only inside the
# type-filter modal, which we don't currently drive).
TX_SEARCH_BUTTON_ID = "lbl_search-button"

# Tx-history results table is virtualized: only ~5 of N rendered
# rows are in the DOM at any time. We side-step the lossy DOM
# scrape by driving the "Export Transactions Data" modal that
# Schwab exposes — same data, machine-readable, complete in one
# fetch. The modal offers a choice of format (CSV/JSON/XML); all
# three are captured — load ingests the JSON (it adds AcctgRuleCd),
# CSV/XML stay as opaque documents.
TX_EXPORT_FORMATS = (
    # (display_label, radio_input_id, on-disk extension)
    ("Csv",  "input-csv",  "csv"),
    ("Json", "input-json", "json"),
    ("Xml",  "input-xml",  "xml"),
)


# ============================================================
# Helpers
# ============================================================

def is_post_auth_url(url: str) -> bool:
    """Return True if `url` is a logged-in-only Schwab SPA route."""
    return url.startswith("https://client.schwab.com/app/")


# The login + 2FA flow runs on Schwab's gateway SPA
# (sws-gateway*.schwab.com — this flow sees the sws-gateway-nr host,
# schwab-api's OAuth flow sws-gateway). When the gateway ends a session
# with a notice — account lockout among them — it routes to
# `#/information/<code>`. That page is terminal: no flow proceeds past
# it, so every wait loop treats it as an immediate, non-retryable stop.
# Mirrored in schwab-api/oauth_landmarks.py.
def is_gateway_notice_url(url: str) -> bool:
    """True for the gateway SPA's terminal-notice route family
    (`https://sws-gateway*.schwab.com/ui/host/#/information/<code>`)."""
    try:
        parts = urllib.parse.urlsplit(url)
    except ValueError:
        return False
    host = parts.netloc.lower()
    if not (host.startswith("sws-gateway") and host.endswith(".schwab.com")):
        return False
    return parts.fragment.startswith("/information")


# Where the notice page renders its message: a #msgLabel span inside
# the <lms-information> element (observed live). The content arrives
# via an async fetch seconds after the route change, so extraction
# polls for these before falling back to whole-body text.
NOTICE_MESSAGE_SELECTORS = ("#msgLabel", "lms-information")

# Case-insensitive substrings that mark a notice page as an account
# lockout rather than a generic interstitial, verified against the
# gateway's live content catalog: the lockout notice renders "we need to
# verify your identity before proceeding" — the identity-verification
# lock behind which web/mobile logins sit — while sibling codes render a
# generic "There's a problem logging you in" (deliberately not matched:
# the run always prints the page's own text verbatim either way, and
# this only selects the extra "unlock with Schwab" hint).
LOCKOUT_TEXT_MARKERS = ("locked", "verify your identity")


def looks_locked(page_text: str) -> bool:
    """Whether visible notice-page text reads as an account lockout."""
    lowered = page_text.lower()
    return any(m in lowered for m in LOCKOUT_TEXT_MARKERS)
