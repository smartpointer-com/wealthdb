#!/usr/bin/env bash
# Behaviour test of .github/ci-suites.sh, under a stub make that lists
# the suites and collectors it is given:
#   - it prints the collectors job's and the rest job's suites, in the
#     list's order;
#   - it fails, naming the suite, when a claimed suite is not in the
#     list, when two jobs claim one suite, or when the rest job is left
#     with none;
#   - on the real Makefile, every suite reaches exactly one job.
set -uo pipefail
# The makes this test starts read only what it gives them.
unset MAKEFLAGS MFLAGS MAKELEVEL

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
SCRIPT="$ROOT/.github/ci-suites.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
cat > "$TMP/bin/make" <<'STUB'
#!/bin/sh
case "$2" in
    list-suites) printf '%s\n' $STUB_SUITES ;;
    list-collectors) printf '%s\n' $STUB_COLLECTORS ;;
    *) echo "stub make: unexpected $*" >&2; exit 2 ;;
esac
STUB
chmod +x "$TMP/bin/make"

# suites SUITES COLLECTORS: the script's output and status over the stub.
suites() {
    (cd "$TMP" && env PATH="$TMP/bin:$PATH" STUB_SUITES="$1" STUB_COLLECTORS="$2" sh "$SCRIPT" 2>&1)
}

echo "ci-suites:"
out="$(suites "wealthdb demo web pii ca cb" "ca cb")"; rc=$?
[ "$rc" = 0 ] && ok "a list every job can take passes" || bad "exit $rc: $out"
has_item "the collectors job takes the collectors" "collectors=ca cb" "$out"
has_item "the rest job takes every other suite, in order" "rest=web pii" "$out"

out="$(suites "wealthdb web ca" "ca")"; rc=$?
[ "$rc" != 0 ] && ok "a claimed suite the list lacks fails" || bad "exit 0: $out"
has "...and names it" "a job claims demo, which make test does not run" "$out"
lacks "...and prints no job's suites" "rest=" "$out"

out="$(suites "wealthdb demo web ca" "ca demo")"; rc=$?
[ "$rc" != 0 ] && ok "a suite two jobs claim fails" || bad "exit 0: $out"
has "...and names it" "claimed by two jobs: demo" "$out"

out="$(suites "wealthdb demo ca" "ca")"; rc=$?
[ "$rc" != 0 ] && ok "a list with nothing left for the rest job fails" || bad "exit 0: $out"
has "...and says so" "the rest job has no suite" "$out"

# The real list: the collectors and the rest add up to make test's suites
# less the go and demo jobs'.
out="$(cd "$ROOT" && sh "$SCRIPT" 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "the Makefile's suites pass" || bad "exit $rc: $out"
jobs="$(echo wealthdb demo $(sed -n 's/^collectors=//p; s/^rest=//p' <<<"$out") | tr ' ' '\n' | sort)"
want="$(cd "$ROOT" && make -s list-suites | sort)"
[ "$jobs" = "$want" ] && ok "...and each reaches one job" || bad "jobs: $(echo $jobs) want: $(echo $want)"

summary "ci-suites tests"
