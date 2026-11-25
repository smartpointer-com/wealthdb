#!/usr/bin/env bash
# Behaviour test: WEALTHDB_IMAGE_TAG names the image every wrapper builds
# and runs — the engine wrapper, the Go toolchain wrapper and a docker
# collector — and the collector build hands the tag to its Dockerfile, so
# a second checkout builds on its own shared bases. Unset, the
# checkout's .image-tag names it, else it is latest; a malformed
# .image-tag stops the wrapper. A build whose inputs hash matches the
# image's wealthdb.inputs label is skipped, for the engine, a collector
# and the shared bases; an extra argument, a changed input or a rebuilt
# base image builds. base-camoufox takes its browser mode and the local
# archive, and a test build keeps one that carries the browser. A
# collector's inputs are its Dockerfile and requirements.txt alone. The
# wrappers run from a copy of the tree, so the checkout's own .image-tag
# plays no part. A stubbed docker on PATH records the argv; nothing real
# is built.
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

mkdir -p "$TMP/bin" "$TMP/cfg" "$TMP/data" "$TMP/cache"
# The wrappers under test, in the tree's layout.
TREE="$TMP/tree"
mkdir -p "$TREE/wealthdb" "$TREE/shared/wrappers"
cp "$ROOT/wealthdb/wealthdb" "$ROOT/wealthdb/wealthdb-go" "$TREE/wealthdb/"
cp "$ROOT/shared/wrappers/wrapper-lib.sh" "$ROOT/shared/wrappers/image-tag.sh" \
    "$ROOT/shared/wrappers/engine-mode.sh" \
    "$ROOT/shared/wrappers/envfile-lib.sh" "$ROOT/shared/wrappers/demo-root.sh" \
    "$ROOT/shared/wrappers/paths-lib.sh" "$ROOT/shared/wrappers/service-lib.sh" \
    "$ROOT/shared/wrappers/setup-lib.sh" "$TREE/shared/wrappers/"
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
has_item "engine runs wealthdb:latest by default" "wealthdb:latest" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" status)"
has_item "engine runs the tagged image" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" status)"
has_item "engine builds the tagged image" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" build)"
has_item "go wrapper runs the tagged image" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb-go" version)"
out="$(collector_build feature)"
has_item "collector builds the tagged image" "wealthdb/testcol:feature" "$out"
has_item "collector build passes the tag to FROM" "WEALTHDB_IMAGE_TAG=feature" "$out"
out="$(collector_build "")"
has_item "collector image defaults to latest" "wealthdb/testcol:latest" "$out"
has_item "collector base defaults to latest" "WEALTHDB_IMAGE_TAG=latest" "$out"

# The checkout's .image-tag names the tag when the variable is unset,
# and the variable still wins over it.
printf 'pinned\n' > "$TREE/.image-tag"
has_item "engine runs the .image-tag image" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" status)"
has_item "engine builds the .image-tag image" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" build)"
has_item "go wrapper runs the .image-tag image" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb-go" version)"
out="$(collector_build "")"
has_item "collector builds the .image-tag image" "wealthdb/testcol:pinned" "$out"
has_item "collector base follows .image-tag" "WEALTHDB_IMAGE_TAG=pinned" "$out"
has_item "the variable wins over .image-tag" "wealthdb:feature" \
    "$(run_env WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" status)"
has_item "the engine run through a symlink reads .image-tag" "wealthdb:pinned" \
    "$(run_env WEALTHDB_IMAGE_TAG= wealthdb status)"
has_item "make resolves .image-tag" "pinned" "$(make_tag -u WEALTHDB_IMAGE_TAG)"
has_item "make: the variable wins" "feature" "$(make_tag WEALTHDB_IMAGE_TAG=feature)"
has_item "mcp reads .image-tag" "wealthdb:pinned" "$(knob_tag "$TREE/mcp/mcp")"
has_item "web reads .image-tag" "wealthdb/metabase:pinned" "$(knob_tag "$TREE/web/web")"
rm -f "$TREE/.image-tag"
has_item "make defaults to latest" "latest" "$(make_tag -u WEALTHDB_IMAGE_TAG)"
has_item "mcp defaults to latest" "wealthdb:latest" "$(knob_tag "$TREE/mcp/mcp")"
has_item "web defaults to latest" "wealthdb/metabase:latest" "$(knob_tag "$TREE/web/web")"

