"""
Centralised DOM landmarks and URLs for the UBS Switzerland retail
e-banking UI.

Selectors are kept in one place so a UI change touches one file.
Each constant is named after what it identifies, not where it lives,
so login.py and download.py can refer to selectors without caring
which template / SPA route hosts them.

UI surfaces this module references are read-only by contract — see
CLAUDE.md §1.
"""

from __future__ import annotations

import re

# ============================================================
# Hosts and entry-point URLs
# ============================================================

# UBS Switzerland retail e-banking is fronted by Nevis (the auth
# gateway) and load-balanced across four numbered hosts. The
# canonical entry point used to be `ebanking-ch.ubs.com`; in
# practice the SPA tells the browser to navigate to one of
# `ebanking-ch[1-4].ubs.com`. We accept any of them as legitimate
# UBS hosts.
HOSTS = (
    "ebanking-ch.ubs.com",
    "ebanking-ch1.ubs.com",
    "ebanking-ch2.ubs.com",
    "ebanking-ch3.ubs.com",
    "ebanking-ch4.ubs.com",
)
HOST_RE = re.compile(r"^https://ebanking-ch[1-4]?\.ubs\.com/")

# The login entry point. Hitting this with no session cookie serves
# the `AuthGetContractNrDialog` SPA bootstrap; with a valid cookie
# it redirects into the post-auth workbench.
LOGIN_ENTRY_URL = "https://ebanking-ch.ubs.com/workbench/WorkbenchOpenAction.do"

# Path the login flow lives on. URL contains `?login` while the
# SPA is in the contract-entry or QR-challenge dialog.
LOGIN_QUERY = "login"

# Post-auth, UBS redirects out of the Nevis auth gateway into the
# post-login SPA bundle, served at `/app/OQJ/<N>/ebanking/spa.html`
# with hash routing (`#/home`, `#/accounts`, ...). Older deep links
# under `/workbench/WorkbenchOpenAction.do?navitemid=<NavItem>` are
# also valid post-auth pages — UBS renders them server-side as
# workbench-style pages. We accept either family. `/workspace/`
# shows up as an intermediate redirect target in some flows.
#
# The `OQJ/<N>` segment is a versioned SPA bundle path that UBS
# rolls forward; do not pin it.
POST_AUTH_PATH_HINTS = ("/app/", "/workbench/", "/workspace/")


def is_post_auth_url(url: str) -> bool:
    """True iff the browser is on a logged-in UBS workbench URL.

    Used by login.py post-MFA, by login.py --check, and (in future)
    by download.py's session-verify step.
    """
    if not HOST_RE.match(url):
        return False
    if not any(hint in url for hint in POST_AUTH_PATH_HINTS):
        return False
    # `?login` (or `&login`) means we are still in the auth dialog.
    if f"?{LOGIN_QUERY}" in url or f"&{LOGIN_QUERY}" in url:
        return False
    # The dedicated logout-cookie iframe target lives at
    # /workspace/delete-login-cookie and would otherwise match the
    # /workspace/ hint above. Exclude it explicitly.
    if "delete-login-cookie" in url:
        return False
    return True


# ============================================================
# Stage 1 — AuthGetContractNrDialog (contract-number entry)
# ============================================================

# The SPA bootstraps with `initialProps.template = "AuthGetContractNrDialog"`
# on the first page. We read this from window state to confirm we
# are not on an unexpected screen (e.g. an interstitial KYC dialog
# UBS occasionally injects).
TEMPLATE_CONTRACT_NR = "AuthGetContractNrDialog"

# The visible contract-number input. The SPA labels it
# "Contract number" / "Vertragsnummer" depending on language. We
# locate by `name="loginalias"` (stable across locales) and fall
# back to the `id="contractNumber"` Playwright role-based lookup.
CONTRACT_INPUT_NAME = "loginalias"
CONTRACT_INPUT_ID = "contractNumber"

# Submit button on the contract-entry form. The SPA renders a
# `<button type="submit">` with an explicit data-testid that we
# can target without depending on locale.
CONTRACT_SUBMIT_TESTID = "submit-button"


# ============================================================
# Stage 1b — "Login starten" interstitial
# ============================================================

