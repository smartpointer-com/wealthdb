#!/usr/bin/env python3
"""Q2 `mobilews` wire contract for First Citizens — the browserless core
shared by login.py and download.py.

First Citizens' digital banking is a Q2 white-label platform served at
`digitalbanking.firstcitizens.com/FCBTCOnline/`; all data arrives over a
`mobilews` REST/JSON API (DESIGN.md §3). This module holds the parts that
touch no browser and no network: the endpoint paths, the rule that reads
the `logonUser` response as authenticated / needs-2FA, the deposit-account
filter, and the roster / history projections. Everything here is pure, so
it is unit-tested against synthetic payloads.

The **login itself is not here** — the Akamai Bot Manager sensor on
`preLogonUser`/`logonUser` (DESIGN.md §3) means the logon must run through
the real browser's JS; login.py drives that. Once authenticated, the data
calls carry only the session cookie + a `q2token` header (no sensor), so
download.py replays them over `page.request`.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import date
from urllib.parse import quote

# The app root. All mobilews endpoints hang off `<BASE>/mobilews/…`.
APP_ORIGIN = "https://digitalbanking.firstcitizens.com"
APP_BASE = f"{APP_ORIGIN}/FCBTCOnline"
MOBILEWS = f"{APP_BASE}/mobilews"

# The public entry. The login form lives in a MODAL on this homepage
# (revealed by SEL_LOGIN_TRIGGER); submitting it navigates into the Q2 SPA
# at APP_UUX for the actual logon (DESIGN.md §3).
START_URL = "https://www.firstcitizens.com/"

# The Q2 single-page app (hash-routed): #/login → #/login/interstitial →
# #/landingPage on success, or #/login/mfa/* when a device is not trusted.
APP_UUX = f"{APP_BASE}/uux.aspx"

# Login-form controls, pinned from the explore DOM snapshots (DESIGN.md §3):
# the homepage "Log In" button opens a modal carrying the classic light-DOM
# form; the submit button then drives the SPA logon. (SEL_TAC_ENTRY, the
# Secure Access Code field, is a Q2/Stencil shadow-DOM component used only on
# the untrusted-device MFA path, which is driven by hand over VNC for now —
# §4.2.)
SEL_LOGIN_TRIGGER = "button.fcb-modal__trigger[aria-controls='fcb-modal--login-master']"
SEL_USER = "#login-form-0-id-textfield"
SEL_PASSWORD = "#login-form-0-password-textfield"
SEL_SUBMIT = "button[name='db-login-button']"
SEL_TAC_ENTRY = "#tacEntry"

# SPA hash-route markers for auth detection (never a bare URL match — the
# pre-auth and post-auth pages share the uux.aspx path, only the fragment
# differs). Authenticated once the route reaches the landing page; a 2FA
# challenge shows as a /login/mfa/* route, which walks
# targets → entertarget → register.
URL_MARK_LANDING = "/landingPage"
URL_MARK_MFA = "/login/mfa"
URL_MARK_MFA_ENTER = "/login/mfa/entertarget"
URL_MARK_MFA_REGISTER = "/login/mfa/register"

# Secure Access Code (2FA) screen controls — Q2/Stencil, pinned from the
# 2026-08-13 explore capture. These are light-DOM host elements; Playwright
# pierces the open shadow root for a q2-input's inner `<input>` and a q2-btn's
# inner button. The targets screen renders one btnTacTarget per delivery
# method, its slotted text "Text: …" (SMS) or "Call: …" (voice).
SEL_MFA_TARGET = "q2-btn[test-id='btnTacTarget']"
SEL_MFA_CODE = "#tacEntry"                 # q2-input; inner input via f"{SEL_MFA_CODE} input"
SEL_MFA_SUBMIT = "q2-btn[test-id='btnSubmit']"
SEL_MFA_REGISTER = "q2-btn[test-id='btnRegister']"

# The slotted-text prefix that identifies each delivery button by target kind.
MFA_TARGET_PREFIX = {"sms": "Text:", "voice": "Call:"}


def is_authed_url(url: str) -> bool:
    """True when the SPA hash route indicates a signed-in landing page."""
    return URL_MARK_LANDING in (url or "")


def is_mfa_url(url: str) -> bool:
    """True when the SPA hash route indicates a 2FA challenge (untrusted
    device)."""
    return URL_MARK_MFA in (url or "")

# The CSRF token the SPA sends on every authenticated call, as both a
# `q2token` cookie and a `q2token` request header (DESIGN.md §3). download.py
# harvests the cookie value and echoes it as the header.
Q2TOKEN = "q2token"

# Transaction-export formats the accountExport endpoint offers, mapped
# lower-case handle → the Q2 `<Format>` path segment (all five captured,
# DESIGN.md §3). Csv carries the per-row running balance; QFX_1_0_2 is the
# OFX-family form with a stable id; Qbo duplicates QFX (the chase result),
# and Xls/Ofx are the remaining variants. The default fetch mirrors chase
# (Csv + QFX) — but note the accountHistory JSON already carries both a
# stable id and a running balance, so Phase 3 may not need the exports as a
# transaction source at all (DESIGN.md §4.3). Extensible: pass --format to
# widen.
EXPORT_FORMATS = {
    "csv": "Csv",
    "qfx": "QFX_1_0_2",
    "xls": "Xls",
    "ofx": "Ofx",
    "qbo": "Qbo",
}
DEFAULT_EXPORT_FORMATS = ("csv", "qfx")

# accessCodeTargets[].notificationType (DESIGN.md §3): 3 = SMS text,
# 2 = voice call. Mapped to a human kind for the 2FA dialog.
NOTIFY_SMS = 3
NOTIFY_VOICE = 2
NOTIFY_KIND = {NOTIFY_SMS: "sms", NOTIFY_VOICE: "voice"}

# The deposit-account marker in the roster: extended.hydraProductTypeCode
# == "D" (Deposit). Cards / loans / lines carry other codes and are out of
# scope (CLAUDE.md). Kept as the one place the deposit rule lives.
DEPOSIT_PRODUCT_TYPE_CODE = "D"

# The unit-separator (0x1F) the Q2 API uses inside compound query values —
# in `sort` (field%1Fdirection) and in the `postedDate` range below.
US = "\x1f"


def _mdy(d: date) -> str:
    """A date as `M/D/YYYY` with no leading zeros — the format the Q2
    endpoints expect in the `postedDate` filter."""
    return f"{d.month}/{d.day}/{d.year}"


def posted_date_range(since: date, until: date) -> str:
    """The `postedDate` filter value that narrows `accountHistory` /
    `accountExport` to the inclusive `[since, until]` window: two `M/D/YYYY`
    dates joined by the unit separator, the end carrying an end-of-day time —
    the exact shape the account-detail "Time Period" picker sends (captured
    2026-08-15). Callers pass it to the `posted_date` param of the history /
    export URL builders, which percent-encode it."""
    return f"{_mdy(since)}{US}{_mdy(until)} 23:59:59.999"


# --- endpoint builders ----------------------------------------------------

def pre_logon_url() -> str:
    return f"{MOBILEWS}/preLogonUser"


def logon_url() -> str:
    return f"{MOBILEWS}/logonUser"


def accounts_url() -> str:
    return f"{MOBILEWS}/accounts"


def account_history_url(account_id: str) -> str:
    return f"{MOBILEWS}/accountHistory/{account_id}"


# accountHistory is paginated (DESIGN.md §3): `page[number]` (1-based) +
# `page[size]`, newest first. The SPA sorts by postedDate descending — the
# separator between field and direction is a literal US (0x1F), sent
# percent-encoded as %1F. A page returns `data.transactions` (a list) plus
# `data.transactionCount` (the total) and `data.oldestTransactionDate`. The
# endpoint **narrows server-side** to a `postedDate` range (see
# posted_date_range) — the account-detail "Time Period" / "Custom Date"
# picker sends exactly that (measured 2026-08-15); `--lookback` rides it, and
# `data.transactionCount` reflects the window.
HISTORY_PAGE_SIZE = 100
HISTORY_SORT = "postedDate%1Fd"          # postedDate, descending


def account_history_page_url(account_id: str, page_number: int,
                             page_size: int = HISTORY_PAGE_SIZE,
                             posted_date: str | None = None) -> str:
    url = (f"{account_history_url(account_id)}"
           f"?page[number]={page_number}&page[size]={page_size}"
           f"&sort={HISTORY_SORT}")
    if posted_date:
        url += "&postedDate=" + quote(posted_date)
    return url


def history_data(body: dict) -> dict:
    """The `data` object of an accountHistory response: `transactions` (list),
    `transactionCount` (int total), `oldestTransactionDate` (str). Tolerant of
    a missing/misshaped body."""
    d = _logon_data(body)
    return d if isinstance(d, dict) else {}


def history_transactions(body: dict) -> list:
    """Just the transactions list from an accountHistory page."""
    txs = history_data(body).get("transactions")
    return txs if isinstance(txs, list) else []


def account_export_url(account_id: str, fmt_handle: str,
                       posted_date: str | None = None) -> str:
    """POST endpoint for an activity export in `fmt_handle` (a key of
    EXPORT_FORMATS). A `posted_date` (see posted_date_range) narrows the
    export to that window, exactly as it narrows the history (measured
    2026-08-15). Raises KeyError on an unknown handle."""
    url = f"{MOBILEWS}/accountExport/{account_id}/{EXPORT_FORMATS[fmt_handle]}"
    if posted_date:
        url += "?postedDate=" + quote(posted_date)
    return url


def account_statement_list_url(account_id: str) -> str:
    return f"{MOBILEWS}/accountStatement/{account_id}"


def account_statement_pdf_url(account_id: str, doc_id: str) -> str:
    return f"{MOBILEWS}/accountStatement/{account_id}/{doc_id}/pdf"


# --- logonUser classification --------------------------------------------

@dataclass(frozen=True)
class AccessCodeTarget:
    """One 2FA destination. From `accessCodeTargets` (logonUser) it is a
    masked delivery method; login.py's terminal 2FA reads the same shape off
    the on-screen delivery buttons, keying `value` to the button index.
    `display` is the label the human sees, `kind` one of 'sms'/'voice'/
    'other'."""
    value: str
    display: str
    kind: str


@dataclass(frozen=True)
class LogonOutcome:
    """The `logonUser` response reduced to a decision (DESIGN.md §3):
    - `authenticated` — HTTP 200, no `accessCodeTargets`: the trusted-device
      path, straight into the app.
    - `needs_2fa` — HTTP 203 with `accessCodeTargets`: a step-up challenge.
    Exactly one is true on a well-formed response; both false means an
    unexpected shape the caller surfaces as an error."""
    authenticated: bool
    needs_2fa: bool
    targets: tuple[AccessCodeTarget, ...] = field(default_factory=tuple)
    status: int = 0


def parse_access_code_targets(body: dict) -> tuple[AccessCodeTarget, ...]:
    """Reduce a `logonUser` (or standalone) body's `accessCodeTargets` to
    typed targets. Tolerant of missing fields — an absent list yields ()."""
    data = _logon_data(body)
    raw = data.get("accessCodeTargets") or ()
    out = []
    for t in raw:
        ntype = t.get("notificationType")
        out.append(AccessCodeTarget(
            value=str(t.get("value", "")),
            display=str(t.get("display", "")),
            kind=NOTIFY_KIND.get(ntype, "other"),
        ))
    return tuple(out)


def classify_logon(status: int, body: dict) -> LogonOutcome:
    """Read a `logonUser` response into a LogonOutcome (DESIGN.md §3).

    A 203 with at least one `accessCodeTargets` entry is a 2FA challenge; a
    200 with no targets and a populated `userProfileData` is an
    authenticated trusted-device login. Anything else (e.g. a 200 that still
    lists targets, or a non-2xx) is neither — the caller decides how to
    surface it. Never raises on shape."""
    targets = parse_access_code_targets(body)
    data = _logon_data(body)
    if status == 203 and targets:
        return LogonOutcome(authenticated=False, needs_2fa=True,
                            targets=targets, status=status)
    if status == 200 and not targets and data.get("userProfileData") is not None:
        return LogonOutcome(authenticated=True, needs_2fa=False, status=status)
    # Some builds return 200 with a populated profile and no explicit
    # userProfileData key on re-auth; treat a 200 with no targets as authed
    # only when nothing signals a pending challenge.
    if status == 200 and not targets:
        return LogonOutcome(authenticated=True, needs_2fa=False, status=status)
    return LogonOutcome(authenticated=False, needs_2fa=bool(targets),
                        targets=targets, status=status)


def _logon_data(body: dict) -> dict:
    """The `data` object Q2 wraps object responses in (logonUser, account
    detail), or the body itself if it isn't wrapped."""
    if not isinstance(body, dict):
        return {}
    data = body.get("data")
    return data if isinstance(data, dict) else body


