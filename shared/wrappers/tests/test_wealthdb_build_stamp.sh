#!/usr/bin/env bash
# Behaviour test for the version stamp `wealthdb build` passes to the
# image build: the release tag only for a clean engine tree on a release
# tag, the nearest release tag, and HEAD's hash with -dirty for an engine
# tree with changes. Runs a copy of the wrapper inside a scratch git repo
# with a stubbed docker on PATH, so nothing real is built.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
WRAPPER="${1:-$HERE/../../../wealthdb/wealthdb}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A docker stub that records its arguments, one per line.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$DOCKER_ARGS"
STUB
chmod +x "$TMP/bin/docker"
export DOCKER_ARGS="$TMP/args"
export PATH="$TMP/bin:$PATH"

# A hermetic git: no user or system config, a fixed identity, and none of
# the variables a calling hook may have set.
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid

REPO="$TMP/repo"
mkdir -p "$REPO/wealthdb"
cp "$WRAPPER" "$REPO/wealthdb/wealthdb"
echo "package main" > "$REPO/wealthdb/main.go"
echo "readme" > "$REPO/README.md"
git -C "$REPO" init -q
git -C "$REPO" add -A
git -C "$REPO" commit -qm one

# stamp prints the three build args the wrapper passed, as TAG|BASE|COMMIT.
stamp() {
	: > "$DOCKER_ARGS"
	"$1/wealthdb/wealthdb" build >/dev/null 2>&1
	local t b c
	t="$(sed -n 's/^WEALTHDB_TAG=//p' "$DOCKER_ARGS")"
	b="$(sed -n 's/^WEALTHDB_BASE=//p' "$DOCKER_ARGS")"
	c="$(sed -n 's/^WEALTHDB_COMMIT=//p' "$DOCKER_ARGS")"
	echo "$t|$b|$c"
}
expect() {
	local got
	got="$(stamp "$REPO")"
	if [ "$got" = "$2" ]; then ok "$1"; else bad "$1: got '$got', want '$2'"; fi
}
sha() { git -C "$REPO" rev-parse --short=7 HEAD; }

echo "wealthdb build version stamp:"
expect "no release tag: nightly of HEAD" "||$(sha)"

git -C "$REPO" tag v9.9.0
expect "clean tree on a release tag: the tag" "v9.9.0|v9.9.0|$(sha)"

echo "draft" > "$REPO/wealthdb/new.go"
expect "untracked engine file: dirty, no tag" "|v9.9.0|$(sha)-dirty"
rm "$REPO/wealthdb/new.go"

echo "edit" >> "$REPO/wealthdb/main.go"
expect "modified engine file: dirty, no tag" "|v9.9.0|$(sha)-dirty"
git -C "$REPO" checkout -q -- wealthdb/main.go

echo "edit" >> "$REPO/README.md"
expect "change outside the engine tree: still the tag" "v9.9.0|v9.9.0|$(sha)"
git -C "$REPO" checkout -q -- README.md

git -C "$REPO" commit -q --allow-empty -m two
git -C "$REPO" tag latest
expect "a commit past the release (non-release tag ignored)" "|v9.9.0|$(sha)"

PLAIN="$TMP/plain"
mkdir -p "$PLAIN/wealthdb"
cp "$WRAPPER" "$PLAIN/wealthdb/wealthdb"
got="$(GIT_CEILING_DIRECTORIES="$TMP" stamp "$PLAIN")"
if [ "$got" = "||" ]; then ok "outside a git checkout: no stamp"; else bad "outside a git checkout: got '$got', want '||'"; fi

echo
echo "wealthdb build stamp tests: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
