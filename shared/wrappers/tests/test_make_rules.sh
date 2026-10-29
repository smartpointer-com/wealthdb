#!/usr/bin/env bash
# Behaviour test of the Makefile's generated collector rules, from dry
# runs in a copy of the tree holding one collector of each runtime:
#   - only a Docker collector's build depends on the shared bases;
#   - a Docker collector's tests run in its image directly, not through
#     its wrapper: no secrets or data mount and no fixed container name;
#     they run on a tmpfs copy of the checkout at /app;
#   - a host-venv or hybrid collector's tests run on its venv;
#   - clean-wealthdb clears the Go cache wealthdb-go uses;
#   - test-demo's scratch roots are the image tag's own, and clean-demo
#     removes that root only when it holds the tests' marker;
#   - lint reinstalls ruff when its pin changes, and update-venvs stops
#     at the first pip that fails.
# Nothing is built: make prints, and a stub docker prints. clean-demo,
# lint-python and update-venvs run for real, on roots and stub venvs
# under the temp dir.
set -uo pipefail
# The makes this test starts read only what it gives them: an outer
# make's flags and command-line variables (make test-wrappers X=…) stay
# out.
unset MAKEFLAGS MFLAGS MAKELEVEL

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

# The physical path: make's working dir resolves symlinks such as macOS's
# /var -> /private/var, and the /src mount is named from it.
TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$TMP"' EXIT
TREE="$TMP/tree"
mkdir -p "$TREE/shared/wrappers" "$TREE/wealthdb" "$TMP/bin"
cp "$ROOT/Makefile" "$TREE/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$ROOT/shared/wrappers/demo-root.sh" "$TREE/shared/wrappers/"
cp "$ROOT/wealthdb/wealthdb-go" "$TREE/wealthdb/"
# One collector per runtime: Docker, host venv, and a hybrid.
for c in dock host hyb; do mkdir -p "$TREE/collectors/$c/tests"; done
touch "$TREE/collectors/dock/Dockerfile" "$TREE/collectors/hyb/Dockerfile" \
      "$TREE/collectors/hyb/.host-venv" \
      "$TREE/collectors/host/requirements.txt" "$TREE/collectors/hyb/requirements.txt"
