#!/usr/bin/env bash
# Behaviour test: WEALTHDB_IMAGE_TAG names the image every wrapper builds
# and runs — the engine wrapper, the Go toolchain wrapper and a docker
# collector — and the collector build hands the tag to its Dockerfile, so
# a second checkout builds on its own shared bases. Unset, the
# checkout's .image-tag names it, else it is latest; a malformed
# .image-tag stops the wrapper. A build whose inputs hash matches the
# image's wealthdb.inputs label is skipped, for the engine, a collector
# and the shared bases; an extra argument, a changed input or a rebuilt
# base image builds. The wrappers run from a copy of the tree, so the
# checkout's own .image-tag plays no part. A stubbed docker on PATH
# records the argv; nothing real is built.
set -uo pipefail
# The makes this test starts read only what it gives them: an outer
# make's flags and command-line variables (make test-wrappers X=…) stay
# out.
unset MAKEFLAGS MFLAGS MAKELEVEL

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
cp "$ROOT/shared/wrappers/wrapper-lib.sh" "$ROOT/shared/wrappers/image-tag.sh" \
    "$ROOT/shared/wrappers/demo-root.sh" "$TREE/shared/wrappers/"
mkdir -p "$TREE/mcp" "$TREE/web"
cp "$ROOT/Makefile" "$TREE/"
cp "$ROOT/mcp/mcp" "$TREE/mcp/"
cp "$ROOT/web/web" "$TREE/web/"
# A symlink to the engine wrapper, as `make install` lays one on PATH.
ln -s "$TREE/wealthdb/wealthdb" "$TMP/bin/wealthdb"

# make_tag: the tag the Makefile resolves, from a dry run of `help`.
make_tag() {
    (cd "$TMP" && env "$@" make -f "$TREE/Makefile" -np help 2>/dev/null) | sed -n 's/^TAG := //p'
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
has "mcp defaults to latest" "wealthdb:latest" "$(knob_tag "$TREE/mcp/mcp")"
has "web defaults to latest" "wealthdb/metabase:latest" "$(knob_tag "$TREE/web/web")"

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
if out="$(cd "$TMP" && env -u WEALTHDB_IMAGE_TAG make -f "$TREE/Makefile" -n help 2>&1 >/dev/null)"; then
    bad "a malformed .image-tag stops make"
elif printf '%s\n' "$out" | grep -qF '*** no image tag'; then
    ok "a malformed .image-tag stops make"
else
    bad "a malformed .image-tag stops make, for another reason: $out"
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
untagged="$(cd "$ROOT" && grep -HE '^FROM[[:space:]]+wealthdb/' collectors/*/Dockerfile shared/images/*.Dockerfile |
    grep -vF ':${WEALTHDB_IMAGE_TAG}')"
if [ -z "$untagged" ]; then ok "every FROM a shared base takes the tag"
else bad "a FROM a shared base without the tag: $untagged"; fi

# A hash of the inputs, with no docker. A changed file or mode changes
# it, a missing path is part of it, so a deleted input is not an empty
# one, and a moved tree keeps it.
hdir="$TMP/hashsrc"
mkdir -p "$hdir/sub" "$hdir/empty"
printf 'a\n' > "$hdir/sub/f"
hash_of() {
    # shellcheck disable=SC1091
    . "$TREE/shared/wrappers/image-tag.sh"
    wealthdb_inputs_hash "$@"
}
hash1="$(hash_of "$hdir")"
printf 'b\n' > "$hdir/sub/f"
hash2="$(hash_of "$hdir")"
if [ -n "$hash1" ] && [ "$hash1" != "$hash2" ]; then
    ok "a changed file changes the inputs hash"
else
    bad "a changed file changes the inputs hash"
fi
hash_x="$(chmod +x "$hdir/sub/f" && hash_of "$hdir")"
if [ -n "$hash_x" ] && [ "$hash_x" != "$hash2" ]; then
    ok "a changed mode changes the inputs hash"
else
    bad "a changed mode changes the inputs hash"
fi
mkdir -p "$TMP/moved"
cp -Rp "$hdir" "$TMP/moved/"
if [ "$(hash_of "$TMP/moved/hashsrc")" = "$hash_x" ]; then
    ok "a moved tree keeps its inputs hash"
else
    bad "a moved tree keeps its inputs hash"
fi
ln -s "$TMP/no-such-file" "$hdir/sub/dangling"
hash_l="$(hash_of "$hdir" 2>/dev/null)"
if [ -n "$hash_l" ] && [ "$hash_l" != "$hash_x" ]; then
    ok "a dangling link is hashed as a link"
else
    bad "a dangling link is hashed as a link"
