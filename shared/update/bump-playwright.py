#!/usr/bin/env python3
"""Keep the Playwright base image, pip pins and user agents in lockstep.

ubs-web and swissquote run on mcr.microsoft.com/playwright/python. The
image tag, the `playwright` pip pin in each collector's requirements.txt
and the Chrome major in each collector's USER_AGENT name one release
together. base-playwright.Dockerfile is the record: its FROM line holds
the version and its `# chrome-major: NNN` line holds the Chrome major of
the Chromium that version bundles.

    bump-playwright.py [--root DIR]    move every place to the newest release
    bump-playwright.py --check [--root DIR]
                                       fail when a place disagrees with the record

The bump reads the registry's tag list and takes the highest X.Y.Z that
has a `vX.Y.Z-noble` tag. The Chrome major comes from the browsers.json
inside that version's `playwright` wheel on PyPI: the file the package
installs its browsers from. Only the wheel's zip index and that one
member are fetched, with HTTP range requests.

Every read happens before any write, and every place must match exactly
once, so a failed read or a place that does not match changes no file.
Each file is swapped in whole; a write that fails partway leaves the
files apart, which --check (and so `make test-wrappers`) reports.
Standard library only.
"""

import argparse
import io
import json
import os
import re
import sys
import tempfile
import urllib.parse
import urllib.request
import zipfile

TAGS_URL = "https://mcr.microsoft.com/v2/playwright/python/tags/list"
PYPI_URL = "https://pypi.org/pypi/playwright/{version}/json"
TIMEOUT = 60

DOCKERFILE = "shared/images/base-playwright.Dockerfile"
FROM_RE = r"^FROM mcr\.microsoft\.com/playwright/python:v(\d+\.\d+\.\d+)-noble$"
CHROME_RE = r"^# chrome-major: (\d+)$"

# (file, pattern, kind): group 1 of the pattern holds the value, and
# kind names it: version (X.Y.Z), minor (X.Y) or chrome (the major).
PLACES = [(DOCKERFILE, FROM_RE, "version"), (DOCKERFILE, CHROME_RE, "chrome")]
for _c in ("ubs-web", "swissquote"):
    PLACES += [
        (f"collectors/{_c}/requirements.txt",
         r"playwright/python:v(\d+\.\d+\.\d+)-noble\)", "version"),
        (f"collectors/{_c}/requirements.txt",
         r"^playwright==(\d+\.\d+\.\d+)$", "version"),
        (f"collectors/{_c}/login.py",
         r"^    \"Chrome/(\d+)\.0\.0\.0 Safari/537\.36\"$", "chrome"),
        (f"collectors/{_c}/login.py",
         r"Playwright (\d+\.\d+) ships", "minor"),
    ]
PLACES.append(("collectors/ubs-web/README.md",
               r"`mcr\.microsoft\.com/playwright/python:v(\d+\.\d+\.\d+)-noble`",
               "version"))

NOBLE_TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)-noble$")


class Failure(Exception):
    pass


def expected(kind, version, chrome):
    return {"version": version,
            "minor": ".".join(version.split(".")[:2]),
            "chrome": chrome}[kind]


def read_places(root):
    """Return {file: text}, failing unless every pattern matches once."""
    texts = {}
    for path, pattern, _ in PLACES:
        if path not in texts:
            try:
                with open(os.path.join(root, path), encoding="utf-8") as f:
                    texts[path] = f.read()
            except OSError as e:
                raise Failure(f"{path}: {e.strerror}") from e
        n = len(re.findall(pattern, texts[path], re.M))
        if n != 1:
            raise Failure(f"{path}: /{pattern}/ matches {n} times, want 1")
    return texts


def recorded(texts):
    """The version and Chrome major base-playwright.Dockerfile records."""
    text = texts[DOCKERFILE]
    return (re.search(FROM_RE, text, re.M).group(1),
            re.search(CHROME_RE, text, re.M).group(1))


def check(root):
    texts = read_places(root)
    version, chrome = recorded(texts)
    bad = []
    for path, pattern, kind in PLACES:
        got = re.search(pattern, texts[path], re.M).group(1)
        want = expected(kind, version, chrome)
        if got != want:
            bad.append(f"{path}: {kind} {got}, want {want}")
    if bad:
        raise Failure(f"out of step with {DOCKERFILE} "
                      f"(Playwright {version}, Chrome {chrome}):\n  "
                      + "\n  ".join(bad))
    print(f"playwright: {len(PLACES)} places in step "
          f"(Playwright {version}, Chrome {chrome})")


def fetch(url):
    try:
        with urllib.request.urlopen(url, timeout=TIMEOUT) as r:
            return r.read()
    except (OSError, ValueError) as e:
        raise Failure(f"{url}: {e}") from e


