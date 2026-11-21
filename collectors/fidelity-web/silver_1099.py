"""
fidelity-web silver: the Consolidated 1099.

Which copy of each form to keep, the account and tax year each copy
files under in `documents`, and the Form 1099-B lots of the kept copy
in `closed_lots`.
"""

from __future__ import annotations

import logging
import re

from silver_lots import _bool_column, _closed_lot_term, _insert_closed_lot
from silver_retail import normalize_payload

log = logging.getLogger("fidelity-web.load")


# A Consolidated 1099 under either naming Fidelity uses:
# `<YYYY>-<nickname>-<NNNN>-Consolidated-Form-1099.pdf` and
# `[CORRECTED_]Consolidated_Form_1099[__<n>].pdf`.
_CONSOLIDATED_1099_RE = re.compile(r"Consolidated[-_]Form[-_]1099",
                                   re.IGNORECASE)


def _consolidated_1099_candidates(dump_dir):
    """The Consolidated Form 1099 PDFs in a dump's ``documents/``,
    corrected forms included, in sorted order."""
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return []
    return [p for p in sorted(docs_dir.glob("*.pdf"))
            if _CONSOLIDATED_1099_RE.search(p.name)]


def _held_1099_forms(conn):
    """``(account, tax_year) -> form_prepared`` of the 1099-B lots silver
    holds."""
    return {(aid, year): prepared for aid, year, prepared in conn.execute(
        "SELECT account_external_id, tax_year, MAX(form_prepared) "
        "FROM closed_lots WHERE document_kind = 'form_1099b' "
        "GROUP BY account_external_id, tax_year")}


def _latest_1099_copies(copies):
    """The copy to keep of each form among ``copies``, and the sha256 of
    each copy that names no form: ``({(account, tax_year): (prepared,
    path, sha)}, [sha])``.

    Fidelity re-renders a Consolidated 1099 on every download, so one
    form arrives in every dump with new bytes. A form is its account and
    tax year; the copy prepared latest wins, which puts a corrected form
    over the original. Between copies prepared the same day the first
    one wins, in dump order: the copy a routine load took when the form
    first arrived, which later loads keep. So a `load --force` keeps
    the same copy, and no load re-parses a re-rendered form. Only the
    identities are read by then; the lots are parsed for the copies
    kept. A copy names no form when its identity does not read or
    states no account, tax year or prepared date."""
    chosen, unreadable = {}, []
    for dump, path, sha, ident in copies:
        key = (ident.get("account_external_id"), ident.get("tax_year"))
        if "_error" in ident or None in key or not ident.get("prepared"):
            log.warning(
                "1099: %s/%s states no account, tax year or prepared "
                "date; skipping (%s)", dump.name, path.name,
                ident.get("_error", "unrecognised layout"))
            unreadable.append(sha)
            continue
        best = chosen.get(key)
        if best is None or ident["prepared"] > best[0]:
            chosen[key] = (ident["prepared"], path, sha)
    return chosen, unreadable


def _file_1099_documents(conn, copies):
    """Give each copy's `documents` row the account and tax year its form
    states. Returns the number of rows changed.

    The newer file name, ``Consolidated_Form_1099.pdf``, states neither,
    and the ``<YYYY>-…`` one states only the year, so the classifier
    leaves the rest NULL. Only a NULL column is filled: a column the
    form does not state stays NULL, and a year the name stated stays."""
    changed = 0
    for _, _, sha, ident in copies:
        if "_error" in ident:
            continue
        aid, year = ident.get("account_external_id"), ident.get("tax_year")
        changed += conn.execute(
            "UPDATE documents"
            "   SET account_external_id = COALESCE(account_external_id, ?),"
            "       tax_year = COALESCE(tax_year, ?)"
            " WHERE content_sha256 = ?"
            "   AND ((account_external_id IS NULL AND ? IS NOT NULL)"
            "     OR (tax_year IS NULL AND ? IS NOT NULL))",
            (aid, year, sha, aid, year)).rowcount
    return changed


def _replace_1099_lots(conn, parsed, sha):
    """Replace the 1099-B lots of the form ``parsed`` is a copy of with
    its own. Returns the number of lots written."""
    aid = parsed["account_external_id"]
    year = parsed["tax_year"]
    conn.execute(
        "DELETE FROM closed_lots WHERE document_kind = 'form_1099b' "
        "AND account_external_id = ? AND tax_year = ?", (aid, year))
    occurrence: dict[str, int] = {}
    for lot in parsed.get("lots", []):
        identity = "|".join(str(lot.get(k)) for k in (
            "form_8949_box", "term", "covered", "cusip", "description",
            "action", "quantity", "acquired_date", "date_sold", "proceeds",
            "cost_basis"))
        _insert_closed_lot(conn, "1099b_", f"{aid}|{year}|{identity}",
                           occurrence, {
            "document_kind": "form_1099b",
            "account_external_id": aid,
            "tax_year": year,
            "form_prepared": parsed.get("prepared"),
            "security_name": lot.get("description"),
            "instrument_key": lot.get("symbol") or lot.get("cusip"),
            "cusip": lot.get("cusip"),
            "action": lot.get("action"),
            "quantity": lot.get("quantity"),
            "acquired_date": lot.get("acquired_date"),
            "disposed_date": lot.get("date_sold"),
            "proceeds": lot.get("proceeds"),
            "cost_basis": lot.get("cost_basis"),
            "accrued_market_discount": lot.get("accrued_market_discount"),
            "wash_sale_disallowed": lot.get("wash_sale_disallowed"),
            "realized_gain_loss": lot.get("gain_loss"),
            "federal_tax_withheld": lot.get("federal_tax_withheld"),
            "term": _closed_lot_term(lot.get("term")),
            "covered": _bool_column(lot.get("covered")),
            "form_8949_box": lot.get("form_8949_box"),
            "corrected": _bool_column(lot.get("corrected")),
            "source_sha256": sha,
            "payload": normalize_payload(lot),
        })
    return len(parsed.get("lots", []))
