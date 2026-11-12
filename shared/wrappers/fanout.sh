#!/bin/sh
# fanout.sh [-j N] [--times] -- CMD ITEM... — run the shell command CMD
# once for each ITEM, with the item as $1, N runs at a time (default:
# one per CPU). Each run writes a log of its own. Once all have
# finished, the logs print in ITEM order, and a failed run's log is
# followed by "==> NAME failed", NAME being the item's last path part.
# The exit status is 1 when a run failed, else 0. With --times, each
# run prints "start ITEM" as it starts, each log follows a
# "==> NAME Ns" line with the run's seconds, and the failure line ends
# in the run's exit status. An item holds no white space.
set -u

usage() {
    echo "usage: fanout.sh [-j N] [--times] -- CMD ITEM..." >&2
    exit 2
}

jobs=
times=0
while [ $# -gt 0 ]; do
    case "$1" in
        -j) [ $# -ge 2 ] || usage; jobs="$2"; shift 2 ;;
        --times) times=1; shift ;;
        --) shift; break ;;
        *) usage ;;
    esac
done
[ $# -ge 1 ] || usage
cmd="$1"
shift
[ -n "$jobs" ] || jobs=$(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)

dir=$(mktemp -d "${TMPDIR:-/tmp}/fanout.XXXXXX") || exit 1
trap 'rm -rf "$dir"' EXIT

# Each run's files are named by its place in the list, so two items
# with one last path part keep logs of their own.
i=0
for item; do
    i=$((i + 1))
    printf '%s %s\n' "$i" "$item"
done > "$dir/items"

# xargs puts the two words of an items line after the fixed arguments.
< "$dir/items" xargs -n 2 -P "$jobs" sh -c '
    dir="$1"; times="$2"; cmd="$3"; i="$4"; item="$5"
    [ "$times" = 1 ] && echo "start $item"
    start=$(date +%s)
    sh -c "$cmd" sh "$item" > "$dir/$i.log" 2>&1
    echo $? > "$dir/$i.rc"
    echo $(( $(date +%s) - start )) > "$dir/$i.time"' _ "$dir" "$times" "$cmd"

fail=0
while read -r i item; do
    name=${item##*/}
    if [ "$times" = 1 ]; then
        secs=$(cat "$dir/$i.time" 2>/dev/null) && secs="${secs}s" || secs="(no time)"
        echo "==> $name $secs"
    fi
    cat "$dir/$i.log" 2>/dev/null
    rc=$(cat "$dir/$i.rc" 2>/dev/null || echo 1)
    if [ "$rc" != 0 ]; then
        if [ "$times" = 1 ]; then
            echo "==> $name failed ($rc)"
        else
            echo "==> $name failed"
        fi
        fail=1
    fi
done < "$dir/items"
exit $fail
