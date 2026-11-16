"""Exercises: the exercise-detail xlsx files, the certificate each one
produced, and the valuation basis they give held shares (DESIGN.md §5.3)."""
from __future__ import annotations

import re
import xml.etree.ElementTree as ET
import zipfile
from pathlib import Path

from common import _date_ts, _f, _read_json, _s, log


def _last_exercise_price(edir: Path) -> float | None:
    """Fallback valuation basis when no exercise-detail xlsx was captured —
    the highest strike among option grants with any exercised quantity. The
    primary basis is _last_exercise_fmv (the fair-market-value at the last
    exercise). None if nothing was exercised."""
    body = _read_json(edir / "options.json")
    rows = body.get("rows") if isinstance(body, dict) else None
    prices = [
        _f(r.get("exercise_price")) for r in (rows or [])
        if isinstance(r, dict) and (_f(r.get("exercised")) or 0) > 0
        and _f(r.get("exercise_price")) is not None
    ]
    return max(prices) if prices else None


_XL_NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"


def _parse_exercise_xlsx(path: Path) -> dict | None:
    """Parse a Carta exercise-detail xlsx into {grant_label, date, shares,
    exercise_price, fmv}. The sheet opens with a title naming the grant
    ('Exercise details for <grant label> (<holder>)'), then label/value rows
    ('Grant exercised on <date>', 'Shares exercised', 'Exercise price', 'Fair
    market value on exercise'); the value trails its label cell. xlsx == zip
    of XML, parsed with the stdlib. None if unreadable / no date."""
    try:
        z = zipfile.ZipFile(path)
        try:
            shared = [(t.text or "") for t in ET.fromstring(
                z.read("xl/sharedStrings.xml")).iter(_XL_NS + "t")]
        except KeyError:
            shared = []
        sheet = ET.fromstring(z.read("xl/worksheets/sheet1.xml"))
    except (OSError, zipfile.BadZipFile, ET.ParseError, KeyError) as exc:
        log.warning("could not read exercise xlsx %s: %s", path.name, exc)
        return None
    cells: list[str] = []
    for c in sheet.iter(_XL_NS + "c"):
        v = c.find(_XL_NS + "v")
        is_ = c.find(_XL_NS + "is")
        if is_ is not None:
            txt = "".join(x.text or "" for x in is_.iter(_XL_NS + "t"))
        elif v is not None:
            txt = shared[int(v.text)] if c.get("t") == "s" else v.text
        else:
            txt = ""
        cells.append((txt or "").strip())

    def value_after(label: str):
        for i, cell in enumerate(cells):
            if cell.startswith(label):
                for nxt in cells[i + 1:i + 4]:
                    if re.fullmatch(r"[\d.]+", nxt):
                        return nxt
        return None

    blob = "\n".join(cells)
    m = re.search(r"exercised on\s+(\d{2}/\d{2}/\d{4})", blob)
    if not m:
        return None
    title = re.search(r"Exercise details for\s+(\S+)", blob)
    return {"grant_label": title.group(1) if title else None,
            "date": m.group(1), "shares": value_after("Shares exercised"),
            "exercise_price": value_after("Exercise price"),
            "fmv": value_after("Fair market value")}


def _read_exercises(edir: Path) -> list[dict]:
    """Every exercise-detail xlsx under exercises/, parsed, each with the
    `grant_id` its file name (grant_<gid>_er_<erid>.xlsx) carries. Sorted
    by file name, so the result does not depend on directory order."""
    xdir = edir / "exercises"
    if not xdir.is_dir():
        return []
    out = []
    for xl in sorted(xdir.glob("*.xlsx")):
        ex = _parse_exercise_xlsx(xl)
        if ex:
            m = re.match(r"grant_(\d+)_er_", xl.name)
            out.append({**ex, "grant_id": m.group(1) if m else None})
    return out


def _last_exercise_fmv(exercises: list[dict]) -> float | None:
    """The fair-market-value at the most recent exercise — the holder's
    after-exercise valuation basis (shares exercised at a strike below FMV
    are worth FMV). None if no exercise detail states one."""
    best_ts, best_fmv = None, None
    for ex in exercises:
        if ex.get("fmv") is None:
            continue
        ts = _date_ts(ex.get("date"))
        if ts is not None and (best_ts is None or ts > best_ts):
            best_ts, best_fmv = ts, _f(ex["fmv"])
    return best_fmv


# The security lists whose rows are grants that can be exercised into shares;
# a certificate's `exercise_from` names one of them by its label.
_GRANT_FILES = ("options", "equity-grants", "rsu", "sar")


def _match_exercises(edir: Path, exercises: list[dict]) -> dict:
    """Pair each share certificate born from an exercise with the exercise
    detail behind it: {certificate id: exercise}.

    A certificate names its grant by label (`exercise_from`) and states its
    quantity and issue date; an exercise detail states its grant, the shares
    exercised and the exercise date. The certificate is issued on or some
    days after the exercise, so dates alone do not pair them. Within one
    grant and one quantity, certificates and exercises pair in date order:
    each certificate, oldest first, takes the oldest unpaired exercise dated
    on or before its issue date. A certificate with no such exercise is left
    out."""
    labels: dict[str, str] = {}
    for fname in _GRANT_FILES:
        body = _read_json(edir / f"{fname}.json")
        for row in (body.get("rows") if isinstance(body, dict) else None) or []:
            if isinstance(row, dict) and row.get("id") is not None:
                labels[str(row["id"])] = _s(row.get("label"))
    pool: dict[tuple, list[tuple[int, dict]]] = {}
    for ex in exercises:
        label = labels.get(ex.get("grant_id")) or ex.get("grant_label")
        ts, qty = _date_ts(ex.get("date")), _f(ex.get("shares"))
        if label and ts is not None and qty is not None:
            pool.setdefault((label, qty), []).append((ts, ex))
    for group in pool.values():
        group.sort(key=lambda p: p[0])

    body = _read_json(edir / "shares.json")
    certs = []
    for row in (body.get("rows") if isinstance(body, dict) else None) or []:
        if not isinstance(row, dict) or not row.get("exercise_from"):
            continue
        ts = _date_ts(row.get("issue_date"))
        if row.get("id") is not None and ts is not None:
            certs.append((ts, row))
    out = {}
    for ts, row in sorted(certs, key=lambda c: c[0]):
        group = pool.get((row["exercise_from"], _f(row.get("quantity"))), [])
        for i, (ex_ts, ex) in enumerate(group):
            if ex_ts <= ts:
                out[row["id"]] = ex
                del group[i]
                break
    return out