# A malformed .image-tag stops the wrapper before docker runs: it never
# falls back to latest.
printf 'not a tag\n' > "$TREE/.image-tag"
out="$(run_env WEALTHDB_IMAGE_TAG= "$TREE/wealthdb/wealthdb" build 2>&1)"; rc=$?
if [ "$rc" -eq 2 ] && ! grep -q '^build$' <<<"$out"; then
    ok "a malformed .image-tag stops the engine wrapper"
else
    bad "a malformed .image-tag stops the engine wrapper (exit $rc: $out)"
fi
out="$(collector_build "" 2>&1)"; rc=$?
if [ "$rc" -eq 2 ]; then ok "a malformed .image-tag stops a collector wrapper"
else bad "a malformed .image-tag stops a collector wrapper (exit $rc)"; fi
if out="$(cd "$TMP" && env -u WEALTHDB_IMAGE_TAG make -f "$TREE/Makefile" -n help 2>&1 >/dev/null)"; then
    bad "a malformed .image-tag stops make"
elif grep -qF '*** no image tag' <<<"$out"; then
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

# --ignore leaves out what a .dockerignore leaves out of the context:
# a name at the top and a suffix at any depth. A line it cannot follow
# fails the hash, and a missing file leaves nothing out.
idir="$TMP/ignsrc"
mkdir -p "$idir/docs" "$idir/cmd"
printf 'docs\n**/*.md\n' > "$TMP/ign"
printf 'g\n' > "$idir/cmd/m.go"
printf 'd\n' > "$idir/docs/x.txt"
printf 'r\n' > "$idir/cmd/n.md"
ign1="$(hash_of --ignore "$TMP/ign" "$idir")"
printf 'e\n' >> "$idir/docs/x.txt"; printf 'e\n' >> "$idir/cmd/n.md"; printf 'n\n' > "$idir/README.md"
ign2="$(hash_of --ignore "$TMP/ign" "$idir")"
printf 'e\n' >> "$idir/cmd/m.go"
ign3="$(hash_of --ignore "$TMP/ign" "$idir")"
if [ -n "$ign1" ] && [ "$ign1" = "$ign2" ] && [ "$ign2" != "$ign3" ]; then
    ok "--ignore leaves out what the .dockerignore leaves out, and only that"
else
    bad "--ignore leaves out what the .dockerignore leaves out, and only that"
fi
printf 'docs\n!docs/keep.md\n' > "$TMP/ign-bad"
if ign_bad="$(hash_of --ignore "$TMP/ign-bad" "$idir" 2>/dev/null)"; then
    bad "a .dockerignore line --ignore cannot follow fails the hash (got $ign_bad)"
else
    ok "a .dockerignore line --ignore cannot follow fails the hash"
fi
if [ "$(hash_of --ignore "$TMP/no-such-ignore" "$idir")" = "$(hash_of "$idir")" ]; then
    ok "a missing .dockerignore leaves nothing out"
else
    bad "a missing .dockerignore leaves nothing out"
fi

# An image whose label matches that hash is not built. The stub returns
# the label on the inputs inspect, STUB_ENV on the environment inspect
# (an image that is not there with STUB_NO_IMAGE) and STUB_ID on any
# other inspect, and prints BUILT and its argv on a build and PULLED and
# the image on a pull. STUB_FAIL fails every call but a build, with
# Docker's message for a missing image under STUB_MISSING and for a
# daemon that is down under STUB_DOWN.
skipdir="$TMP/skipcol"
mkdir -p "$skipdir"
printf 'FROM wealthdb/base-python:${WEALTHDB_IMAGE_TAG}\n' > "$skipdir/Dockerfile"
printf 'requests>=2\n' > "$skipdir/requirements.txt"
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
if [ "${1:-}" = build ]; then
    printf 'BUILT\n'
    printf '%s\n' "$@"
    exit 0
fi
if [ -n "${STUB_MISSING:-}" ] && [ "${1:-} ${2:-}" = "image inspect" ]; then
    echo "Error response from daemon: No such image: ${*: -1}" >&2
fi
if [ -n "${STUB_DOWN:-}" ]; then
    echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2
fi
[ -n "${STUB_FAIL:-}" ] && exit 1
if [ "${1:-}" = pull ]; then
    printf 'PULLED %s\n' "${2:-}"
    exit 0
