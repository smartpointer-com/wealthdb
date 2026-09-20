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

import base64
import re

# ============================================================
# Hosts and entry-point URLs
# ============================================================

# UBS Switzerland retail e-banking is fronted by Nevis (the auth
# gateway) and load-balanced across four numbered hosts. The entry
# point is the unnumbered `ebanking-ch.ubs.com` (LOGIN_ENTRY_URL);
# from there the SPA tells the browser to navigate to one of
# `ebanking-ch[1-4].ubs.com`. All five are legitimate UBS hosts.
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

    Used by login.py post-MFA, by login.py --check, and by
    download.py's session-verify step.
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
# locate by `name="loginalias"`, which is stable across locales.
# Submission clicks the card's visible primary submit button, so no
# further landmark is needed.
CONTRACT_INPUT_NAME = "loginalias"

# After the contract-number form, UBS sometimes (always?) renders
# a short "Login starten" interstitial: a card with one button to
# advance to the QR challenge. login.py resolves it by waiting for
# whichever renders first — the QR image or the interstitial — and
# clicking through; no dedicated landmark is needed.


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

# Anchor selector on the homepage. Every cash account has an anchor
# whose href contains the route + an opaque `accountId=` token (the only
# stable identifier we get — IBANs are partially redacted in the
# rendered display).
#
# The card area uses the same anchor shape under a different `target=`
# sub-surface, but no card selector is pinned here: nothing consumes one
# yet, and a landmark is added when the code that reads it is, from a
# capture rather than from a guess.
HOME_CASH_ACCOUNT_LINK_SELECTOR = (
    'a[href*="cash-account-transactions"][href*="accountId="]'
)


# ============================================================
# Cash-account transactions page (#/accounts?)
# ============================================================

# Server-side export buttons in the transactions toolbar: CSV, PDF and
# MT940. Identified by title rather than `data-name` because the title
# is the stable handle here.
#
# These are the CASH surface's. The card area's toolbar is a different
# markup (its buttons carry `data-testid`), and nothing below is used
# there — the card ledger is read from the API instead (DESIGN.md §5).
TXN_BUTTON_CSV_SELECTOR = 'button[title="CSV"]'
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
#
# The class is CSS-modules generated: a stable `UWR_FilterItem_
# filter-content_` prefix plus a build hash that rotates whenever
# UBS redeploys the docs micro-frontend (observed 2026-07: _GXm9A
# → _TzYzX). Match on the stable prefix only, never a full class.
DOC_FILTER_BUTTON_SELECTOR = 'button[class*="UWR_FilterItem_filter-content_"]'
DOC_FILTER_INDEX_PERIOD = 0

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



# ============================================================
# Portfolio securities transactions
# (#/assets/asset-view/securitiesTransactions)
# ============================================================
#
# The cash surface above reaches only the accounts the homepage files
# as cash tiles. A managed portfolio's own movements — every securities
# trade, the corporate actions against its holdings — live here
# instead, on a legacy `/assetview/` application the SPA hosts in child
# frames. Before this route existed as a landmark those movements
# reached silver only through the annual Account Statement PDF, which
# is published once a year and therefore leaves the current year's
# trades unreadable until the following January.


def portfolio_overview_url_for_portfolio(
        portfolio_uid: str, banking_relation_id: str) -> str:
    """Hash route for a portfolio's overview page.

    The step before the transaction list. Both routes render from the
    same bundle, but the list hosts a legacy application in child
    frames that are only built when the SPA has a portfolio in hand;
    arriving at the list cold renders the route and nothing inside
    it."""
    return (
        "#/assets/asset-view/portfolio-overview"
        f"?bankingRelationId={banking_relation_id}"
        "&navitemid=PortfolioDashboard"
        f"&portfolioUid={portfolio_uid}"
    )


def securities_transactions_url_for_portfolio(
        portfolio_uid: str, banking_relation_id: str) -> str:
    """Hash route for the securities transaction list.

    Takes the same (portfolioUid, bankingRelationId) pair as
    `positions_url_for_portfolio`, and shows that portfolio: the
    surface's other scopes are reachable only through its chooser."""
    return (
        "#/assets/asset-view/securitiesTransactions"
        f"?bankingRelationId={banking_relation_id}"
        "&navitemid=PortfolioDashboard"
        f"&portfolioUid={portfolio_uid}"
    )


# The portfolio switcher, in the SPA's own header.
#
# This is what moves the surface from one portfolio to another, and it
# is NOT the chooser the list renders into its own HTML: that one is
# the custody-account filter within a portfolio. Two of these sit side
# by side, for the banking relationship and the portfolio; the
# relationship's is read-only where a login holds one, so the portfolio
# is the one that is not disabled.
#
# The class hashes are CSS-modules build output and rotate whenever UBS
# redeploys, so these match on the stable prefix and on the semantics
# beside it, never on a full class (see DOC_FILTER_BUTTON_SELECTOR).
PORTFOLIO_SWITCHER_BUTTON = (
    'button[class*="UWR_ContextSelector"][aria-expanded]'
    ':not([aria-disabled="true"])'
)
# Its options, rendered only while it is open.
PORTFOLIO_SWITCHER_ITEM = '[class*="UWR_ContextSelectorItemTitle_container_"]'


# The filter panel, as live controls.
#
# The window is not a property of any request: the surface keeps one
# period per scope and answers every export for it, and the fields that
# carry it exist only in the panel — a form the SPA builds into a frame
# of its own, absent from the document the endpoint answers with. So it
# is set by driving these, letting the page's own script assemble and
# submit the form.
#
# The panel is found by the presence of its from-field rather than by
# URL: it is a sibling of the list's frame and is addressed by nothing
# the walk already holds.
TXN_FILTER_DATE_FROM = "#dateFrom0"
# "Set manually", against the preset dropdown beside it. Rendered
# already selected, and set anyway: a panel remembering a preset
# ignores both dates.
TXN_FILTER_MANUAL_RADIO = 'input[name="dateRangeRadioButton"][value="0"]'

# The list's own CSV export, in its toolbar. Clicking it is what
# produces the file: the surface answers an export from the state the
# page holds, and a request reconstructing that state is answered with
# the rendered page instead, however faithfully it is built.
TXN_EXPORT_CSV_BUTTON = "img.gj9XLSButton"

# The footer that closes an export. It states the period the answer
# covers, the way the cash CSV's `From:` header does, and is the row
# the parser stops at.
TXN_EXPORT_FOOTER_PREFIX = "Transaction list:"
TXN_EXPORT_FOOTER_RE = re.compile(
    r"from\s+(\d{2}\.\d{2}\.\d{4})\s+to\s+(\d{2}\.\d{2}\.\d{4})",
    re.IGNORECASE,
)
