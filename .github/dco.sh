#!/bin/sh
# dco.sh BASE HEAD — the sign-off check of a pull request
# (CONTRIBUTING.md). Every commit in BASE..HEAD that is not a merge
# carries a Signed-off-by trailer with its author's e-mail: with it the
# author certifies the Developer Certificate of Origin for that commit.
# The e-mail is compared without regard to case; the name may differ.
#
# A merge is not checked. CI tests GitHub's merge of the change into its
# base, and a merge of the base into the branch brings in commits the
# base already holds. Commits BASE reaches are never checked.
#
# Exits 0 when every commit is signed off, 1 when one is not (it names
# each, and the fix), 2 on a usage error. Runs in a git checkout that
# holds both revisions. POSIX sh and git.
set -eu

[ "$#" -eq 2 ] || { echo "usage: dco.sh BASE HEAD" >&2; exit 2; }
for rev in "$1" "$2"; do
    git rev-parse -q --verify "$rev^{commit}" > /dev/null ||
        { echo "dco: $rev is no commit here" >&2; exit 2; }
done

total=0
missing=0
for c in $(git rev-list --no-merges --reverse "$1..$2"); do
    total=$((total + 1))
    email="$(git log -1 --format=%ae "$c" | tr '[:upper:]' '[:lower:]')"
    if [ -n "$email" ] &&
        git log -1 --format='%(trailers:key=Signed-off-by,valueonly,unfold)' "$c" |
        sed -n 's/.*<\([^>]*\)>[[:space:]]*$/\1/p' | tr '[:upper:]' '[:lower:]' |
        grep -qxF -- "$email"; then
        continue
    fi
    if [ "$missing" -eq 0 ]; then
        echo "dco: these commits have no Signed-off-by line with their author's e-mail:" >&2
    fi
    missing=$((missing + 1))
    echo "  $(git log -1 --format='%h %s' "$c")" >&2
done

if [ "$missing" -gt 0 ]; then
    cat >&2 <<EOF
dco: $missing of $total commits are not signed off.
Each commit certifies the Developer Certificate of Origin with a line
"Signed-off-by: Name <e-mail>" that carries its author's e-mail
(CONTRIBUTING.md). To add the line to every commit of the branch, rebase
it on wealthdb's main, then push it again:
  git fetch https://github.com/smartpointer-com/wealthdb.git main
  git rebase --signoff FETCH_HEAD
  git push --force-with-lease
EOF
    exit 1
fi
if [ "$total" -eq 0 ]; then
    echo "dco: no commit to check"
else
    echo "dco: all $total commits are signed off"
fi
