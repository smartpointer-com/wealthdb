"""Link activity rows to the holdings their statements say they concern.

The Activity region names a security by a kerned NAME, the Holdings
region by its symbol, CUSIP or OCC code, so a row carries no instrument
key of its own. Two passes supply one, on evidence of different strength.

**The proof**, for a row that moves a quantity. Across one statement's
period, the rows that moved a holding add up to exactly the change in
that holding's quantity between the two snapshots that bracket the
period. That is what such a row's link rests on; for it, a name only
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

A **reversal** (a cancelled trade) moved the instrument of the row it
reverses. Beside that row in one window the two add nothing to any
key, so both leave the proof. In a later window it states the key an
earlier window linked its row to, even one the window never held, and
its printed name becomes a name of that key there.

**The name pass**, for a row that names a security but moves no quantity
of it: a dividend, its withholding, interest, a return of capital, cash
in lieu of a fraction. There is no arithmetic to prove such a row with,
so its name decides, and only where the account's own statements leave
one answer. Its candidates are the keys held, under a compatible name,
at whichever ends of its window are known. The one candidate is the
link unless, at some statement end, the account held that key beside
another under a name that fits too — a second share class, a second
bond of the issuer, whose late dividend would read exactly the same —
or unless the keys the proof linked a fitting trade name to, among those
the account showed within a pay lag before the row, are all other keys.
With no candidate held, those recent proven keys stand in for it when
there is one and only one, under the same sibling rule: a dividend paid
after the sale finds the key its trades moved. A withholding row that
prints its dividend's name on the same statement and day lands where
the dividend does, without being paired with it; one printed shorter is
judged on its own, and since it fits at least the holdings the
dividend's name fits, it can be refused where the dividend links.

The standard is weaker than the proof, and acceptable only because of
what such a link does: it names the security an amount came from and
moves no amount, section or class in any report, where a trade's link
moves money between asset classes.

Everything neither pass can settle stays unlinked, with the reason and
the token it was looked up by, for gold's ``instrument_hint``.
"""
from __future__ import annotations

import re
from collections import Counter, defaultdict
from dataclasses import dataclass, field, replace
from datetime import date, timedelta

# Why a row stayed unlinked. Census keys and log text, in the order the
# rule meets them.
NO_BRACKET = "no bracket"          # the window's opening or closing is unknown
NO_CANDIDATE = "no candidate"      # nothing held in the window fits it
DOES_NOT_CLOSE = "does not close"  # no assignment closes every key
NOT_FORCED = "not forced"          # the quantities allow another key or none
TOO_MANY = "too many"              # the search exceeded its budget
# Why a named row stayed unlinked, beside NO_CANDIDATE.
SEVERAL_FIT = "several fit"            # more than one key fits the name
SIBLING_HELD = "sibling held"          # its key once sat beside one that fits
NAMES_CONTRADICT = "names contradict"  # proven trades moved only other keys
TOO_OLD = "too old"                    # its proven keys were seen too long ago

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
# How long after an account last showed a key the proven trades that
# moved it still speak for a named row: they name its key when nothing
# fitting is held, and contradict a held one otherwise. A dividend pays
# up to a few statements after the sale.
_PAY_LAG = timedelta(days=183)


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
    identity for the row and comes back in :class:`Links`; ``reverses`` is
    the ``ref`` of the row a reversal cancels. ``day``, when known, is
    what dates the key's appearance for the name pass."""
    ref: object
    name: str
    quantity: float
    stated_key: str | None = None
    reverses: object = None
    day: date | None = None


@dataclass(frozen=True)
class Named:
    """One activity row that names a security but moves no quantity of
    it, for the name pass."""
    ref: object
    name: str
    day: date


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
    named: tuple[Named, ...] = ()


@dataclass
class Links:
    """The outcome per row ``ref``: the key it was linked to, or the
    reason it was not and the token it was looked up by — a movement's
    stated key, else the row's name as :func:`squash` compares it.
    ``named`` holds the refs the name pass decided. A reversal that left
    the proof beside the row it reverses has no outcome, nor has that
    row."""
    keys: dict[object, str] = field(default_factory=dict)
    unlinked: dict[object, tuple[str, str]] = field(default_factory=dict)
    named: set[object] = field(default_factory=set)

    def linked(self, *, named: bool = False) -> int:
        """How many of one pass's rows were linked."""
        return sum((ref in self.named) == named for ref in self.keys)

    def census(self, *, named: bool = False) -> Counter:
        """Why one pass's other rows were not."""
        return Counter(reason for ref, (reason, _) in self.unlinked.items()
                       if (ref in self.named) == named)


def squash(name: str) -> str:
    """The form names compare in, and the hint an unlinked row states."""
    return re.sub(r"\s+", "", name or "").upper()


def link(statements) -> Links:
    """Link every movement the statements' arithmetic proves, then every
    named row the account's statements leave one answer for."""
    by_account = defaultdict(list)
    for st in statements:
        by_account[st.account].append(st)
    links = Links()
    windows = []
    for sts in by_account.values():
        sts.sort(key=lambda s: (s.end, s.start or s.end))
        for prev, st in zip([None] + sts[:-1], sts, strict=True):
            opening = _opening(prev, st)
            _link_window(st, opening, links)
            windows.append((st, opening))
    # The name pass reads what the proof settled in every window, later
    # ones included, so it runs once the proof is done.
    evidence = {account: _Evidence(sts, links)
                for account, sts in by_account.items()}
    for st, opening in windows:
        _link_named(st, opening, evidence[st.account], links)
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