def _data_list(body: dict) -> list:
    """The `data` array Q2 wraps list responses in (accounts roster,
    statement listing, transaction history), or () if absent/misshaped."""
    if not isinstance(body, dict):
        return []
    data = body.get("data")
    return data if isinstance(data, list) else []


# --- roster ---------------------------------------------------------------

def is_deposit_account(acct: dict) -> bool:
    """True for a deposit account (checking / savings), the only kind in
    scope (CLAUDE.md). Keyed on extended.hydraProductTypeCode == 'D'."""
    ext = acct.get("extended") or {}
    return str(ext.get("hydraProductTypeCode", "")).upper() == DEPOSIT_PRODUCT_TYPE_CODE


def parse_accounts(body: dict, *, deposit_only: bool = True) -> list[dict]:
    """Project the `accounts` roster to the fields download.py needs, keeping
    only deposit accounts by default. Each row: `id` (the endpoint key),
    `account_external_id` (the bank-masked number), `product_type_name`
    (Checking / Savings), `nickname`, and the raw `balances` labels/values as
    the source gives them. Pure — tested on synthetic payloads (no real
    numbers or balances)."""
    rows = []
    for acct in _data_list(body):
        if not isinstance(acct, dict):
            continue
        if deposit_only and not is_deposit_account(acct):
            continue
        ext = acct.get("extended") or {}
        rows.append({
            "id": str(acct.get("id", "")),
            "account_external_id": str(ext.get("accountNumberInternal")
                                       or acct.get("accountNumber") or ""),
            "product_type_name": str(ext.get("productTypeName", "")),
            "product_name": str(ext.get("productName", "")),
            "nickname": str(ext.get("nickName", "")),
            "hydra_product_type_code": str(ext.get("hydraProductTypeCode", "")),
            "balances": _balances(ext),
        })
    return rows


def _balances(ext: dict) -> list[dict]:
    """The up-to-three labelled balances the roster carries (Available /
    Current / …), as `{description, value}` pairs, dropping empty slots."""
    out = []
    for i in (1, 2, 3):
        desc = str(ext.get(f"balanceDescription{i}", "") or "")
        val = str(ext.get(f"balance{i}", "") or "")
        if desc or val:
            out.append({"description": desc, "value": val})
    return out


def parse_statement_list(body: dict) -> list[dict]:
    """Project an `accountStatement/<id>` listing to `{period, doc_id}` rows
    (DESIGN.md §3): `period` is the statement date (MM/DD/YYYY), `doc_id` the
    value the PDF endpoint takes. Newest first, as the source returns them."""
    rows = []
    for s in _data_list(body):
        if not isinstance(s, dict):
            continue
        doc_id = str(s.get("value", ""))
        if not doc_id:
            continue
        rows.append({"period": str(s.get("period", "")), "doc_id": doc_id})
    return rows