# After the contract-number form, UBS sometimes (always?) renders
# a short interstitial: a card titled "Login mit Access App" /
# "Login with Access App" with one button to advance to the QR
# challenge. The button label is locale-dependent ("Login starten",
# "Start login", "Avviare il login", ...) so we identify it by
# being the only submit-style button on a screen whose card title
# matches the access-app heading. The card title is rendered with
# a `data-testid="card-title"` (same element used on the QR page).

TEMPLATE_CONFIRM_ACCESS_APP = "AuthConfirmAccessAppDialog"

# We never read this template by name (since spa.js may call it
# something else); we identify the screen by the presence of the
# advance-button locator below.


# ============================================================
# Stage 2 — AuthQRDialog (Access App QR challenge)
# ============================================================

TEMPLATE_QR = "AuthQRDialog"

# After contract submission the SPA renders an <img data-testid=
# "qr-scanner-image"> whose `src` is a `data:image/png;base64,...`
# URL. We read the data URL directly rather than relying on the
# server-side `qrCodeImageInternal` REST endpoint, because the SPA
# does the base64-to-img wiring for us.
QR_IMG_TESTID = "qr-scanner-image"

# Card title on the QR dialog. Used as an additional readiness
# signal so we don't try to grab the img before the SPA has
# rendered the dialog at all.
QR_DIALOG_TITLE_TESTID = "card-title"

# The polling endpoint the SPA hits every 2s while waiting for the
# user to scan + approve in Access App. JSON shape (from spa.js):
#
#   { "result": "DONE"  | "CONTINUE" | "REFRESH_CONTINUE",
#     "qrCodeImage": "<base64 PNG>"      (only on REFRESH_CONTINUE),
#     "appLink":     "<deep link URL>"   (only on REFRESH_CONTINUE) }
#
# On DONE the SPA auto-submits the form; the resulting POST hands
# Nevis the proof-of-approval and Nevis redirects to the post-auth
# workbench. We don't poll the endpoint directly — we let the SPA
# do its thing and watch for the URL transition.
QR_POLL_INTERVAL_SECONDS = 2  # set by the SPA in setInterval(f, 2e3)

# The SPA periodically rotates the QR (REFRESH_CONTINUE), updating
# the `<img>` src in place. We re-render the terminal QR on every
# observed src change so a slow Access-App scan doesn't fail.

# ============================================================
# Templates we explicitly do NOT want to see
# ============================================================

# If UBS routes us to one of these instead of the expected QR
# dialog, the script bails out with an actionable error rather
# than hanging on a wait_for. Names are best-effort — the SPA
# bundles them as string literals; expand the list as new ones
# surface in the wild.
UNEXPECTED_TEMPLATES = (
    # Access Card challenge (TAN reader) — would be served if the
    # user has Access Card enabled instead of / in addition to
    # Access App. This script only implements the Access App path.
    "AuthAccessCardDialog",
    # PIN-change interstitial UBS sometimes injects after MFA.
    "AuthChangePinDialog",
    # Soft-challenge limit exceeded — too many failed attempts.
    "AuthSoftChallengeExceededDialog",
)


# ============================================================
# Post-auth SPA navigation (used by download.py)
# ============================================================

# Hash routes inside the post-auth SPA. UBS routes via fragment
# identifiers on a single bundle URL (/app/OQJ/<N>/ebanking/spa.html).
# We append a route fragment to navigate without a full page load.
ROUTE_HOME = "#/home"
# Custody-account positions, default portfolio of the default banking
# relationship. Used as a fallback when we can't enumerate explicit
# portfolioUids. `preselectFirstPortfolio=true` tells the SPA to skip
# the chooser shell — without it the page loads empty.
ROUTE_POSITIONS_DEFAULT = (
    "#/assets/asset-view?goto=positions"
    "&preselectFirstPortfolio=true&navitemid=PositionsPage"
)