fi
for a in "$@"; do
    case "$a" in
        *wealthdb.inputs*) printf '%s\n' "${STUB_LABEL:-}"; exit 0 ;;
        *.Config.Env*)
            [ -n "${STUB_NO_IMAGE:-}" ] && exit 1
            printf '%s\n' "${STUB_ENV:-}"; exit 0 ;;
    esac
done
printf '%s\n' "${STUB_ID:-sha256:from}"
STUB
chmod +x "$TMP/bin/docker"
# skip_build LABEL [ARGS...]: `build ARGS` of a collector in skipdir
# whose image carries LABEL.
skip_build() {
    (
        export PATH="$TMP/bin:$PATH" STUB_LABEL="$1" WEALTHDB_IMAGE_TAG=feature
        shift
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(); HERE="$skipdir"
        unset TESTCOL_IMAGE
        # shellcheck disable=SC1091
        source "$TREE/shared/wrappers/wrapper-lib.sh"
        wrapper_init
        wrapper_build build "$@"
    )
}
skiphash="$(hash_of "$skipdir/Dockerfile" "$skipdir/requirements.txt" -- sha256:from)"
out="$(skip_build "$skiphash")"
if grep -qx 'build: wealthdb/testcol:feature unchanged' <<<"$out" \
    && ! grep -qx BUILT <<<"$out"; then
    ok "a labeled image is not rebuilt"
else
    bad "a labeled image is not rebuilt ($out)"
fi
out="$(skip_build other)"
if grep -qx BUILT <<<"$out"; then
    ok "a different label rebuilds the image"
else
    bad "a different label rebuilds the image ($out)"
fi
out="$(skip_build "$skiphash" --no-cache)"
if grep -qx BUILT <<<"$out"; then
    ok "extra build arguments rebuild a labeled image"
else
    bad "extra build arguments rebuild a labeled image ($out)"
fi
out="$(STUB_ID=sha256:rebuilt skip_build "$skiphash")"
if grep -qx BUILT <<<"$out"; then
    ok "a rebuilt base image rebuilds the collector"
else
    bad "a rebuilt base image rebuilds the collector ($out)"
fi
# The image holds no source: its inputs are the Dockerfile and
# requirements.txt, so a doc or code edit beside them builds nothing and
# an edit to requirements.txt builds.
printf 'notes\n' > "$skipdir/README.md"
printf 'print(1)\n' > "$skipdir/download.py"
out="$(skip_build "$skiphash")"
if grep -qx 'build: wealthdb/testcol:feature unchanged' <<<"$out"; then
    ok "an edit to the code or docs builds nothing"
else
    bad "an edit to the code or docs builds nothing ($out)"
fi
printf 'requests>=3\n' > "$skipdir/requirements.txt"
out="$(skip_build "$skiphash")"
if grep -qx BUILT <<<"$out"; then
    ok "a requirements edit rebuilds the image"
else
    bad "a requirements edit rebuilds the image ($out)"
fi

# A wealthdb/ base that is not local stops the build: `docker build`
# would ask a registry for it. STUB_FAIL fails every inspect.
out="$(STUB_FAIL=1 skip_build "" 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "a base image that is not local: build exits 2" || bad "a base image that is not local: build exits $rc"
has "...naming the make target that builds the bases first" \
    "testcol: the base image wealthdb/base-python:feature is not built yet. In a checkout, run: make build-testcol" "$out"
lacks "...and docker build never runs" "BUILT" "$out"
# An installed copy has no Makefile and no base images' Dockerfiles.
INST="$TMP/installed"
mkdir -p "$INST/shared/wrappers"
cp "$TREE/shared/wrappers/"*.sh "$INST/shared/wrappers/"
# inst_verb VERB: VERB of the collector in skipdir, from the installed
# copy.
inst_verb() {
    (
        export PATH="$TMP/bin:$PATH" WEALTHDB_IMAGE_TAG=feature HOME="$TMP/home"
        export XDG_DATA_HOME="$TMP/data" XDG_CACHE_HOME="$TMP/cache"
        mkdir -p "$HOME/.secrets"
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(); HERE="$skipdir"
        unset TESTCOL_IMAGE WEALTHDB_SECRETS_DIR WEALTHDB_DATA_ROOT
        # shellcheck disable=SC1091
        source "$INST/shared/wrappers/wrapper-lib.sh"
        wrapper_init
        wrapper_main "$@"
    )
}
out="$(STUB_FAIL=1 inst_verb build 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "an installed copy, no base image: build exits 2" || bad "an installed copy, no base image: build exits $rc"
has "...naming a checkout, since the copy builds none" \
    "testcol: the base image wealthdb/base-python:feature is not built yet. An installed copy builds no image. Run this collector from a checkout." "$out"
