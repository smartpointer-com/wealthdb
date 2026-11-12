#!/usr/bin/env bash
# Behaviour test of fanout.sh, on a tree of stub items:
#   - the runs overlap, and the logs print in item order;
#   - --times prints each start, a "==> NAME Ns" line before each log,
#     and the exit status on a failure line;
#   - without --times only the logs print, and a failure line;
#   - the exit status is 1 when a run failed, else 0;
#   - two items with one last path part keep logs of their own;
#   - a call without a command is a usage error.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
FANOUT="$HERE/../fanout.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/a" "$TMP/b"
# first waits up to 10 s for second to start, so it can end only when
# the two runs overlap; it then prints after second has printed.
cat > "$TMP/first" <<EOF
for _ in \$(seq 100); do [ -e "$TMP/second-ran" ] && break; sleep 0.1; done
[ -e "$TMP/second-ran" ] || { echo "first: second never ran beside it"; exit 4; }
echo "first ran"
EOF
printf 'touch "%s/second-ran"; echo "second ran"\n' "$TMP" > "$TMP/second"
printf 'echo "fails"; exit 3\n' > "$TMP/fails"
printf 'echo "a/same"\n' > "$TMP/a/same"
printf 'echo "b/same"\n' > "$TMP/b/same"

fan() { "$FANOUT" "$@" 2>&1; }

echo "fanout:"
out="$(fan -j 2 --times -- 'sh "$1"' "$TMP/first" "$TMP/second")"; rc=$?
[ "$rc" = 0 ] && ok "runs that pass exit 0" || bad "exit $rc: $out"
has "the runs overlap" "first ran" "$out"
logs="$(grep -v '^start ' <<<"$out" | sed -E 's/^(==> [a-z]+) [0-9]+s$/\1 Ns/')"
expect="==> first Ns
first ran
==> second Ns
second ran"
[ "$logs" = "$expect" ] && ok "--times prints each log after its name and seconds, in item order" \
    || bad "--times output: $out"
has_item "--times prints each start" "start $TMP/first" "$out"
has_item "--times prints the start of every run" "start $TMP/second" "$out"

out="$(fan --times -- 'sh "$1"' "$TMP/fails" "$TMP/a/same")"; rc=$?
[ "$rc" = 1 ] && ok "a failed run makes the exit status 1" || bad "exit $rc: $out"
has_item "--times names the failed run and its status" "==> fails failed (3)" "$out"
has_item "the runs after a failure still print" "a/same" "$out"

out="$(fan -- 'sh "$1"' "$TMP/fails" "$TMP/a/same" "$TMP/b/same")"; rc=$?
[ "$rc" = 1 ] && ok "a failed run makes the exit status 1 without --times" || bad "exit $rc: $out"
expect="fails
==> fails failed
a/same
b/same"
[ "$out" = "$expect" ] && ok "without --times only the logs and the failure line print" \
    || bad "output: $out"

out="$(fan -j 1 -- 'echo "item $1"' x y)"
[ "$out" = "item x
item y" ] && ok "CMD gets the item as \$1" || bad "output: $out"

out="$(fan -j 2)"; rc=$?
[ "$rc" = 2 ] && ok "no command is a usage error" || bad "exit $rc: $out"
has "the usage names the form" "usage: fanout.sh" "$out"

summary "fanout tests"
