"""Fidelity web landmarks: the URLs, selectors, timings and limits the
walk and the sign-in drive, as data.

`download.py` imports every name here. The module holds no browser
code. Its helpers are pure: the hashed key an account is filed under,
the list of excluded accounts, and the log label of a charitable API
URL.
"""

from __future__ import annotations

import re
from pathlib import Path

from collectorkit import bronze


# ---------------------------------------------------------------------------
# Portfolio pages and the positions phase
# ---------------------------------------------------------------------------

URL_PORTFOLIO_SUMMARY = "https://digital.fidelity.com/ftgw/digital/portfolio/summary"
URL_POSITIONS = "https://digital.fidelity.com/ftgw/digital/portfolio/positions"
URL_ACTIVITY = "https://digital.fidelity.com/ftgw/digital/portfolio/activity"
URL_DOCUMENTS = "https://digital.fidelity.com/ftgw/digital/portfolio/documents"
URL_BALANCES = "https://digital.fidelity.com/ftgw/digital/portfolio/balances"
URL_PERFORMANCE = "https://digital.fidelity.com/ftgw/digital/portfolio/performance"
POST_AUTH_PREFIX = "https://digital.fidelity.com/ftgw/digital/portfolio/"
# The document center is a separate "Enterprise Document Center" SPA
# off the portfolio host. Navigating to URL_DOCUMENTS 302s there, so
# the documents phase accepts this
# prefix as a valid post-auth landing (it is NOT a session-timeout
# bounce, which would redirect to the /prgw/digital/signin login).
DOCCENTER_PREFIX = "https://digitalservices.fidelity.com/navigate/ent-documentcenter/"

# Account-selector account-link testid pattern is
# ``ap143528-accounts-selector-account-link-<account-id>``, with
# the id being 9-digit for brokerage / trust / 529 and 7-digit
# for a Fidelity Charitable DAF (auto-excluded by length in
# walk()). The pattern is used inline by
# ``enumerate_account_dimensions``; the named selectors below
# cover the rest of the positions phase.
SEL_ALL_ACCOUNTS = ".acct-selector__all-accounts"
SEL_KEBAB_MENU = "[data-testid='kebab-menu']"
# Any account-selector account-link (the enumeration target). Used
# as a hydration proxy when retrying enumeration on the positions
# surface — the links render in the DOM even with the dropdown
# collapsed.
SEL_ACCOUNT_LINK = (
    "[data-testid^='ap143528-accounts-selector-account-link-']"
)
SEL_PRESET_VIEW_SELECT = "[data-testid='preset-views-dropdown'] select"

# Timing.
KEBAB_OPEN_WAIT_S = 2.0
TABLE_RENDER_WAIT_S = 3.0
SPA_HYDRATE_WAIT_S = 5.0
DOWNLOAD_TIMEOUT_MS = 30_000

# Custom-range requests longer than this are bisected into
# MAX_ACTIVITY_WINDOW_DAYS-day windows, one CSV per window.
#
# The activity Custom range is capped at 93 days per export by
# Fidelity, but the windows are cut to 30 — the width of the page's
# OWN default filter ("Past 30 days") — and that is a correctness
# rule, not a politeness one.
#
# The CSV is generated from whatever the table currently holds, and
# the filter's label updates the instant Apply is pressed, well before
# the rows behind it arrive. An export taken in that gap is the
# PREVIOUS filter's data under the new window's name. Asking only for
# windows no wider than the page's own default bounds the damage: a
# stale answer is then a SUPERSET of what was asked for rather than a
# truncation of it, and a superset is recoverable where a truncation
# is a hole nothing reports. A 93-day window has the opposite
# property and can silently return 30 days.
#
# It is a floor under the damage, not the mechanism that prevents it.
# `activity_export_matches` is that: it refuses any export whose rows
# fall outside the window asked for, superset included, and pulls the
# window again. The cap is what makes a run that exhausts its retries
# still land something usable.
MAX_ACTIVITY_WINDOW_DAYS = 30

