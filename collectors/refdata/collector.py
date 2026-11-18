"""The refdata collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="refdata",
    env_prefix="REFDATA",
    session="key",
    unattended=True,
    window="full",
)
