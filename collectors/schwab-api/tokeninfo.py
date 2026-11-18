"""The token file's age report for `login --check`: when the file was
issued, and when its refresh token runs out by the 7-day cap. Read
from the file alone; the check's probe call is in `login.py`.
"""

from __future__ import annotations

import json
import logging
from datetime import datetime, timezone
from pathlib import Path

# The login's own logger: a record reads the same whichever module
# writes it.
log = logging.getLogger("schwab-login")


# Schwab refresh tokens are valid for 7 days from issue. Surfaced as a
# constant so --check output matches the actual cap.
REFRESH_TOKEN_TTL_SECONDS = 7 * 24 * 3600


def read_token_creation_time(path: Path) -> datetime | None:
    try:
        with path.open("r", encoding="utf-8") as fh:
            blob = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        log.warning("Could not parse token file %s: %s", path, exc)
        return None
    ts = blob.get("creation_timestamp")
    if isinstance(ts, (int, float)):
        return datetime.fromtimestamp(ts, tz=timezone.utc)
    try:
        return datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc)
    except OSError:
        return None


def print_token_age(token_path: Path) -> None:
    """Print when the token file was issued and when its refresh token
    runs out by the 7-day cap. An estimate from the file alone: the
    probe (`login.probe_token`) is what proves the token."""
    issued = read_token_creation_time(token_path)
    print(f"Token file:        {token_path}")
    if issued is None:
        print("Issued (UTC):      unknown (no issue timestamp in the file)")
        return
    age = datetime.now(timezone.utc) - issued
    remaining_s = REFRESH_TOKEN_TTL_SECONDS - age.total_seconds()
    print(f"Issued (UTC):      {issued.isoformat(timespec='seconds')}")
    print(f"Age:               {age}")
    if remaining_s <= 0:
        print("Estimated expiry:  EXPIRED by the 7-day cap.")
        return
    renew_by = datetime.fromtimestamp(
        issued.timestamp() + REFRESH_TOKEN_TTL_SECONDS, tz=timezone.utc,
    )
    print(f"Estimated expiry:  {renew_by.isoformat(timespec='seconds')} "
          f"({remaining_s / 3600:.1f}h from now)")
    if remaining_s / 3600 < 24:
        print("WARNING: refresh window expires in under 24 hours. "
              "Re-run login soon.")
