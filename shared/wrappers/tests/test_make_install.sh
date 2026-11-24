#!/usr/bin/env bash
# Behaviour test of the Makefile's install, uninstall and update in a
# tree that config.mk gives a prefix:
#   - in a git checkout, install copies the files git tracks, config.mk,
#     .image-tag, a native tree's engine and the dashboards' two files:
#     no test, and nothing git does not track;
#   - in a tree with no .git, every file but the tests, a core file and
#     a log;
#   - a second install drops what the tree dropped, and a copy that
#     comes out short replaces nothing;
#   - uninstall removes the copy and both links, and in a native tree
#     first stops both servers, when the command in BINDIR is this
#     tree's;
#   - PREFIX and ENGINE in the environment set nothing;
#   - update refuses in a native tree before it touches a venv.
# The tree is a small one in the checkout's layout, under this
# checkout's Makefile. Its launcher, its Python and docker are stubs
# that record how they were called.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# A hook's git variables would point the tree's git at another repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE ENGINE PREFIX BINDIR WEALTHDB_IMAGE_TAG WEALTHDB_DEMO_ROOT
export HOME="$TMP/home"
sandbox_env "$TMP"
mkdir -p "$HOME" "$TMP/bin"
stub_docker "$TMP/bin"
export STUB_LOG="$TMP/log"
export PATH="$TMP/bin:$PATH"

# logger PATH: a program that appends its arguments, on one line, to
# $STUB_LOG, and fails.
logger() {
    mkdir -p "$(dirname "$1")"
    printf '#!/bin/sh\necho "$(basename "$0") $*" >> "$STUB_LOG"\nexit 1\n' > "$1"
    chmod +x "$1"
}

TREE="$TMP/tree"
P="$TMP/prefix"
COPY="$P/libexec/wealthdb"
mkdir -p "$TREE/shared/wrappers" "$TREE/shared/collectorkit" "$TREE/collectors/testcol/tests" \
    "$TREE/bin" "$TREE/mcp" "$TREE/web" "$TREE/demo" "$TREE/skills/s"
