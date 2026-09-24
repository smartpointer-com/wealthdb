"""
Parser for the SVB Private Bank deposit and mortgage statement PDFs.

The second and third document families in this archive. Unlike the
brokerage statements (``pdf_parsers_svbwa``) these carry **no text
layer at all** — every extractor returns zero characters, because
they are print-stream renderings rastered to a single image per
page. They are read by OCR instead
(``collectorkit.pdf.extract_text_ocr``), which makes this parser's
input layout-ordered lines rebuilt from bounding boxes rather than a
PDF's own text. Either recogniser is a fixed local model, so the same
bytes yield the same lines on every run and the parse cache can
memoise the result — but the two do not read a page identically, which
is why nothing here trusts the characters without the statement's own
arithmetic agreeing, and why anything this parser turns away — an
unrecognised title, a registration that did not match, a section whose
sums did not close — is retried at a finer raster before the refusal
is believed.

Three forms live here, told apart by their page-1 title:

* **Deposit statement** — ``Checking Account Statement`` (one
  account) or ``Combined Account Statement`` (checking and savings
  in one document, each with its own section). Both print, per
  account:
  ::

      Account Number: NNNNNNNNNN
      Balance Summary
      Beginning Balance as of MM-DD-YY $X  Ending Balance as of MM-DD-YY $Y
      (+) Deposits        $D    Average Statement Balance: …
      (+) Interest Paid   $I    Annual Percentage Yield Earned: …
      (-) Withdrawals     $W
      (-) Service Charges $C
      TRANSACTION DETAIL[: <Account Name>]
      Date Description Deposit Withdrawal Balance
      MM-DD <description> $amount $running-balance

  ``beginning + D + I − W − C`` must equal the stated ending
  balance; every ledger row's running balance must equal the
  previous one plus that row's amount; the last row's balance must
  land on the stated ending balance; and the rows must sort into
  the same four buckets the summary states, each to the cent. All
  four are checked, and a section that fails any is reported as
  unreadable rather than half-trusted: this is OCR, and the
  statement's own arithmetic is the only thing that can tell a
  misread digit from a real one. Withdrawals print ``$-1,234.56``;
  deposits print plain.

  The fourth check is what lets a row carry a KIND and not merely
  a direction. The summary splits four ways, so the ledger does
  too: a credit whose description says interest is interest
  income, a debit whose description says service charge is a fee,
  and what is left is capital in and out. Wording alone would be a
  guess — but wording the summary's own four totals then confirm
  is not, and a statement whose wording drifted refuses rather
  than booking its interest as capital.

  Those checks are also what lets the ledger and summary patterns be
  LIBERAL about what OCR does to a character. A ``-`` read as
  ``:``, a lost closing paren, a token or two of page-edge furniture
  at the head of a line — each is accepted, because a line misread
  as a row, a summary figure, or the ledger's start or end fails one
  of the four checks above. Strictness belongs in the arithmetic,
  where it can tell right from wrong, not in the pattern, where it
  can only tell familiar from unfamiliar.

* **Mortgage statement** — ``Mortgage Loan Statement``, one loan.
  Prints ``Outstanding Principal``, an interest rate, a
  ``Transaction Activity`` ledger and a ``Past Payment Breakdown``
  of principal and interest, last month and year to date. The
  principal and the breakdown are read; the ledger is not, because
  the deposit account the loan is paid from already books the
  outflow, and booking it again against the loan would double-count
  the payment.

* **Annual loan statement** — ``ANNUAL LOAN STATEMENT FOR <year>``,
  a dot-matrix form whose digits do not survive OCR (decimal points
  and thousands separators are lost). Recognised so it is counted
  rather than mistaken for an unreadable mortgage statement, and
  not parsed: everything on it is already on the monthly statements
  in a legible form.

The text-level parsers (``classify_ocr_text``,
``parse_deposit_statement``, ``parse_mortgage_statement``) are pure
functions of strings, exercised by unit tests against synthetic
fixtures. ``parse_svbdep_statement_pdf(path, expected_signatures=…)``
is the orchestration entry-point; it OCRs the PDF and returns the
same dict shape the brokerage parser does, so one loader consumes
both.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from pdf_parsers_svbwa import FAMILY_UNKNOWN
from statement_tokens import iso_from_short_date, parse_money

# Document families this parser recognises. `unknown` is shared with
# the text-layer pass — it is the same disposition either side of OCR.
FAMILY_DEPOSIT = "deposit"
FAMILY_MORTGAGE = "mortgage"
FAMILY_ANNUAL_LOAN = "annual_loan"

# The description each family's single position row carries. Both are the
# row's identity in `historical_position_snapshots`, whose primary key
# includes it, and gold classifies on it, so they must stay stable.
CASH_DESC = "CASH BALANCE"
LOAN_DESC = "MORTGAGE PRINCIPAL"

# Deposit ledger rows are all one section — the statement prints one
# transaction detail per account and no verb column at all. What a row IS
# comes from the column its figure lands in and the wording beside it,
# and holds only because the Balance Summary confirms the split.
SECTION_LEDGER = "deposit_ledger"

# Page-1 titles. The deposit family prints one of two, depending on
# whether the document covers one account or several.
_DEPOSIT_TITLE_RE = re.compile(
    r"\b(?:Checking|Savings|Combined)\s+Account\s+Statement\b", re.IGNORECASE)
_MORTGAGE_TITLE_RE = re.compile(r"\bMortgage\s+Loan\s+Statement\b", re.IGNORECASE)
_ANNUAL_LOAN_TITLE_RE = re.compile(r"\bANNUAL\s+LOAN\s+STATEMENT\s+FOR\b")


def classify_ocr_text(text):
    """Return the document family of an OCRed statement, or
    ``unknown``. The annual form is checked first: it names a loan
    too, and only its own title tells it from a monthly one."""
    if _ANNUAL_LOAN_TITLE_RE.search(text):
        return FAMILY_ANNUAL_LOAN
    if _MORTGAGE_TITLE_RE.search(text):
        return FAMILY_MORTGAGE
    if _DEPOSIT_TITLE_RE.search(text):
        return FAMILY_DEPOSIT
    return FAMILY_UNKNOWN


# ============================================================
# Shared token shapes
# ============================================================

# A money column. The deposit ledger signs a withdrawal inline
# ("$-1,234.56"); the parenthesised spelling the brokerage statements
# use is tolerated rather than turned away, since parse_money reads
# either and a figure this pattern rejected would read as missing.
_MONEY = r"\(?\$-?[\d,]*\d(?:\.\d{2})?\)?"

# A date's separator, as widely as this recognition returns it. The
# statements print a hyphen or a slash, and either comes back as a
# colon or a dot often enough that a pattern insisting on one reads a
# perfectly good date as no date at all. Nothing is loosened by taking
# all four: every date captured here goes through `iso_from_short_date`,
# which is what decides whether the token names a real day.
_DATE_SEP = r"[-/.:]"
_SHORT_DATE = rf"\d{{2}}{_DATE_SEP}\d{{2}}{_DATE_SEP}\d{{2}}"

# Statement period. A deposit statement prints a range, "Statement from
# ... to ..."; a mortgage one prints a single "Statement Date:" and no
# range of its own. Both take the shared short-date shape rather than a
# literal separator: the two families are printed with different ones,
# and the recogniser is free to lose either.
_PERIOD_RE = re.compile(
    rf"Statement\s+from\s+(?P<start>{_SHORT_DATE})\s+to\s+"
    rf"(?P<end>{_SHORT_DATE})")
_STATEMENT_DATE_RE = re.compile(
    rf"Statement\s+Date:?\s+(?P<date>{_SHORT_DATE})")

def parse_statement_period(text):
    """Return ``(start, end)`` ISO dates for a deposit statement, or
    ``(None, None)`` when the header is unreadable."""
    m = _PERIOD_RE.search(text)
    if not m:
        return None, None
    start, end = iso_from_short_date(m["start"]), iso_from_short_date(m["end"])
    if start is None or end is None:
        return None, None
    return start, end


def parse_statement_date(text):
    """Return the ISO ``Statement Date:`` of a mortgage statement, or
    ``None``."""
    m = _STATEMENT_DATE_RE.search(text)
    if not m:
        return None
    return iso_from_short_date(m["date"])


# ============================================================
# Deposit statements
# ============================================================

# Each account's section opens on its own account-number line and
# runs to the next one. The number is re-stamped in the page header
# too, which is harmless: a repeat of the same id extends its section
# rather than starting another.
_ACCOUNT_NUMBER_RE = re.compile(r"Account\s+Number:\s*(?P<acct>\d{10})\b")

# The Balance Summary block. The beginning and ending balances share
# one line; the four movement lines each carry their figure first,
# ahead of an unrelated statistic in the column beside it.
_BEGIN_END_RE = re.compile(
    rf"Beginning\s+Balance\s+as\s+of\s+(?P<start>{_SHORT_DATE})\s+"
    rf"(?P<begin>{_MONEY})\s+Ending\s+Balance\s+as\s+of\s+"
    rf"(?P<end>{_SHORT_DATE})\s+(?P<ending>{_MONEY})")
# The sign marker is "(+)" or "(-)", whose closing paren OCR
# sometimes drops.
_PLUS = r"\(\s*\+\s*\)?"
_MINUS = r"\(\s*-\s*\)?"
# Page-edge furniture — a stray mark, a digit, half a routing number, a
# form code — that the recogniser reads onto the head of a line. Up to
# two such tokens are skipped. The skip is lazy, taking as few tokens as
# let the rest of the pattern match, so a ledger row whose description
# opens on a date keeps its own; and it is bounded, because a longer run
# is prose: three words ahead of a date are a sentence, not a margin.
# Every anchor that opens, closes, dates or totals a ledger takes it,
# safely for the reason the module docstring gives. The beginning-and-
# ending balance line is searched anywhere on its line instead: its two
# labelled dates and two figures cannot occur in prose.
_NOISE = r"(?:\S+\s+){0,2}?"
_SUMMARY_LINES = (
    ("deposits",
     re.compile(rf"^{_NOISE}{_PLUS}\s*Deposits\s+(?P<amt>{_MONEY})")),
    ("interest",
     re.compile(rf"^{_NOISE}{_PLUS}\s*Interest\s+Paid\s+(?P<amt>{_MONEY})")),
    ("withdrawals",
     re.compile(rf"^{_NOISE}{_MINUS}\s*Withdrawals\s+(?P<amt>{_MONEY})")),
    ("charges",
     re.compile(rf"^{_NOISE}{_MINUS}\s*Service\s+Charges\s+"
                rf"(?P<amt>{_MONEY})")),
)

# How the ledger spells those same four buckets in a row's own
# description, kept beside the summary wording each one has to
# reconcile against. Only two need naming: a credit is interest or it
# is a deposit, a debit is a service charge or it is a withdrawal.
# Both stay NARROW on purpose — a wide pattern would sweep a
# counterparty name into the wrong bucket, and the reconciliation below
# would then refuse a section that read perfectly.
_LEDGER_INTEREST_RE = re.compile(r"\bInterest\b", re.IGNORECASE)
_LEDGER_CHARGE_RE = re.compile(r"\bService\s+Charge", re.IGNORECASE)

# The verb each bucket books under. These are keys of the loader's verb
# map, which is what carries the distinction downstream: interest has to
# arrive as income and a service charge as a fee, or the account's whole
# economic return — which for a deposit account IS the interest — is
# booked as capital moving in and out and nets to nothing.
_VERB_BY_BUCKET = {
    "deposits": "DEPOSIT",
    "interest": "INTEREST",
    "withdrawals": "WITHDRAWAL",
    "charges": "FEE PAID",
}

# A ledger row: MM-DD, a description, the signed amount, the running
# balance, behind at most the furniture `_NOISE` skips. Continuation
# lines carrying the rest of a description have no leading date and
# are skipped. So are the ledger's own
# Beginning/Ending Balance rows, which repeat the summary rather than
# moving money: they print one money column, not the amount-and-balance
# pair this asks for, and so never match. The row anchor takes the
# same separators the header dates do; what the token then MEANS is
# statement_tokens' business.
_LEDGER_ROW_RE = re.compile(
    rf"^{_NOISE}(?P<date>\d{{2}}{_DATE_SEP}\d{{2}})\s+(?P<rest>\S.*?)\s+"
    rf"(?P<amount>{_MONEY})\s+(?P<balance>{_MONEY})\s*$")
# Opens the ledger, and re-opens it on each page it continues onto.
_LEDGER_START_RE = re.compile(rf"^{_NOISE}TRANSACTION\s+DETAIL\b")
# The reconciliation worksheet printed under the ledger, and the
# statement's legend, both of which are past the last row.
_LEDGER_END_RE = re.compile(rf"^{_NOISE}(?:CHECKS OUTSTANDING|ACCOUNT SUMMARY)\b")

# A section whose arithmetic misses by more than this is not trusted.
CENT = 0.005


@dataclass
class DepositLedgerRow:
    """One line of a deposit account's transaction detail."""
    date: str                 # ISO YYYY-MM-DD
    description: str
    amount: float             # signed: deposits positive, withdrawals negative
    balance: float            # running balance the statement prints
    bucket: str               # the Balance Summary figure it is part of
    ordinal: int

    @property
    def verb(self):
        """The loader's verb for this row's bucket. Only meaningful on a
        section whose buckets reconciled — a section that did not
        contributes no rows at all."""
        return _VERB_BY_BUCKET[self.bucket]