# How far back an activity backfill reaches when the page publishes no
# date bounds to clamp against — which is the current generation's
# behaviour, since its date inputs carry no min/max. Fidelity's own
# retention on this export has been ~4-5 years; five is the generous
# reading. It matters because `--lookback all` asks for thirty years,
# and every year past retention is a month of identical empty exports.
# The floor can still lie before what Fidelity serves. The page refuses
# those first windows, and `mark_outside_retention` keeps them out of
# the phase's gaps.
ACTIVITY_RETENTION_FLOOR_DAYS = 5 * 366

# The error a window records when the page refuses its Custom range.
ACTIVITY_RANGE_REFUSED = "custom-range not applied"

# The status of a refused window that ends before the first window the
# page accepted (see `mark_outside_retention`).
ACTIVITY_OUTSIDE_RETENTION = "outside-retention"

# How many times a window's export is pulled before its rows are
# accepted, and how long to let the table settle between tries. The
# table lags the filter by one apply, so the second pull is normally
# the one that lands; the third is slack for a slow fetch.
ACTIVITY_EXPORT_ATTEMPTS = 3
ACTIVITY_EXPORT_RETRY_SECONDS = 3.0


# ---------------------------------------------------------------------------
# Login + session
# ---------------------------------------------------------------------------

LANDMARK_TIMEOUT_MS = 60_000

# Canonical ${PREFIX}_ credential envs first, the unprefixed FIDELITY_*
# names second: read FIDELITY_WEB_USERNAME/PASSWORD, falling back to
# FIDELITY_USERNAME/PASSWORD so an existing env file keeps working.
USERNAME_ENVS = ("FIDELITY_WEB_USERNAME", "FIDELITY_USERNAME")
PASSWORD_ENVS = ("FIDELITY_WEB_PASSWORD", "FIDELITY_PASSWORD")

DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/fidelity-web.env"),
    Path.home() / ".secrets" / "fidelity-web.env",
)

SIGNIN_URL = "https://digital.fidelity.com/prgw/digital/signin/"

# Login form selectors. Mirrors the captured DOM; see DESIGN.md §8.2.
# Username field: a <select> on returning devices ("remember my
# username" cookie present), a text <input> on first visit.
SEL_USERNAME_SELECT = "#dom-select-username"
SEL_USERNAME_OTHER_OPTION_VALUE = "default"
# The username input, current generation first. `~=` and not `=` on
# the autocomplete match: the attribute is a SPACE-SEPARATED TOKEN
# LIST (`autocomplete="username webauthn"` today), so an exact-value
# match misses it the moment Fidelity adds a token. That one operator
# would have carried this selector through the rename below on its
# own, which is why it sits ahead of every superseded id.
SEL_USERNAME_TEXT_INPUT_CANDIDATES = (
    "input#dom-username-input",
    "input[autocomplete~=username]",
    "input#userId-input",
    "input[name=userId]",
    "input[aria-labelledby=dom-username-label]",
)
SEL_PASSWORD_INPUT = "#dom-pswd-input"
SEL_LOGIN_BUTTON = "#dom-login-button"

# International Usage Agreement interstitial — Fidelity interposes
# this page for non-US-locale clients (Camoufox geoip=True can trip
# it, depending on the egress IP). The agreement must be accepted
# before the signin form is served. The accept link runs
# javascript:acceptAgreement() which then routes onward.
SEL_IUA_ACCEPT_CANDIDATES = (
    "a.accept-link",
    "a[title='I Accept']",
    "a:has-text('I Accept')",
)

# 2FA selectors.
SEL_2FA_CODE_INPUT = "#dom-totp-security-code-input"
SEL_2FA_SUBMIT_CANDIDATES = (
    "button[type=submit]:has-text('Submit')",
    "button[type=submit]:has-text('Continue')",
    "button[type=submit]:has-text('Verify')",
    "button[type=submit]",
)

# Trust-this-browser checkbox: the <input> is overlaid by a PVD
# styled <label>::before that intercepts pointer events. Clicking
# the label (not the input) is the only reliable activation path.
SEL_TRUST_BROWSER_CHECKBOX = "#dom-trust-device-checkbox"
SEL_TRUST_BROWSER_LABEL = "label[for='dom-trust-device-checkbox']"