mkdir -p "$TREE/collectors/host/.venv/bin" "$TREE/collectors/hyb/.venv/bin"
printf '#!/bin/sh\n' > "$TREE/collectors/host/.venv/bin/python"
cp "$TREE/collectors/host/.venv/bin/python" "$TREE/collectors/hyb/.venv/bin/python"
chmod +x "$TREE/collectors"/*/.venv/bin/python

# dry TARGET: the recipe make would run for TARGET, as one line per command.
dry() { (cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" make -n "$1" 2>&1); }

echo "make collector rules:"
has "a Docker collector's build builds the bases" "build base image" "$(dry build-dock)"
lacks "a host-venv collector's build builds no base" "build base image" "$(dry build-host)"
out="$(dry test-dock)"
has "a Docker collector tests in its image" 'docker run --rm' "$out"
has "the checkout is mounted read-only" "-v \"$TREE/collectors/dock:/src:ro\"" "$out"
has "the tests run on a tmpfs at /app, the test user's" '--tmpfs "/app:exec,uid=$(id -u),gid=$(id -g)"' "$out"
has "the image's entrypoint is not run, since the tmpfs hides it" "--entrypoint sh" "$out"
has "the checkout is copied to /app, then the tests run through its entrypoint" \
    "-c 'cp -R /src/. /app && exec /app/entrypoint.sh sh -c \"cd /app && pytest -q -p no:cacheprovider\"'" "$out"
lacks "the tests do not run on the mount" ":/app" "$(printf '%s' "$out" | grep -o -- '-v "[^"]*"')"
lacks "the test mounts no secrets" ":/secrets" "$out"
lacks "the test mounts no data" ":/data" "$out"
lacks "the test takes no fixed container name" "--name" "$out"
lacks "the test does not go through the wrapper" "collectors/dock/dock sh" "$out"
has "a host-venv collector tests on its venv" \
    "collectors/host/.venv/bin/python -m pytest" "$(dry test-host)"
has "a hybrid tests on its host venv" \
    "collectors/hyb/.venv/bin/python -m pytest" "$(dry test-hyb)"

# The Go cache wealthdb-go mounts, from its argv under a stub docker.
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@"
STUB
chmod +x "$TMP/bin/docker"
gocache="$(env PATH="$TMP/bin:$PATH" WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" \
    "$TREE/wealthdb/wealthdb-go" version | sed -n 's/^GOCACHE=//p')"
has "clean-wealthdb clears wealthdb-go's build cache" "rm -rf $gocache" "$(dry clean-wealthdb)"

demo="$TMP/cache/wealthdb/demo-test-t"
has "test-demo's scratch roots are the tag's own" \
    "WEALTHDB_DEMO_TEST_ROOT=\"$demo\"" "$(dry test-demo)"
has "clean-demo removes the same root" "rm -rf \"$demo\"" "$(dry clean-demo)"
has "clean runs clean-demo" "rm -rf \"$demo\"" "$(dry clean)"
# clean-demo for real: the tag's marked root goes; an unmarked dir at
# that path and another tag's marked root stay.
clean_demo() { (cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" make clean-demo >/dev/null 2>&1); }
mkdir -p "$demo/a" "$TMP/cache/wealthdb/demo-test-u/a"
touch "$demo/.wealthdb-demo-tests" "$TMP/cache/wealthdb/demo-test-u/.wealthdb-demo-tests"
clean_demo
if [ ! -e "$demo" ]; then ok "clean-demo removes the tag's marked root"; else bad "clean-demo left $demo"; fi
if [ -d "$TMP/cache/wealthdb/demo-test-u/a" ]; then ok "clean-demo leaves another tag's root"; else bad "clean-demo removed another tag's root"; fi
mkdir -p "$demo/a"
clean_demo
if [ -d "$demo/a" ]; then ok "clean-demo leaves a dir without the marker"; else bad "clean-demo removed an unmarked $demo"; fi

# lint-python and update-venvs for real, on stub venvs whose pip and
# python record their calls.
echo "make venv rules:"
real() { (cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" make --no-print-directory "$@" 2>&1); }
lint="$TREE/shared/lint"
mkdir -p "$lint/.venv/bin"
printf 'ruff>=1,<2\n' > "$lint/requirements.txt"
printf '#!/bin/sh\necho "pip $*" >> "%s"\n' "$TMP/pip.log" > "$lint/.venv/bin/pip"
printf '#!/bin/sh\nexit 0\n' > "$lint/.venv/bin/ruff"
chmod +x "$lint/.venv/bin/pip" "$lint/.venv/bin/ruff"
pips() { cat "$TMP/pip.log" 2>/dev/null | wc -l | tr -d ' '; }
real lint-python >/dev/null
[ "$(pips)" = 1 ] && ok "lint installs ruff into a venv without a stamp" || bad "pip calls: $(pips)"
real lint-python >/dev/null
[ "$(pips)" = 1 ] && ok "lint skips pip while the stamp matches" || bad "pip calls: $(pips)"
printf 'ruff>=2,<3\n' > "$lint/requirements.txt"
real lint-python >/dev/null
[ "$(pips)" = 2 ] && ok "lint reinstalls after a pin edit" || bad "pip calls: $(pips)"
rm -rf "$lint/.venv"

# A failed pip stops update-venvs at once, with a failure status.
printf '#!/bin/sh\nexit 1\n' > "$TREE/collectors/host/.venv/bin/python"
out="$(real update-venvs)"; rc=$?
[ "$rc" != 0 ] && ok "a failed pip fails update-venvs" || bad "update-venvs exit 0 over a failed pip"
lacks "a failed pip stops before the next venv" "update collectors/hyb/.venv" "$out"
printf '#!/bin/sh\n' > "$TREE/collectors/host/.venv/bin/python"
out="$(real update-venvs)"; rc=$?
[ "$rc" = 0 ] && ok "update-venvs succeeds over working venvs" || bad "update-venvs exit $rc: $out"
has "update-venvs updates every collector venv" "update collectors/hyb/.venv" "$out"

summary "make rule tests"
