#!/usr/bin/env bash
# Behaviour test of shared/update/bump-playwright.py:
#   - in this checkout, the Playwright base image tag, both `playwright`
#     pip pins and both user agents' Chrome majors agree with the version
#     and chrome-major base-playwright.Dockerfile records;
#   - a hand edit of any one place fails that check;
#   - a bump takes the highest plain vX.Y.Z-noble tag, reads the Chrome
#     major from that version's wheel, and rewrites every place in step;
#   - the newest tag already pinned changes nothing;
#   - an unreachable registry, a tag list with no -noble tag or a wheel
#     with no browsers.json fails and leaves every file as it was;
#   - over HTTP, the wheel is read by range requests, its browsers.json
#     and zip index alone, and a server that ignores the range fails.
# The bump runs on a copy of the files, against invented file:// tag
# lists and wheels. The HTTP read runs against a loopback server; nothing
# leaves the host.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
BUMP="$ROOT/shared/update/bump-playwright.py"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }
has() { if printf '%s\n' "$3" | grep -qF -- "$2"; then ok "$1"; else bad "$1 (no '$2' in: $3)"; fi; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FILES="shared/images/base-playwright.Dockerfile
collectors/ubs-web/requirements.txt collectors/ubs-web/login.py
collectors/ubs-web/README.md
collectors/swissquote/requirements.txt collectors/swissquote/login.py"

# fresh: a copy of the files the bump touches, at $TMP/tree.
fresh() {
    local f
    rm -rf "$TMP/tree"
    for f in $FILES; do
        mkdir -p "$TMP/tree/$(dirname "$f")"
        cp "$ROOT/$f" "$TMP/tree/$f"
    done
}
sums() { (cd "$TMP/tree" && for f in $FILES; do cksum "$f"; done); }
run()  { python3 -I "$BUMP" --root "$TMP/tree" "$@" 2>&1; }

# tags NAME TAG...: a registry tags/list body at $TMP/NAME.json.
tags() {
    local name="$1"; shift
    python3 -I -c 'import json, sys; print(json.dumps({"name": "playwright/python", "tags": sys.argv[2:]}))' \
        "$@" > "$TMP/$name.json"
}
# wheel VERSION CHROMIUM: a stand-in PyPI release whose wheel bundles a
# browsers.json with that Chromium version (empty: no browsers.json).
wheel() {
    python3 -I - "$TMP" "$1" "$2" <<'PY'
import json, pathlib, sys, zipfile
tmp, version, chromium = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
whl = tmp / f"playwright-{version}-py3-none-manylinux1_x86_64.whl"
with zipfile.ZipFile(whl, "w") as z:
    z.writestr("playwright/__init__.py", "")
    if chromium:
        z.writestr("playwright/driver/package/browsers.json", json.dumps({"browsers": [
            {"name": "firefox", "browserVersion": "999.0"},
            {"name": "chromium", "browserVersion": chromium}]}))
(tmp / f"pypi-{version}.json").write_text(json.dumps({"urls": [
    {"filename": "playwright-x.tar.gz", "url": "file:///nonexistent"},
    {"filename": whl.name, "url": whl.as_uri()}]}))
PY
}
bump() { run --tags-url "file://$TMP/$1.json" --pypi-url "file://$TMP/pypi-{version}.json"; }

echo "playwright pins:"
out="$(python3 -I "$BUMP" --check 2>&1)" \
    && ok "this checkout's Playwright places are in step" \
    || bad "this checkout's Playwright places are in step ($out)"

fresh
pinned="$(sed -n 's/^FROM .*python:v\([0-9.]*\)-noble$/\1/p' "$TMP/tree/shared/images/base-playwright.Dockerfile")"
run --check >/dev/null && ok "a copy of the places checks clean" || bad "a copy of the places checks clean"

# A hand edit of one place fails the check.
for edit in \
    "collectors/ubs-web/login.py|s#Chrome/[0-9]*\.0\.0\.0#Chrome/1.0.0.0#|a ubs-web user agent major" \
    "collectors/swissquote/login.py|s#Chrome/[0-9]*\.0\.0\.0#Chrome/1.0.0.0#|a swissquote user agent major" \
    "collectors/ubs-web/requirements.txt|s#^playwright==.*#playwright==0.0.1#|the ubs-web pip pin" \
    "collectors/swissquote/requirements.txt|s#^playwright==.*#playwright==0.0.1#|the swissquote pip pin" \
    "shared/images/base-playwright.Dockerfile|s#^\# chrome-major: .*#\# chrome-major: 1#|the recorded Chrome major" \
    "shared/images/base-playwright.Dockerfile|s#python:v[0-9.]*-noble#python:v0.0.1-noble#|the image tag" \
    "collectors/ubs-web/requirements.txt|s#python:v[0-9.]*-noble)#python:v0.0.1-noble)#|the ubs-web base image comment" \
    "collectors/swissquote/requirements.txt|s#python:v[0-9.]*-noble)#python:v0.0.1-noble)#|the swissquote base image comment" \
    "collectors/ubs-web/login.py|s#Playwright [0-9.]* ships#Playwright 0.1 ships#|a ubs-web Playwright minor" \
    "collectors/swissquote/login.py|s#Playwright [0-9.]* ships#Playwright 0.1 ships#|a swissquote Playwright minor" \
    "collectors/ubs-web/README.md|s#python:v[0-9.]*-noble#python:v0.0.1-noble#|the ubs-web README tag" \
    "collectors/ubs-web/login.py|/Chrome\//d|a missing user agent"; do
    IFS='|' read -r f expr what <<<"$edit"
    fresh
    sed -e "$expr" "$TMP/tree/$f" > "$TMP/edited" && cp "$TMP/edited" "$TMP/tree/$f"
    if run --check >/dev/null; then bad "a hand edit of $what fails the check"; else ok "a hand edit of $what fails the check"; fi
done

# A bump takes the highest plain -noble semver: 99.70.10 over 99.70.9,
# and none of the pre-release, other-distro or per-arch tags.
fresh
tags list latest next "v$pinned-noble" v99.70.9-noble v99.70.10-noble v99.70.10-noble-amd64 \
    v1.71.0-jammy v1.71.1.dev1-canary-20990101000000-noble v1.72.0-beta-noble v2.0.0 jammy
wheel 99.70.10 170.0.1234.5
out="$(bump list)"
has "the bump reports the move" "$pinned -> 99.70.10 (Chrome 170)" "$out"
grep -qx 'FROM mcr.microsoft.com/playwright/python:v99.70.10-noble' "$TMP/tree/shared/images/base-playwright.Dockerfile" \
    && grep -qx '# chrome-major: 170' "$TMP/tree/shared/images/base-playwright.Dockerfile" \
    && ok "the Dockerfile names the new tag and Chrome major" \
    || bad "the Dockerfile names the new tag and Chrome major"
for c in ubs-web swissquote; do
    grep -qx 'playwright==99.70.10' "$TMP/tree/collectors/$c/requirements.txt" \
        && grep -qF 'playwright/python:v99.70.10-noble)' "$TMP/tree/collectors/$c/requirements.txt" \
        && ok "$c's pip pin follows" || bad "$c's pip pin follows"
    grep -qF '"Chrome/170.0.0.0 Safari/537.36"' "$TMP/tree/collectors/$c/login.py" \
        && grep -qF 'Playwright 99.70 ships' "$TMP/tree/collectors/$c/login.py" \
        && ok "$c's user agent follows" || bad "$c's user agent follows"
done
grep -qF '`mcr.microsoft.com/playwright/python:v99.70.10-noble`' "$TMP/tree/collectors/ubs-web/README.md" \
    && ok "the ubs-web README follows" || bad "the ubs-web README follows"
run --check >/dev/null && ok "the bumped places check clean" || bad "the bumped places check clean"

# The newest tag already pinned changes nothing.
before="$(sums)"
out="$(bump list)"
has "an already-pinned newest tag says so" "nothing to change" "$out"
[ "$(sums)" = "$before" ] && ok "an already-pinned newest tag changes no file" \
    || bad "an already-pinned newest tag changes no file"

# Failures leave every file untouched.
fresh
before="$(sums)"
if out="$(bump missing)"; then bad "an unreachable registry fails"; else ok "an unreachable registry fails"; fi
has "an unreachable registry names the URL" "missing.json" "$out"
tags nonoble latest v9.0.0-jammy v9.0.0-noble-arm64
if bump nonoble >/dev/null; then bad "a tag list with no -noble tag fails"; else ok "a tag list with no -noble tag fails"; fi
tags nowheel "v$pinned-noble" v99.80.0-noble
if bump nowheel >/dev/null; then bad "a release with no PyPI entry fails"; else ok "a release with no PyPI entry fails"; fi
tags nojson "v$pinned-noble" v99.81.0-noble
wheel 99.81.0 ""
if bump nojson >/dev/null; then bad "a wheel with no browsers.json fails"; else ok "a wheel with no browsers.json fails"; fi
[ "$(sums)" = "$before" ] && ok "a failed bump changes no file" || bad "a failed bump changes no file"

# Over HTTP the wheel is read by range requests (RangeReader). A
# loopback server serves an invented wheel, padded so that a whole read
# would show; with ranges off it answers 200 to each, which must fail.
out="$(python3 -I - "$BUMP" <<'PY' 2>&1
import http.server, importlib.util, io, json, re, sys, threading, zipfile
spec = importlib.util.spec_from_file_location("bump", sys.argv[1])
bump = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bump)
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w") as z:
    z.writestr("playwright/__init__.py", "#" * 100_000)
    z.writestr("playwright/driver/package/browsers.json", json.dumps(
        {"browsers": [{"name": "chromium", "browserVersion": "171.0.1.2"}]}))
