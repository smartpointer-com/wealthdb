"""The schwab-api collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: `download --debug`'s HTTP trace, which debugcap lands
# under screenshots/ fleet-wide. Every other file in a run dir is a load
# input.
SPEC = Collector(
    name="schwab-api",
    env_prefix="SCHWAB_API",
    session="token",
    unattended=True,
    window="bounded",
)
