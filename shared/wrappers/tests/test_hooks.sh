#!/usr/bin/env bash
# Behaviour test of the git hooks (.githooks), through real commits in a
# scratch repository whose privacy check and make are stubs:
#   - pre-commit runs the privacy check on the staged change with HEAD's
#     rules, then the lint on the staged tree;
#   - commit-msg runs it on the message file with HEAD's rules;
#   - a failed check refuses the commit and says which, and a refused
#     pre-commit check runs no lint;
#   - a lint that fails or rewrites a staged file refuses the commit.
set -uo pipefail
unset MAKEFLAGS MFLAGS MAKELEVEL

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

# The physical path, as git names the top of the work tree by it.
TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$TMP"' EXIT
REPO="$TMP/repo"
LOG="$TMP/calls.log"
mkdir -p "$REPO/shared/pii" "$REPO/shared/wrappers" "$TMP/bin"
cp -R "$ROOT/.githooks" "$REPO/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$REPO/shared/wrappers/"
# The privacy check logs its arguments and fails the call that
# STUB_PII_FAIL names (staged or message).
cat > "$REPO/shared/pii/check_at.sh" <<STUB
#!/bin/sh
echo "check \$*" >> "$LOG"
[ "\$2" != "\${STUB_PII_FAIL:-}" ]
STUB
# make logs its arguments; STUB_LINT=fail fails it, and STUB_LINT=rewrite
# edits a staged file in the tree it is given.
cat > "$TMP/bin/make" <<STUB
#!/bin/sh
echo "make \$*" >> "$LOG"
case "\${STUB_LINT:-}" in
    fail) exit 1 ;;
    rewrite) echo rewritten >> "\$3/a.txt" ;;
esac
STUB
chmod +x "$REPO/shared/pii/check_at.sh" "$TMP/bin/make"

git() {
    env PATH="$TMP/bin:$PATH" WEALTHDB_IMAGE_TAG=t XDG_CACHE_HOME="$TMP/cache" \
        GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com \
        GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com \
        GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 \
        git -C "$REPO" "$@"
}
git init -q
git config core.hooksPath .githooks
git add -A
git commit -q --no-verify -m "base"

# commit MSG [VAR=VALUE...]: stages an edit and commits it under the
# hooks; prints the hooks' output, and the calls they made after a
# "calls:" line.
n=0
commit() {
    local msg="$1"
    shift
    n=$((n + 1))
    echo "edit $n" >> "$REPO/a.txt"
    git add a.txt
    : > "$LOG"
    (for kv in "$@"; do export "${kv?}"; done; git commit -q -m "$msg") 2>&1
    local rc=$?
    echo "calls:"
    cat "$LOG"
    return "$rc"
}

echo "hooks:"
out="$(commit "a clean change")"; rc=$?
[ "$rc" = 0 ] && ok "a change both checks pass commits" || bad "exit $rc: $out"
has_item "pre-commit checks the staged change with HEAD's rules" "check HEAD staged" "$out"
has "commit-msg checks the message file with HEAD's rules" "check HEAD message " "$out"
has "...the message git wrote" ".git/COMMIT_EDITMSG" "$out"
has "the lint runs on a scratch tree of the index" "make -s -C $TMP/cache/wealthdb/pre-commit." "$out"
has "...with this checkout's ruff venv" "LINT_VENV=$REPO/shared/lint/.venv" "$out"
[ -z "$(ls -A "$TMP/cache/wealthdb")" ] && ok "the scratch tree is removed" \
    || bad "left: $(ls "$TMP/cache/wealthdb")"

out="$(commit "staged leak" STUB_PII_FAIL=staged)"; rc=$?
[ "$rc" != 0 ] && ok "a failed check of the staged change refuses the commit" || bad "exit 0: $out"
has "...and says so" "pre-commit: the privacy check failed on the staged change; commit refused" "$out"
lacks "...and runs no lint" "make " "$out"

out="$(commit "message leak" STUB_PII_FAIL=message)"; rc=$?
[ "$rc" != 0 ] && ok "a failed check of the message refuses the commit" || bad "exit 0: $out"
has "...and says so" "commit-msg: the privacy check failed on the message; commit refused" "$out"

out="$(commit "lint fails" STUB_LINT=fail)"; rc=$?
[ "$rc" != 0 ] && ok "a failed lint refuses the commit" || bad "exit 0: $out"
has "...and says so" "pre-commit: make lint failed on the staged tree; commit refused" "$out"

out="$(commit "lint rewrites" STUB_LINT=rewrite)"; rc=$?
[ "$rc" != 0 ] && ok "a lint that rewrites a staged file refuses the commit" || bad "exit 0: $out"
has "...and names the file" "a.txt: FAILED" "$out"

[ "$(git rev-list --count HEAD)" = 2 ] && ok "only the clean change was committed" \
    || bad "commits: $(git rev-list --count HEAD)"

summary "hook tests"
