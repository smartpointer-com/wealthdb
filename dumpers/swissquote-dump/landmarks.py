"""
Centralised DOM landmarks and URLs for the Swissquote e-banking and
Trading Platform UIs.

Selectors are kept in one place so that a UI change touches one file.
Each constant is named after what it identifies, not where it lives,
so login.py and download.py can refer to selectors without caring
which SPA hosts them.

UI surfaces this module references are read-only by contract — see
CLAUDE.md §1. The Buy/Sell buttons that appear inside every position
row are inside the DOM we parse; reading them is fine but they must
never be clicked.
"""

from __future__ import annotations

# ============================================================
# Hosts and entry-point URLs
# ============================================================

HOST = "trade.swissquote.ch"

# Swissquote uses F5 BIG-IP APM as its auth gateway. F5 intercepts any
# request to a protected resource that lacks a valid session cookie
# and serves the login + MFA pages at /my.policy, then redirects back
# to the originally-requested URL once authenticated.
#
# There is no static "login URL" — we must request a protected resource
# to trigger F5's interception. The eBanking SPA root works reliably
# as the trigger and is also where the user lands after auth.
LOGIN_TRIGGER_URL = f"https://{HOST}/sqc-web-client-portal/"

# Path fragment F5 uses for the login form. The MFA wait page lives
# at a *different* URL (F5 transitions away from /my.policy as soon
# as credentials are accepted, before the user approves the push),
# so "/my.policy not in url" is NOT a valid signal of "MFA done".
F5_AUTH_PATH = "/my.policy"

# Positive landmark for "fully authenticated". F5 attaches a
# `url_id=<hex>` query parameter when it forwards the post-auth
# request back to the originally-requested protected URL. We wait
# for this AND the SPA-root path before declaring login successful.
def is_post_auth_url(url: str) -> bool:
    """URL is on the post-auth eBanking SPA path, not the F5 auth form.

    F5 may or may not attach a `url_id=` query parameter on the
    redirect; testing for it produced false negatives. The reliable
    signal is just "we're on the protected SPA path and not on
    /my.policy". Used by login.py post-MFA, by login.py --check, and
    by download.py's session-verify.
    """
    return F5_AUTH_PATH not in url and "/sqc-web-client-portal/" in url


def is_profile_validation_url(url: str) -> bool:
    """Detects Swissquote's periodic regulatory-KYC interstitial.

    After MFA approval, F5 sometimes routes the user to a profile-
    validation plugin (e.g. the "executive position" question that
    Swissquote refreshes annually for regulatory reasons). The script
    cannot answer this on the user's behalf — it has to be done once
    via a regular browser. We detect it so we can fail fast with an
    actionable message instead of hanging on wait_for_url.
    """
    return "sq-profile-validation-plugin" in url



# Trading Platform — hash-routed SPA. Append a route fragment to land
# on a specific page. Reachable once F5 has issued a session cookie;
# unlike LOGIN_TRIGGER_URL this URL does NOT trigger login interception
# when unauthenticated (F5 serves it as a public-looking blank SPA).
TRADING_PLATFORM_BASE_URL = f"https://{HOST}/eding_trading-platform/"
ROUTE_PORTFOLIO_OVERVIEW = "#portfoliooverview"
ROUTE_TRANSACTIONS = "#transactions"

# eBanking — separate SPA, where the Documents listing page lives.
# The SPA's default landing route is #accountOverview/main; documents
# are at #documents. The Period filter defaults to the last 30 days,
# so the script must widen it to fetch all historical documents.
EBANKING_BASE_URL = LOGIN_TRIGGER_URL  # same SPA root as the login trigger
DOCUMENTS_URL = f"{EBANKING_BASE_URL}#documents"
ROUTE_ACCOUNT_OVERVIEW = "#accountOverview/main"

