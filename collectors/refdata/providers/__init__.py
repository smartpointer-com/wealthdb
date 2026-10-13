"""The providers, by name. Adding one is a module and a line here; the
order a run asks them in is refdata.cfg's."""

from __future__ import annotations

from .inbox import Inbox
from .sec_nport import SecNport

REGISTRY = {p.name: p for p in (Inbox(), SecNport())}
