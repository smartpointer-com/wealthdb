"""The schwab-api collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# The default debug_subdirs (screenshots/) is where `download --debug`'s
# HTTP trace lands; every other file in a run dir is a load input.
SPEC = Collector(
    name="schwab-api",
    env_prefix="SCHWAB_API",
    session="token",
    unattended=True,
    window="bounded",
)
