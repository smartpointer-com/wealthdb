"""Link activity rows to the holdings the statements prove they moved.

The Activity region names a security by a kerned NAME, the Holdings
region by its symbol, CUSIP or OCC code, so a row carries no instrument
key of its own. What the statements do carry is arithmetic: across one
statement's period, the rows that moved a holding add up to exactly the
change in that holding's quantity between the two snapshots that
bracket the period. That is the proof a link rests on here; a name only
proposes the candidates.

A **window** is one statement of one account. Its closing is that
statement's holdings; its opening is the previous statement's when that
one ends the day before this one starts, and nothing at all when the
statement states a $0.00 period beginning value. Any other window —
after a missing month, beside a statement whose holdings table was
lost — proves nothing.

A row's **candidates** are the window's keys it could have moved: the
key it states outright (an option leg's OCC code), or else every key
held under a name compatible with the one it prints. Names compare with
whitespace removed, since the extraction kerns by inserting spaces, and
one must be a prefix of the other — the blotter truncates a long name
and appends trade notes to a short one.

The **rule**: assign each row to one of its candidates or to none, so
that every key's rows sum to its change. A row is linked only when it
lands on the same key in every such assignment. A row a name merely
suggests is never forced onto a key: two rows that cancel could equally
be another security bought and sold inside the period, and two keys
with the same change cannot be told apart, so neither is linked. A row
that states its key cannot go unassigned, but its key must still close.

Everything the proof cannot settle stays unlinked, with the reason and
the token it was looked up by, for gold's ``instrument_hint``.
"""
from __future__ import annotations

import re
from collections import Counter, defaultdict
from dataclasses import dataclass, field
from datetime import date, timedelta

# Why a row stayed unlinked. Census keys and log text, in the order the
# rule meets them.
NO_BRACKET = "no bracket"          # the window's opening or closing is unknown
NO_CANDIDATE = "no candidate"      # nothing held in the window fits it
DOES_NOT_CLOSE = "does not close"  # no assignment closes every key
NOT_FORCED = "not forced"          # the quantities allow another key or none
TOO_MANY = "too many"              # the search exceeded its budget

# Statement quantities carry at most three decimals, so thousandths make
# the arithmetic exact.
_SCALE = 1000
# A name shorter than this is too little to go on as a prefix, so it
# must match a holding's whole name.
_MIN_PREFIX = 4
# Bounds on the two searches below. A component that would exceed one
# links nothing.
_MAX_SUMS = 200_000
_MAX_NODES = 200_000


class _Budget(Exception):
    """A search ran past its bound."""


@dataclass(frozen=True)
class Holding:
    key: str
    name: str
    quantity: float


@dataclass(frozen=True)
class Movement:
    """One activity row that changed a position. ``ref`` is the caller's
    identity for the row and comes back in :class:`Links`."""
    ref: object
    name: str
    quantity: float
    stated_key: str | None = None


@dataclass(frozen=True)
class Statement:
    """What one statement says about one account. ``holdings`` is
    ``None`` when the statement lost its table (a carry-forward), which
    is not the same as holding nothing."""
    account: str
    start: date | None
    end: date
    holdings: tuple[Holding, ...] | None
    opens_empty: bool
    movements: tuple[Movement, ...]


@dataclass
class Links:
    """The outcome per movement ``ref``: the key it was linked to, or the
    reason it was not and the token it was looked up by — its stated key,
    else its name as :func:`squash` compares it."""
    keys: dict[object, str] = field(default_factory=dict)
    unlinked: dict[object, tuple[str, str]] = field(default_factory=dict)

    def census(self) -> Counter:
        return Counter(reason for reason, _ in self.unlinked.values())


def squash(name: str) -> str:
    """The form names compare in, and the hint an unlinked row states."""
    return re.sub(r"\s+", "", name or "").upper()


def link(statements) -> Links:
    """Link every movement the statements' arithmetic proves."""
    by_account = defaultdict(list)
    for st in statements:
        by_account[st.account].append(st)
    links = Links()
    for sts in by_account.values():
        sts.sort(key=lambda s: (s.end, s.start or s.end))
        for prev, st in zip([None] + sts[:-1], sts):
            _link_window(st, _opening(prev, st), links)
    return links


def _opening(prev: Statement | None, st: Statement):
    if (prev is not None and prev.holdings is not None
            and st.start is not None
            and prev.end + timedelta(days=1) == st.start):
        return _positions(prev.holdings)
    if st.opens_empty:
        return _positions(())
    return None


def _positions(holdings):
    """``(key -> scaled quantity, key -> names)``. A key printed on two
    rows (a cash and a margin lot) is one position."""
    qty, names = defaultdict(int), defaultdict(set)
    for h in holdings:
        qty[h.key] += round(h.quantity * _SCALE)
        names[h.key].add(squash(h.name))
    return qty, names


def _compatible(row_name: str, held: str) -> bool:
    if min(len(row_name), len(held)) < _MIN_PREFIX:
        return row_name == held
    return held.startswith(row_name) or row_name.startswith(held)


