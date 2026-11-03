#!/bin/sh
# check_at.sh REV ARGS... — run pii_check.py ARGS with the rules, the
# allow list and the known findings as commit REV holds them.
#
# The hooks pass HEAD and CI the base of the change, so neither the
# rules nor allow.txt nor known.txt that a change edits ever judge that
# change: a value added to allow.txt still fails the commit that adds
# it, and a person lets that commit through on purpose (--no-verify, or
# a merge over the red check). Only while REV holds no checker at all,
# in the commit that adds it, does the working tree's run. A file REV
# lacks counts as empty.
#
# POSIX sh; the checker needs python3 (3.9 or newer) and git.
set -eu

[ "$#" -ge 2 ] || { echo "usage: check_at.sh REV ARGS..." >&2; exit 2; }
rev="$1"
shift
here="$(cd "$(dirname "$0")" && pwd)"
dir="$(mktemp -d "${TMPDIR:-/tmp}/pii-check.XXXXXX")"
trap 'rm -rf "$dir"' EXIT
trap 'exit 1' INT TERM

if git cat-file -e "$rev:shared/pii/pii_check.py" 2>/dev/null; then
	git show "$rev:shared/pii/pii_check.py" > "$dir/pii_check.py"
	for f in allow.txt known.txt; do
		if git cat-file -e "$rev:shared/pii/$f" 2>/dev/null; then
			git show "$rev:shared/pii/$f" > "$dir/$f"
		fi
	done
else
	for f in pii_check.py allow.txt known.txt; do
		if [ -f "$here/$f" ]; then
			cp "$here/$f" "$dir/"
		fi
	done
fi
python3 "$dir/pii_check.py" "$@"
