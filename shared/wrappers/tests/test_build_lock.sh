#!/usr/bin/env bash
# Behaviour test: wealthdb_build_lock (image-tag.sh) lets one build of an
# image run at a time. A holder killed with SIGKILL frees the lock once
# the commands it started are gone too; a live holder makes the next
# build wait and say which lock it waits on and who holds it; the lock
# file names its holder; `make clean` removes the locks no build holds
# and keeps a held one.
set -uo pipefail
# The makes this test starts read only what it gives them: an outer
# make's flags and command-line variables (make test-wrappers X=…) stay
# out.
unset MAKEFLAGS MFLAGS MAKELEVEL

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'pkill -P $$ 2>/dev/null; rm -rf "$TMP"' EXIT
LOCK="$TMP/locks/engine-x.lock"

# hold: take the lock in a shell of its own and keep it until
# $TMP/release appears.
hold() {
    rm -f "$TMP/held" "$TMP/release"
    sh -c '. "$1/shared/wrappers/image-tag.sh"; wealthdb_build_lock "$2" || exit 1
           echo held > "$3"; while [ ! -e "$4" ]; do sleep 0.1; done' \
        _ "$ROOT" "$LOCK" "$TMP/held" "$TMP/release" &
    HOLDER=$!
    for _ in $(seq 100); do [ -f "$TMP/held" ] && break; sleep 0.1; done
}

release() {
    touch "$TMP/release"
    wait "$HOLDER" 2>/dev/null
}

# take TIMEOUT: take the lock and give it back; status 124 when it was
# still waiting after TIMEOUT seconds. Its stderr is in $TMP/err.
take() {
    sh -c '. "$1/shared/wrappers/image-tag.sh"; wealthdb_build_lock "$2"' _ "$ROOT" "$LOCK" 2>"$TMP/err" &
    local pid=$! waited=0
    while kill -0 "$pid" 2>/dev/null; do
        if [ "$waited" -ge $(($1 * 10)) ]; then
            pkill -P "$pid" 2>/dev/null
            kill "$pid" 2>/dev/null
            wait "$pid" 2>/dev/null
            return 124
        fi
        sleep 0.1
        waited=$((waited + 1))
    done
    wait "$pid"
}

echo "== a free lock is taken at once and names its holder =="
take 5 && ok "taken" || bad "a free lock was not taken"
[ -s "$LOCK" ] && ok "the lock file names a pid" || bad "the lock file is empty"
[ ! -s "$TMP/err" ] && ok "no wait was reported" || bad "a free lock reported a wait: $(cat "$TMP/err")"

echo "== a live holder makes the next build wait, and the wait says so =="
hold
[ "$(tr -d '[:space:]' < "$LOCK")" = "$HOLDER" ] && ok "the lock file names the holder's pid" \
    || bad "the lock file names $(cat "$LOCK"), the holder is $HOLDER"
take 4
[ $? -eq 124 ] && ok "the next build waits while the holder lives" || bad "the next build did not wait"
grep -q "build: waiting for $LOCK, held by pid $HOLDER" "$TMP/err" \
    && ok "a wait past three seconds prints the lock path and the holder" \
    || bad "no wait message: $(cat "$TMP/err")"
release
take 5 && ok "the lock is free once the holder exits" || bad "the lock stayed held after its holder exited"

echo "== make clean keeps a held lock =="
hold
(cd "$ROOT" && make --no-print-directory BUILD_LOCK_DIR="$TMP/locks" clean-build-locks >/dev/null 2>&1)
[ -e "$LOCK" ] && ok "the held lock file stays" || bad "make clean removed a held lock"
take 1
[ $? -eq 124 ] && ok "and the next build still waits for its holder" \
    || bad "a second build took the lock during a clean"
release

echo "== a killed holder frees the lock =="
# SIGKILL runs no trap and no handler: what an interrupted build's
# cleanup could not do, the kernel does.
hold
pkill -KILL -P "$HOLDER" 2>/dev/null
kill -KILL "$HOLDER" 2>/dev/null
wait "$HOLDER" 2>/dev/null
take 5 && ok "after SIGKILL the lock is taken at once" || bad "SIGKILL left the lock held"

echo "== make clean removes the locks =="
mkdir -p "$TMP/cache/wealthdb/build-locks"
touch "$TMP/cache/wealthdb/build-locks/wealthdb-x.lock"
(cd "$ROOT" && XDG_CACHE_HOME="$TMP/cache" make --no-print-directory clean-build-locks >/dev/null)
[ ! -e "$TMP/cache/wealthdb/build-locks" ] && ok "the lock directory is gone" \
    || bad "make clean-build-locks left $(ls "$TMP/cache/wealthdb/build-locks")"
(cd "$ROOT" && make -n clean 2>/dev/null) | grep -q "build-locks" \
    && ok "make clean includes it" || bad "make clean does not remove the locks"

echo
summary "build lock tests"
