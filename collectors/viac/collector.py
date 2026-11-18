"""The viac collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: `download --debug`'s HTTP trace, which debugcap lands
# under screenshots/ fleet-wide. Everything else in a run dir is a bronze
# capture.
SPEC = Collector(
    name="viac",
    env_prefix="VIAC",
    session="state",
    unattended=True,
    window="bounded",
)