lacks "...and docker build never runs" "BUILT" "$out"
lacks "...and no make target, which the copy does not have" "make build-" "$out"
out="$(STUB_FAIL=1 STUB_MISSING=1 inst_verb load 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "an installed copy, no image: a verb exits 2" || bad "an installed copy, no image: a verb exits $rc"
has "...naming a checkout" \
    "testcol: the image wealthdb/testcol:feature is not built yet. An installed copy builds no image. Run this collector from a checkout." "$out"
lacks "...and no build command of the wrapper" "testcol build" "$out"
# A base of another name is the build's to fetch.
printf 'FROM python:3.12-slim\n' > "$skipdir/Dockerfile"
out="$(STUB_FAIL=1 skip_build "" 2>&1)"
has "an upstream base that is not local is left to docker build" "BUILT" "$out"
printf 'FROM wealthdb/base-python:${WEALTHDB_IMAGE_TAG}\n' > "$skipdir/Dockerfile"

# wealthdb_require_local_bases and wealthdb_require_image on their own.
# The stub fails an inspect with STUB_MISSING (no such image) or with
# STUB_DOWN (no daemon), and prints the image it was asked for.
require() { # FUNCTION DOCKERFILE-OR-IMAGE
    (
        export PATH="$TMP/bin:$PATH" WEALTHDB_IMAGE_TAG=feature
        # shellcheck disable=SC1091
        . "$TREE/shared/wrappers/image-tag.sh"
        "$1" who "$2" "Do this." 2>&1
        echo "rc=$?"
    )
}
printf 'FROM python:3.12-slim AS tools\nFROM wealthdb/base-x:${WEALTHDB_IMAGE_TAG} AS base\nFROM base\n' > "$TMP/stages.Dockerfile"
out="$(STUB_MISSING=1 STUB_FAIL=1 require wealthdb_require_local_bases "$TMP/stages.Dockerfile")"
has "a wealthdb/ base in a later FROM line is checked too" \
    "who: the base image wealthdb/base-x:feature is not built yet. Do this." "$out"
has "...and refused" "rc=1" "$out"
out="$(require wealthdb_require_local_bases "$TMP/stages.Dockerfile")"
[ "$out" = "rc=0" ] && ok "local bases: nothing is said" || bad "local bases: $out"
out="$(STUB_DOWN=1 STUB_FAIL=1 require wealthdb_require_local_bases "$TMP/stages.Dockerfile")"
has "a Docker that does not answer: no build, with Docker's message" \
    "who: Docker could not say whether the base image wealthdb/base-x:feature is built: Cannot connect to the Docker daemon" "$out"
has "...refused" "rc=1" "$out"
out="$(STUB_FAIL=1 require wealthdb_require_local_bases "$TMP/no-such.Dockerfile")"
[ "$out" = "rc=0" ] && ok "no Dockerfile: left to the build, which has none" || bad "no Dockerfile: $out"
out="$(STUB_MISSING=1 STUB_FAIL=1 require wealthdb_require_image wealthdb/x:feature)"
has "a missing image: the message ends in the hint" "who: the image wealthdb/x:feature is not built yet. Do this." "$out"
has "...refused" "rc=1" "$out"
out="$(STUB_DOWN=1 STUB_FAIL=1 require wealthdb_require_image wealthdb/x:feature)"
[ "$out" = "rc=0" ] && ok "a Docker that does not answer is left to the run" || bad "a Docker that does not answer: $out"