fi
rm "$hdir/sub/dangling"
hash3="$(hash_of "$hdir/empty" "$hdir/missing")"
hash4="$(hash_of "$hdir/empty" "$hdir/also-missing")"
if [ -n "$hash3" ] && [ "$hash3" != "$hash4" ]; then
    ok "a missing path is part of the inputs hash"
else
    bad "a missing path is part of the inputs hash"
fi
lit="$TMP/litcwd"
mkdir -p "$lit/v0.6.0"
lit_same="$(cd "$lit" && hash_of -- v0.6.0)"
lit_other="$(cd "$lit" && hash_of -- v0.6.1)"
lit_dir="$(cd "$lit" && hash_of v0.6.0)"
if [ -n "$lit_same" ] && [ "$lit_same" = "$(cd "$lit" && hash_of -- v0.6.0)" ] \
    && [ "$lit_same" != "$lit_other" ] && [ "$lit_same" != "$lit_dir" ]; then
    ok "a version string is hashed as text"
else
    bad "a version string is hashed as text"
fi

# An image whose label matches that hash is not built. The stub returns
# the label on the inputs inspect and STUB_ID on any other inspect, and
# prints BUILT and its argv on a build and PULLED and the image on a pull.
skipdir="$TMP/skipcol"
mkdir -p "$skipdir"
printf 'FROM wealthdb/base-python:${WEALTHDB_IMAGE_TAG}\n' > "$skipdir/Dockerfile"
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
if [ "${1:-}" = build ]; then
    printf 'BUILT\n'
    printf '%s\n' "$@"
    exit 0
fi
[ -n "${STUB_FAIL:-}" ] && exit 1
if [ "${1:-}" = pull ]; then
    printf 'PULLED %s\n' "${2:-}"
    exit 0
fi
for a in "$@"; do
    case "$a" in
        *wealthdb.inputs*) printf '%s\n' "${STUB_LABEL:-}"; exit 0 ;;
    esac
done
printf '%s\n' "${STUB_ID:-sha256:from}"
STUB
chmod +x "$TMP/bin/docker"
skiphash="$(hash_of "$skipdir" -- sha256:from)"
out="$(
    export PATH="$TMP/bin:$PATH" STUB_LABEL="$skiphash" WEALTHDB_IMAGE_TAG=feature
    NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(); HERE="$skipdir"
    unset TESTCOL_IMAGE
    # shellcheck disable=SC1091
    source "$TREE/shared/wrappers/wrapper-lib.sh"
    wrapper_init
    wrapper_build build
)"
if printf '%s\n' "$out" | grep -qx 'build: wealthdb/testcol:feature unchanged' \
    && ! printf '%s\n' "$out" | grep -qx BUILT; then
    ok "a labeled image is not rebuilt"
else
    bad "a labeled image is not rebuilt ($out)"
fi
out="$(
    export PATH="$TMP/bin:$PATH" STUB_LABEL=other WEALTHDB_IMAGE_TAG=feature
    NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(); HERE="$skipdir"
    unset TESTCOL_IMAGE
    # shellcheck disable=SC1091
    source "$TREE/shared/wrappers/wrapper-lib.sh"
    wrapper_init
    wrapper_build build
)"
if printf '%s\n' "$out" | grep -qx BUILT; then
    ok "a different label rebuilds the image"
else
    bad "a different label rebuilds the image ($out)"
fi
out="$(
    export PATH="$TMP/bin:$PATH" STUB_LABEL="$skiphash" WEALTHDB_IMAGE_TAG=feature
    NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(); HERE="$skipdir"
    unset TESTCOL_IMAGE
    # shellcheck disable=SC1091
    source "$TREE/shared/wrappers/wrapper-lib.sh"
    wrapper_init
    wrapper_build build --no-cache
)"
if printf '%s\n' "$out" | grep -qx BUILT; then
    ok "extra build arguments rebuild a labeled image"
else
    bad "extra build arguments rebuild a labeled image ($out)"
fi

out="$(
    export PATH="$TMP/bin:$PATH" STUB_LABEL="$skiphash" STUB_ID=sha256:rebuilt WEALTHDB_IMAGE_TAG=feature
    NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(); HERE="$skipdir"
    unset TESTCOL_IMAGE
    # shellcheck disable=SC1091
    source "$TREE/shared/wrappers/wrapper-lib.sh"
    wrapper_init
    wrapper_build build
)"
if printf '%s\n' "$out" | grep -qx BUILT; then
    ok "a rebuilt base image rebuilds the collector"
else
    bad "a rebuilt base image rebuilds the collector ($out)"
