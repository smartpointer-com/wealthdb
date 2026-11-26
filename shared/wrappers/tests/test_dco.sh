#!/usr/bin/env bash
# Behaviour test of .github/dco.sh, the sign-off check of a pull request,
# in a repository of its own:
#   - commits signed off with their author's e-mail pass, the case of
#     the e-mail and the name aside;
#   - a commit with no sign-off, one with another e-mail, or one whose
#     line is not a trailer fails, and the failure names each commit
#     and the fix;
#   - merges, and commits the base already holds, are not checked;
#   - a range with no commit passes; a revision that is not there is a
#     usage error;
# and of ci.yml: the check job waits for every other job, dco among them.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
SCRIPT="$ROOT/.github/dco.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# No user or system git config, and none of a calling hook's variables.
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
REPO="$TMP/repo"
git init -q -b main "$REPO"
cd "$REPO" || exit 1

# commit AUTHOR_EMAIL MESSAGE: an empty commit by Ann at that e-mail.
commit() {
    GIT_AUTHOR_NAME=Ann GIT_AUTHOR_EMAIL="$1" git commit -q --allow-empty -m "$2"
}
# dco BASE HEAD: the check's output and status.
dco() { sh "$SCRIPT" "$1" "$2" 2>&1; }

commit owner@example.invalid "the base, not signed off"
base="$(git rev-parse HEAD)"

echo "signed-off commits:"
commit ann@example.invalid "$(printf 'one\n\nSigned-off-by: Ann <ann@example.invalid>')"
commit ann@example.invalid "$(printf 'two\n\nA body.\n\nSigned-off-by: A. N. Other <ANN@Example.Invalid>')"
commit ann@example.invalid "$(printf 'three\n\nSigned-off-by: Bo <bo@example.invalid>\nSigned-off-by: Ann <ann@example.invalid>')"
out="$(dco "$base" HEAD)"; rc=$?
[ "$rc" = 0 ] && ok "commits signed off with their author's e-mail pass" || bad "exit $rc: $out"
has "...and says how many it checked" "dco: all 3 commits are signed off" "$out"
signed="$(git rev-parse HEAD)"

echo "commits that are not signed off:"
commit ann@example.invalid "four, no sign-off"
commit ann@example.invalid "$(printf 'five, another e-mail\n\nSigned-off-by: Bo <bo@example.invalid>')"
commit ann@example.invalid "$(printf 'six\n\nSigned-off-by: Ann <ann@example.invalid>\n\nA paragraph after it.')"
out="$(dco "$base" HEAD)"; rc=$?
[ "$rc" = 1 ] && ok "a commit with no sign-off fails the check" || bad "exit $rc: $out"
has "...and is named" "four, no sign-off" "$out"
has "a sign-off with another e-mail is named" "five, another e-mail" "$out"
has "a sign-off line that is not a trailer is named" "six" "$out"
lacks "a signed-off commit is not named" "  $(git rev-parse --short "$signed") three" "$out"
has "the count is of the commits checked" "dco: 3 of 6 commits are not signed off." "$out"
has "the fix is named" "git rebase --signoff FETCH_HEAD" "$out"

echo "what is not checked:"
git checkout -q -b side "$signed"
commit ann@example.invalid "$(printf 'on the side\n\nSigned-off-by: Ann <ann@example.invalid>')"
git checkout -q main
git checkout -q -b branch "$signed"
GIT_AUTHOR_NAME=Ann GIT_AUTHOR_EMAIL=ann@example.invalid git merge -q --no-ff -m "a merge, not signed off" side
out="$(dco "$base" HEAD)"; rc=$?
[ "$rc" = 0 ] && ok "a merge is not checked" || bad "exit $rc: $out"
has "...nor counted" "dco: all 4 commits are signed off" "$out"
git checkout -q main
git checkout -q -b later "$base"
commit owner@example.invalid "the base moves on, not signed off"
later="$(git rev-parse HEAD)"
git checkout -q branch
GIT_AUTHOR_NAME=Ann GIT_AUTHOR_EMAIL=ann@example.invalid git merge -q --no-ff -m "the base merged in" later
out="$(dco "$later" HEAD)"; rc=$?
[ "$rc" = 0 ] && ok "a commit the base holds is not checked" || bad "exit $rc: $out"
out="$(dco "$signed" "$signed")"; rc=$?
[ "$rc" = 0 ] && ok "a range with no commit passes" || bad "exit $rc: $out"
has "...and says so" "dco: no commit to check" "$out"
out="$(dco "$base" no-such-rev)"; rc=$?
[ "$rc" = 2 ] && ok "a revision that is not there is a usage error" || bad "exit $rc: $out"
has "...naming it" "no-such-rev is no commit here" "$out"
out="$(sh "$SCRIPT" "$base" 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "one argument is a usage error" || bad "exit $rc: $out"

echo "ci.yml:"
cd "$ROOT" || exit 1
jobs="$(awk '/^jobs:/ { on = 1; next } on && /^  [a-z][a-z0-9_-]*:$/ { sub(/:$/, ""); print $1 }' \
    .github/workflows/ci.yml | sort)"
needs="$(sed -n '/^  check:$/,/^  [a-z]/ s/^ *needs: *\[\(.*\)\]$/\1/p' .github/workflows/ci.yml |
    tr ',' '\n' | tr -d ' ' | sort)"
has_item "the workflow has the dco job" dco "$jobs"
[ -n "$needs" ] && [ "$needs" = "$(grep -vx check <<<"$jobs")" ] &&
    ok "the check job waits for every other job" ||
    bad "check needs: $(echo $needs); jobs: $(echo $jobs)"

summary "dco tests"