WHEEL, served = buf.getvalue(), []

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_HEAD(self):
        self.send_response(200)
        self.send_header("Content-Length", str(len(WHEEL)))
        self.end_headers()

    def do_GET(self):
        mode = self.path.split("/")[1]
        if self.path.endswith(".json"):
            body = json.dumps({"urls": [{"filename": "playwright-9.9.9-py3-none-manylinux1_x86_64.whl",
                                         "url": f"{base}/{mode}/p.whl"}]}).encode()
            self.send_response(200)
        else:
            m = re.fullmatch(r"bytes=(\d+)-(\d+)", self.headers.get("Range", ""))
            if mode == "range" and m:
                body = WHEEL[int(m[1]):int(m[2]) + 1]
                self.send_response(206)
            else:
                body = WHEEL
                self.send_response(200)
            served.append(len(body))
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
base = f"http://127.0.0.1:{srv.server_address[1]}"
threading.Thread(target=srv.serve_forever, daemon=True).start()
print("range:", bump.chrome_major("9.9.9", base + "/range/pypi-{version}.json"))
print("partial:", sum(served) < 100_000)
try:
    bump.chrome_major("9.9.9", base + "/plain/pypi-{version}.json")
    print("plain: read")
except bump.Failure as e:
    print("plain: failed", e)
srv.shutdown()
PY
)"
has "a range read finds the Chrome major in the wheel" "range: 171" "$out"
has "...and fetches the index and browsers.json alone" "partial: True" "$out"
has "a server that ignores the range fails" "plain: failed" "$out"
has "...and says why, naming the wheel" "/plain/p.whl: no range support (HTTP 200)" "$out"

echo "  $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