@dataclass
class DepositAccount:
    """One account's section of a deposit statement.

    ``error`` is ``None`` when the section's own arithmetic closed —
    the summary to the cent, every running balance to the cent, and
    the rows' four bucket totals to the cent — and names the failure
    otherwise. A caller books nothing from a section carrying an
    error: an OCRed digit that is wrong looks exactly like one that
    is right until the statement's own sums disagree.
    """
    account_external_id: str
    beginning_balance: float
    ending_balance: float
    deposits: float
    interest: float
    withdrawals: float
    charges: float
    rows: list
    error: str | None


def parse_deposit_statement(text):
    """Parse every account section of a deposit statement.

    Returns ``(accounts, period_start, period_end)`` with one
    :class:`DepositAccount` per account, in document order.
    """
    start, end = parse_statement_period(text)
    lines = [ln.strip() for ln in text.splitlines()]
    out = []
    for account_id, section in _account_sections(lines):
        out.append(_parse_deposit_account(account_id, section, end))
    return out, start, end


def _account_sections(lines):
    """Split the lines into ``(account id, lines)`` runs, one per
    account. The account-number line is re-stamped in each page
    header, so consecutive runs of the same id are one section."""
    marks = []
    for i, line in enumerate(lines):
        m = _ACCOUNT_NUMBER_RE.search(line)
        if m and (not marks or marks[-1][1] != m["acct"]):
            marks.append((i, m["acct"]))
    sections = []
    for n, (start, account_id) in enumerate(marks):
        stop = marks[n + 1][0] if n + 1 < len(marks) else len(lines)
        sections.append((account_id, lines[start:stop]))
    return sections