def _keys_fitting(name: str, names_by_key) -> set[str]:
    """The keys held under a name compatible with ``name``."""
    return {k for k, held in names_by_key.items()
            if any(_compatible(name, h) for h in held)}


def _link_window(st: Statement, opening, links: Links) -> None:
    moves, inherited = _without_reversed_pairs(st.movements, links)
    if not moves:
        return
    if opening is None or st.holdings is None:
        for m in moves:
            _unlink(links, m, NO_BRACKET)
        return
    (open_qty, open_names), (close_qty, close_names) = opening, _positions(st.holdings)
    change = {k: close_qty[k] - open_qty[k] for k in open_qty.keys() | close_qty.keys()}
    names = {k: open_names[k] | close_names[k] for k in change}
    for m in inherited:
        change.setdefault(m.stated_key, 0)
        names.setdefault(m.stated_key, set()).add(squash(m.name))
    cand = []
    for m in moves:
        if m.stated_key is not None:
            cand.append({m.stated_key} & change.keys())
        else:
            cand.append(_keys_fitting(squash(m.name), names))
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


def _without_reversed_pairs(moves, links: Links):
    """The window's movements as the proof takes them, and the reversals
    among them that inherited their row's key from an earlier window."""
    here = {m.ref for m in moves}
    reversed_here = {m.reverses for m in moves if m.reverses in here}
    kept, inherited = [], []
    for m in moves:
        if m.ref in reversed_here or m.reverses in reversed_here:
            continue
        if m.reverses in links.keys:
            m = replace(m, stated_key=links.keys[m.reverses])
            inherited.append(m)
        kept.append(m)
    return kept, inherited


def _unlink(links: Links, m: Movement, reason: str) -> None:
    # A byte-identical statement filed twice shares its rows' refs with the
    # first copy, whose window may have proved them.
    if m.ref not in links.keys:
        links.unlinked[m.ref] = (reason, m.stated_key or squash(m.name))


class _Evidence:
    """What one account's statements settled, as the name pass reads it:
    the names each key was held under at each known statement end, the
    days the account showed a key — held at an end, or moved by a proven
    row — and the keys the proof linked each printed trade name to."""

    def __init__(self, statements, links: Links):
        self.ends = []                   # per known end: key -> names held
        self.held_at = defaultdict(set)  # key -> indices into self.ends
        self.names = defaultdict(set)    # key -> every name it was held under
        self.seen = defaultdict(set)     # key -> days the account showed it
        self.proven = defaultdict(set)   # trade name -> keys the proof linked
        self._fitting, self._proven_fitting = {}, {}
        for st in statements:
            if st.holdings is not None:
                names = _positions(st.holdings)[1]
                for key, held in names.items():
                    self.held_at[key].add(len(self.ends))
                    self.names[key] |= held
                    self.seen[key].add(st.end)
                self.ends.append(names)
            for m in st.movements:
                key = links.keys.get(m.ref)
                if key is None:
                    continue
                self.proven[squash(m.name)].add(key)
                if m.day is not None:
                    self.seen[key].add(m.day)

    def fitting(self, name: str) -> set[str]:
        """The keys ever held under a name compatible with ``name``."""
        if name not in self._fitting:
            self._fitting[name] = _keys_fitting(name, self.names)
        return self._fitting[name]

    def proven_fitting(self, name: str) -> set[str]:
        """The keys the proof linked a trade printed under a name
        compatible with ``name`` to."""
        if name not in self._proven_fitting:
            self._proven_fitting[name] = {
                k for p, keys in self.proven.items() if _compatible(name, p)
                for k in keys}
        return self._proven_fitting[name]

    def has_sibling(self, key: str, name: str) -> bool:
        """Whether, at some statement end, ``key`` was held beside another
        key under a name that fits ``name`` as well."""
        return any(
            any(_compatible(name, n) for n in self.ends[i][other])
            for other in self.fitting(name) - {key}
            for i in self.held_at[key] & self.held_at[other])

    def recent(self, key: str, day: date) -> bool:
        """Whether the account showed ``key`` within the pay lag before
        ``day``."""
        return any(day - _PAY_LAG <= seen <= day for seen in self.seen[key])

    def stale(self, key: str, day: date) -> bool:
        """Whether the account showed ``key`` before the pay lag that ends
        on ``day``."""
        return any(seen < day - _PAY_LAG for seen in self.seen[key])


def _link_named(st: Statement, opening, ev: _Evidence, links: Links) -> None:
    ends = [opening[1]] if opening is not None else []
    if st.holdings is not None:
        ends.append(_positions(st.holdings)[1])
    for r in st.named:
        # A statement filed twice shares its rows' refs with the first
        # copy, which saw the wider window.
        if r.ref in links.keys or r.ref in links.unlinked:
            continue
        links.named.add(r.ref)
        name = squash(r.name)
        cand = set().union(*(_keys_fitting(name, end) for end in ends))
        proven = ev.proven_fitting(name)
        recent = {k for k in proven if ev.recent(k, r.day)}
        key = reason = None
        if len(cand) > 1 or (not cand and len(recent) > 1):
            reason = SEVERAL_FIT
        elif cand or recent:
            (key,) = cand or recent
            if ev.has_sibling(key, name):
                reason = SIBLING_HELD
            elif recent and key not in recent:
                reason = NAMES_CONTRADICT
        elif any(ev.stale(k, r.day) for k in proven):
            reason = TOO_OLD
        else:
            reason = NO_CANDIDATE
        if reason is None:
            links.keys[r.ref] = key
        else:
            links.unlinked[r.ref] = (reason, name)


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
