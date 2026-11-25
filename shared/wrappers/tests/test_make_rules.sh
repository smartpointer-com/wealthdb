#!/usr/bin/env bash
# Behaviour test of the Makefile's generated collector rules, from dry
# runs in a copy of the tree holding one collector of each runtime:
#   - only a Docker collector's build depends on the shared bases, and
#     its tests build base-camoufox without the browser;
#   - a Docker collector's tests run in its image directly, not through
#     its wrapper: no secrets or data mount and no fixed container name;
#     they run on a tmpfs copy of the checkout at /app;
#   - a host-venv or hybrid collector's tests run on its venv, and fail
#     when it has none;
#   - clean-wealthdb clears the Go cache wealthdb-go uses, and
#     test-wealthdb takes its packages and go test flags from
#     WEALTHDB_PKGS and GO_TEST_FLAGS; its catalogue check leaves no
#     scratch folder in the cache;
#   - test-demo's scratch roots are the image tag's own, and clean-demo
#     removes that root only when it holds the tests' marker;
#   - lint reinstalls ruff when its pin changes, a collector's venv
#     reinstalls only when its requirements change, and update-venvs
#     stops at the first pip that fails;
#   - make test runs the suites it is given, prints each one's log after
#     its name and seconds, and fails when one fails; a subset says how
#     many of the suites it runs, every suite is the default, and an
#     empty SUITES is an error;
#   - site-shots runs as the caller, with the checkout read-only and no
#     secrets;
#   - an exported SHOW prints no value from make pii;
#   - site-publish exports the env file's token, and refuses a file that
#     does not parse without printing its lines.
# Nothing is built: make prints, and a stub docker prints. test-wealthdb,
# clean-demo, lint-python, build-host, update-venvs, test and
# site-publish run for real, on a stub engine, roots, stub venvs, stub
# suites and a stub interpreter under the temp dir.
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
cp "$ROOT/shared/wrappers/image-tag.sh" "$ROOT/shared/wrappers/demo-root.sh" \
   "$ROOT/shared/wrappers/envfile-lib.sh" "$ROOT/shared/wrappers/fanout.sh" \
   "$ROOT/shared/wrappers/engine-mode.sh" "$TREE/shared/wrappers/"
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
dry() { (cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" make -n "$@" 2>&1); }

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
has "a Docker collector's build bakes the Camoufox browser in" "mode=bundle;" "$(dry build-dock)"
has "a Docker collector's tests build base-camoufox without the browser" "mode=none;" "$out"
has "a host-venv collector tests on its venv" \
    "collectors/host/.venv/bin/python -m pytest" "$(dry test-host)"
has "a hybrid tests on its host venv" \
    "collectors/hyb/.venv/bin/python -m pytest" "$(dry test-hyb)"

# A host collector with tests and nothing to build has no venv: its
# tests fail rather than pass unrun.
mkdir -p "$TREE/collectors/bare/tests"
out="$(cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" make test-bare 2>&1)"; rc=$?
[ "$rc" != 0 ] && ok "tests without a venv fail" || bad "test-bare exit 0: $out"
has "the failure says why" "bare: cannot run tests (no venv)" "$out"
rm -rf "$TREE/collectors/bare"

# The Go cache wealthdb-go mounts, from its argv under a stub docker.
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@"
STUB
chmod +x "$TMP/bin/docker"
gocache="$(env PATH="$TMP/bin:$PATH" WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" \
    "$TREE/wealthdb/wealthdb-go" version | sed -n 's/^GOCACHE=//p')"
has "clean-wealthdb clears wealthdb-go's build cache" "rm -rf $gocache" "$(dry clean-wealthdb)"
has_item "test-wealthdb tests every package by default" \
    "wealthdb/wealthdb-test  ./..." "$(dry test-wealthdb)"
has_item "WEALTHDB_PKGS and GO_TEST_FLAGS reach go test" \
    "wealthdb/wealthdb-test -count=1 ./internal/gold/..." \
    "$(dry test-wealthdb WEALTHDB_PKGS=./internal/gold/... GO_TEST_FLAGS=-count=1)"
# test-wealthdb for real, on a stub engine and a stub go test. The stub
# engine prints the catalogue under test, and records the scratch root
# the check gives it.
catroot="$TMP/cache/wealthdb/catalogue-check-t"
cat > "$TREE/wealthdb/wealthdb" <<'STUB'
#!/bin/sh
if [ "$1" = config-env ]; then
    [ -d "$XDG_CONFIG_HOME" ] && echo "$XDG_CONFIG_HOME" > "$STUB_ROOT_SEEN"
    for n in ${STUB_CATALOGUE:-}; do echo "$n"; done
