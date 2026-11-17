"""
What earlier complete runs fetched, read from their manifests: the
ledger a plan counts as had until gold holds it, so a second run before
the next load asks for it no more.

An answer is settled by provider, subject kind (fund, security, bulk),
subject and fact, with the day it states and the day of the latest run
that fetched it. The facts are those of a manifest entry beside a few
of the ledger's own:

- a fund request's composition (COMPOSITION) unless it asked for facts
  alone, and each fact it got, by the day the fetch stated;
- a security request's lookup (IDENTITY), FIGURES where it asked for
  families of figures, and each fact a security wants it got;
- a bulk request itself (fact None), and, for each security its
  meta.json's `answered` names, NAMED and each fact it asked for.

Apart: every security request sent, one that failed included, for a
provider that waits min_age_days between two (base.py).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import date, datetime, timezone

from collectorkit import bronze
from providers.base import ProviderError, complete_runs, read_meta
from wants import COMPOSITION, IDENTITY

FIGURES, NAMED = "figures", "named"


@dataclass
class Ledger:
    # (provider, kind, subject, fact) -> the day an answer states, None
    # where it states none -> the day of the latest run that fetched it
    runs: dict = field(default_factory=dict)
    # (provider, security key) -> the day of the latest request sent
    sent: dict = field(default_factory=dict)
    # (kind, subject, fact) -> the providers that settled it
    by: dict = field(default_factory=dict, compare=False, repr=False)

    def settle(self, provider, kind, subject, fact, day, run) -> None:
        """Settle an answer; run None: a run of no known day, which
        never counts as recent."""
        days = self.runs.setdefault((provider, kind, subject, fact), {})
        if run is None:
            days.setdefault(day, None)
        else:
            days[day] = max(filter(None, (days.get(day), run)))
        self.by.setdefault((kind, subject, fact), set()).add(provider)

    def last(self, provider, kind, subject, fact) -> date | None:
        """The day of the latest run that settled the fact."""
        return max(filter(None, self.runs.get((provider, kind, subject, fact), {}).values()), default=None)

    def settled(self, provider, kind, subject, fact) -> set:
        """The days the provider's answers stated the fact for."""
        return {d for d in self.runs.get((provider, kind, subject, fact), ()) if d is not None}

    def held(self, kind, subject, fact) -> set:
        """The days any provider's answers stated the fact for."""
        return {d for p in self.by.get((kind, subject, fact), ()) for d in self.settled(p, kind, subject, fact)}

    def subjects(self, provider, kind, fact=None) -> dict:
        """subject -> the day of the latest run that settled its fact,
        of one provider."""
        return {s: self.last(p, k, s, f) for (p, k, s, f) in self.runs if (p, k, f) == (provider, kind, fact)}

    def last_sent(self, provider, key) -> date | None:
        """The day of the latest security request sent to the provider
        for the key, one that failed too."""
        return self.sent.get((provider, key))

    @classmethod
    def read(cls, bronze_dir) -> Ledger:
        """The ledger of the complete runs in bronze_dir; empty without
        one."""
        out = cls()
        for run, manifest in complete_runs(bronze_dir):
            day = datetime.fromtimestamp(bronze.parse_run_ts(run.name), timezone.utc).date()
            for e in manifest.get("requests") or []:
                if e.get("kind") == "security" and e.get("status") in ("ok", "error", "unavailable"):
                    out.sent[(e["provider"], e["ref"])] = max(out.sent.get((e["provider"], e["ref"]), day), day)
                if e.get("status") == "ok" and e.get("kind") in _READERS:
                    _READERS[e["kind"]](out, run, e, day)
        return out


def _fund(ledger, run, e, day) -> None:
    as_of = date.fromisoformat(e["as_of"]) if e.get("as_of") else None
    if as_of and not e.get("facts_only"):
        ledger.settle(e["provider"], "fund", e["ref"], COMPOSITION, as_of, day)
    for fact in e.get("facts") or ():
        ledger.settle(e["provider"], "fund", e["ref"], fact, as_of, day)


def _security(ledger, run, e, day) -> None:
    for fact in (IDENTITY, *((FIGURES,) if e.get("families") else ()), *(e.get("facts") or ())):
        ledger.settle(e["provider"], "security", e["ref"], fact, None, day)


def _bulk(ledger, run, e, day) -> None:
    ledger.settle(e["provider"], "bulk", e["ref"], None, None, day)
    for key in answered(run / e["dir"]) if e.get("dir") else ():
        for fact in (NAMED, *(e.get("facts") or ())):
            ledger.settle(e["provider"], "security", key, fact, None, day)


_READERS = {"fund": _fund, "security": _security, "bulk": _bulk}


def flat_keys(keys) -> set:
    """Security keys as base.py's bulk contract states them: a list, or
    lists (or one key) by another identifier, such as a CIK."""
    if isinstance(keys, dict):
        keys = [k for v in keys.values() for k in ([v] if isinstance(v, str) else v)]
    return set(keys or ())


def answered(folder) -> set:
    """The security keys a bulk answer names in its meta.json's
    `answered` (base.py); none where it names none."""
    try:
        return flat_keys(read_meta(folder).get("answered"))
    except (ProviderError, OSError, ValueError, AttributeError):
        return set()