# Logout: best-effort. The user menu surfaces a 'Log Out' anchor;
# the run clicks it before the Camoufox context tears down so Fidelity
# invalidates the session cookie server-side. If selectors drift
# the context.close() still cleans up locally — Fidelity's idle
# timeout (~15 min) will eventually invalidate the cookie too.
SEL_LOGOUT_CANDIDATES = (
    "a:has-text('Log Out')",
    "a:has-text('Log out')",
    "button:has-text('Log Out')",
    "[aria-label='Log Out']",
)


# ---------------------------------------------------------------------------
# Accounts
# ---------------------------------------------------------------------------

def account_key(account_external_id):
    """First 16 hex chars of sha256(id) — the on-disk filename
    discriminator per DESIGN §2. Keeps ``ls`` of a bronze dir from
    leaking real Fidelity account numbers."""
    return bronze.short_token(account_external_id)


def parse_exclude_list(value):
    if not value:
        return set()
    return {x.strip() for x in value.split(",") if x.strip()}


# ---------------------------------------------------------------------------
# Open and closed lots — the positions page's read-only lot queries
# ---------------------------------------------------------------------------

POSWEB_API = "https://digital.fidelity.com/ftgw/digital/positions/poswebex/api"
# The only endpoints `_poswebex_post` will call. `state/save`, which
# stores display settings, is deliberately absent.
POSWEB_READ_ENDPOINTS = frozenset({
    "positions", "openlots", "closedpositions", "closedlots"})
LOTS_PAGE_SIZE = 10
# A pause before each lot request, drawn from this range, so a run
# opens tables at a person's pace rather than in a burst. About one
# request in LOT_LONG_PAUSE_EVERY waits LOT_LONG_PAUSE_S instead, the
# way a reader stops to look at a table.
LOT_REQUEST_PAUSE_S = (2.0, 5.0)
LOT_LONG_PAUSE_EVERY = 25
LOT_LONG_PAUSE_S = (20.0, 45.0)
# The most lot tables one run fetches. Past it, the remaining new or
# changed positions are deferred to the next run, so a full refresh
# spreads over several nights instead of sending a thousand requests
# in one session. A nightly's own changes stay well under it.
LOT_MAX_FETCHES_PER_RUN = 200
LOT_CONTEXT_TIMEOUT_S = 60
# Consecutive failed lot requests after which the step stops: by then
# the session has most likely ended, and every further request would
# fail too.
LOT_MAX_CONSECUTIVE_FAILURES = 5

# ---------------------------------------------------------------------------
# Activity & Orders — the time filter and the export popover
# ---------------------------------------------------------------------------

# The activity time filter, in the generations Fidelity has served.
#
# The time-period control is a `<filter-by-time>` button (the `fds-*`
# design system) that expands a panel. The button carries the open
# state on `aria-expanded`, which is the semantic signal and survives
# a renaming of the panel itself. The older inline pill
# (`ap143528-timeperiod-filter`) and its radio probe stay as the
# fallback for a partial rollback.
SEL_TIMEPICKER_PILL = (
    "[data-testid='filter-by-time-button'], "
    "[data-testid='ap143528-timeperiod-filter']"
)
SEL_TIMEPICKER_OPEN = (
    "[data-testid='filter-by-time-button'][aria-expanded='true'], "
    "input#Custom[type='radio'], input#Recent[type='radio']"
)

# The Custom tab's own controls, current generation first. The `fds-*`
# date inputs carry no `min`/`max`, where the generation before them
# did — the bounds probe reads null and simply does not clamp, and the
# retention floor is enforced by the panel's own validation instead
# (`from-date-too-old-error`, `date-range-too-large-error`).
SEL_ACTIVITY_FROM_DATE = "#input-from-date, #customized-timeperiod-from-date"
SEL_ACTIVITY_TO_DATE = "#input-to-date, #customized-timeperiod-to-date"
# Apply is an unlabelled submit button, so it is addressed through the
# form the date inputs sit in rather than by name: both the form id and
# the button id are generated per render.
SEL_ACTIVITY_APPLY = (
    "form:has(#input-from-date) button[type='submit'], "
    "button[aria-label='Apply Customized Time Period']"
)