def _parse_deposit_account(account_id, lines, period_end):
    summary = _parse_balance_summary(lines)
    if summary is None:
        return DepositAccount(account_id, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, [],
                              "balance summary unreadable")
    rows, row_error = _parse_ledger(
        lines, period_end, summary["beginning_balance"],
        summary["ending_balance"])
    account = DepositAccount(
        account_external_id=account_id,
        beginning_balance=summary["beginning_balance"],
        ending_balance=summary["ending_balance"],
        deposits=summary["deposits"],
        interest=summary["interest"],
        withdrawals=summary["withdrawals"],
        charges=summary["charges"],
        rows=rows,
        error=None,
    )
    account.error = (_summary_error(account) or row_error
                     or _split_error(account))
    return account


def _parse_balance_summary(lines):
    """Read the Balance Summary block, or ``None`` when the beginning
    and ending balances did not come through. The four movement
    figures default to zero: the layout omits a line it has nothing
    for, and the summary check is what catches a genuinely lost one."""
    found = {}
    for line in lines:
        m = _BEGIN_END_RE.search(line)
        if m and "beginning_balance" not in found:
            begin, ending = parse_money(m["begin"]), parse_money(m["ending"])
            if begin is not None and ending is not None:
                found["beginning_balance"] = begin
                found["ending_balance"] = ending
            continue
        for key, pattern in _SUMMARY_LINES:
            if key in found:
                continue
            hit = pattern.match(line)
            if hit:
                amount = parse_money(hit["amt"])
                if amount is not None:
                    found[key] = amount
                break
    if "beginning_balance" not in found:
        return None
    for key, _pattern in _SUMMARY_LINES:
        found.setdefault(key, 0.0)
    return found