def _link_window(st: Statement, opening, links: Links) -> None:
    moves = st.movements
    if not moves:
        return
    if opening is None or st.holdings is None:
        for m in moves:
            _unlink(links, m, NO_BRACKET)
        return
    (open_qty, open_names), (close_qty, close_names) = opening, _positions(st.holdings)
    change = {k: close_qty[k] - open_qty[k] for k in open_qty.keys() | close_qty.keys()}
    names = {k: open_names[k] | close_names[k] for k in change}
    cand = []
    for m in moves:
        if m.stated_key is not None:
            cand.append({m.stated_key} & change.keys())
        else:
            name = squash(m.name)
            cand.append({k for k in change
                         if any(_compatible(name, h) for h in names[k])})
    for rows, keys in _components(cand):
        qty = {i: round(moves[i].quantity * _SCALE) for i in rows}
        exact = {i for i in rows if moves[i].stated_key is not None}
        if len(keys) == 1:
            forced, reason = _solve_one(rows, qty, exact, next(iter(keys)), change)
        else:
            forced, reason = _solve_many(rows, qty, exact, cand, keys, change)
        for i in rows:
            if i in forced:
                links.keys[moves[i].ref] = forced[i]
                links.unlinked.pop(moves[i].ref, None)
            else:
                _unlink(links, moves[i], reason)
    for i, c in enumerate(cand):
        if not c:
            _unlink(links, moves[i], NO_CANDIDATE)


def _unlink(links: Links, m: Movement, reason: str) -> None:
    # A byte-identical statement filed twice shares its rows' refs with the
    # first copy, whose window may have proved them.
    if m.ref not in links.keys:
        links.unlinked[m.ref] = (reason, m.stated_key or squash(m.name))


def _components(cand):
    """Group rows that share a candidate key, transitively."""
    parent = {}

    def find(x):
        while parent.setdefault(x, x) != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    for i, keys in enumerate(cand):
        for k in keys:
            parent[find(("row", i))] = find(("key", k))
    groups = defaultdict(lambda: ([], set()))
    for i, keys in enumerate(cand):
        if keys:
            rows, ks = groups[find(("row", i))]
            rows.append(i)
            ks.update(keys)
    return list(groups.values())


def _solve_one(rows, qty, exact, key, change):
    """Settle rows whose one candidate is ``key``. Returns the forced rows
    (row -> key) and the reason for the rest.

    The rows assigned to the key are the stated ones plus some subset of
    the others summing to what the stated ones leave of its change. A row
    is forced exactly when no such subset omits it. Rows of one sign admit
    no smaller subset with the same sum, which settles almost every window
    without a search."""
    free = [i for i in rows if i not in exact]
    target = change[key] - sum(qty[i] for i in exact)
    if target == sum(qty[i] for i in free) and (
            all(qty[i] > 0 for i in free) or all(qty[i] < 0 for i in free)):
        return dict.fromkeys(rows, key), NOT_FORCED
    prefix = [{0}]
    for i in free:
        prefix.append(prefix[-1] | {s + qty[i] for s in prefix[-1]})
        if len(prefix[-1]) > _MAX_SUMS:
            return {}, TOO_MANY
    if target not in prefix[-1]:
        return {}, DOES_NOT_CLOSE
    forced = dict.fromkeys(exact, key)
    suffix = {0}
    for pos in range(len(free) - 1, -1, -1):
        i = free[pos]
        # Forced unless the target can be reached without row i.
        if not any(target - s in suffix for s in prefix[pos]):
            forced[i] = key
        suffix = suffix | {s + qty[i] for s in suffix}
        if len(suffix) > _MAX_SUMS:
            return {}, TOO_MANY
    return forced, NOT_FORCED


def _solve_many(rows, qty, exact, cand, keys, change):
    """Settle rows that could belong to more than one key, as
    :func:`_solve_one` does: every assignment is enumerated, pruned by what
    the rows still to place could add to each key, under a node budget."""
    order = sorted(rows, key=lambda i: len(cand[i]))
    options = {i: sorted(cand[i]) + ([] if i in exact else [None])
               for i in order}
    remaining = {k: change[k] for k in keys}
    # What the rows from position p on could still add to each key.
    up = [defaultdict(int) for _ in range(len(order) + 1)]
    down = [defaultdict(int) for _ in range(len(order) + 1)]
    for p in range(len(order) - 1, -1, -1):
        up[p] = defaultdict(int, up[p + 1])
        down[p] = defaultdict(int, down[p + 1])
        for k in cand[order[p]]:
            bound = up[p] if qty[order[p]] > 0 else down[p]
            bound[k] += qty[order[p]]
    seen = {i: set() for i in order}
    assign = {}
    nodes = 0
    found = False

    def feasible(p):
        return all(down[p][k] <= remaining[k] <= up[p][k] for k in keys)

    def place(p):
        nonlocal nodes, found
        nodes += 1
        if nodes > _MAX_NODES:
            raise _Budget
        if p == len(order):
            found = True
            for i, k in assign.items():
                seen[i].add(k)
            return
        i = order[p]
        for k in options[i]:
            assign[i] = k
            if k is not None:
                remaining[k] -= qty[i]
            if feasible(p + 1):
                place(p + 1)
            if k is not None:
                remaining[k] += qty[i]
        del assign[i]

    try:
        if feasible(0):
            place(0)
    except _Budget:
        return {}, TOO_MANY
    if not found:
        return {}, DOES_NOT_CLOSE
    forced = {i: next(iter(ks)) for i, ks in seen.items()
              if len(ks) == 1 and None not in ks}
    return forced, NOT_FORCED
