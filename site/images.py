#!/usr/bin/env python3
"""Keep the project page's screenshots in a GitHub release, not in git.

    python3 site/images.py convert SRC DEST
    python3 site/images.py fetch DEST [--repo OWNER/NAME]
    python3 site/images.py publish SRC [--repo OWNER/NAME] [--no-deploy]

convert   turns the PNG shots in SRC into the WebP files the page loads,
          2000 pixels wide, and removes WebP files in DEST that have no
          shot any more. Needs Pillow.
fetch     downloads every file of the release into DEST. The Pages
          workflow and a local preview both use it. It needs no token,
          but a token lifts the API's limit for anonymous callers.
publish   makes the release hold exactly the WebP files in SRC, then
          starts the Pages workflow so the page picks them up. Every
          file goes up before an old one is removed, so a failed upload
          leaves the published shots in place. The release is created
          on first use, as a pre-release so GitHub never shows it as
          the latest wealthdb release.

The repository defaults to the one the checkout's origin points at. The
token comes from WEALTHDB_SITE_TOKEN; publish needs one allowed to write
the repository's contents and run its workflows.
"""

import argparse
import json
import os
import pathlib
import re
import subprocess
import sys
import urllib.error
import urllib.request

API = "https://api.github.com"
UPLOADS = "https://uploads.github.com"
TAG = "site-images"
# The name a file is uploaded under until every file is up.
STAGING = "staging-"
RELEASE_NAME = "Website images"
RELEASE_BODY = ("The screenshots on the project page. The Pages workflow downloads "
                "them at deploy time, so they stay out of the git history.")
WIDTH = 2000
QUALITY = 82

_urlopen = urllib.request.urlopen


class GitHubError(Exception):
    """An API call GitHub answered with an error."""


def parse_remote(url):
    """OWNER/NAME of a GitHub remote URL, ssh or https."""
    m = re.match(r"^(?:git@github\.com:|ssh://git@github\.com/|https://github\.com/)([^/]+)/(.+?)(?:\.git)?/?$", url)
    if not m:
        raise SystemExit(f"images: not a GitHub remote: {url}")
    return f"{m.group(1)}/{m.group(2)}"


def origin_repo():
    """OWNER/NAME of the checkout's origin."""
    url = subprocess.run(["git", "remote", "get-url", "origin"], capture_output=True, text=True, check=True).stdout
    return parse_remote(url.strip())


def request(method, url, token=None, body=None, data=None, content_type=None, accept="application/vnd.github+json"):
    """One GitHub call; returns the decoded JSON body, or None when empty."""
    if body is not None:
        data, content_type = json.dumps(body).encode(), "application/json"
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Accept", accept)
    req.add_header("X-GitHub-Api-Version", "2022-11-28")
    if content_type:
        req.add_header("Content-Type", content_type)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with _urlopen(req, timeout=120) as r:
            raw = r.read()
    except urllib.error.HTTPError as e:
        with e:
            detail = e.read().decode(errors="replace")[:300]
        raise GitHubError(f"{method} {url}: HTTP {e.code} {detail}") from None
    if not raw:
        return None
    return json.loads(raw) if accept.endswith("json") else raw


def get_release(repo, tag, token=None):
    """The release behind tag, or None when there is none."""
    try:
        return request("GET", f"{API}/repos/{repo}/releases/tags/{tag}", token)
    except GitHubError as e:
        if "HTTP 404" in str(e):
            return None
        raise


def create_release(repo, tag, ref, token):
    """A new pre-release for tag, on ref."""
    return request("POST", f"{API}/repos/{repo}/releases", token, body={
        "tag_name": tag, "target_commitish": ref, "name": RELEASE_NAME, "body": RELEASE_BODY,
        "prerelease": True, "make_latest": "false"})


def local_files(src, suffix):
    """{name: path} of the files in src with the given suffix."""
    return {p.name: p for p in sorted(pathlib.Path(src).glob(f"*{suffix}"))}