def newest_noble(tags_json):
    """The highest X.Y.Z with a plain vX.Y.Z-noble tag in a tags/list body."""
    try:
        tags = json.loads(tags_json)["tags"]
    except (ValueError, KeyError, TypeError) as e:
        raise Failure(f"tag list is not a registry tags/list body: {e}") from e
    versions = [tuple(int(g) for g in m.groups())
                for m in map(NOBLE_TAG_RE.match, tags) if m]
    if not versions:
        raise Failure("tag list has no vX.Y.Z-noble tag")
    return ".".join(map(str, max(versions)))


class RangeReader(io.RawIOBase):
    """A seekable read-only view of a remote file, by HTTP range requests."""

    def __init__(self, url):
        super().__init__()
        self.url, self.pos = url, 0
        req = urllib.request.Request(url, method="HEAD")
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            self.size = int(r.headers["Content-Length"])

    def readable(self):
        return True

    def seekable(self):
        return True

    def tell(self):
        return self.pos

    def seek(self, offset, whence=io.SEEK_SET):
        base = {io.SEEK_SET: 0, io.SEEK_CUR: self.pos, io.SEEK_END: self.size}
        self.pos = base[whence] + offset
        return self.pos

    def read(self, n=-1):
        end = self.size if n is None or n < 0 else min(self.pos + n, self.size)
        if end <= self.pos:
            return b""
        req = urllib.request.Request(
            self.url, headers={"Range": f"bytes={self.pos}-{end - 1}"})
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            if r.status != 206:
                raise Failure(f"{self.url}: no range support (HTTP {r.status})")
            data = r.read()
        self.pos += len(data)
        return data


def open_wheel(url):
    if urllib.parse.urlparse(url).scheme == "file":
        return open(urllib.request.url2pathname(urllib.parse.urlparse(url).path), "rb")
    return RangeReader(url)


def chrome_major(version, pypi_url):
    """The Chromium major the `playwright` wheel of version bundles."""
    url = pypi_url.format(version=version)
    try:
        files = json.loads(fetch(url))["urls"]
        wheels = [f for f in files if f["filename"].endswith(".whl")]
        # Every platform's wheel carries the same browsers.json.
        wheel = next((f for f in wheels if "manylinux" in f["filename"]
                      and "x86_64" in f["filename"]), wheels[0])["url"]
    except (ValueError, KeyError, TypeError, IndexError) as e:
        raise Failure(f"{url}: no playwright {version} wheel ({e})") from e
    try:
        with open_wheel(wheel) as raw, zipfile.ZipFile(raw) as z:
            name = next(n for n in z.namelist()
                        if n.endswith("/driver/package/browsers.json"))
            browsers = json.loads(z.read(name))["browsers"]
        full = next(b["browserVersion"] for b in browsers if b["name"] == "chromium")
    except (OSError, ValueError, KeyError, TypeError, StopIteration,
            zipfile.BadZipFile) as e:
        raise Failure(f"{wheel}: no chromium browserVersion ({e!r})") from e
    major = full.split(".")[0]
    if not major.isdigit():
        raise Failure(f"{wheel}: chromium browserVersion {full!r} has no major")
    return major


def write_all(root, texts):
    """Replace each file through a temp file beside it, keeping its mode."""
    for path, text in texts.items():
        dest = os.path.join(root, path)
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(dest), prefix=".bump-")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(text)
            os.chmod(tmp, os.stat(dest).st_mode & 0o7777)
            os.replace(tmp, dest)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)


def bump(root, tags_url, pypi_url):
    texts = read_places(root)
    pinned, _ = recorded(texts)
    newest = newest_noble(fetch(tags_url))
    if newest == pinned:
        print(f"playwright: {pinned} is the newest -noble tag; nothing to change")
        return
    chrome = chrome_major(newest, pypi_url)
    new = dict(texts)
    for path, pattern, kind in PLACES:
        value = expected(kind, newest, chrome)
        m = re.search(pattern, new[path], re.M)
        new[path] = new[path][:m.start(1)] + value + new[path][m.end(1):]
    changed = {p: t for p, t in new.items() if t != texts[p]}
    write_all(root, changed)
    print(f"playwright: {pinned} -> {newest} (Chrome {chrome})")
    for path in sorted(changed):
        print(f"    rewrote {path}")


def main(argv=None):
    ap = argparse.ArgumentParser(
        description="Move the Playwright base image, pip pins and user "
                    "agents to the newest release, in lockstep.")
    ap.add_argument("--root", default=os.path.join(os.path.dirname(__file__), "..", ".."),
                    help="repository root (default: this checkout)")
    ap.add_argument("--check", action="store_true",
                    help="only check that every place agrees with the Dockerfile")
    ap.add_argument("--tags-url", default=TAGS_URL, help=argparse.SUPPRESS)
    ap.add_argument("--pypi-url", default=PYPI_URL, help=argparse.SUPPRESS)
    args = ap.parse_args(argv)
    try:
        if args.check:
            check(args.root)
        else:
            bump(args.root, args.tags_url, args.pypi_url)
    except Failure as e:
        print(f"playwright: ERROR: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