# Account-list selectors on the eBanking #accountOverview/main page.
# Each `<li.AccountListItem>` wraps a single account; inside it,
# `.AccountDetails__portfolioTitle` contains the `<TYPE> <CUSTOMER_ID>`
# label that Swissquote uses as the informal account-type indicator.
ACCOUNT_LIST_ROW = "li.AccountListItem"
ACCOUNT_PORTFOLIO_TITLE = ".AccountDetails__portfolioTitle"

# Document fetch endpoint. The page is JS-driven, but each document
# is ultimately served by this REST endpoint with the session cookie.
# download.py reuses the cookie via Playwright's `request` API and
# bypasses the per-row download click entirely.
DOC_FETCH_URL_TEMPLATE = (
    f"https://{HOST}/sqc-ctrp-notifications-plugin/webapi/notifications/"
    "getPdfDocument/{customer_id}/{doc_id}?documentType={doc_type}"
)

# ============================================================
# Login page — F5 BIG-IP gateway form
# ============================================================

LOGIN_USERNAME_INPUT = "input#usernameText"
LOGIN_PASSWORD_INPUT = "input#passwordText"
LOGIN_SUBMIT_BUTTON = "button#loginText"

# ============================================================
# MFA — Mobile Level 3 wait page
# ============================================================

# The MFA page does not have a stable form; the user approves the
# push on their phone and the page transitions on its own. We detect
# arrival on this page by a text landmark, and detect departure by
# *navigation away* from it (URL change), not by DOM mutation.
MFA_PAGE_TEXT_LANDMARK = "Mobile Level 3 Authentication"

# Operation No. — the 6-character TAN code the phone app shows
# alongside the approval prompt. The user is supposed to verify the
# two match before tapping approve. login.py scrapes it from this
# selector and prints it to the terminal so the operator does not
# need to reach for the browser screenshot.
MFA_OPERATION_CODE_SELECTOR = ".SmartL3__operation"

# Per-push countdown text — informational, not used as a landmark.
# The Swissquote UI says "This request is valid for 60 seconds".

# ============================================================
# Post-login landing — Trading Platform root
# ============================================================

# The Trading Platform SPA sets this testid on its root container
# once the user is authenticated. We poll for it as the "logged-in"
# liveness signal in login.py --check and at the start of download.py.
TRADING_PLATFORM_ROOT_TESTID = "SecuritiesRetailTradingPlatform"
TRADING_PLATFORM_ROOT_SELECTOR = (
    f'[data-testid="{TRADING_PLATFORM_ROOT_TESTID}"]'
)

# ============================================================
# Transactions page (Trading Platform #transactions)
# ============================================================

# Top-right "download" dropdown trigger. Opens a menu containing
# CSV / PDF / etc. The CSV item label depends on the locale; we
# force the UI to English before clicking.
TXN_EXPORT_DROPDOWN_TRIGGER = ".Dropdown__trigger--export"

# Date range inputs — three boxes per date (DD / MM / YYYY), two
# date pickers (from / to). All four selectors are `.all()` matches;
# index [0..2] is the "from" date, [3..5] is the "to" date.
TXN_DATE_DAY_INPUT = "input.InputDate__input--day"
TXN_DATE_MONTH_INPUT = "input.InputDate__input--month"
TXN_DATE_YEAR_INPUT = "input.InputDate__input--year"

# Menu items inside the transactions export dropdown. The dropdown
# renders these only after the trigger is clicked. Items are
# `<li class="Menu__item Menu__item--export">` with text "CSV report"
# / "PDF report". We match by class + has-text so a future label
# change like "CSV file" still hits.
TXN_EXPORT_MENU_CSV = 'li.Menu__item--export:has-text("CSV")'

# ============================================================
# Portfolio Overview page (Trading Platform #portfoliooverview)
# ============================================================