def convert(src, dest):
    """WebP files in dest from the PNG shots in src; returns their names."""
    from PIL import Image

    dest = pathlib.Path(dest)
    dest.mkdir(parents=True, exist_ok=True)
    shots = local_files(src, ".png")
    if not shots:
        raise SystemExit(f"images: no PNG shots in {src}")
    written = []
    for name, path in shots.items():
        out = dest / (pathlib.Path(name).stem + ".webp")
        with Image.open(path) as im:
            im = im.convert("RGB")
            if im.width > WIDTH:
                im = im.resize((WIDTH, round(im.height * WIDTH / im.width)), Image.LANCZOS)
            im.save(out, "WEBP", quality=QUALITY, method=6)
        written.append(out.name)
    for stale in set(local_files(dest, ".webp")) - set(written):
        (dest / stale).unlink()
    return sorted(written)


def fetch(repo, tag, dest, token=None):
    """Download every file of the release into dest; returns their names."""
    release = get_release(repo, tag, token)
    if not release or not release.get("assets"):
        raise SystemExit(f"images: release {tag} of {repo} has no files")
    dest = pathlib.Path(dest)
    dest.mkdir(parents=True, exist_ok=True)
    names = []
    for a in release["assets"]:
        if a["name"].startswith(STAGING):
            continue  # left by a publish that failed part way
        # The public download URL, without the token: GitHub redirects to
        # storage that refuses a request carrying one.
        data = request("GET", a["browser_download_url"], accept="application/octet-stream")
        (dest / a["name"]).write_bytes(data)
        names.append(a["name"])
    return sorted(names)


def publish(repo, tag, src, token, ref="main", workflow="pages.yml", deploy=True):
    """Make the release hold exactly the WebP files in src, then deploy."""
    files = local_files(src, ".webp")
    if not files:
        raise SystemExit(f"images: no WebP files in {src}; run make site-shots first")
    release = get_release(repo, tag, token) or create_release(repo, tag, ref, token)

    def asset_url(asset_id):
        return f"{API}/repos/{repo}/releases/assets/{asset_id}"

    old = {}
    for a in release.get("assets", []):
        # A staged file a failed run left behind would block its name.
        if a["name"].startswith(STAGING):
            request("DELETE", asset_url(a["id"]), token)
        else:
            old[a["name"]] = a["id"]
    # GitHub refuses a second asset of one name, so each file goes up
    # under a staging name. The old assets stay until every file is up.
    upload = sorted(files)
    staged = {}
    for name in upload:
        staged[name] = request(
            "POST", f"{UPLOADS}/repos/{repo}/releases/{release['id']}/assets?name={STAGING}{name}",
            token, data=files[name].read_bytes(), content_type="image/webp")["id"]
        print(f"images: uploaded {name}", flush=True)
    for name in upload:
        if name in old:
            request("DELETE", asset_url(old.pop(name)), token)
        request("PATCH", asset_url(staged[name]), token, body={"name": name})
    # An asset with no local file goes last, so the release mirrors src.
    for asset_id in old.values():
        request("DELETE", asset_url(asset_id), token)
    if deploy:
        request("POST", f"{API}/repos/{repo}/actions/workflows/{workflow}/dispatches", token, body={"ref": ref})
        print(f"images: started {workflow} on {ref}", flush=True)
    return upload


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("convert")
    c.add_argument("src")
    c.add_argument("dest")
    for name, folder in (("fetch", "dest"), ("publish", "src")):
        p = sub.add_parser(name)
        p.add_argument(folder)
        p.add_argument("--repo", help="OWNER/NAME (default: the origin remote)")
    sub.choices["publish"].add_argument("--no-deploy", action="store_true")
    a = ap.parse_args(argv)
    if a.cmd == "convert":
        print(f"images: wrote {len(convert(a.src, a.dest))} files to {a.dest}")
        return
    repo = a.repo or origin_repo()
    token = os.environ.get("WEALTHDB_SITE_TOKEN")
    try:
        if a.cmd == "fetch":
            print(f"images: fetched {len(fetch(repo, TAG, a.dest, token))} files to {a.dest}")
        else:
            if not token:
                raise SystemExit("images: publish needs WEALTHDB_SITE_TOKEN")
            publish(repo, TAG, a.src, token, deploy=not a.no_deploy)
    except GitHubError as e:
        raise SystemExit(f"images: {e}") from None


if __name__ == "__main__":
    sys.exit(main())