fi
STUB
printf '#!/bin/sh\n' > "$TREE/wealthdb/wealthdb-test"
chmod +x "$TREE/wealthdb/wealthdb" "$TREE/wealthdb/wealthdb-test"
test_wealthdb() {
    (cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" STUB_ROOT_SEEN="$TMP/cat-seen" "$@" \
        make --no-print-directory test-wealthdb 2>&1)
}
out="$(test_wealthdb)"; rc=$?
[ "$rc" = 0 ] && ok "test-wealthdb passes on a catalogue that names the collectors" || bad "test-wealthdb exit $rc: $out"
[ "$(cat "$TMP/cat-seen" 2>/dev/null)" = "$catroot" ] && ok "the catalogue check runs the engine on a scratch root in the cache" ||
    bad "the engine's root: $(cat "$TMP/cat-seen" 2>/dev/null)"
[ ! -e "$catroot" ] && ok "...which is gone when the check ends" || bad "the catalogue check left $catroot"
# What a killed run left is no part of the next one.
mkdir -p "$catroot/old"
out="$(test_wealthdb STUB_CATALOGUE=ghost)"; rc=$?
[ "$rc" != 0 ] && ok "a catalogue with a collector that has no wrapper fails" || bad "test-wealthdb passed: $out"
has "...printing the difference" "> ghost" "$out"
[ ! -e "$catroot" ] && ok "...and removing the scratch root too" || bad "a failed catalogue check left $catroot"
rm -f "$TREE/wealthdb/wealthdb" "$TREE/wealthdb/wealthdb-test"

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

# A host collector's venv: one pip for its requirements and collectorkit,
# none while neither changes.
mkdir -p "$TREE/shared/collectorkit"
printf '[project]\n' > "$TREE/shared/collectorkit/pyproject.toml"
printf 'requests\n' > "$TREE/collectors/host/requirements.txt"
printf '#!/bin/sh\necho "pip $*" >> "%s"\n' "$TMP/pip.log" > "$TREE/collectors/host/.venv/bin/pip"
chmod +x "$TREE/collectors/host/.venv/bin/pip"
: > "$TMP/pip.log"
real build-host >/dev/null
[ "$(pips)" = 1 ] && ok "a collector venv installs once" || bad "pip calls: $(pips)"
has "one pip takes the requirements and collectorkit" \
    "pip install -q -r collectors/host/requirements.txt -e shared/collectorkit" "$(cat "$TMP/pip.log")"
real build-host >/dev/null
[ "$(pips)" = 1 ] && ok "a warm build starts no pip" || bad "pip calls: $(pips)"
printf 'requests\nidna\n' > "$TREE/collectors/host/requirements.txt"
real build-host >/dev/null
[ "$(pips)" = 2 ] && ok "an edit to the requirements reinstalls" || bad "pip calls: $(pips)"
rm -f "$TREE/collectors/host/.venv/.stamp" "$TREE/collectors/host/.venv/bin/pip"

# A failed pip stops update-venvs at once, with a failure status.
printf '#!/bin/sh\nexit 1\n' > "$TREE/collectors/host/.venv/bin/python"
out="$(real update-venvs)"; rc=$?
[ "$rc" != 0 ] && ok "a failed pip fails update-venvs" || bad "update-venvs exit 0 over a failed pip"
lacks "a failed pip stops before the next venv" "update collectors/hyb/.venv" "$out"
printf '#!/bin/sh\n' > "$TREE/collectors/host/.venv/bin/python"
out="$(real update-venvs)"; rc=$?
[ "$rc" = 0 ] && ok "update-venvs succeeds over working venvs" || bad "update-venvs exit $rc: $out"
has "update-venvs updates every collector venv" "update collectors/hyb/.venv" "$out"

# make test for real, on stub suites every make reads (MAKEFILES), one
# that fails and one that passes.
echo "make test:"
printf 'test-sa: ; @echo "sa ran"\ntest-sb: ; @echo "sb ran"; exit 3\n' > "$TMP/suites.mk"
suites() { MAKEFILES="$TMP/suites.mk" real test SUITES="$1"; }
out="$(suites "sb sa")"; rc=$?
[ "$rc" != 0 ] && ok "a failed suite fails make test" || bad "make test exit 0: $out"
has_item "each suite's start prints" "start sa" "$out"
has_item "a failed suite is named with its status" "==> sb failed (2)" "$out"
order="$(grep -E '^(==> s[ab] [0-9]+s|s[ab] ran)$' <<<"$out" | sed -E 's/ [0-9]+s$/ Ns/' | tr '\n' ' ')"
[ "$order" = "==> sb Ns sb ran ==> sa Ns sa ran " ] && \
    ok "each log follows its name and seconds, in the order given" || bad "order: $order"
has_item "a subset says how many of the suites it runs" \
    "make test: running 2 of $(real list-suites | wc -l | tr -d ' ') suites (SUITES)" "$out"
out="$(suites sa)"; rc=$?
[ "$rc" = 0 ] && ok "passing suites pass make test" || bad "make test exit $rc: $out"
out="$(suites "")"; rc=$?
[ "$rc" != 0 ] && ok "an empty SUITES fails make test" || bad "make test exit 0: $out"
has "...and says why" "SUITES names no suite" "$out"
lacks "...before any suite starts" "start " "$out"
# With no SUITES, a stub fan-out prints the suites it was handed.
cp "$TREE/shared/wrappers/fanout.sh" "$TMP/fanout.sh"
printf '#!/bin/sh\nshift 5\necho "fanout $*"\n' > "$TREE/shared/wrappers/fanout.sh"
out="$(real test)"
has_item "make test runs every suite by default, in start order" \
    "fanout $(echo $(real list-suites))" "$out"
lacks "...and names no subset" "make test: running" "$out"
cp "$TMP/fanout.sh" "$TREE/shared/wrappers/fanout.sh"

echo "make site rules:"
out="$(cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" \
    WEALTHDB_DEMO_ROOT="$TMP/demo" make -n site-shots 2>&1)"
