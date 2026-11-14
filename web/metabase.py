"""One Metabase's REST API, for the tools that talk to it:
web/provision.py, demo/check_dashboards.py and site/capture.py.

Standard library only: web/web runs provision.py with the host's Python,
and this file sits beside it.
"""
import copy
import json
import urllib.error
import urllib.request


class Error(Exception):
    """A listing Metabase did not answer: its status and its body."""

    def __init__(self, what, status, body):
        super().__init__(f"could not list {what} ({status})")
        self.status, self.body = status, body


class Client:
    """The REST API of the Metabase at `base`, its scheme, host and port:
    as the user of session token `session`, under API key `api_key`, or
    with neither before a login. A call waits `timeout` seconds at most."""

    def __init__(self, base, session=None, api_key=None, timeout=30):
        self.base, self.session, self.api_key, self.timeout = base, session, api_key, timeout

    def with_auth(self, session=None, api_key=None):
        """This Metabase as the user of `session`, or under `api_key`."""
        other = copy.copy(self)
        other.session, other.api_key = session, api_key
        return other

    def call(self, path, method="GET", data=None, timeout=None):
        """(HTTP status, decoded JSON body) for one API call, {} for an
        empty body and {"message": text} for an error body that is no
        JSON. A call that gets no HTTP answer at all returns (0, {})."""
        body = json.dumps(data).encode() if data is not None else None
        r = urllib.request.Request(self.base + path, data=body, method=method)
        r.add_header("Content-Type", "application/json")
        if self.session:
            r.add_header("X-Metabase-Session", self.session)
        if self.api_key:
            r.add_header("X-API-Key", self.api_key)
        try:
            with urllib.request.urlopen(r, timeout=self.timeout if timeout is None else timeout) as resp:
                raw = resp.read().decode()
                return resp.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as e:
            raw = e.read().decode()
            try:
                return e.code, json.loads(raw)
            except Exception:
                return e.code, {"message": raw}
        except OSError:
            # Connection refused or reset, and a timeout: urlopen raises a
            # connect timeout as URLError but a read timeout as TimeoutError.
            return 0, {}

    def get(self, path, timeout=None):
        return self.call(path, timeout=timeout)

    def post(self, path, data):
        return self.call(path, "POST", data)

    def put(self, path, data):
        return self.call(path, "PUT", data)

    def delete(self, path):
        return self.call(path, "DELETE")

    def collection(self, name):
        """The collection named `name` that is not archived, or None
        where there is none. Raises Error when the listing fails, which
        read as empty would make a second collection of the name."""
        st, cols = self.get("/api/collection")
        if st != 200 or not isinstance(cols, list):
            raise Error("collections", st, cols)
        return next((c for c in cols if c.get("name") == name and not c.get("archived")), None)

    def dashboards(self, collection):
        """The dashboards in collection id `collection`, as its item
        listing gives them. Raises Error when the listing fails, which
        read as empty would make every dashboard a second time."""
        st, items = self.get(f"/api/collection/{collection}/items?models=dashboard")
        if st != 200:
            raise Error("dashboards", st, items)
        return items.get("data") or []
