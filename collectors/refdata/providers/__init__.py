"""The providers, by name. Adding one is a module and a line here; the
order a run asks them in is refdata.cfg's."""

from __future__ import annotations

from .amundi import Amundi
from .fidelity529 import Fidelity529
from .gleif import Gleif
from .inbox import Inbox
from .invesco import Invesco
from .ishares import IShares
from .openfigi import OpenFigi
from .sec_edgar import SecEdgar
from .sec_nmfp import SecNmfp
from .sec_nport import SecNport
from .spdr import Spdr
from .sprott import Sprott
from .swisscanto import Swisscanto
from .swissfunddata import SwissFundData
from .xtrackers import Xtrackers

REGISTRY = {p.name: p for p in (Inbox(), IShares(), Xtrackers(), Invesco(), Amundi(), Spdr(), Sprott(),
                                Swisscanto(), SwissFundData(), Fidelity529(), SecNport(), SecNmfp(), SecEdgar(),
                                Gleif(), OpenFigi())}