cp "$ROOT/Makefile" "$TREE/"
cp "$ROOT"/shared/wrappers/*.sh "$TREE/shared/wrappers/"
logger "$TREE/wealthdb/wealthdb"
for f in LICENSE README.md bin/wealthdb-collect mcp/mcp web/web shared/collectorkit/kit.py \
    collectors/testcol/testcol collectors/testcol/load.py collectors/testcol/tests/test_a.py \
    collectors/testcol/test_b.py demo/generate.py skills/s/SKILL.md; do
    echo "$f" > "$TREE/$f"
done
git -C "$TREE" init -q
git -C "$TREE" add .
# What git does not track: the build's files, and what a run leaves.
echo t > "$TREE/.image-tag"
mkdir -p "$TREE/web/lib" "$TREE/collectors/testcol/.venv/bin" "$TREE/shared/collectorkit/__pycache__"
for f in web/lib/metabase.jar web/lib/duckdb.metabase-driver.jar web/lib/notes.txt \
    collectors/testcol/core collectors/testcol/run.log collectors/testcol/session.json \
    collectors/testcol/.venv/bin/python shared/collectorkit/__pycache__/kit.pyc; do
    echo "$f" > "$TREE/$f"
done

# mk ARGS...: make in TREE, apart from the make that runs this test.
mk() {
    (cd "$TREE" && env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL make --no-print-directory "$@") > "$TMP/out" 2> "$TMP/err"
}
# record KIND: the config.mk ./configure writes for a prefix.
record() {
    printf 'ENGINE=%s\nPREFIX=%s\nBINDIR=%s/bin\nPYTHON=%s\n' "$1" "$P" "$P" "$TMP/bin/python-stub" > "$TREE/config.mk"
}
logger "$TMP/bin/python-stub"
holds() { if [ -e "$COPY/$2" ]; then ok "$1"; else bad "$1 (no $2 in the copy)"; fi; }
omits() { if [ -e "$COPY/$2" ]; then bad "$1 ($2 is in the copy)"; else ok "$1"; fi; }

echo "install, in a git checkout:"
record container
mk install || bad "make install failed: $(cat "$TMP/err")"
for f in LICENSE README.md bin/wealthdb-collect wealthdb/wealthdb mcp/mcp web/web \
    shared/wrappers/image-tag.sh shared/collectorkit/kit.py collectors/testcol/testcol \
    collectors/testcol/load.py demo/generate.py skills/s/SKILL.md; do
    holds "the copy holds the tracked $f" "$f"
done
holds "and the configure record" config.mk
holds "and the image tag" .image-tag
holds "and the dashboards' two files" web/lib/metabase.jar
holds "(both)" web/lib/duckdb.metabase-driver.jar
omits "no other file of web/lib" web/lib/notes.txt
omits "no core file" collectors/testcol/core
omits "no log" collectors/testcol/run.log
omits "no other untracked file" collectors/testcol/session.json
omits "no tests folder" collectors/testcol/tests
omits "no test file" collectors/testcol/test_b.py
omits "no venv" collectors/testcol/.venv
omits "no cache" shared/collectorkit/__pycache__
omits "no Makefile: the copy builds nothing" Makefile
[ "$P/bin/wealthdb" -ef "$COPY/wealthdb/wealthdb" ] && ok "the launcher is linked from the bin directory" ||
    bad "bin/wealthdb is not the copy's launcher"
[ "$P/bin/wealthdb-collect" -ef "$COPY/bin/wealthdb-collect" ] && ok "and the dispatcher" ||
    bad "bin/wealthdb-collect is not the copy's dispatcher"

echo "a second install:"
echo stale > "$COPY/stale"
git -C "$TREE" rm -q -f collectors/testcol/load.py
mk install || bad "the second install failed: $(cat "$TMP/err")"
omits "a file the tree dropped is gone from the copy" collectors/testcol/load.py
omits "and a file that was only in the copy" stale
holds "the rest is there" collectors/testcol/testcol
[ ! -e "$COPY.new" ] && [ ! -e "$COPY.old" ] && ok "nothing is left beside the copy" || bad "a folder is left beside the copy"

echo stays > "$COPY/stays"
git -C "$TREE" rm -q --cached LICENSE
if mk install; then bad "a copy that lacks a file is installed"; else ok "a copy that comes out short fails the install"; fi
has "it names the file" "the copy lacks LICENSE" "$(cat "$TMP/err")"
holds "and the copy that was there stands" stays
holds "whole" LICENSE
git -C "$TREE" add LICENSE

echo "uninstall:"
: > "$STUB_LOG"
mk uninstall || bad "make uninstall failed: $(cat "$TMP/err")"
[ ! -e "$COPY" ] && ok "the copy is removed" || bad "the copy is still there"
[ ! -e "$COPY.new" ] && ok "and what a failed install left beside it" || bad "$COPY.new is still there"
[ ! -L "$P/bin/wealthdb" ] && [ ! -L "$P/bin/wealthdb-collect" ] && ok "and both links" || bad "a link is still there"
lacks "a tree of the container engine stops no server" "stop" "$(cat "$STUB_LOG")"

echo "a native tree:"
record native
if mk install; then bad "a native tree with no engine is installed"; else ok "no engine built: the install fails"; fi
has "and names the build" "wealthdb/wealthdb-engine is missing; run make all first" "$(cat "$TMP/err")"
echo engine > "$TREE/wealthdb/wealthdb-engine"
mk install || bad "make install failed: $(cat "$TMP/err")"
holds "the copy holds the engine" wealthdb/wealthdb-engine
: > "$STUB_LOG"
mk uninstall || bad "make uninstall failed, though a stop that fails must not fail it: $(cat "$TMP/err")"
[ "$(cat "$STUB_LOG")" = "wealthdb mcp stop
wealthdb web stop" ] && ok "uninstall stops both servers first, by the installed command" || bad "uninstall ran: $(cat "$STUB_LOG")"
[ ! -e "$COPY" ] && [ ! -L "$P/bin/wealthdb" ] && ok "then removes the copy and the links" || bad "the copy or a link is still there"

mk install || bad "make install failed: $(cat "$TMP/err")"
logger "$TMP/other/wealthdb"
ln -sf "$TMP/other/wealthdb" "$P/bin/wealthdb"
: > "$STUB_LOG"
mk uninstall || bad "make uninstall failed: $(cat "$TMP/err")"
lacks "a command in the bin directory that is another tree's is not run" "stop" "$(cat "$STUB_LOG")"
[ ! -e "$COPY" ] && ok "and the copy is removed all the same" || bad "the copy is still there"

echo "update, in a native tree:"
logger "$TREE/shared/lint/.venv/bin/python"
: > "$STUB_LOG"
if mk update; then bad "make update runs in a native tree"; else ok "make update refuses"; fi
has "it says why" "this tree runs the native engine" "$(cat "$TMP/err")"
[ ! -s "$STUB_LOG" ] && ok "before any venv, image or module is touched" || bad "make update ran: $(cat "$STUB_LOG")"

echo "a tree with no .git:"
mv "$TREE/.git" "$TMP/git-aside"
record container
mk install || bad "make install failed: $(cat "$TMP/err")"
holds "the copy holds the tree's files" collectors/testcol/testcol
holds "also one git would not track" collectors/testcol/session.json
omits "but no core file" collectors/testcol/core
omits "no log" collectors/testcol/run.log
omits "no tests folder" collectors/testcol/tests
omits "no test file" collectors/testcol/test_b.py
omits "no venv" collectors/testcol/.venv
mk uninstall || bad "make uninstall failed: $(cat "$TMP/err")"

echo "the environment:"
rm -f "$TREE/config.mk"
PREFIX="$P" ENGINE=native mk -n install uninstall || bad "make -n install uninstall failed: $(cat "$TMP/err")"
out="$(cat "$TMP/out")"
has "PREFIX in the environment sets no prefix: install links the tree" 'tree/wealthdb/wealthdb" ' "$out"
lacks "and copies nothing" "libexec" "$out"
lacks "ENGINE in the environment sets no kind: uninstall stops no server" "stop" "$out"
ENGINE=podman mk -n install || bad "an ENGINE in the environment that is no kind stops make: $(cat "$TMP/err")"
if mk install PREFIX="$P" BINDIR="$P/bin"; then bad "a prefix on the command line installs with no config.mk"
else ok "a prefix on the command line is read, and needs a config.mk"; fi
has "it names ./configure" "config.mk is missing; run ./configure and make all first" "$(cat "$TMP/err")"
[ ! -e "$COPY" ] && ok "and nothing is copied" || bad "a copy was made"

summary "make install tests"
