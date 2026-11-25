#!/usr/bin/env bash
# Behaviour test of the local Camoufox browser archive:
#   - camoufox-archive.py's checksum covers each entry's path, mode and
#     content and each link's target, and not the order tar wrote them in,
#     how gzip packed them or a ./ before each name;
#   - it refuses an entry listed twice, one that is not a file, directory
#     or symbolic link, and an archive without its camoufox-cache/ entry;
#   - unpack checks the archive first: a wrong or missing checksum, an
#     entry outside camoufox-cache/ or an existing destination unpacks
#     nothing;
#   - `make camoufox-archive` writes the archive from the kept image when
#     its checksum is the pinned one, and otherwise keeps the archive that
#     is there. A kept image built without the browser writes nothing.
# A stub docker stands in for the kept image; nothing real is built.
set -uo pipefail
# The makes this test starts read only what it gives them: an outer
# make's flags and command-line variables (make test-wrappers X=…) stay
# out.
unset MAKEFLAGS MFLAGS MAKELEVEL

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# macOS tar would add an AppleDouble entry for each file's attributes.
export COPYFILE_DISABLE=1
ARC="$ROOT/shared/images/camoufox-archive.py"
# unpack needs tarfile's extraction filters, which an older python3 on
# PATH may lack; make test-wrappers names a newer one in PYTHON.
arc() { "${PYTHON:-python3}" -I "$ARC" "$@"; }

# A tree of every entry kind, then two archives of it: one in tar's
# order, one in reverse order and packed harder.
SRC="$TMP/src"
mkdir -p "$SRC/camoufox-cache/sub"
printf 'browser\n' > "$SRC/camoufox-cache/camoufox-bin"
chmod 755 "$SRC/camoufox-cache/camoufox-bin"
printf 'pref\n' > "$SRC/camoufox-cache/sub/prefs.js"
ln -s sub/prefs.js "$SRC/camoufox-cache/link"
pack() { (cd "$SRC" && tar -czf "$1" camoufox-cache); }
pack "$TMP/a.tgz"
(cd "$SRC" && find camoufox-cache | sort -r | tar --no-recursion -cf - -T - | gzip -9 > "$TMP/b.tgz")

echo "camoufox archive:"
sum_a="$(arc sum "$TMP/a.tgz")"
if [ ${#sum_a} -eq 64 ] && [ "$sum_a" = "$(arc sum "$TMP/b.tgz")" ]; then
    ok "one tree, one checksum, whatever the order and packing"
else
    bad "one tree, one checksum, whatever the order and packing"
fi
changed() {
    pack "$TMP/c.tgz"
    if [ "$(arc sum "$TMP/c.tgz")" != "$sum_a" ]; then ok "$1"; else bad "$1"; fi
}
chmod 700 "$SRC/camoufox-cache/camoufox-bin"
changed "a changed mode changes the checksum"
chmod 755 "$SRC/camoufox-cache/camoufox-bin"
printf 'other\n' > "$SRC/camoufox-cache/sub/prefs.js"
changed "a changed file changes the checksum"
printf 'pref\n' > "$SRC/camoufox-cache/sub/prefs.js"
ln -sfn camoufox-bin "$SRC/camoufox-cache/link"
changed "a changed link target changes the checksum"
ln -sfn sub/prefs.js "$SRC/camoufox-cache/link"
(cd "$SRC" && tar -czf "$TMP/dot.tgz" ./camoufox-cache)
if [ "$(arc sum "$TMP/dot.tgz")" = "$sum_a" ]; then
    ok "a ./ before each name keeps the checksum"
else
    bad "a ./ before each name keeps the checksum"
fi
# refused WHAT ARCHIVE: the checksum of ARCHIVE fails, saying WHAT.
refused() {
    out="$(arc sum "$2" 2>&1)"; rc=$?
    if [ "$rc" -eq 1 ] && grep -qF "$1" <<<"$out"; then ok "$3"
    else bad "$3 (exit $rc: $out)"; fi
}
(cd "$SRC" && tar -cf "$TMP/twice.tar" camoufox-cache && tar -rf "$TMP/twice.tar" camoufox-cache/sub/prefs.js \
    && gzip -c "$TMP/twice.tar" > "$TMP/twice.tgz")
refused "in the archive twice" "$TMP/twice.tgz" "an entry listed twice is refused"
HARD="$TMP/hard"
mkdir -p "$HARD/camoufox-cache"
printf 'h\n' > "$HARD/camoufox-cache/one"
ln "$HARD/camoufox-cache/one" "$HARD/camoufox-cache/two"
(cd "$HARD" && tar -czf "$TMP/hard.tgz" camoufox-cache)
refused "is not a file, a directory or a symbolic link" "$TMP/hard.tgz" "a hard link is refused"
(cd "$SRC" && tar --no-recursion -czf "$TMP/notop.tgz" camoufox-cache/camoufox-bin)
refused "no camoufox-cache/ directory" "$TMP/notop.tgz" "an archive without its top directory is refused"

if arc unpack "$TMP/a.tgz" "$sum_a" "$TMP/out" \
    && [ "$(cat "$TMP/out/sub/prefs.js")" = pref ] \
    && [ -x "$TMP/out/camoufox-bin" ] && [ "$(readlink "$TMP/out/link")" = sub/prefs.js ]; then
    ok "an archive with the pinned checksum unpacks its tree"
else
    bad "an archive with the pinned checksum unpacks its tree"
fi
out="$(arc unpack "$TMP/a.tgz" "$sum_a" "$TMP/out" 2>&1)"; rc=$?
if [ "$rc" -eq 1 ] && grep -q 'exists' <<<"$out"; then
    ok "an existing destination is never unpacked over"
else
    bad "an existing destination is never unpacked over (exit $rc: $out)"
fi
out="$(arc unpack "$TMP/a.tgz" 0000 "$TMP/wrong" 2>&1)"; rc=$?
if [ "$rc" -eq 1 ] && [ ! -e "$TMP/wrong" ] && grep -qF "its checksum is $sum_a" <<<"$out" \
    && grep -qF 'make camoufox-archive' <<<"$out"; then
    ok "a wrong checksum unpacks nothing and says what to do"
else
    bad "a wrong checksum unpacks nothing and says what to do (exit $rc: $out)"
fi
out="$(arc unpack "$TMP/a.tgz" "" "$TMP/unpinned" 2>&1)"; rc=$?
if [ "$rc" -eq 1 ] && [ ! -e "$TMP/unpinned" ] && grep -q 'pins no checksum' <<<"$out"; then
    ok "an architecture with no pinned checksum unpacks nothing"
else
    bad "an architecture with no pinned checksum unpacks nothing (exit $rc: $out)"
fi
mkdir -p "$SRC/other"
printf 'x\n' > "$SRC/other/x"
(cd "$SRC" && tar -czf "$TMP/outside.tgz" camoufox-cache other)
out="$(arc unpack "$TMP/outside.tgz" "$sum_a" "$TMP/outside" 2>&1)"; rc=$?
if [ "$rc" -eq 1 ] && [ ! -e "$TMP/outside" ] && grep -q 'outside camoufox-cache' <<<"$out"; then
    ok "an entry outside camoufox-cache/ unpacks nothing"
else
    bad "an entry outside camoufox-cache/ unpacks nothing (exit $rc: $out)"
fi
# A link out of the tree passes the checksum and fails the extraction,
# which leaves no partial tree behind.
ESC="$TMP/esc"
mkdir -p "$ESC/src/camoufox-cache" "$ESC/out"
ln -s /etc/hosts "$ESC/src/camoufox-cache/hosts"
(cd "$ESC/src" && tar -czf "$ESC/esc.tgz" camoufox-cache)
out="$(arc unpack "$ESC/esc.tgz" "$(arc sum "$ESC/esc.tgz")" "$ESC/out/tree" 2>&1)"; rc=$?
if [ "$rc" -eq 1 ] && [ -z "$(ls -A "$ESC/out")" ]; then
    ok "a failed extraction leaves nothing behind"
else
    bad "a failed extraction leaves nothing behind (exit $rc: $out; left: $(ls -A "$ESC/out"))"
fi

# make camoufox-archive, in a copy of the tree whose Dockerfile pins the
# first archive's checksum. The stub answers the image's architecture
# and streams STUB_TAR as the kept image's tar.
TREE="$TMP/tree"
mkdir -p "$TREE/shared/wrappers" "$TREE/shared/images" "$TMP/bin"
cp "$ROOT/Makefile" "$TREE/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$ROOT/shared/wrappers/demo-root.sh" "$TREE/shared/wrappers/"
cp "$ARC" "$TREE/shared/images/"
printf 'ARG CAMOUFOX_BUILD=1.0-beta.1\nARG CAMOUFOX_SUM_ARM64=%s\n' "$sum_a" \
    > "$TREE/shared/images/base-camoufox.Dockerfile"
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"
case "${1:-}" in
    image) case "$*" in *.Config.Env*) printf '%s\n' "${STUB_ENV:-}" ;; *) echo arm64 ;; esac ;;
    run) cat "$STUB_TAR" ;;