def _summary_error(account):
    """The statement's own arithmetic: beginning plus what came in,
    less what went out, is the ending balance it states."""
    expected = (account.beginning_balance + account.deposits
                + account.interest - account.withdrawals - account.charges)
    delta = round(expected - account.ending_balance, 2)
    if abs(delta) < CENT:
        return None
    return (f"balance summary does not close: "
            f"{expected:.2f} vs stated {account.ending_balance:.2f}")


def _ledger_bucket(description, amount):
    """Which of the summary's four figures a ledger row belongs to,
    read off the wording the statement itself uses. The sign narrows it
    to two, and the description picks between them."""
    if amount > 0:
        if _LEDGER_INTEREST_RE.search(description):
            return "interest"
        return "deposits"
    if _LEDGER_CHARGE_RE.search(description):
        return "charges"
    return "withdrawals"


def _split_error(account):
    """The statement's own arithmetic one level finer: each bucket the
    rows sorted into must total the figure the summary states for it.

    This is what makes the kinds trustworthy rather than a reading of
    the wording. A description that drifted — a charge the statement
    stopped calling one, an interest credit under a name this does not
    know — lands the row in the wrong bucket, and the bucket it landed
    in stops matching. Refusing there is the point: falling back to a
    plain deposit or withdrawal would book the interest as capital on
    exactly the statements whose wording moved, which is the failure
    nothing downstream could see.
    """
    totals = dict.fromkeys(_VERB_BY_BUCKET, 0.0)
    for row in account.rows:
        # The summary states all four as positive magnitudes; the ledger
        # signs the two that leave.
        totals[row.bucket] += abs(row.amount)
    for bucket in _VERB_BY_BUCKET:
        # A bucket is named for the summary field it answers to, which is
        # what lets the two be compared without a second table.
        stated = getattr(account, bucket)
        if abs(round(totals[bucket] - stated, 2)) >= CENT:
            return (f"ledger {bucket} total {totals[bucket]:.2f} is not the "
                    f"stated {stated:.2f}")
    return None


