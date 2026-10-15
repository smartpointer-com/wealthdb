#!/usr/bin/env bash
# Behaviour test: the demo root a checkout builds into. WEALTHDB_DEMO_ROOT
# wins; unset, the checkout's .demo-root names it, else ~/wealthdb-demo;
# a variable or a .demo-root that names no directory stops make. The
# demo's containers take their name from the root. The Makefile runs
# from a copy of the tree, so the checkout's own .demo-root plays no
# part.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }
is()  { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$3', want '$2')"; fi; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TREE="$TMP/tree"
mkdir -p "$TREE/shared/wrappers"
cp "$ROOT/Makefile" "$TREE/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$ROOT/shared/wrappers/demo-root.sh" "$TREE/shared/wrappers/"

# make_root: the demo root the Makefile resolves, from a dry run of help.
make_root() {
    (cd "$TMP" && env "$@" HOME=/home/u make -f "$TREE/Makefile" -np help 2>/dev/null) | sed -n 's/^DEMO_ROOT := //p'
}

# make_name: the name the demo's containers take, from the same dry run.
make_name() {
    (cd "$TMP" && env "$@" HOME=/home/u make -f "$TREE/Makefile" -np help 2>/dev/null) | sed -n 's/^DEMO_NAME := //p'
}

echo "demo root:"
is "the default is ~/wealthdb-demo" "/home/u/wealthdb-demo" "$(make_root -u WEALTHDB_DEMO_ROOT)"
printf '  ~/wealthdb-other-demo  \n' > "$TREE/.demo-root"
is ".demo-root names it, ~ expanded" "/home/u/wealthdb-other-demo" "$(make_root -u WEALTHDB_DEMO_ROOT)"
is "the variable wins" "/srv/demo" "$(make_root WEALTHDB_DEMO_ROOT=/srv/demo)"
is "an empty variable is unset" "/home/u/wealthdb-other-demo" "$(make_root WEALTHDB_DEMO_ROOT=)"
if (cd "$TMP" && env WEALTHDB_DEMO_ROOT='  ' make -f "$TREE/Makefile" -n help >/dev/null 2>&1); then
    bad "a blank variable stops make"
else
    ok "a blank variable stops make"
fi

echo "container name:"
rm -f "$TREE/.demo-root"
is "the default root gives demo" "demo" "$(make_name -u WEALTHDB_DEMO_ROOT)"
is "a leading wealthdb- goes" "other-demo" "$(make_name WEALTHDB_DEMO_ROOT=~/wealthdb-other-demo)"
is "any other name stays" "demo" "$(make_name WEALTHDB_DEMO_ROOT=/srv/demo)"
is "what a container name cannot hold reads as -" "my-demo.2" "$(make_name WEALTHDB_DEMO_ROOT='/srv/my+demo.2')"
is "the bare prefix gives demo" "demo" "$(make_name WEALTHDB_DEMO_ROOT=/srv/wealthdb-)"
is "a trailing slash is dropped" "x" "$(make_name WEALTHDB_DEMO_ROOT=/srv/x/)"

for bad_root in 'two words' ''; do
    printf '%s\n' "$bad_root" > "$TREE/.demo-root"
    if (cd "$TMP" && env -u WEALTHDB_DEMO_ROOT make -f "$TREE/Makefile" -n help >/dev/null 2>&1); then
        bad "a .demo-root of '$bad_root' stops make"
    else
        ok "a .demo-root of '$bad_root' stops make"
    fi
done
rm -f "$TREE/.demo-root"
mkdir "$TREE/.demo-root"
if (cd "$TMP" && env -u WEALTHDB_DEMO_ROOT make -f "$TREE/Makefile" -n help >/dev/null 2>&1); then
    bad "a .demo-root directory stops make"
else
    ok "a .demo-root directory stops make"
fi

echo "  $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
