"""Tests for site/images.py against a fake GitHub; no network."""

import io
import json
import sys
import tempfile
import unittest
import urllib.error
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import images  # noqa: E402

REPO = "example-org/example"


class Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class FakeGitHub:
    """Records every request; answers from a release held in memory."""

    def __init__(self, release=None):
        self.release = release
        self.calls = []

    def __call__(self, req, timeout=None):
        method, url = req.get_method(), req.full_url
        self.calls.append((method, url, dict(req.header_items()), req.data))
        if method == "GET" and url.endswith(f"/releases/tags/{images.TAG}"):
            if self.release is None:
                raise urllib.error.HTTPError(url, 404, "Not Found", {}, io.BytesIO(b'{"message":"Not Found"}'))
            return Response(json.dumps(self.release).encode())
        if method == "GET":
            return Response(b"image:" + url.rsplit("/", 1)[1].encode())
        if method == "POST" and url.endswith("/releases"):
            self.release = {"id": 7, "assets": [], **json.loads(req.data)}
            return Response(json.dumps(self.release).encode())
        if method == "POST" and "uploads.github.com" in url:
            return Response(b'{"id": 1}')
        return Response(b"")


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.tmp))

    def fake(self, release=None):
        gh = FakeGitHub(release)
        old, images._urlopen = images._urlopen, gh
        self.addCleanup(setattr, images, "_urlopen", old)
        return gh


class ParseRemote(unittest.TestCase):
    def test_forms(self):
        for url in ("git@github.com:example-org/example.git", "https://github.com/example-org/example",
                    "https://github.com/example-org/example.git", "ssh://git@github.com/example-org/example.git"):
            self.assertEqual(images.parse_remote(url), REPO, url)

    def test_not_github(self):
        with self.assertRaises(SystemExit):
            images.parse_remote("https://gitlab.com/example-org/example.git")


class Publish(Base):
    def files(self, *names):
        for n in names:
            (self.tmp / n).write_bytes(n.encode())
        (self.tmp / "notes.txt").write_text("not an image")

    def test_replaces_every_asset_then_deploys(self):
        self.files("a.webp", "b.webp")
        gh = self.fake({"id": 3, "assets": [{"id": 11, "name": "a.webp"}, {"id": 12, "name": "gone.webp"}]})
        uploaded = images.publish(REPO, images.TAG, self.tmp, "t0ken")
        self.assertEqual(uploaded, ["a.webp", "b.webp"])
        steps = [(m, u.split("github.com")[1]) for m, u, _, _ in gh.calls]
        self.assertEqual(steps, [
            ("GET", f"/repos/{REPO}/releases/tags/{images.TAG}"),
            ("DELETE", f"/repos/{REPO}/releases/assets/11"),
            ("DELETE", f"/repos/{REPO}/releases/assets/12"),
            ("POST", f"/repos/{REPO}/releases/3/assets?name=a.webp"),
            ("POST", f"/repos/{REPO}/releases/3/assets?name=b.webp"),
            ("POST", f"/repos/{REPO}/actions/workflows/pages.yml/dispatches"),
        ])
        _, _, headers, data = gh.calls[3]
        self.assertEqual(headers["Authorization"], "Bearer t0ken")
        self.assertEqual(headers["Content-type"], "image/webp")
        self.assertEqual(data, b"a.webp")
        self.assertEqual(json.loads(gh.calls[-1][3]), {"ref": "main"})

    def test_creates_a_pre_release_when_missing(self):
        self.files("a.webp")
        gh = self.fake(None)
        images.publish(REPO, images.TAG, self.tmp, "t0ken", deploy=False)
        created = json.loads(next(d for m, u, _, d in gh.calls if m == "POST" and u.endswith("/releases")))
        self.assertEqual(created["tag_name"], images.TAG)
        self.assertTrue(created["prerelease"])
        self.assertEqual(created["make_latest"], "false")
        self.assertFalse(any(u.endswith("/dispatches") for _, u, _, _ in gh.calls))

    def test_refuses_an_empty_folder(self):
        gh = self.fake({"id": 3, "assets": [{"id": 11, "name": "a.webp"}]})
        with self.assertRaises(SystemExit):
            images.publish(REPO, images.TAG, self.tmp, "t0ken")
        self.assertEqual(gh.calls, [])


class Fetch(Base):
    def test_downloads_every_asset_without_the_token(self):
        gh = self.fake({"id": 3, "assets": [
            {"id": 11, "name": "a.webp", "browser_download_url": "https://github.com/dl/a.webp"},
            {"id": 12, "name": "b.webp", "browser_download_url": "https://github.com/dl/b.webp"}]})
        names = images.fetch(REPO, images.TAG, self.tmp / "shots", token="t0ken")
        self.assertEqual(names, ["a.webp", "b.webp"])
        self.assertEqual((self.tmp / "shots/a.webp").read_bytes(), b"image:a.webp")
        self.assertIn("Authorization", gh.calls[0][2])
        self.assertTrue(all("Authorization" not in h for _, _, h, _ in gh.calls[1:]))

    def test_no_release_fails(self):
        self.fake(None)
        with self.assertRaises(SystemExit):
            images.fetch(REPO, images.TAG, self.tmp)

    def test_other_errors_surface(self):
        def boom(req, timeout=None):
            raise urllib.error.HTTPError(req.full_url, 500, "err", {}, io.BytesIO(b"down"))
        old, images._urlopen = images._urlopen, boom
        self.addCleanup(setattr, images, "_urlopen", old)
        with self.assertRaises(images.GitHubError):
            images.fetch(REPO, images.TAG, self.tmp)


@unittest.skipUnless(__import__("importlib").util.find_spec("PIL"), "Pillow not installed")
class Convert(Base):
    def test_scales_to_width_and_prunes(self):
        from PIL import Image
        src, dest = self.tmp / "png", self.tmp / "webp"
        src.mkdir()
        dest.mkdir()
        Image.new("RGBA", (2880, 2356), (16, 185, 129, 255)).save(src / "overview.png")
        Image.new("RGB", (800, 600)).save(src / "small.png")
        (dest / "stale.webp").write_bytes(b"old")
        self.assertEqual(images.convert(src, dest), ["overview.webp", "small.webp"])
        self.assertFalse((dest / "stale.webp").exists())
        with Image.open(dest / "overview.webp") as im:
            self.assertEqual((im.format, im.size), ("WEBP", (2000, 1636)))
        with Image.open(dest / "small.webp") as im:
            self.assertEqual(im.size, (800, 600))

    def test_no_shots_fails(self):
        with self.assertRaises(SystemExit):
            images.convert(self.tmp, self.tmp / "out")


if __name__ == "__main__":
    unittest.main()