def _parse_ledger(lines, period_end, beginning_balance, ending_balance):
    """Read the transaction detail, checking each row against the
    running balance the statement prints beside it.

    Returns ``(rows, error)``. The chain is the structural check: a
    row whose balance does not equal the one above it plus its own
    amount has had a digit, a column, or a whole row misread. Where
    it lands matters too — a row dropped off the END breaks nothing
    downstream, so the last balance has to reach the stated ending
    one.

    An unreadable statement period refuses the ledger outright. Every
    row is dated ``MM-DD`` and takes its year from the period, so
    without one there is no ledger to check — and a silently empty one
    would pass both checks on any month whose movements net flat, which
    is a dormant account: real rows would vanish with no signal at all.
    """
    if not period_end:
        return [], "statement period unreadable"
    year = int(period_end[:4])
    rows = []
    running = beginning_balance
    in_ledger = False
    for line in lines:
        if _LEDGER_START_RE.match(line):
            in_ledger = True
            continue
        if not in_ledger:
            continue
        if _LEDGER_END_RE.match(line):
            in_ledger = False
            continue
        m = _LEDGER_ROW_RE.match(line)
        if not m:
            continue
        amount, balance = parse_money(m["amount"]), parse_money(m["balance"])
        if amount is None or balance is None:
            continue
        when = iso_from_short_date(m["date"], year=year)
        if when is None:
            continue
        if abs(round(running + amount - balance, 2)) >= CENT:
            return rows, f"running balance breaks at {when}"
        running = balance
        description = " ".join(m["rest"].split())
        rows.append(DepositLedgerRow(
            date=when,
            description=description,
            amount=amount,
            balance=balance,
            bucket=_ledger_bucket(description, amount),
            ordinal=len(rows),
        ))
    if abs(round(running - ending_balance, 2)) >= CENT:
        return rows, (f"ledger ends at {running:.2f}, "
                      f"stated ending balance {ending_balance:.2f}")
    return rows, None


# ============================================================
# Mortgage statements
# ============================================================

_OUTSTANDING_RE = re.compile(
    rf"Outstanding\s+Principal\s+(?P<amt>{_MONEY})")
_LOAN_NUMBER_RE = re.compile(
    r"(?:Account\s+Number|MORTGAGE\s+LOAN\s+NO\.?)\s*:?\s*(?P<acct>\d{10})\b")
# The principal / interest split, last month and year to date. Anchored
# exactly, unlike the deposit ledger's lines: no sum checks these two
# figures, so a looser anchor would be a guess nothing catches.
_BREAKDOWN_RE = re.compile(
    rf"^(?P<label>Principal|Interest)\s+(?P<month>{_MONEY})\s+(?P<ytd>{_MONEY})\s*$")


