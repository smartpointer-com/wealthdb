"""
URL and DOM landmarks for Schwab client-web Playwright flows.

Centralising these means selector / URL drift can be fixed in one
place. Each constant carries a comment describing the surface it
identifies; bump them in lockstep when Schwab restyles.

Nothing here performs I/O — pure configuration data + a couple of
URL-shape helpers.
"""

from __future__ import annotations

# ============================================================
# Entry points
# ============================================================

# Where login.py lands the operator. The header
# of this page embeds a `#schwablmslogin` iframe carrying the
# actual login form (gateway SPA at sws-gateway-nr.schwab.com).
# We pre-fill the form via frame_locator; the operator clicks
# Log In and satisfies Symantec VIP themselves via VNC.
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

# Symantec VIP / 2FA code entry. After the Log In click, a top-level
# page on sws-gateway-nr.schwab.com is served with a code
# input and a Continue button. Schwab has shipped at least two ids
# for the input (`securityCode` and the older `txt-token`), and the
# Continue button has shifted between a submit and a role=button —
# the CLI-MFA flow tries multiple selectors and falls back to a
# heuristic visible-text-input + Enter-key submit when none match.
# A DOM snapshot is logged on miss so the selector list can be
# narrowed across iterations.
MFA_CODE_INPUT_CANDIDATES = (
    # Current observation (Symantec VIP "Confirm Your Identity"
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
# signal in the upcoming combined login+scrape flow.
ACCOUNT_SUMMARY_URL = "https://client.schwab.com/app/accounts/summary"

# Statements & Tax Forms page — download.py drives this to
# enumerate per-account bank documents and fetch the PDFs.
STATEMENTS_URL = "https://client.schwab.com/app/accounts/statements/"

# Transaction History page. download.py's transactions mode
# currently only captures the rendered HTML + a screenshot per
# account; the real row-walker is deferred until we have a sample.
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
# preset <option value="..."> values below. The Schwab UI defaults
# to "Last3Months" in the live SPA even though the saved-state HTML
# snapshot in samples/ shows "Last10Years" selected — so we
# explicitly drive select_option rather than trusting the default.
# "Custom" exposes two date inputs whose DOM we don't have a
# sample of yet; --range custom is TODO.
DATE_RANGE_SELECT_ID = "date-range-select-id"
DATE_RANGE_VALUES = (
    "Today",
    "Last7Days",
    "Last3Months",
    "Last6Months",
    "Last5Years",
    "Last10Years",   # longest preset; "all available"
    "Custom",
)
# 3-month default matches the convention of the sibling
# collectors (schwab-api, ubs-psn, ubs-web).
# Bump explicitly with `--range Last10Years` for a full backfill.
DATE_RANGE_DEFAULT = "Last3Months"


# ============================================================
# Transaction History page selectors
# ============================================================
#
# Same SPA chrome as Statements (account-selector, date-range
# select), with a different element id for pagination and an
# "Apply" button instead of "Search". Reuses
# ACCOUNT_SELECTOR_* and DATE_RANGE_SELECT_ID above.

TX_PAGINATION_ELEMENT_ID = "pagination"
TX_ROW_SELECTOR = "sdps-table-row.sdps-tables__row--body"

# Tx-history's date-range <select> reuses Statements' `id`
# (`date-range-select-id`) but exposes a DIFFERENT set of
# option values:
#   Statements:  Today | Last7Days | Last3Months | Last6Months
#                | Last5Years | Last10Years | Custom
#   Tx-history:  Today | Last7Days | CurrentMonth | PreviousMonth
#                | Last6Months | CurrentYear | PreviousYear
#                | All | SpecifyDateRange
# So `Last10Years` is meaningless here; "all available" is `All`.
TX_DATE_RANGE_VALUES = (
    "Today", "Last7Days", "CurrentMonth", "PreviousMonth",
    "Last6Months", "CurrentYear", "PreviousYear",
    "All", "SpecifyDateRange",
)
# Schwab's tx-history option set doesn't include a "Last3Months"
# preset — the closest larger preset is "Last6Months". We use it
# as the default to stay roughly aligned with the Statements
# 3-month default; "All" is available for a full backfill.
TX_DATE_RANGE_DEFAULT = "Last6Months"

# Tx-history applies date-range / symbol filters via a Search
# button (NOT an Apply button — that's only inside the
# type-filter modal, which we don't currently drive).
TX_SEARCH_BUTTON_ID = "lbl_search-button"

# Tx-history results table is virtualized: only ~5 of N rendered
# rows are in the DOM at any time. We side-step the lossy DOM
# scrape by driving the "Export Transactions Data" modal that
# Schwab exposes — same data, machine-readable, complete in one
# fetch. The modal offers a choice of format (CSV/JSON/XML);
# we grab all three on the first iteration so silver can prefer
# whichever has the richest field set.
TX_EXPORT_MODAL_TITLE = "Export Transactions Data"
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
