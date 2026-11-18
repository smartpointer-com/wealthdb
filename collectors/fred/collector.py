"""The fred collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: the only debug artefact fred writes is `download
# --debug`'s HTTP trace, which debugcap lands under screenshots/
# fleet-wide. Everything else in a complete run dir (run.json + each
# <series_id>.json) is a load input; naming either would delete one.
SPEC = Collector(
    name="fred",
    env_prefix="FRED",
    session="key",
    unattended=True,
    window="bounded",
)