@dataclass
class MortgageStatement:
    """One monthly mortgage statement.

    ``outstanding_principal`` is the balance the statement states, as
    a POSITIVE number — the loader is what signs it, because how a
    liability is carried is a silver-shape decision, not something
    the statement says.
    """
    account_external_id: str
    statement_date: str
    outstanding_principal: float
    breakdown: dict
    error: str | None


def parse_mortgage_statement(text):
    """Parse a monthly mortgage statement. Always returns a
    :class:`MortgageStatement`; ``error`` names what was unreadable
    when the loan number, date, or balance did not come through."""
    lines = [ln.strip() for ln in text.splitlines()]
    account = _first_group(lines, _LOAN_NUMBER_RE, "acct")
    when = parse_statement_date(text)
    principal = parse_money(_first_group(lines, _OUTSTANDING_RE, "amt"))

    breakdown = {}
    for line in lines:
        m = _BREAKDOWN_RE.match(line)
        if m:
            key = m["label"].lower()
            breakdown.setdefault(key, {
                "paid_last_month": parse_money(m["month"]),
                "paid_year_to_date": parse_money(m["ytd"]),
            })

    missing = [name for name, value in
               (("loan number", account), ("statement date", when),
                ("outstanding principal", principal)) if value is None]
    return MortgageStatement(
        account_external_id=account or "",
        statement_date=when or "",
        outstanding_principal=principal if principal is not None else 0.0,
        breakdown=breakdown,
        error=("unreadable: " + ", ".join(missing)) if missing else None,
    )


def _first_group(lines, pattern, group):
    for line in lines:
        m = pattern.search(line)
        if m:
            return m[group]
    return None


# ============================================================
# PDF orchestration
# ============================================================

# Raster resolution to retry a document at when a gate turns it away at
# the default one. A refusal is a signal that the recognition missed
# something, and a finer raster is the cheapest thing to try; it is
# only ever paid on a document that failed, and at most once. The
# recognisers differ here — Apple's Vision reads this archive at the
# default scale, RapidOCR needs the finer one for the worst of the
# 1-bit scans — so the escalation is what makes one parser serve both.
OCR_RETRY_SCALE = 5


def parse_svbdep_statement_pdf(path, *, expected_signatures=()):
    """OCR one deposit or mortgage statement and return the same dict
    shape the brokerage parser does, so one loader consumes both::

        {
            "path": …, "family": "deposit" | "mortgage" | …,
            "period_start": …, "period_end": …,
            "accounts": [{"account_external_id": …,
                          "holdings": [...], "activity": [...],
                          "_error": … | absent}],
        }

    A deposit account contributes a single cash holding valued at the
    ending balance the statement states, and its ledger becomes
    activity rows, each under the verb its Balance Summary bucket
    implies. A mortgage statement contributes one position row
    at MINUS the outstanding principal, carrying the principal /
    interest split, and no activity, because the deposit account the
    loan is paid from already books the payment. A deposit section
    whose arithmetic did not close, or a mortgage statement whose loan
    number, date or principal did not come through, carries an
    ``_error`` and no rows at all, so a misread can only ever cost
    coverage, never insert a wrong number.

    ``expected_signatures`` guards the document the same way the
    brokerage parser's does — the registration is printed on these
    statements too, and OCR is what makes it readable.

    Every one of the three gates below — the title, the registration,
    the sections' own arithmetic — escalates to the finer raster
    before it gives up, because all three read the same degraded page
    and none of them can tell a page it should refuse from one the
    recognition simply missed. Only the first gate to fail pays for
    that pass, and the ones after it work from the text it produced,
    so a document is rastered at most twice however many gates it
    trips.
    """
    from collectorkit.pdf import OCR_SCALE, extract_text_ocr

    can_escalate = OCR_RETRY_SCALE > OCR_SCALE
    finer = None

    def escalate():
        nonlocal finer
        if finer is None:
            finer = extract_text_ocr(path, scale=OCR_RETRY_SCALE)
        return finer

    text = extract_text_ocr(path)
    at_finer = False

    family = classify_ocr_text(text)
    if family == FAMILY_UNKNOWN and can_escalate:
        # Only the unknown disposition is worth a second pass: every
        # other family is a title that was read, and the annual form is
        # recognised precisely so it can be left unparsed. Everything
        # reaching this parser is already known to be a scan from this
        # archive, so an unread title is far likelier than a foreign PDF.
        finer_text = escalate()
        finer_family = classify_ocr_text(finer_text)
        if finer_family != FAMILY_UNKNOWN:
            family, text, at_finer = finer_family, finer_text, True
    if family not in (FAMILY_DEPOSIT, FAMILY_MORTGAGE):
        return {"path": str(path), "family": family, "accounts": []}

    if expected_signatures and not any(s in text for s in expected_signatures):
        # An exact substring match of a registration line against OCR
        # output, so one lost glyph anywhere in it fails the whole
        # document — and it is then counted as a misfiled PDF rather
        # than as the recognition miss it usually is.
        recovered = (can_escalate and not at_finer
                     and any(s in escalate() for s in expected_signatures))
        if not recovered:
            return {
                "_error": "signature-mismatch",
                "family": family,
                "path": str(path),
            }
        text, at_finer = escalate(), True

    parsed = _result_for(path, family, text)
    if _refused(parsed) and can_escalate and not at_finer:
        retry = _result_for(path, family, escalate())
        if _refused(retry) < _refused(parsed):
            parsed = retry
    if not parsed["accounts"]:
        # A recognised statement with no sections at all. The account
        # headings the split keys on were lost, so every arithmetic gate
        # below them passed on nothing — the document would be dropped
        # while the census counted it as cleanly parsed, and the parse
        # cache would memoise that. Name it instead, in the shape the
        # signature gate above uses, so the build reports it and declines
        # to cache it.
        return {"_error": "no-account-sections",
                "family": family,
                "path": str(path)}
    return parsed


