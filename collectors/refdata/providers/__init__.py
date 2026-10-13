"""The providers, by name. Adding one is a module and a line here; the
order a run asks them in is refdata.cfg's."""

from __future__ import annotations

from .gleif import Gleif
from .inbox import Inbox
from .openfigi import OpenFigi
from .sec_edgar import SecEdgar
from .sec_nport import SecNport
from .spdr import Spdr

REGISTRY = {p.name: p for p in (Inbox(), SecNport(), SecEdgar(), Gleif(), OpenFigi(), Spdr())}