def positions_url_for_portfolio(portfolio_uid: str,
                                banking_relation_id: str) -> str:
    """Hash route for the positions snapshot of a specific portfolio.

    Anchors on the homepage carry one (portfolioUid, bankingRelationId)
    pair per portfolio. Navigating with these explicit values lets us
    iterate every portfolio under the active banking relationship and
    pull a distinct positions.csv for each. Without them UBS defaults
    to the first portfolio only (`preselectFirstPortfolio=true`)."""
    return (
        "#/assets/asset-view?goto=positions"
        f"&portfolioUid={portfolio_uid}"
        f"&bankingRelationId={banking_relation_id}"
        "&navitemid=PortfolioPositions"
    )


# Anchors on the homepage that link to per-portfolio overview pages.
# We harvest the `portfolioUid` and `bankingRelationId` query params
# from each, then pivot to the positions route above.
HOME_PORTFOLIO_LINK_SELECTOR = (
    'a[href*="portfolioUid="][href*="bankingRelationId="]'
)
# Bare `#/documents` shows a "We cannot display this page" stub —
# the docs micro-frontend requires the navitemid query to bootstrap.
ROUTE_DOCUMENTS = "#/documents/bank-documents?navitemid=MailboxEdocumentsPg"
ROUTE_CASH_ACCOUNT_TRANSACTIONS_PREFIX = (
    "#/accounts?target=cash-account-transactions"
)

# Anchor selector on the homepage. Every cash account has an
# anchor whose href contains the route + an opaque `accountId=`
# token (the only stable identifier we get — IBANs are partially
# redacted in the rendered display).
#
# Card / credit-card-transaction anchors are intentionally not
# scraped: this is a wealth-management toolkit, not personal
# finance. The `#/cards?target=card-account-transactions` route
# exists but we ignore it.
HOME_CASH_ACCOUNT_LINK_SELECTOR = (
    'a[href*="cash-account-transactions"][href*="accountId="]'
)


# ============================================================
# Account transactions page (#/accounts? or #/cards?)
# ============================================================

# Three server-side export buttons in the transactions toolbar.
# Cash accounts expose all three. Card accounts only render the CSV
# button — and on cards the button carries `title="CSV"` but no
# `data-name`, so we identify by title (which is stable for both
# surfaces).
TXN_BUTTON_CSV_SELECTOR = 'button[title="CSV"]'
TXN_BUTTON_PDF_SELECTOR = 'button[title="PDF"]'
TXN_BUTTON_MT940_SELECTOR = 'button[data-name="button-swiftMt940Export"]'

# MT940 download flow: clicking the MT940 button does NOT trigger a
# direct download. Instead UBS opens a `<dialog>` titled "Select the
# file variant to export" with two radios + an Export button. We
# select the enriched variant, then click Export (which is the call
# that actually streams the MT940 file).
#
# When the active filter spans more than 1000 transactions UBS
# replaces the variant chooser with an info-only "A maximum of 1000
# transactions can be exported" dialog (Close button only). We
# detect that case by the absence of the Export button.
MT940_DIALOG_EXPORT_BUTTON = '[data-testid="export-button"]'
MT940_DIALOG_CANCEL_BUTTON = '[data-testid="cancel-download-button"]'
MT940_DIALOG_CLOSE_BUTTON = 'dialog[open] [aria-label="Close"]'
# The native radio inputs are visually hidden; UBS styles the
# wrapping <label> as the click target. We click the label, not the
# input, otherwise Playwright bails with "element is not visible".
MT940_DIALOG_RADIO_ENRICHED_LABEL = 'label[for="download-option-1"]'
MT940_DIALOG_RADIO_LIGHT_LABEL = 'label[for="download-option-0"]'

# Period filter combobox. Clicking opens a popover (`<dialog>`)
# with a Predefined/Custom radio toggle and either a list of
# preset periods or a from/to date-picker.
TXN_PERIOD_FILTER_SELECTOR = '[aria-label="Filter by period"]'