def _result_for(path, family, text):
    if family == FAMILY_DEPOSIT:
        return _deposit_result(path, text)
    return _mortgage_result(path, text)


def _refused(parsed):
    """How many of a document's account sections its own arithmetic
    would not accept.

    A recognised statement with NO sections counts as one refusal rather
    than as nothing to refuse. Its account headings are what the section
    split keys on, so losing them to the raster yields an empty document
    that every arithmetic gate then passes vacuously — the whole
    statement would be dropped while being counted as clean, which is
    the one outcome this parser's gates exist to prevent. Counting it
    earns the finer raster, and names it unreadable if that fails too.
    """
    accounts = parsed.get("accounts", [])
    if not accounts and parsed.get("family") in (FAMILY_DEPOSIT, FAMILY_MORTGAGE):
        return 1
    return sum(1 for acct in accounts if acct.get("_error"))


def _deposit_result(path, text):
    accounts, start, end = parse_deposit_statement(text)
    out = []
    for account in accounts:
        entry = {
            "account_external_id": account.account_external_id,
            "holdings": [],
            "activity": [],
            "activity_totals": {},
        }
        if account.error:
            entry["_error"] = account.error
        else:
            entry["holdings"] = [{
                "description": CASH_DESC,
                "instrument_key": None,
                "quantity": None,
                "price": None,
                "market_value": account.ending_balance,
                "cost_basis": None,
                "unrealized_gain": None,
            }]
            entry["activity"] = [{
                "date": row.date,
                "section": SECTION_LEDGER,
                "account_type": "",
                "verb": row.verb,
                "description": row.description,
                "quantity": None,
                "amount": row.amount,
                "ordinal": row.ordinal,
            } for row in account.rows]
        out.append(entry)
    return {
        "path": str(path),
        "family": FAMILY_DEPOSIT,
        "period_start": start,
        "period_end": end,
        "accounts": out,
    }


def _mortgage_result(path, text):
    loan = parse_mortgage_statement(text)
    entry = {
        "account_external_id": loan.account_external_id,
        "holdings": [],
        "activity": [],
        "activity_totals": {},
    }
    if loan.error:
        entry["_error"] = loan.error
    else:
        entry["holdings"] = [{
            "description": LOAN_DESC,
            "instrument_key": None,
            "quantity": None,
            "price": None,
            # Carried NEGATIVE: the fidelity adapter passes market_value
            # through unchanged, so a liability has to arrive signed or it
            # reads as an asset of the same size.
            "market_value": -loan.outstanding_principal,
            "cost_basis": None,
            "unrealized_gain": None,
            # The principal / interest split, which is the one thing these
            # statements know that the paying account's own ledger does not.
            "payment_breakdown": loan.breakdown,
        }]
    return {
        "path": str(path),
        "family": FAMILY_MORTGAGE,
        "period_start": loan.statement_date or None,
        "period_end": loan.statement_date or None,
        "accounts": [entry],
    }