fi
from="$(. "$TREE/shared/wrappers/image-tag.sh"; WEALTHDB_IMAGE_TAG=feature wealthdb_from_ref "$skipdir/Dockerfile")"
if [ "$from" = wealthdb/base-python:feature ]; then
    ok "the FROM lookup fills in the image tag"
else
    bad "the FROM lookup fills in the image tag ($from)"
fi

# An empty hash (python3 failed) never matches: not a missing image, not
# an unlabeled one.
labeled() {
    (
        export PATH="$TMP/bin:$PATH"
        # shellcheck disable=SC1091
        . "$TREE/shared/wrappers/image-tag.sh"
        wealthdb_labeled wealthdb/testcol:feature ""
    )
}
if STUB_FAIL=1 labeled; then bad "an empty hash matches a missing image"
else ok "an empty hash matches no missing image"; fi
if STUB_LABEL= labeled; then bad "an empty hash matches an unlabeled image"
else ok "an empty hash matches no unlabeled image"; fi

# The engine wrapper skips a labeled image too. Outside git, the stamp
# is empty and the engine has no Dockerfile here, so its inputs are the
# wrapper's tree, three empty stamps and "missing".
enginehash="$(hash_of "$TREE/wealthdb" -- "" "" "" missing)"
out="$(run_env STUB_LABEL="$enginehash" WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" build)"
if [ "$out" = "build: wealthdb:feature unchanged" ]; then
    ok "the engine skips a labeled image"
else
    bad "the engine skips a labeled image ($out)"
fi
out="$(run_env STUB_LABEL="$enginehash" WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" build --no-cache)"
if printf '%s\n' "$out" | grep -qx BUILT && printf '%s\n' "$out" | grep -qxF "wealthdb.inputs=$enginehash"; then
    ok "an extra argument builds the engine, labeled"
else
    bad "an extra argument builds the engine, labeled ($out)"
fi
# A hash that fails builds without the skip and without a label.
mkdir -p "$TMP/nopy"
printf '#!/bin/sh\nexit 1\n' > "$TMP/nopy/python3"
chmod +x "$TMP/nopy/python3"
out="$(run_env PATH="$TMP/nopy:$TMP/bin:$PATH" STUB_LABEL= WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" build)"
if printf '%s\n' "$out" | grep -qx BUILT && ! printf '%s\n' "$out" | grep -q wealthdb.inputs; then
    ok "a failed hash builds the engine without a label"
else
    bad "a failed hash builds the engine without a label ($out)"
fi

# make base-images skips a base whose label matches its own inputs, and
# update-bases pulls and builds whatever the label says. The stub
# answers every label inspect with STUB_LABEL, so a label that matches
# base-python matches no other base.
mkdir -p "$TREE/shared/collectorkit" "$TREE/shared/images"
cp "$ROOT/shared/images/"* "$TREE/shared/images/"
cp "$ROOT/shared/.dockerignore" "$TREE/shared/"
printf 'x\n' > "$TREE/shared/collectorkit/f.py"
# base_hash: base-python's inputs, the helper files in shared/images
# among them.
base_hash() {
    (
        cd "$TREE" || exit 1
        set --
        for x in shared/images/*; do
            case "$x" in *.Dockerfile) ;; *) set -- "$@" "$x" ;; esac
        done
        hash_of shared/images/base-python.Dockerfile shared/collectorkit "$@" \
            shared/.dockerignore -- sha256:from
    )
}
bases() {
    (cd "$TREE" && run_env STUB_LABEL="$1" WEALTHDB_IMAGE_TAG=feature \
        make --no-print-directory "$2" 2>&1)
}
pyhash="$(base_hash)"
out="$(bases "$pyhash" base-images)"
if printf '%s\n' "$out" | grep -qx 'build: wealthdb/base-python:feature unchanged' \
    && ! printf '%s\n' "$out" | grep -q 'build base image wealthdb/base-python' \
    && printf '%s\n' "$out" | grep -q 'build base image wealthdb/base-playwright:feature'; then
    ok "base-images skips a labeled base and builds the others"
else
    bad "base-images skips a labeled base and builds the others ($out)"
fi
printf '# another base changed\n' >> "$TREE/shared/images/base-camoufox.Dockerfile"
if [ "$(base_hash)" = "$pyhash" ]; then
    ok "another base's Dockerfile is not an input of base-python"
else
    bad "another base's Dockerfile is not an input of base-python"
fi
out="$(bases "$pyhash" update-bases)"
if printf '%s\n' "$out" | grep -q 'build base image wealthdb/base-python:feature (--pull)' \
    && printf '%s\n' "$out" | grep -q '^PULLED python:'; then
    ok "update-bases pulls and builds a labeled base"
else
    bad "update-bases pulls and builds a labeled base ($out)"
fi

echo "  $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
