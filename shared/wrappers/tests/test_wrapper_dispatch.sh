#!/usr/bin/env bash
# Behaviour test: the dispatch of wrapper_main and wrapper_wants_help.
# wrapper_main sends build to wrapper_build and prune to
# wrapper_host_prune, and every other verb to the docker-run path, whose
# first step is wrapper_resolve_dir_args. All three are stubbed to print
# their name and exit, so nothing real runs.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

# dispatch [args...]: which path wrapper_main takes for args, and the
# args that path gets.
dispatch() {
    (
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=()
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh"
        wrapper_build()            { echo "build: $*"; exit 0; }
        wrapper_host_prune()       { echo "prune: $*"; exit 0; }
        wrapper_resolve_dir_args() { echo "run: $*"; exit 0; }
        wrapper_main "$@"
    )
}

# wants_help [args...]: yes or no, as wrapper_wants_help answers.
wants_help() {
    (
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh"
        if wrapper_wants_help "$@"; then echo yes; else echo no; fi
    )
}

echo "wrapper_main:"
check "build goes to wrapper_build with every arg" "$(dispatch build --no-cache)" "build: build --no-cache"
check "prune goes to wrapper_host_prune with every arg" "$(dispatch prune --dry-run)" "prune: prune --dry-run"
check "download takes the docker-run path" "$(dispatch download --since 2024-01-01)" "run: download --since 2024-01-01"
check "login takes the docker-run path" "$(dispatch login --check)" "run: login --check"
check "build as a later arg is not the build verb" "$(dispatch --data-dir /x build)" "run: --data-dir /x build"
check "prune as a later arg is not the prune verb" "$(dispatch load prune)" "run: load prune"
check "no args take the docker-run path" "$(dispatch)" "run: "

echo "wrapper_wants_help:"
check "no args" "$(wants_help)" yes
check "help" "$(wants_help help)" yes
check "--help" "$(wants_help --help)" yes
check "-h" "$(wants_help -h)" yes
check "help with more args" "$(wants_help help build)" yes
check "a verb" "$(wants_help download)" no
check "a verb's own --help" "$(wants_help download --help)" no
check "an empty first arg" "$(wants_help '')" no

echo
summary "wrapper dispatch tests"
