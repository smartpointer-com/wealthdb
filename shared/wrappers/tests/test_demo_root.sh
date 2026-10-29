#!/usr/bin/env bash
# Behaviour test: the demo root a checkout builds into. WEALTHDB_DEMO_ROOT
# wins; unset, the checkout's .demo-root names it, else ~/wealthdb-demo;
# a variable or a .demo-root that names no directory stops make. The
# demo's containers take their name from the root. The Makefile runs
# from a copy of the tree, so the checkout's own .demo-root plays no
# part.
set -uo pipefail
# The makes this test starts read only what it gives them: an outer
# make's flags and command-line variables (make test-wrappers X=…) stay
# out.
unset MAKEFLAGS MFLAGS MAKELEVEL

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
is()  { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$3', want '$2')"; fi; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TREE="$TMP/tree"
mkdir -p "$TREE/shared/wrappers"
cp "$ROOT/Makefile" "$TREE/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$ROOT/shared/wrappers/demo-root.sh" "$TREE/shared/wrappers/"

# make_var VAR [env...]: the value the Makefile gives VAR, from a dry run
# of help: DEMO_ROOT is the demo root, DEMO_NAME the name its containers
# take.
make_var() {
    local var="$1"
    shift
    (cd "$TMP" && env "$@" HOME=/home/u make -f "$TREE/Makefile" -np help 2>/dev/null) | sed -n "s/^$var := //p"
}

echo "demo root:"
is "the default is ~/wealthdb-demo" "/home/u/wealthdb-demo" "$(make_var DEMO_ROOT -u WEALTHDB_DEMO_ROOT)"
printf '  ~/wealthdb-other-demo  \n' > "$TREE/.demo-root"
is ".demo-root names it, ~ expanded" "/home/u/wealthdb-other-demo" "$(make_var DEMO_ROOT -u WEALTHDB_DEMO_ROOT)"
is "the variable wins" "/srv/demo" "$(make_var DEMO_ROOT WEALTHDB_DEMO_ROOT=/srv/demo)"
is "an empty variable is unset" "/home/u/wealthdb-other-demo" "$(make_var DEMO_ROOT WEALTHDB_DEMO_ROOT=)"
if (cd "$TMP" && env WEALTHDB_DEMO_ROOT='  ' make -f "$TREE/Makefile" -n help >/dev/null 2>&1); then
    bad "a blank variable stops make"
else
    ok "a blank variable stops make"
fi

echo "container name:"
rm -f "$TREE/.demo-root"
is "the default root gives demo" "demo" "$(make_var DEMO_NAME -u WEALTHDB_DEMO_ROOT)"
is "a leading wealthdb- goes" "other-demo" "$(make_var DEMO_NAME WEALTHDB_DEMO_ROOT=~/wealthdb-other-demo)"
is "any other name stays" "demo" "$(make_var DEMO_NAME WEALTHDB_DEMO_ROOT=/srv/demo)"
is "what a container name cannot hold reads as -" "my-demo.2" "$(make_var DEMO_NAME WEALTHDB_DEMO_ROOT='/srv/my+demo.2')"
is "the bare prefix gives demo" "demo" "$(make_var DEMO_NAME WEALTHDB_DEMO_ROOT=/srv/wealthdb-)"
is "a trailing slash is dropped" "x" "$(make_var DEMO_NAME WEALTHDB_DEMO_ROOT=/srv/x/)"

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

summary "demo root tests"
