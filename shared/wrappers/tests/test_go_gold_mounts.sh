#!/usr/bin/env bash
# Behaviour test: wealthdb-go forwards the reload-verification harness's
# gold paths (WEALTHDB_GOLD_VERIFY, WEALTHDB_GOLD_BEFORE) as absolute
# paths and mounts each file's directory read-only at the same path, once
# per directory, and adds nothing when they are unset. A stubbed docker on
# PATH records the argv; nothing real runs.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

TREE="$TMP/tree"
mkdir -p "$TREE/wealthdb" "$TREE/shared/wrappers" "$TMP/bin" "$TMP/gold/a" "$TMP/gold/b"
cp "$ROOT/wealthdb/wealthdb-go" "$TREE/wealthdb/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$TREE/shared/wrappers/"
cat > "$TMP/bin/docker" <<'EOF'
#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done
env | grep '^WEALTHDB_GOLD_' | sed 's/^/ENV /'
EOF
chmod +x "$TMP/bin/docker"
: > "$TMP/gold/a/after.db"
: > "$TMP/gold/a/before.db"
: > "$TMP/gold/b/before.db"

run() { (cd "$TMP/gold" && env PATH="$TMP/bin:$PATH" XDG_CACHE_HOME="$TMP/cache" WEALTHDB_IMAGE_TAG=t "$@" \
    "$TREE/wealthdb/wealthdb-go" test ./internal/gold -run TestTaxonomyInvariants); }
count() { printf '%s\n' "$2" | grep -c -x -F -- "$1"; }

out="$(run)"
lacks "unset: no harness variable" "WEALTHDB_GOLD" "$out"
lacks "unset: no read-only mount" ":ro" "$out"
has "unset: the go argv follows the image" "$(printf 'wealthdb:t\ntest')" "$out"

out="$(run WEALTHDB_GOLD_VERIFY=a/after.db WEALTHDB_GOLD_BEFORE="$TMP/gold/b/before.db")"
has "a relative path is forwarded absolute" "ENV WEALTHDB_GOLD_VERIFY=$TMP/gold/a/after.db" "$out"
has "an absolute path is forwarded as given" "ENV WEALTHDB_GOLD_BEFORE=$TMP/gold/b/before.db" "$out"
has "each directory is mounted read-only" "$TMP/gold/a:$TMP/gold/a:ro" "$out"
has "the other directory too" "$TMP/gold/b:$TMP/gold/b:ro" "$out"
has "the image still precedes the go argv" "$(printf 'wealthdb:t\ntest')" "$out"

out="$(run WEALTHDB_GOLD_VERIFY=a/after.db WEALTHDB_GOLD_BEFORE=a/before.db)"
n="$(count "$TMP/gold/a:$TMP/gold/a:ro" "$out")"
[ "$n" = 1 ] && ok "one directory is mounted once" || bad "one directory mounted $n times"

out="$(run WEALTHDB_GOLD_VERIFY=missing/after.db 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "a path in no directory exits 2" || bad "a path in no directory exits $rc"

summary "wealthdb-go gold mount tests"
