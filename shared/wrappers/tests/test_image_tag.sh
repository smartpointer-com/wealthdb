#!/usr/bin/env bash
# Behaviour test: WEALTHDB_IMAGE_TAG names the image every wrapper builds
# and runs — the engine wrapper, the Go toolchain wrapper and a docker
# collector — and the collector build hands the tag to its Dockerfile, so
# a second checkout builds on its own shared bases. Unset, the
# checkout's .image-tag names it, else it is latest; a malformed
# .image-tag stops the wrapper. The wrappers run from a copy of the tree,
# so the checkout's own .image-tag plays no part. A stubbed docker on
# PATH records the argv; nothing real is built.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin" "$TMP/cfg" "$TMP/data" "$TMP/cache"
# The wrappers under test, in the tree's layout.
TREE="$TMP/tree"
mkdir -p "$TREE/wealthdb" "$TREE/shared/wrappers"
cp "$ROOT/wealthdb/wealthdb" "$ROOT/wealthdb/wealthdb-go" "$TREE/wealthdb/"
cp "$ROOT/shared/wrappers/wrapper-lib.sh" "$ROOT/shared/wrappers/image-tag.sh" "$TREE/shared/wrappers/"
mkdir -p "$TREE/mcp" "$TREE/web"
cp "$ROOT/Makefile" "$TREE/"
cp "$ROOT/mcp/mcp" "$TREE/mcp/"
cp "$ROOT/web/web" "$TREE/web/"
# A symlink to the engine wrapper, as `make install` lays one on PATH.
ln -s "$TREE/wealthdb/wealthdb" "$TMP/bin/wealthdb"

# make_tag: the tag the Makefile resolves, from a dry run of `help`.
make_tag() {
    (cd "$TMP" && env "$@" make -f "$TREE/Makefile" -np help 2>&1) | sed -n 's/^TAG := //p'
}
# knob_tag SCRIPT: the image mcp or web resolves, sourced as their tests
# do. Sourced, each finds its directory from $0, so $0 is a path beside it.
knob_tag() {
    (cd "$TMP" && env WEALTHDB_IMAGE_TAG= WEALTHDB_WEB_IMAGE= \
        bash -c '. "$1" >/dev/null 2>&1; _resolve_knobs; echo "$IMAGE"' "$(dirname "$1")/test" "$1")
}
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@"
STUB
chmod +x "$TMP/bin/docker"

# has DESC LINE ARGV: the stubbed docker's argv holds LINE as one item.
has() {
    if printf '%s\n' "$3" | grep -qxF -- "$2"; then ok "$1"; else bad "$1 (no '$2' in: $(printf '%s ' $3))"; fi
}

run_env() {
    env PATH="$TMP/bin:$PATH" HOME="$TMP" XDG_CONFIG_HOME="$TMP/cfg" \
        XDG_CACHE_HOME="$TMP/cache" WEALTHDB_DATA_ROOT="$TMP/data" "$@"
}

collector_build() {
    (
        export PATH="$TMP/bin:$PATH"
        [ -n "${1:-}" ] && export WEALTHDB_IMAGE_TAG="$1" || unset WEALTHDB_IMAGE_TAG
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(); HERE="$TMP"
        unset TESTCOL_IMAGE
        # shellcheck disable=SC1091
        source "$TREE/shared/wrappers/wrapper-lib.sh"
        wrapper_init
        wrapper_build build
    )
}

echo "image tag:"
has "engine runs wealthdb:latest by default" "wealthdb:latest" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" status)"
has "engine runs the tagged image" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" status)"
has "engine builds the tagged image" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" build)"
has "go wrapper runs the tagged image" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb-go" version)"
out="$(collector_build feature)"
has "collector builds the tagged image" "wealthdb/testcol:feature" "$out"
has "collector build passes the tag to FROM" "WEALTHDB_IMAGE_TAG=feature" "$out"
out="$(collector_build "")"
has "collector image defaults to latest" "wealthdb/testcol:latest" "$out"
has "collector base defaults to latest" "WEALTHDB_IMAGE_TAG=latest" "$out"

# The checkout's .image-tag names the tag when the variable is unset,
# and the variable still wins over it.
printf 'pinned\n' > "$TREE/.image-tag"
has "engine runs the .image-tag image" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" status)"
has "engine builds the .image-tag image" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" build)"
has "go wrapper runs the .image-tag image" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb-go" version)"
out="$(collector_build "")"
has "collector builds the .image-tag image" "wealthdb/testcol:pinned" "$out"
has "collector base follows .image-tag" "WEALTHDB_IMAGE_TAG=pinned" "$out"
has "the variable wins over .image-tag" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" status)"
has "the engine run through a symlink reads .image-tag" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= wealthdb status)"
has "make resolves .image-tag" "pinned" "$(make_tag -u WEALTHDB_IMAGE_TAG)"
has "make: the variable wins" "feature" "$(make_tag WEALTHDB_IMAGE_TAG=feature)"
has "mcp reads .image-tag" "wealthdb:pinned" "$(knob_tag "$TREE/mcp/mcp")"
has "web reads .image-tag" "wealthdb/metabase:pinned" "$(knob_tag "$TREE/web/web")"
rm -f "$TREE/.image-tag"
has "make defaults to latest" "latest" "$(make_tag -u WEALTHDB_IMAGE_TAG)"
printf 'pinned\n' > "$TREE/.image-tag"

# A malformed .image-tag stops the wrapper before docker runs: it never
# falls back to latest.
printf 'not a tag\n' > "$TREE/.image-tag"
out="$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" build 2>&1)"; rc=$?
if [ "$rc" -eq 2 ] && ! printf '%s\n' "$out" | grep -q '^build$'; then
    ok "a malformed .image-tag stops the engine wrapper"
else
    bad "a malformed .image-tag stops the engine wrapper (exit $rc: $out)"
fi
out="$(collector_build "" 2>&1)"; rc=$?
if [ "$rc" -eq 2 ]; then ok "a malformed .image-tag stops a collector wrapper"
else bad "a malformed .image-tag stops a collector wrapper (exit $rc)"; fi
if (cd "$TMP" && env -u WEALTHDB_IMAGE_TAG make -f "$TREE/Makefile" -n help >/dev/null 2>&1); then
    bad "a malformed .image-tag stops make"
else
    ok "a malformed .image-tag stops make"
fi
rm -f "$TREE/.image-tag"

# A .image-tag that is no readable file (a dangling link, a directory)
# stops the wrapper too: it is never read as no file at all.
for kind in link dir; do
    if [ "$kind" = link ]; then ln -s "$TMP/no-such-file" "$TREE/.image-tag"; else mkdir "$TREE/.image-tag"; fi
    out="$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" status 2>&1)"; rc=$?
    if [ "$rc" -eq 2 ]; then ok "a .image-tag $kind stops the engine wrapper"
    else bad "a .image-tag $kind stops the engine wrapper (exit $rc: $out)"; fi
    rm -rf "$TREE/.image-tag"
done

# Every Dockerfile that builds on a shared base takes the tag from the
# build argument, so no checkout silently builds on another's bases.
for f in "$ROOT"/collectors/*/Dockerfile "$ROOT"/shared/images/*.Dockerfile; do
    if grep -qE '^FROM wealthdb/.*:latest' "$f"; then
        bad "${f#"$ROOT"/} pins a shared base to :latest"
    fi
done
ok "no Dockerfile pins a shared base to :latest"

echo "  $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
