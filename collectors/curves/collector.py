"""The curves collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: the only debug artefact curves writes is `download
# --debug`'s HTTP trace, which debugcap lands under screenshots/
# fleet-wide. Everything else in a complete run dir (run.json + each
# <portal>.csv) is a load input; naming either would delete one.
SPEC = Collector(
    name="curves",
    env_prefix="CURVES",
    session="none",
    unattended=True,
    window="bounded",
)