# The popover's CSV item, current generation first: the custom
# element's own light-DOM id, then the native button by its label.
# `_click_activity_download` tries these before its text-matched
# fallbacks, and the open-wait polls their union — which is derived
# here rather than written out a second time, so the two cannot drift
# apart. Pure CSS on purpose: a `:has-text()` pseudo-class has no
# place in a selector used only to answer "did the popover open".
SEL_ACTIVITY_CSV_SELECTORS = (
    "#download-csv-button",
    "button[aria-label='Download as CSV']",
)
SEL_ACTIVITY_CSV_ITEM = ", ".join(SEL_ACTIVITY_CSV_SELECTORS)

# The time-filter button states the filter currently in force on its
# own label ("Open time filter. Current filter: 01/07/2026 -
# 10/09/2026"). That label is the only positive confirmation the page
# offers that an Apply actually took, and reading it is what stops a
# window from exporting under the WRONG range — see
# `_activity_filter_shows_range`.
SEL_TIMEPICKER_LABEL = "[data-testid='filter-by-time-button']"

# ---------------------------------------------------------------------------
# Enterprise Document Center — the year filter
# ---------------------------------------------------------------------------

SEL_DOCCENTER_TIMEFILTER = "#options-select-TimeFilter"
SEL_DOCCENTER_APPLY = (
    "button.pvd-button--primary:has-text('Apply'), "
    "button:has-text('Apply')"
)

# ---------------------------------------------------------------------------
# Donor-Advised Fund (Fidelity Charitable)
# ---------------------------------------------------------------------------

DAF_HOST = "charitablegift.fidelity.com"
DAF_ORIGIN = f"https://{DAF_HOST}"
# CGFLogon consumes the retail session cookie and redirects into the
# donor SPA — the SSO hop. Navigating it (rather than the SPA URL
# directly) is what plants the charitablegift.fidelity.com session
# cookies that page.request then reuses.
DAF_SSO_URL = f"{DAF_ORIGIN}/cgfweb/CGFLogon.cgfdo?Ref_at=ng"
# The donor SPA the SSO hop lands on. Nothing navigates here directly;
# kept as the pinned record of where the walk stands after the hop.
DAF_SPA_URL = f"{DAF_ORIGIN}/cgfweb/fc-donor/?Ref_at=ng"
DAF_API = f"{DAF_ORIGIN}/fc-services/api/v1"
# The API bootstrap: POST identity/self mints the per-session auth JWT
# (returned in the fid-cgf-auth-jwt response header) and returns the
# donor's partyId. Subsequent GETs carry the JWT as a request header.
DAF_IDENTITY_URL = f"{DAF_API}/identity/self"
DAF_JWT_HEADER = "fid-cgf-auth-jwt"
# Document types the listing endpoint serves (DESIGN.md §12.1). Each is
# queried over the full [establish-date, today] window; every returned
# row is fetched as a PDF via document/download.
DAF_DOCUMENT_TYPES = ("STATEMENT", "GRANT", "CONTRIBUTION", "FORM_8283")
DAF_PAGE_SIZE = 100


def _daf_ep(url):
    """A PII-safe endpoint label for logs. Account / party / grant ids
    ride in BOTH the path (``givingAccounts/<acctNbr>``) and the query
    (``?accountId=<acctNbr>``) of the charitable API, so this strips the
    query, drops the API-root prefix, and masks every long digit run —
    the log shows ``givingAccounts/<id>``, never a real account number."""
    path = url.split("?", 1)[0]
    for prefix in (DAF_API, DAF_ORIGIN):
        if path.startswith(prefix):
            path = path[len(prefix):]
            break
    return re.sub(r"\d{4,}", "<id>", path.lstrip("/")) or path