# Three export-flavoured buttons live on this page; only two are of
# interest:
#   - `.ExportButton`  (aria-label "Export"), next to the Positions
#                       table — downloads the Positions XLS.
#   - `.CaptionButton` (aria-label "Export"), next to the Assets
#                       table — downloads the List of Assets XLS
#                       (per-currency cash + FX rollup).
#   - `.srp-ControlsPanel__printInfo` (aria-label "Export account
#                       overview") — a third export near "Buying
#                       power"; not used here (purpose unconfirmed).
POSITIONS_EXPORT_BUTTON = 'button.ExportButton[aria-label="Export"]'
LIST_OF_ASSETS_EXPORT_BUTTON = 'button.CaptionButton[aria-label="Export"]'

# The third button — top-right of the Overview section, near
# "Buying power". Server-side renders a portfolio-summary PDF
# (`account-overview_<customer>.pdf`, ~35KB). Saved as
# `account_overview.pdf` in bronze.
ACCOUNT_OVERVIEW_EXPORT_BUTTON = (
    'button.srp-ControlsPanel__printInfo[aria-label="Export account overview"]'
)

# Filename pattern Swissquote uses for each export, for sanity-
# checking the captured download:
#   positions.xls       e.g. Positions_<customer>_<ddmmyyyy>_<hh>_<mm>.xls
#   list_of_assets.xls  e.g. List_of_assets_<ddmmyyyy>.xls
POSITIONS_DOWNLOAD_NAME_RE = r"^Positions_\d+_\d{8}_\d{2}_\d{2}\.xls$"
LIST_OF_ASSETS_DOWNLOAD_NAME_RE = r"^List_of_assets_\d{8}\.xls$"

# Position rows inside the Portfolio Overview table. Reading-only —
# Buy/Sell buttons appear in these rows and MUST NEVER be clicked
# (CLAUDE.md §1). We only use the row selector to count expected
# rows for cross-checking against the XLS export.
PORTFOLIO_POSITION_ROW = (
    "tr.TableRow:not(.TableRow--subTotalRow):not(.TableRow--totalRow)"
)

# ============================================================
# Documents page (eBanking)
# ============================================================

# Each document is one table row. Rows tagged --notDownloaded have
# never been fetched server-side from Swissquote's POV; we do not
# rely on that tag for our own dedup (we use content_sha256 in
# silver), but it is useful for prioritising unfetched docs.
DOC_ROW = "tr.NotificationRow"
DOC_ROW_NOT_DOWNLOADED = "tr.NotificationRow.NotificationRow--notDownloaded"

# Spinner shown over the documents table while it (re)loads after a
# filter change. Visible briefly, then removed when the table renders.
DOC_TABLE_SPINNER = ".LoadingTable"

# Each row carries the document ID and type as part of the per-row
# download button's class/data attributes. The exact extraction is
# in download.py; this constant pins the row's download-button class.
DOC_ROW_DOWNLOAD_BUTTON = ".ua-docListTable__button--downloadDocument"

# Document type taxonomy as observed in the URL parameter
# documentType=...; not constrained at the schema layer, but useful
# for the loader to populate document_type without parsing the URL.
DOC_TYPE_VALUES = frozenset({
    "Corporate",    # corporate-action notices
    "Stock",        # trade/exchange notices
    "Transfer",     # incoming/outgoing wire notices
    "Account",      # account statements
    "PERSON",       # account-holder correspondence
})

# ============================================================
# Read-only / write-surface boundary
# ============================================================

# These selectors identify UI surfaces that MUST NEVER be navigated
# to or clicked. They are listed here so a code reviewer can grep
# for them and confirm the codebase never references them in a
# `click()` or `goto()` context. See CLAUDE.md §1.
FORBIDDEN_TO_CLICK_SELECTORS = frozenset({
    "button.Button--buy",                                    # row-level Buy
    "button.Button--sell",                                   # row-level Sell
    "button.securitiesRetailTradeButtonPlugin-Button--buy",  # alternate
    "button.securitiesRetailTradeButtonPlugin-Button--sell", # alternate
})