# Inside the Period popover. The two surfaces (transactions vs.
# documents) render DIFFERENT date-input markup:
#   - Transactions: inputs have `name="dateFrom"` / `name="dateTo"`,
#     Apply has `data-name="filter-item-submit-button"`.
#   - Documents: inputs have no `name=` attribute (labeled "Start
#     date" / "End date"), Apply is `<button type="submit">`.
# We use placeholder-based selection and prefer the data-name
# Apply button when present, falling back to any submit button.
# All popover selectors are scoped to `dialog[open]` — the SPA
# leaves stale, hidden radios and inputs elsewhere in the page DOM
# (asset-class sliders, view toggles, etc.) and an unscoped selector
# silently grabs the wrong one.
PERIOD_POPOVER_CUSTOM_RADIO = '[role="radio"]:has-text("Custom")'  # combine with dialog[open] when used
PERIOD_POPOVER_DATE_INPUTS = 'dialog[open] input[placeholder="DD.MM.YYYY"]'
PERIOD_POPOVER_APPLY_BUTTON_PRIMARY = 'dialog[open] button[data-name="filter-item-submit-button"]'
PERIOD_POPOVER_APPLY_BUTTON_FALLBACK = 'dialog[open] form button[type="submit"]'

# UBS hard-caps the transactions UI at ~28 months of history; any
# `from` date earlier than the cap triggers an inline validation
# message like:
#   "The start date must be later than 01.01.2024."
# We detect this and clamp `since` to the offered minimum date.
# Bank documents have a longer (10y) retention and use a different
# minimum, so the same regex applies to both surfaces.
PERIOD_POPOVER_MIN_DATE_RE = re.compile(
    r"start date must be later than (\d{2}\.\d{2}\.\d{4})", re.IGNORECASE,
)
PERIOD_POPOVER_MAX_DATE_RE = re.compile(
    r"end date must be earlier than (\d{2}\.\d{2}\.\d{4})", re.IGNORECASE,
)

# Number-of-transactions counter — used as a "data has rendered"
# readiness signal before triggering an export.
TXN_COUNT_SELECTOR = '[data-name="number-of-trx"]'

# Hash fragment that flags this as a cash-account view.
TXN_TARGET_CASH = "cash-account-transactions"


# ============================================================
# Bank documents page (#/documents)
# ============================================================

# Each document row carries an <a href="https://…/api/v1/digital-
# banking/files/<token>?apikey=<key>&Accept=application/pdf&…">.
# The link is the rendered "document name" hyperlink AND the row's
# trailing download icon points at the same URL. We harvest these
# directly from the DOM and fetch via context.request.get() — no
# per-row clicking, no Playwright `expect_download` plumbing.
# UBS serves these hrefs as relative paths (e.g. `/api/v1/…`) in
# the live page DOM, even though saved-as-HTML dumps show absolute
# URLs. We accept either form and let the caller resolve against
# the page origin.
DOC_FILES_API_URL_RE = re.compile(
    r"(?:https://ebanking-ch[0-9]?\.ubs\.com)?"
    r"/api/v1/digital-banking/files/([A-Za-z0-9]+)\?[^\"\']+"
)
DOC_LINK_SELECTOR = 'a[href*="/api/v1/digital-banking/files/"]'

# Five filter buttons on the documents toolbar, in DOM order:
# (1) Period (2) Category (3) Banking relationship
# (4) Account/Portfolio (5) Status. They all share a generic
# CSS class with no individual handle, so we locate by index. The
# Status filter happens to carry a `data-tour-target`; the others
# do not.
DOC_FILTER_BUTTON_SELECTOR = "button.UWR_FilterItem_filter-content_GXm9A"
DOC_FILTER_INDEX_PERIOD = 0
DOC_FILTER_INDEX_CATEGORY = 1
DOC_FILTER_INDEX_BANKING_RELATIONSHIP = 2
DOC_FILTER_INDEX_ACCOUNT_PORTFOLIO = 3
DOC_FILTER_INDEX_STATUS = 4

# UBS caps the documents list at 999 rows per query. When the
# current filter selection would return more, the header counter
# shows "(999)" and a banner reads:
#
#   "Not all documents are displayed right now. If you cannot find
#    a document, try applying filters."
#
# Our window-splitting strategy: harvest a half-window at a time,
# and when a window's count hits the cap, recursively bisect.
DOC_LIST_CAP = 999


# ============================================================
# Positions snapshot page (#/assets/asset-view?goto=positions)
# ============================================================

# CSV export button on the positions toolbar. Same `data-name` as
# the transactions CSV button but with a different aria-label.
POSITIONS_BUTTON_CSV_SELECTOR = (
    'button[data-name="button-csvExport"][aria-label*="positions"]'
)
