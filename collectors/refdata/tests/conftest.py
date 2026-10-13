"""Put the collector's own modules on the path, as the wrapper runs them."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
