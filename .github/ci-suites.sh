#!/bin/sh
# ci-suites.sh — which CI job runs which suite of `make test`
# (.github/workflows/ci.yml). The go jobs run wealthdb, the demo job
# demo, the collectors job every collector, and the rest job every
# other suite the Makefile lists, so a new suite reaches CI unedited.
# Prints the collectors job's and the rest job's suites as
# collectors=… and rest=… lines, the form $GITHUB_OUTPUT reads. Fails
# when a suite a job claims is not in the list, or two jobs claim one.
# Runs from the repo root.
set -eu

# One line each, the words a space apart.
all=$(echo $(make -s list-suites))
collectors=$(echo $(make -s list-collectors))
claimed="wealthdb demo $collectors"

fail=0
for s in $claimed; do
    case " $all " in
        *" $s "*) ;;
        *) echo "ci-suites: a job claims $s, which make test does not run" >&2; fail=1 ;;
    esac
done
dups=$(printf '%s\n' $claimed | sort | uniq -d)
if [ -n "$dups" ]; then
    echo "ci-suites: claimed by two jobs: $(echo $dups)" >&2
    fail=1
fi
rest=
for s in $all; do
    case " $claimed " in
        *" $s "*) ;;
        *) rest="$rest $s" ;;
    esac
done
if [ -z "$rest" ]; then
    echo "ci-suites: the rest job has no suite" >&2
    fail=1
fi
[ "$fail" = 0 ] || exit 1
echo "collectors=$collectors"
echo "rest=${rest# }"
