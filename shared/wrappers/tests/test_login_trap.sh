#!/usr/bin/env bash
# Behaviour test for the `login` traps of the wrappers whose `login` folds
# into `download`: amex, chase, fidelity-web, raiffeisen_at and
# schwab-web. A bare `login` is a host-side no-op that never starts a
# container. amex, chase and raiffeisen_at let `login --check` and the
# help flags through to the container; fidelity-web and schwab-web have no
# session to probe, so their trap takes every `login`. A bare `login`
# with a credential missing prints the setup instructions and exits 78
# instead (setup-lib.sh), and with the setup complete prints the notice.
# Runs each wrapper
# with a stubbed docker on PATH, so nothing real is invoked. Pass wrapper
# paths to test those instead of the five.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
COLLECTORS="$HERE/../../../collectors"
if [[ $# -gt 0 ]]; then
    WRAPPERS=("$@")
else
    WRAPPERS=("$COLLECTORS/amex/amex" "$COLLECTORS/chase/chase"
              "$COLLECTORS/fidelity-web/fidelity-web"
              "$COLLECTORS/raiffeisen_at/raiffeisen_at"
              "$COLLECTORS/schwab-web/schwab-web")
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A docker stub that records the SUBCOMMAND, not just that docker was
# reached: the wrapper's safety guard runs `docker ps` before any verb, so
# "docker was called" is satisfied even by a verb that was trapped.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "$1" >> "$DOCKER_CALLS"
exit 0
STUB
chmod +x "$TMP/bin/docker"
export DOCKER_CALLS="$TMP/calls"
export PATH="$TMP/bin:$PATH"

# Everything the wrappers resolve is redirected into $TMP so the test
# writes nowhere else: wrapper_check_mounts mkdir -p's the data,
# startupCache and debug dirs, and errors out unless the secrets dir
# already exists. sandbox_env clears the fleet-wide roots, and each case
# clears the wrapper's own overrides, so no ambient one can steer a mount
# back out of the sandbox.
sandbox_env "$TMP"
export WEALTHDB_SECRETS_DIR="$TMP/secrets"
mkdir -p "$WEALTHDB_SECRETS_DIR"

# run WRAPPER ARGS...: run the wrapper with its own overrides cleared;
# true when it reached `docker run`.
run() {
    local wrapper="$1" prefix
    shift
    prefix="$(basename "$wrapper" | tr 'a-z-' 'A-Z_')"
    : > "$DOCKER_CALLS"
    env -u "${prefix}_SECRETS_DIR" -u "${prefix}_DATA_DIR" -u "${prefix}_DEBUG_DIR" \
        "$wrapper" "$@"
}
started() { grep -qx "run" "$DOCKER_CALLS"; }

for wrapper in "${WRAPPERS[@]}"; do
    name="$(basename "$wrapper")"
    echo "$name login trap:"

    rm -f "$WEALTHDB_SECRETS_DIR/$name.env"
    out="$(run "$wrapper" login 2>&1)"; rc=$?
    [[ $rc -eq 78 ]] && ok "bare login with no credential exits 78" ||
        bad "bare login with no credential exit=$rc"
    grep -q "Then run: wealthdb-collect $name login" <<<"$out" \
        && ok "bare login with no credential says what to do" ||
        bad "bare login with no credential: $out"
    started && bad "bare login with no credential started a container" ||
        ok "bare login with no credential starts no container"

    awk '$1 == "var" {print $2 "=x"}' "$(dirname "$wrapper")/setup.txt" \
        > "$WEALTHDB_SECRETS_DIR/$name.env"
    out="$(run "$wrapper" login 2>&1)"; rc=$?
    [[ $rc -eq 0 ]] && ok "bare login exits 0" || bad "bare login exit=$rc"
    grep -q "folds into 'download'" <<<"$out" \
        && ok "bare login explains itself" || bad "bare login message: $out"
    started && bad "bare login started a container" || ok "bare login starts no container"

    # The flags a collector with a session probe lets through: `--check`
    # probes the session, and a trapped `--help` would print the no-op
    # notice instead of the collector's own usage.
    for flag in --check --help -h; do
        run "$wrapper" login "$flag" >/dev/null 2>&1
        if [[ "$name" == amex || "$name" == chase || "$name" == raiffeisen_at ]]; then
            started && ok "login $flag passes through" || bad "login $flag was trapped"
        elif [[ "$flag" == --check ]]; then
            started && bad "login --check started a container" \
                || ok "login --check is trapped too: no session to probe"
        fi
    done
done

summary "login trap tests"