has "site-shots mounts the checkout read-only" "-v \"$TREE:/repo:ro\"" "$out"
lacks "site-shots mounts no secrets" ":/secrets" "$out"
has "site-shots runs as the caller" "--user \"\$(id -u):\$(id -g)\" -e HOME=/tmp" "$out"
has "site-shots sources the shared browser bootstrap" "source /opt/entrypoint-lib.sh" "$out"
has "site-shots bakes the Camoufox browser in" "mode=bundle;" "$out"
has "clean runs clean-site" "==> clean site" "$(dry clean)"
lacks "an exported SHOW prints no value" "--show"     "$(cd "$TREE" && env SHOW=1 WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" make -n pii 2>&1)"
has "SHOW on the command line prints them" "--show"     "$(cd "$TREE" && env WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" make -n pii SHOW=1 2>&1)"
has "cleanall removes the site venv" "rm -rf site/.venv" "$(dry cleanall)"

# site-publish for real, with a stub interpreter that prints the token it
# sees in place of site/images.py.
unset WEALTHDB_SITE_TOKEN
printf '#!/bin/sh\necho "token=${WEALTHDB_SITE_TOKEN:-}"\n' > "$TMP/bin/stubpy"
chmod +x "$TMP/bin/stubpy"
publish() { real site-publish PYTHON="$TMP/bin/stubpy" SITE_ENV="$TMP/site.env"; }
printf "WEALTHDB_SITE_TOKEN='fake-file-token'\n" > "$TMP/site.env"
has "site-publish exports the env file's token" "token=fake-file-token" "$(publish)"
has "the env file wins over the environment" "token=fake-file-token" \
    "$(WEALTHDB_SITE_TOKEN=fake-env-token publish)"
printf "WEALTHDB_SITE_TOKEN='fake-bad-token' (\n" > "$TMP/site.env"
out="$(publish)"; rc=$?
[ "$rc" != 0 ] && ok "site-publish refuses an env file that does not parse" \
    || bad "site-publish exit 0 over a bad env file: $out"
has "the refusal names the file" "$TMP/site.env has a bash syntax error" "$out"
lacks "the refusal prints none of the file's lines" "fake-bad-token" "$out"
lacks "the publish does not run" "token=" "$out"
rm -f "$TMP/site.env"
has "without an env file the environment's token is used" "token=fake-env-token" \
    "$(WEALTHDB_SITE_TOKEN=fake-env-token publish)"

summary "make rule tests"