# Every Dockerfile of the checkout FROM a wealthdb/ image is one that
# the check reads and that a guarded build builds: a collector's
# (wrapper_build) or a shared base's (the Makefile's BUILD_BASES).
unguarded="" unread=""
while IFS= read -r f; do
    case "${f#"$ROOT"/}" in
        collectors/*/Dockerfile | shared/images/base-*.Dockerfile) ;;
        *) unguarded="$unguarded ${f#"$ROOT"/}" ;;
    esac
    out="$(STUB_MISSING=1 STUB_FAIL=1 require wealthdb_require_local_bases "$f")"
    grep -qx 'rc=1' <<<"$out" || unread="$unread ${f#"$ROOT"/}"
done < <(grep -lE '^FROM[[:space:]].*wealthdb/' "$ROOT"/*/Dockerfile "$ROOT"/collectors/*/Dockerfile \
    "$ROOT"/shared/images/*.Dockerfile 2>/dev/null)
[ -z "$unguarded" ] && ok "only collectors and shared bases build FROM a wealthdb/ image" ||
    bad "a build with no check builds FROM a wealthdb/ image:$unguarded"
[ -z "$unread" ] && ok "the check refuses each of them when its base is not local" ||
    bad "the check does not read the wealthdb/ base of:$unread"
for site in "$ROOT/shared/wrappers/wrapper-lib.sh" "$ROOT/Makefile"; do
    grep -q 'wealthdb_require_local_bases' "$site" && ok "${site#"$ROOT"/} runs the check" ||
        bad "${site#"$ROOT"/} builds with no check"
done

# Every `docker run` passes --pull=never: each argv starts `run -d` or
# `run --rm`, or, in wrapper-lib.sh, on a line of its own with --rm.
sites="$(grep -nE '(^|[^[:alnum:]_-])run[[:space:]]+(-d|--rm)|^[[:space:]]*--rm[[:space:]]' \
    "$ROOT/Makefile" "$ROOT/wealthdb/wealthdb" "$ROOT/wealthdb/wealthdb-go" "$ROOT/web/web" "$ROOT/mcp/mcp" \
    "$ROOT"/shared/wrappers/*.sh "$ROOT"/bin/* "$ROOT"/shared/native/* |
    grep -vE '^[^:]+:[0-9]+:[[:space:]]*#')"
n="$(grep -c . <<<"$sites")"
[ "$n" -ge 10 ] && ok "the docker run sites are found ($n)" || bad "only $n docker run sites found: the pattern has aged"
pulls="$(grep -v -- '--pull=never' <<<"$sites")"
[ -z "$pulls" ] && ok "every docker run passes --pull=never" || bad "a docker run that may pull: $pulls"

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
if grep -qx BUILT <<<"$out" && grep -qxF "wealthdb.inputs=$enginehash" <<<"$out"; then
    ok "an extra argument builds the engine, labeled"
else
    bad "an extra argument builds the engine, labeled ($out)"
fi
# A hash that fails builds without the skip and without a label.
mkdir -p "$TMP/nopy"
printf '#!/bin/sh\nexit 1\n' > "$TMP/nopy/python3"
chmod +x "$TMP/nopy/python3"
out="$(run_env PATH="$TMP/nopy:$TMP/bin:$PATH" STUB_LABEL= WEALTHDB_IMAGE_TAG=feature "$TREE/wealthdb/wealthdb" build)"
if grep -qx BUILT <<<"$out" && ! grep -q wealthdb.inputs <<<"$out"; then
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
if grep -qx 'build: wealthdb/base-python:feature unchanged' <<<"$out" \
    && ! grep -q 'build base image wealthdb/base-python' <<<"$out" \
    && grep -q 'build base image wealthdb/base-playwright:feature' <<<"$out"; then
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
if grep -q 'build base image wealthdb/base-python:feature (--pull)' <<<"$out" \
    && grep -q '^PULLED python:' <<<"$out"; then
    ok "update-bases pulls and builds a labeled base"
else
    bad "update-bases pulls and builds a labeled base ($out)"
fi

# base-camoufox takes its browser mode as a build argument, and in
# bundle mode the local archive's directory as a build context when that
# directory exists. A none build never replaces a base-camoufox that
# carries the browser, up to date or stale; a bundle build replaces one
# without it.
camo_hash() {
    (
        mode="$1"
        cd "$TREE" || exit 1
        set --
        for x in shared/images/*; do
            case "$x" in *.Dockerfile) ;; *) set -- "$@" "$x" ;; esac
        done
        hash_of shared/images/base-camoufox.Dockerfile shared/collectorkit "$@" \
            shared/.dockerignore -- sha256:from "browser=$mode"
    )
}
camo_bases() {
    label="$1"
    shift
    (cd "$TREE" && run_env STUB_LABEL="$label" WEALTHDB_IMAGE_TAG=feature \
        make --no-print-directory "$@" 2>&1)
}
# update-bases prints each build's argv; base-images keeps it in a log.
camo_block() { sed -n '/build base image wealthdb\/base-camoufox/,$p' <<<"$1"; }
out="$(camo_bases none update-bases)"
has_item "base-camoufox bakes in the browser by default" "CAMOUFOX_BROWSER=bundle" "$(camo_block "$out")"
lacks_item "no archive directory, no archive context" "--build-context" "$(camo_block "$out")"
lacks "the other bases take no browser mode" "CAMOUFOX_BROWSER" \
    "$(sed '/build base image wealthdb\/base-camoufox/,$d' <<<"$out")"
mkdir -p "$TMP/cache/wealthdb/camoufox"
out="$(camo_bases none update-bases)"
has_item "the archive directory is a build context" \
    "camoufox-archive=$TMP/cache/wealthdb/camoufox" "$(camo_block "$out")"
out="$(camo_bases none update-bases CAMOUFOX_BROWSER=none)"
has_item "a none build leaves the browser out" "CAMOUFOX_BROWSER=none" "$(camo_block "$out")"
lacks_item "a none build takes no archive context" "--build-context" "$(camo_block "$out")"
out="$(STUB_ENV=WEALTHDB_CAMOUFOX_BROWSER=bundle \
    camo_bases "$(camo_hash bundle)" base-images CAMOUFOX_BROWSER=none)"
has "a none build keeps a base-camoufox with the browser" \
    "build: wealthdb/base-camoufox:feature unchanged" "$out"
# A stale one is rebuilt with the browser, and so is one from before
# the browser mode, which has no such variable. Only one built without
# the browser, or none at all, is built without it.
kept="build base image wealthdb/base-camoufox:feature (browser: bundle, as the image there carries it)"
out="$(STUB_ENV=WEALTHDB_CAMOUFOX_BROWSER=bundle camo_bases stale base-images CAMOUFOX_BROWSER=none)"
has "a none build rebuilds a stale base-camoufox with the browser" "$kept" "$out"
out="$(STUB_ENV=PATH=/usr/bin camo_bases stale base-images CAMOUFOX_BROWSER=none)"
has "a none build rebuilds a base-camoufox without the mode variable with the browser" "$kept" "$out"
out="$(STUB_ENV=WEALTHDB_CAMOUFOX_BROWSER=none camo_bases stale base-images CAMOUFOX_BROWSER=none)"
has "a none build rebuilds a base-camoufox without the browser as none" \
    "build base image wealthdb/base-camoufox:feature (browser: none)" "$out"
out="$(STUB_NO_IMAGE=1 camo_bases stale base-images CAMOUFOX_BROWSER=none)"
has "a none build with no base-camoufox there builds none" \
    "build base image wealthdb/base-camoufox:feature (browser: none)" "$out"
out="$(camo_bases "$(camo_hash none)" base-images)"
has "a bundle build replaces a base-camoufox without the browser" \
    "build base image wealthdb/base-camoufox:feature (browser: bundle)" "$out"
out="$(camo_bases "$(camo_hash bundle)" update-bases CAMOUFOX_BROWSER=none)"
has "update-bases builds a none base-camoufox over one with the browser" \
    "build base image wealthdb/base-camoufox:feature (browser: none)" "$out"
# A base FROM a wealthdb/ image is built only when that image is local:
# `docker build` would ask a registry for the name.
out="$(STUB_MISSING=1 STUB_FAIL=1 camo_bases stale base-images)"; rc=$?
[ "$rc" != 0 ] && ok "base-images fails at a base whose wealthdb/ parent is not local" ||
    bad "base-images went on with no local parent"
has "...naming the parent and the command" \
    "base-camoufox: the base image wealthdb/base-playwright:feature is not built yet. Run: make base-images" "$out"
lacks "...and never builds it" "build base image wealthdb/base-camoufox" "$out"
has "...after the bases FROM an upstream image" "build base image wealthdb/base-playwright:feature" "$out"
rm -r "$TMP/cache/wealthdb/camoufox"

summary "image tag tests"