esac
STUB
chmod +x "$TMP/bin/docker"
archive() {
    (cd "$TREE" && env PATH="$TMP/bin:$PATH" XDG_CACHE_HOME="$TMP/cache" STUB_LOG="$TMP/docker.log" \
        STUB_TAR="$1" WEALTHDB_IMAGE_TAG=t WEALTHDB_DEMO_ROOT="$TMP/demo" \
        make --no-print-directory camoufox-archive 2>&1)
}
DEST="$TMP/cache/wealthdb/camoufox/camoufox-cache-1.0-beta.1-lin.arm64.tgz"
out="$(archive "$TMP/a.tgz")"; rc=$?
if [ "$rc" -eq 0 ] && cmp -s "$DEST" "$TMP/a.tgz"; then
    ok "the archive is written when its checksum is the pinned one"
else
    bad "the archive is written when its checksum is the pinned one (exit $rc: $out)"
fi
has "the archive comes from the kept image of the pinned build" \
    "run --rm --pull=never --entrypoint tar wealthdb/base-camoufox:keep-1.0-beta.1 -C /opt -czf - camoufox-cache" \
    "$(cat "$TMP/docker.log")"
out="$(archive "$TMP/c.tgz")"; rc=$?
if [ "$rc" -ne 0 ] && cmp -s "$DEST" "$TMP/a.tgz" && grep -qF "$(arc sum "$TMP/c.tgz")" <<<"$out" \
    && [ "$(ls "$(dirname "$DEST")")" = "$(basename "$DEST")" ]; then
    ok "another checksum keeps the archive there, prints the new one and leaves no temporary file"
else
    bad "another checksum keeps the archive there, prints the new one and leaves no temporary file (exit $rc: $out)"
fi
: > "$TMP/docker.log"
out="$(STUB_ENV=WEALTHDB_CAMOUFOX_BROWSER=none archive "$TMP/c.tgz")"; rc=$?
if [ "$rc" -ne 0 ] && grep -qF 'is a build without the browser' <<<"$out" \
    && ! grep -q '^run ' "$TMP/docker.log" && cmp -s "$DEST" "$TMP/a.tgz"; then
    ok "a kept image without the browser writes nothing and says so"
else
    bad "a kept image without the browser writes nothing and says so (exit $rc: $out)"
fi

summary "camoufox archive tests"
